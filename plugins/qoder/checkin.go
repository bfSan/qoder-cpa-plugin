// checkin.go implements the daily benefit check-in for CN and Intl accounts:
// the manual handleManualCheckin endpoint, the 10:00 / 21:00 auto timer, and
// the per-account mutex that prevents duplicate check-ins from racing browser
// tabs. Both regions use the campaign contract (see campaign.go); CN keeps the
// legacy daily-check-in endpoint as a fallback.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var (
	checkinLoopStop chan struct{}
	checkinLoopMu   sync.Mutex
)

func ensureCheckinLoop() {
	checkinLoopMu.Lock()
	defer checkinLoopMu.Unlock()
	if checkinLoopStop != nil {
		return // already running
	}
	checkinLoopStop = make(chan struct{})
	go checkinLoop(checkinLoopStop)
}

// Note: there is deliberately no stopCheckinScheduler. The plugin shutdown
// export is a no-op (see cliproxyPluginShutdown) because the host invokes it
// during its own runtime teardown, where touching Go sync primitives from the
// plugin's c-shared runtime caused SIGSEGV on every restart.

func nextCheckinTime(now time.Time) time.Time {
	var earliest time.Time
	// Consider both checkin and keepalive schedules so the timer wakes up for
	// whichever fires first (e.g. 21:00 checkin vs 22:00 keepalive → 21:00 wins,
	// then 22:00 keepalive fires on the next tick).
	hours := append([]int{}, checkinHours...)
	hours = append(hours, keepaliveHours...)
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour) // slot already passed today → tomorrow
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

func checkinLoop(stop chan struct{}) {
	retryTicker := time.NewTicker(30 * time.Second)
	defer retryTicker.Stop()
	timer := time.NewTimer(time.Until(nextCheckinTime(time.Now())))
	defer timer.Stop()
	for {
		select {
		case <-stop:
			cancelScheduledCheckinRetries()
			return
		case <-retryTicker.C:
			// Keep the scheduled timer armed. Recreating it on every retry tick could
			// lose a 10:00/21:00 event when both channels become ready together.
			runDueCheckinRetries()
		case <-timer.C:
			runAutoCheckin()
			if shouldRunKeepaliveNow(time.Now()) {
				runTokenKeepalive()
			}
			timer.Reset(time.Until(nextCheckinTime(time.Now())))
		}
	}
}

// runAutoCheckin is the scheduled lifecycle tick (10:00 / 21:00).
// It runs the optional daily check-in, then reconciles credit state
// (disable exhausted / reenable after credits). The one-shot Pro upgrade
// claim remains manual.
//
// v0.6.31: per-account work runs concurrently (sem=4) — was serial, so N accounts
// meant 3N serial HTTP round-trips on the billing API. Matches the pattern used
// by buildDashboardEx and handleManualCheckin.
func runAutoCheckin() {
	checkinAutoMu.RLock()
	doCheckin := checkinAuto
	checkinAutoMu.RUnlock()
	// Lifecycle may still run when check-in is off (credit gate).
	if !doCheckin && !lifecycleEnabled() {
		return
	}
	files, err := hostAuthList()
	if err != nil {
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, f := range files {
		f := f
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			processAutoCheckinAccount(f, doCheckin)
		}()
	}
	wg.Wait()
}

// processAutoCheckinAccount handles one account's scheduled tick. Extracted so
// runAutoCheckin can fan out per-account work without duplicating logic.
func processAutoCheckinAccount(f pluginapi.HostAuthFileEntry, doCheckin bool) {
	if doCheckin {
		runCheckinAccount(f, "auto", nil)
	}
	// Never hold the check-in lock while reconciling: lifecycle writes use it too.
	if lifecycleEnabled() {
		_, _ = reconcileOneAccount(f.AuthIndex, f.ID, true)
	}
}

