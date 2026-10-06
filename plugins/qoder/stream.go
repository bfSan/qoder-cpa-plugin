// stream.go owns the upstream SSE data plane: emitting cleaned chunks back to
// the host stream (streamEmit/close), pumping the upstream SSE in a goroutine
// (pumpUpstreamStream), collecting it synchronously (collectUpstreamStream),
// and the SSE-frame helpers that re-frame, filter, and aggregate chunks.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// streamEmit pushes one chunk payload to the host stream. Returns an error if
// the host rejected it (e.g. the client already disconnected and the stream
// was closed), which the pump uses to stop reading a dead upstream.
func streamEmit(streamID string, payload []byte) error {
	if streamID == "" {
		return fmt.Errorf("no stream id")
	}
	body, _ := json.Marshal(map[string]any{"stream_id": streamID, "payload": payload})
	_, err := hostCall(pluginabi.MethodHostStreamEmit, body)
	return err
}

// streamEmitError forwards a terminal stream failure to the host.
//
// The message must travel in the RPC `error` field (rpcStreamEmitRequest.Error),
// not inside a JSON payload: the host turns that field into the chunk's Err,
// which is what its cooldown layer classifies. Sending an `{"error":...}`
// payload instead makes the host treat the bytes as a normal data chunk, so the
// client sees a truncated stream and CPA synthesizes its own generic
// "empty_stream" error at the transport layer — which is classified as a
// credential-scoped failure and cools the whole auth. Keeping the error on the
// RPC field also preserves our connection-lifecycle wording.
//
// A-37: never emit raw upstream bodies that may contain Bearer/JWT.
func streamEmitError(streamID, message string) {
	if streamID == "" {
		return
	}
	body, err := streamErrorFrame(streamID, redactSecrets(message))
	if err != nil {
		return
	}
	_, _ = hostCall(pluginabi.MethodHostStreamEmit, body)
}

// streamErrorFrame renders the host.stream.emit request for a terminal error.
// Kept separate from streamEmitError so tests can pin the wire shape without a
// live host RPC bridge: the message must live in the top-level "error" field
// (rpcStreamEmitRequest.Error), never inside "payload".
func streamErrorFrame(streamID, message string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"stream_id": streamID,
		"error":     message,
	})
}

var streamCloseOnce sync.Map // streamID -> sync.Once

func streamClose(streamID string) {
	if streamID == "" {
		return
	}
	actual, _ := streamCloseOnce.LoadOrStore(streamID, &sync.Once{})
	once := actual.(*sync.Once)
	once.Do(func() {
		body, _ := json.Marshal(map[string]any{"stream_id": streamID})
		_, _ = hostCall(pluginabi.MethodHostStreamClose, body)
	})
}

func streamHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	return h
}

// -----------------------------------------------------------------------------
// Upstream error envelopes (HTTP 200 with the status inside the SSE envelope)
// -----------------------------------------------------------------------------

// sseEnvelope is one `data:` frame of Qoder's nested SSE gateway stream:
//
//	data:{"headers":{...},"body":"<json-string>","statusCodeValue":200,...}
//
// The gateway answers HTTP 200 even when the upstream refused the request; the
// real status only appears in statusCodeValue with a body such as
// {"code":"429","message":"...rate limit..."}. Reading only `body` (as this
// plugin did before 0.9.7) turned that refusal into an ordinary chunk: the host
// saw a stream that ended without a single usable chunk, synthesized its own
// `empty_stream`, and — because that synthesized message lacks the plugin's
// "(unexpected EOF)" lifecycle wording — cooled the *credential* instead of the
// model. A single transient rate limit then escalated into `503
// auth_unavailable` for every model on the account.
type sseEnvelope struct {
	Body            *string `json:"body"`
	StatusCodeValue *int    `json:"statusCodeValue"`
	StatusCode      *int    `json:"statusCode"`
}

// statusCodeValueOrZero reports the envelope's HTTP status, tolerating both
// spellings and numeric rendering as a JSON string. Returns 0 when the envelope
// carries no usable status (then callers keep the historical chunk behavior).
func (e *sseEnvelope) statusCodeValueOrZero() int {
	if e == nil {
		return 0
	}
	if e.StatusCodeValue != nil {
		return *e.StatusCodeValue
	}
	if e.StatusCode != nil {
		return *e.StatusCode
	}
	return 0
}

