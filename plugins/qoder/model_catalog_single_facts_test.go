package main

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// A catalog-only refresh must not erase capability facts it cannot re-derive.
// Regression: persistModelCatalogFor(sa, dyn, nil, nil) wiped region_facts on
// every panel refresh, so thinking tiers and context tiers disappeared.
func TestCatalogOnlyRefreshPreservesExistingFacts(t *testing.T) {
	redirectUserConfigDir(t)
	store, err := defaultQoderModelStore()
	if err != nil {
		t.Fatal(err)
	}
	sa := &storedAuth{}
	sa.Account.UID = "uid-refresh"
	sa.Auth.Region = regionCN
	levels := []string{"low", "high"}
	original := modelCatalogCacheV1{
		SchemaVersion: modelCatalogSchemaVersion, FactsVersion: modelCatalogFactsVersion,
		IdentitySHA256: modelCatalogIdentitySHA256("cn", "uid-refresh"), Region: "cn", UID: "uid-refresh",
		Models:  []pluginapi.ModelInfo{{ID: "m1"}, {ID: "m2"}},
		Factors: map[string]float64{"m1": 0.5},
		RegionFacts: map[string]modelRegionFacts{
			"m1": {Thinking: &thinkingFacts{Status: "supported", Levels: levels}},
		},
	}
	if err := store.saveModelCatalog(original); err != nil {
		t.Fatal(err)
	}
	factors, facts := existingModelCatalogFacts(sa)
	if factors["m1"] != 0.5 || facts["m1"].Thinking == nil || len(facts["m1"].Thinking.Levels) != 2 {
		t.Fatalf("existing facts not recovered: %+v %+v", factors, facts)
	}
	persistModelCatalogFor(sa, []pluginapi.ModelInfo{{ID: "m1"}, {ID: "m2"}, {ID: "m3"}}, factors, facts)
	caches, err := store.loadModelCatalogAll()
	if err != nil || len(caches) != 1 {
		t.Fatalf("reload: %v %d", err, len(caches))
	}
	got := caches[0]
	if len(got.Models) != 3 {
		t.Fatalf("models not refreshed: %d", len(got.Models))
	}
	if got.Factors["m1"] != 0.5 || got.RegionFacts["m1"].Thinking == nil {
		t.Fatal("refresh erased capability facts")
	}
}

// The real regression: the panel refresh path itself must leave the facts that
// callModelsAPI just persisted intact.
func TestRefreshAccountModelCatalogKeepsFactsWrittenByDiscovery(t *testing.T) {
	redirectUserConfigDir(t)
	store, err := defaultQoderModelStore()
	if err != nil {
		t.Fatal(err)
	}
	sa := &storedAuth{}
	sa.Account.UID = "uid-refresh-path"
	sa.Auth.Region = regionCN
	identity := modelCatalogIdentitySHA256("cn", "uid-refresh-path")
	persisted := modelCatalogCacheV1{
		SchemaVersion: modelCatalogSchemaVersion, FactsVersion: modelCatalogFactsVersion,
		IdentitySHA256: identity, Region: "cn", UID: "uid-refresh-path",
		Models:  []pluginapi.ModelInfo{{ID: "m1"}},
		Factors: map[string]float64{"m1": 0.25},
		RegionFacts: map[string]modelRegionFacts{
			"m1": {Thinking: &thinkingFacts{Status: "supported", Levels: []string{"low", "high"}}},
		},
	}
	if err := store.saveModelCatalog(persisted); err != nil {
		t.Fatal(err)
	}
	oldCall, oldBundle, oldTrigger := callModelsAPIFn, panelHostAuthBundle, triggerHostAuthReregisterFn
	defer func() {
		callModelsAPIFn, panelHostAuthBundle, triggerHostAuthReregisterFn = oldCall, oldBundle, oldTrigger
	}()
	callModelsAPIFn = func(*storedAuth) ([]pluginapi.ModelInfo, error) {
		return []pluginapi.ModelInfo{{ID: "m1"}}, nil
	}
	panelHostAuthBundle = func(string) (*storedAuth, *hostAuthPhysical, error) {
		return sa, &hostAuthPhysical{Name: "qoder-cn-x.json", JSON: []byte(`{}`)}, nil
	}
	triggerHostAuthReregisterFn = func(*hostAuthPhysical) error { return nil }
	if got := refreshAccountModelCatalog("idx"); got != "refreshed" {
		t.Fatalf("status %q", got)
	}
	caches, err := store.loadModelCatalogAll()
	if err != nil || len(caches) != 1 {
		t.Fatalf("reload %v %d", err, len(caches))
	}
	got := caches[0]
	if got.Factors["m1"] != 0.25 || got.RegionFacts["m1"].Thinking == nil {
		t.Fatalf("refresh erased capability facts: %+v %+v", got.Factors, got.RegionFacts)
	}
}
