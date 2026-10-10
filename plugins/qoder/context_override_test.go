package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

// 上下文档位覆盖：面板点一档，后端就按这一档发请求，并且重启后仍然记得。
//
// 这一组测试守的是"三处显示同一个数"：面板显示的生效档、CPA 对外声明的档、
// 以及真正塞进上游请求体 max_input_tokens 的值。三者只要有一处不同步，操作者
// 就会看到一个自己拿不到的窗口——这正是加这个功能要消除的误导。

// seedTierCatalog 往缓存里放一个模型的三档目录，返回恢复函数。
func seedTierCatalog(t *testing.T, modelID string) (*storedAuth, func()) {
	t.Helper()
	sa := testStoredAuthCNAccount()
	restore := setDynamicModelsCacheForTest(map[string][]pluginapi.ModelInfo{
		accountCacheKey(sa): {{ID: modelID, Name: "Test Model"}},
	})
	dynamicModelsCache.Lock()
	entry := dynamicModelsCache.byAccount[accountCacheKey(sa)]
	// ContextLength 是后端算好的生效值（上游默认档），真实目录里它总有值 ——
	// 上游给每个模型都标了一个 is_default 档。这里照实填上，否则面板那一格会
	// 渲染成 nil，就测不到"覆盖值替换掉默认档"这件事。
	defaultTokens := int64(200000)
	entry.region = map[string]modelRegionFacts{
		modelID: {
			ContextLength: &defaultTokens,
			ContextTiers: []contextTier{
				{Label: "200K", Tokens: 200000, IsDefault: true},
				{Label: "400K", Tokens: 400000},
				{Label: "1M", Tokens: 1000000},
			},
		},
	}
	entry.fetched = time.Now()
	dynamicModelsCache.byAccount[accountCacheKey(sa)] = entry
	dynamicModelsCache.Unlock()
	return sa, restore
}

func resetContextOverridesForTest(t *testing.T) {
	t.Helper()
	prev := currentContextOverrides()
	t.Cleanup(func() { replaceContextOverrides(prev) })
	replaceContextOverrides(nil)
}

// yamlNodeFrom 把一段 YAML 文档解析成 yaml.Node。空串返回零值 Node —— 那是
// "这个键根本没出现在配置里"的形状，与"显式空映射"必须区分开。
func yamlNodeFrom(t *testing.T, doc string) yaml.Node {
	t.Helper()
	if doc == "" {
		return yaml.Node{}
	}
	var wrapper struct {
		Value yaml.Node `yaml:"value"`
	}
	if err := yaml.Unmarshal([]byte("value:\n"+indentYAML(doc)), &wrapper); err != nil {
		t.Fatalf("fixture yaml did not parse: %v", err)
	}
	return wrapper.Value
}

// indentYAML 把多行片段缩进两级，好让上面那个 wrapper 把它读成 value 的值。
func indentYAML(doc string) string {
	lines := strings.Split(strings.TrimRight(doc, "\n"), "\n")
	for i, line := range lines {
		lines[i] = "  " + line
	}
	return strings.Join(lines, "\n") + "\n"
}

