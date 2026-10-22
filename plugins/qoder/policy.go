// policy.go is the pure decision layer for credit-driven lifecycle actions:
// given an account's region and current credits, decide whether to disable
// CN: disable when exhausted, re-enable after check-in restores credits, or
// leave it alone. No I/O happens here — reconcileOneAccount consumes these
// decisions and applies them via the lifecycle.go authfile helpers.
package main

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// lifecycleAction is the policy decision for one account.
type lifecycleAction int

const (
	lifecycleNone lifecycleAction = iota
	lifecycleDisable
	lifecycleDelete
	lifecycleReenable
)

func (a lifecycleAction) String() string {
	switch a {
	case lifecycleDisable:
		return "disable"
	case lifecycleDelete:
		return "delete"
	case lifecycleReenable:
		return "reenable"
	default:
		return "none"
	}
}

// lifecycleAuto gates automatic disable/delete/reenable. Default true.
var (
	lifecycleAuto   = true
	lifecycleAutoMu sync.RWMutex
)

func lifecycleEnabled() bool {
	lifecycleAutoMu.RLock()
	defer lifecycleAutoMu.RUnlock()
	return lifecycleAuto
}

// shouldActOnCredits is true only when credits are *known* exhausted.
// nil / empty (no packages, no used) is unknown → false.
func shouldActOnCredits(cr *creditsSummary) bool {
	return isCreditsExhausted(cr)
}

// hardCreditMarkers are case-insensitive substrings in upstream error bodies.
var hardCreditMarkers = []string{
	"insufficient credit",
	"insufficient credits",
	"no credit",
	"no credits",
	"credit exhausted",
	"credits exhausted",
	"out of credit",
	"out of credits",
	"quota exceeded",
	"quota exhaust",
	"payment required",
	"积分不足",
	"额度不足",
	"余额不足",
	"积分用完",
	"额度用尽",
	"没有积分",
	"credit not enough",
	"not enough credit",
}

// isHardCreditError reports business "out of credits" style failures.
// 402 is treated as payment/credit. Pure 429 is not hard unless body has credit markers.
func isHardCreditError(status int, body string) bool {
	// Legacy billing-refresh hint only. Freeze scope is decided separately by
	// classifyQuotaReason; neither this heuristic nor a balance disables auths.
	if status == httpStatusPaymentRequired {
		return true
	}
	lower := strings.ToLower(body)
	for _, marker := range hardCreditMarkers {
		if strings.Contains(lower, strings.ToLower(marker)) {
			return true
		}
	}
	return false
}

const httpStatusPaymentRequired = 402

// isSoftRateLimit is pure throttling without hard-credit semantics.
//
// ★ 10605 queue admission: the gateway refuses a request that it has ACCEPTED
// into a short admission queue with HTTP 403 rather than 429, body
// {"code":"10605","message":"{\"isQueued\":true,\"modelKey\":\"qfmodel\",
// \"queueType\":\"p3\",\"retryAfterSeconds\":30}"}. Before isQueueAdmission,
// this fell through every branch and the refusal was terminal. Because the
// status is 403, the host classified the forwarded message as AUTH, so the
// client reported a bogus API-key failure and never retried.
//
// Measured admission window on qfmodel is 1-5s (not the advertised 30s), so
// the existing retryBackoffSchedule (0.5-4s per attempt, 3 attempts) absorbs
// it; the retryAfterSeconds hint is not used as a delay floor on purpose.
func isSoftRateLimit(status int, body string) bool {
	// A daily cap cannot recover through the short queue retry loop.
	if isDailyCountLimit(status, body) {
		return false
	}
	// Queue admission is checked BEFORE the credit gate on purpose. The
	// gateway's 10605 body is a nested document whose tail is truncated in
	// transit, and on a credential without a quota package it can also carry
	// credit wording further along. When that happened the credit gate won:
	// isHardCreditError returned true, this function returned false, and the
	// queue never retried — the exact 403-then-no-retry seen in production on
	// the Intl credential. A request that was queued was still accepted, so
	// "queued" must outrank "looks like a credit fault" here. Genuine credit
	// rejections never carry queue fields, so they keep their old path.
	if isQueueAdmission(status, body) {
		return true
	}
	if isHardCreditError(status, body) {
		return false
	}
	if status == 429 {
		return true
	}
	lower := strings.ToLower(body)
	return strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "throttl")
}

// upstreamQueueCode is the gateway's queue-admission business code, in the
// unescaped form used after backslashes are stripped by isQueueAdmission.
const upstreamQueueCode = `"code":"10605"`

