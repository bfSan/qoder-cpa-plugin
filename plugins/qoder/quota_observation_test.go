package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestQuotaObservationNarrowClassification(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   cooldownReason
	}{
		{403, dailyQuotaBody, cooldownReasonDailyQuota},
		{403, dailyQuotaBody + " shared credits exhausted", cooldownReasonDailyQuota},
		{403, queueAdmissionBody + " shared credits exhausted", ""},
		{403, `{"code":110,"message":"invalid signature"}`, ""},
		{403, `{"code":110,"message":"insufficient credits; invalid signature"}`, ""},
		{401, `{"message":"shared credits exhausted"}`, ""},
		{402, `{"message":"payment required"}`, ""},
		{403, `{"code":6004,"message":"unknown"}`, ""},
		{403, `{"code":110,"message":"Daily limit"}`, ""},
		{200, dailyQuotaBody, ""},
		{402, `{"message":"insufficient credits"}`, cooldownReasonModelCredit},
		{403, `{"message":"quota exceeded"}`, cooldownReasonModelQuota},
		{403, `{"message":"account credits exhausted"}`, cooldownReasonSharedCredit},
		{402, `{"scope":"account","message":"insufficient credits"}`, cooldownReasonSharedCredit},
		{402, `{"scope":"model","message":"insufficient credits"}`, cooldownReasonModelCredit},
	} {
		if got := classifyQuotaReason(tc.status, tc.body); got != tc.want {
			t.Fatalf("%d %s: got %s want %s", tc.status, tc.body, got, tc.want)
		}
	}
}

func TestQuotaExplicitResetParsing(t *testing.T) {
	now := time.Date(2026, 10, 22, 11, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		h      http.Header
		body   string
		want   time.Time
		source string
	}{
		{"retry seconds", http.Header{"Retry-After": {"90"}}, "", now.Add(90 * time.Second), "retry_after"},
		{"retry date", http.Header{"Retry-After": {now.Add(time.Hour).Format(http.TimeFormat)}}, "", now.Add(time.Hour), "retry_after"},
		{"reset epoch", http.Header{"X-Ratelimit-Reset": {fmt.Sprint(now.Add(time.Hour).Unix())}}, "", now.Add(time.Hour), "reset_header"},
		{"reset ms", nil, fmt.Sprintf(`{"reset_at":%d}`, now.Add(time.Hour).UnixMilli()), now.Add(time.Hour), "reset_body"},
		{"reset iso", nil, `{"resetAt":"2026-10-22T20:00:00+08:00"}`, now.Add(time.Hour), "reset_body"},
		{"nested retry", nil, `{"message":"{\"retryAfterSeconds\":120}"}`, now.Add(120 * time.Second), "retry_after_body"},
		{"no midnight", nil, `{"message":"daily quota exhausted"}`, time.Time{}, ""},
		{"invalid", http.Header{"Retry-After": {"-1"}, "X-Quota-Reset": {"garbage"}}, `{"resetAt":"2026-10-22 23:59:59"}`, time.Time{}, ""},
		{"expired", http.Header{"Retry-After": {"0"}}, `{"reset_at":1}`, time.Time{}, ""},
		{"overflow", http.Header{"Retry-After": {"99999999999999999999"}}, "", time.Time{}, ""},
		{"user content ignored", nil, `{"choices":[{"message":{"content":"resetAt"}}],"content":{"reset_at":"2026-10-22T12:00:00Z"}}`, time.Time{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseQuotaHint(tc.h, tc.body, now)
			if !got.ResetAt.Equal(tc.want) || got.Source != tc.source {
				t.Fatalf("got %+v want %s %s", got, tc.want, tc.source)
			}
		})
	}
}

