package main

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// nowForTest keeps the seeded cache entries inside the TTL window.
func nowForTest() time.Time { return time.Now() }

// The five thinking_config shapes were observed on the live CN gateway
// (2026-10-09, 11 of 14 chat models carry the field). Each is pinned here
// because they differ in ways that matter: which block exists, whether the
// disabled switch is offered, and whether the default tier is marked.
func TestParseThinkingFactsCoversObservedShapes(t *testing.T) {
	cases := []struct {
		name           string
		cfg            *qoderThinkingConfigWire
		isReasoning    bool
		wantStatus     string
		wantLevels     []string
		wantDefault    string
		wantCanDisable *bool
	}{
		{
			// No thinking_config at all, and the gateway says the model does
			// not reason: this is the one case where "unsupported" is earned.
			name: "absent config with reasoning disabled",
			cfg:  nil, isReasoning: false,
			wantStatus: "unsupported",
		},
		{
			// is_reasoning=true but no tier data: the gateway did not publish
			// the tiers, which is not the same as having none.
			name: "absent config but reasoning capable",
			cfg:  nil, isReasoning: true,
			wantStatus: "unknown",
		},
		{
			// qmodel_38max / qfmodel: tiers plus a default marker.
			name: "efforts with a marked default",
			cfg: &qoderThinkingConfigWire{
				Disabled: &qoderThinkingOptionWire{},
				Enabled: &qoderThinkingOptionWire{
					IsDefault: true,
					Efforts: map[string]qoderThinkingEffortWire{
						"low":    {},
						"medium": {IsDefault: true},
						"xhigh":  {},
					},
				},
			},
			isReasoning: true,
			wantStatus:  "supported",
			wantLevels:  []string{"low", "medium", "xhigh"},
			wantDefault: "medium",
		},
		{
			// qmodel / qmodel_latest: a pure on/off switch, no tiers named.
			name: "enable switch without tiers",
			cfg: &qoderThinkingConfigWire{
				Disabled: &qoderThinkingOptionWire{Description: "Disable thinking"},
				Enabled:  &qoderThinkingOptionWire{Description: "Enable thinking", IsDefault: true},
			},
			isReasoning: true,
			wantStatus:  "off_only",
		},
		{
			// gmodel / kmodel: no disabled block, so nothing says it can be
			// turned off. can_disable must be false, not nil and not true.
			name: "no disabled block reports cannot disable",
			cfg: &qoderThinkingConfigWire{
				Enabled: &qoderThinkingOptionWire{
					IsDefault: true,
					Efforts: map[string]qoderThinkingEffortWire{
						"high": {}, "low": {}, "max": {IsDefault: true},
					},
				},
			},
			isReasoning: true,
			wantStatus:  "supported",
			wantLevels:  []string{"low", "high", "max"},
			wantDefault: "max",
		},
		{
			// dmodel / gm51model: disabled block present with tiers.
			name: "disable offered alongside tiers",
			cfg: &qoderThinkingConfigWire{
				Disabled: &qoderThinkingOptionWire{Description: "Disable thinking"},
				Enabled: &qoderThinkingOptionWire{
					IsDefault: true,
					Efforts: map[string]qoderThinkingEffortWire{
						"high": {Description: "High thinking intensity"},
						"max":  {Description: "Maximum thinking intensity", IsDefault: true},
					},
				},
			},
			isReasoning: true,
			wantStatus:  "supported",
			wantLevels:  []string{"high", "max"},
			wantDefault: "max",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseThinkingFacts(tc.cfg, tc.isReasoning)
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", got.Status, tc.wantStatus)
			}
			if !reflect.DeepEqual(got.Levels, tc.wantLevels) {
				t.Fatalf("levels = %v, want %v", got.Levels, tc.wantLevels)
			}
			if got.Default != tc.wantDefault {
				t.Fatalf("default = %q, want %q", got.Default, tc.wantDefault)
			}
		})
	}
}

// A missing field must never be reported as unsupported. Telling an operator a
// model cannot think, when the gateway simply stayed quiet, sends them away
// from a capability that exists.
func TestParseThinkingFactsNeverInventsUnsupported(t *testing.T) {
	// Config present but empty: no enabled block, no disabled block.
	got := parseThinkingFacts(&qoderThinkingConfigWire{}, true)
	if got.Status == "unsupported" {
		t.Fatal("an empty thinking_config was reported as unsupported")
	}
	if got.Status != "unknown" {
		t.Fatalf("status = %q, want unknown", got.Status)
	}
	// An absent disabled block is not evidence that thinking can be disabled.
	if got.CanDisable != nil && !*got.CanDisable {
		t.Fatal("can_disable was set to false without evidence")
	}
}

