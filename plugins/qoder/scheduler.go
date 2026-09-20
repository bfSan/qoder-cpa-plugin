// scheduler.go implements the CPA scheduler.pick capability for qoder.
//
// Routing uses the panel-selected active account (region from that card's
// domain). When the selection is exhausted/disabled/missing, randomly switch
// to another non-exhausted qoder candidate. Non-qoder candidates are
// always deferred so the built-in scheduler handles them.
package main

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Legacy config values kept for configure() compatibility; pick always uses
// panel active-auth selection now (not credit-max ranking).
const (
	schedulerModeOff     = "off"
	schedulerModeCredits = "credits"
)

var (
	schedulerMode   = schedulerModeOff
	schedulerModeMu sync.RWMutex
)

// setSchedulerMode is a test helper that returns a restore func.
func setSchedulerMode(mode string) func() {
	schedulerModeMu.Lock()
	old := schedulerMode
	schedulerMode = mode
	schedulerModeMu.Unlock()
	return func() {
		schedulerModeMu.Lock()
		schedulerMode = old
		schedulerModeMu.Unlock()
	}
}

func loadedSchedulerMode() string {
	schedulerModeMu.RLock()
	defer schedulerModeMu.RUnlock()
	return schedulerMode
}

// handleSchedulerPick selects a qoderwork auth candidate based on the
// panel-selected active account. Non-qoderwork candidates are always deferred
// (Handled: false) so the built-in scheduler handles them.
//
// scheduler_mode:
//   - "off"     -> defer normal routing to built-in. The plugin still handles
//     one narrow case: a qoder auth is cooling for the requested model and
//     another qoder auth is available. This keeps model-scoped cooldowns
//     effective without taking over provider selection.
//   - "credits" -> plugin picks via panel-selected active account (sticky, with
//     fallback when that account becomes exhausted/disabled).
//
// Default is off (see schedulerMode init). Users opting into the plugin's
// routing should set scheduler_mode: credits in plugin config.
func handleSchedulerPick(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}

	// Collect qoderwork candidates only.
	var wbCandidates []pluginapi.SchedulerAuthCandidate
	for _, c := range req.Candidates {
		if c.Provider != providerName {
			continue
		}
		if candidateDisabled(c) {
			continue
		}
		wbCandidates = append(wbCandidates, c)
	}
	if len(wbCandidates) == 0 {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	// Per-(auth, model) cooldown. Qoder failures are frequently model-scoped:
	// one auth's qwen model can fail while the same auth's other models are
	// healthy. Filter only the cooling pair and leave the routing policy
	// otherwise untouched.
	reqModel := requestModelForCooldown(req.Model, req.Options.Metadata)
	var availabilityCandidates []pluginapi.SchedulerAuthCandidate
	var coolingCandidates []pluginapi.SchedulerAuthCandidate
	for _, c := range wbCandidates {
		if reqModel != "" && modelIsCooling(c.ID, reqModel) {
			coolingCandidates = append(coolingCandidates, c)
			continue
		}
		availabilityCandidates = append(availabilityCandidates, c)
	}
	if len(coolingCandidates) > 0 && loadedSchedulerMode() != schedulerModeCredits {
		// In off mode the host normally owns routing. Intervene only for a
		// single-provider qoder route where filtering a cooling pair leaves a
		// healthy qoder candidate. If every qoder candidate is cooling, defer
		// so the host produces its normal model_cooldown response.
		if !strings.EqualFold(strings.TrimSpace(req.Provider), providerName) ||
			len(availabilityCandidates) == 0 {
			return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
		}
	}
	if loadedSchedulerMode() == schedulerModeCredits &&
		len(availabilityCandidates) == 0 && len(coolingCandidates) > 0 {
		// credits mode is explicitly plugin-owned. Keep the previous
		// fail-open behavior so a stale cooldown cannot brick all routing;
		// the failure path refreshes the entry if the upstream is still down.
		availabilityCandidates = coolingCandidates
	}
	wbCandidates = availabilityCandidates

	// Build thin view for active-auth picker.
	cands := make([]activeAuthCandidate, 0, len(wbCandidates))
	for _, c := range wbCandidates {
		_, exhausted := cachedCreditsScore(c.ID)
		cands = append(cands, activeAuthCandidate{
			ID:        c.ID,
			Disabled:  false, // already filtered
			Exhausted: exhausted,
		})
	}
	picked := pickActiveAuth(cands)
	if picked == "" {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	return okEnvelope(pluginapi.SchedulerPickResponse{
		AuthID:  picked,
		Handled: true,
	})
}

// candidateDisabled reports host-disabled auth from Status/metadata.
func candidateDisabled(c pluginapi.SchedulerAuthCandidate) bool {
	st := strings.ToLower(strings.TrimSpace(c.Status))
	if st == "disabled" {
		return true
	}
	if c.Metadata != nil {
		if v, ok := c.Metadata["disabled"]; ok {
			switch t := v.(type) {
			case bool:
				return t
			case string:
				return strings.EqualFold(strings.TrimSpace(t), "true")
			}
		}
	}
	return false
}

// cachedCreditsScore returns (remain, exhausted) from accountCache.
// remain is -1 when unknown; exhausted uses isCreditsExhausted.
// Key is auth.ID (same as SchedulerAuthCandidate.ID and activeAuthID).
func cachedCreditsScore(authID string) (int64, bool) {
	v, ok := accountCache.Load(authID)
	if !ok {
		return -1, false
	}
	entry, ok := v.(*accountCacheEntry)
	if !ok || entry.credits == nil {
		return -1, false
	}
	return entry.credits.TotalRemain, isCreditsExhausted(entry.credits)
}
