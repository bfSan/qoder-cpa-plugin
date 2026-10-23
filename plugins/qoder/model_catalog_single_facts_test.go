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
