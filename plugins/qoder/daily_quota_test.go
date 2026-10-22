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
)

const dailyQuotaBody = `{"code":"110","message":"Billing daily count exceeded"}`

func assertDailyQuotaMessage(t *testing.T, message string) {
	t.Helper()
	if regexp.MustCompile(`\b(?:401|403)\b`).MatchString(message) || !strings.Contains(message, "quota exceeded") || !strings.Contains(message, dailyQuotaMarker) {
		t.Fatalf("unsafe/unclassified daily quota message: %s", message)
	}
	if strings.Contains(message, "排队") || strings.Contains(message, "secret-test") {
		t.Fatalf("misleading/raw daily quota message: %s", message)
	}
}

func assertDailyQuotaError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected daily quota error")
	}
	assertDailyQuotaMessage(t, err.Error())
	var sc interface{ StatusCode() int }
	if errors.As(err, &sc) {
		t.Fatalf("public status leaked: %d", sc.StatusCode())
	}
	var env envelope
	if e := json.Unmarshal(errorEnvelopeFor(err), &env); e != nil || env.Error == nil {
		t.Fatalf("bad envelope: %v", e)
	}
	if env.Error.HTTPStatus != 0 || env.Error.Code != "quota_exceeded" {
		t.Fatalf("bad classification: %+v", env.Error)
	}
	assertDailyQuotaMessage(t, env.Error.Message)
}

func TestDailyQuotaClassification(t *testing.T) {
	for _, body := range []string{
		dailyQuotaBody,
		`{"code": 110, "message": "Billing daily count exceeded"}`,
		`{"code": "403", "message": "{\"code\": \"110\", \"message\": \"Billing daily count exceeded\"}"}`,
		`{"padding":"` + strings.Repeat("x", 700) + `","code":"110","message":"Billing daily count exceeded secret-test 401 403"}`,
	} {
		if !isDailyCountLimit(403, body) || isSoftRateLimit(403, body) {
			t.Fatalf("classification/retry: %s", body)
		}
		assertDailyQuotaError(t, chatUpstreamError(403, body))
		assertDailyQuotaError(t, upstreamEnvelopeError(403, body))
		assertDailyQuotaMessage(t, streamEnvelopeError(403, body))
	}
	for _, tc := range []struct {
		status int
		body   string
	}{
		{401, dailyQuotaBody}, {500, dailyQuotaBody},
		{403, `{"code":"110","message":"invalid signature"}`},
		{403, `{"code":"1110","message":"Billing daily count exceeded"}`},
		{403, `{"message":"Billing daily count exceeded"}`},
		{403, queueAdmissionBody}, {402, `{"message":"insufficient credits"}`},
	} {
		if isDailyCountLimit(tc.status, tc.body) {
			t.Fatalf("ordinary error rewritten: %+v", tc)
		}
	}
}

