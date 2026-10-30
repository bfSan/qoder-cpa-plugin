package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func assertQueueClientMessage(t *testing.T, message string) {
	t.Helper()
	// This is the auth-first rule used by the deployed DSH adapter. Retaining
	// an auth status anywhere in the nested body recreates the original bug.
	if regexp.MustCompile(`\b(?:401|403)\b`).MatchString(message) {
		t.Fatalf("queue message is still classified as AUTH: %s", message)
	}
	for _, marker := range []string{"rate limit", "RESOURCE_LIMIT", "retryAfterSeconds=", "unexpected EOF"} {
		if !strings.Contains(message, marker) {
			t.Fatalf("queue message is missing %q: %s", marker, message)
		}
	}
}

func assertQueueClientError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected exhausted queue error")
	}
	assertQueueClientMessage(t, err.Error())
	var sc interface{ StatusCode() int }
	if errors.As(err, &sc) {
		t.Fatalf("queue error must not trigger credential-wide status cooldown: %d", sc.StatusCode())
	}
	var env envelope
	if json.Unmarshal(errorEnvelopeFor(err), &env) != nil || env.Error == nil {
		t.Fatal("expected RPC error envelope")
	}
	if env.Error.Code != "rate_limit" || env.Error.HTTPStatus != 0 {
		t.Fatalf("unexpected public RPC classification: %+v", env.Error)
	}
	assertQueueClientMessage(t, env.Error.Message)
}

func TestQueueErrorFormatsFullBodyBeforeTruncation(t *testing.T) {
	body := `{"padding":"` + strings.Repeat("x", 600) + `","code":"10605","retryAfterSeconds":22,"message":"Bearer secret-test-token 401 403 额度不足"}`
	for name, err := range map[string]error{
		"HTTP": chatUpstreamError(403, body),
		"SSE":  upstreamEnvelopeError(403, body),
	} {
		t.Run(name, func(t *testing.T) {
			assertQueueClientError(t, err)
			if !strings.Contains(err.Error(), "retryAfterSeconds=22") || strings.Contains(err.Error(), "secret-test-token") || strings.Contains(err.Error(), "额度不足") {
				t.Fatalf("unsafe/lost retry hint: %s", err)
			}
		})
	}
	assertQueueClientMessage(t, streamEnvelopeError(403, body))
}

func TestQueueErrorRetryHints(t *testing.T) {
	cases := []struct {
		body string
		want int
	}{
		{queueAdmissionBody, 30},
		{`{"code":"10605","retryAfterSeconds":22}`, 22},
		{`{\"code\":\"10605\",\"retryAfterSeconds\":5`, 5},
		{`{"code":"10605","retryAfterSeconds":"12"}`, 12},
		{`{"code":"10605","retryAfterSeconds":0}`, 30},
		{`{"code":"10605","retryAfterSeconds":-1}`, 30},
		{`{"code":"10605","retryAfterSeconds":403}`, 300},
		{`{"code":"10605","retryAfterSeconds":9999999999999999999999999}`, 30},
		{`{"code":"10605"}`, 30},
	}
	for _, tc := range cases {
		err := newQueueAdmissionError(403, tc.body)
		if err == nil || err.retryAfterSeconds != tc.want {
			t.Fatalf("body=%s got=%v want=%d", tc.body, err, tc.want)
		}
		assertQueueClientError(t, err)
	}
}

func TestQueueErrorUnwrapsOnlyAtClientBoundary(t *testing.T) {
	_, err := aggregateQoderSSE(strings.NewReader(queueAdmissionEnvelope+"\n\n"), "qfmodel")
	if upstreamStatusFromError(err) != 403 || !isSoftRateLimit(403, err.Error()) {
		t.Fatalf("internal queue retry semantics lost: %v", err)
	}
	// The fold retains the upstream status for retry decisions, but neither
	// a handler's terminal error nor its RPC envelope may expose that status.
	assertQueueClientError(t, queueErrorForClient(err))
	var env envelope
	if json.Unmarshal(errorEnvelopeFor(fmt.Errorf("fold: %w", err)), &env) != nil || env.Error == nil || env.Error.HTTPStatus != 0 {
		t.Fatalf("nested queue status leaked through RPC: %+v", env.Error)
	}
	assertQueueClientMessage(t, env.Error.Message)
}