// handleManualCheckin checks in one account (auth_index) or all qoderwork
// accounts. Unlike the workbuddy three-phase classify/execute/summarize flow,
// all entry points share the confirmation state machine below. Each check-in
// HTTP request has an 8s context; the Host RPC wire may impose its own deadline.
func handleManualCheckin(req pluginapi.ManagementRequest) map[string]any {
	t0 := time.Now()
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(req.Body, &body)
	authIndex := strings.TrimSpace(body.AuthIndex)
	single := authIndex != ""

	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	var targets []pluginapi.HostAuthFileEntry
	for _, f := range files {
		if !single || f.AuthIndex == authIndex {
			targets = append(targets, f)
		}
	}
	if len(targets) == 0 {
		return map[string]any{"error": "no matching account"}
	}

	// Run checkins concurrently (one goroutine per account, bounded).
	type result struct {
		idx int
		out map[string]any
	}
	outCh := make(chan result, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, f := range targets {
		wg.Add(1)
		go func(i int, f pluginapi.HostAuthFileEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			outCh <- result{idx: i, out: checkinOneAccount(f)}
		}(i, f)
	}
	wg.Wait()
	close(outCh)

	results := make([]map[string]any, len(targets))
	for r := range outCh {
		results[r.idx] = r.out
	}
	successN, alreadyN, unsupportedN, failN, pendingN, skippedN := 0, 0, 0, 0, 0, 0
	for _, r := range results {
		if r["error"] != nil {
			failN++
			continue
		}
		if r["reason"] == "unsupported" {
			unsupportedN++
			continue
		}
		if r["state"] == "pending_confirmation" {
			pendingN++
			continue
		}
		if r["reason"] == "already" && r["success"] == true {
			alreadyN++
			continue
		}
		if r["skipped"] == true {
			skippedN++
			continue
		}
		if r["success"] == true {
			successN++
		} else {
			failN++
		}
	}
	return map[string]any{
		"results": results,
		"summary": map[string]any{
			"total":       len(targets),
			"success":     successN,
			"already":     alreadyN,
			"unsupported": unsupportedN,
			"fail":        failN,
			"pending":     pendingN,
			"skipped":     skippedN,
			"elapsed_ms":  time.Since(t0).Milliseconds(),
		},
	}
}

// Manual, automatic, and scheduled retries share the same account mutex and
// state. Manual means permission to check in, NOT permission to enable auth.
func checkinOneAccount(f pluginapi.HostAuthFileEntry) map[string]any {
	return runCheckinAccount(f, "manual", nil)
}

const checkinRunLimit = 512

var checkinRetryDelays = [...]time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute}

type checkinRunRecord struct {
	authID, day, state, campaignID           string
	at, next                                 time.Time
	retries                                  int
	confirmOnly, confirmed, legacy, inFlight bool
	generation                               uint64
}

var checkinRuns = struct {
	sync.Mutex
	records map[string]checkinRunRecord
}{records: make(map[string]checkinRunRecord)}
var checkinRetryDispatch sync.Mutex

func checkinAutoEnabled() bool {
	checkinAutoMu.RLock()
	defer checkinAutoMu.RUnlock()
	return checkinAuto
}

// Cancel clears schedules, not the uncertain POST checkpoint or spent budget.
// Safe to call while lifecycle holds checkinLockFor(index).
func cancelCheckinRetry(index string) {
	checkinRuns.Lock()
	defer checkinRuns.Unlock()
	if r, ok := checkinRuns.records[index]; ok {
		r.next = time.Time{}
		r.generation++
		checkinRuns.records[index] = r
	}
}

func cancelScheduledCheckinRetries() {
	checkinRuns.Lock()
	defer checkinRuns.Unlock()
	for index, r := range checkinRuns.records {
		r.next = time.Time{}
		r.generation++
		checkinRuns.records[index] = r
	}
}

