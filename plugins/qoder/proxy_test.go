package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

type qoderProxyTestTransport func(*http.Request) (*http.Response, error)

func (f qoderProxyTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func isolateQoderRouting(t *testing.T) {
	t.Helper()
	oldRoute := qoderProxyState.Load()
	oldClient := sharedHTTPClient()
	oldBridge := qoderBridgeAvailable
	oldCall := hostCall
	qoderProxyState.Store(&qoderProxyRouting{mode: qoderProxyInherit})
	qoderBridgeAvailable = func() bool { return false }
	t.Cleanup(func() {
		qoderProxyState.Store(oldRoute)
		sharedClient = oldClient
		qoderBridgeAvailable = oldBridge
		hostCall = oldCall
	})
}

func TestQoderBridgeFailureNeverReplaysBufferedOrStream(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, kind := range []string{"transport", "envelope", "malformed"} {
			t.Run(kind+map[bool]string{true: "stream", false: "buffered"}[stream], func(t *testing.T) {
				isolateQoderRouting(t)
				bridge, direct := 0, 0
				qoderBridgeAvailable = func() bool { return true }
				sharedClient = &http.Client{Transport: qoderProxyTestTransport(func(*http.Request) (*http.Response, error) {
					direct++
					return nil, errors.New("direct route must not run")
				})}
				hostCall = func(method string, raw []byte) ([]byte, error) {
					bridge++
					switch kind {
					case "transport":
						return nil, errors.New("private-token https://user:pass@proxy.example")
					case "envelope":
						return []byte(`{"ok":false,"error":{"message":"private-token"}}`), nil
					default:
						return []byte(`{bad`), nil
					}
				}
				req, _ := http.NewRequest(http.MethodPost, "https://qoder.example/token?verifier=secret", strings.NewReader(`{"nonce":"secret"}`))
				var err error
				if stream {
					_, _, _, err = hostHTTPDoStream(req)
				} else {
					_, err = hostHTTPDo(req)
				}
				if err == nil || bridge != 1 || direct != 0 {
					t.Fatalf("err=%v bridge=%d direct=%d", err, bridge, direct)
				}
				if strings.Contains(err.Error(), "private-token") || strings.Contains(err.Error(), "user:pass") || strings.Contains(err.Error(), "verifier") {
					t.Fatal("routing error exposed request/proxy secrets")
				}
			})
		}
	}
}

func TestQoderInvalidProxyBlocksAllPathsWithoutSecrets(t *testing.T) {
	isolateQoderRouting(t)
	calls := 0
	sharedClient = &http.Client{Transport: qoderProxyTestTransport(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("no network") })}
	for _, raw := range []string{"direct", "ftp://proxy.local", "http://user:private@", "http://proxy.local:99999", "https://proxy.local/?key=private"} {
		if err := configureQoderProxy(raw); err == nil || strings.Contains(err.Error(), "private") {
			t.Fatalf("invalid proxy result=%v", err)
		}
		r, _ := http.NewRequest(http.MethodGet, "http://example.invalid", nil)
		if _, err := qoderRoutedHTTPDo(r); err == nil {
			t.Fatal("OAuth/billing route ignored invalid explicit proxy")
		}
		r, _ = http.NewRequest(http.MethodPost, "http://example.invalid", strings.NewReader("{}"))
		if _, _, _, err := hostHTTPDoStream(r); err == nil {
			t.Fatal("stream ignored invalid explicit proxy")
		}
	}
	if calls != 0 {
		t.Fatal("blocked route still made network calls")
	}
	if err := configureQoderProxy(""); err != nil || currentQoderProxy().mode != qoderProxyInherit {
		t.Fatal("clear did not restore inherit")
	}
}

