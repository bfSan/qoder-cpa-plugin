package main

import (
	"regexp"
	"strings"
)

const dailyQuotaMarker = "DAILY_COUNT_LIMIT"

var dailyQuotaCodePattern = regexp.MustCompile(`"code"\s*:\s*(?:"110"|110)\s*[,}]`)

// isDailyCountLimit deliberately requires both the business code and message.
// A bare 403/110 is not evidence of quota exhaustion. Nested JSON strings are
// flattened before matching, including numeric codes and whitespace variants.
func isDailyCountLimit(status int, body string) bool {
	if strings.HasPrefix(body, "qoder quota exceeded:") && strings.Contains(body, dailyQuotaMarker) {
		return true // our typed error after folding, not a raw upstream status
	}
	if status != 403 {
		return false
	}
	flat := strings.ReplaceAll(body, `\`, "")
	return dailyQuotaCodePattern.MatchString(flat) &&
		strings.Contains(strings.ToLower(flat), "billing daily count exceeded")
}

// No StatusCode/Unwrap: the upstream auth-looking status must not escape the
// client boundary or cool every model of the credential. Like queue errors,
// the deployed host requires the lifecycle marker to leave cooldown ownership
// with this plugin. It is an ABI compatibility marker, not a real stream EOF.
// Quota wording makes DSH classify this as QUOTA (not AUTH or quick retry).
type dailyQuotaError struct{ hint quotaHint }

func (*dailyQuotaError) Error() string {
	return "qoder quota exceeded: 上游每日调用次数已达上限，该账号的此模型暂时冷却；并非 API 密钥失效。稍后重试或使用其他账号/模型 (DAILY_COUNT_LIMIT; unexpected EOF)"
}

func newDailyQuotaError(status int, body string) *dailyQuotaError {
	if isDailyCountLimit(status, body) {
		return &dailyQuotaError{hint: parseQuotaHint(nil, body, cooldownNowFn())}
	}
	return nil
}

// dailyQuotaCooldownError is an execution-time safety net: the host may retain
// a previously registered model catalog. Never call upstream during this pair's
// daily quota cooldown, even before model.for_auth refreshes. Reads don't extend
// the finite TTL, and no assumption is made about the upstream reset timezone.
func dailyQuotaCooldownError(authID, model string) error {
	if err := otherQuotaCooldownError(authID, model); err != nil {
		return err
	}
	key := modelCooldownKey{AuthID: strings.TrimSpace(authID), ModelID: normalizeCooldownModel(model)}
	now := cooldownNowFn()
	cooldownMu.Lock()
	defer cooldownMu.Unlock()
	entry, ok := cooldownTable[key]
	if !ok || entry.Reason != cooldownReasonDailyQuota {
		return nil
	}
	if !entry.Until.After(now) {
		delete(cooldownTable, key)
		return nil
	}
	return &dailyQuotaError{}
}