// configure() 是重启后恢复档位的唯一路径：config.yaml 里的 model_context
// 只有经过它才会回到内存。这里直接驱动它，因为"重启后档位还在"这句承诺就落在
// 这一条链上——组件函数单测得再全也证明不了它被接上了。
func TestConfigureRestoresContextOverrides(t *testing.T) {
	resetContextOverridesForTest(t)
	prevHidden, _ := loadedModelOverlay()
	t.Cleanup(func() { syncOverlayHiddenModels(prevHidden.Hide); syncOverlayModelOrder(prevHidden.Order) })

	payload, err := json.Marshal(map[string]any{
		"config_yaml": []byte("model_context:\n  qmodel_38max: 1000000\n  dmodel: 400000\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	configure(payload)

	got := currentContextOverrides()
	if got["qmodel_38max"] != 1000000 || got["dmodel"] != 400000 {
		t.Fatalf("overrides after configure = %v, want the two from config", got)
	}

	// 配置里没有这个键时不能清掉已有的覆盖：面板 PATCH 只带 model_order 是
	// 常见情形，那时若顺手清空，操作者刚选的档位会在下一次保存排序时消失。
	payload, _ = json.Marshal(map[string]any{
		"config_yaml": []byte("model_order:\n  - qmodel_38max\n"),
	})
	configure(payload)
	if got := currentContextOverrides(); got["qmodel_38max"] != 1000000 {
		t.Fatalf("an unrelated PATCH wiped the overrides: %v", got)
	}

	// 显式空映射才是"清空"。
	payload, _ = json.Marshal(map[string]any{
		"config_yaml": []byte("model_context: {}\n"),
	})
	configure(payload)
	if len(currentContextOverrides()) != 0 {
		t.Fatalf("an explicit empty map did not clear the overrides: %v", currentContextOverrides())
	}
}

func TestContextTierOverrideChangesTheRequestBody(t *testing.T) {
	modelID := "qmodel_38max"
	sa, restoreCatalog := seedTierCatalog(t, modelID)
	defer restoreCatalog()
	resetContextOverridesForTest(t)

	// 没有覆盖时按上游默认档发。
	raw, err := buildQoderBody(slimRequest(), modelID, "personal",
		withModelContext(mustFactsFor(t, sa, modelID)))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := maxInputTokensOf(t, raw); got != 200000 {
		t.Fatalf("default tier: max_input_tokens = %d, want 200000", got)
	}

	// 覆盖成 1M 之后，同一个请求体必须换成 1M。这是这个功能的核心承诺。
	value := int64(1000000)
	setContextOverride(modelID, &value)
	raw, err = buildQoderBody(slimRequest(), modelID, "personal",
		withModelContext(mustFactsFor(t, sa, modelID)))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := maxInputTokensOf(t, raw); got != 1000000 {
		t.Fatalf("overridden tier: max_input_tokens = %d, want 1000000", got)
	}

	// 清除覆盖后回到上游默认档，不能留下痕迹。
	setContextOverride(modelID, nil)
	raw, err = buildQoderBody(slimRequest(), modelID, "personal",
		withModelContext(mustFactsFor(t, sa, modelID)))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := maxInputTokensOf(t, raw); got != 200000 {
		t.Fatalf("after clearing: max_input_tokens = %d, want the default 200000", got)
	}
}

func mustFactsFor(t *testing.T, sa *storedAuth, modelID string) *modelRegionFacts {
	t.Helper()
	facts, ok := contextTierForModel(sa, modelID)
	if !ok || facts == nil {
		t.Fatalf("no context facts for %s", modelID)
	}
	return facts
}

// 覆盖值必须同时改变面板显示的那一列，否则面板会继续显示旧档位，
// 而请求已经按新档位发出——两者不一致比不改更糟。
func TestContextTierOverrideShowsUpInThePanelRow(t *testing.T) {
	modelID := "qmodel_38max"
	_, restoreCatalog := seedTierCatalog(t, modelID)
	defer restoreCatalog()
	resetContextOverridesForTest(t)

	rowFor := func() map[string]any {
		for _, row := range buildPanelRegionModels() {
			if row["id"] == modelID {
				return row
			}
		}
		return nil
	}
	row := rowFor()
	if row == nil {
		t.Fatal("model missing from the panel catalog")
	}
	cn, _ := row["cn"].(map[string]any)
	if cn == nil || cn["context_length"] != int64(200000) {
		t.Fatalf("unoverridden cell context_length = %v, want 200000", cn["context_length"])
	}
	// 两区的 cell 都必须跟着覆盖值走：面板一列展示两区，只改一区会让操作者
	// 以为覆盖没生效。
	value := int64(1000000)
	setContextOverride(modelID, &value)
	row = rowFor()
	for _, region := range []string{"cn", "intl"} {
		cell, _ := row[region].(map[string]any)
		if cell == nil || cell["present"] != true {
			continue
		}
		if cell["context_length"] != int64(1000000) {
			t.Fatalf("%s context_length = %v, want the override 1000000", region, cell["context_length"])
		}
		if cell["context_override"] != true {
			t.Fatalf("%s context_override not set; the panel cannot tell an override from a default", region)
		}
	}
	// 档位选项必须原样列出，供面板画按钮。
	options, _ := row["context_options"].([]int64)
	if len(options) != 3 || options[0] != 200000 || options[2] != 1000000 {
		t.Fatalf("context_options = %v, want the three ascending tiers", options)
	}
}

// 覆盖只能落在该模型上报过的档位里。放行表外的值等于替上游承诺一个它没答应的
// 窗口：上游会回落到自己的限制，而面板仍显示那个大数。
func TestContextTierWriteRejectsTiersTheModelDoesNotOffer(t *testing.T) {
	modelID := "qmodel_38max"
	_, restoreCatalog := seedTierCatalog(t, modelID)
	defer restoreCatalog()
	resetContextOverridesForTest(t)

	body := func(tokens *int64) []byte {
		payload := map[string]any{"id": modelID}
		if tokens != nil {
			payload["context_length"] = *tokens
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	// 表外的值被拒，并回报可选项，好让面板直接展示而不是让操作者猜。
	foreign := int64(750000)
	resp := handleModelContextWrite(pluginapi.ManagementRequest{Body: body(&foreign)})
	if resp["success"] != false {
		t.Fatalf("a tier outside the reported set was accepted: %v", resp)
	}
	if got, _ := resp["context_options"].([]int64); len(got) != 3 {
		t.Fatalf("rejection did not report the offered tiers: %v", resp)
	}
	if len(currentContextOverrides()) != 0 {
		t.Fatal("a rejected write must not store anything")
	}

	// 表内的值被接受。
	ok := int64(400000)
	resp = handleModelContextWrite(pluginapi.ManagementRequest{Body: body(&ok)})
	if resp["success"] != true {
		t.Fatalf("a reported tier was rejected: %v", resp)
	}
	if got := currentContextOverrides()[modelID]; got != 400000 {
		t.Fatalf("stored override = %d, want 400000", got)
	}

	// 省略 context_length 表示清除覆盖，回到上游默认档。
	resp = handleModelContextWrite(pluginapi.ManagementRequest{Body: body(nil)})
	if resp["success"] != true {
		t.Fatalf("clearing the override failed: %v", resp)
	}
	if _, still := currentContextOverrides()[modelID]; still {
		t.Fatal("clearing did not remove the override")
	}
}

// 目录还没加载时无从判断档位是否合法，此时必须拒绝而不是放行：放行会让面板显示
// 一个后端无法验证的窗口。
func TestContextTierWriteRefusesBeforeTheCatalogIsLoaded(t *testing.T) {
	resetContextOverridesForTest(t)
	restore := setDynamicModelsCacheForTest(nil)
	defer restore()

	tokens := int64(1000000)
	raw, _ := json.Marshal(map[string]any{"id": "unknown-model", "context_length": tokens})
	resp := handleModelContextWrite(pluginapi.ManagementRequest{Body: raw})
	if resp["success"] != false {
		t.Fatalf("a write with no catalog was accepted: %v", resp)
	}
	if len(currentContextOverrides()) != 0 {
		t.Fatal("nothing may be stored when the tiers cannot be verified")
	}
}

// config.yaml 里的 model_context 必须能解析回来，并且键缺失时保留现状 ——
// 后者关系到"PATCH 只带 model_order 时不能顺手清掉档位覆盖"。
func TestContextOverrideConfigParsing(t *testing.T) {
	resetContextOverridesForTest(t)

	// 键缺失（Kind==0）返回 nil，调用方据此保留现状。
	if got, err := normalizedContextOverrideConfig(yamlNodeFrom(t, "")); err != nil || got != nil {
		t.Fatalf("absent key: got %v / %v, want nil / nil", got, err)
	}
	parsed, err := normalizedContextOverrideConfig(yamlNodeFrom(t, "qmodel_38max: 1000000\ndmodel: 400000\n"))
	if err != nil {
		t.Fatal(err)
	}
	if parsed["qmodel_38max"] != 1000000 || parsed["dmodel"] != 400000 {
		t.Fatalf("parsed overrides = %v", parsed)
	}
	// 非法值必须报错，而不是被静默丢掉：静默丢弃会让配置看起来生效了、实际没有。
	for _, bad := range []string{"m: 0\n", "m: -5\n", "m: notanumber\n", "m: 999999999999\n"} {
		if _, err := normalizedContextOverrideConfig(yamlNodeFrom(t, bad)); err == nil {
			t.Fatalf("invalid config %q was accepted", bad)
		}
	}

	// replaceContextOverrides 写入后，读侧看到的就是新值。
	replaceContextOverrides(parsed)
	if got := currentContextOverrides()["qmodel_38max"]; got != 1000000 {
		t.Fatalf("after replace, override = %d, want 1000000", got)
	}
	// 显式空表表示清空。
	replaceContextOverrides(map[string]int64{})
	if len(currentContextOverrides()) != 0 {
		t.Fatal("an empty map must clear every override")
	}
}
