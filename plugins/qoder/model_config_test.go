package main

import (
	"reflect"
	"testing"

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
	t.Cleanup(setDynamicModelsCacheForTest(nil))
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
	t.Cleanup(setDynamicModelsCacheForTest(nil))

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

// TestDynamicCacheIsPerAccount is the regression test for the bug where one
// shared cache slot answered every account: the CN gateway advertises
// q37fmodel / gm51model while the Intl gateway advertises
// ultimate / performance / efficient, so a single slot made whichever account
// was discovered first serve its catalog to the other — surfacing as an
// intermittent "unknown provider for model" on a perfectly valid alias.
func TestDynamicCacheIsPerAccount(t *testing.T) {
	t.Cleanup(setDynamicModelsCacheForTest(nil))

	cnKey := accountCacheKey(&storedAuth{
		Auth:    storedTokens{AccessToken: "cn-token", Region: regionCN},
		Account: storedAccount{UID: "uid-cn"},
	})
	intlKey := accountCacheKey(&storedAuth{
		Auth:    storedTokens{AccessToken: "intl-token", Region: regionIntl},
		Account: storedAccount{UID: "uid-intl"},
	})
	if cnKey == intlKey {
		t.Fatalf("CN and Intl must not share a cache key (both %q)", cnKey)
	}

	storeDynamicModelsFor(cnKey, testModels("gmodel", "gm51model", "q37fmodel"), nil)
	storeDynamicModelsFor(intlKey, testModels("gmodel", "ultimate", "performance", "efficient"), nil)

	cn, ok := cachedDynamicModelsFor(cnKey)
	if !ok {
		t.Fatal("CN entry missing")
	}
	intl, ok := cachedDynamicModelsFor(intlKey)
	if !ok {
		t.Fatal("Intl entry missing")
	}
	if !containsModel(cn, "gm51model") {
		t.Fatalf("CN catalog = %v, want gm51model", modelIDs(cn))
	}
	if containsModel(cn, "ultimate") {
		t.Fatalf("CN catalog leaked the Intl list: %v", modelIDs(cn))
	}
	if !containsModel(intl, "ultimate") {
		t.Fatalf("Intl catalog = %v, want ultimate", modelIDs(intl))
	}
	if containsModel(intl, "gm51model") {
		t.Fatalf("Intl catalog leaked the CN list: %v", modelIDs(intl))
	}

	// The provider-wide view (model.static) must be the union of both.
	all, ok := cachedDynamicModelsAll()
	if !ok {
		t.Fatal("union catalog missing")
	}
	for _, want := range []string{"gm51model", "q37fmodel", "ultimate", "performance", "efficient"} {
		if !containsModel(all, want) {
			t.Fatalf("union = %v, missing %s", modelIDs(all), want)
		}
	}
}

func containsModel(models []pluginapi.ModelInfo, id string) bool {
	for _, m := range models {
		if m.ID == id {
			return true
		}
	}
	return false
}