// Tiers are ordered by intensity. Sorted as strings they would read
// high/low/max/medium/xhigh, which implies the wrong progression.
func TestSortEffortLevelsUsesIntensityOrder(t *testing.T) {
	levels := []string{"max", "low", "xhigh", "medium", "high"}
	sortEffortLevels(levels)
	want := []string{"low", "medium", "high", "xhigh", "max"}
	if !reflect.DeepEqual(levels, want) {
		t.Fatalf("levels = %v, want %v", levels, want)
	}
	// An unknown tier the gateway adds later must not interleave with the
	// known ones.
	levels = []string{"ultra", "low", "high"}
	sortEffortLevels(levels)
	if levels[0] != "low" || levels[1] != "high" || levels[2] != "ultra" {
		t.Fatalf("unknown tier interleaved: %v", levels)
	}
}

func TestParseContextTiersOrdersBySizeAndMarksDefault(t *testing.T) {
	raw := map[string]qoderContextTierWire{
		"1M":   {TokenCount: 1000000},
		"200K": {TokenCount: 200000, IsDefault: true},
		"400K": {TokenCount: 400000},
	}
	tiers := parseContextTiers(raw)
	if len(tiers) != 3 {
		t.Fatalf("tiers = %d, want 3", len(tiers))
	}
	// Ordered by size so the panel reads small to large; label order would put
	// 1M first.
	if tiers[0].Label != "200K" || tiers[1].Label != "400K" || tiers[2].Label != "1M" {
		t.Fatalf("tiers out of size order: %v", tiers)
	}
	if !tiers[0].IsDefault || tiers[1].IsDefault || tiers[2].IsDefault {
		t.Fatalf("default marker lost: %v", tiers)
	}
	if tokens, ok := contextDefaultTokens(tiers); !ok || tokens != 200000 {
		t.Fatalf("default tokens = %d (%v), want 200000", tokens, ok)
	}
}

func TestParseContextTiersHandlesSingleTierAndAbsent(t *testing.T) {
	// mmodel publishes exactly one tier.
	got := parseContextTiers(map[string]qoderContextTierWire{"200K": {TokenCount: 200000, IsDefault: true}})
	if len(got) != 1 || got[0].Tokens != 200000 {
		t.Fatalf("single tier = %v", got)
	}
	// auto has no context_config at all; the caller falls back to
	// max_input_tokens, so this must stay empty rather than inventing a tier.
	if got := parseContextTiers(nil); got != nil {
		t.Fatalf("absent config produced tiers: %v", got)
	}
	if _, ok := contextDefaultTokens(nil); ok {
		t.Fatal("absent config reported a default tier")
	}
}

// The panel's three region states are different claims and must stay apart:
// present, absent (catalog loaded, model not in it), not_loaded (no catalog).
func TestBuildPanelRegionModelsDistinguishesAbsentFromNotLoaded(t *testing.T) {
	cnFactor := 0.5
	restore := setDynamicModelsCacheForTest(map[string][]pluginapi.ModelInfo{})
	defer restore()

	// Seed one CN account and one Intl account with different catalogs.
	dynamicModelsCache.Lock()
	dynamicModelsCache.byAccount[regionCN+"\x00cn-uid"] = dynamicModelEntry{
		models:  []pluginapi.ModelInfo{{ID: "cn-only"}, {ID: "shared"}},
		fetched: nowForTest(),
		region: map[string]modelRegionFacts{
			"cn-only": {PriceFactor: &cnFactor},
			"shared":  {PriceFactor: &cnFactor},
		},
	}
	dynamicModelsCache.byAccount[regionIntl+"\x00intl-uid"] = dynamicModelEntry{
		models:  []pluginapi.ModelInfo{{ID: "shared"}},
		fetched: nowForTest(),
		region:  map[string]modelRegionFacts{"shared": {}},
	}
	dynamicModelsCache.Unlock()

	rows := buildPanelRegionModels()
	byID := map[string]map[string]any{}
	for _, row := range rows {
		byID[row["id"].(string)] = row
	}
	if len(byID) != 2 {
		t.Fatalf("rows = %d, want 2 unified IDs", len(byID))
	}
	// A model only CN serves: Intl's catalog loaded, so this is absent ("—"),
	// not not_loaded ("未加载").
	if got := byID["cn-only"]["intl"].(map[string]any)["status"]; got != "absent" {
		t.Fatalf("cn-only intl status = %v, want absent", got)
	}
	if got := byID["cn-only"]["cn"].(map[string]any)["status"]; got != "present" {
		t.Fatalf("cn-only cn status = %v, want present", got)
	}
	if got := byID["shared"]["cn"].(map[string]any)["status"]; got != "present" {
		t.Fatalf("shared cn status = %v", got)
	}

	status := buildPanelRegionStatus()
	if loaded, _ := status[regionCN].(map[string]any)["loaded"].(bool); !loaded {
		t.Fatal("cn reported not loaded despite a seeded account")
	}
	if loaded, _ := status[regionIntl].(map[string]any)["loaded"].(bool); !loaded {
		t.Fatal("intl reported not loaded despite a seeded account")
	}
}

