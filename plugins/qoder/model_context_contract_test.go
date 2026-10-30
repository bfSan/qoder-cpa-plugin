package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func contractModelResponse(t *testing.T, raw []byte, err error) []pluginapi.ModelInfo {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("invalid envelope %s: %v", raw, err)
	}
	var result pluginapi.ModelResponse
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatal(err)
	}
	return result.Models
}

func contractForAuth(t *testing.T, sa *storedAuth) []pluginapi.ModelInfo {
	t.Helper()
	raw, err := handleModelForAuth(mustJSONBytes(t, pluginapi.AuthModelRequest{AuthID: "contract-account", StorageJSON: mustJSONBytes(t, sa)}))
	return contractModelResponse(t, raw, err)
}

func contractSeed(t *testing.T) (*storedAuth, *storedAuth) {
	t.Helper()
	resetContextOverridesForTest(t)
	t.Cleanup(setModelOverlayForTest(modelOverlay{}))
	t.Cleanup(setDynamicModelsCacheForTest(nil))
	cn := testStoredAuthCNAccount()
	fillFixtureToken(cn)
	cn.Account.UID = "contract-cn"
	intl := testStoredAuthCNAccount()
	fillFixtureToken(intl)
	intl.Account.UID = "contract-intl"
	intl.Auth.Region = regionIntl
	intl.Auth.Domain = domainIntl
	for _, pair := range []struct {
		sa    *storedAuth
		tiers []contextTier
	}{
		{cn, []contextTier{{Label: "200K", Tokens: 200000, IsDefault: true}, {Label: "1M", Tokens: 1000000}}},
		{intl, []contextTier{{Label: "200K", Tokens: 200000}, {Label: "400K", Tokens: 400000, IsDefault: true}}},
	} {
		tokens, _ := contextDefaultTokens(pair.tiers)
		storeDynamicModelsForWithRegions(accountCacheKey(pair.sa), []pluginapi.ModelInfo{{ID: "shared", ContextLength: 1000000, MaxCompletionTokens: 8192}}, nil, map[string]modelRegionFacts{
			"shared": {ContextLength: &tokens, ContextTiers: pair.tiers},
		})
	}
	return cn, intl
}

func TestContextContractRealForAuthStaticBodySwitchAndClear(t *testing.T) {
	cn, intl := contractSeed(t)
	for _, pair := range []struct {
		sa   *storedAuth
		want int64
	}{{cn, 200000}, {intl, 400000}} {
		got := contractForAuth(t, pair.sa)
		if len(got) != 1 || got[0].ContextLength != pair.want {
			t.Fatalf("default registration: %+v", got)
		}
	}
	if options := supportedContextTiersFor("shared"); !reflect.DeepEqual(options, []int64{200000}) {
		t.Fatalf("global tiers must intersect: %v", options)
	}
	denied := handleModelContextWrite(pluginapi.ManagementRequest{Body: []byte(`{"id":"shared","context_length":1000000}`)})
	if denied["success"] != false {
		t.Fatalf("CN-only tier accepted globally: %v", denied)
	}
	proposal := handleModelContextWrite(pluginapi.ManagementRequest{Body: []byte(`{"id":"shared","context_length":200000}`)})
	if proposal["success"] != true || proposal["persistent"] != false || len(currentContextOverrides()) != 0 {
		t.Fatalf("proposal changed state: %v", proposal)
	}
	replaceContextOverrides(proposal["model_context"].(map[string]int64))
	for _, sa := range []*storedAuth{cn, intl} {
		got := contractForAuth(t, sa)
		if got[0].ContextLength != 200000 || got[0].MaxCompletionTokens != 8192 {
			t.Fatalf("override registration: %+v", got)
		}
		for _, stream := range []bool{false, true} {
			req := slimRequest()
			req.Stream = stream
			raw, err := buildQoderBody(req, "shared", "personal", withModelContext(mustFactsFor(t, sa, "shared")))
			if err != nil {
				t.Fatal(err)
			}
			if tokens, _ := maxInputTokensOf(t, raw); tokens != 200000 {
				t.Fatalf("stream=%v body context=%d", stream, tokens)
			}
			body := decodeBody(t, raw)
			if body["parameters"].(map[string]any)["max_tokens"] != float64(32768) {
				t.Fatalf("context changed output limit: %s", raw)
			}
		}
	}
	raw, err := handleModelStatic([]byte(`{}`))
	got := contractModelResponse(t, raw, err)
	if len(got) != 1 || got[0].ContextLength != 200000 {
		t.Fatalf("static context: %+v", got)
	}
	clear := handleModelContextWrite(pluginapi.ManagementRequest{Body: []byte(`{"id":"shared","context_length":null}`)})
	if len(currentContextOverrides()) != 1 {
		t.Fatal("clear proposal prematurely applied")
	}
	replaceContextOverrides(clear["model_context"].(map[string]int64))
	if got := contractForAuth(t, intl); got[0].ContextLength != 400000 {
		t.Fatalf("clear did not restore account default: %+v", got)
	}
	// Conservative provider static default does not borrow the larger Intl value.
	raw, err = handleModelStatic([]byte(`{}`))
	got = contractModelResponse(t, raw, err)
	if got[0].ContextLength != 200000 {
		t.Fatalf("static did not take conservative minimum: %+v", got)
	}
}