func TestQuotaSelectiveRecoveryAndTTL(t *testing.T) {
	resetCooldowns(t)
	now := time.Date(2026, 10, 22, 11, 0, 0, 0, time.UTC)
	cooldownNowFn = func() time.Time { return now }
	recordUpstreamFailure("a", "daily", 403, dailyQuotaBody)
	recordUpstreamFailure("a", "paid", 402, `{"message":"shared credits exhausted"}`)
	recordUpstreamFailure("b", "paid", 402, `{"message":"shared credits exhausted"}`)
	if dailyQuotaCooldownError("a", "free") == nil {
		t.Fatal("explicit shared evidence must guard all models")
	}
	for _, cr := range []*creditsSummary{nil, {}, {TotalRemain: 0, TotalUsed: 100, PackCount: 1}} {
		if observePositiveCredits("a", cr) != 0 {
			t.Fatal("unknown/zero cleared freeze")
		}
	}
	if observePositiveCredits("a", &creditsSummary{TotalRemain: 1}) != 1 {
		t.Fatal("positive balance did not clear shared freeze")
	}
	if dailyQuotaCooldownError("a", "free") != nil {
		t.Fatal("shared freeze survived fresh balance")
	}
	assertDailyQuotaError(t, dailyQuotaCooldownError("a", "daily"))
	if dailyQuotaCooldownError("b", "free") == nil {
		t.Fatal("another account was cleared")
	}
	rows := quotaObservationSnapshotFor("a")
	if len(rows) != 1 || rows[0]["scope"] != "model" || rows[0]["occurred_at"] == nil || rows[0]["next_probe_at"] == nil || rows[0]["recovery"] == nil || rows[0]["persistent"] != false {
		t.Fatalf("missing explanation: %+v", rows)
	}
	now = now.Add(modelCooldownDailyQuota)
	if dailyQuotaCooldownError("a", "daily") != nil || dailyQuotaCooldownError("b", "free") != nil {
		t.Fatal("finite TTL did not expire")
	}
}

func TestQuotaResetSurvivesFoldAndPublicBoundary(t *testing.T) {
	resetCooldowns(t)
	base := time.Date(2026, 10, 22, 11, 0, 0, 0, time.UTC)
	cooldownNowFn = func() time.Time { return base }
	wire, _ := json.Marshal(map[string]any{"body": dailyQuotaBody, "statusCodeValue": 403, "headers": map[string]any{"Retry-After": []string{"120"}}})
	_, err := aggregateQoderSSE(strings.NewReader("data:"+string(wire)+"\n\n"), "daily")
	assertDailyQuotaError(t, queueErrorForClient(err))
	recordUpstreamError("a", "daily", 403, err)
	if until := modelCoolingUntil("a", "daily"); !until.Equal(base.Add(2 * time.Minute)) {
		t.Fatalf("lost explicit reset: %s", until)
	}
	if row := quotaObservationSnapshotFor("a")[0]; row["reset_source"] != "retry_after" {
		t.Fatalf("missing provenance %+v", row)
	}
}

func TestQuotaErrorSafeAllBoundaries(t *testing.T) {
	body := `{"scope":"account","message":"insufficient credits Bearer secret-test 401 403"}`
	for _, err := range []error{chatUpstreamError(403, body), upstreamEnvelopeError(403, body), upstreamStatusError(403, chatUpstreamError(403, body)), queueErrorForClient(&statusError{status: 403, err: chatUpstreamError(403, body)})} {
		if err == nil || strings.Contains(err.Error(), "secret-test") || strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "unexpected EOF") {
			t.Fatalf("unsafe %v", err)
		}
		var sc interface{ StatusCode() int }
		if errors.As(err, &sc) {
			t.Fatalf("leaked status %d", sc.StatusCode())
		}
		var env envelope
		_ = json.Unmarshal(errorEnvelopeFor(err), &env)
		if env.Error == nil || env.Error.Code != "quota_exceeded" || env.Error.HTTPStatus != 0 {
			t.Fatalf("bad RPC %+v", env.Error)
		}
	}
	if msg := streamEnvelopeError(403, body); strings.Contains(msg, "403") || strings.Contains(msg, "secret-test") {
		t.Fatal(msg)
	}
	auth := chatUpstreamError(403, `{"code":110,"message":"invalid signature"}`)
	if strings.Contains(auth.Error(), "quota exceeded") || !strings.Contains(auth.Error(), "403") {
		t.Fatalf("real auth masked %v", auth)
	}
}