func queueFailureGateway(t *testing.T, transport string, failures int32, hits *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(hits, 1)
		if n <= failures {
			body := queueAdmissionBody
			if strings.HasSuffix(transport, "-credit") {
				body += " 额度不足"
			}
			if strings.HasPrefix(transport, "http") {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(body))
			} else {
				w.Header().Set("Content-Type", "text/event-stream")
				env, _ := json.Marshal(map[string]any{"body": body, "statusCodeValue": 403})
				_, _ = w.Write(append(append([]byte("data:"), env...), []byte("\n\n")...))
			}
			return
		}
		_, _ = w.Write([]byte("data:" + `{"body":"{\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}","statusCodeValue":200}` + "\n\n" + "data:" + `{"body":"[DONE]","statusCodeValue":200}` + "\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func queueExecuteRequest(t *testing.T) []byte {
	t.Helper()
	storage, err := json.Marshal(testStoredAuth())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(pluginapi.ExecutorRequest{
		AuthID: "queue-test", Model: "qfmodel", StorageJSON: storage,
		Payload: json.RawMessage(`{"model":"qfmodel","messages":[{"role":"user","content":"hello"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestQueueExhaustionAllExecutorPaths(t *testing.T) {
	for _, transport := range []string{"http", "sse", "http-credit", "sse-credit"} {
		for _, path := range []string{"execute", "collect", "async"} {
			t.Run(transport+"/"+path, func(t *testing.T) {
				resetCooldowns(t)
				fake := installFakeHost(t)
				installFastRetry(t)
				var hits int32
				srv := queueFailureGateway(t, transport, upstreamRetryMaxAttempts, &hits)
				restore := setGatewayBaseForTest(regionCN, srv.URL)
				defer restore()
				prepare := func(ctx context.Context) (*http.Request, context.CancelFunc, error) {
					return buildChatRequest(ctx, testStoredAuth(), "body", "qfmodel")
				}
				switch path {
				case "execute":
					_, err := handleExecExecute(queueExecuteRequest(t))
					assertQueueClientError(t, err)
				case "collect":
					chunks, status, err := collectUpstreamStreamQoder(prepare, false, &sseUsageCollector{})
					if len(chunks) != 0 || status != 403 {
						t.Fatalf("internal status/chunks: %d/%d", status, len(chunks))
					}
					assertQueueClientError(t, err)
				case "async":
					pumpUpstreamStream(prepare, func() {}, "queue-stream", false, "qfmodel", "qfmodel", "", time.Now(), "queue-test", "qfmodel")
					if len(fake.chunks()) != 0 {
						t.Fatal("queue refusal emitted as payload")
					}
					errs := fake.streamErrors()
					if len(errs) != 1 {
						t.Fatalf("terminal errors: %v", errs)
					}
					assertQueueClientMessage(t, errs[0])
				}
				if got := atomic.LoadInt32(&hits); got != upstreamRetryMaxAttempts {
					t.Fatalf("attempts=%d", got)
				}
				if path != "collect" && !modelIsCooling("queue-test", "qfmodel") {
					t.Fatal("affected model must cool")
				}
				if modelIsCooling("queue-test", "gmodel") {
					t.Fatal("unrelated model must remain available")
				}
			})
		}
	}
}

func TestQueueExecuteRetryRecovers(t *testing.T) {
	for _, transport := range []string{"http", "sse"} {
		t.Run(transport, func(t *testing.T) {
			resetCooldowns(t)
			installFakeHost(t)
			installFastRetry(t)
			var hits int32
			srv := queueFailureGateway(t, transport, 1, &hits)
			restore := setGatewayBaseForTest(regionCN, srv.URL)
			defer restore()
			out, err := handleExecExecute(queueExecuteRequest(t))
			if err != nil || len(out) == 0 {
				t.Fatalf("retry did not recover: %v", err)
			}
			if hits != 2 || modelIsCooling("queue-test", "qfmodel") {
				t.Fatalf("attempts=%d or unexpected cooldown", hits)
			}
		})
	}
}

func TestQueueAfterOutputNeverReplays(t *testing.T) {
	resetCooldowns(t)
	fake := installFakeHost(t)
	installFastRetry(t)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte("data:" + `{"body":"{\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}","statusCodeValue":200}` + "\n\n" + queueAdmissionEnvelope + "\n\n"))
	}))
	defer srv.Close()
	restore := setGatewayBaseForTest(regionCN, srv.URL)
	defer restore()
	prepare := func(ctx context.Context) (*http.Request, context.CancelFunc, error) {
		return buildChatRequest(ctx, testStoredAuth(), "body", "qfmodel")
	}
	pumpUpstreamStream(prepare, func() {}, "queue-stream", false, "qfmodel", "qfmodel", "", time.Now(), "queue-test", "qfmodel")
	if hits != 1 || len(fake.chunks()) != 1 || len(fake.streamErrors()) != 1 {
		t.Fatalf("partial stream was replayed/lost: attempts=%d chunks=%v errors=%v", hits, fake.chunks(), fake.streamErrors())
	}
	assertQueueClientMessage(t, fake.streamErrors()[0])
}

func TestQueueCreditTextDoesNotReconcileAccount(t *testing.T) {
	old := hostCall
	defer func() { hostCall = old }()
	var calls int
	hostCall = func(string, []byte) ([]byte, error) {
		calls++
		return []byte(`{"ok":true}`), nil
	}
	reconcileByUID("queue-test", 403, queueAdmissionBody+" 额度不足")
	if calls != 0 {
		t.Fatalf("queue must not inspect/reconcile account: %d host calls", calls)
	}
}

func TestQueueNormalizationLeavesOtherErrorsAlone(t *testing.T) {
	for _, status := range []int{401, 402, 403, 429, 500} {
		body := `{"message":"ordinary refusal"}`
		if newQueueAdmissionError(status, body) != nil {
			t.Fatalf("non-queue %d rewritten", status)
		}
		err := chatUpstreamError(status, body)
		if queueErrorForClient(err) != err || !strings.Contains(err.Error(), fmt.Sprint(status)) {
			t.Fatalf("non-queue %d changed", status)
		}
	}
}