func dailyFailureGateway(t *testing.T, transport string, hits *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		if transport == "http" {
			w.WriteHeader(403)
			_, _ = w.Write([]byte(dailyQuotaBody))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		env, _ := json.Marshal(map[string]any{"body": dailyQuotaBody, "statusCodeValue": 403})
		_, _ = fmt.Fprintf(w, "data:%s\n\n", env)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDailyQuotaAllExecutorPaths(t *testing.T) {
	for _, transport := range []string{"http", "sse"} {
		for _, path := range []string{"execute", "collect", "async"} {
			t.Run(transport+"/"+path, func(t *testing.T) {
				resetCooldowns(t)
				fake := installFakeHost(t)
				installFastRetry(t)
				var hits int32
				srv := dailyFailureGateway(t, transport, &hits)
				restore := setGatewayBaseForTest(regionCN, srv.URL)
				defer restore()
				prepare := func(ctx context.Context) (*http.Request, context.CancelFunc, error) {
					return buildChatRequest(ctx, testStoredAuth(), "body", "qfmodel")
				}
				switch path {
				case "execute":
					_, err := handleExecExecute(queueExecuteRequest(t))
					assertDailyQuotaError(t, err)
				case "collect":
					// Exercise the real handler, not just the collector: it owns the cooldown.
					_, err := handleExecStream(queueExecuteRequest(t))
					assertDailyQuotaError(t, err)
				case "async":
					pumpUpstreamStream(prepare, func() {}, "daily-stream-"+transport, false, "qfmodel", "qfmodel", "", time.Now(), "queue-test", "qfmodel")
					if len(fake.chunks()) != 0 || len(fake.streamErrors()) != 1 {
						t.Fatalf("unexpected stream result: %v", fake.streamErrors())
					}
					assertDailyQuotaMessage(t, fake.streamErrors()[0])
				}
				if hits != 1 {
					t.Fatalf("daily cap was fast-retried: %d", hits)
				}
				rows := cooldownSnapshotFor("queue-test")
				if len(rows) != 1 || rows[0]["reason"] != "daily_quota" {
					t.Fatalf("missing quota cooldown: %+v", rows)
				}
				if modelIsCooling("queue-test", "gmodel") || modelIsCooling("other", "qfmodel") {
					t.Fatal("cooldown leaked to another pair")
				}
				// A cached host catalog must not allow another upstream attempt.
				_, err := handleExecExecute(queueExecuteRequest(t))
				assertDailyQuotaError(t, err)
				_, err = handleExecStream(queueExecuteRequest(t))
				assertDailyQuotaError(t, err)
				if hits != 1 {
					t.Fatalf("executor cooldown guard bypassed: %d", hits)
				}
			})
		}
	}
}

func TestDailyQuotaUnwrapAtPublicBoundary(t *testing.T) {
	env, _ := json.Marshal(map[string]any{"body": dailyQuotaBody, "statusCodeValue": 403})
	_, err := aggregateQoderSSE(strings.NewReader("data:"+string(env)+"\n\n"), "qfmodel")
	if upstreamStatusFromError(err) != 403 {
		t.Fatalf("lost internal status: %v", err)
	}
	assertDailyQuotaError(t, queueErrorForClient(fmt.Errorf("fold: %w", err)))
	var out envelope
	if e := json.Unmarshal(errorEnvelopeFor(err), &out); e != nil || out.Error == nil || out.Error.HTTPStatus != 0 || out.Error.Code != "quota_exceeded" {
		t.Fatalf("RPC leaked internal status: %+v", out.Error)
	}
}

func TestDailyQuotaCooldownExpiryAndAlias(t *testing.T) {
	resetCooldowns(t)
	base := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	now := base
	cooldownNowFn = func() time.Time { return now }
	model := requestModelForCooldown("qfmodel", map[string]any{"requested_model": "qoder/qwen3.8-flash"})
	recordUpstreamFailure("auth-a", model, 403, dailyQuotaBody)
	until := modelCoolingUntil("auth-a", model)
	if !until.Equal(base.Add(30 * time.Minute)) {
		t.Fatalf("bad finite TTL: %s", until)
	}
	now = base.Add(10 * time.Minute)
	assertDailyQuotaError(t, dailyQuotaCooldownError("auth-a", "qoder/qwen3.8-flash"))
	if !modelCoolingUntil("auth-a", model).Equal(until) {
		t.Fatal("read extended cooldown")
	}
	if dailyQuotaCooldownError("auth-a", "gmodel") != nil || dailyQuotaCooldownError("other", model) != nil {
		t.Fatal("guard not pair scoped")
	}
	now = until
	if dailyQuotaCooldownError("auth-a", model) != nil || modelIsCooling("auth-a", model) {
		t.Fatal("pair did not recover after TTL")
	}
}

func TestDailyQuotaDoesNotReconcileCreditLifecycle(t *testing.T) {
	old := hostCall
	defer func() { hostCall = old }()
	var calls int32
	hostCall = func(string, []byte) ([]byte, error) { atomic.AddInt32(&calls, 1); return []byte(`{"ok":true}`), nil }
	body := dailyQuotaBody + " insufficient credits"
	reconcileAfterExecutorError("auth-a", 403, body)
	reconcileByUID("auth-a", 403, body)
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatal("daily count triggered credit lifecycle")
	}
}

func TestDailyQuotaCannotBeDowngradedByInflightFailure(t *testing.T) {
	resetCooldowns(t)
	base := time.Now()
	now := base
	cooldownNowFn = func() time.Time { return now }
	recordUpstreamFailure("auth-a", "qfmodel", 403, dailyQuotaBody)
	until := modelCoolingUntil("auth-a", "qfmodel")
	now = base.Add(time.Minute)
	for _, tc := range []struct {
		status int
		body   string
	}{
		{0, "empty_stream: upstream stream closed before first payload"},
		{429, "rate limit"}, {403, queueAdmissionBody}, {403, dailyQuotaBody},
	} {
		recordUpstreamFailure("auth-a", "qfmodel", tc.status, tc.body)
		assertDailyQuotaError(t, dailyQuotaCooldownError("auth-a", "qfmodel"))
		if !modelCoolingUntil("auth-a", "qfmodel").Equal(until) {
			t.Fatal("in-flight failure changed daily TTL")
		}
	}
}

func TestDailyQuotaDiscoveredDuringQueueBackoff(t *testing.T) {
	for _, path := range []string{"execute", "collect", "async"} {
		t.Run(path, func(t *testing.T) {
			resetCooldowns(t)
			fake := installFakeHost(t)
			old := retrySleep
			defer func() { retrySleep = old }()
			retrySleep = func(context.Context, time.Duration) error {
				recordUpstreamFailure("queue-test", "qfmodel", 403, dailyQuotaBody)
				return nil
			}
			var hits int32
			srv := queueFailureGateway(t, "sse", 10, &hits)
			restore := setGatewayBaseForTest(regionCN, srv.URL)
			defer restore()
			switch path {
			case "execute":
				_, err := handleExecExecute(queueExecuteRequest(t))
				assertDailyQuotaError(t, err)
			case "collect":
				_, err := handleExecStream(queueExecuteRequest(t))
				assertDailyQuotaError(t, queueErrorForClient(err))
			case "async":
				prepare := func(ctx context.Context) (*http.Request, context.CancelFunc, error) {
					return buildChatRequest(ctx, testStoredAuth(), "body", "qfmodel")
				}
				pumpUpstreamStream(prepare, func() {}, "daily-backoff", false, "qfmodel", "qfmodel", "", time.Now(), "queue-test", "qfmodel")
				if len(fake.streamErrors()) != 1 {
					t.Fatalf("errors: %v", fake.streamErrors())
				}
				assertDailyQuotaMessage(t, fake.streamErrors()[0])
			}
			if hits != 1 {
				t.Fatalf("called upstream after concurrent daily cap: %d", hits)
			}
		})
	}
}

func TestDailyQuotaCatalogDoesNotNeedRefreshAfterTTL(t *testing.T) {
	resetCooldowns(t)
	base := time.Now()
	now := base
	cooldownNowFn = func() time.Time { return now }
	recordUpstreamFailure("daily-auth", "beta", 403, dailyQuotaBody)
	ids := servingModelIDsFor(t, "daily-auth", []string{"alpha", "beta"})
	if !hasModelID(ids, "beta") || !hasModelID(ids, "alpha") {
		t.Fatalf("wrong cooling catalog: %v", ids)
	}
	assertDailyQuotaError(t, dailyQuotaCooldownError("daily-auth", "beta"))
	now = base.Add(modelCooldownDailyQuota)
	// The previously registered catalog still advertises beta: no refresh is needed.
	if dailyQuotaCooldownError("daily-auth", "beta") != nil {
		t.Fatal("expired guard blocked cached catalog")
	}
	if ids := servingModelIDsFor(t, "daily-auth", []string{"alpha", "beta"}); !hasModelID(ids, "beta") {
		t.Fatalf("catalog did not restore: %v", ids)
	}
}
