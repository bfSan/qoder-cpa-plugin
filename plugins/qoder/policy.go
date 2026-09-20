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
	if status == httpStatusPaymentRequired {
		return true
	}
	lower := strings.ToLower(body)
	for _, m := range hardCreditMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	// Chinese markers may not lower-map usefully; also scan raw.
	for _, m := range hardCreditMarkers {
		if strings.Contains(body, m) {
			return true
		}
	}
	return false
}

const httpStatusPaymentRequired = 402

// isSoftRateLimit is pure throttling without hard-credit semantics.
func isSoftRateLimit(status int, body string) bool {
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

// lifecycleActionFor chooses disable/none from credits.
// QoderWork is CN-only — disable (not delete) so check-in can restore credits
// without forcing the user to re-import a PAT.
func lifecycleActionFor(region string, cr *creditsSummary) lifecycleAction {
	if !shouldActOnCredits(cr) {
		return lifecycleNone
	}
	return lifecycleDisable
}

// shouldReenableCN is true when a CN account is disabled but now has credits.
func shouldReenableCN(disabled bool, cr *creditsSummary) bool {
	if !disabled {
		return false
	}
	if cr == nil {
		return false
	}
	if isCreditsExhausted(cr) {
		return false
	}
	// Known positive remain, or non-exhausted with packages still having room.
	return cr.TotalRemain > 0
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
// qoder variant: only unambiguous account-level statuses pass (401/402/429).
// 403 and the request-level shapes (413/输入过大 etc.) stay status-less — the
// qoder upstream has no evidenced business-403 family, and chatSizeMarkers
// failures are request-level by definition; both keep the host's 1-minute
// transient default (pre-0.8.13 behavior unchanged).
func upstreamStatusError(status int, err error) error {
	if status == http.StatusUnauthorized ||
		status == http.StatusPaymentRequired ||
		status == http.StatusTooManyRequests {
		return &statusError{status: status, err: err}
	}
	return err
}

// chatUpstreamError renders an upstream chat failure for the client, adding
// actionable copy when the rejection was caused by oversized input so users
// don't mistake it for an account/quota problem.
func chatUpstreamError(status int, body string) error {
	trimmed := truncateRedacted(body, 200)
	if chatInputTooLarge(status, body) {
		return fmt.Errorf("输入过大被上游拒绝（请求级问题，与账号无关）：请压缩上下文/清理会话后重试 — upstream %d: %s", status, trimmed)
	}
	return fmt.Errorf("upstream %d: %s", status, trimmed)
}

// displayNote builds a one-line note for CPAMP Auth cards.
func displayNote(sa *storedAuth, cr *creditsSummary, disabled bool) string {
	region := "CN"
	if sa != nil && authRegion(sa) == regionIntl {
		region = "INTL"
	}
	parts := []string{region}
	if disabled {
		parts = append(parts, "已禁用")
	}
	switch {
	case cr == nil:
		parts = append(parts, "积分未知")
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

// labelForAuth adds [CN] for host labels.
func labelForAuth(sa *storedAuth) string {
	base := "QoderWork"
	if sa != nil && strings.TrimSpace(sa.Account.Nickname) != "" {
		base = strings.TrimSpace(sa.Account.Nickname)
	}
	tag := "CN"
	if "cn" == "global" {
		tag = "CN"
	}
	return base + " [" + tag + "]"
}
