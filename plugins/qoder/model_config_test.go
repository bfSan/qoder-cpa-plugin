package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func testModels(ids ...string) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, pluginapi.ModelInfo{
			ID:                         id,
			Name:                       id,
			OwnedBy:                    providerName,
			SupportedGenerationMethods: []string{"chat"},
		})
	}
	return out
}

func modelIDs(models []pluginapi.ModelInfo) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

func TestApplyModelOverlayHideRestoreAndAdd(t *testing.T) {
	base := testModels("auto", "qwen", "hidden")
	visible := modelIDs(applyModelOverlay(base, modelOverlay{
		Hide: []string{"hidden"},
		Add:  []string{"custom"},
	}))
	if want := []string{"auto", "qwen", "custom"}; !reflect.DeepEqual(visible, want) {
		t.Fatalf("visible = %v, want %v", visible, want)
	}

	admin := modelIDs(applyModelOverlayForAdmin(base, modelOverlay{Hide: []string{"hidden"}}))
	if want := []string{"auto", "qwen", "hidden"}; !reflect.DeepEqual(admin, want) {
		t.Fatalf("admin = %v, want hidden entry preserved", admin)
	}
}

func TestApplyModelOverlayOrderPinsFirst(t *testing.T) {
	base := testModels("auto", "qwen", "kimi", "gpt")
	got := modelIDs(applyModelOverlay(base, modelOverlay{Order: []string{"kimi", "auto"}}))
	if want := []string{"kimi", "auto", "qwen", "gpt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ordered = %v, want %v", got, want)
	}
}

func TestStoreModelOverlayRejectsInvalidEntries(t *testing.T) {
	if _, err := storeModelOverlay(modelOverlay{Hide: []string{" "}}); err == nil {
		t.Fatal("blank model ID must be rejected")
	}
	if _, err := storeModelOverlay(modelOverlay{Add: []string{"x", "x"}}); err == nil {
		t.Fatal("duplicate model ID must be rejected")
	}
}

func TestHandleModelOverlayActionHidePersistsHiddenModels(t *testing.T) {
	defer setModelOverlayForTest(modelOverlay{})()
	res := handleModelOverlayAction(pluginapi.ManagementRequest{Body: []byte(`{"action":"hide","id":"qmodel"}`)})
	if res["success"] != true {
		t.Fatalf("hide failed: %#v", res)
	}
	if got := currentHiddenModels(); len(got) != 1 {
		t.Fatalf("hidden_models = %v, want one entry", got)
	}
	if res["persistent"] != true {
		t.Fatalf("persistent = %v, want true", res["persistent"])
	}
}

func TestBuildModelListQueryReportsOverlayAndCooldowns(t *testing.T) {
	defer setModelOverlayForTest(modelOverlay{Hide: []string{"hidden"}})()
	oldModels := dynamicModelsCache.models
	oldFetched := dynamicModelsCache.fetched
	t.Cleanup(func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.models = oldModels
		dynamicModelsCache.fetched = oldFetched
		dynamicModelsCache.Unlock()
	})
	storeDynamicModels(testModels("auto", "hidden"))
	markModelCooldown("auth-a", "auto", cooldownReasonRateLimit)
	t.Cleanup(func() { clearModelCooldown("auth-a", "") })

	res := buildModelListQuery()
	models, _ := res["models"].([]map[string]any)
	if len(models) != 2 {
		t.Fatalf("models = %#v, want visible + hidden admin row", models)
	}
	if models[1]["hidden"] != true {
		t.Fatalf("hidden flag = %#v, want true", models[1])
	}
	if models[0]["coolingAccounts"] != 1 {
		t.Fatalf("coolingAccounts = %#v, want 1", models[0]["coolingAccounts"])
	}
}

func TestBuildModelListQueryReportsPriceFactor(t *testing.T) {
	defer setModelOverlayForTest(modelOverlay{})()
	oldModels := dynamicModelsCache.models
	oldFetched := dynamicModelsCache.fetched
	oldFactors := priceFactorCache.factors
	t.Cleanup(func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.models = oldModels
		dynamicModelsCache.fetched = oldFetched
		dynamicModelsCache.Unlock()
		priceFactorCache.Lock()
		priceFactorCache.factors = oldFactors
		priceFactorCache.Unlock()
	})
	dynamicModelsCache.Lock()
	dynamicModelsCache.models = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.Unlock()
	priceFactorCache.Lock()
	priceFactorCache.factors = nil
	priceFactorCache.Unlock()

	res := buildModelListQuery()
	models, _ := res["models"].([]map[string]any)
	byID := make(map[string]map[string]any, len(models))
	for _, m := range models {
		id, _ := m["id"].(string)
		byID[id] = m
	}
	// Static fallback table feeds models that have no dynamic reading yet.
	if got := byID["auto"]["priceFactor"]; got != 0.5 {
		t.Fatalf("auto priceFactor = %#v, want 0.5", got)
	}
	if got := byID["qfmodel"]["priceFactor"]; got != 0.0 {
		t.Fatalf("qfmodel priceFactor = %#v, want 0 (free)", got)
	}
	if _, ok := byID["ultimate"]["priceFactor"]; ok {
		t.Fatal("ultimate has no known factor; priceFactor must be omitted")
	}

	// A dynamic reading overrides the static table.
	storePriceFactors(map[string]float64{"auto": 0.9})
	res = buildModelListQuery()
	models, _ = res["models"].([]map[string]any)
	for _, m := range models {
		if m["id"] == "auto" && m["priceFactor"] != 0.9 {
			t.Fatalf("dynamic auto priceFactor = %#v, want 0.9", m["priceFactor"])
		}
	}
}
