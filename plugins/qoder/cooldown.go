// cooldown.go implements per-(account, model) throttling for Qoder.
//
// An upstream failure on one model says nothing about the rest of the
// account's catalog. Cooling the whole credential would turn a single
// degraded model (for example an empty_stream on one upstream model) into
// an auth-wide outage, which is especially harmful when only one Qoder auth
// is configured. Keep throttling scoped to the failed (auth, model) pair and
// let the scheduler route other models through the same auth.
//
// State is process-local: it survives config reloads but not a CPA restart.
package main

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	// Default durations. Rate limits back off longer than protocol-level
	// failures because the upstream explicitly asked us to slow down.
	modelCooldownRateLimit = 300 * time.Second
	modelCooldownUnknown   = 60 * time.Second

	// modelCooldownMaxEntries bounds the table so a hostile or buggy upstream
	// cannot grow it without limit.
	modelCooldownMaxEntries = 4096
)

type cooldownReason string

const (
	cooldownReasonRateLimit cooldownReason = "rate_limit"
	cooldownReasonUnknown   cooldownReason = "upstream_error"
)

type modelCooldownKey struct {
	AuthID  string
	ModelID string
}

type modelCooldownEntry struct {
	Reason    cooldownReason
	Until     time.Time
	UpdatedAt time.Time
}

// requestModelForCooldown resolves the model key used for per-(auth, model)
// cooldown state.
//
// CPA performs model aliasing/rewriting between auth selection and plugin
// execution, so req.Model at execution time may already be the upstream model
// (for example "dfmodel") while the scheduler saw the client-facing route model
// (for example "deepseek-v4.1-flash"). Keep cooldowns keyed by the routing
// model so the scheduler can skip the same (auth, model) pair that failed.
//
// requested_model is set by CPA's execution handlers and survives the
// model-alias rewrite. Metadata values are checked first; req.Model remains
// the fallback for paths that do not populate metadata.
func requestModelForCooldown(reqModel string, metadata map[string]any) string {
	for _, key := range []string{
		"auth_selection_model",
		"requested_model",
	} {
		if model := metadataModelName(metadata, key); model != "" {
			return normalizeCooldownModel(model)
		}
	}
	return normalizeCooldownModel(reqModel)
}

func metadataModelName(metadata map[string]any, key string) string {
	if len(metadata) == 0 || strings.TrimSpace(key) == "" {
		return ""
	}
	raw, ok := metadata[key]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func normalizeCooldownModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	return strings.TrimSpace(stripProviderPrefix(model))
}

var (
	cooldownMu    sync.Mutex
	cooldownTable = make(map[modelCooldownKey]modelCooldownEntry)
	cooldownNowFn = time.Now
)

// markModelCooldown records a throttled (account, model) pair. An empty model
// is ignored: without a model ID the entry would freeze the account, which is
// exactly what this table exists to avoid.
func markModelCooldown(authID, model string, reason cooldownReason) {
	authID = strings.TrimSpace(authID)
	model = strings.TrimSpace(model)
	if authID == "" || model == "" {
		return
	}
	ttl := modelCooldownUnknown
	if reason == cooldownReasonRateLimit {
		ttl = modelCooldownRateLimit
	}
	now := cooldownNowFn()
	cooldownMu.Lock()
	defer cooldownMu.Unlock()
	cooldownSweepLocked(now)
	if len(cooldownTable) >= modelCooldownMaxEntries {
		oldestKey := modelCooldownKey{}
		oldestUntil := time.Time{}
		first := true
		for k, v := range cooldownTable {
			if first || v.Until.Before(oldestUntil) {
				oldestKey, oldestUntil, first = k, v.Until, false
			}
		}
		delete(cooldownTable, oldestKey)
	}
	cooldownTable[modelCooldownKey{AuthID: authID, ModelID: model}] = modelCooldownEntry{
		Reason:    reason,
		Until:     now.Add(ttl),
		UpdatedAt: now,
	}
}

// modelCoolingUntil reports when the pair becomes usable again. The zero time
// means the pair is not cooling down.
func modelCoolingUntil(authID, model string) time.Time {
	authID = strings.TrimSpace(authID)
	model = strings.TrimSpace(model)
	if authID == "" || model == "" {
		return time.Time{}
	}
	now := cooldownNowFn()
	key := modelCooldownKey{AuthID: authID, ModelID: model}
	cooldownMu.Lock()
	defer cooldownMu.Unlock()
	entry, ok := cooldownTable[key]
	if !ok {
		return time.Time{}
	}
	if !entry.Until.After(now) {
		delete(cooldownTable, key)
		return time.Time{}
	}
	return entry.Until
}

// modelIsCooling reports whether the pair should be skipped right now.
func modelIsCooling(authID, model string) bool {
	return !modelCoolingUntil(authID, model).IsZero()
}

// clearModelCooldown removes one pair, or every pair for the account when the
// model is empty. Returns how many entries were removed.
func clearModelCooldown(authID, model string) int {
	authID = strings.TrimSpace(authID)
	model = strings.TrimSpace(model)
	if authID == "" {
		return 0
	}
	cooldownMu.Lock()
	defer cooldownMu.Unlock()
	removed := 0
	if model != "" {
		key := modelCooldownKey{AuthID: authID, ModelID: model}
		if _, ok := cooldownTable[key]; ok {
			delete(cooldownTable, key)
			removed++
		}
		return removed
	}
	for k := range cooldownTable {
		if k.AuthID == authID {
			delete(cooldownTable, k)
			removed++
		}
	}
	return removed
}

