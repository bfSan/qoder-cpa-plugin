package main

import (
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
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
	if err == nil || !isAuthRejectedError(err) {
		t.Fatalf("expected typed auth rejection, got %v", err)
	}
	if success, _ := res["success"].(bool); success {
		t.Fatalf("401 claim returned success=true: %#v", res)
	}
}

// The full manual path must report an error, never "今日已签到".
func TestCheckinOneAccountReportsErrorOn401(t *testing.T) {
	upstreamRejectingServer(t)
	sa := &storedAuth{Auth: storedTokens{AccessToken: "dt-test", Domain: domainCN}}
	resetCheckinTestState(t)
	installStubAuthHost(t, map[string][]byte{"auth-reject": fixtureAuthFile(t, sa, false)})
	out := checkinOneAccount(pluginapi.HostAuthFileEntry{AuthIndex: "auth-reject", ID: "auth-reject"})
	if out["error"] == nil || out["success"] == true || out["reason"] == "already" || out["retry_scheduled"] != false {
		t.Fatalf("auth failure masked: %#v", out)
	}

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
	for _, message := range []string{"今日签到失败", "already unavailable", "已签到功能暂不可用"} {
		t.Run(message, func(t *testing.T) {
			f, _ := checkinHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"success":false,"message":"` + message + `"}`))
					return
				}
				_, _ = w.Write([]byte(campaignListJSON))
			}, false)
			out := checkinOneAccount(f)
			if out["success"] == true || out["reason"] == "already" {
				t.Fatalf("text was mistaken for evidence: %#v", out)
			}
		})
	}
}