func TestQuotaHTTP200ErrorShapes(t *testing.T) {
	for _, payload := range []string{
		`{"body":{"code":110,"message":"Billing daily count exceeded"},"statusCodeValue":403}`,
		`{"body":"{\"code\":110,\"message\":\"Billing daily count exceeded\"}","statusCode":"403"}`,
	} {
		_, err := aggregateQoderSSE(strings.NewReader("data:"+payload+"\n\n"), "daily")
		assertDailyQuotaError(t, queueErrorForClient(err))
	}
	for _, payload := range []string{`{"code":110,"message":"invalid signature"}`, `{"body":{"code":110,"message":"invalid signature"},"statusCodeValue":403}`} {
		_, err := aggregateQoderSSE(strings.NewReader("data:"+payload+"\n\n"), "daily")
		if err == nil || strings.Contains(err.Error(), "quota exceeded") || !strings.Contains(err.Error(), "invalid signature") {
			t.Fatalf("auth shape masked: %v", err)
		}
	}
	_, err := aggregateQoderSSE(strings.NewReader("data:"+dailyQuotaBody+"\n\n"), "daily")
	var daily *dailyQuotaError
	if errors.As(err, &daily) {
		t.Fatal("status-less 110 invented daily 403")
	}
}

func TestQuotaStreamAfterOutputNeverReplays(t *testing.T) {
	resetCooldowns(t)
	fake := installFakeHost(t)
	installFastRetry(t)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = fmt.Fprint(w, "data:"+`{"body":"{\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}","statusCodeValue":200}`+"\n\n")
		e, _ := json.Marshal(map[string]any{"body": dailyQuotaBody, "statusCodeValue": 403})
		_, _ = fmt.Fprintf(w, "data:%s\n\n", e)
	}))
	defer srv.Close()
	restore := setGatewayBaseForTest(regionCN, srv.URL)
	defer restore()
	prepare := func(ctx context.Context) (*http.Request, context.CancelFunc, error) {
		return buildChatRequest(ctx, testStoredAuth(), "body", "qfmodel")
	}
	pumpUpstreamStream(prepare, func() {}, "quota-after-output", false, "qfmodel", "qfmodel", "", time.Now(), "a", "qfmodel")
	if hits != 1 || len(fake.chunks()) != 1 || len(fake.streamErrors()) != 1 {
		t.Fatalf("hits=%d chunks=%d errors=%v", hits, len(fake.chunks()), fake.streamErrors())
	}
	assertDailyQuotaMessage(t, fake.streamErrors()[0])
}

func TestQuotaSharedGuardAllDispatchPaths(t *testing.T) {
	for _, path := range []string{"execute", "collect", "async"} {
		t.Run(path, func(t *testing.T) {
			resetCooldowns(t)
			fake := installFakeHost(t)
			recordUpstreamFailure("queue-test", "qfmodel", 403, `{"message":"shared credits exhausted"}`)
			var hits int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&hits, 1) }))
			defer srv.Close()
			restore := setGatewayBaseForTest(regionCN, srv.URL)
			defer restore()
			var err error
			switch path {
			case "execute":
				_, err = handleExecExecute(queueExecuteRequest(t))
			case "collect":
				_, err = handleExecStream(queueExecuteRequest(t))
			case "async":
				prepare := func(ctx context.Context) (*http.Request, context.CancelFunc, error) {
					return buildChatRequest(ctx, testStoredAuth(), "body", "qfmodel")
				}
				pumpUpstreamStream(prepare, func() {}, "shared-guard", false, "qfmodel", "qfmodel", "", time.Now(), "queue-test", "qfmodel")
				if len(fake.streamErrors()) != 1 {
					t.Fatal(fake.streamErrors())
				}
				err = errors.New(fake.streamErrors()[0])
			}
			if err == nil || !strings.Contains(err.Error(), "SHARED_CREDIT_EXHAUSTED") || hits != 0 {
				t.Fatalf("dispatch escaped guard: %v hits=%d", err, hits)
			}
		})
	}
}

func TestQuotaClearDoesNotReadOrEnablePhysical(t *testing.T) {
	resetCooldowns(t)
	old := hostCall
	hostCall = func(string, []byte) ([]byte, error) { t.Fatal("quota clear touched auth"); return nil, nil }
	defer func() { hostCall = old }()
	recordUpstreamFailure("a", "daily", 403, dailyQuotaBody)
	recordUpstreamFailure("a", "paid", 403, `{"message":"shared credits exhausted"}`)
	if clearQuotaObservations("a", "daily") != 1 || dailyQuotaCooldownError("a", "free") == nil {
		t.Fatal("model clear removed shared freeze")
	}
	if clearQuotaObservations("a", "") != 1 || dailyQuotaCooldownError("a", "free") != nil {
		t.Fatal("account clear failed")
	}
}