// reserveCheckin is called under the account lock, before any upstream I/O.
// Refuse admission instead of evicting today's uncertain POST/retry budget.
func reserveCheckin(index, id, source string, now time.Time) (checkinRunRecord, bool) {
	checkinRuns.Lock()
	defer checkinRuns.Unlock()
	day := now.Format("2006-01-02")
	if strings.TrimSpace(index) == "" {
		return checkinRunRecord{}, false
	}
	r, exists := checkinRuns.records[index]
	if !exists {
		if len(checkinRuns.records) >= checkinRunLimit {
			oldest := ""
			var at time.Time
			for k, v := range checkinRuns.records {
				if v.inFlight || v.confirmOnly || v.day == day && (v.retries > 0 || !v.next.IsZero()) {
					continue
				}
				if oldest == "" || v.at.Before(at) {
					oldest, at = k, v.at
				}
			}
			if oldest == "" {
				return r, false
			}
			delete(checkinRuns.records, oldest)
		}
	}
	if r.day != day {
		// Retry schedules never cross days. An uncertain campaign POST, however,
		// may belong to a multi-day activity: retain its read-only checkpoint.
		r = checkinRunRecord{day: day, generation: r.generation, confirmOnly: r.confirmOnly, campaignID: r.campaignID, legacy: r.legacy}
	}
	if source == "retry" {
		if r.next.IsZero() || r.next.After(now) || r.retries >= len(checkinRetryDelays) {
			return r, false
		}
		// Charge before list/get failure too, so a broken Host cannot retry forever.
		r.retries++
		r.next = time.Time{}
	}
	if source == "auto" && r.state != "" && r.state != "none" && r.state != "disabled" {
		return r, false
	}
	r.authID, r.at, r.inFlight = id, now, true
	checkinRuns.records[index] = r
	return r, true
}

type checkinAttemptResult struct {
	state, message, campaignID                string
	confirmed, confirmOnly, retryable, legacy bool
	status                                    *checkinSummary
	receipt                                   map[string]any
}

func checkinFailed(err error, confirmOnly bool) checkinAttemptResult {
	r := checkinAttemptResult{state: "failed", message: "无法读取签到凭证或状态", confirmOnly: confirmOnly}
	var ce *checkinRequestError
	if errors.As(err, &ce) {
		r.message, r.retryable = ce.Error(), ce.Retryable
		r.confirmOnly = r.confirmOnly || ce.Uncertain
	} else {
		r.retryable = checkinTransportRetryable(err)
	}
	return r
}