// parseSSEEnvelope decodes one upstream `data:` frame. Returns (nil, false) when
// the frame is not a JSON object, so callers can keep skipping non-envelope
// lines exactly as before.
func parseSSEEnvelope(payload string) (*sseEnvelope, bool) {
	trimmed := strings.TrimSpace(payload)
	if trimmed == "" {
		return nil, false
	}
	if !strings.HasPrefix(trimmed, "{") {
		return nil, false
	}
	var raw struct {
		Body            json.RawMessage `json:"body"`
		StatusCodeValue json.RawMessage `json:"statusCodeValue"`
		StatusCode      json.RawMessage `json:"statusCode"`
	}
	if json.Unmarshal([]byte(trimmed), &raw) != nil {
		return nil, false
	}
	env := &sseEnvelope{}
	if len(raw.Body) > 0 {
		var s string
		if json.Unmarshal(raw.Body, &s) == nil {
			env.Body = &s
		}
	}
	if v, ok := jsonInt(raw.StatusCodeValue); ok {
		env.StatusCodeValue = &v
	}
	if v, ok := jsonInt(raw.StatusCode); ok {
		env.StatusCode = &v
	}
	return env, true
}

// jsonInt decodes a JSON number-or-numeric-string into an int.
func jsonInt(raw json.RawMessage) (int, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return 0, false
	}
	if n, err := strconv.Atoi(strings.Trim(trimmed, `"`)); err == nil {
		return n, true
	}
	var f float64
	if json.Unmarshal([]byte(trimmed), &f) == nil {
		return int(f), true
	}
	return 0, false
}

// upstreamEnvelopeStatus reports the effective status of one envelope: the
// envelope's own statusCodeValue, or the 4xx/5xx code carried by its body.
//
// A body-level code is consulted as well because the gateway emits two
// status-less shapes: a plain {"code":"429","message":"..."} refusal and an
// {"error":{...}} envelope. Without that fallback an HTTP 200 envelope holding
// a 429 body would still pass as a chunk. Returns 0 when the frame carries no
// refusal status at all, in which case callers keep the historical behavior.
func upstreamEnvelopeStatus(env *sseEnvelope) int {
	if env == nil {
		return 0
	}
	if status := env.statusCodeValueOrZero(); status >= 400 {
		return status
	}
	code, ok := envelopeBodyErrorCode(envelopeBodyText(env))
	if !ok {
		return 0
	}
	return code
}

func isHTTPErrorStatus(code int) bool { return code >= 400 && code <= 599 }

// envelopeBodyText returns the envelope body, or "" when absent.
func envelopeBodyText(env *sseEnvelope) string {
	if env == nil || env.Body == nil {
		return ""
	}
	return *env.Body
}

// envelopeBodyErrorCode extracts a 4xx/5xx code from an upstream error body.
// Accepted shapes: {"code":"429",...}, {"error":{"code":429},...},
// {"error":{"status_code":429},...}.
func envelopeBodyErrorCode(body string) (int, bool) {
	trimmed := strings.TrimSpace(body)
	if !strings.HasPrefix(trimmed, "{") {
		return 0, false
	}
	var payload map[string]any
	if json.Unmarshal([]byte(trimmed), &payload) != nil {
		return 0, false
	}
	if code, ok := codeFromAny(payload["code"]); ok && isHTTPErrorStatus(code) {
		return code, true
	}
	if errObj, ok := payload["error"].(map[string]any); ok {
		for _, key := range []string{"code", "status_code", "statusCode", "status"} {
			if code, ok := codeFromAny(errObj[key]); ok && isHTTPErrorStatus(code) {
				return code, true
			}
		}
	}
	return 0, false
}

// codeFromAny renders a JSON code value as an HTTP status when it is numeric.
// Non-numeric business codes ("RATE_LIMITED") are ignored here: the error text
// still reaches the cooldown layer, which matches on its own markers.
func codeFromAny(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return int(n), true
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(x)); err == nil {
			return n, true
		}
	}
	return 0, false
}