func TestLifecycleManualDisableWinsLockRace(t *testing.T) {
	sa := testStoredAuthCNAccount()
	fillFixtureToken(sa)
	raw, _ := buildAuthFileJSON(sa, true, "CN · 已禁用", map[string]any{"disabled_reason": "exhausted"})
	oldGet, oldSave := hostAuthGetPhysicalFn, hostAuthPersistMigrateFn
	defer func() { hostAuthGetPhysicalFn = oldGet; hostAuthPersistMigrateFn = oldSave }()
	var saves int
	hostAuthGetPhysicalFn = func(string) (*hostAuthPhysical, error) {
		return &hostAuthPhysical{Name: "qoder-cn-race.json", JSON: raw}, nil
	}
	hostAuthPersistMigrateFn = func(_, _, _ string, _ []byte) error { saves++; return nil }
	mu := checkinLockFor("manual-race")
	mu.Lock()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- reenableAuth("manual-race", "manual-race", sa, &creditsSummary{TotalRemain: 100})
	}()
	<-started
	raw, _ = buildAuthFileJSON(sa, true, "CN · 已禁用", map[string]any{"disabled_reason": disableReasonManual})
	mu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if saves != 0 {
		t.Fatal("manual disable overwritten after stale observation")
	}
}

func TestLifecycleUnknownDisableAndReadFailureProtected(t *testing.T) {
	sa := testStoredAuthCNAccount()
	fillFixtureToken(sa)
	for _, reason := range []string{"", "manual", "unknown", "session_dead"} {
		raw, _ := buildAuthFileJSON(sa, true, "CN · 已禁用 · 余100", map[string]any{"disabled_reason": reason})
		if legacyExhaustedDisable(raw) {
			t.Fatalf("unknown/manual reason accepted: %q", reason)
		}
	}
	oldGet, oldSave := hostAuthGetPhysicalFn, hostAuthPersistMigrateFn
	defer func() { hostAuthGetPhysicalFn = oldGet; hostAuthPersistMigrateFn = oldSave }()
	hostAuthGetPhysicalFn = func(string) (*hostAuthPhysical, error) { return nil, errors.New("physical unavailable") }
	hostAuthPersistMigrateFn = func(_, _, _ string, _ []byte) error { t.Fatal("saved stale auth after read failure"); return nil }
	if syncAuthNote("failed-read", "failed-read", sa, &creditsSummary{TotalRemain: 100}, false) == nil {
		t.Fatal("missing physical error")
	}
	if reenableAuth("failed-read", "failed-read", sa, &creditsSummary{TotalRemain: 100}) == nil {
		t.Fatal("missing physical error")
	}
}

func TestLifecycleSyncPreservesManualAndRotatedAuth(t *testing.T) {
	sa := testStoredAuthCNAccount()
	fillFixtureToken(sa)
	raw, _ := buildAuthFileJSON(sa, true, "CN · 已禁用", map[string]any{"disabled_reason": "manual", "extra": "preserve"})
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	doc["auth"].(map[string]any)["accessToken"] = "rotated"
	raw, _ = json.Marshal(doc)
	oldGet, oldSave := hostAuthGetPhysicalFn, hostAuthPersistMigrateFn
	defer func() { hostAuthGetPhysicalFn = oldGet; hostAuthPersistMigrateFn = oldSave }()
	hostAuthGetPhysicalFn = func(string) (*hostAuthPhysical, error) {
		return &hostAuthPhysical{Name: "qoder-cn-sync.json", JSON: raw}, nil
	}
	var saved []byte
	hostAuthPersistMigrateFn = func(_, _, _ string, b []byte) error { saved = b; return nil }
	if err := syncAuthNote("manual-sync", "manual-sync", sa, &creditsSummary{TotalRemain: 100}, false); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(saved, &doc)
	if doc["disabled"] != true || doc["disabled_reason"] != "manual" || doc["extra"] != "preserve" || doc["auth"].(map[string]any)["accessToken"] != "rotated" {
		t.Fatalf("physical fields overwritten %s", saved)
	}
	lifecycleState.Delete("manual-sync")
}

