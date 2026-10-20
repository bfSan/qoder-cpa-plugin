package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// queueAdmissionEnvelope is the gateway's real shape for the 10605 refusal:
// HTTP 200 with the refusal nested in the SSE envelope, carrying
// statusCodeValue 403.
const queueAdmissionEnvelope = `data:{"headers":{},"body":"{\"code\":\"403\",\"message\":\"{\\\"code\\\":\\\"10605\\\",\\\"message\\\":\\\"{\\\\\\\"isQueued\\\\\\\":true,\\\\\\\"modelKey\\\\\\\":\\\\\\\"qfmodel\\\\\\\",\\\\\\\"queueType\\\\\\\":\\\\\\\"p3\\\\\\\",\\\\\\\"retryAfterSeconds\\\\\\\":30}\\\",\\\"type\\\":\\\"server_error\\\"}\"}","statusCodeValue":403}`

// queueThenSuccessGateway answers the first `failures` calls with the queued
// envelope, then streams a normal completion.
func queueThenSuccessGateway(t *testing.T, failures int32, hits *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if n <= failures {
			_, _ = w.Write([]byte(queueAdmissionEnvelope + "\n\n"))
			return
		}
		_, _ = w.Write([]byte("data:" + `{"headers":{},"body":"{\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}","statusCodeValue":200}` + "\n\n"))
		_, _ = w.Write([]byte("data:" + `{"headers":{},"body":"[DONE]","statusCodeValue":200}` + "\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestAggregateQueueAdmissionRetries is the regression test for the real
// production defect: /v1/responses reaches handleExecExecute, folds the SSE
// through aggregateQoderSSE, and that branch retried ONLY 429. A 10605 queue
// admission (statusCodeValue 403) was therefore terminal, and its 403 reached
// the host as AUTH — the client showed "API 密钥无效".
func TestAggregateQueueAdmissionRetries(t *testing.T) {
	resetCooldowns(t)
	installFakeHost(t)
	calls := installFastRetry(t)
	var hits int32
	srv := queueThenSuccessGateway(t, 1, &hits)
	restore := setGatewayBaseForTest(regionCN, srv.URL)
	defer restore()

	sa := testStoredAuth()
	prepare := func(ctx context.Context) (*http.Request, context.CancelFunc, error) {
		return buildChatRequest(ctx, sa, "body", "qfmodel")
	}

	// Mirror the retry loop in handleExecExecute exactly.
	var foldErr error
	for attempt := 1; attempt <= upstreamRetryMaxAttempts; attempt++ {
		if attempt > 1 {
			_ = retrySleep(context.Background(), time.Millisecond)
		}
		req, cancel, err := prepare(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		stream, status, _, err := hostHTTPDoStream(req)
		if cancel != nil {
			cancel()
		}
		if err != nil {
			t.Fatal(err)
		}
		if status >= 400 {
			stream.Close()
			t.Fatalf("gateway should answer 200 with an envelope refusal, got %d", status)
		}
		_, foldErr = aggregateQoderSSE(newHostStreamReader(stream), "qwen3.8-flash")
		stream.Close()
		if foldErr == nil {
			break
		}
		foldStatus := upstreamStatusFromError(foldErr)
		if isSoftRateLimit(foldStatus, foldErr.Error()) && attempt < upstreamRetryMaxAttempts {
			continue
		}
		t.Fatalf("queue admission was not retried: status=%d err=%v", foldStatus, foldErr)
	}

	if foldErr != nil {
		t.Fatalf("the retried fold should have succeeded, got %v", foldErr)
	}
	if got := atomic.LoadInt32(calls); got == 0 {
		t.Fatal("no backoff retry was performed")
	}
	if hits != 2 {
		t.Fatalf("want exactly 2 upstream hits (queued then success), got %d", hits)
	}
	if modelIsCooling("auth-1", "qfmodel") {
		t.Fatal("a recovered queue admission must not cool the pair")
	}
}