func TestQoderProxyYAMLOnlyUsesTopLevelString(t *testing.T) {
	isolateQoderRouting(t)
	for _, raw := range []string{"proxy-url: [http://proxy.local]", "proxy-url: http://a\nproxy-url: http://b", "[]"} {
		if configureQoderProxyYAML([]byte(raw)) == nil {
			t.Fatal("accepted malformed proxy setting")
		}
	}
	if err := configureQoderProxyYAML([]byte("nested:\n  proxy-url: invalid\n")); err != nil || currentQoderProxy().mode != qoderProxyInherit {
		t.Fatal("nested value became proxy configuration")
	}
	if err := configureQoderProxyYAML([]byte("proxy-url: http://127.0.0.1:7890\n")); err != nil || currentQoderProxy().mode != qoderProxyExplicit {
		t.Fatalf("valid explicit proxy=%v", err)
	}
}

func TestQoderExplicitProxyUsedForOAuthBillingAndStream(t *testing.T) {
	isolateQoderRouting(t)
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Host != "upstream.invalid" {
			t.Errorf("request did not reach proxy as absolute URL: %s", r.URL)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: ok\n\n")
	}))
	defer proxy.Close()
	if err := configureQoderProxy(proxy.URL); err != nil {
		t.Fatal(err)
	}
	qoderBridgeAvailable = func() bool { return true }
	hostCall = func(string, []byte) ([]byte, error) {
		t.Fatal("explicit proxy must not switch to host bridge")
		return nil, nil
	}
	for _, call := range []func(*http.Request) (*hostHTTPResponse, error){hostHTTPDo, qoderRoutedHTTPDo} {
		r, _ := http.NewRequest(http.MethodPost, "http://upstream.invalid/poll", strings.NewReader("{}"))
		resp, err := call(r)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("response=%v error=%v", resp, err)
		}
	}
	r, _ := http.NewRequest(http.MethodPost, "http://upstream.invalid/chat", strings.NewReader("{}"))
	s, status, _, err := hostHTTPDoStream(r)
	if err != nil || status != 200 {
		t.Fatal(err)
	}
	b, err := io.ReadAll(newHostStreamReader(s))
	s.Close()
	s.Close()
	if err != nil || string(b) != "data: ok\n\n" {
		t.Fatalf("body=%q err=%v", b, err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestQoderDirectRedirectDoesNotReplayMutatingRequest(t *testing.T) {
	isolateQoderRouting(t)
	var redirected atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/target" {
			redirected.Add(1)
			return
		}
		w.Header().Set("Location", "/target")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	r, _ := http.NewRequest(http.MethodPost, server.URL+"/claim", strings.NewReader("{}"))
	resp, err := hostHTTPDo(r)
	if err != nil || resp.StatusCode != 307 || redirected.Load() != 0 {
		t.Fatalf("response=%v err=%v redirected=%d", resp, err, redirected.Load())
	}
}

func TestQoderDirectStreamRemainsStreamingAndCancellationWorks(t *testing.T) {
	isolateQoderRouting(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader("{}"))
	s, _, _, err := hostHTTPDoStream(r)
	if err != nil {
		t.Fatal(err)
	}
	chunk, _, err := s.Read()
	if err != nil || !strings.Contains(string(chunk), "first") {
		t.Fatalf("chunk=%q err=%v", chunk, err)
	}
	cancel()
	s.Close()
	if _, done, err := s.Read(); !done || err == nil {
		t.Fatal("cancelled stream was not closed")
	}
}

func TestQoderBridgeReadErrorsDoNotLeakSecrets(t *testing.T) {
	isolateQoderRouting(t)
	hostCall = func(method string, _ []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPStreamRead {
			return []byte(`{"ok":true,"result":{"error":"Bearer private-token"}}`), nil
		}
		return []byte(`{"ok":true}`), nil
	}
	s := &hostHTTPStream{streamID: "test"}
	defer s.Close()
	_, _, err := s.Read()
	if err == nil || strings.Contains(err.Error(), "private-token") {
		t.Fatalf("error=%v", err)
	}
}
