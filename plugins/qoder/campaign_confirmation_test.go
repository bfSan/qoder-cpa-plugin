package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestCampaignCNLostPostNeverFallsBackToLegacyClaim(t *testing.T) {
	resetCheckinTestState(t)
	var campaignPosts, legacyCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "daily-check-in") {
			legacyCalls.Add(1)
			_, _ = w.Write([]byte(`{"success":true}`))
			return
		}
		if r.Method == http.MethodPost {
			campaignPosts.Add(1)
			w.WriteHeader(500)
			return
		}
		_, _ = w.Write([]byte(campaignListJSON))
	}))
	defer srv.Close()
	defer setUpstreamBaseForTest(regionCN, srv.URL)()
	sa := &storedAuth{Auth: storedTokens{Region: regionCN, Domain: domainCN, AccessToken: "token"}}
	installStubAuthHost(t, map[string][]byte{"cn-uncertain": fixtureAuthFile(t, sa, false)})
	f := pluginapi.HostAuthFileEntry{AuthIndex: "cn-uncertain", ID: "cn-uncertain"}
	defer accountCache.Delete(f.ID)
	for i := 0; i < 2; i++ {
		out := checkinOneAccount(f)
		if out["success"] == true || out["state"] != "pending_confirmation" {
			t.Fatalf("unexpected result: %#v", out)
		}
	}
	if campaignPosts.Load() != 1 || legacyCalls.Load() != 0 {
		t.Fatalf("unsafe fallback: posts=%d legacy=%d", campaignPosts.Load(), legacyCalls.Load())
	}
}

func TestCampaignCNLegacyFallbackRequiresExplicitMissingSurface(t *testing.T) {
	for _, status := range []int{404, 401, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			resetCheckinTestState(t)
			var posts, legacyGets atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "campaigns") {
					w.WriteHeader(status)
					return
				}
				if r.Method == http.MethodPost {
					posts.Add(1)
					_, _ = w.Write([]byte(`{"success":true,"rewardCredits":100}`))
					return
				}
				legacyGets.Add(1)
				if posts.Load() > 0 {
					_, _ = w.Write([]byte(`{"status":"CLAIMED","lastClaimedAt":` + itoa(time.Now().Unix()) + `}`))
				} else {
					_, _ = w.Write([]byte(`{"status":"CLAIMABLE"}`))
				}
			}))
			defer srv.Close()
			defer setUpstreamBaseForTest(regionCN, srv.URL)()
			sa := &storedAuth{Auth: storedTokens{Region: regionCN, Domain: domainCN, AccessToken: "token"}}
			installStubAuthHost(t, map[string][]byte{"cn-fallback": fixtureAuthFile(t, sa, false)})
			defer accountCache.Delete("cn-fallback")
			out := checkinOneAccount(pluginapi.HostAuthFileEntry{AuthIndex: "cn-fallback", ID: "cn-fallback"})
			if status == 404 {
				if out["success"] != true || posts.Load() != 1 || legacyGets.Load() < 2 {
					t.Fatalf("legacy ability lost: %#v posts=%d gets=%d", out, posts.Load(), legacyGets.Load())
				}
			} else if out["success"] == true || posts.Load() != 0 || legacyGets.Load() != 0 {
				t.Fatalf("blind fallback: %#v posts=%d legacy=%d", out, posts.Load(), legacyGets.Load())
			}
		})
	}
}

func TestCampaignRateLimitRetriesAfterFreshStatus(t *testing.T) {
	var posts atomic.Int32
	f, _ := checkinHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if posts.Add(1) == 1 {
				w.WriteHeader(429)
				return
			}
			_, _ = w.Write([]byte(`{"status":"CLAIMED"}`))
			return
		}
		if strings.Contains(r.URL.Path, "campaigns") {
			_, _ = w.Write([]byte(campaignListJSON))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}, false)
	out := checkinOneAccount(f)
	if out["success"] == true || out["retry_scheduled"] != true {
		t.Fatalf("429 classification: %#v", out)
	}
	checkinRuns.Lock()
	r := checkinRuns.records[f.AuthIndex]
	r.next = time.Now().Add(-time.Second)
	checkinRuns.records[f.AuthIndex] = r
	checkinRuns.Unlock()
	runDueCheckinRetries()
	r = checkinRuns.records[f.AuthIndex]
	if posts.Load() != 2 || !r.confirmed || r.retries != 1 || !r.next.IsZero() {
		t.Fatalf("429 retry failed: posts=%d record=%+v", posts.Load(), r)
	}
}