// looksLikeErrorBody reports whether a body carries an explicit error shape.
func looksLikeErrorBody(body string) bool {
	trimmed := strings.TrimSpace(body)
	if !strings.HasPrefix(trimmed, "{") {
		return false
	}
	if _, hasCode := envelopeBodyErrorCode(trimmed); hasCode {
		return true
	}
	lower := strings.ToLower(trimmed)
	return strings.Contains(lower, `"error"`) ||
		strings.Contains(lower, `"code"`) ||
		strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "throttl")
}

// upstreamEnvelopeError renders the client-facing error for a refused envelope
// on a synchronous path.
//
// Unlike streamEnvelopeError it carries no "(unexpected EOF)" marker: nothing
// here is a transport lifecycle event. These callers wrap the result in
// upstreamStatusError, so the host classifies the failure from the HTTP status
// that crosses the RPC boundary — not from the message text.
func upstreamEnvelopeError(status int, body string) error {
	trimmed := truncateRedacted(body, 200)
	if trimmed == "" {
		trimmed = "empty error body"
	}
	return fmt.Errorf("upstream %d: %s", status, trimmed)
}

// streamEnvelopeError renders the terminal message for the async stream path.
//
// The message must keep this plugin distinguishable from the host's own
// synthesized `empty_stream`: the host skips credential cooldown only for
// messages it recognizes as connection-lifecycle failures, and "(unexpected
// EOF)" is that marker. Rate limiting is a model-scoped, short-lived condition
// — it must cool the (account, model) pair, never the whole credential.
func streamEnvelopeError(status int, body string) string {
	trimmed := truncateRedacted(body, 300)
	if trimmed == "" {
		trimmed = "empty error body"
	}
	if isSoftRateLimit(status, trimmed) {
		return fmt.Sprintf("upstream %d: %s (rate limit, unexpected EOF)", status, trimmed)
	}
	return fmt.Sprintf("upstream %d: %s", status, trimmed)
}

// retrySleep performs the backoff wait. Tests replace it to avoid real delays;
// production keeps sleepBeforeRetry so a client disconnect interrupts the wait.
var retrySleep = func(ctx context.Context, wait time.Duration) error {
	return sleepBeforeRetry(ctx, wait)
}

// retryBackoffSchedule returns the delay before attempt n (1-based), with
// jitter, for the transient rate-limit retry loop.
func retryBackoffSchedule(attempt int) time.Duration {
	base := time.Second << uint(attempt-1) // 1s, 2s, 4s
	half := base / 2
	jitter := time.Duration(rand.Int63n(int64(half)))
	return half + jitter
}

