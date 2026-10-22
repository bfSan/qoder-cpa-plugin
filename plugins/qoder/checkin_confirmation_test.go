package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func resetCheckinTestState(t *testing.T) {
	t.Helper()
	checkinRuns.Lock()
	old := checkinRuns.records
	checkinRuns.records = make(map[string]checkinRunRecord)
	checkinRuns.Unlock()
	checkinAutoMu.Lock()
	oldAuto := checkinAuto
	checkinAuto = true
	checkinAutoMu.Unlock()
	t.Cleanup(func() {
		checkinRuns.Lock()
		checkinRuns.records = old
		checkinRuns.Unlock()
		checkinAutoMu.Lock()
		checkinAuto = oldAuto
		checkinAutoMu.Unlock()
	})
}

func checkinHTTPFixture(t *testing.T, handler http.HandlerFunc, disabled bool) (pluginapi.HostAuthFileEntry, *stubAuthHost) {
	t.Helper()
	resetCheckinTestState(t)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Cleanup(setUpstreamBaseForTest(regionIntl, srv.URL))
	sa := &storedAuth{Auth: storedTokens{Region: regionIntl, Domain: domainIntl, AccessToken: "test-token"}}
	idx := "confirmation-" + t.Name()
	stub := installStubAuthHost(t, map[string][]byte{idx: fixtureAuthFile(t, sa, disabled)})
	t.Cleanup(func() { accountCache.Delete(idx) })
	return pluginapi.HostAuthFileEntry{AuthIndex: idx, ID: idx, Name: "qoder-test.json"}, stub
}

func TestCheckinCampaignReceiptSurvivesDisappearingList(t *testing.T) {
	var posts atomic.Int32
	f, _ := checkinHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			posts.Add(1)
			_, _ = w.Write([]byte(`{"data":{"status":"CLAIMED","grantId":"grant-1","claimedAt":"2026-10-22T04:41:11Z"}}`))
		case strings.Contains(r.URL.Path, "campaigns"):
			if posts.Load() == 0 {
				_, _ = w.Write([]byte(campaignListJSON))
			} else {
				_, _ = w.Write([]byte(`{"campaigns":[]}`))
			}
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}, false)
	body, _ := json.Marshal(map[string]string{"auth_index": f.AuthIndex})
	out := handleManualCheckin(pluginapi.ManagementRequest{Body: body})
	result := out["results"].([]map[string]any)[0]
	if result["success"] != true || result["grant_id"] != "grant-1" || result["claimed_at"] == nil {
		t.Fatalf("receipt not confirmed: %#v", result)
	}
	second := checkinOneAccount(f)
	if second["success"] != true || second["reason"] != "already" || posts.Load() != 1 {
		t.Fatalf("lost confirmation/replayed claim: %#v posts=%d", second, posts.Load())
	}
	_, ci, _, _ := cachedAccountDetails(f.ID, &storedAuth{Auth: storedTokens{Region: regionIntl, AccessToken: "test-token"}}, true)
	if ci == nil || !ci.TodayCheckedIn || !checkinGraceActive(f.ID) {
		t.Fatalf("cache lost receipt/grace: %#v", ci)
	}
}

func TestCheckinUnknownStatusNeverPosts(t *testing.T) {
	for _, body := range []string{`{}`, `null`, `{"campaigns":[{"campaignId":"x","actionType":"CLAIM_BENEFIT","claimStatus":"UNKNOWN"}]}`, `{"campaigns":[{"actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE"}]}`} {
		t.Run(body, func(t *testing.T) {
			var posts atomic.Int32
			f, _ := checkinHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					posts.Add(1)
				}
				_, _ = w.Write([]byte(body))
			}, false)
			out := checkinOneAccount(f)
			if out["success"] == true || posts.Load() != 0 {
				t.Fatalf("unknown status accepted: %#v posts=%d", out, posts.Load())
			}
		})
	}
}

func TestCheckinUncertainPostRetriesOnlyConfirmation(t *testing.T) {
	for _, response := range []string{`{}`, `{"message":"今日领取失败"}`, `not-json`} {
		t.Run(response, func(t *testing.T) {
			var posts atomic.Int32
			f, _ := checkinHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					posts.Add(1)
					_, _ = w.Write([]byte(response))
					return
				}
				_, _ = w.Write([]byte(campaignListJSON))
			}, false)
			out := checkinOneAccount(f)
			if out["success"] == true || out["state"] != "pending_confirmation" {
				t.Fatalf("unconfirmed claimed success: %#v", out)
			}
			for i := 0; i < 3; i++ {
				checkinRuns.Lock()
				r := checkinRuns.records[f.AuthIndex]
				r.next = time.Now().Add(-time.Second)
				checkinRuns.records[f.AuthIndex] = r
				checkinRuns.Unlock()
				runDueCheckinRetries()
			}
			again := checkinOneAccount(f)
			checkinRuns.Lock()
			r := checkinRuns.records[f.AuthIndex]
			checkinRuns.Unlock()
			if posts.Load() != 1 || r.retries != 3 || !r.next.IsZero() || !r.confirmOnly || again["success"] == true {
				t.Fatalf("blind retry/budget leak: posts=%d record=%+v out=%#v", posts.Load(), r, again)
			}
		})
	}
}

