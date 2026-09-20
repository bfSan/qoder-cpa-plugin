package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func campaignTestAuth(serverURL string) (*storedAuth, func()) {
	restore := setUpstreamBaseForTest(regionIntl, serverURL)
	return &storedAuth{
		Auth: storedTokens{Region: regionIntl, AccessToken: "test-token"},
	}, restore
}

const campaignListJSON = `{
  "showCampaign": true,
  "claimable": true,
  "campaigns": [
    {"campaignId":"abc","campaignKey":"act-1","actionType":"CLAIM_BENEFIT",
     "claimStatus":"CLAIMABLE","benefit":{"kind":"CREDITS","amount":100}},
    {"campaignId":"def","campaignKey":"act-2","actionType":"VIEW_DETAILS",
     "claimStatus":"CLAIMABLE"}
  ]
}`

func TestCampaignCheckinSummaryClaimable(t *testing.T) {
	var status campaignStatusResponse
	if err := json.Unmarshal([]byte(campaignListJSON), &status); err != nil {
		t.Fatal(err)
	}
	sum := campaignCheckinSummary(&status)
	if !sum.Active || sum.TodayCheckedIn {
		t.Fatalf("summary = %#v, want active and not yet checked in", sum)
	}
	if sum.DailyCredit != 100 {
		t.Fatalf("daily credit = %d, want 100", sum.DailyCredit)
	}
}

func TestCampaignCheckinSummaryClaimed(t *testing.T) {
	var status campaignStatusResponse
	body := `{"showCampaign":true,"claimable":false,"campaigns":[
	  {"campaignId":"abc","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMED",
	   "benefit":{"kind":"CREDITS","amount":100}}]}`
	if err := json.Unmarshal([]byte(body), &status); err != nil {
		t.Fatal(err)
	}
	sum := campaignCheckinSummary(&status)
	if !sum.TodayCheckedIn || sum.TodayCredit != 100 {
		t.Fatalf("summary = %#v, want claimed with today's credit", sum)
	}
}

func TestCampaignCheckinSummaryIgnoresViewDetailsOnly(t *testing.T) {
	var status campaignStatusResponse
	body := `{"showCampaign":true,"claimable":false,"campaigns":[
	  {"campaignId":"def","actionType":"VIEW_DETAILS","claimStatus":"CLAIMABLE"}]}`
	if err := json.Unmarshal([]byte(body), &status); err != nil {
		t.Fatal(err)
	}
	sum := campaignCheckinSummary(&status)
	if sum.TodayCheckedIn {
		t.Fatalf("summary = %#v, VIEW_DETAILS must not count as a claimable benefit", sum)
	}
}

func TestCampaignClaimableSkipsExpired(t *testing.T) {
	var status campaignStatusResponse
	body := `{"campaigns":[{"campaignId":"old","actionType":"CLAIM_BENEFIT",
	  "claimStatus":"CLAIMABLE","startAt":1,"endAt":2,
	  "benefit":{"kind":"CREDITS","amount":50}}]}`
	if err := json.Unmarshal([]byte(body), &status); err != nil {
		t.Fatal(err)
	}
	if c := claimableCampaign(&status); c != nil {
		t.Fatalf("expired campaign returned: %#v", c)
	}
}

func TestFetchCampaignCheckinSummaryUsesIntlEndpoint(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(campaignListJSON))
	}))
	defer srv.Close()

	sa, restore := campaignTestAuth(srv.URL)
	defer restore()

	sum, err := fetchCampaignCheckinSummary(sa)
	if err != nil {
		t.Fatalf("fetchCampaignCheckinSummary: %v", err)
	}
	if gotPath != "/sash/api/v1/me/campaigns" {
		t.Fatalf("path = %q, want /sash/api/v1/me/campaigns", gotPath)
	}
	if gotAuth != "Bearer test-token" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if !sum.Active || sum.TodayCheckedIn {
		t.Fatalf("summary = %#v", sum)
	}
}

func TestPerformCampaignCheckinClaimsFirstClaimable(t *testing.T) {
	var claimPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/sash/api/v1/me/campaigns":
			_, _ = w.Write([]byte(campaignListJSON))
		case r.Method == http.MethodPost && r.URL.Path == "/sash/api/v1/me/campaigns/abc/claim":
			claimPath = r.URL.Path
			_, _ = w.Write([]byte(`{"status":"CLAIMED"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	sa, restore := campaignTestAuth(srv.URL)
	defer restore()

	res, err := performCampaignCheckin(sa)
	if err != nil {
		t.Fatalf("performCampaignCheckin: %v", err)
	}
	if claimPath != "/sash/api/v1/me/campaigns/abc/claim" {
		t.Fatalf("claim path = %q", claimPath)
	}
	if res["success"] != true {
		t.Fatalf("result = %#v, want success", res)
	}
	if res["result"] != "CLAIMED" {
		t.Fatalf("result marker = %#v", res["result"])
	}
}

func TestPerformCampaignCheckinAlreadyClaimed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"showCampaign":true,"claimable":false,"campaigns":[
		  {"campaignId":"abc","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMED"}]}`))
	}))
	defer srv.Close()

	sa, restore := campaignTestAuth(srv.URL)
	defer restore()

	res, err := performCampaignCheckin(sa)
	if err != nil {
		t.Fatalf("performCampaignCheckin: %v", err)
	}
	if res["result"] != "ALREADY_CLAIMED" {
		t.Fatalf("result = %#v, want ALREADY_CLAIMED", res)
	}
}