func confirmedCheckinAttempt(sa *storedAuth, previous checkinRunRecord) checkinAttemptResult {
	// A previous uncertain POST stays bound to its original contract/campaign.
	// A missing campaign is not confirmation, and cannot permit another POST.
	if previous.confirmOnly {
		var ci *checkinSummary
		var err error
		if previous.legacy {
			ci, err = fetchDailyCheckinStatus(sa)
		} else {
			var cs *campaignStatusResponse
			cs, err = fetchCampaignStatus(sa)
			if err == nil {
				ci = campaignCheckinSummary(cs)
				ci.TodayCheckedIn = false
				for _, c := range cs.Campaigns {
					if c.CampaignID == previous.campaignID && strings.EqualFold(c.ClaimStatus, "CLAIMED") {
						ci.TodayCheckedIn, ci.ClaimedAt = true, c.ClaimedAt
						break
					}
				}
			}
		}
		if err != nil {
			return checkinFailed(err, true)
		}
		if ci != nil && ci.TodayCheckedIn {
			return checkinAttemptResult{state: "already", message: "已领取（上游状态已确认）", confirmed: true, status: ci, campaignID: previous.campaignID, legacy: previous.legacy}
		}
		return checkinAttemptResult{state: "pending_confirmation", message: "领取结果尚未确认；仅有限次复查，不重复提交", retryable: true, confirmOnly: true, status: ci, campaignID: previous.campaignID, legacy: previous.legacy}
	}
	obs, err := fetchCheckinObservation(sa)
	if err != nil {
		return checkinFailed(err, false)
	}
	ci := obs.summary
	if ci == nil {
		return checkinFailed(&checkinRequestError{Kind: "protocol"}, false)
	}
	if ci.TodayCheckedIn {
		return checkinAttemptResult{state: "already", message: "今日已签到（上游状态已确认）", confirmed: true, status: ci, legacy: obs.legacy}
	}
	if previous.confirmed && (!obs.legacy && (obs.campaign == nil || obs.campaign.CampaignID == previous.campaignID) || obs.legacy && previous.legacy) {
		// The listing may remove a claimed campaign, or lag behind its receipt.
		return checkinAttemptResult{state: "already", message: "已领取（本进程已保存确认结果）", confirmed: true, campaignID: previous.campaignID, legacy: previous.legacy}
	}
	if !ci.Active {
		return checkinAttemptResult{state: "none", message: "当前暂无可领取权益（不代表已签到）", status: ci}
	}
	var receipt map[string]any
	var claimErr error
	id := ""
	if obs.legacy {
		receipt, claimErr = performDailyCheckinCall(sa)
	} else {
		if obs.campaign == nil {
			return checkinFailed(&checkinRequestError{Kind: "protocol"}, false)
		}
		id = obs.campaign.CampaignID
		receipt, claimErr = performCampaignClaim(sa, obs.campaign)
	}
	if receipt["confirmed_receipt"] == true && claimErr == nil {
		confirmed := *ci
		confirmed.TodayCheckedIn, confirmed.TodayCredit = true, ci.DailyCredit
		confirmed.ClaimedAt, _ = receipt["claimed_at"].(string)
		result := checkinAttemptResult{state: "success", message: "签到成功（领取回执已确认）", confirmed: true, status: &confirmed, receipt: receipt, campaignID: id}
		if receipt["replayed"] == true {
			result.state, result.message = "already", "已领取（权益已存在，无法判断由谁领取）"
		}
		return result
	}
	// A definite business/auth rejection must not become a retry. Transient 429
	// is safe to retry after a new read; transport/5xx/malformed POST is not.
	if claimErr != nil {
		var ce *checkinRequestError
		if errors.As(claimErr, &ce) && !ce.Uncertain {
			result := checkinFailed(claimErr, false)
			result.status, result.campaignID, result.legacy = ci, id, obs.legacy
			return result
		}
	}
	// Legacy acknowledgement/duplicate markers require verification. Even a lost
	// response may have mutated upstream, so from here on retries are read-only.
	pending := checkinRunRecord{confirmOnly: true, campaignID: id, legacy: obs.legacy}
	result := confirmedCheckinAttempt(sa, pending)
	result.campaignID, result.legacy, result.receipt = id, obs.legacy, receipt
	if result.confirmed && receipt["result"] != "ALREADY_CLAIMED" {
		result.state, result.message = "success", "签到成功（上游状态已确认）"
	}
	if !result.confirmed && claimErr == nil && receipt["success"] == false && receipt["result"] != "ALREADY_CLAIMED" {
		result.state, result.message, result.retryable, result.confirmOnly = "failed", "上游明确拒绝领取，未确认成功", false, false
	}
	if !result.confirmed && claimErr != nil {
		var ce *checkinRequestError
		if errors.As(claimErr, &ce) && !ce.Retryable {
			result.retryable = false
		}
	}
	return result
}

