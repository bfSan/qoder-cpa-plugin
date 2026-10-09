// models.go implements the ModelProvider capability: static and per-auth
// model lists, dynamic model discovery via the upstream models API, alias
// reverse resolution (client-facing alias → upstream model id), and the
// host-config oauth-excluded-models filter.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// wbModels is the static fallback model list for QoderWork. Kept in sync with
// the union of the upstream chat scenes as of 2026-10. Two regions exist and
// they do NOT serve the same keys: the CN gateway (qoder.com.cn) advertises
// q37fmodel and gm51model, while the Intl gateway (qoder.com) advertises the
// ultimate/performance/efficient tiers instead. So a discovery failure falls
// back to this union rather than to one region's list, and no entry here may be
// dropped just because the other region lacks it.
//
// (History: this comment used to claim qmodel_preview / q36fmodel / gm51model
// were "retired upstream". That was wrong for gm51model — it is simply CN-only,
// and deleting it here or its alias in the host config removes a working model.
// qmodel_preview / q36fmodel are genuinely unreachable keys.)
//
// Dynamic refresh via /algo/api/v2/model/list replaces this at runtime when an
// account is present. Aliases use the qoder/ prefix in AuthAttributes; bare IDs
// work too.
func wbModels() []pluginapi.ModelInfo {
	return []pluginapi.ModelInfo{
		{ID: "auto", Name: "Auto", ContextLength: 200000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "ultimate", Name: "Ultimate", ContextLength: 1000000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "performance", Name: "Performance", ContextLength: 1000000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "efficient", Name: "Efficient", ContextLength: 200000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "qmodel_38max", Name: "Qwen3.8-Max", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "qfmodel", Name: "Qwen3.8-Flash", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "qmodel_latest", Name: "Qwen3.7-Max", ContextLength: 1000000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "qmodel", Name: "Qwen3.7-Plus", ContextLength: 1000000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "q37fmodel", Name: "Qwen3.7-Flash", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "kmodel_latest", Name: "Kimi-K3", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "kmodel", Name: "Kimi-K2.8-Preview", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "gmodel", Name: "GLM-5.3", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "gfmodel", Name: "GLM-5.3-Flash", ContextLength: 1000000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "gm51model", Name: "GLM-5.2", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "dmodel", Name: "DeepSeek-V4-Pro", ContextLength: 1000000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "dfmodel", Name: "DeepSeek-Flash", ContextLength: 1000000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "mmodel", Name: "MiniMax-M3", ContextLength: 1000000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
	}
}

// dynamicModelEntry is one account's cached discovery result.
type dynamicModelEntry struct {
	models  []pluginapi.ModelInfo
	factors map[string]float64
	// region caches the per-model capability facts (reasoning tiers, context
	// tiers) that pluginapi.ModelInfo cannot carry. It is stored next to the
	// models rather than in a provider-wide map because the CN and Intl
	// gateways advertise different tiers for the same model ID.
	region  map[string]modelRegionFacts
	fetched time.Time
}

// dynamicModelsCache holds discovery results keyed by account.
//
// It MUST be keyed per account: the CN and Intl gateways advertise different
// chat keys (see wbModels), so a single shared slot made whichever account
// happened to be discovered first answer for every other one. That surfaced as
// an intermittently "unknown provider for model" on a perfectly valid alias —
// e.g. gm51model resolved while the CN list was cached, then 400'd once the
// Intl list replaced it — and it made the panel show one region's catalog for
// both accounts. The empty key is a synthetic slot used by unit tests and as
// the last-resort fallback.
var dynamicModelsCache struct {
	sync.RWMutex
	byAccount map[string]dynamicModelEntry
}

// accountCacheKey identifies the account a discovery result belongs to. The UID
// is stable across token refreshes; the token prefix only backs up files that
// predate it. Region is folded in so two files sharing a UID can never collide.
func accountCacheKey(sa *storedAuth) string {
	if sa == nil {
		return ""
	}
	key := strings.TrimSpace(sa.Account.UID)
	if key == "" {
		n := len(sa.Auth.AccessToken)
		if n > 16 {
			n = 16
		}
		key = sa.Auth.AccessToken[:n]
	}
	if key == "" {
		return ""
	}
	return authRegion(sa) + "\x00" + key
}

func cachedDynamicModelsFor(key string) ([]pluginapi.ModelInfo, bool) {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	e, ok := dynamicModelsCache.byAccount[key]
	if !ok || len(e.models) == 0 || time.Since(e.fetched) >= dynamicModelsCacheTTL {
		return nil, false
	}
	return e.models, true
}

// cachedDynamicModelsAll unions every cached account (plus the synthetic slot)
// into the provider-wide catalog. This is what model.static should report: the
// panel and the host both need to see every key the provider can serve, not
// whichever region was pulled last.
func cachedDynamicModelsAll() ([]pluginapi.ModelInfo, bool) {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	seen := make(map[string]struct{})
	var out []pluginapi.ModelInfo
	for _, e := range dynamicModelsCache.byAccount {
		if len(e.models) == 0 || time.Since(e.fetched) >= dynamicModelsCacheTTL {
			continue
		}
		for _, m := range e.models {
			if _, dup := seen[m.ID]; dup {
				continue
			}
			seen[m.ID] = struct{}{}
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// storeDynamicModels seeds the synthetic slot, which stands in for "the last
// account to answer" in unit tests and the provider-wide fallback.
func storeDynamicModels(models []pluginapi.ModelInfo) {
	storeDynamicModelsFor("", models, nil)
}

func storeDynamicModelsFor(key string, models []pluginapi.ModelInfo, factors map[string]float64) {
	storeDynamicModelsForWithRegions(key, models, factors, nil)
}

// storeDynamicModelsForWithRegions is storeDynamicModelsFor plus the per-model
// capability facts. Kept separate so the many existing callers and tests that
// only have a model list are unaffected.
func storeDynamicModelsForWithRegions(key string, models []pluginapi.ModelInfo, factors map[string]float64, region map[string]modelRegionFacts) {
	dynamicModelsCache.Lock()
	if dynamicModelsCache.byAccount == nil {
		dynamicModelsCache.byAccount = make(map[string]dynamicModelEntry)
	}
	dynamicModelsCache.byAccount[key] = dynamicModelEntry{models: models, factors: factors, region: region, fetched: time.Now()}
	dynamicModelsCache.Unlock()
}

// cachedRegionFactsFor returns one account's capability facts. The second
// result is false when that account has no cached discovery, which callers must
// treat as "not loaded" rather than "no models".
func cachedRegionFactsFor(key string) (map[string]modelRegionFacts, bool) {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	e, ok := dynamicModelsCache.byAccount[key]
	if !ok || len(e.models) == 0 || time.Since(e.fetched) >= dynamicModelsCacheTTL {
		return nil, false
	}
	return e.region, true
}

// cachedRegionKeysByRegion lists the cache keys belonging to one region. Keys
// are "<region>\x00<uid>" (see accountCacheKey), so the prefix decides.
func cachedRegionKeysByRegion(region string) []string {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	prefix := region + "\x00"
	keys := make([]string, 0, len(dynamicModelsCache.byAccount))
	for key := range dynamicModelsCache.byAccount {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// setDynamicModelsCacheForTest replaces the whole cache and returns a restore
// func, so tests can seed one account without reaching into the map's internals.
func setDynamicModelsCacheForTest(entries map[string][]pluginapi.ModelInfo) func() {
	dynamicModelsCache.Lock()
	prev := dynamicModelsCache.byAccount
	next := make(map[string]dynamicModelEntry, len(entries))
	for k, v := range entries {
		next[k] = dynamicModelEntry{models: v, fetched: time.Now()}
	}
	dynamicModelsCache.byAccount = next
	dynamicModelsCache.Unlock()
	return func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.byAccount = prev
		dynamicModelsCache.Unlock()
	}
}

func storePriceFactors(factors map[string]float64) {
	dynamicModelsCache.Lock()
	if dynamicModelsCache.byAccount == nil {
		dynamicModelsCache.byAccount = make(map[string]dynamicModelEntry)
	}
	e := dynamicModelsCache.byAccount[""]
	e.factors = factors
	dynamicModelsCache.byAccount[""] = e
	dynamicModelsCache.Unlock()
}

// staticPriceFactors mirrors the upstream chat-scene price_factor values
// observed 2026-10. Used until a successful dynamic fetch overrides them, so the
// panel can still show rates when the models API is unreachable. Every key here
// is live on at least one region (q37fmodel / gm51model are CN-only), so none is
// dead weight.
var staticPriceFactors = map[string]float64{
	"auto":          0.5,
	"qmodel_38max":  0.5,
	"qfmodel":       0,
	"qmodel_latest": 0.5,
	"qmodel":        0.1,
	"q37fmodel":     0.1,
	"dmodel":        0.5,
	"dfmodel":       0.1,
	"gmodel":        0.8,
	"gfmodel":       0.1,
	"gm51model":     0.6,
	"kmodel_latest": 1.4,
	"kmodel":        0.8,
	"mmodel":        0.2,
}

// priceFactorForModel reports the billing multiplier for a model id.
// Dynamic values win; static table is the fallback. ok=false means unknown.
func priceFactorForModel(id string) (factor float64, ok bool) {
	dynamicModelsCache.RLock()
	for _, e := range dynamicModelsCache.byAccount {
		if f, hit := e.factors[id]; hit {
			dynamicModelsCache.RUnlock()
			return f, true
		}
	}
	dynamicModelsCache.RUnlock()
	factor, ok = staticPriceFactors[id]
	return factor, ok
}

func fetchDynamicModels() []pluginapi.ModelInfo {
	models, _ := fetchDynamicModelsForce(false)
	return models
}

// fetchDynamicModelsForce pulls every account's upstream model list and returns
// the union, reporting the last discovery error so a forced refresh can tell the
// operator the pull failed instead of silently serving the cached list as if it
// were fresh.
//
// force skips the cache read the way an explicit panel refresh wants: without
// it every call inside the 5 minute TTL answers from memory, so pressing
// refresh showed the same list no matter what upstream had published.
//
// The result is the union across accounts because this backs model.static, which
// must describe everything the provider can serve. Each account's own result is
// stored under its own key, so a per-auth query never inherits another region's
// catalog.
func fetchDynamicModelsForce(force bool) ([]pluginapi.ModelInfo, error) {
	if !force {
		if models, ok := cachedDynamicModelsAll(); ok {
			return models, nil
		}
	}
	models := wbModels()
	files, err := hostAuthListFiles()
	if err != nil || len(files) == 0 {
		return models, err
	}
	// Strict filename-prefix match — same filter as host_auth.go hostAuthList.
	// (Earlier code also matched files containing "codebuddy" anywhere, which
	// would wrongly include workbuddy-*.json auths here and cause us to call
	// the qoderwork models API with a workbuddy token.)
	prefix := providerName + "-"
	var lastErr error
	seen := make(map[string]struct{})
	var out []pluginapi.ModelInfo
	pulled := false
	for _, f := range files {
		if !strings.HasPrefix(strings.ToLower(f.Name), prefix) {
			continue
		}
		raw, err := hostAuthGetByIndex(f.AuthIndex)
		if err != nil {
			lastErr = err
			continue
		}
		sa, err := parseStored(raw)
		if err != nil || sa == nil {
			if err != nil {
				lastErr = err
			}
			continue
		}
		dyn, err := callModelsAPI(sa)
		if err != nil {
			lastErr = err
			continue
		}
		if len(dyn) == 0 {
			continue
		}
		// callModelsAPI already stored this account's own entry (models and
		// price factors) under its key; union it into the provider-wide list.
		pulled = true
		for _, m := range dyn {
			if _, dup := seen[m.ID]; dup {
				continue
			}
			seen[m.ID] = struct{}{}
			out = append(out, m)
		}
	}
	if pulled && len(out) > 0 {
		return out, nil
	}
	return models, lastErr
}

// fetchDynamicModelsFromStorage answers model.for_auth for one account. It must
// only ever use that account's own cached catalog: the CN and Intl gateways
// advertise different keys, so sharing one slot handed the Intl account the CN
// list and made valid keys 400 as "unknown provider for model".
func fetchDynamicModelsFromStorage(storageJSON []byte) []pluginapi.ModelInfo {
	sa, err := parseStored(storageJSON)
	if err != nil || sa == nil {
		return fetchDynamicModels()
	}
	key := accountCacheKey(sa)
	if models, ok := cachedDynamicModelsFor(key); ok {
		return models
	}
	if dyn, err := callModelsAPI(sa); err == nil && len(dyn) > 0 {
		return dyn
	}
	// Discovery failed for this account: reuse its stale entry if we have one
	// rather than another region's list, then fall back to the union.
	dynamicModelsCache.RLock()
	stale, ok := dynamicModelsCache.byAccount[key]
	dynamicModelsCache.RUnlock()
	if ok && len(stale.models) > 0 {
		return stale.models
	}
	if models, ok := cachedDynamicModelsAll(); ok {
		return models
	}
	return wbModels()
}

// fetchDynamicModels calls the QoderWork API to get the latest model list.
// Falls back to the hardcoded list on any error.
// callModelsAPI GETs /algo/api/v2/model/list from the QoderWork gateway
// with COSY signing (same as inference). Returns plain JSON (not QoderEncoding).
// Falls back to wbModels() on any error.
func callModelsAPI(sa *storedAuth) ([]pluginapi.ModelInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// model/list works with an empty JSON object body (verified in
	// reference_impl.py). The COSY signature covers the request body, so
	// the body we sign MUST be the body we send: the gateway recomputes
	// md5 over the bytes it actually received, and an unsigned/absent body
	// against a signed "{}" fails with 403 "Signature invalid" (issue #8:
	// 3/3 repros; body==signed-body passes 3/3). A GET with a body is
	// unusual but legal, and matches how the chat path signs+sends.
	encodedBody := qoderEncode([]byte("{}"))
	rawURL := endpointModelsFor(sa) // includes ?Encode=1
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, strings.NewReader(encodedBody))
	if err != nil {
		return nil, err
	}
	if err := applyCosyHeaders(req, sa, encodedBody, rawURL, "", false); err != nil {
		return nil, fmt.Errorf("cosy sign: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models API status %d", resp.StatusCode)
	}
	// Response is plain JSON: {"chat":[{key,display_name,...}], "developer":[...], ...}
	var apiResp map[string]json.RawMessage
	if err := json.Unmarshal(resp.Body, &apiResp); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	// Prefer the "chat" scene (matches our inference use case).
	chatRaw, ok := apiResp["chat"]
	if !ok {
		return nil, fmt.Errorf("no chat scene in models response")
	}
	var models []struct {
		Key            string  `json:"key"`
		DisplayName    string  `json:"display_name"`
		Enable         bool    `json:"enable"`
		IsReasoning    bool    `json:"is_reasoning"`
		IsVL           bool    `json:"is_vl"`
		MaxInputTokens int64   `json:"max_input_tokens"`
		PriceFactor    float64 `json:"price_factor"`
		// The gateway publishes reasoning and context tiers per model; without
		// these two the panel could only show "未上报" and a bare token count.
		Thinking      *qoderThinkingConfigWire        `json:"thinking_config"`
		ContextConfig map[string]qoderContextTierWire `json:"context_config"`
	}
	if err := json.Unmarshal(chatRaw, &models); err != nil {
		return nil, fmt.Errorf("chat scene parse: %w", err)
	}
	var out []pluginapi.ModelInfo
	factors := make(map[string]float64, len(models))
	regions := make(map[string]modelRegionFacts, len(models))
	for _, m := range models {
		if !m.Enable {
			continue
		}
		factors[m.Key] = m.PriceFactor
		ctx2 := int64(180000)
		if m.MaxInputTokens > 0 {
			ctx2 = m.MaxInputTokens
		}
		tiers := parseContextTiers(m.ContextConfig)
		out = append(out, pluginapi.ModelInfo{
			ID:                         m.Key,
			Name:                       m.DisplayName,
			ContextLength:              ctx2,
			MaxCompletionTokens:        8192,
			OwnedBy:                    providerName,
			SupportedGenerationMethods: []string{"chat"},
		})
		// The advertised context length is the default tier when one is
		// marked: max_input_tokens is the ceiling the gateway accepts, while
		// the default tier is what it applies without an explicit choice, and
		// the panel labels that one as current.
		effectiveCtx := ctx2
		if tokens, ok := contextDefaultTokens(tiers); ok {
			effectiveCtx = tokens
		}
		factor := m.PriceFactor
		regions[m.Key] = modelRegionFacts{
			PriceFactor:   &factor,
			ContextLength: &effectiveCtx,
			ContextTiers:  tiers,
			Thinking:      parseThinkingFacts(m.Thinking, m.IsReasoning),
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no enabled chat models")
	}
	// Cache under this account's own key: the CN and Intl gateways advertise
	// different chat keys, so a shared slot would answer one region with the
	// other's catalog. The capability facts ride along for the same reason --
	// the tiers are per region too.
	storeDynamicModelsForWithRegions(accountCacheKey(sa), out, factors, regions)
	return out, nil
}

func cacheModelAliases(host pluginapi.HostConfigSummary) {
	entries := host.OAuthModelAlias[providerName]
	if len(entries) == 0 {
		// Host may key the channel case-insensitively; fall back to a scan.
		for channel, list := range host.OAuthModelAlias {
			if strings.EqualFold(strings.TrimSpace(channel), providerName) {
				entries = list
				break
			}
		}
	}
	byAlias := make(map[string]string, len(entries))
	for _, e := range entries {
		name := strings.TrimSpace(e.Name)
		alias := strings.TrimSpace(e.Alias)
		if name == "" || alias == "" || strings.EqualFold(name, alias) {
			continue
		}
		byAlias[strings.ToLower(alias)] = name
	}
	modelAliasCache.Lock()
	modelAliasCache.byAlias = byAlias
	modelAliasCache.Unlock()
}

// resolveUpstreamModel maps an aliased requested model back to the real
// upstream model ID. Returns the input unchanged when nothing matches.
func resolveUpstreamModel(model string, attributes map[string]string) string {
	m := strings.TrimSpace(model)
	if m == "" {
		return model
	}
	key := strings.ToLower(m)
	if name, ok := parseModelAliasAttribute(attributes)[key]; ok {
		return name
	}
	modelAliasCache.RLock()
	name, ok := modelAliasCache.byAlias[key]
	modelAliasCache.RUnlock()
	if ok {
		return name
	}
	return m
}

// parseModelAliasAttribute decodes a per-auth alias override from auth
// attributes. Accepts JSON ([{"name":...,"alias":...}] or {alias:name}) or
// comma-separated "alias=name" pairs.
func parseModelAliasAttribute(attributes map[string]string) map[string]string {
	if len(attributes) == 0 {
		return nil
	}
	raw := ""
	for _, k := range []string{"model_alias", "model-alias", "oauth-model-alias"} {
		if v := strings.TrimSpace(attributes[k]); v != "" {
			raw = v
			break
		}
	}
	if raw == "" {
		return nil
	}
	out := make(map[string]string)
	add := func(name, alias string) {
		name, alias = strings.TrimSpace(name), strings.TrimSpace(alias)
		if name != "" && alias != "" && !strings.EqualFold(name, alias) {
			out[strings.ToLower(alias)] = name
		}
	}
	if strings.HasPrefix(raw, "[") {
		var list []struct {
			Name  string `json:"name"`
			Alias string `json:"alias"`
		}
		if json.Unmarshal([]byte(raw), &list) == nil {
			for _, e := range list {
				add(e.Name, e.Alias)
			}
			return out
		}
	}
	if strings.HasPrefix(raw, "{") {
		var m map[string]string
		if json.Unmarshal([]byte(raw), &m) == nil {
			for alias, name := range m {
				add(name, alias)
			}
			return out
		}
	}
	for _, pair := range strings.Split(raw, ",") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) == 2 {
			add(kv[1], kv[0])
		}
	}
	return out
}

// filterExcludedModels removes models listed in oauth-excluded-models for
// the qoderwork provider. The host passes this config via HostConfigSummary.
func filterExcludedModels(models []pluginapi.ModelInfo, host pluginapi.HostConfigSummary) []pluginapi.ModelInfo {
	if len(host.ExcludedModels) == 0 {
		return models
	}
	// Try exact provider match, then case-insensitive scan.
	excluded := host.ExcludedModels[providerName]
	if len(excluded) == 0 {
		for channel, list := range host.ExcludedModels {
			if strings.EqualFold(strings.TrimSpace(channel), providerName) {
				excluded = list
				break
			}
		}
	}
	if len(excluded) == 0 {
		return models
	}
	excludeSet := make(map[string]struct{}, len(excluded))
	for _, m := range excluded {
		excludeSet[strings.ToLower(strings.TrimSpace(m))] = struct{}{}
	}
	// Use a fresh slice — models[:0] would alias the input's backing array,
	// which may be the dynamicModelsCache's own slice. Mutating it in place
	// would corrupt the cache for subsequent callers (P0 bug: after one
	// filterExcludedModels call, cache returns the filtered list as the
	// "full" list on the next fetch).
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		if _, skip := excludeSet[strings.ToLower(m.ID)]; skip {
			continue
		}
		out = append(out, m)
	}
	return out
}

// publishUsage reports one upstream attempt into CPAMP request monitoring.
// requestedModel is client-facing (may be alias); upstreamModel is resolved.

func handleModelStatic(raw []byte) ([]byte, error) {
	var req pluginapi.StaticModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	cacheModelAliases(req.Host)
	models := effectiveModelCatalog()
	models = sortModelsForCatalog(models)
	models = filterExcludedModels(models, req.Host)
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
}

func handleModelForAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	// Always return the plugin's canonical provider key. The host skips any
	// response whose Provider doesn't match the auth's provider, so echoing
	// req.AuthProvider back would silently drop the model list whenever the
	// auth file carries a non-canonical provider string.
	cacheModelAliases(req.Host)
	models := fetchDynamicModelsFromStorage(req.StorageJSON)
	models = applyModelOverlay(cloneModelInfos(models), loadedModelOverlayForRead())
	models = sortModelsForCatalog(models)
	models = filterExcludedModels(models, req.Host)
	models = filterCoolingModels(req, models)
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
}

// filterCoolingModels withholds the models this account is currently cooling.
//
// Why this exists: the plugin has always recorded per-(account, model)
// throttling for the panel (see cooldown.go), but model.for_auth ignored it, so
// CPA went on believing the pair was healthy. With routing.session-affinity
// enabled CPA pins a session to one account for the whole TTL, so a session kept
// being routed into a pair this plugin had already decided to throttle until the
// affinity expired, instead of failing over.
//
// Withholding the model is what makes CPA route around it. CPA registers this
// exact response against the auth (RegisterClient, keyed by the auth ID) and its
// selection loop skips any account whose registration does not carry the
// requested model (authSupportsRouteModel -> ClientSupportsModel in
// conductor_selection.go), an affinity-bound account included.
//
// Only the (account, model) pair is withheld, never the whole account. That is
// deliberate and is the same rule cooldown.go documents: cooling the whole
// credential would turn one degraded model into an auth-wide outage, which is
// worst exactly when a single Qoder auth is configured.
//
// The filter applies only to this response. The dynamic cache and the panel's
// admin catalog keep the full list, because an operator has to see a cooling
// model in order to clear it.
func filterCoolingModels(req pluginapi.AuthModelRequest, models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	if len(models) == 0 {
		return models
	}
	authID := strings.TrimSpace(req.AuthID)
	if authID == "" {
		// Nothing scopes the cooldown without an auth ID, and matching against
		// every account's state could hide a healthy model.
		return models
	}
	cooling := coolingModelSetFor(authID)
	if len(cooling) == 0 {
		return models
	}
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		if _, skip := cooling[id]; skip {
			continue
		}
		// Cooldowns are keyed by the routing model while this response carries
		// catalog IDs, and CPA rewrites aliases between the two (the client asks
		// for qoder-auto, upstream is auto). Try both the raw ID and its
		// resolved upstream name. When neither matches the model is kept: a
		// missed filter only preserves today's behaviour, whereas a false
		// positive would make a working model disappear from the catalog.
		matched := false
		for _, candidate := range []string{resolveUpstreamModel(id, req.Attributes), stripProviderPrefix(id)} {
			if candidate == "" {
				continue
			}
			if _, skip := cooling[candidate]; skip {
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		out = append(out, model)
	}
	return out
}