// sleepBeforeRetry waits for the rate-limit backoff, honouring context
// cancellation so a client disconnect stops the retry loop immediately.
func sleepBeforeRetry(ctx context.Context, wait time.Duration) error {
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	if ctx == nil {
		<-timer.C
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// pumpUpstreamStream reads the upstream SSE response in the background and
// emits each cleaned chunk to the host stream. It closes the stream when done.
// An emit failure (client disconnected → host closed the stream) aborts the
// pump so we stop reading a dead upstream. cancel is invoked on every exit so
// the underlying http request context is released promptly.
//
// v0.7.0: requests now route via host.http.do_stream so request-log captures
// the outbound call and host transport policy applies. The host bridge emits
// arbitrary 32KB chunks, so we adapt to io.Reader and keep the bufio.Scanner
// SSE line framing unchanged.
// upstreamRequestPrepare builds one fresh upstream attempt request.
//
// A retry needs its own request: the host HTTP bridge reads and consumes the
// request body on every call, so a *http.Request cannot be reused.
type upstreamRequestPrepare func(ctx context.Context) (*http.Request, context.CancelFunc, error)

// upstreamRetryMaxAttempts bounds the rate-limit retry loop. The gateway's
// throttling window is short, so a couple of backoffs absorb it without
// turning a transient refusal into a session failure.
const upstreamRetryMaxAttempts = 3

// pumpUpstreamStream reads the upstream SSE response in the background and
// emits each cleaned chunk to the host stream. It closes the stream when done.
// An emit failure (client disconnected → host closed the stream) aborts the
// pump so we stop reading a dead upstream. cancel is invoked on every exit so
// the underlying http request context is released promptly.
//
// release hands back the per-account concurrency slot; it is deferred so the
// slot is returned on every exit path, panic included.
//
// v0.7.0: requests now route via host.http.do_stream so request-log captures
// the outbound call and host transport policy applies. The host bridge emits
// arbitrary 32KB chunks, so we adapt to io.Reader and keep the bufio.Scanner
// SSE line framing unchanged.
func pumpUpstreamStream(prepare upstreamRequestPrepare, release func(), streamID string, sseFramed bool, requestedModel, upstreamModel, authUID string, started time.Time, authID, cooldownModel string) {
	// Always close the host stream exactly once on every exit path.
	closed := false
	closeOnce := func() {
		if closed {
			return
		}
		closed = true
		streamClose(streamID)
	}
	defer closeOnce()
	if release != nil {
		defer release()
	}

	// One parent context spans every attempt so cancellation (client gone, host
	// shutting the stream down) also interrupts the backoff sleep.
	parentCtx, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()

	for attempt := 1; attempt <= upstreamRetryMaxAttempts; attempt++ {
		if attempt > 1 {
			wait := retryBackoffSchedule(attempt - 1)
			if err := retrySleep(parentCtx, wait); err != nil {
				publishUsage(requestedModel, upstreamModel, authUID, started, usage.Detail{}, true, 0, err.Error())
				return
			}
		}
		httpReq, cancel, err := prepare(parentCtx)
		if err != nil {
			publishUsage(requestedModel, upstreamModel, authUID, started, usage.Detail{}, true, 0, err.Error())
			streamEmitError(streamID, fmt.Sprintf("http_error: %v", err))
			return
		}
		if retry := pumpUpstreamAttempt(httpReq, cancel, streamID, sseFramed, requestedModel, upstreamModel, authUID, started, authID, cooldownModel, attempt); !retry {
			return
		}
	}
}

// pumpUpstreamAttempt runs exactly one upstream attempt and reports whether the
// caller should retry.
//
// It returns true only for a pure rate limit observed before any chunk reached
// the client: retrying then is safe and cheap. Every other outcome — success,
// fatal upstream error, or an exhausted retry budget — publishes usage, records
// the model-scoped cooldown and emits the terminal error frame itself, then
// returns false.
func pumpUpstreamAttempt(httpReq *http.Request, cancel context.CancelFunc, streamID string, sseFramed bool, requestedModel, upstreamModel, authUID string, started time.Time, authID, cooldownModel string, attempt int) bool {
	if cancel != nil {
		defer cancel()
	}

	stream, statusCode, _, err := hostHTTPDoStream(httpReq)
	if err != nil {
		publishUsage(requestedModel, upstreamModel, authUID, started, usage.Detail{}, true, 0, err.Error())
		streamEmitError(streamID, fmt.Sprintf("http_error: %v", err))
		return false
	}
	defer stream.Close()
	if statusCode >= 400 {
		// Drain the error body via the same bridge so the message is complete.
		errPayload, _ := io.ReadAll(newHostStreamReader(stream))
		body := string(errPayload)
		if isSoftRateLimit(statusCode, body) && attempt < upstreamRetryMaxAttempts {
			// Throttling: back off and try again instead of failing the session.
			return true
		}
		publishUsage(requestedModel, upstreamModel, authUID, started, usage.Detail{}, true, statusCode, body)
		recordUpstreamFailure(authID, cooldownModel, statusCode, body)
		if authUID != "" {
			go reconcileByUID(authUID, statusCode, body)
		}
		streamEmitError(streamID, chatUpstreamError(statusCode, body).Error())
		return false
	}
	collector := &sseUsageCollector{}
	emitted := false
	scanner := bufio.NewScanner(newHostStreamReader(stream))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		env, ok := parseSSEEnvelope(strings.TrimPrefix(line, "data:"))
		if !ok {
			continue
		}
		// The gateway answers HTTP 200 even when it refused the request; the
		// real status lives inside the envelope. Treat a refusal as an error
		// frame — never as a chunk — otherwise the host sees a stream that
		// ended without a single usable chunk and cools the whole credential.
		if envStatus := upstreamEnvelopeStatus(env); envStatus >= 400 {
			body := envelopeBodyText(env)
			if isSoftRateLimit(envStatus, body) && attempt < upstreamRetryMaxAttempts && !emitted {
				return true
			}
			publishUsage(requestedModel, upstreamModel, authUID, started, collector.detail(), true, envStatus, body)
			recordUpstreamFailure(authID, cooldownModel, envStatus, body)
			if authUID != "" {
				go reconcileByUID(authUID, envStatus, body)
			}
			streamEmitError(streamID, streamEnvelopeError(envStatus, body))
			return false
		}
		bodyStr := envelopeBodyText(env)
		if bodyStr == "" {
			continue
		}
		if bodyStr == "[DONE]" {
			break
		}
		collector.feed(bodyStr)
		cleaned := cleanChunkJSON(bodyStr)
		if cleaned == "" {
			continue
		}
		if sseFramed {
			cleaned = "data: " + cleaned
		}
		if err := streamEmit(streamID, []byte(cleaned)); err != nil {
			// Client disconnected / host closed stream — abort; do not report success.
			publishUsage(requestedModel, upstreamModel, authUID, started, collector.detail(), true, 0, "stream_emit: "+err.Error())
			return false
		}
		emitted = true
	}
	// A mid-stream read failure means the client received a truncated stream:
	// surface it as an error frame and record the attempt as failed.
	if err := scanner.Err(); err != nil {
		publishUsage(requestedModel, upstreamModel, authUID, started, collector.detail(), true, 0, err.Error())
		readErr := upstreamReadError(err)
		recordUpstreamFailure(authID, cooldownModel, 0, readErr.Error())
		streamEmitError(streamID, readErr.Error())
		return false
	}
	if !emitted {
		errEmpty := emptyStreamError()
		publishUsage(requestedModel, upstreamModel, authUID, started, collector.detail(), true, 0, errEmpty.Error())
		recordUpstreamFailure(authID, cooldownModel, 0, errEmpty.Error())
		streamEmitError(streamID, errEmpty.Error())
		return false
	}
	publishUsage(requestedModel, upstreamModel, authUID, started, collector.detail(), false, 0, "")
	invalidateAccountCredits(authID, authUID)
	return false
}

// collectUpstreamStreamQoder is the QoderWork-flavoured synchronous fallback
// (no async stream id): drain the upstream nested SSE, unwrap the inner
// OpenAI chunks, return them as a slice. The collector, when non-nil,
// observes the unwrapped inner chunks for usage extraction.
//
// prepare builds a fresh request per attempt (the host bridge consumes the
// request body), so a rate-limited envelope or HTTP status can be retried with
// backoff before it is reported to the host. The returned status is the
// effective upstream status — the envelope's statusCodeValue when the gateway
// refused inside an HTTP 200 stream.
func collectUpstreamStreamQoder(prepare upstreamRequestPrepare, sseFramed bool, collector *sseUsageCollector) ([]pluginapi.ExecutorStreamChunk, int, error) {
	var lastStatus int
	var lastErr error
	for attempt := 1; attempt <= upstreamRetryMaxAttempts; attempt++ {
		if attempt > 1 {
			if err := retrySleep(context.Background(), retryBackoffSchedule(attempt-1)); err != nil {
				break
			}
		}
		chunks, status, err := collectUpstreamAttempt(prepare, sseFramed, collector)
		if err == nil {
			return chunks, status, nil
		}
		lastStatus, lastErr = status, err
		if !isSoftRateLimit(status, err.Error()) {
			break
		}
	}
	if lastErr == nil {
		lastErr = emptyStreamError()
	}
	return nil, lastStatus, lastErr
}

// collectUpstreamAttempt performs one synchronous collection attempt.
func collectUpstreamAttempt(prepare upstreamRequestPrepare, sseFramed bool, collector *sseUsageCollector) ([]pluginapi.ExecutorStreamChunk, int, error) {
	httpReq, cancel, err := prepare(context.Background())
	if err != nil {
		return nil, 0, fmt.Errorf("http_error: %w", err)
	}
	if cancel != nil {
		defer cancel()
	}
	stream, statusCode, _, err := hostHTTPDoStream(httpReq)
	if err != nil {
		return nil, 0, fmt.Errorf("http_error: %w", err)
	}
	defer stream.Close()
	if statusCode >= 400 {
		payload, _ := io.ReadAll(newHostStreamReader(stream))
		// 0.8.13: account-level statuses ride the error envelope so the host
		// cooldown layer stops re-picking a drained credential.
		return nil, statusCode, upstreamStatusError(statusCode, chatUpstreamError(statusCode, string(payload)))
	}
	chunks := make([]pluginapi.ExecutorStreamChunk, 0, 64)
	scanner := bufio.NewScanner(newHostStreamReader(stream))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		env, ok := parseSSEEnvelope(strings.TrimPrefix(line, "data:"))
		if !ok {
			continue
		}
		// HTTP 200 with a refusal inside the envelope: this is an upstream
		// error, not a chunk. Reporting it with its real status lets the host
		// scope the cooldown to the credential/model instead of synthesizing
		// its own credential-scoped empty_stream.
		if envStatus := upstreamEnvelopeStatus(env); envStatus >= 400 {
			body := envelopeBodyText(env)
			return nil, envStatus, upstreamStatusError(envStatus, upstreamEnvelopeError(envStatus, body))
		}
		bodyStr := envelopeBodyText(env)
		if bodyStr == "" || bodyStr == "[DONE]" {
			continue
		}
		if collector != nil {
			collector.feed(bodyStr)
		}
		cleaned := cleanChunkJSON(bodyStr)
		if cleaned == "" {
			continue
		}
		if sseFramed {
			cleaned = "data: " + cleaned
		}
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: json.RawMessage(cleaned)})
	}
	if err := scanner.Err(); err != nil {
		return chunks, 0, upstreamReadError(err)
	}
	if len(chunks) == 0 {
		return nil, 0, emptyStreamError()
	}
	return chunks, 0, nil
}