func TestQuotaOlderBalanceCannotClearNewFreeze(t *testing.T) {
	resetCooldowns(t)
	now := time.Now()
	cooldownNowFn = func() time.Time { return now }
	recordUpstreamFailure("a", "paid", 403, `{"message":"shared credits exhausted"}`)
	if observePositiveCreditsAt("a", &creditsSummary{TotalRemain: 100}, now.Add(-time.Second)) != 0 {
		t.Fatal("in-flight older balance cleared newer freeze")
	}
	if observePositiveCreditsAt("a", &creditsSummary{TotalRemain: 100}, now.Add(time.Second)) != 1 {
		t.Fatal("new positive balance did not clear freeze")
	}
}

func TestQuotaCompactJSONErrorOnHTTP200(t *testing.T) {
	resetCooldowns(t)
	payload := `{"body":{"code":110,"message":"Billing daily count exceeded"},"statusCodeValue":403}`
	env, ok := parseUpstreamLine(payload)
	if !ok || upstreamEnvelopeStatus(env) != 403 {
		t.Fatal("raw JSON error not decoded")
	}
	_, err := aggregateQoderSSE(strings.NewReader(payload), "daily")
	assertDailyQuotaError(t, queueErrorForClient(err))
	if _, ok := parseUpstreamLine(`{"choices":[{"delta":{"content":"hello"}}]}`); ok {
		t.Fatal("bare success JSON became an error frame")
	}
}

func TestQuotaQueueHintsRemainModelScoped(t *testing.T) {
	resetCooldowns(t)
	now := time.Now()
	cooldownNowFn = func() time.Time { return now }
	err := attachQuotaHeaders(newQueueAdmissionError(403, queueAdmissionBody), http.Header{"Retry-After": {"60"}})
	recordUpstreamError("a", "queued", 403, err)
	if got := modelCoolingUntil("a", "queued"); !got.Equal(now.Add(time.Minute)) {
		t.Fatalf("queue lost explicit header: %s", got)
	}
	if modelIsCooling("a", "other") {
		t.Fatal("queue hint froze account")
	}
	assertQueueClientError(t, queueErrorForClient(err))
}

func TestQuotaFreezeCannotBeDowngradedByInflightTransport(t *testing.T) {
	resetCooldowns(t)
	recordUpstreamFailure("a", "paid", 402, `{"message":"insufficient credits"}`)
	until := modelCoolingUntil("a", "paid")
	recordUpstreamFailure("a", "paid", 429, "rate limit")
	recordUpstreamFailure("a", "paid", 0, "empty_stream")
	if !modelCoolingUntil("a", "paid").Equal(until) || dailyQuotaCooldownError("a", "paid") == nil {
		t.Fatal("in-flight transient replaced quota freeze")
	}
}

func TestQuotaQueueNumericBusinessCode(t *testing.T) {
	for _, body := range []string{`{"code": 10605,"message":"queued"}`, `{"code" : "10605", "message":"queued"}`} {
		assertQueueClientError(t, chatUpstreamError(403, body))
	}
}

func TestLifecycleOnlyExplicitOldExhaustionMigrates(t *testing.T) {
	sa := testStoredAuthCNAccount()
	fillFixtureToken(sa)
	oldGet, oldSave := hostAuthGetPhysicalFn, hostAuthPersistMigrateFn
	defer func() { hostAuthGetPhysicalFn, hostAuthPersistMigrateFn = oldGet, oldSave }()
	for _, reason := range []string{"", "unknown", "manual", "exhausted", "credits_exhausted"} {
		raw, _ := buildAuthFileJSON(sa, true, "CN · 已禁用", map[string]any{"disabled_reason": reason})
		hostAuthGetPhysicalFn = func(string) (*hostAuthPhysical, error) {
			return &hostAuthPhysical{Name: "qoder-cn-old.json", JSON: raw}, nil
		}
		saved := false
		hostAuthPersistMigrateFn = func(_, _, _ string, b []byte) error {
			saved = true
			if parseDisabledFromAuthJSON(b) {
				t.Fatal("migration remained disabled")
			}
			return nil
		}
		changed, err := reenableAuthChanged("old-migrate", "old-migrate", sa, &creditsSummary{TotalRemain: 0, TotalUsed: 100})
		want := reason == "exhausted" || reason == "credits_exhausted"
		if err != nil || changed != want || saved != want {
			t.Fatalf("reason %q changed=%v saved=%v err=%v", reason, changed, saved, err)
		}
	}
	lifecycleState.Delete("old-migrate")
}