func TestCheckinPost500DisappearanceIsNotConfirmation(t *testing.T) {
	var posts atomic.Int32
	f, _ := checkinHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts.Add(1)
			w.WriteHeader(500)
			return
		}
		if posts.Load() > 0 {
			_, _ = w.Write([]byte(`{"campaigns":[]}`))
		} else {
			_, _ = w.Write([]byte(campaignListJSON))
		}
	}, false)
	for i := 0; i < 2; i++ {
		if out := checkinOneAccount(f); out["success"] == true || out["state"] != "pending_confirmation" {
			t.Fatalf("disappearance falsely confirmed: %#v", out)
		}
	}
	if posts.Load() != 1 {
		t.Fatalf("post replayed %d times", posts.Load())
	}
}

func TestCheckinPermanentFailuresNeverSchedule(t *testing.T) {
	for _, status := range []int{400, 401, 403, 422} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f, _ := checkinHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`{"message":"今日领取失败"}`))
					return
				}
				_, _ = w.Write([]byte(campaignListJSON))
			}, false)
			out := checkinOneAccount(f)
			if out["success"] == true || out["retry_scheduled"] != false {
				t.Fatalf("permanent failure retried/succeeded: %#v", out)
			}
		})
	}
}

func TestCheckinPhysicalDisabledAndExplicitManual(t *testing.T) {
	var posts atomic.Int32
	f, stub := checkinHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts.Add(1)
			_, _ = w.Write([]byte(`{"status":"CLAIMED"}`))
			return
		}
		if strings.Contains(r.URL.Path, "campaigns") {
			_, _ = w.Write([]byte(campaignListJSON))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}, true)
	runCheckinAccount(f, "auto", nil)
	if posts.Load() != 0 {
		t.Fatal("auto ignored fresh physical disabled")
	}
	out := checkinOneAccount(f)
	if out["success"] != true || posts.Load() != 1 || len(stub.saved) != 0 || !parseDisabledFromAuthJSON(stub.files[f.AuthIndex]) {
		t.Fatalf("manual toggled account or failed: %#v", out)
	}
}

func TestCheckinManualAutoAndRetryShareLock(t *testing.T) {
	var posts atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	f, _ := checkinHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			if posts.Add(1) == 1 {
				close(started)
				<-release
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
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); checkinOneAccount(f) }()
	<-started
	wg.Add(2)
	go func() { defer wg.Done(); runCheckinAccount(f, "auto", nil) }()
	go func() { defer wg.Done(); runCheckinAccount(f, "retry", nil) }()
	close(release)
	wg.Wait()
	if posts.Load() != 1 {
		t.Fatalf("concurrent claims=%d", posts.Load())
	}
}

func TestCheckinHostListFailureSpendsFiniteRetryBudget(t *testing.T) {
	resetCheckinTestState(t)
	idx := "host-list-failure"
	checkinRuns.records[idx] = checkinRunRecord{day: time.Now().Format("2006-01-02"), at: time.Now(), next: time.Now().Add(-time.Second), confirmOnly: true, campaignID: "abc"}
	old := hostCall
	calls := 0
	hostCall = func(method string, body []byte) ([]byte, error) {
		if method == pluginabi.MethodHostAuthList {
			calls++
		}
		return nil, errors.New("host unavailable")
	}
	t.Cleanup(func() { hostCall = old })
	for i := 0; i < 3; i++ {
		checkinRuns.Lock()
		r := checkinRuns.records[idx]
		r.next = time.Now().Add(-time.Second)
		checkinRuns.records[idx] = r
		checkinRuns.Unlock()
		runDueCheckinRetries()
	}
	for i := 0; i < 5; i++ {
		runDueCheckinRetries()
	}
	r := checkinRuns.records[idx]
	if calls != 3 || r.retries != 3 || !r.next.IsZero() || !r.confirmOnly {
		t.Fatalf("unbounded list failure: calls=%d record=%+v", calls, r)
	}
}

func TestCheckinCapacityPreservesPostAndRetryBudget(t *testing.T) {
	resetCheckinTestState(t)
	now := time.Now()
	for i := 0; i < checkinRunLimit; i++ {
		checkinRuns.records[itoa(int64(i))] = checkinRunRecord{day: now.Format("2006-01-02"), at: now, confirmOnly: i%2 == 0, retries: i % 2}
	}
	if _, ok := reserveCheckin("new", "new", "manual", now); ok {
		t.Fatal("evicted safety checkpoint to admit account")
	}
	cancelScheduledCheckinRetries()
	if _, ok := reserveCheckin("new", "new", "manual", now); ok {
		t.Fatal("cancel erased checkpoint protection")
	}
	if _, ok := reserveCheckin("new", "new", "manual", now.Add(24*time.Hour)); !ok {
		t.Fatal("old-day state did not expire")
	}
}

func TestCheckinCancelDuringFlightDoesNotReschedule(t *testing.T) {
	resetCheckinTestState(t)
	before, ok := reserveCheckin("cancel", "cancel", "manual", time.Now())
	if !ok {
		t.Fatal("reserve")
	}
	cancelCheckinRetry("cancel")
	out := finishCheckin("cancel", before, checkinAttemptResult{state: "pending_confirmation", confirmOnly: true, retryable: true})
	if out["retry_scheduled"] != false || !checkinRuns.records["cancel"].confirmOnly {
		t.Fatalf("cancellation lost: %#v", out)
	}
	checkinAutoMu.Lock()
	checkinAuto = false
	checkinAutoMu.Unlock()
	before, _ = reserveCheckin("off", "off", "manual", time.Now())
	out = finishCheckin("off", before, checkinAttemptResult{state: "failed", retryable: true})
	if out["retry_scheduled"] != false {
		t.Fatalf("off scheduled manual retry: %#v", out)
	}
}
