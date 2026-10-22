// host_bridge.go routes every upstream HTTP call through the CPA host's
// http bridge (host.http.do / host.http.do_stream / stream_read / stream_close).
// Inherit mode uses the bridge and its transport policy. Explicit plugin proxy
// mode uses that proxy exclusively. A failed bridge call is NEVER retried direct.
// Direct variants are used only when the bridge is unavailable
// (unit tests, or hosts older than v7.2.x without the http bridge RPC).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// sharedHTTPClient is the fallback HTTP client used ONLY when the host HTTP
// bridge is unavailable (unit tests, or hosts older than v7.2.x without
// host.http.* RPC). All production upstream calls should route via hostHTTPDo
// / hostHTTPDoStream so request-log captures them and host transport policy
// applies. Direct use of this client in new code is a compliance bug.
func sharedHTTPClient() *http.Client {
	httpClientOnce.Do(func() {
		// No cookie jar here: auth is carried by Bearer headers, and a shared
		// jar would leak upstream set-cookie state across accounts (multi-account
		// deployments could cross-contaminate sessions). Only the short-lived
		// login clients get a jar.
		sharedClient = &http.Client{
			Timeout: 120 * time.Second,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				MaxIdleConns:        20,
				IdleConnTimeout:     90 * time.Second,
				MaxIdleConnsPerHost: 5,
			},
		}
	})
	return sharedClient
}

// hostHTTPResponse is the plugin-side view of an HTTP response that came back
// through the host bridge. Body is fully buffered (matches the historical
// io.ReadAll(resp.Body) usage pattern in billing / models / usage callers).
type hostHTTPResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// rpcHostHTTPRequestWire mirrors internal/pluginhost/host_callbacks.go's
// rpcHostHTTPRequest on the wire. The "request" sub-object is the actual HTTP
// call; the flat method/url/headers/body fields are an alternate form we don't
// use (host prefers Request when present).
type rpcHostHTTPRequestWire struct {
	Request *rpcHostHTTPInner `json:"request,omitempty"`
}

type rpcHostHTTPInner struct {
	Method  string              `json:"method,omitempty"`
	URL     string              `json:"url,omitempty"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    []byte              `json:"body,omitempty"`
}

type rpcHostHTTPStreamResponseWire struct {
	StatusCode int                         `json:"status_code"`
	Headers    map[string][]string         `json:"headers,omitempty"`
	StreamID   string                      `json:"stream_id,omitempty"`
	Chunks     []pluginapi.HTTPStreamChunk `json:"chunks,omitempty"`
}

type rpcHostHTTPStreamReadResponseWire struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

// hostBridgeUnwrap unwraps the pluginabi.Envelope returned by host RPC and
// returns the inner Result payload. Returns an error when the envelope itself
// signals failure (ok=false) or is malformed.
func hostBridgeUnwrap(raw []byte, method string) (json.RawMessage, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("%s: decode envelope: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: host error %s: %s", method, env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("%s: host returned not-ok", method)
	}
	return env.Result, nil
}

// hostBridgeAvailable reports whether host.http.* RPC is wired up. False in
// unit tests (no hostAPI) and when the host binary predates the bridge.
func hostBridgeAvailable() bool {
	return hostAPI != nil && hostAPI.call != nil
}

// hostHTTPDo performs a non-streaming upstream call via the host's http bridge.
// Request body is read eagerly (callers already have []byte or a small buffer).
// The response body is likewise read eagerly — all existing call sites consume
// it via io.ReadAll then Close, so we keep that shape and discard the closer.
//
// Fallback: when the host bridge is unavailable (unit tests, host older than
// v7.2.x without the http bridge), we route through sharedHTTPClient directly.
// This keeps the plugin functional in dev/test contexts while preferring the
// compliant path in production.
var qoderBridgeAvailable = hostBridgeAvailable

func hostHTTPDo(req *http.Request) (*hostHTTPResponse, error) {
	if req != nil {
		route := currentQoderProxy()
		if route.mode == qoderProxyBlocked {
			if req.Body != nil {
				_ = req.Body.Close()
			}
			return nil, blockedQoderHTTPError()
		}
		if route.mode == qoderProxyExplicit {
			return qoderHTTPDoWithClient(route.client, req)
		}
	}
	if req == nil {
		return nil, fmt.Errorf("nil request")
	}
	var bodyBytes []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("read request body: %w", err)
		}
		_ = req.Body.Close()
		bodyBytes = b
	}
	if !qoderBridgeAvailable() {
		return hostHTTPDoDirect(req, bodyBytes)
	}
	wire := rpcHostHTTPRequestWire{
		Request: &rpcHostHTTPInner{
			Method:  req.Method,
			URL:     req.URL.String(),
			Headers: map[string][]string(req.Header),
			Body:    bodyBytes,
		},
	}
	raw, err := hostCall(pluginabi.MethodHostHTTPDo, mustJSON(wire))
	if err != nil {
		return nil, qoderTransportFailure(req.Context(), err)
	}
	result, err := hostBridgeUnwrap(raw, pluginabi.MethodHostHTTPDo)
	if err != nil {
		return nil, qoderTransportFailure(req.Context(), err)
	}
	resp, err := decodeBridgeHTTPResponse(result)
	if err != nil {
		return nil, qoderTransportFailure(req.Context(), err)
	}
	if resp.StatusCode < 100 || resp.StatusCode > 599 {
		return nil, fmt.Errorf("Qoder host HTTP response has invalid status")
	}
	return resp, nil
}

// decodeBridgeHTTPResponse parses the host bridge's host.http.do result.
// The host marshals pluginapi.HTTPResponse WITHOUT json tags, so the wire keys
// are Go field names ("StatusCode"/"Headers"/"Body"). Parsing only
// "status_code" silently decoded every bridged response with StatusCode=0
// (underscore defeats Go's case-insensitive key fallback) — the same root
// cause workbuddy 0.9.10 fixed for its "models API status 0" symptom.
// qoder's callers only used the status for >=400 checks, so the bug stayed
// latent here; both spellings are now accepted.
func decodeBridgeHTTPResponse(result json.RawMessage) (*hostHTTPResponse, error) {
	var resp struct {
		StatusCode    int                 `json:"StatusCode"`
		StatusCodeAlt int                 `json:"status_code"`
		Headers       map[string][]string `json:"Headers"`
		Body          []byte              `json:"Body"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, fmt.Errorf("decode host.http.do response: %w", err)
	}
	status := resp.StatusCode
	if status == 0 {
		status = resp.StatusCodeAlt
	}
	return &hostHTTPResponse{
		StatusCode: status,
		Headers:    http.Header(resp.Headers),
		Body:       resp.Body,
	}, nil
}