// clientNeedsSSEFrame reports whether chunk payloads must carry their own
// "data: " SSE framing. CPA's chat-completions passthrough adds the prefix
// itself, but every cross-format response translator (claude/gemini/codex/...)
// only consumes payloads already framed as "data: " lines. The host hands the
// plugin the inbound request path in Metadata, so we frame chunks ourselves for
// any entry path other than the native OpenAI chat-completions one.
func clientNeedsSSEFrame(metadata map[string]any) bool {
	path, _ := metadata["request_path"].(string)
	switch strings.ToLower(strings.TrimSpace(path)) {
	case "/v1/chat/completions", "/v1/completions":
		return false
	default:
		return true
	}
}

// cleanChunkJSON strips only the known-problematic empty tool-call shells
// from choice deltas: a null/empty function_call and an empty tool_calls array
// (QoderWork emits these on the terminal chunk, and strict clients interpret
// them as a truncated tool call). Other empty-but-legal values are preserved:
// content:"" is a valid delta (pure tool-call chunk) and the role-only first
// chunk must survive so clients can establish the message role.
//
// It also drops upstream *error* bodies. A chunk without `choices` is only
// forwarded when it is a legitimate non-choice frame (e.g. a usage-only tail);
// a body carrying an error code/message is a refusal, and letting it through
// makes the host read it as a data chunk — the client then sees a stream that
// produced no usable chunk and CPA synthesizes a credential-scoped
// empty_stream. Returning "" makes the callers skip the frame.
func cleanChunkJSON(s string) string {
	var obj map[string]any
	if json.Unmarshal([]byte(s), &obj) != nil {
		return s
	}
	if _, hasChoices := obj["choices"]; !hasChoices {
		if looksLikeErrorBody(s) {
			return ""
		}
	}
	changed := false
	if choices, ok := obj["choices"].([]any); ok {
		for _, c := range choices {
			choice, ok := c.(map[string]any)
			if !ok {
				continue
			}
			delta, ok := choice["delta"].(map[string]any)
			if !ok {
				continue
			}
			if v, present := delta["function_call"]; present && isEmptyValue(v) {
				delete(delta, "function_call")
				changed = true
			}
			if v, present := delta["tool_calls"]; present {
				if arr, isArr := v.([]any); isArr && len(arr) == 0 {
					delete(delta, "tool_calls")
					changed = true
				}
			}
			// Upstream often pads terminal/noop deltas with empty noise fields
			// that clients ignore but pollute wire size / some parsers.
			for _, noise := range []string{"extra_fields", "refusal", "reasoning_content"} {
				if v, present := delta[noise]; present && isEmptyValue(v) {
					delete(delta, noise)
					changed = true
				}
			}
			// Drop a fully-empty delta ONLY when the choice carries no other
			// signal (no finish_reason): e.g. {"delta":{"function_call":null}}
			// reduced to {}. A delta with role/content:"" is meaningful and
			// never reaches this branch (those fields are preserved above).
			if len(delta) == 0 {
				if fr, _ := choice["finish_reason"].(string); fr == "" {
					return ""
				}
			}
		}
	}
	if !changed {
		return s
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return s
	}
	return string(out)
}

