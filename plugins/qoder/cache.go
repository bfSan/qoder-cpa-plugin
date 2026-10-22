// cache.go holds the per-account in-memory cache for plan / checkin / credits
// snapshots and the singleflight machinery that dedups concurrent upstream
// fetches for the same account. The cache is the coordination point between
// the dashboard and reconcile paths.
package main

import (
	"sort"
	"sync"
	"time"
)

// the failed field falls back to the previous value instead of being wiped.
type accountCacheEntry struct {
	checkin *checkinSummary
	credits *creditsSummary
	plan    string
	fetched time.Time
	// v0.8.8: 最近一次签到成功时刻（本地时钟）。lifecycle 在
	// checkinGraceWindow 内跳过 disable —— 签到后积分池刷新有延迟，
	// 立即 reconcile 可能读到 remain=0 的旧快照而误禁用刚签到的账号。
	checkinAt      time.Time
	checkinFetched time.Time // beginning of the authoritative read, not completion
	creditsFetched time.Time
	planFetched    time.Time
	confirmedAt    time.Time // receipt/status confirmation; distinct from grace
	invalidate     bool      // snapshot-local explicit authentication rejection
}

var (
	accountCache    sync.Map // auth_id (auth.ID) -> *accountCacheEntry
	accountCacheTTL = 45 * time.Second
)

// accountDetailFlight is a per-authID singleflight: concurrent dashboard /
// reconcile callers for the same account share one upstream fetch instead of
// stampeding the billing API. Without this, two parallel panel refreshes would
// each spawn 3 goroutines per account → 6× upstream QPS and last-writer-wins
// cache corruption.
var accountDetailFlight sync.Map // authID -> *accountDetailCall

type accountDetailCall struct {
	done chan struct{}
	plan string
	ci   *checkinSummary
	cr   *creditsSummary
	errs []string
}

