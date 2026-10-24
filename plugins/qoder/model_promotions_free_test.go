package main

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Free models are announced through the presentation text, not the model list.
// Regression: DeepSeek (dfmodel) arrives with is_free=true and price_factor 0
// but no promotion block; the campaign wording (and its expiry) only exists in
// the ide-text document, so the panel showed nothing at all.
func TestFreeModelAppearsAsPromotion(t *testing.T) {
	restoreText := withIDETextFixture(t, fixtureIDETextCN)
	defer restoreText()
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
	if promo["badge_zh"] != "限时免费" {
		t.Fatalf("limited-time badge missing: %+v", promo)
	}
}