func aggregateCompletion(r io.Reader, model string) ([]byte, error) {
	var content, reasoning, role, respModel, respID, finish string
	var created int64
	var usage map[string]any
	// tool_calls arrive as streaming deltas: each chunk carries an index plus a
	// partial call (id/type/function.name on the first delta, argument text
	// fragments afterwards). Merge by index instead of appending raw fragments
	// so the folded completion holds whole calls.
	toolCalls := map[int]map[string]any{}
	var toolOrder []int
	var scanErr error

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		data := stripDataPrefix(scanner.Text())
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if v, ok := chunk["id"].(string); ok && v != "" {
			respID = v
		}
		if v, ok := chunk["model"].(string); ok && v != "" {
			respModel = v
		}
		if v, ok := chunk["created"].(float64); ok {
			created = int64(v)
		}
		if v, ok := chunk["usage"].(map[string]any); ok {
			usage = v
		}
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			choice, _ := c.(map[string]any)
			if delta, ok := choice["delta"].(map[string]any); ok {
				if v, ok := delta["role"].(string); ok && v != "" {
					role = v
				}
				if v, ok := delta["content"].(string); ok {
					content += v
				}
				if v, ok := delta["reasoning_content"].(string); ok {
					reasoning += v
				}
				if tcs, ok := delta["tool_calls"].([]any); ok {
					for _, tc := range tcs {
						call, ok := tc.(map[string]any)
						if !ok {
							continue
						}
						idx := 0
						if v, ok := call["index"].(float64); ok {
							idx = int(v)
						}
						merged, seen := toolCalls[idx]
						if !seen {
							merged = map[string]any{"index": idx}
							toolCalls[idx] = merged
							toolOrder = append(toolOrder, idx)
						}
						mergeToolCallDelta(merged, call)
					}
				}
			}
			if v, ok := choice["finish_reason"].(string); ok && v != "" {
				finish = v
			}
		}
	}
	if err := scanner.Err(); err != nil {
		scanErr = err
	}
	// A mid-stream read failure means the folded completion is truncated. The
	// host discards the payload entirely when the plugin returns an error
	// (sdk/api/handlers executeWithPluginExecutor), so fail fast here instead
	// of assembling a partial completion nobody can safely consume.
	if scanErr != nil {
		return nil, fmt.Errorf("upstream stream read error: %w", scanErr)
	}

	message := map[string]any{"role": firstNonEmpty(role, "assistant"), "content": content}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if len(toolOrder) > 0 {
		sort.Ints(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		message["tool_calls"] = calls
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	result := map[string]any{
		"id":      firstNonEmpty(respID, "chatcmpl-qoderwork"),
		"object":  "chat.completion",
		"created": created,
		"model":   firstNonEmpty(respModel, model),
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": firstNonEmpty(finish, "stop"),
		}},
	}
	if usage != nil {
		result["usage"] = usage
	}
	out, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// aggregateQoderSSE folds QoderWork's nested SSE stream into a single