func TestContextContractReloadedOverrideRechecksActualAccount(t *testing.T) {
	cn, intl := contractSeed(t)
	// A previously saved config is retained, not rejected/unloading the plugin,
	// when another account/region with a different entitlement is discovered.
	replaceContextOverrides(map[string]int64{"shared": 1000000})
	if got := contractForAuth(t, cn); got[0].ContextLength != 1000000 {
		t.Fatalf("own supported override rejected: %+v", got)
	}
	if got := contractForAuth(t, intl); got[0].ContextLength != 400000 {
		t.Fatalf("CN context leaked to Intl: %+v", got)
	}
	fact := mustFactsFor(t, intl, "shared")
	if fact.ContextStatus != "override_rejected" || fact.ContextOverride {
		t.Fatalf("fallback status hidden: %+v", fact)
	}
	for _, row := range buildPanelRegionModels() {
		if row["id"] == "shared" {
			cell := row[regionIntl].(map[string]any)
			if cell["context_length"] != int64(400000) || cell["context_status"] != "override_rejected" {
				t.Fatalf("panel false override: %+v", cell)
			}
		}
	}
	if contextOverrideStates()["shared"]["status"] != "unverified_or_incompatible" {
		t.Fatal("global incompatibility hidden")
	}
}

func TestContextContractUnknownAndLastGoodNeverBorrowOtherAccount(t *testing.T) {
	cn, intl := contractSeed(t)
	fillFixtureToken(cn)
	fillFixtureToken(intl)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	t.Cleanup(server.Close)
	t.Cleanup(setGatewayBaseForTest(regionCN, server.URL))
	t.Cleanup(setGatewayBaseForTest(regionIntl, server.URL))
	dynamicModelsCache.Lock()
	entry := dynamicModelsCache.byAccount[accountCacheKey(cn)]
	entry.fetched = time.Now().Add(-2 * dynamicModelsCacheTTL)
	dynamicModelsCache.byAccount[accountCacheKey(cn)] = entry
	dynamicModelsCache.Unlock()
	if got := contractForAuth(t, cn); len(got) != 1 || got[0].ContextLength != 200000 {
		t.Fatalf("own last-good lost: %+v", got)
	}
	if fact := mustFactsFor(t, cn, "shared"); *fact.ContextLength != 200000 {
		t.Fatal("body lost own last-good after TTL")
	}
	unknown := *intl
	unknown.Account.UID = "new-account-no-catalog"
	if got := contractForAuth(t, &unknown); len(got) != 0 {
		t.Fatalf("unknown account borrowed static/union entitlement: %+v", got)
	}
	if _, ok := contextTierForModel(&unknown, "shared"); ok {
		t.Fatal("unknown account borrowed context")
	}
	if got := fetchDynamicModelsFromStorage([]byte(`not-json`)); len(got) != 0 {
		t.Fatalf("invalid storage borrowed union: %+v", got)
	}
}

func TestContextContractMissingFactsVetoGlobalOverride(t *testing.T) {
	cn, _ := contractSeed(t)
	storeDynamicModelsFor(regionCN+"\x00unknown-facts", []pluginapi.ModelInfo{{ID: "shared", ContextLength: 1000000}}, nil)
	if got := supportedContextTiersFor("shared"); len(got) != 0 {
		t.Fatalf("missing account facts didn't veto: %v", got)
	}
	if got := contractForAuth(t, cn); got[0].ContextLength != 200000 {
		t.Fatal("missing other account facts damaged known account")
	}
	union, _ := cachedDynamicModelsAll()
	if union[0].ContextLength != 0 {
		t.Fatalf("static hides unknown capability: %+v", union)
	}
	for _, row := range buildPanelRegionModels() {
		if row["id"] == "shared" {
			cell := row[regionCN].(map[string]any)
			if cell["context_length"] != nil || cell["account_count"] != 2 {
				t.Fatalf("region hid an account with unknown facts: %+v", cell)
			}
		}
	}
}