func finishCheckin(index string, before checkinRunRecord, result checkinAttemptResult) map[string]any {
	now := time.Now()
	enabled := checkinAutoEnabled()
	checkinRuns.Lock()
	current := checkinRuns.records[index]
	r := before
	r.at, r.state, r.inFlight = now, result.state, false
	r.confirmed = result.confirmed || before.confirmed
	if result.campaignID != "" && result.campaignID != before.campaignID {
		r.confirmed = result.confirmed
	}
	r.confirmOnly = result.confirmOnly || before.confirmOnly && !result.confirmed
	if result.confirmed {
		r.confirmOnly = false
	}
	if result.campaignID != "" {
		r.campaignID = result.campaignID
	}
	if result.campaignID != "" || result.status != nil || result.confirmed {
		r.legacy = result.legacy
	}
	r.next = time.Time{}
	if result.retryable && enabled && r.retries < len(checkinRetryDelays) && current.generation == before.generation {
		next := now.Add(checkinRetryDelays[r.retries])
		if next.Format("2006-01-02") == r.day {
			r.next = next
		}
	}
	r.generation = current.generation
	checkinRuns.records[index] = r
	checkinRuns.Unlock()
	out := map[string]any{"auth_index": index, "success": result.confirmed, "confirmed": result.confirmed, "state": result.state, "message": result.message, "retry_count": r.retries, "retry_scheduled": !r.next.IsZero()}
	if !r.next.IsZero() {
		out["next_retry_at"] = r.next.UTC().Format(time.RFC3339)
	}
	if result.state != "success" {
		out["reason"] = result.state
	}
	if result.state == "already" || result.state == "none" || result.state == "disabled" || result.state == "unsupported" {
		out["skipped"] = true
	}
	if result.state == "failed" {
		out["error"] = result.message
	}
	if result.confirmed {
		for _, key := range []string{"claimed_at", "grant_id", "replayed", "campaign_id"} {
			if v, ok := result.receipt[key]; ok {
				out[key] = v
			}
		}
		if n, ok := result.receipt["rewardCredits"].(float64); ok {
			out["reward_credits"] = int64(n)
		}
		if result.status != nil {
			out["streak_days"], out["total_credits"] = result.status.StreakDays, result.status.TotalCredits
			if result.status.ClaimedAt != "" {
				out["claimed_at"] = result.status.ClaimedAt
			}
		}
	}
	return out
}

func runCheckinAccount(f pluginapi.HostAuthFileEntry, source string, listErr error) map[string]any {
	mu := checkinLockFor(f.AuthIndex)
	mu.Lock()
	defer mu.Unlock()
	if source != "manual" && !checkinAutoEnabled() {
		cancelCheckinRetry(f.AuthIndex)
		return map[string]any{"auth_index": f.AuthIndex, "success": false, "skipped": true, "reason": "cancelled"}
	}
	before, ok := reserveCheckin(f.AuthIndex, f.ID, source, time.Now())
	if !ok {
		return map[string]any{"auth_index": f.AuthIndex, "success": false, "skipped": true, "reason": "not_scheduled", "message": "任务已处理、未到期或安全状态容量已满"}
	}
	finish := func(result checkinAttemptResult) map[string]any {
		out := finishCheckin(f.AuthIndex, before, result)
		out["name"] = f.Name
		return out
	}
	if listErr != nil {
		return finish(checkinAttemptResult{state: "failed", message: "无法读取签到账号列表", retryable: true, confirmOnly: before.confirmOnly})
	}
	phys, err := hostAuthGetPhysicalFn(f.AuthIndex)
	if err != nil {
		return finish(checkinFailed(err, before.confirmOnly))
	}
	if phys == nil {
		return finish(checkinAttemptResult{state: "failed", message: "签到凭证不存在", confirmOnly: before.confirmOnly})
	}
	if source != "manual" && (phys.Disabled || parseDisabledFromAuthJSON(phys.JSON)) {
		return finish(checkinAttemptResult{state: "disabled", message: "账号已禁用，跳过自动签到及重试", confirmOnly: before.confirmOnly})
	}
	sa, err := parseStored(phys.JSON)
	if err != nil {
		return finish(checkinFailed(err, before.confirmOnly))
	}
	if !supportsCheckin(sa) {
		return finish(checkinAttemptResult{state: "unsupported", message: "当前区域不支持每日签到"})
	}
	started := time.Now()
	result := confirmedCheckinAttempt(sa, before)
	if result.confirmed && (!before.confirmed || result.campaignID != "" && result.campaignID != before.campaignID) {
		// The first confirmation starts grace. Re-reading an existing receipt must
		// not extend grace indefinitely on repeated button presses.
		storeCheckinConfirmation(f.ID, result.status, time.Now())
	} else if result.status != nil {
		storeAccountSnapshot(f.ID, &accountCacheEntry{checkin: result.status, checkinFetched: started})
	}
	out := finish(result)
	out["nickname"] = sa.Account.Nickname
	if result.confirmed {
		// A fresh credits read may fail without invalidating a grant receipt. Never
		// restamp fallback credit data as newly fetched, and never change disabled.
		creditStart := time.Now()
		if cr, crErr := fetchUserResourceFn(sa); crErr == nil && cr != nil {
			fresh := *cr
			fresh.FetchedAt = time.Now().UTC().Format(time.RFC3339)
			storeAccountSnapshot(f.ID, &accountCacheEntry{credits: &fresh, creditsFetched: creditStart})
			out["credits"] = &fresh
		}
	}
	return out
}