// OpenAI chat.completion object. The gateway emits frames shaped:
//
//	data:{"headers":{...},"body":"<json-string>","statusCodeValue":200,...}
//
// where `body` is a JSON-encoded string that itself contains an OpenAI-style
// chunk: {"choices":[{"delta":{...}}],...}. We unwrap the outer envelope,
// then aggregate the inner chunks using the same logic as aggregateCompletion.
//
// Terminal frames: data:{"body":"[DONE]"} followed by an event:finish line
// with timing metadata (ignored).
//
// Two envelope refusals are reported instead of folded: the envelope's own
// statusCodeValue >= 400, and a body-level 4xx/5xx code ({"code":"429",...}).
// Before 0.9.7 the outer status was never read here, so a rate-limited
// /v1/responses call folded the error body into an empty completion — which the
// host then classified as a credential-scoped empty_stream.
func aggregateQoderSSE(r io.Reader, model string) ([]byte, error) {
	// Unwrap the nested SSE into an inner plain-text stream of OpenAI chunks,
	// then delegate to aggregateCompletion. We materialise the inner stream
	// into memory because the gateway's SSE is short-lived (one chat call)
	// and the inner stream is at most a few hundred KB.
	var inner strings.Builder
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			// Skip event:/id:/retry:/comment lines
			continue
		}
		env, ok := parseSSEEnvelope(strings.TrimPrefix(line, "data:"))
		if !ok {
			continue
		}
		if envStatus := upstreamEnvelopeStatus(env); envStatus >= 400 {
			body := envelopeBodyText(env)
			return nil, upstreamStatusError(envStatus, upstreamEnvelopeError(envStatus, body))
		}
		bodyStr := envelopeBodyText(env)
		if bodyStr == "" {
			continue
		}
		if bodyStr == "[DONE]" {
			break
		}
		// Re-emit the inner JSON as a standard "data:<json>\n" SSE frame so
		// aggregateCompletion's parser can consume it unchanged.
		inner.WriteString("data:")
		inner.WriteString(bodyStr)
		inner.WriteString("\n\n")
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("qoder SSE read: %w", err)
	}
	completion, err := aggregateCompletion(strings.NewReader(inner.String()), model)
	if err != nil {
		return nil, err
	}
	return completion, nil
}