func TestContextContractCatalogParseDoesNotInventDefaultsOrReasoning(t *testing.T) {
	resetContextOverridesForTest(t)
	t.Cleanup(setDynamicModelsCacheForTest(nil))
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"chat":[{"key":"no-default","enable":true,"max_input_tokens":1000000,"context_config":{"200K":{"token_count":200000},"1M":{"token_count":1000000}},"thinking_config":{"enabled":{"is_default":true,"efforts":{"low":{},"high":{},"max":{}}}}},{"key":"absent","enable":true},{"key":"known","enable":true,"max_input_tokens":1000000,"context_config":{"200K":{"token_count":200000,"is_default":true},"1M":{"token_count":1000000}}}]}`)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(setGatewayBaseForTest(regionCN, server.URL))
	sa := testStoredAuthCNAccount()
	fillFixtureToken(sa)
	if _, err := callModelsAPI(sa); err != nil {
		t.Fatal(err)
	}
	facts, _ := lastGoodRegionFactsFor(accountCacheKey(sa))
	if facts["no-default"].ContextLength != nil {
		t.Fatal("missing tier default was guessed")
	}
	if facts["no-default"].Thinking.Default != "" {
		t.Fatal("enabled default was guessed as strongest effort")
	}
	if !reflect.DeepEqual(facts["no-default"].Thinking.SupportedByAdapter, []string{"low"}) || !reflect.DeepEqual(facts["no-default"].Thinking.UnsupportedByAdapter, []string{"high", "max"}) {
		t.Fatalf("adapter boundaries lost: %+v", facts["no-default"].Thinking)
	}
	if facts["absent"].Thinking.Status != "unknown" || facts["absent"].ContextLength != nil {
		t.Fatalf("missing capability fabricated: %+v", facts["absent"])
	}
	for _, m := range contractForAuth(t, sa) {
		want := int64(0)
		if m.ID == "known" {
			want = 200000
		}
		if m.ContextLength != want {
			t.Fatalf("parsed model %s context=%d want=%d", m.ID, m.ContextLength, want)
		}
	}
}

func TestContextContractSameRegionVarianceIsExplicit(t *testing.T) {
	cn, _ := contractSeed(t)
	other := *cn
	other.Account.UID = "contract-cn-second"
	tokens := int64(400000)
	storeDynamicModelsForWithRegions(accountCacheKey(&other), []pluginapi.ModelInfo{{ID: "shared"}}, nil, map[string]modelRegionFacts{"shared": {ContextLength: &tokens, ContextTiers: []contextTier{{Tokens: 400000, IsDefault: true}}}})
	for _, row := range buildPanelRegionModels() {
		if row["id"] == "shared" {
			cell := row[regionCN].(map[string]any)
			if cell["context_status"] != "account_variance" || cell["context_length"] != int64(200000) || cell["account_count"] != 2 {
				t.Fatalf("account difference hidden: %+v", cell)
			}
			if len(cell["account_variants"].([]map[string]any)) != 2 {
				t.Fatal("raw account facts lost")
			}
		}
	}
}

func TestContextContractLegacyTokenIdentityUsesEntireToken(t *testing.T) {
	a, b := testStoredAuthCNAccount(), testStoredAuthCNAccount()
	a.Account.UID = ""
	b.Account.UID = ""
	a.Auth.AccessToken = "same-prefix-123456789-first"
	b.Auth.AccessToken = "same-prefix-123456789-second"
	if accountCacheKey(a) == accountCacheKey(b) {
		t.Fatal("legacy token prefix collision")
	}
}

// Exercise both production executor entry points against a loopback-only fake
// gateway; no real chat, credentials, claim or production config is touched.
func TestContextContractExecuteAndStreamUseSameAccountResolver(t *testing.T) {
	cn, intl := contractSeed(t)
	resetCooldowns(t)
	replaceContextOverrides(map[string]int64{"shared": 1000000})
	received := make(chan []byte, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encoded, _ := io.ReadAll(r.Body)
		rearranged := make([]byte, len(encoded))
		for i, c := range encoded {
			if c < 128 {
				rearranged[i] = qoderC2S[c]
			}
		}
		n := len(rearranged)
		a := n / 3
		std := string(rearranged[n-a:]) + string(rearranged[a:n-a]) + string(rearranged[:a])
		decoded, err := base64.StdEncoding.DecodeString(std)
		if err != nil {
			w.WriteHeader(400)
			return
		}
		received <- decoded
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data:"+`{"headers":{},"body":"{\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}","statusCodeValue":200}`+"\n\n")
		_, _ = io.WriteString(w, "data:"+`{"headers":{},"body":"[DONE]","statusCodeValue":200}`+"\n\n")
	}))
	t.Cleanup(server.Close)
	t.Cleanup(setGatewayBaseForTest(regionCN, server.URL))
	t.Cleanup(setGatewayBaseForTest(regionIntl, server.URL))
	for _, pair := range []struct {
		sa   *storedAuth
		want int64
	}{{cn, 1000000}, {intl, 400000}} {
		for _, stream := range []bool{false, true} {
			request := pluginapi.ExecutorRequest{AuthID: "contract-executor", Model: "shared", StorageJSON: mustJSONBytes(t, pair.sa), Payload: []byte(`{"messages":[{"role":"system","content":"test"},{"role":"user","content":"local fixture"}],"reasoning_effort":"max"}`)}
			raw := mustJSONBytes(t, request)
			var err error
			if stream {
				_, err = handleExecStream(raw)
			} else {
				_, err = handleExecExecute(raw)
			}
			if err != nil {
				t.Fatalf("stream=%v: %v", stream, err)
			}
			bodyRaw := <-received
			if tokens, _ := maxInputTokensOf(t, bodyRaw); tokens != pair.want {
				t.Fatalf("stream=%v context=%d want=%d", stream, tokens, pair.want)
			}
			body := decodeBody(t, bodyRaw)
			params := body["parameters"].(map[string]any)
			if params["max_tokens"] != float64(32768) {
				t.Fatal("context overwrote output max_tokens")
			}
			if _, ok := params["reasoning_effort"]; ok {
				t.Fatal("unproven max reasoning was forwarded")
			}
			if body["model_config"].(map[string]any)["is_reasoning"] != true {
				t.Fatal("fixed reasoning flag was guessed from catalog")
			}
		}
	}
}

func TestContextContractNoMarkedDefaultIgnoresLegacyCachedCeiling(t *testing.T) {
	resetContextOverridesForTest(t)
	ceiling := int64(1000000)
	facts := effectiveContextFacts("m", modelRegionFacts{ContextLength: &ceiling, ContextTiers: []contextTier{{Tokens: 200000}, {Tokens: 1000000}}})
	if facts.ContextLength != nil || facts.ContextStatus != "unknown" {
		t.Fatalf("legacy ceiling became a default: %+v", facts)
	}
}

func TestContextContractLegacyThinkingAdapterClaimsAreRecomputed(t *testing.T) {
	facts := cloneThinkingFacts(&thinkingFacts{Status: "supported", Levels: []string{"low", "high", "max"}, SupportedByAdapter: []string{"max"}})
	if !reflect.DeepEqual(facts.SupportedByAdapter, []string{"low"}) || !reflect.DeepEqual(facts.UnsupportedByAdapter, []string{"high", "max"}) {
		t.Fatalf("stale adapter capability trusted: %+v", facts)
	}
}

func TestContextContractLegacyDiskFactsDoNotPromoteGuesses(t *testing.T) {
	tokens := int64(1000000)
	cache := modelCatalogCacheV1{RegionFacts: map[string]modelRegionFacts{
		"legacy":  {ContextLength: &tokens, Thinking: &thinkingFacts{Status: "supported", Default: "max", Levels: []string{"low", "max"}}},
		"unknown": {Thinking: &thinkingFacts{Status: "unsupported"}},
		"tiered":  {ContextLength: &tokens, ContextTiers: []contextTier{{Tokens: 200000, IsDefault: true}, {Tokens: 1000000}}},
	}}
	loaded := trustedLoadedCatalogFacts(cache)
	if loaded["legacy"].ContextLength != nil || loaded["legacy"].Thinking.Default != "" || loaded["unknown"].Thinking.Status != "unknown" {
		t.Fatalf("legacy guesses promoted: %+v", loaded)
	}
	fact := effectiveContextFacts("tiered", loaded["tiered"])
	if fact.ContextLength == nil || *fact.ContextLength != 200000 {
		t.Fatal("explicit legacy default evidence was discarded")
	}
	cache.FactsVersion = modelCatalogFactsVersion
	current := trustedLoadedCatalogFacts(cache)
	if current["legacy"].ContextLength == nil || current["legacy"].Thinking.Default != "max" {
		t.Fatal("current version explicit evidence was lost")
	}
}

func TestContextContractAmbiguousDefaultsStayUnknown(t *testing.T) {
	if _, ok := contextDefaultTokens([]contextTier{{Tokens: 200000, IsDefault: true}, {Tokens: 1000000, IsDefault: true}}); ok {
		t.Fatal("conflicting context defaults were guessed")
	}
	thinking := parseThinkingFacts(&qoderThinkingConfigWire{Enabled: &qoderThinkingOptionWire{Efforts: map[string]qoderThinkingEffortWire{"low": {IsDefault: true}, "max": {IsDefault: true}}}}, true)
	if thinking.Default != "" {
		t.Fatalf("conflicting reasoning defaults were guessed: %+v", thinking)
	}
}
