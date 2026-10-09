package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// 上下文档位必须来自模型的真实档位，而不是模板里写死的常量。
// 模板里 max_input_tokens 固定 180000，会让 1M 档的模型被上游当成 180K：
// 长上下文请求会被按超长拒绝，而面板却显示着 1M。

func decodeBody(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	return out
}

func maxInputTokensOf(t *testing.T, raw []byte) (int64, bool) {
	t.Helper()
	body := decodeBody(t, raw)
	mc, ok := body["model_config"].(map[string]any)
	if !ok {
		return 0, false
	}
	v, ok := mc["max_input_tokens"]
	if !ok {
		return 0, false
	}
	f, ok := v.(float64)
	if !ok {
		return 0, false
	}
	return int64(f), true
}

func TestBuildQoderBodyAppliesDefaultContextTier(t *testing.T) {
	tokens := int64(1000000)
	facts := &modelRegionFacts{ContextLength: &tokens}
	raw, err := buildQoderBody(slimRequest(), "dfmodel", "personal", withModelContext(facts))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := maxInputTokensOf(t, raw)
	if !ok {
		t.Fatal("model_config.max_input_tokens missing")
	}
	if got != 1000000 {
		t.Fatalf("max_input_tokens = %d, want 1000000", got)
	}
}

// 没有目录数据时必须保留模板默认，不能因为取不到档位就把窗口写成 0。
func TestBuildQoderBodyLeavesTemplateValueWithoutFacts(t *testing.T) {
	raw, err := buildQoderBody(slimRequest(), "dfmodel", "personal")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := maxInputTokensOf(t, raw)
	if !ok {
		t.Fatal("model_config.max_input_tokens missing")
	}
	if got <= 0 {
		t.Fatalf("max_input_tokens = %d, want the template value", got)
	}
}

// 传 nil facts 与不传选项等价，避免调用方在无数据时写出 0。
func TestBuildQoderBodyIgnoresNilFacts(t *testing.T) {
	base, err := buildQoderBody(slimRequest(), "dfmodel", "personal")
	if err != nil {
		t.Fatal(err)
	}
	withNil, err := buildQoderBody(slimRequest(), "dfmodel", "personal", withModelContext(nil))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := maxInputTokensOf(t, base)
	b, _ := maxInputTokensOf(t, withNil)
	if a != b {
		t.Fatalf("nil facts changed the window: %d != %d", a, b)
	}
}

// ContextLength 缺失时退回默认档位，而不是放弃。
func TestWithModelContextFallsBackToDefaultTier(t *testing.T) {
	facts := &modelRegionFacts{
		ContextTiers: []contextTier{
			{Label: "200K", Tokens: 200000, IsDefault: true},
			{Label: "1M", Tokens: 1000000},
		},
	}
	var cfg qoderBodyConfig
	withModelContext(facts)(&cfg)
	if cfg.maxInputTokens != 200000 {
		t.Fatalf("maxInputTokens = %d, want the default tier 200000", cfg.maxInputTokens)
	}
}

// contextTierForModel 必须按账号所在区域取档位，不能跨区域借数据。
func TestContextTierForModelScopesByAccountRegion(t *testing.T) {
	cnTokens := int64(200000)
	sa := testStoredAuthCNAccount()
	restore := setDynamicModelsCacheForTest(map[string][]pluginapi.ModelInfo{
		accountCacheKey(sa): {{ID: "dfmodel", Name: "DF"}},
	})
	defer restore()
	// 缓存里补上档位事实。
	dynamicModelsCache.Lock()
	entry := dynamicModelsCache.byAccount[accountCacheKey(sa)]
	entry.region = map[string]modelRegionFacts{
		"dfmodel": {ContextLength: &cnTokens},
	}
	entry.fetched = time.Now()
	dynamicModelsCache.byAccount[accountCacheKey(sa)] = entry
	dynamicModelsCache.Unlock()

	facts, ok := contextTierForModel(sa, "dfmodel")
	if !ok || facts == nil {
		t.Fatal("context facts not found for the account's own model")
	}
	if facts.ContextLength == nil || *facts.ContextLength != 200000 {
		t.Fatalf("context length = %v, want 200000", facts.ContextLength)
	}
	// 该账号目录里没有的模型必须报告未找到，不能编一个窗口出来。
	if _, ok := contextTierForModel(sa, "not-in-catalog"); ok {
		t.Fatal("unknown model reported context facts")
	}
	if _, ok := contextTierForModel(nil, "dfmodel"); ok {
		t.Fatal("nil auth reported context facts")
	}
}
