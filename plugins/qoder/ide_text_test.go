package main

import (
	"errors"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The announcement text is the only place a limited-time campaign states its
// wording and expiry, so parsing it is the feature. Fixtures below are copied
// verbatim from the live CN/Intl documents (version 1.0.82 / 1.0.341).
const fixtureIDETextCN = `{
  "zh": {
    "modelSelector.item.dfmodel": "DeepSeek-Flash",
    "modelSelector.item.dfmodel.description": "深度求索正式版模型（DeepSeek-V4.1-Flash-1017），Agent 能力、世界知识与推理性能全面领先。DeepSeek 实行峰谷定价，Credits 以实际消耗为准",
    "modelSelector.item.dfmodel.markdownDescription": "深度求索正式版模型（DeepSeek-V4.1-Flash-1017），Agent 能力、世界知识与推理性能全面领先。个人版限时免费，至 11月01日 00:00（UTC+8）。",
    "modelSelector.item.qfmodel": "Qwen3.8-Flash",
    "modelSelector.item.qfmodel.markdownDescription": "千问开源权重的多模态 MoE 模型，在能力、延迟与成本间取得出色平衡。限时免费畅用，[查看详情](https://docs.qoder.cn/events/flashoffer)",
    "modelSelector.item.dmodel": "DeepSeek-Pro",
    "modelSelector.item.dmodel.markdownDescription": "深度求索正式版模型。按量计费。",
    "model.dfmodel.detail": "Desktop 通道的同一文案"
  },
  "en": {
    "modelSelector.item.dfmodel": "DeepSeek-Flash",
    "modelSelector.item.dfmodel.markdownDescription": "DeepSeek's official release. Free for individuals until Nov 01 00:00 (UTC+8)."
  }
}`

func withIDETextFixture(t *testing.T, raw string) func() {
	t.Helper()
	restore := resetIDETextCacheForTest()
	origLatest, origDoc := fetchIDETextLatest, fetchIDETextDocument
	fetchIDETextLatest = func(string) (string, error) { return "https://example.test/text.json", nil }
	fetchIDETextDocument = func(string) ([]byte, error) { return []byte(raw), nil }
	return func() {
		fetchIDETextLatest, fetchIDETextDocument = origLatest, origDoc
		restore()
	}
}

func seedFactsForTest(t *testing.T, region, uid string, facts map[string]modelRegionFacts) func() {
	t.Helper()
	dynamicModelsCache.Lock()
	prev := dynamicModelsCache.byAccount
	models := make([]pluginapi.ModelInfo, 0, len(facts))
	for id := range facts {
		models = append(models, pluginapi.ModelInfo{ID: id})
	}
	dynamicModelsCache.byAccount = map[string]dynamicModelEntry{
		region + "\x00" + uid: {models: models, region: facts, fetched: time.Now()},
	}
	dynamicModelsCache.Unlock()
	return func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.byAccount = prev
		dynamicModelsCache.Unlock()
	}
}

func TestIDETextParsesBothKeyNamespaces(t *testing.T) {
	entries := parseIDETextDocument([]byte(fixtureIDETextCN))
	df, ok := entries["dfmodel"]
	if !ok {
		t.Fatalf("dfmodel missing: %+v", entries)
	}
	if df.Name != "DeepSeek-Flash" {
		t.Fatalf("name = %q", df.Name)
	}
	if !contains(df.Detail, "限时免费") || !contains(df.Detail, "11月01日") {
		t.Fatalf("campaign detail lost: %q", df.Detail)
	}
	if entries["dmodel"].Detail == "" {
		t.Fatal("non-offer model text should still parse")
	}
}

func TestIDETextLinkExtraction(t *testing.T) {
	entries := parseIDETextDocument([]byte(fixtureIDETextCN))
	if got := ideTextLink(entries["qfmodel"]); got != "https://docs.qoder.cn/events/flashoffer" {
		t.Fatalf("link = %q", got)
	}
	if got := stripMarkdownLinks(entries["qfmodel"].Detail); contains(got, "](") {
		t.Fatalf("markdown not stripped: %q", got)
	}
	if !contains(stripMarkdownLinks(entries["qfmodel"].Detail), "限时免费畅用") {
		t.Fatal("label should survive markdown stripping")
	}
}