// isQueueAdmission reports the gateway's HTTP 403 "you are queued" refusal.
//
// The body reaches this layer JSON-escaped one or more levels deep (the
// message field itself contains a JSON document), so the code appears as
// `"code":"10605"` or as `\"code\":\"10605\"`. Stripping backslashes matches
// both forms. Gating on 403 is safe because the gateway assigns that status
// only to this code; a genuine credential rejection carries no queue fields.
func isQueueAdmission(status int, body string) bool {
	if status != http.StatusForbidden {
		return false
	}
	flat := strings.ReplaceAll(body, `\`, "")
	if strings.Contains(flat, upstreamQueueCode) || queueCodePattern.MatchString(flat) {
		return true
	}
	// Defensive: honour queue semantics even if the numeric code shifts.
	lower := strings.ToLower(flat)
	return strings.Contains(lower, `"isqueued":true`) ||
		strings.Contains(lower, `"queuetype"`)
}

// lifecycleActionFor chooses disable/none from credits.
//
// ★ Credits exhausted no longer disables the account. The catalog carries
// free models (price_factor 0, e.g. qfmodel), and those stay callable on a
// credential with 0 remaining credits — so "耗尽" does NOT mean the credential
// is useless. The old behaviour disabled the whole auth, which took every free
// model down with it: the account kept its catalog in the panel but CPA never
// offered it as a candidate again, and because a disabled account receives no
// traffic the balance could never move on its own — a deadlock, not a policy.
// (Intl had no check-in to restore credits, so there the account was parked
// forever; this is the same defect workbuddy retired in 0.9.10. Intl gained the
// campaign check-in on 2026-10-21 but that only makes the rule less pressing —
// it does not change it: exhausted credits are still not a reason to disable.)
//
// The upstream is the authority on what a drained credential may still call:
// a refused paid model answers with a hard-credit error, which
// isHardCreditError already classifies for cooldown and for the panel's
// exhausted badge. Leaving the account enabled costs nothing, because CPA
// routes elsewhere when a credential genuinely cannot serve.
//
// Kept as a named function (rather than deleting the call site) so the region
// split and its rationale remain visible if this ever needs to come back.
func lifecycleActionFor(region string, cr *creditsSummary) lifecycleAction {
	if !shouldActOnCredits(cr) {
		return lifecycleNone
	}
	// Exhausted: leave the credential in place so free (rate 0) models keep
	// working; a paid model's own upstream error is the correct signal.
	return lifecycleNone
}

// shouldReenable is the balance-only migration prerequisite. Callers MUST also
// require legacyExhaustedDisable on the latest physical document while holding
// the account lock. Unknown/manual/session-dead disables are never automatic.
// Positive balance is not required for an explicitly proven old exhaustion
// disable: this fork keeps free models available at zero credits.
func shouldReenable(disabled bool, cr *creditsSummary) bool {
	if !disabled {
		return false
	}
	if cr == nil {
		return false
	}
	return true
}

// chatSizeMarkers are substrings (case-insensitive) of upstream rejections
// caused by oversized input/context. These are REQUEST-level problems: they
// must never read as account trouble (credits) and deserve actionable copy.
var chatSizeMarkers = []string{
	"too long", "too large", "context length", "context_length", "context too",
	"max input", "input token", "token limit", "prompt is too long",
	"内容过长", "输入过长", "上下文过长", "上下文太长", "超过最大",
}

// chatInputTooLarge reports whether an upstream chat rejection was caused by
// oversized input: explicit HTTP 413 (without credit semantics), or a body
// naming a size/context limit.
func chatInputTooLarge(status int, body string) bool {
	if status == http.StatusRequestEntityTooLarge {
		return !isHardCreditError(status, body)
	}
	lower := strings.ToLower(body)
	for _, m := range chatSizeMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// statusError carries an upstream HTTP status across the RPC boundary. The
// host's decodeEnvelopeResult rebuilds it as rpcError (via the envelope error
// http_status field, see errorEnvelopeFor) whose StatusCode() drives
// MarkResult's per-status cooldown: 402 -> 30 min, 429 -> escalating quota
// backoff (credential-scoped across models), 401 -> 30 min. Free-tier
// exhaustion (qfmodel 等) typically surfaces as 429 — this is exactly the
// "stop hammering a drained credential" behavior requested on 2026-09-20.
type statusError struct {
	status int
	err    error
}

func (e *statusError) Error() string   { return e.err.Error() }
func (e *statusError) StatusCode() int { return e.status }
func (e *statusError) Unwrap() error   { return e.err }

// upstreamStatusError wraps a translated upstream chat failure with the HTTP
// status the host cooldown layer should attribute to the credential.
//
// Typed Qoder business failures (daily quota, shared/model credit, queue)
// retain plugin-owned cooldown semantics and never carry auth-looking status.
// Other errors preserve the historical 401/402/429 passthrough policy.
func upstreamStatusError(status int, err error) error {
	if safe := queueErrorForClient(err); safe != err {
		return safe
	}
	switch err.(type) {
	case *dailyQuotaError, *queueAdmissionError, *quotaObservationError:
		return err
	}
	if status == http.StatusUnauthorized ||
		status == http.StatusPaymentRequired ||
		status == http.StatusTooManyRequests {
		return &statusError{status: status, err: err}
	}
	return err
}

// emptyStreamError renders an upstream "stream closed before first payload"
// failure.
//
// The wording is load-bearing. When the gateway accepts the request and then
// closes the SSE stream without sending anything, that is a transport
// lifecycle event, not a credential or quota fault. CPA classifies the error
// from the message text that survives the RPC boundary, so the trailing
// "(unexpected EOF)" makes the host treat it as a connection-lifecycle failure:
// `shouldSkipCredentialCooldown` returns early, the credential keeps its
// healthy status, and single-model flakiness cannot promote the whole auth
// into a cooldown. The plugin still records the pair in its own per-model
// cooldown table for panel visibility.
func emptyStreamError() error {
	return fmt.Errorf("empty_stream: upstream stream closed before first payload (unexpected EOF)")
}

// upstreamReadError renders a mid-stream read failure with the same
// transport-lifecycle classification as emptyStreamError.
func upstreamReadError(err error) error {
	return fmt.Errorf("upstream stream read error (unexpected EOF): %w", err)
}

// chatUpstreamError renders an upstream chat failure for the client, adding
// actionable copy when the rejection was caused by oversized input so users
// don't mistake it for an account/quota problem.
func chatUpstreamError(status int, body string) error {
	if quota := quotaErrorWithHeaders(status, body, nil); quota != nil {
		return quota
	}
	if queued := newQueueAdmissionError(status, body); queued != nil {
		return queued
	}
	trimmed := truncateRedacted(body, 200)
	if chatInputTooLarge(status, body) {
		return fmt.Errorf("输入过大被上游拒绝（请求级问题，与账号无关）：请压缩上下文/清理会话后重试 — upstream %d: %s", status, trimmed)
	}
	return fmt.Errorf("upstream %d: %s", status, trimmed)
}

// notePrefix renders the region/disabled head of an auth-card note, without
// the credit segment. Kept separate so syncAuthNote can rebuild a note while
// preserving a previously-known credit segment.
func notePrefix(sa *storedAuth, disabled bool) string {
	region := "CN"
	if sa != nil && authRegion(sa) == regionIntl {
		region = "INTL"
	}
	parts := []string{region}
	if disabled {
		parts = append(parts, "已禁用")
	}
	return strings.Join(parts, " · ")
}

// creditSegmentFromNote extracts the credit segment of an existing auth note
// (everything after the region / disabled prefix). Returns "" when the note
// carries no usable credit information, so callers never resurrect "积分未知".
func creditSegmentFromNote(note string) string {
	parts := strings.Split(note, " · ")
	segments := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || part == "CN" || part == "INTL" || part == "已禁用" {
			continue
		}
		segments = append(segments, part)
	}
	seg := strings.Join(segments, " · ")
	if seg == "" || strings.HasPrefix(seg, "积分未知") {
		return ""
	}
	return seg
}

// displayNote builds a one-line note for CPAMP Auth cards.
//
// cr == nil means "credits unknown right now" (startup, a lazy panel refresh,
// or a failed billing call). displayNote falls back to the placeholder because
// it has no disk access; callers that can read the previous note should prefer
// displayNoteWithPrev so a restart or transient billing error cannot regress a
// card that already shows live credits.
func displayNote(sa *storedAuth, cr *creditsSummary, disabled bool) string {
	return displayNoteWithPrev(sa, cr, disabled, "")
}

// displayNoteWithPrev is displayNote plus a previously known credit segment.
// prev is ignored whenever cr carries fresh data or holds no usable value.
func displayNoteWithPrev(sa *storedAuth, cr *creditsSummary, disabled bool, prev string) string {
	parts := []string{notePrefix(sa, disabled)}
	switch {
	case cr == nil:
		if prev = creditSegmentFromNote(prev); prev != "" {
			parts = append(parts, prev)
		} else {
			parts = append(parts, "积分未知")
		}
	case cr.NoAddOnPack && cr.TotalUsed == 0:
		// The account has no quota at all: the gateway omitted its add-on pack and
		// reports every balance as zero. Saying "耗尽 · 余0 已用0" here is actively
		// misleading — it reads as "spent everything", which sends the operator
		// looking for usage that never happened. Nothing was spent; there is simply
		// no pack on this account.
		parts = append(parts, "无可用额度包（上游未下发）")
	case isCreditsExhausted(cr):
		parts = append(parts, fmt.Sprintf("耗尽 · 余%d 已用%d", cr.TotalRemain, cr.TotalUsed))
	default:
		// Show remain as primary (what you can still spend). Used is real cycle spend.
		// Size (capacity) grows with check-in packs — do not treat size↑ as usage↓.
		if cr.TotalSize > 0 {
			parts = append(parts, fmt.Sprintf("余%d 已用%d 池%d", cr.TotalRemain, cr.TotalUsed, cr.TotalSize))
		} else {
			parts = append(parts, fmt.Sprintf("余%d 已用%d", cr.TotalRemain, cr.TotalUsed))
		}
	}
	note := strings.Join(parts, " · ")
	if len(note) > 80 {
		note = note[:77] + "..."
	}
	return note
}

// labelForAuth adds the account region for host labels.
func labelForAuth(sa *storedAuth) string {
	base := "Qoder"
	if sa != nil && strings.TrimSpace(sa.Account.Nickname) != "" {
		base = strings.TrimSpace(sa.Account.Nickname)
	}
	tag := "CN"
	if authRegion(sa) == regionIntl {
		tag = "Intl"
	}
	return base + " [" + tag + "]"
}