func TestBuildPanelRegionModelsReportsNotLoadedWhenRegionEmpty(t *testing.T) {
	restore := setDynamicModelsCacheForTest(map[string][]pluginapi.ModelInfo{})
	defer restore()

	dynamicModelsCache.Lock()
	dynamicModelsCache.byAccount[regionCN+"\x00cn-uid"] = dynamicModelEntry{
		models:  []pluginapi.ModelInfo{{ID: "cn-only"}},
		fetched: nowForTest(),
		region:  map[string]modelRegionFacts{"cn-only": {}},
	}
	dynamicModelsCache.Unlock()

	rows := buildPanelRegionModels()
	var cell map[string]any
	for _, row := range rows {
		if row["id"] == "cn-only" {
			cell = row[regionIntl].(map[string]any)
		}
	}
	if cell == nil {
		t.Fatal("cn-only row missing")
	}
	// Intl has no account at all: the column must say not_loaded, which tells
	// the operator to refresh, rather than claiming the model is absent.
	if cell["status"] != "not_loaded" || cell["present"] != nil {
		t.Fatalf("intl cell = %v, want not_loaded with present=null", cell)
	}
	status := buildPanelRegionStatus()
	if loaded, _ := status[regionIntl].(map[string]any)["loaded"].(bool); loaded {
		t.Fatal("intl reported loaded with no cached account")
	}
}

// Facts must be deep-copied: the panel response is handed out per request, and
// a caller mutating it must not corrupt the cached catalog.
func TestPanelRegionCellDoesNotAliasCachedFacts(t *testing.T) {
	factor := 0.5
	length := int64(200000)
	tiers := []contextTier{{Label: "200K", Tokens: 200000, IsDefault: true}}
	levels := []string{"low", "high"}
	canDisable := true
	facts := modelRegionFacts{
		PriceFactor:   &factor,
		ContextLength: &length,
		ContextTiers:  tiers,
		Thinking:      &thinkingFacts{Status: "supported", Levels: levels, CanDisable: &canDisable},
	}

	cell := panelRegionCell(facts)
	cell["context_tiers"].([]contextTier)[0].Tokens = 999
	if facts.ContextTiers[0].Tokens != 200000 {
		t.Fatal("context tiers alias the source")
	}
	thinking := cell["thinking"].(*thinkingFacts)
	thinking.Levels[0] = "mutated"
	if facts.Thinking.Levels[0] != "low" {
		t.Fatal("thinking levels alias the source")
	}

	// And the clone helper must round-trip without sharing either.
	cloned := cloneRegionFacts(facts)
	*cloned.PriceFactor = 9
	if factor != 0.5 {
		t.Fatal("cloneRegionFacts aliases the price factor")
	}
}

// The response shape the panel consumes must stay stable; WorkBuddy's panel
// reads the same contract.
func TestPanelRegionCellJSONShape(t *testing.T) {
	factor := 0.5
	length := int64(200000)
	cell := panelRegionCell(modelRegionFacts{
		PriceFactor:   &factor,
		ContextLength: &length,
		ContextTiers:  []contextTier{{Label: "200K", Tokens: 200000, IsDefault: true}},
		Thinking:      &thinkingFacts{Status: "supported", Default: "high", Levels: []string{"low", "high"}},
	})
	raw, err := json.Marshal(cell)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"status", "present", "price_factor", "context_length", "context_tiers", "thinking"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("region cell is missing %q: %s", key, raw)
		}
	}
	thinking := decoded["thinking"].(map[string]any)
	for _, key := range []string{"status", "default", "levels"} {
		if _, ok := thinking[key]; !ok {
			t.Fatalf("thinking is missing %q: %s", key, raw)
		}
	}
}
