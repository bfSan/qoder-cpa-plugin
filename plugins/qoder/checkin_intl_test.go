package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Intl 签到在 2026-10-21 上游开放：同一个账号通过
// GET /sash/api/v1/me/campaigns 看到 CLAIM_BENEFIT「每天领 100 Credits」，
// POST /{campaignId}/claim 返回 {"status":"CLAIMED"}。
// 这组测试锁住三件事：
//  1. 能力标志与真实调用入口不再把 Intl 拒之门外；
//  2. 定时任务那条路径（processAutoCheckinAccount）确实会为 Intl 发起领取；
//  3. 上游"今天已领"（claimed campaign + replayed）仍渲染成"今日已签到"，
//     不会因为改了门控就把已领报成失败。

const intlCampaignListJSON = `{
  "showCampaign": true,
  "claimable": true,
  "campaigns": [
    {"campaignId":"01a15aa1-693e-7ecd-8404-82cb63a0375a","campaignKey":"act-20261021-460",
     "actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE",
     "benefit":{"kind":"CREDITS","amount":100}},
    {"campaignId":"01a159af-8a04-76b8-b0f4-59d8e2b60583","campaignKey":"act-20261020-944",
     "actionType":"VIEW_DETAILS","claimStatus":"CLAIMED"}
  ]
}`

// intlCampaignServer serves the campaign pair the way openapi.qoder.sh does.
func intlCampaignServer(t *testing.T, claimBody string, claimed *string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/sash/api/v1/me/campaigns":
			_, _ = w.Write([]byte(intlCampaignListJSON))
		case r.Method == http.MethodPost && r.URL.Path == "/sash/api/v1/me/campaigns/01a15aa1-693e-7ecd-8404-82cb63a0375a/claim":
			if claimed != nil {
				*claimed = r.URL.Path
			}
			_, _ = w.Write([]byte(claimBody))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	restore := setUpstreamBaseForTest(regionIntl, srv.URL)
	t.Cleanup(restore)
}

func intlFixtureAuth(t *testing.T, srvReady bool) *storedAuth {
	t.Helper()
	sa := &storedAuth{
		Account: storedAccount{Nickname: "薄枫2", UID: "01a0bc87-453b-7c60-b164-0f60ebe81258"},
		Auth:    storedTokens{Region: regionIntl, Domain: domainIntl, AccessToken: "dt-intl-token"},
	}
	if !srvReady {
		sa.Auth.AccessToken = ""
	}
	return sa
}

// 手动签到必须真的发出 claim 请求并回报成功。
func TestCheckinOneAccountClaimsIntlCampaign(t *testing.T) {
	var claimPath string
	intlCampaignServer(t, `{"status":"CLAIMED"}`, &claimPath)

	sa := intlFixtureAuth(t, true)
	installStubAuthHost(t, map[string][]byte{
		"idx-intl": fixtureAuthFile(t, sa, false),
	})

	entry := pluginapi.HostAuthFileEntry{AuthIndex: "idx-intl", ID: "idx-intl", Name: "qoder-intl.json", Type: providerName}
	out := checkinOneAccount(entry)

	if claimPath == "" {
		t.Fatalf("Intl check-in never claimed the campaign: %#v", out)
	}
	if out["success"] != true {
		t.Fatalf("result = %#v, want success", out)
	}
	if got, _ := out["reward_credits"].(int64); got != 100 {
		t.Fatalf("reward_credits = %#v, want 100", out["reward_credits"])
	}
	if out["reason"] == "unsupported" {
		t.Fatalf("Intl must no longer be reported as unsupported: %#v", out)
	}
}

// 自动调度 tick 走的是另一条入口（processAutoCheckinAccount），
// 它对 supportsCheckin 的判断决定了整个 Intl 账号会不会被跳过。
func TestAutoCheckinClaimsIntlCampaign(t *testing.T) {
	var claimPath string
	intlCampaignServer(t, `{"status":"CLAIMED"}`, &claimPath)

	sa := intlFixtureAuth(t, true)
	installStubAuthHost(t, map[string][]byte{
		"idx-intl": fixtureAuthFile(t, sa, false),
	})
	t.Cleanup(func() {
		if v, ok := accountCache.Load("idx-intl"); ok {
			_ = v
			accountCache.Delete("idx-intl")
		}
	})

	entry := pluginapi.HostAuthFileEntry{AuthIndex: "idx-intl", ID: "idx-intl", Name: "qoder-intl.json", Type: providerName}
	processAutoCheckinAccount(entry, true)

	if claimPath == "" {
		t.Fatal("scheduled tick skipped the Intl account; its campaign was never claimed")
	}
}

// 上游已领取时 claim 返回 replayed:true —— 必须读成"今日已签到"，不是失败。
func TestCheckinOneAccountIntlReplayedIsAlready(t *testing.T) {
	intlCampaignServer(t, `{"status":"CLAIMED","replayed":true}`, nil)

	sa := intlFixtureAuth(t, true)
	installStubAuthHost(t, map[string][]byte{
		"idx-intl": fixtureAuthFile(t, sa, false),
	})

	entry := pluginapi.HostAuthFileEntry{AuthIndex: "idx-intl", ID: "idx-intl", Name: "qoder-intl.json", Type: providerName}
	out := checkinOneAccount(entry)

	if out["success"] != true || out["reason"] != "already" {
		t.Fatalf("replayed claim = %#v, want success + reason=already", out)
	}
	if msg, _ := out["message"].(string); msg != "今日已签到" {
		t.Fatalf("message = %q, want 今日已签到", msg)
	}
}

// 已领取的账号在状态里已被标记为 CLAIMED，手动签到必须直接短路为"已签"，
// 不再发第二次 claim（上游虽幂等，但白跑一趟还会在面板上多一次请求）。
func TestCheckinOneAccountIntlAlreadyClaimedShortCircuits(t *testing.T) {
	var claimPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			claimPath = r.URL.Path
		}
		_, _ = w.Write([]byte(`{"showCampaign":true,"claimable":false,"campaigns":[
		  {"campaignId":"abc","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMED",
		   "benefit":{"kind":"CREDITS","amount":100}}]}`))
	}))
	defer srv.Close()
	restore := setUpstreamBaseForTest(regionIntl, srv.URL)
	defer restore()

	sa := intlFixtureAuth(t, true)
	installStubAuthHost(t, map[string][]byte{
		"idx-intl": fixtureAuthFile(t, sa, false),
	})

	entry := pluginapi.HostAuthFileEntry{AuthIndex: "idx-intl", ID: "idx-intl", Name: "qoder-intl.json", Type: providerName}
	out := checkinOneAccount(entry)

	if claimPath != "" {
		t.Fatalf("claimed campaign was re-claimed at %s", claimPath)
	}
	if out["success"] != true || out["reason"] != "already" {
		t.Fatalf("out = %#v, want already", out)
	}
}
