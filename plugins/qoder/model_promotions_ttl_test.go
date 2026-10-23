package main

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Promotions must survive an expired discovery TTL. Regression: the activity
// panel read facts through the TTL-gated accessor, so once a catalog aged past
// 12h (or the process restarted with an older FetchedAt) the campaigns
// disappeared even though they were still stored.
func TestPromotionsSurviveExpiredDiscoveryTTL(t *testing.T) {
	factor := 0.4
	dynamicModelsCache.Lock()
	prev := dynamicModelsCache.byAccount
	dynamicModelsCache.byAccount = map[string]dynamicModelEntry{
		"cn\x00uid-ttl": {
			models:  []pluginapi.ModelInfo{{ID: "qmodel_38max"}},
			factors: map[string]float64{"qmodel_38max": 0.4},
			region: map[string]modelRegionFacts{
				"qmodel_38max": {
					PriceFactor: &factor,
					Promotion:   &qoderPromotionFacts{Active: true, BadgeZh: "错峰 4 折"},
				},
			},
			fetched: time.Now().Add(-2 * dynamicModelsCacheTTL),
		},
	}
	dynamicModelsCache.Unlock()
	defer func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.byAccount = prev
		dynamicModelsCache.Unlock()
	}()

	got := buildModelPromotionsQuery()
	promotions, _ := got["promotions"].(map[string][]map[string]any)
	if len(promotions[regionCN]) != 1 {
		t.Fatalf("expired TTL erased promotions: %+v", promotions)
	}
	if got["status"].(map[string]any)[regionCN] != "loaded" {
		t.Fatalf("status should still be loaded: %+v", got["status"])
	}
}