// Called only from the existing loop, with no retained request callbacks. The
// due scan doesn't spend the budget: reserve under each account lock does.
func runDueCheckinRetries() {
	if !checkinRetryDispatch.TryLock() {
		return
	}
	defer checkinRetryDispatch.Unlock()
	if !checkinAutoEnabled() {
		cancelScheduledCheckinRetries()
		return
	}
	now := time.Now()
	var due []pluginapi.HostAuthFileEntry
	checkinRuns.Lock()
	for index, r := range checkinRuns.records {
		if r.day != now.Format("2006-01-02") {
			r.next = time.Time{}
			checkinRuns.records[index] = r
			continue
		}
		if !r.next.IsZero() && !r.next.After(now) {
			due = append(due, pluginapi.HostAuthFileEntry{AuthIndex: index, ID: r.authID})
		}
	}
	checkinRuns.Unlock()
	if len(due) == 0 {
		return
	}
	files, listErr := hostAuthList()
	live := make(map[string]pluginapi.HostAuthFileEntry, len(files))
	for _, f := range files {
		live[f.AuthIndex] = f
	}
	for _, f := range due {
		if listErr == nil {
			entry, exists := live[f.AuthIndex]
			if !exists {
				cancelCheckinRetry(f.AuthIndex)
				continue
			}
			f = entry
		}
		runCheckinAccount(f, "retry", listErr)
	}
}

// summarizeCheckinClaimRes renders a non-success claim response as one
// human-readable line. QoderWork failure bodies carry structured fields
// (result / message / error as objects) — the panel's string concatenation of
// such a map produced "[object Object]" (2026-09-20 field report), so the
// summary is built server-side. Known scalar fields surface directly;
// unknown shapes fall back to bounded redacted JSON.
func summarizeCheckinClaimRes(res map[string]any) string {
	parts := make([]string, 0, 4)
	if r, ok := res["result"].(string); ok && r != "" {
		parts = append(parts, "result="+r)
	}
	for _, k := range []string{"message", "msg", "error"} {
		switch v := res[k].(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				parts = append(parts, k+"="+v)
			}
		case map[string]any:
			if b, err := json.Marshal(v); err == nil {
				parts = append(parts, k+"="+truncateRedacted(string(b), 160))
			}
		}
	}
	if c, ok := res["code"].(float64); ok {
		parts = append(parts, fmt.Sprintf("code=%d", int64(c)))
	}
	if len(parts) == 0 {
		if b, err := json.Marshal(res); err == nil && string(b) != "{}" {
			return "上游未确认签到成功：" + truncateRedacted(string(b), 240)
		}
		return "上游未确认签到成功（无明细）"
	}
	return "上游未确认签到成功：" + strings.Join(parts, " · ")
}

