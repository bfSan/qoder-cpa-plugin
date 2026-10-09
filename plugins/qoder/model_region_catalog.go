// model_region_catalog.go builds the panel's CN / Intl comparison view.
//
// The panel used to show one merged model list, which hides a real difference:
// the two Qoder gateways advertise different chat keys and different tiers for
// the same model ID. A single list cannot say "CN offers this, Intl does not",
// and it silently presented one region's price factor and context tiers as if
// they were universal.
//
// The data comes from the per-account discovery cache, which is already keyed by
// region (see accountCacheKey), so the two columns can never borrow each
// other's facts. Nothing here calls upstream: a region with no cached account is
// reported as not_loaded, which is what tells the operator to press refresh.
//
// This is deliberately the same JSON contract WorkBuddy's panel consumes, so the
// two plugins' panels stay alike:
//
//	{"id": ..., "cn": {...}, "intl": {...}}
//
// and each region cell is
//
//	{"status":"present|absent|not_loaded","present":bool|null,
//	 "price_factor":..., "context_length":..., "context_tiers":[...],
//	 "thinking":{"status":...,"default":...,"can_disable":...,"levels":[...]}}
//
// present / absent / not_loaded are three distinct claims and are kept apart:
// "this region serves the model", "this region's catalog loaded and does not
// contain it", and "this region has no loaded catalog at all". Collapsing the
// last two would report a working region as missing a model.
package main

import (
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// panelRegionCell renders one model's facts for one region.
func panelRegionCell(facts modelRegionFacts) map[string]any {
	cell := map[string]any{
		"status":  "present",
		"present": true,
	}
	if facts.PriceFactor != nil {
		cell["price_factor"] = *facts.PriceFactor
	} else {
		cell["price_factor"] = nil
	}
	if facts.ContextLength != nil {
		cell["context_length"] = *facts.ContextLength
	} else {
		cell["context_length"] = nil
	}
	// Always emit the array so the panel can read .length unconditionally; a
	// missing key and an empty list would otherwise need separate handling.
	tiers := cloneContextTiers(facts.ContextTiers)
	if tiers == nil {
		tiers = []contextTier{}
	}
	cell["context_tiers"] = tiers
	if facts.Thinking != nil {
		// Deep copy: the response is handed out per request, so returning the
		// cached pointer would let one caller's mutation corrupt the catalog
		// every later request reads.
		cell["thinking"] = cloneThinkingFacts(facts.Thinking)
	} else {
		// No facts recorded for this model. Say unknown rather than fabricating
		// "unsupported": the cache was written before the field existed, or the
		// model was added through the overlay and never seen upstream.
		cell["thinking"] = &thinkingFacts{Status: "unknown"}
	}
	return cell
}

// missingRegionCell states why a region has no facts for a model. loaded is per
// region, not per model: one region serving the model says nothing about
// whether the other region's catalog even exists.
func missingRegionCell(regionLoaded bool) map[string]any {
	if regionLoaded {
		return map[string]any{"status": "absent", "present": false}
	}
	return map[string]any{"status": "not_loaded", "present": nil}
}

// buildPanelRegionModels merges every region's cached catalog into one row per
// model ID. A model served by only one region appears once, with the other
// column marked absent or not_loaded.
func buildPanelRegionModels() []map[string]any {
	regions := []string{regionCN, regionIntl}
	loaded := map[string]bool{}
	merged := map[string]map[string]modelRegionFacts{}

	for _, region := range regions {
		for _, key := range cachedRegionKeysByRegion(region) {
			facts, ok := cachedRegionFactsFor(key)
			if !ok {
				continue
			}
			loaded[region] = true
			for id, fact := range facts {
				id = strings.TrimSpace(id)
				if id == "" {
					continue
				}
				byRegion := merged[id]
				if byRegion == nil {
					byRegion = map[string]modelRegionFacts{}
					merged[id] = byRegion
				}
				// First writer wins for a given region. cachedRegionKeysByRegion
				// is sorted, so this is deterministic when two accounts in the
				// same region report the same model.
				if _, seen := byRegion[region]; !seen {
					byRegion[region] = cloneRegionFacts(fact)
				}
			}
		}
	}

	ids := make([]string, 0, len(merged))
	for id := range merged {
		ids = append(ids, id)
	}
	// Sorted so the rows do not reshuffle between refreshes: the cache is a map
	// and its iteration order is not stable.
	sort.Strings(ids)

	// Build the name index once: looking each ID up by scanning the whole
	// catalog would be rows x models on every panel load.
	names := make(map[string]string, len(ids))
	for _, m := range cachedModelsForDisplayName() {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		if _, seen := names[id]; seen {
			continue
		}
		if name := strings.TrimSpace(m.Name); name != "" {
			names[id] = name
		} else if display := strings.TrimSpace(m.DisplayName); display != "" {
			names[id] = display
		}
	}

	rows := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		row := map[string]any{"id": id}
		if name := names[id]; name != "" {
			row["name"] = name
		}
		byRegion := merged[id]
		for _, region := range regions {
			if fact, ok := byRegion[region]; ok {
				row[region] = panelRegionCell(fact)
				continue
			}
			row[region] = missingRegionCell(loaded[region])
		}
		rows = append(rows, row)
	}
	return rows
}

// cachedModelsForDisplayName exposes the union catalog for name lookup without
// exporting the cache itself.
func cachedModelsForDisplayName() []pluginapi.ModelInfo {
	models, _ := cachedDynamicModelsAll()
	return models
}

// buildPanelRegionStatus reports whether each region has a usable catalog. The
// panel uses this to explain a column of not_loaded cells, and to avoid saying
// "no models" when the truth is "not fetched yet".
func buildPanelRegionStatus() map[string]any {
	status := make(map[string]any, 2)
	for _, region := range []string{regionCN, regionIntl} {
		keys := cachedRegionKeysByRegion(region)
		usable := false
		models := 0
		for _, key := range keys {
			if _, ok := cachedRegionFactsFor(key); !ok {
				continue
			}
			usable = true
			if cached, ok := cachedDynamicModelsFor(key); ok {
				models += len(cached)
			}
		}
		status[region] = map[string]any{
			"loaded": usable,
			"status": regionStatusLabel(usable),
			"models": models,
		}
	}
	return status
}

func regionStatusLabel(loaded bool) string {
	if loaded {
		return "ready"
	}
	return "not_loaded"
}