// mergeToolCallDelta folds one streaming tool_call fragment into the merged
// call: scalar fields (id/type) are taken when first seen, function.name is
// concatenated (upstream may split it), and function.arguments text fragments
// are appended in arrival order.
func mergeToolCallDelta(merged, delta map[string]any) {
	for _, k := range []string{"id", "type"} {
		if _, present := merged[k]; !present {
			if v, ok := delta[k].(string); ok && v != "" {
				merged[k] = v
			}
		}
	}
	dfn, _ := delta["function"].(map[string]any)
	if dfn == nil {
		return
	}
	mfn, _ := merged["function"].(map[string]any)
	if mfn == nil {
		mfn = map[string]any{}
		merged["function"] = mfn
	}
	if v, ok := dfn["name"].(string); ok && v != "" {
		cur, _ := mfn["name"].(string)
		mfn["name"] = cur + v
	}
	if v, ok := dfn["arguments"].(string); ok && v != "" {
		cur, _ := mfn["arguments"].(string)
		mfn["arguments"] = cur + v
	}
}

func stripDataPrefix(s string) string {
	s = strings.TrimSpace(s)
	for strings.HasPrefix(s, "data:") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// isEmptyValue reports whether v is a "zero" SSE field that should be
// stripped from outgoing chunks. Used by cleanChunkJSON to drop the legacy
// function_call shell ({"name":"","arguments":""}) some upstreams emit on
// the terminal chunk — clients that strictly validate the delta will reject
// the shell as an invalid tool call.
func isEmptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		if len(x) == 0 {
			return true
		}
		for _, val := range x {
			if !isEmptyValue(val) {
				return false
			}
		}
		return true
	}
	return false
}