// hostHTTPDoDirect executes the request via the plugin's own http.Client.
// Used as a fallback when the host bridge is unavailable (unit tests).
func hostHTTPDoDirect(req *http.Request, bodyBytes []byte) (*hostHTTPResponse, error) {
	// Rebuild the request since req.Body was already consumed.
	newReq, err := http.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	newReq.Header = req.Header.Clone()
	return qoderHTTPDoWithClient(sharedHTTPClient(), newReq)
}

func blockedQoderHTTPError() error {
	return fmt.Errorf("Qoder HTTP blocked by invalid proxy-url configuration")
}

func qoderHTTPDoWithClient(client *http.Client, req *http.Request) (*hostHTTPResponse, error) {
	copy := *client
	copy.CheckRedirect = qoderRejectRedirect
	resp, err := copy.Do(req)
	if err != nil {
		return nil, qoderTransportFailure(req.Context(), err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, qoderTransportFailure(req.Context(), err)
	}
	return &hostHTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Header.Clone(), Body: raw}, nil
}

// hostHTTPStream is a handle for an in-flight host-bridged stream. Read returns
// the next chunk; Close aborts the upstream.
//
// Two modes:
//   - Bridged: streamID set, Read/Close forward to host RPC.
//   - Direct (test fallback): direct holds the full buffered body, Read drains
//     it once then reports done. Close is a no-op.
type hostHTTPStream struct {
	streamID  string
	direct    []byte // compatibility for synthetic buffered bridge tests
	directAt  int
	body      io.ReadCloser // explicit proxy/bridge-unavailable streams stay streaming
	ctx       context.Context
	readMu    sync.Mutex
	closeOnce sync.Once
	closed    atomic.Bool
}

// hostHTTPDoStream opens a streaming call via the host bridge. The host owns
// the actual http.Response body; we pull chunks via hostHTTPStreamRead.
//
// Falls back to direct http.Client.Do when the bridge is unavailable (tests).
// In that case the returned hostHTTPStream wraps an in-memory copy of the
// full response body so Read/Close have the same shape.
func hostHTTPDoStream(req *http.Request) (*hostHTTPStream, int, http.Header, error) {
	if req != nil {
		route := currentQoderProxy()
		if route.mode == qoderProxyBlocked {
			if req.Body != nil {
				_ = req.Body.Close()
			}
			return nil, 0, nil, blockedQoderHTTPError()
		}
		if route.mode == qoderProxyExplicit {
			return qoderHTTPDoStreamWithClient(route.client, req)
		}
	}
	if req == nil {
		return nil, 0, nil, fmt.Errorf("nil request")
	}
	var bodyBytes []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, 0, nil, fmt.Errorf("read request body: %w", err)
		}
		_ = req.Body.Close()
		bodyBytes = b
	}
	if !qoderBridgeAvailable() {
		return hostHTTPDoStreamDirect(req, bodyBytes)
	}
	wire := rpcHostHTTPRequestWire{
		Request: &rpcHostHTTPInner{
			Method:  req.Method,
			URL:     req.URL.String(),
			Headers: map[string][]string(req.Header),
			Body:    bodyBytes,
		},
	}
	raw, err := hostCall(pluginabi.MethodHostHTTPDoStream, mustJSON(wire))
	if err != nil {
		return nil, 0, nil, qoderTransportFailure(req.Context(), err)
	}
	result, err := hostBridgeUnwrap(raw, pluginabi.MethodHostHTTPDoStream)
	if err != nil {
		return nil, 0, nil, qoderTransportFailure(req.Context(), err)
	}
	var resp rpcHostHTTPStreamResponseWire
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, 0, nil, fmt.Errorf("decode host.http.do_stream response: %w", err)
	}
	if resp.StreamID == "" {
		return nil, resp.StatusCode, http.Header(resp.Headers), fmt.Errorf("host stream bridge unavailable")
	}
	return &hostHTTPStream{streamID: resp.StreamID, ctx: req.Context()}, resp.StatusCode, http.Header(resp.Headers), nil
}

