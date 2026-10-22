package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Regression lock for the 2026-10-22 field report: manual check-in did nothing
// for Intl because claimedCampaign matched EXPIRED campaigns. The upstream
// /me/campaigns list keeps old CLAIM_BENEFIT entries around indefinitely, so an
// unbounded scan reported today_checked_in=true forever and checkinOneAccount
// short-circuited before ever issuing a POST /claim.
//
// claimedCampaign must only recognise a campaign that is BOTH claimed and still
// inside its own [startAt, endAt] window.
func TestClaimedCampaignIgnoresExpiredEntries(t *testing.T) {
	now := time.Now().Unix()
	status := &campaignStatusResponse{
		Campaigns: []campaign{
			{
				CampaignID:  "old",
				ActionType:  "CLAIM_BENEFIT",
				ClaimStatus: "CLAIMED",
				StartAt:     now - 7*24*3600,
				EndAt:       now - 6*24*3600, // expired a week ago
				Benefit:     &campaignBen{Kind: "CREDITS", Amount: 100},
			},
		},
	}

	if got := claimedCampaign(status); got != nil {
		t.Fatalf("expired campaign %q must not count as claimed-today: %#v", got.CampaignID, got)
	}
	sum := campaignCheckinSummary(status)
	if sum.TodayCheckedIn {
		t.Fatal("expired campaign must not set TodayCheckedIn")
	}
	if sum.Active {
		t.Fatal("expired campaign must not keep the activity marked active")
	}
}

// A campaign claimed inside its live window still reports checked-in.
func TestClaimedCampaignAcceptsLiveWindow(t *testing.T) {
	now := time.Now().Unix()
	status := &campaignStatusResponse{
		Campaigns: []campaign{
			{
				CampaignID:  "live",
				ActionType:  "CLAIM_BENEFIT",
				ClaimStatus: "CLAIMED",
				StartAt:     now - 3600,
				EndAt:       now + 3600,
				Benefit:     &campaignBen{Kind: "CREDITS", Amount: 100},
			},
		},
	}

	got := claimedCampaign(status)
	if got == nil || got.CampaignID != "live" {
		t.Fatalf("live claimed campaign not recognised: %#v", got)
	}
	sum := campaignCheckinSummary(status)
	if !sum.TodayCheckedIn || !sum.Active {
		t.Fatalf("live claimed campaign summary = %#v, want checked-in + active", sum)
	}
}

// A campaign whose window has not opened yet must not count as claimed either.
func TestClaimedCampaignIgnoresFutureEntries(t *testing.T) {
	now := time.Now().Unix()
	status := &campaignStatusResponse{
		Campaigns: []campaign{
			{
				CampaignID:  "future",
				ActionType:  "CLAIM_BENEFIT",
				ClaimStatus: "CLAIMED",
				StartAt:     now + 3600,
				EndAt:       now + 7200,
			},
		},
	}
	if got := claimedCampaign(status); got != nil {
		t.Fatalf("future campaign must not count as claimed-today: %#v", got)
	}
}

// End-to-end: the ONLY CLAIM_BENEFIT entry is an expired one. With the old
// unbounded claimedCampaign the summary said "already checked in", so
// checkinOneAccount returned before POSTing anything and the panel's manual
// check-in silently did nothing. With the window filter the expired entry no
// longer counts, so the flow reaches POST /claim.
func TestExpiredCampaignDoesNotBlockTodaysClaim(t *testing.T) {
	now := time.Now().Unix()
	var claimPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			// Yesterday's campaign lingers as CLAIMED; today's is claimable.
			_, _ = w.Write([]byte(`{"showCampaign":true,"claimable":false,"campaigns":[
			  {"campaignId":"old","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMED",
			   "startAt":` + itoa(now-7*24*3600) + `,"endAt":` + itoa(now-6*24*3600) + `,
			   "benefit":{"kind":"CREDITS","amount":100}}
			]}`))
		case r.Method == http.MethodPost:
			claimPath = r.URL.Path
			_, _ = w.Write([]byte(`{"status":"CLAIMED","replayed":false,"claimedAt":"2026-10-22T04:41:11Z"}`))
		default:
			http.NotFound(w, r)
		}
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

	// The old code reported reason=already ("今日已签到") for this account,
	// which is exactly what made the panel's manual check-in look like a no-op.
	// The expired campaign must no longer masquerade as today's grant.
	if out["reason"] == "already" {
		t.Fatalf("expired campaign was reported as already-checked-in: %#v", out)
	}
	if msg, _ := out["message"].(string); msg == "今日已签到" {
		t.Fatalf("expired campaign produced 今日已签到: %#v", out)
	}
	// Nothing claimable today -> honest no-op, never a fake success claim.
	if claimPath != "" {
		t.Fatalf("no claimable campaign but a claim was issued at %s", claimPath)
	}
	if out["reason"] != "none" {
		t.Fatalf("out = %#v, want reason=none", out)
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
