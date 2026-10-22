package main

// Quota evidence is deliberately process-local. A billing snapshot (including
// zero balance / no packages) is never evidence that every model is unusable.
import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	cooldownReasonSharedCredit cooldownReason = "shared_credit_exhausted"
	cooldownReasonModelCredit  cooldownReason = "model_credit_exhausted"
	cooldownReasonModelQuota   cooldownReason = "model_quota"
)

type quotaHint struct {
	ResetAt time.Time
	Source  string
}

type quotaObservationError struct {
	reason cooldownReason
	hint   quotaHint
}

func (e *quotaObservationError) Error() string {
	if e.reason == cooldownReasonSharedCredit {
		return "qoder quota exceeded: 上游明确报告账号共享积分耗尽，暂时冻结；新余额到账或到探测时间后可重试 (SHARED_CREDIT_EXHAUSTED; unexpected EOF)"
	}
	return "qoder quota exceeded: 上游拒绝此模型的额度请求，仅此模型暂时冷却；并非密钥失效 (MODEL_QUOTA_LIMIT; unexpected EOF)"
}

// classifyQuotaReason does not import another provider's business codes. The
// Qoder 403/110 daily message and 10605 queue have priority over credit wording.
func classifyQuotaReason(status int, body string) cooldownReason {
	if isDailyCountLimit(status, body) {
		return cooldownReasonDailyQuota
	}
	if isQueueAdmission(status, body) {
		return ""
	}
	if strings.HasPrefix(body, "qoder quota exceeded:") {
		if strings.Contains(body, "SHARED_CREDIT_EXHAUSTED;") {
			return cooldownReasonSharedCredit
		}
		if strings.Contains(body, "MODEL_QUOTA_LIMIT;") {
			return cooldownReasonModelQuota
		}
	}
	if status < 400 || status > 599 || status == 401 {
		return ""
	}
	lower := strings.ToLower(strings.ReplaceAll(body, `\`, ""))
	// Auth failures carrying code 110 must remain real auth failures unless the
	// exact daily-count predicate above succeeds.
	if dailyQuotaCodePattern.MatchString(lower) {
		return ""
	}
	credit := false
	for _, marker := range []string{"insufficient credit", "credits exhausted", "credit exhausted", "out of credits", "out of credit", "credit not enough", "not enough credit", "积分不足", "积分用完", "积分耗尽", "没有积分"} {
		if strings.Contains(lower, marker) {
			credit = true
			break
		}
	}
	shared := strings.Contains(lower, "insufficient shared credits") || strings.Contains(lower, "insufficient account credits") || strings.Contains(lower, "shared credits exhausted") || strings.Contains(lower, "shared credit exhausted") || strings.Contains(lower, "account credits exhausted") || strings.Contains(lower, "account credit exhausted") || strings.Contains(lower, "共享积分耗尽") || strings.Contains(lower, "账号积分耗尽")
	// Explicit structured account/shared scope is stronger than an incidental
	// mention of an account in an error, and only applies to a credit rejection.
	if credit {
		walkQuotaObjects(body, 0, func(m map[string]any) {
			for _, key := range []string{"scope", "quotaScope", "quota_scope"} {
				if s, ok := m[key].(string); ok && (s == "account" || s == "shared" || s == "shared_credit") {
					shared = true
				}
			}
		})
	}
	if shared {
		return cooldownReasonSharedCredit
	}
	if credit {
		return cooldownReasonModelCredit
	}
	for _, marker := range []string{"quota exceeded", "quota exhausted", "额度不足", "额度用尽"} {
		if strings.Contains(lower, marker) {
			return cooldownReasonModelQuota
		}
	}
	return ""
}

// Traverse only error-envelope fields, never choices/content or arbitrary user
// objects. Nesting and string decoding are bounded for untrusted error bodies.
func walkQuotaObjects(body string, depth int, visit func(map[string]any)) {
	if depth > 6 || len(body) > 4*1024*1024 {
		return
	}
	var m map[string]any
	if json.Unmarshal([]byte(body), &m) != nil || m == nil {
		return
	}
	visit(m)
	for _, key := range []string{"error", "body", "message", "data"} {
		switch v := m[key].(type) {
		case string:
			walkQuotaObjects(v, depth+1, visit)
		case map[string]any:
			b, _ := json.Marshal(v)
			walkQuotaObjects(string(b), depth+1, visit)
		}
	}
}

// Only explicit Retry-After and named reset values are interpreted. There is no
// timezone / midnight guess. Expired, malformed and overflow values are ignored.
func parseQuotaHint(headers http.Header, body string, now time.Time) quotaHint {
	var hint quotaHint
	take := func(t time.Time, source string) {
		if t.After(now) && !t.After(now.Add(365*24*time.Hour)) && (hint.ResetAt.IsZero() || t.After(hint.ResetAt)) {
			hint = quotaHint{t, source}
		}
	}
	retry := func(s, source string) {
		s = strings.TrimSpace(s)
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			if n > 0 && n <= int64((365*24*time.Hour)/time.Second) {
				take(now.Add(time.Duration(n)*time.Second), source)
			}
		} else if t, err := http.ParseTime(s); err == nil {
			take(t, source)
		}
	}
	reset := func(s, source string) {
		if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s)); err == nil {
			take(t, source)
			return
		}
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil || n <= 0 {
			return
		}
		if n >= 1e12 {
			take(time.UnixMilli(n), source)
		} else {
			take(time.Unix(n, 0), source)
		}
	}
	for key, vals := range headers {
		for _, v := range vals {
			switch strings.ToLower(key) {
			case "retry-after":
				retry(v, "retry_after")
			case "x-ratelimit-reset", "x-quota-reset":
				reset(v, "reset_header")
			}
		}
	}
	walkQuotaObjects(body, 0, func(m map[string]any) {
		for _, key := range []string{"retryAfterSeconds", "retry_after_seconds"} {
			if v, ok := m[key]; ok {
				retry(quotaScalar(v), "retry_after_body")
			}
		}
		for _, key := range []string{"resetAt", "reset_at", "resetTime", "reset_time", "resetTimestamp", "reset_timestamp"} {
			if v, ok := m[key]; ok {
				reset(quotaScalar(v), "reset_body")
			}
		}
	})
	return hint
}
func quotaScalar(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return x.String()
	}
	return ""
}

func quotaErrorWithHeaders(status int, body string, headers http.Header) error {
	reason := classifyQuotaReason(status, body)
	hint := parseQuotaHint(headers, body, cooldownNowFn())
	if reason == cooldownReasonDailyQuota {
		return &dailyQuotaError{hint: hint}
	}
	if reason != "" {
		return &quotaObservationError{reason: reason, hint: hint}
	}
	return nil
}

func recordUpstreamFailureWithHeaders(authID, model string, status int, body string, headers http.Header) {
	reason := classifyQuotaReason(status, body)
	if reason != "" {
		recordQuotaObservation(authID, model, reason, parseQuotaHint(headers, body, cooldownNowFn()))
		return
	}
	recordUpstreamFailureLegacy(authID, model, status, body)
	if isSoftRateLimit(status, body) {
		recordCooldownHint(authID, model, parseQuotaHint(headers, body, cooldownNowFn()))
	}
}
func recordUpstreamError(authID, model string, status int, err error) {
	if err == nil {
		return
	}
	var daily *dailyQuotaError
	if errors.As(err, &daily) {
		recordQuotaObservation(authID, model, cooldownReasonDailyQuota, daily.hint)
		return
	}
	var quota *quotaObservationError
	if errors.As(err, &quota) {
		recordQuotaObservation(authID, model, quota.reason, quota.hint)
		return
	}
	recordUpstreamFailure(authID, model, status, err.Error())
	var queued *queueAdmissionError
	if errors.As(err, &queued) {
		recordCooldownHint(authID, model, queued.hint)
	}
}
func recordCooldownHint(authID, model string, hint quotaHint) {
	if !hint.ResetAt.After(cooldownNowFn()) {
		return
	}
	key := modelCooldownKey{AuthID: strings.TrimSpace(authID), ModelID: normalizeCooldownModel(model)}
	cooldownMu.Lock()
	defer cooldownMu.Unlock()
	entry, ok := cooldownTable[key]
	if !ok || entry.Reason != cooldownReasonRateLimit {
		return
	}
	entry.Until, entry.Hint = hint.ResetAt, hint
	cooldownTable[key] = entry
}

func recordQuotaObservation(authID, model string, reason cooldownReason, hint quotaHint) {
	authID, model = strings.TrimSpace(authID), normalizeCooldownModel(model)
	if authID == "" {
		return
	}
	if reason == cooldownReasonSharedCredit {
		model = ""
	} else if model == "" {
		return
	}
	now := cooldownNowFn()
	until := now.Add(modelCooldownDailyQuota)
	if hint.ResetAt.After(now) {
		until = hint.ResetAt
	}
	cooldownMu.Lock()
	defer cooldownMu.Unlock()
	cooldownSweepLocked(now)
	key := modelCooldownKey{AuthID: authID, ModelID: model}
	// Duplicate or weaker in-flight failures must not extend a finite freeze.
	if old, ok := cooldownTable[key]; ok && (old.Reason == cooldownReasonDailyQuota || old.Reason == reason) {
		return
	}
	if len(cooldownTable) >= modelCooldownMaxEntries {
		return
	}
	cooldownTable[key] = modelCooldownEntry{Reason: reason, Until: until, UpdatedAt: now, Hint: hint}
}
func quotaObservationSnapshotFor(authID string) []map[string]any { return cooldownSnapshotFor(authID) }
func quotaObservationSnapshotAll() []map[string]any              { return cooldownSnapshotAll() }
func clearQuotaObservations(authID, model string) int {
	return clearModelCooldown(authID, normalizeCooldownModel(model))
}

// Call only for a newly fetched successful billing response, never stale cache.
// This operation changes no physical disabled state and clears no model quotas.
func observePositiveCredits(authID string, cr *creditsSummary) int {
	return observePositiveCreditsAt(authID, cr, cooldownNowFn())
}

func observePositiveCreditsAt(authID string, cr *creditsSummary, observedAt time.Time) int {
	if cr == nil || !(cr.TotalRemain > 0) {
		return 0
	}
	key := modelCooldownKey{AuthID: strings.TrimSpace(authID)}
	cooldownMu.Lock()
	defer cooldownMu.Unlock()
	if entry, ok := cooldownTable[key]; ok && entry.Reason == cooldownReasonSharedCredit && !observedAt.IsZero() && !observedAt.Before(entry.UpdatedAt) {
		delete(cooldownTable, key)
		return 1
	}
	return 0
}

func quotaEntryFields(key modelCooldownKey, e modelCooldownEntry, now time.Time) map[string]any {
	scope, recovery := "model", "ttl_probe_or_manual_clear"
	if e.Reason == cooldownReasonSharedCredit {
		scope, recovery = "account", "positive_credits_or_probe_or_manual_clear"
	}
	row := map[string]any{"model": key.ModelID, "reason": string(e.Reason), "scope": scope, "until": e.Until.UTC().Format(time.RFC3339), "seconds": int(e.Until.Sub(now).Round(time.Second) / time.Second), "updated_at": e.UpdatedAt.UTC().Format(time.RFC3339), "occurred_at": e.UpdatedAt.UTC().Format(time.RFC3339), "next_probe_at": e.Until.UTC().Format(time.RFC3339), "recovery": recovery, "persistent": false}
	if !e.Hint.ResetAt.IsZero() {
		row["reset_at"] = e.Hint.ResetAt.UTC().Format(time.RFC3339)
		row["reset_source"] = e.Hint.Source
	}
	return row
}

// Called from the existing dispatch guard; catalogue availability remains stable
// so expiry does not depend on a host model-catalog refresh.
func otherQuotaCooldownError(authID, model string) error {
	authID, model = strings.TrimSpace(authID), normalizeCooldownModel(model)
	now := cooldownNowFn()
	cooldownMu.Lock()
	defer cooldownMu.Unlock()
	for _, key := range []modelCooldownKey{{AuthID: authID}, {AuthID: authID, ModelID: model}} {
		e, ok := cooldownTable[key]
		if !ok {
			continue
		}
		if !e.Until.After(now) {
			delete(cooldownTable, key)
			continue
		}
		switch e.Reason {
		case cooldownReasonSharedCredit, cooldownReasonModelCredit, cooldownReasonModelQuota:
			return &quotaObservationError{reason: e.Reason, hint: e.Hint}
		}
	}
	return nil
}

func isQuotaCooldownReason(r cooldownReason) bool {
	return r == cooldownReasonDailyQuota || r == cooldownReasonSharedCredit || r == cooldownReasonModelCredit || r == cooldownReasonModelQuota
}

// Preserve reset evidence inside typed errors while exposing no upstream body.
func attachQuotaHeaders(err error, headers http.Header) error {
	hint := parseQuotaHint(headers, "", cooldownNowFn())
	if hint.ResetAt.IsZero() {
		return err
	}
	var daily *dailyQuotaError
	if errors.As(err, &daily) {
		if hint.ResetAt.After(daily.hint.ResetAt) {
			daily.hint = hint
		}
		return err
	}
	var quota *quotaObservationError
	if errors.As(err, &quota) {
		if hint.ResetAt.After(quota.hint.ResetAt) {
			quota.hint = hint
		}
	}
	var queued *queueAdmissionError
	if errors.As(err, &queued) && hint.ResetAt.After(queued.hint.ResetAt) {
		queued.hint = hint
	}
	return err
}

// Safe status-less quota error conversion must happen before host status wrapping.
func quotaErrorForClient(err error) error {
	var quota *quotaObservationError
	if errors.As(err, &quota) {
		return quota
	}
	return err
}

// Generic business code 110 is deliberately NOT mapped to quota or success.
// The daily predicate still needs an explicit 403 from the gateway envelope.
func businessEnvelopeStatus(body string) int {
	var m map[string]any
	if json.Unmarshal([]byte(body), &m) != nil {
		return 0
	}
	if _, content := m["choices"]; content {
		return 0
	}
	if n, ok := codeFromAny(m["code"]); ok {
		if n == 110 {
			if isDailyCountLimit(403, body) {
				return 502
			}
			return 403
		}
		if n == 10605 {
			return 403
		}
	}
	if e, ok := m["error"].(map[string]any); ok {
		if n, ok := codeFromAny(e["code"]); ok && (n == 110 || n == 10605) {
			if n == 110 && isDailyCountLimit(403, body) {
				return 502
			}
			return 403
		}
	}
	return 0
}
