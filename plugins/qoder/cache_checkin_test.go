package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCacheOlderRefreshCannotOverwriteCheckinOrCredits(t *testing.T) {
	const id = "cas-checkin-test"
	t.Cleanup(func() { accountCache.Delete(id) })
	old := time.Now().Add(-time.Minute)
	newer := time.Now()
	storeAccountSnapshot(id, &accountCacheEntry{checkin: &checkinSummary{}, checkinFetched: old, credits: &creditsSummary{TotalRemain: 0, FetchedAt: "old"}, creditsFetched: old, fetched: old})
	storeCheckinConfirmation(id, &checkinSummary{TodayCheckedIn: true, Active: true}, newer)
	storeAccountSnapshot(id, &accountCacheEntry{credits: &creditsSummary{TotalRemain: 100, FetchedAt: "new"}, creditsFetched: newer.Add(time.Millisecond)})
	merged := storeAccountSnapshot(id, &accountCacheEntry{checkin: &checkinSummary{}, checkinFetched: old.Add(time.Second), credits: &creditsSummary{TotalRemain: 0, FetchedAt: "stale"}, creditsFetched: old.Add(time.Second), fetched: old.Add(time.Second)})
	if !merged.checkin.TodayCheckedIn || merged.credits.TotalRemain != 100 || !merged.checkinAt.Equal(newer) || merged.credits.FetchedAt != "new" {
		t.Fatalf("stale write won: %+v", merged)
	}
}

func TestCacheFailedCreditsKeepOriginalTimestampAndPointer(t *testing.T) {
	const id = "cache-credit-fallback"
	t.Cleanup(func() { accountCache.Delete(id) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"campaigns":[]}`)) }))
	defer srv.Close()
	defer setUpstreamBaseForTest(regionIntl, srv.URL)()
	oldPlan, oldCredits := fetchPaymentTypeFn, fetchUserResourceFn
	fetchPaymentTypeFn = func(*storedAuth) string { return "Pro" }
	fetchUserResourceFn = func(*storedAuth) (*creditsSummary, error) { return nil, errors.New("temporary failure") }
	t.Cleanup(func() { fetchPaymentTypeFn, fetchUserResourceFn = oldPlan, oldCredits })
	at := time.Now().Add(-time.Hour)
	cr := &creditsSummary{TotalRemain: 123, FetchedAt: at.UTC().Format(time.RFC3339)}
	accountCache.Store(id, &accountCacheEntry{credits: cr, creditsFetched: at, fetched: at})
	sa := &storedAuth{Auth: storedTokens{Region: regionIntl, AccessToken: "token"}}
	_, _, got, errs := cachedAccountDetails(id, sa, true)
	if len(errs) == 0 || got != cr || got.FetchedAt != at.UTC().Format(time.RFC3339) {
		t.Fatalf("fallback restamped: got=%+v errs=%v", got, errs)
	}
	value, _ := accountCache.Load(id)
	if !value.(*accountCacheEntry).creditsFetched.Equal(at) || !value.(*accountCacheEntry).fetched.Equal(at) {
		t.Fatal("failed fetch changed freshness")
	}
}

func TestCacheRealRefreshRacePreservesConfirmationGrace(t *testing.T) {
	const id = "cache-refresh-race"
	t.Cleanup(func() { accountCache.Delete(id) })
	started, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_, _ = w.Write([]byte(`{"campaigns":[]}`))
	}))
	defer srv.Close()
	defer setUpstreamBaseForTest(regionIntl, srv.URL)()
	oldPlan, oldCredits := fetchPaymentTypeFn, fetchUserResourceFn
	fetchPaymentTypeFn = func(*storedAuth) string { return "Pro" }
	fetchUserResourceFn = func(*storedAuth) (*creditsSummary, error) { return &creditsSummary{TotalRemain: 0}, nil }
	defer func() { fetchPaymentTypeFn, fetchUserResourceFn = oldPlan, oldCredits }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		cachedAccountDetails(id, &storedAuth{Auth: storedTokens{Region: regionIntl, AccessToken: "token"}}, true)
	}()
	<-started
	storeCheckinConfirmation(id, &checkinSummary{Active: true, TodayCheckedIn: true}, time.Now())
	close(release)
	<-done
	_, ci, cr, _ := cachedAccountDetailsSnapshot(id)
	if ci == nil || !ci.TodayCheckedIn || !checkinGraceActive(id) || cr != nil {
		t.Fatalf("old-flight result overwrote confirmation: ci=%+v cr=%+v", ci, cr)
	}
}

func TestCachePrunePreservesActiveCheckinGrace(t *testing.T) {
	const id = "prune-protected-grace"
	t.Cleanup(func() { accountCache.Delete(id); accountCache.Delete("prune-unprotected") })
	rememberCheckinMoment(id)
	accountCache.Store("prune-unprotected", &accountCacheEntry{fetched: time.Now()})
	pruneAccountCacheSoftCap(1)
	if !checkinGraceActive(id) {
		t.Fatal("capacity eviction removed active grace")
	}
}
