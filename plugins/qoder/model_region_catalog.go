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
	"reflect"
	"sort"
	"strings"
	"time"

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
	if facts.OriginalPriceFactor != nil {
		cell["original_price_factor"] = *facts.OriginalPriceFactor
	} else {
		cell["original_price_factor"] = nil
	}
	cell["is_free"] = facts.IsFree
	if facts.Promotion != nil {
		cell["promotion"] = cloneQoderPromotion(facts.Promotion)
	} else {
		cell["promotion"] = nil
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
	cell["context_status"] = facts.ContextStatus
	cell["context_override"] = facts.ContextOverride
	cell["context_requested"] = facts.ContextRequested
	cell["max_input_tokens"] = facts.MaxInputTokens
	if tokens, ok := contextDefaultTokens(facts.ContextTiers); ok {
		cell["context_default"] = tokens
	} else {
		cell["context_default"] = nil
	}
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

// buildModelPromotionsQuery exposes promotion metadata separately from the
// large model catalog. Qoder keeps facts per account+region in the in-memory
// discovery cache; this function de-duplicates identical model promotions and
// preserves the gateway's price_factor as the current authoritative rate.
func buildModelPromotionsQuery() map[string]any {
	promotions := map[string][]map[string]any{regionCN: {}, regionIntl: {}}
	status := map[string]any{}
	now := time.Now().UTC()
	for _, region := range []string{regionCN, regionIntl} {
		seen := make(map[string]bool)
		for _, key := range cachedRegionKeysByRegion(region) {
			// Promotions are display metadata, not a routing decision: an
			// expired discovery TTL means "refresh due", never "the campaign
			// vanished". Use the TTL-free accessor so a restart or a stale
			// account does not blank the activity panel while the facts are
			// still on disk.
			facts, ok := lastGoodRegionFactsFor(key)
			if !ok {
				continue
			}
			for modelID, fact := range facts {
				// A model the gateway marks free (price_factor 0) is an active
				// offer even when it carries no promotion object. Dropping it
				// here made "DeepSeek free" invisible in the activity panel
				// while the model itself showed a 0 multiplier.
				free := fact.IsFree || (fact.PriceFactor != nil && *fact.PriceFactor == 0)
				if fact.Promotion == nil && !free {
					continue
				}
				if seen[modelID] {
					continue
				}
				seen[modelID] = true
				entry := map[string]any{
					"model_id":  modelID,
					"promotion": cloneQoderPromotion(fact.Promotion),
					"is_free":   free,
				}
				if fact.Promotion == nil {
					entry["promotion"] = map[string]any{"active": true, "badge_zh": "免费", "badge_en": "Free"}
				}
				if fact.PriceFactor != nil {
					entry["price_factor"] = *fact.PriceFactor
				}
				if fact.OriginalPriceFactor != nil {
					entry["original_price_factor"] = *fact.OriginalPriceFactor
				} else if fact.Promotion != nil && fact.Promotion.BeforePromotionPriceFactor != nil {
					// A free model has no promotion object, so this branch must
					// be nil-guarded; an unguarded read panics the whole query.
					entry["original_price_factor"] = *fact.Promotion.BeforePromotionPriceFactor
				}
				promotions[region] = append(promotions[region], entry)
			}
		}
		sort.SliceStable(promotions[region], func(i, j int) bool {
			return promotions[region][i]["model_id"].(string) < promotions[region][j]["model_id"].(string)
		})
		if len(promotions[region]) == 0 {
			status[region] = "not_loaded"
		} else {
			status[region] = "loaded"
		}
	}
	return map[string]any{
		"promotions": promotions,
		"status":     status,
		"clock": map[string]any{
			"unix":   now.Unix(),
			"minute": now.Hour()*60 + now.Minute(),
			"zone":   "UTC",
		},
	}
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
	variants := map[string]map[string][]map[string]any{}

	for _, region := range regions {
		for _, key := range cachedRegionKeysByRegion(region) {
			facts, ok := lastGoodPanelFactsFor(key)
			if !ok {
				continue
			}
			_, fresh := cachedRegionFactsFor(key)
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
				effective := effectiveContextFacts(id, fact)
				if variants[id] == nil {
					variants[id] = map[string][]map[string]any{}
				}
				variant := panelRegionCell(effective)
				variant["catalog_state"] = "last_good"
				if fresh {
					variant["catalog_state"] = "ready"
				}
				variants[id][region] = append(variants[id][region], variant)
				if previous, seen := byRegion[region]; seen {
					byRegion[region] = conservativeRegionFacts(previous, effective)
				} else {
					byRegion[region] = effective
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
				cell := panelRegionCell(fact)
				cell["account_variants"] = variants[id][region]
				cell["account_count"] = len(variants[id][region])
				cell["context_options"] = supportedContextTiersFor(id)
				row[region] = cell
				continue
			}
			row[region] = missingRegionCell(loaded[region])
		}

		row["context_options"] = supportedContextTiersFor(id)
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
		stale := 0
		for _, key := range keys {
			if _, ok := lastGoodRegionFactsFor(key); !ok {
				continue
			}
			usable = true
			dynamicModelsCache.RLock()
			entry := dynamicModelsCache.byAccount[key]
			models += len(entry.models)
			if time.Since(entry.fetched) >= dynamicModelsCacheTTL {
				stale++
			}
			dynamicModelsCache.RUnlock()
		}
		label := regionStatusLabel(usable)
		if stale > 0 {
			label = "last_good"
		}
		status[region] = map[string]any{
			"loaded":         usable,
			"status":         label,
			"stale_accounts": stale,
			"models":         models,
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

// A region cell is a conservative summary, never an arbitrary account's claim.
// Full anonymous account variants accompany it so differences remain inspectable.
func conservativeRegionFacts(a, b modelRegionFacts) modelRegionFacts {
	out := cloneRegionFacts(a)
	if a.ContextLength == nil || b.ContextLength == nil {
		out.ContextLength = nil
	} else if *b.ContextLength < *a.ContextLength {
		value := *b.ContextLength
		out.ContextLength = &value
	}
	if !reflect.DeepEqual(a.ContextLength, b.ContextLength) || !reflect.DeepEqual(a.ContextTiers, b.ContextTiers) || a.ContextStatus != b.ContextStatus {
		out.ContextStatus = "account_variance"
	}
	out.ContextOverride = a.ContextOverride && b.ContextOverride
	// Show only common tiers in the summary; variants preserve the full reports.
	var common []contextTier
	for _, left := range a.ContextTiers {
		for _, right := range b.ContextTiers {
			if left.Tokens == right.Tokens {
				left.IsDefault = left.IsDefault && right.IsDefault
				common = append(common, left)
				break
			}
		}
	}
	out.ContextTiers = common
	if !reflect.DeepEqual(a.Thinking, b.Thinking) {
		out.Thinking = &thinkingFacts{Status: "account_variance"}
	}
	if !reflect.DeepEqual(a.PriceFactor, b.PriceFactor) {
		out.PriceFactor = nil
	}
	return out
}

// Presence comes from this account's own model list. Missing optional facts
// remain unknown and participate in the conservative region summary.
func lastGoodPanelFactsFor(key string) (map[string]modelRegionFacts, bool) {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	entry, ok := dynamicModelsCache.byAccount[key]
	if !ok || len(entry.models) == 0 {
		return nil, false
	}
	facts := make(map[string]modelRegionFacts, len(entry.models))
	for _, model := range entry.models {
		facts[model.ID] = entry.region[model.ID]
	}
	return facts, true
}