func TestCampaignStructuredAuthRejectionIsPermanent(t *testing.T) {
	for _, body := range []string{`{"code":"UNAUTHORIZED"}`, `{"data":{"errorCode":"FORBIDDEN"}}`, `{"status":"NOT_ELIGIBLE"}`} {
		t.Run(body, func(t *testing.T) {
			f, _ := checkinHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					_, _ = w.Write([]byte(body))
					return
				}
				_, _ = w.Write([]byte(campaignListJSON))
			}, false)
			out := checkinOneAccount(f)
			if out["success"] == true || out["retry_scheduled"] != false || out["error"] == nil {
				t.Fatalf("business failure retried: %#v", out)
			}
		})
	}
}

func TestCampaignUncertainCheckpointSurvivesNewDay(t *testing.T) {
	resetCheckinTestState(t)
	now := time.Now()
	checkinRuns.records["uncertain"] = checkinRunRecord{day: now.Add(-24 * time.Hour).Format("2006-01-02"), confirmOnly: true, campaignID: "multi-day", retries: 3}
	r, ok := reserveCheckin("uncertain", "id", "manual", now)
	if !ok || !r.confirmOnly || r.campaignID != "multi-day" || r.retries != 0 {
		t.Fatalf("cross-day checkpoint lost: %+v", r)
	}
	// New day resets the retry budget, never permission to replay the POST.
}

func TestCampaignConfirmationNotLostAfterLaterStatusFailure(t *testing.T) {
	var posts, reads atomic.Int32
	f, _ := checkinHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			_, _ = w.Write([]byte(`{"status":"CLAIMED"}`))
			return
		}
		if strings.Contains(r.URL.Path, "campaigns") {
			if reads.Add(1) == 2 {
				w.WriteHeader(503)
				return
			}
			_, _ = w.Write([]byte(campaignListJSON))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}, false)
	if out := checkinOneAccount(f); out["success"] != true {
		t.Fatalf("first=%#v", out)
	}
	if out := checkinOneAccount(f); out["success"] == true {
		t.Fatalf("failed status reported success=%#v", out)
	}
	if out := checkinOneAccount(f); out["success"] != true || posts.Load() != 1 {
		t.Fatalf("lost old confirmation: %#v posts=%d", out, posts.Load())
	}
}

func TestCampaignExplicitInactiveWithoutListIsNotAlready(t *testing.T) {
	var posts atomic.Int32
	f, _ := checkinHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		_, _ = w.Write([]byte(`{"showCampaign":false,"claimable":false}`))
	}, false)
	out := checkinOneAccount(f)
	if out["success"] == true || out["reason"] != "none" || posts.Load() != 0 {
		t.Fatalf("inactive incorrectly confirmed: %#v", out)
	}
}

func TestCampaignAlreadyReceiptDoesNotExtendGrace(t *testing.T) {
	f, _ := checkinHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"status":"CLAIMED"}`))
			return
		}
		if strings.Contains(r.URL.Path, "campaigns") {
			_, _ = w.Write([]byte(campaignListJSON))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}, false)
	checkinOneAccount(f)
	before, _ := accountCache.Load(f.ID)
	at := before.(*accountCacheEntry).checkinAt
	checkinOneAccount(f)
	after, _ := accountCache.Load(f.ID)
	if !after.(*accountCacheEntry).checkinAt.Equal(at) {
		t.Fatal("re-reading receipt extended grace")
	}
}

func TestCampaignRedirectAnd408NeverReplayClaim(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusSeeOther, http.StatusRequestTimeout} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var posts atomic.Int32
			f, _ := checkinHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts.Add(1)
					w.Header().Set("Location", "/must-not-follow")
					w.WriteHeader(status)
					return
				}
				_, _ = w.Write([]byte(campaignListJSON))
			}, false)
			for i := 0; i < 2; i++ {
				if out := checkinOneAccount(f); out["success"] == true || out["state"] != "pending_confirmation" {
					t.Fatalf("unverified redirect/timeout: %#v", out)
				}
			}
			if posts.Load() != 1 {
				t.Fatalf("redirect/timeout replayed %d POSTs", posts.Load())
			}
		})
	}
}