// cooldownSnapshotFor lists the still-active pairs for one account.
func cooldownSnapshotFor(authID string) []map[string]any {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil
	}
	now := cooldownNowFn()
	cooldownMu.Lock()
	out := make([]map[string]any, 0)
	for k, v := range cooldownTable {
		if k.AuthID != authID || !v.Until.After(now) {
			continue
		}
		out = append(out, map[string]any{
			"model":      k.ModelID,
			"reason":     string(v.Reason),
			"until":      v.Until.UTC().Format(time.RFC3339),
			"seconds":    int(v.Until.Sub(now).Round(time.Second) / time.Second),
			"updated_at": v.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	cooldownMu.Unlock()
	sortCooldownSnapshot(out)
	return out
}

// cooldownSnapshotAll lists every active pair across accounts.
func cooldownSnapshotAll() []map[string]any {
	now := cooldownNowFn()
	cooldownMu.Lock()
	out := make([]map[string]any, 0)
	for k, v := range cooldownTable {
		if !v.Until.After(now) {
			continue
		}
		out = append(out, map[string]any{
			"auth_id":    k.AuthID,
			"model":      k.ModelID,
			"reason":     string(v.Reason),
			"until":      v.Until.UTC().Format(time.RFC3339),
			"seconds":    int(v.Until.Sub(now).Round(time.Second) / time.Second),
			"updated_at": v.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	cooldownMu.Unlock()
	sortCooldownSnapshotByAuth(out)
	return out
}

func sortCooldownSnapshot(rows []map[string]any) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0; j-- {
			a, _ := rows[j-1]["model"].(string)
			b, _ := rows[j]["model"].(string)
			if b >= a {
				break
			}
			rows[j-1], rows[j] = rows[j], rows[j-1]
		}
	}
}

func sortCooldownSnapshotByAuth(rows []map[string]any) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0; j-- {
			a, _ := rows[j-1]["auth_id"].(string)
			b, _ := rows[j]["auth_id"].(string)
			if b > a {
				break
			}
			if b < a {
				rows[j-1], rows[j] = rows[j], rows[j-1]
				continue
			}
			am, _ := rows[j-1]["model"].(string)
			bm, _ := rows[j]["model"].(string)
			if bm >= am {
				break
			}
			rows[j-1], rows[j] = rows[j], rows[j-1]
		}
	}
}

// cooldownSweepLocked drops expired entries. Callers must hold cooldownMu.
func cooldownSweepLocked(now time.Time) {
	for k, v := range cooldownTable {
		if !v.Until.After(now) {
			delete(cooldownTable, k)
		}
	}
}

// recordUpstreamFailure routes an upstream failure into the model-scoped
// cooldown table.
//
// Qoder's worst failure mode is not an HTTP 429: the gateway can accept the
// request and then close the SSE stream before the first payload, which the
// plugin observes as empty_stream with status 0. That must cool the specific
// model too, otherwise the only configured auth gets repeatedly selected and
// every request to that model fails. Account-level failures (hard credit
// exhaustion, invalid credentials) stay with the existing lifecycle/status
// handling.
func recordUpstreamFailure(authID, model string, status int, body string) {
	model = normalizeCooldownModel(model)
	if model == "" {
		return
	}
	if isHardCreditError(status, body) {
		return
	}
	if status == 429 || isSoftRateLimit(status, body) {
		markModelCooldown(authID, model, cooldownReasonRateLimit)
		return
	}
	// Empty stream / transport-level failures may arrive with no HTTP status
	// (direct read failure) or ride a gateway status such as 503. The body is
	// the reliable discriminator, so do not gate this on status == 0.
	if isEmptyStreamFailure(body) {
		markModelCooldown(authID, model, cooldownReasonUnknown)
	}
}

func isEmptyStreamFailure(body string) bool {
	body = strings.ToLower(strings.TrimSpace(body))
	return strings.Contains(body, "empty_stream") ||
		strings.Contains(body, "stream closed before first payload")
}

// handleCooldownList reports every active pair, or one account's pairs when
// auth_id is given. State lives in memory, so the response says so.
func handleCooldownList(req pluginapi.ManagementRequest) map[string]any {
	if id := strings.TrimSpace(req.Query.Get("auth_id")); id != "" {
		entries := cooldownSnapshotFor(id)
		return map[string]any{
			"entries":    entries,
			"count":      len(entries),
			"persistent": false,
		}
	}
	entries := cooldownSnapshotAll()
	return map[string]any{
		"entries":    entries,
		"count":      len(entries),
		"persistent": false,
	}
}

// handleCooldownClear drops throttling for one pair (auth_id + model) or for
// every pair of one account (auth_id only).
func handleCooldownClear(req pluginapi.ManagementRequest) map[string]any {
	authID := strings.TrimSpace(req.Query.Get("auth_id"))
	model := strings.TrimSpace(req.Query.Get("model"))
	if authID == "" && len(req.Body) > 0 {
		var body struct {
			AuthID  string `json:"auth_id"`
			AuthIdx string `json:"auth_index"`
			Model   string `json:"model"`
		}
		if err := json.Unmarshal(req.Body, &body); err == nil {
			authID = strings.TrimSpace(body.AuthID)
			if authID == "" {
				authID = strings.TrimSpace(body.AuthIdx)
			}
			model = strings.TrimSpace(body.Model)
		}
	}
	if authID == "" {
		return map[string]any{"success": false, "error": "auth_id is required"}
	}
	removed := clearModelCooldown(authID, model)
	return map[string]any{
		"success":    true,
		"auth_id":    authID,
		"model":      model,
		"removed":    removed,
		"persistent": false,
	}
}
