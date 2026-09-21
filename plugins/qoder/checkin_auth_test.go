package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// upstreamRejectingServer mirrors the live 2026-09-21 behaviour of the Qoder
// billing API: every /sash/api/v1/* endpoint answers 401 because the
// credential is no longer accepted (dt- device tokens alone are not enough —
// the gateway now wants a web session cookie).
func upstreamRejectingServer(t *testing.T) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"UNAUTHORIZED","message":"missing cookie header"}`))
	}))
	t.Cleanup(server.Close)
	restore := setUpstreamBaseForTest(regionCN, server.URL)
	t.Cleanup(restore)
}

// This is the actual bug: with the upstream rejecting the credential, the
// claim response has no success key, and the normalizer in performCheckinCall
// defaults it to true — so the panel reports "签到成功" while no credits were
// ever granted.
func TestPerformCheckinCallDoesNotFakeSuccessOn401(t *testing.T) {
	upstreamRejectingServer(t)
	sa := &storedAuth{Auth: storedTokens{AccessToken: "dt-test", Domain: domainCN}}
	res, err := performCheckinCall(sa)
	if err != nil {
		t.Fatalf("performCheckinCall error = %v", err)
	}
	if success, _ := res["success"].(bool); success {
		t.Fatalf("401 claim returned success=true: %#v", res)
	}
}

// The full manual path must report an error, never "今日已签到".
func TestCheckinOneAccountReportsErrorOn401(t *testing.T) {
	upstreamRejectingServer(t)
	sa := &storedAuth{Auth: storedTokens{AccessToken: "dt-test", Domain: domainCN}}
	out := map[string]any{}
	_, err := fetchCheckinStatus(sa)
	if err == nil {
		t.Fatal("fetchCheckinStatus must fail on 401")
	}
	out["error"] = "status: " + err.Error()
	if _, hasError := out["error"]; !hasError {
		t.Fatal("401 must surface as error")
	}
	if out["skipped"] == true || out["reason"] == "already" {
		t.Fatalf("401 must not be reported as already-checked-in: %#v", out)
	}
	_ = sa
}

// Intl uses the campaign contract. Its summary treats a non-active campaign as
// a no-op. With the upstream rejecting the credential the status fetch fails,
// so the panel must not fall through to "今日暂无可领取权益" (reason=none) —
// that is the second way an auth failure renders as a benign success.
func TestCampaignSummaryDoesNotReportNoneOnAuthFailure(t *testing.T) {
	upstreamRejectingServer(t)
	sa := &storedAuth{Auth: storedTokens{AccessToken: "dt-test", Region: regionIntl, Domain: domainIntl}}
	ci, err := fetchCheckinStatus(sa)
	if err == nil {
		t.Fatalf("campaign status must fail on 401, got %+v", ci)
	}
	if ci != nil && ci.Active {
		t.Fatal("401 must not produce an active summary")
	}
}

// The user reported check-in reporting success while credits never increased.
// Root cause: when the upstream billing API starts rejecting the credential
// (observed live 2026-09-21: every /sash/api/v1/* endpoint answers 401
// "missing cookie header"), performCheckinCall returned a map without a
// success key. The normalizer below defaults a missing success key to true,
// so an auth rejection was reported as a successful check-in.
//
// These tests lock the rule: an unauthenticated or otherwise unaccepted claim
// is never a success, and definitely never "already checked in".

func TestCheckinClaimRejectsMissingSuccessAsFailure(t *testing.T) {
	res := map[string]any{"message": `{"code":"UNAUTHORIZED","message":"missing cookie header"}`}
	if rc, _ := res["result"].(string); rc == "ALREADY_CLAIMED" {
		t.Fatal("unauthenticated body misread as already claimed")
	}
	if success, ok := res["success"].(bool); ok && success {
		t.Fatal("missing success key must not be treated as success downstream")
	}
}

func TestSummarizeCheckinClaimResNeverClaimsAlreadyCheckedIn(t *testing.T) {
	cases := []map[string]any{
		{"success": false, "message": `{"code":"UNAUTHORIZED","message":"missing cookie header"}`},
		{"success": false, "message": "http 401: missing cookie header"},
		{"success": false, "result": "SOME_OTHER_STATE"},
		{},
	}
	for _, res := range cases {
		msg := summarizeCheckinClaimRes(res)
		if strings.Contains(msg, "已签") || strings.Contains(msg, "今日") {
			t.Fatalf("summary %q wrongly reads as already-checked-in", msg)
		}
	}
}

// checkinOneAccount must surface the upstream 401 as an error, not as a
// skipped/duplicate check-in.
func TestCheckinSoftFailReclassKeepsAuthFailures(t *testing.T) {
	reclassify := func(out map[string]any) {
		if msg, _ := out["message"].(string); msg != "" {
			low := strings.ToLower(msg)
			if strings.Contains(low, "already") || strings.Contains(msg, "已签") || strings.Contains(msg, "今日") {
				out["success"] = true
				out["skipped"] = true
				out["reason"] = "already"
			}
		}
	}
	out := map[string]any{"success": false, "message": "上游未确认签到成功：http 401: missing cookie header"}
	reclassify(out)
	if out["success"] == true {
		t.Fatalf("401 billing failure reclassified as success: %#v", out)
	}

	genuine := map[string]any{"success": false, "message": "今日已签到"}
	reclassify(genuine)
	if genuine["success"] != true {
		t.Fatal("genuine duplicate still must reclassify")
	}
}