// checkinGraceWindow 是签到成功后 lifecycle 禁用动作的宽限期。签到发放的
// 积分在 quota/usage 接口上有刷新延迟，紧随签到之后的 force refresh
// （面板 load(true)）可能读到旧的 remain=0 快照 —— 没有宽限就会把刚
// 签到的 CN 账号写入 disabled:true（“点签到，账号被禁用且刷新不回来”）。
const checkinGraceWindow = 10 * time.Minute

// rememberCheckinMoment stamps the cache entry (creating one when absent) with
// the check-in time so reconcileOneAccount can honor the grace window.
func rememberCheckinMoment(authID string) {
	if authID != "" {
		storeCheckinConfirmation(authID, nil, time.Now())
	}
}

// checkinGraceActive reports whether authID checked in within the grace window.
func checkinGraceActive(authID string) bool {
	if v, ok := accountCache.Load(authID); ok {
		if e, ok2 := v.(*accountCacheEntry); ok2 {
			return !e.checkinAt.IsZero() && time.Since(e.checkinAt) < checkinGraceWindow
		}
	}
	return false
}

// Fixed stripes cannot be deleted while a waiter still holds an old lock
// pointer (dashboard pruning used to break per-account mutual exclusion).
var checkinLockStripes [256]sync.Mutex

func checkinLockFor(authIndex string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(authIndex))
	return &checkinLockStripes[h.Sum32()%uint32(len(checkinLockStripes))]
}

// checkProUpgradeEligibility returns whether the account can still claim the
// one-time Pro Upgrade pack (+1800).
func checkProUpgradeEligibility(sa *storedAuth) (bool, error) {
	if !supportsProUpgrade(sa) {
		return false, &unsupportedCheckinError{Region: authRegion(sa)}
	}
	req, err := http.NewRequest(http.MethodGet, upstreamBaseFor(sa)+"/sash/api/v1/me/pro-upgrade/eligibility", nil)
	if err != nil {
		return false, err
	}
	billingHeaders(req, sa)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return false, err
	}
	if resp.StatusCode >= 400 {
		return false, fmt.Errorf("http %d", resp.StatusCode)
	}
	var m struct {
		Eligible bool `json:"eligible"`
	}
	if err := json.Unmarshal(resp.Body, &m); err != nil {
		return false, err
	}
	return m.Eligible, nil
}