func TestIDETextOfferDetection(t *testing.T) {
	entries := parseIDETextDocument([]byte(fixtureIDETextCN))
	if !ideTextHasOffer(entries["dfmodel"]) {
		t.Fatal("limited-time free must count as an offer")
	}
	if ideTextHasOffer(entries["dmodel"]) {
		t.Fatal("plain priced model must not count as an offer")
	}
}

// The regression that matters: a free model with campaign wording becomes an
// activity entry that carries the expiry sentence, while a free model without
// wording stays out.
func TestPromotionsIncludeFreeModelWithCampaignText(t *testing.T) {
	restoreText := withIDETextFixture(t, fixtureIDETextCN)
	defer restoreText()
	zero := 0.0
	orig := 0.1
	restore := seedFactsForTest(t, regionCN, "uid-text", map[string]modelRegionFacts{
		"dfmodel": {PriceFactor: &zero, OriginalPriceFactor: &orig, IsFree: true},
		"dmodel":  {PriceFactor: &zero, IsFree: true},
	})
	defer restore()

	got := buildModelPromotionsQuery()
	promos := got["promotions"].(map[string][]map[string]any)[regionCN]
	if len(promos) != 1 {
		t.Fatalf("expected only dfmodel, got %+v", promos)
	}
	entry := promos[0]
	if entry["model_id"] != "dfmodel" {
		t.Fatalf("wrong model: %+v", entry)
	}
	text, _ := entry["text_zh"].(string)
	if !contains(text, "限时免费") || !contains(text, "11月01日") {
		t.Fatalf("expiry wording missing: %q", text)
	}
	promo := entry["promotion"].(map[string]any)
	if promo["badge_zh"] != "限时免费" {
		t.Fatalf("badge = %v", promo["badge_zh"])
	}
}

func TestPromotionsSkipFreeModelWithoutCampaignText(t *testing.T) {
	restoreText := withIDETextFixture(t, fixtureIDETextCN)
	defer restoreText()
	zero := 0.0
	restore := seedFactsForTest(t, regionCN, "uid-plain", map[string]modelRegionFacts{
		"dmodel": {PriceFactor: &zero, IsFree: true},
	})
	defer restore()

	got := buildModelPromotionsQuery()
	if promos := got["promotions"].(map[string][]map[string]any)[regionCN]; len(promos) != 0 {
		t.Fatalf("free-but-unannounced model must not be an activity: %+v", promos)
	}
}

func TestPromotionsSurviveIDETextOutage(t *testing.T) {
	restore := resetIDETextCacheForTest()
	defer restore()
	origLatest, origDoc := fetchIDETextLatest, fetchIDETextDocument
	// Text channel down; structured offer must still show.
	fetchIDETextLatest = func(string) (string, error) { return "", errors.New("boom") }
	fetchIDETextDocument = origDoc
	defer func() { fetchIDETextLatest, fetchIDETextDocument = origLatest, origDoc }()

	active := true
	factor := 0.4
	before := 0.5
	restoreFacts := seedFactsForTest(t, regionCN, "uid-outage", map[string]modelRegionFacts{
		"qmodel_38max": {
			PriceFactor: &factor,
			Promotion: &qoderPromotionFacts{
				Active: active, BadgeZh: "错峰 4 折", DiscountFactor: &factor,
				BeforePromotionPriceFactor: &before,
			},
		},
	})
	defer restoreFacts()

	got := buildModelPromotionsQuery()
	promos := got["promotions"].(map[string][]map[string]any)[regionCN]
	if len(promos) != 1 {
		t.Fatalf("structured offer lost during text outage: %+v", promos)
	}
	status := got["text_status"].(map[string]any)
	if !contains(status[regionCN].(string), "unavailable") {
		t.Fatalf("outage not reported: %v", status)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// Intl serves the text from center.qoder.sh, not from the inference gateway.
// Using api3.qoder.sh made every Intl refresh report "unsupported".
func TestIDETextGatewayHostsPerRegion(t *testing.T) {
	if got := ideTextGateway[regionCN]; got != gatewayBaseCN {
		t.Fatalf("CN host = %q", got)
	}
	if got := ideTextGateway[regionIntl]; got != ideTextBaseIntl {
		t.Fatalf("Intl host = %q", got)
	}
	if ideTextGateway[regionIntl] == gatewayBaseIntl {
		t.Fatal("Intl must not reuse the inference gateway for ide-text")
	}
}
