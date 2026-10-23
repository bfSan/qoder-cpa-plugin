package main

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Free models are active offers even without a promotion object.
// Regression: DeepSeek (dfmodel) and qfmodel arrive with is_free=true and
// price_factor 0 but no promotion block, so the activity panel showed nothing
// while the model list already displayed a 0 multiplier.
func TestFreeModelAppearsAsPromotion(t *testing.T) {
	zero := 0.0
	paid := 0.5
	dynamicModelsCache.Lock()
	prev := dynamicModelsCache.byAccount
	dynamicModelsCache.byAccount = map[string]dynamicModelEntry{
		"cn\x00uid-free": {
			models: []pluginapi.ModelInfo{{ID: "dfmodel"}, {ID: "qmodel_38max"}},
			region: map[string]modelRegionFacts{
				"dfmodel":      {PriceFactor: &zero, IsFree: true},
				"qmodel_38max": {PriceFactor: &paid},
			},
			fetched: time.Now(),
		},
	}
	dynamicModelsCache.Unlock()
	defer func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.byAccount = prev
		dynamicModelsCache.Unlock()
	}()

	got := buildModelPromotionsQuery()
	promotions := got["promotions"].(map[string][]map[string]any)
	if len(promotions[regionCN]) != 1 {
		t.Fatalf("free model missing from promotions: %+v", promotions[regionCN])
	}
	entry := promotions[regionCN][0]
	if entry["model_id"] != "dfmodel" || entry["is_free"] != true {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	promo := entry["promotion"].(map[string]any)
	if promo["badge_zh"] != "免费" {
		t.Fatalf("free badge missing: %+v", promo)
	}
}