// cachedAccountDetails fetches plan/checkin/credits concurrently (upstream
// round-trip dominates; 3 serial calls ≈ 3× latency). On any individual
// failure the previous cached value is kept (stale-while-error) so a
// transient upstream 500 does not blank the panel row.
func cachedAccountDetails(authID string, sa *storedAuth, force bool) (plan string, ci *checkinSummary, cr *creditsSummary, errs []string) {
	var prev *accountCacheEntry
	if v, ok := accountCache.Load(authID); ok {
		prev = v.(*accountCacheEntry)
		if !force && time.Since(prev.fetched) < accountCacheTTL {
			// Return cached values. Do NOT mutate prev.credits here — concurrent
			// goroutines (reconcileOneAccount) may read the same entry.
			// FetchedAt is stamped at Store time; if it's empty (legacy entry),
			// the panel can derive it from prev.fetched if needed.
			return prev.plan, prev.checkin, prev.credits, nil
		}
	}

	// Singleflight: only one goroutine performs the upstream fetch per authID.
	// Others wait on the in-flight call's done channel and reuse its result.
	// Note: force=true callers DO join the flight (P1-1 trade-off). This is
	// intentional: the flight window is short (~3 concurrent fetches), and
	// skipping it would re-introduce the P0-2 race where concurrent writers
	// overwrite each other's cache entries. The result a force caller gets
	// is at most a few hundred ms old — fresh enough for lifecycle decisions.
	call := &accountDetailCall{done: make(chan struct{})}
	actual, loaded := accountDetailFlight.LoadOrStore(authID, call)
	if loaded {
		// Someone else is fetching — wait for their result.
		other := actual.(*accountDetailCall)
		<-other.done
		// Re-read cache: fetcher already Stored; use whatever won the race.
		if v, ok := accountCache.Load(authID); ok {
			if e, ok2 := v.(*accountCacheEntry); ok2 {
				return e.plan, e.checkin, e.credits, other.errs
			}
		}
		return other.plan, other.ci, other.cr, other.errs
	}
	// We are the fetcher. Make sure waiters wake up and the flight entry is
	// released even on panic.
	defer func() {
		call.plan, call.ci, call.cr, call.errs = plan, ci, cr, errs
		close(call.done)
		accountDetailFlight.Delete(authID)
	}()

	started := time.Now()
	var (
		wg      sync.WaitGroup
		errMu   sync.Mutex
		errList []string
	)
	addErr := func(msg string) {
		errMu.Lock()
		errList = append(errList, msg)
		errMu.Unlock()
	}
	// authRejected records that upstream refused the credential itself. A stale
	// snapshot must not survive that: carrying over the previous "已签到" state
	// makes the panel report a check-in that never happened (2026-09-21 field
	// report — credits never increased while the UI said success).
	var authRejected bool
	addAuthReject := func(msg string) {
		errMu.Lock()
		errList = append(errList, msg)
		authRejected = true
		errMu.Unlock()
	}
	wg.Add(2)
	go func() { defer wg.Done(); plan = fetchPaymentTypeFn(sa) }()
	if supportsCheckin(sa) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c, err := fetchCheckinStatus(sa); err == nil {
				ci = c
			} else {
				if isAuthRejectedError(err) {
					addAuthReject("checkin: " + err.Error())
				} else {
					addErr("checkin: " + err.Error())
				}
			}
		}()
	}
	go func() {
		defer wg.Done()
		// Route through the indirection var so tests can exercise this path; the
		// documented seam (fetchUserResourceFn) only helps if it is actually used.
		if r, err := fetchUserResourceFn(sa); err == nil {
			cr = r
		} else {
			if isAuthRejectedError(err) {
				addAuthReject("credits: " + err.Error())
			} else {
				addErr("credits: " + err.Error())
			}
		}
	}()
	wg.Wait()
	// Merge against the latest entry at commit, not the pre-flight snapshot.
	// Failed fields retain their own timestamps; a fallback balance is NOT fresh.
	now := time.Now()
	snapshot := &accountCacheEntry{checkin: ci, credits: cr, plan: plan}
	if ci != nil {
		snapshot.checkinFetched = started
	}
	if cr != nil {
		copy := *cr
		copy.FetchedAt = now.UTC().Format(time.RFC3339)
		snapshot.credits, snapshot.creditsFetched = &copy, started
	}
	if plan != "" {
		snapshot.planFetched = started
	}
	if len(errList) == 0 && cr != nil && plan != "" && (!supportsCheckin(sa) || ci != nil) {
		snapshot.fetched = started
	}
	if authRejected {
		snapshot.invalidate = true
		snapshot.checkinFetched, snapshot.creditsFetched, snapshot.planFetched = started, started, started
		snapshot.checkin, snapshot.credits, snapshot.plan = nil, nil, ""
	}
	if !authRejected && cr != nil {
		observePositiveCreditsAt(authID, cr, started)
	}
	merged := storeAccountSnapshot(authID, snapshot)
	pruneAccountCacheSoftCap(accountCacheSoftCap)
	return merged.plan, merged.checkin, merged.credits, errList
}