// claimProUpgrade claims the one-time Pro Upgrade pack (+1800 credits).
func claimProUpgrade(sa *storedAuth) (map[string]any, error) {
	req, err := http.NewRequest(http.MethodPost, endpointProUpgradeFor(sa), strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	billingHeaders(req, sa)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return map[string]any{"success": false, "message": err.Error()}, nil
	}
	if resp.StatusCode >= 400 {
		return map[string]any{"success": false, "message": fmt.Sprintf("http %d: %s", resp.StatusCode, truncateRedacted(string(resp.Body), 200))}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(resp.Body, &m); err != nil {
		return nil, err
	}
	if _, ok := m["success"]; !ok {
		m["success"] = true
	}
	return m, nil
}

// pruneCheckinLocks removes lock entries for auth indices that no longer
// exist in hostAuthList. Call after dashboard prune.
// Lock keys are auth_index (used for host RPC), so live map needs auth_index too.
func pruneCheckinLocks() {
	files, err := hostAuthList()
	if err != nil {
		return
	}
	live := make(map[string]struct{}, len(files))
	for _, f := range files {
		live[f.ID] = struct{}{}
		live[f.AuthIndex] = struct{}{} // checkinLockFor uses auth_index as key
	}
	checkinLocks.Range(func(key, _ any) bool {
		idx, _ := key.(string)
		if _, ok := live[idx]; !ok {
			checkinLocks.Delete(key)
		}
		return true
	})
}

// handleClaimPro checks pro-upgrade eligibility for one account and claims
// the one-time Pro pack (+ credits) when eligible. Surfaced as a panel
// per-card button — NOT called automatically during login (login writes the
// auth file first; this is a user-triggered post-login action).
// handleClaimPro claims the one-time Pro upgrade pack for one account, or for
// every claimable account when auth_index is omitted.
//
// The panel calls this from a single top-bar button rather than per card: the
// pack is a one-time account-level benefit, so asking the operator to click each
// card in turn only made a whole-account action look per-account.
func handleClaimPro(req pluginapi.ManagementRequest) map[string]any {
	t0 := time.Now()
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(req.Body, &body)
	authIndex := strings.TrimSpace(body.AuthIndex)
	single := authIndex != ""

	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	var targets []pluginapi.HostAuthFileEntry
	for _, f := range files {
		if !single || f.AuthIndex == authIndex {
			targets = append(targets, f)
		}
	}
	if len(targets) == 0 {
		return map[string]any{"error": "no matching account"}
	}

	// Same bounded-concurrency shape as handleManualCheckin: one goroutine per
	// account, at most four in flight, results returned in input order so the
	// panel can pair each row with its account.
	type result struct {
		idx int
		out map[string]any
	}
	outCh := make(chan result, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, f := range targets {
		wg.Add(1)
		go func(i int, f pluginapi.HostAuthFileEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			outCh <- result{idx: i, out: claimProOneAccount(f.AuthIndex)}
		}(i, f)
	}
	wg.Wait()
	close(outCh)

	results := make([]map[string]any, len(targets))
	for r := range outCh {
		results[r.idx] = r.out
	}
	successN, alreadyN, unsupportedN, failN := 0, 0, 0, 0
	for _, r := range results {
		if r["error"] != nil {
			failN++
			continue
		}
		if r["reason"] == "unsupported" {
			unsupportedN++
			continue
		}
		if r["success"] == true {
			successN++
			continue
		}
		// Not eligible with no error means the pack was already taken or the
		// campaign is closed — a normal outcome, not a failure.
		alreadyN++
	}
	resp := map[string]any{
		"results": results,
		"summary": map[string]any{
			"total":       len(targets),
			"success":     successN,
			"already":     alreadyN,
			"unsupported": unsupportedN,
			"fail":        failN,
			"elapsed_ms":  time.Since(t0).Milliseconds(),
		},
	}
	// The single-account shape is what the old per-card button returned, so keep
	// it at the top level for that caller.
	if single && len(results) == 1 {
		for k, v := range results[0] {
			if k == "auth_index" || k == "nickname" {
				continue
			}
			resp[k] = v
		}
		resp["auth_index"] = results[0]["auth_index"]
		resp["nickname"] = results[0]["nickname"]
	}
	return resp
}

// claimProOneAccount runs the eligibility + claim flow for a single account.
func claimProOneAccount(authIndex string) map[string]any {
	authIndex = strings.TrimSpace(authIndex)
	out := map[string]any{"auth_index": authIndex}
	sa, err := hostAuthGet(authIndex)
	if err != nil {
		out["error"] = "get auth: " + err.Error()
		return out
	}
	out["nickname"] = sa.Account.Nickname
	if !supportsProUpgrade(sa) {
		out["success"] = false
		out["skipped"] = true
		out["reason"] = "unsupported"
		out["message"] = "Intl 账号不支持 Pro 升级包领取"
		return out
	}
	eligible, err := checkProUpgradeEligibility(sa)
	if err != nil {
		out["error"] = "eligibility: " + err.Error()
		return out
	}
	if !eligible {
		out["success"] = false
		out["message"] = "不可领取（已领或活动未开放）"
		return out
	}
	res, err := claimProUpgrade(sa)
	if err != nil {
		out["error"] = "claim: " + err.Error()
		return out
	}
	for k, v := range res {
		out[k] = v
	}
	if _, ok := out["success"]; !ok {
		out["success"] = true
	}
	// Refresh the credits snapshot so the panel shows the new balance at once.
	if cr, crErr := fetchUserResource(sa); crErr == nil && cr != nil {
		out["credits"] = cr
	}
	return out
}