// hostHTTPDoStreamDirect is selected before sending when the bridge is absent.
// The response stays streaming and Close interrupts its reader.
func hostHTTPDoStreamDirect(req *http.Request, bodyBytes []byte) (*hostHTTPStream, int, http.Header, error) {
	newReq, err := http.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, 0, nil, err
	}
	newReq.Header = req.Header.Clone()
	return qoderHTTPDoStreamWithClient(sharedHTTPClient(), newReq)
}

func qoderHTTPDoStreamWithClient(client *http.Client, req *http.Request) (*hostHTTPStream, int, http.Header, error) {
	copy := *client
	copy.CheckRedirect = qoderRejectRedirect
	resp, err := copy.Do(req)
	if err != nil {
		return nil, 0, nil, qoderTransportFailure(req.Context(), err)
	}
	return &hostHTTPStream{body: resp.Body, ctx: req.Context()}, resp.StatusCode, resp.Header.Clone(), nil
}

// Read pulls the next chunk. Returns (payload, done, err). done=true means the
// stream ended cleanly; err non-nil means upstream or bridge error.
func (s *hostHTTPStream) Read() ([]byte, bool, error) {
	if s == nil || s.closed.Load() {
		return nil, true, fmt.Errorf("stream closed")
	}
	if s.ctx != nil && s.ctx.Err() != nil {
		s.Close()
		return nil, true, s.ctx.Err()
	}
	s.readMu.Lock()
	defer s.readMu.Unlock()
	if s.body != nil {
		buf := make([]byte, 32*1024)
		n, err := s.body.Read(buf)
		if err == io.EOF {
			return buf[:n], true, nil
		}
		if err != nil {
			return buf[:n], true, qoderTransportFailure(s.ctx, err)
		}
		return buf[:n], false, nil
	}
	if s.direct != nil {
		if s.directAt >= len(s.direct) {
			return nil, true, nil
		}
		out := s.direct[s.directAt:]
		s.directAt = len(s.direct)
		return out, false, nil
	}
	if s.streamID == "" {
		return nil, true, fmt.Errorf("stream closed")
	}
	raw, err := hostCall(pluginabi.MethodHostHTTPStreamRead, mustJSON(map[string]any{"stream_id": s.streamID}))
	if err != nil {
		return nil, true, qoderTransportFailure(s.ctx, err)
	}
	result, err := hostBridgeUnwrap(raw, pluginabi.MethodHostHTTPStreamRead)
	if err != nil {
		return nil, true, qoderTransportFailure(s.ctx, err)
	}
	var resp rpcHostHTTPStreamReadResponseWire
	if json.Unmarshal(result, &resp) != nil {
		return nil, true, fmt.Errorf("invalid host stream response")
	}
	if resp.Error != "" {
		return nil, true, fmt.Errorf("upstream stream read failed")
	}
	return resp.Payload, resp.Done, nil
}

func (s *hostHTTPStream) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		if s.body != nil {
			_ = s.body.Close()
		}
		if s.streamID != "" {
			_, _ = hostCall(pluginabi.MethodHostHTTPStreamClose, mustJSON(map[string]any{"stream_id": s.streamID}))
		}
	})
}

// hostStreamReader adapts a hostHTTPStream to io.Reader so existing
// bufio.Scanner / io.ReadAll call sites work unchanged. The host bridge emits
// arbitrary 32KB chunks (not SSE lines), so line framing must be re-assembled
// by the consumer — Scanner handles that for us.
type hostStreamReader struct {
	s    *hostHTTPStream
	buf  []byte // leftover from previous chunk
	done bool
	err  error
}

func newHostStreamReader(s *hostHTTPStream) *hostStreamReader {
	return &hostStreamReader{s: s}
}

func (r *hostStreamReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for empty := 0; ; empty++ {
		if len(r.buf) > 0 {
			n := copy(p, r.buf)
			r.buf = r.buf[n:]
			return n, nil
		}
		if r.done {
			if r.err != nil {
				return 0, r.err
			}
			return 0, io.EOF
		}
		chunk, done, err := r.s.Read()
		r.done, r.err = done || err != nil, err
		if len(chunk) > 0 {
			n := copy(p, chunk)
			if n < len(chunk) {
				r.buf = append(r.buf, chunk[n:]...)
			}
			return n, nil
		}
		if r.done {
			if err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		if empty >= 100 {
			r.done = true
			r.err = io.ErrNoProgress
			return 0, r.err
		}
	}
}

// mustJSON marshals v and panics on error — the wire structs above are always
// marshalable, so any failure here is a programming bug.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