// CAS merge prevents an older dashboard flight from wiping a confirmation,
// grace timestamp, or a post-claim credits read. All cached objects immutable.
func storeAccountSnapshot(authID string, snapshot *accountCacheEntry) *accountCacheEntry {
	for {
		old, exists := accountCache.Load(authID)
		next := accountCacheEntry{}
		if exists {
			if p, ok := old.(*accountCacheEntry); ok && p != nil {
				next = *p
			}
		}
		if snapshot.checkinAt.After(next.checkinAt) {
			next.checkinAt = snapshot.checkinAt
		}
		if snapshot.confirmedAt.After(next.confirmedAt) {
			next.confirmedAt = snapshot.confirmedAt
		}
		if !snapshot.checkinFetched.IsZero() && !snapshot.checkinFetched.Before(next.checkinFetched) {
			// A receipt remains valid when the campaign disappears from the listing.
			// Auth rejection may invalidate older evidence, but never a newer receipt.
			keepConfirmed := !snapshot.invalidate && next.checkin != nil && next.checkin.TodayCheckedIn && !next.confirmedAt.IsZero() && next.confirmedAt.Format("2006-01-02") == time.Now().Format("2006-01-02") && (snapshot.checkin == nil || !snapshot.checkin.TodayCheckedIn)
			if !keepConfirmed {
				next.checkin, next.checkinFetched = snapshot.checkin, snapshot.checkinFetched
				if snapshot.invalidate {
					next.confirmedAt = time.Time{}
				}
			}
		}
		if !snapshot.creditsFetched.IsZero() && !snapshot.creditsFetched.Before(next.creditsFetched) && !snapshot.creditsFetched.Before(next.checkinAt) {
			next.credits, next.creditsFetched = snapshot.credits, snapshot.creditsFetched
		}
		if !snapshot.planFetched.IsZero() && !snapshot.planFetched.Before(next.planFetched) {
			next.plan, next.planFetched = snapshot.plan, snapshot.planFetched
		}
		if snapshot.invalidate {
			next.fetched = time.Time{}
		} else if snapshot.fetched.After(next.fetched) && !snapshot.fetched.Before(next.checkinAt) {
			next.fetched = snapshot.fetched
		}
		next.invalidate = false
		if exists {
			if accountCache.CompareAndSwap(authID, old, &next) {
				return &next
			}
		} else if _, loaded := accountCache.LoadOrStore(authID, &next); !loaded {
			return &next
		}
	}
}

func storeCheckinConfirmation(authID string, ci *checkinSummary, now time.Time) {
	if authID == "" {
		return
	}
	snapshot := &accountCacheEntry{checkinAt: now, confirmedAt: now}
	if ci != nil {
		copy := *ci
		snapshot.checkin, snapshot.checkinFetched = &copy, now
	}
	storeAccountSnapshot(authID, snapshot)
}

// accountCacheSoftCap limits concurrent cache entries (auth churn / index thrash).
const accountCacheSoftCap = 256

// cachedAccountDetailsSnapshot returns the last known account details without
// hitting upstream. The panel uses it on first paint so a recent snapshot is
// visible immediately instead of every card starting as "loading".
func cachedAccountDetailsSnapshot(authID string) (plan string, ci *checkinSummary, cr *creditsSummary, ok bool) {
	v, found := accountCache.Load(authID)
	if !found {
		return "", nil, nil, false
	}
	entry, typeOK := v.(*accountCacheEntry)
	if !typeOK || entry == nil {
		return "", nil, nil, false
	}
	return entry.plan, entry.checkin, entry.credits, true
}

// pruneAccountCacheSoftCap drops excess entries with the oldest fetched time.
// Called after Store; O(n) over map size — fine for dozens of accounts.
func pruneAccountCacheSoftCap(capN int) {
	if capN <= 0 {
		return
	}
	type item struct {
		key   string
		at    time.Time
		entry *accountCacheEntry
	}
	var items []item
	accountCache.Range(func(key, value any) bool {
		k, _ := key.(string)
		e, ok := value.(*accountCacheEntry)
		if !ok || k == "" {
			accountCache.Delete(key)
			return true
		}
		items = append(items, item{key: k, at: e.fetched, entry: e})
		return true
	})
	if len(items) <= capN {
		return
	}
	// Sort oldest first (was O(n²) bubble — sort.Slice is O(n log n)).
	sort.Slice(items, func(i, j int) bool { return items[i].at.Before(items[j].at) })
	drop := len(items) - capN
	for i := 0; i < len(items) && drop > 0; i++ {
		if !items[i].entry.checkinAt.IsZero() && time.Since(items[i].entry.checkinAt) < checkinGraceWindow {
			continue
		}
		if accountCache.CompareAndDelete(items[i].key, items[i].entry) {
			drop--
		}
	}
}
