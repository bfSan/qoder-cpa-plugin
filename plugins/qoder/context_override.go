// Context overrides remain global configuration proposals, but are validated
// against the intersection of known account catalogs and rechecked per request.
package main

import (
	"sort"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

// maxContextWindowValue 是覆盖值的硬上限。比它更大的值只可能是手滑或攻击，
// 不是真实档位（上游最大 1M，留两个数量级余量）。
const maxContextWindowValue int64 = 100_000_000

var (
	contextOverrideMu sync.RWMutex
	// contextOverrides 按模型 ID 存生效窗口。仅在 configure() 里整体替换，
	// 保证读侧永远看到一份完整快照。
	contextOverrides = map[string]int64{}
)

// contextOverrideForModel 返回该模型的覆盖值（若有）。
func contextOverrideForModel(id string) (int64, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return 0, false
	}
	contextOverrideMu.RLock()
	value, ok := contextOverrides[id]
	contextOverrideMu.RUnlock()
	return value, ok && value > 0
}

// currentContextOverrides 返回覆盖表的一份拷贝，供管理端点与面板读取。
func currentContextOverrides() map[string]int64 {
	contextOverrideMu.RLock()
	out := make(map[string]int64, len(contextOverrides))
	for id, value := range contextOverrides {
		out[id] = value
	}
	contextOverrideMu.RUnlock()
	return out
}

// setContextOverride 写入或清除一个覆盖值。value 为 nil 表示清除（回到上游默认档）。
// 调用方负责校验档位合法性；这里只做边界与规范化。
func setContextOverride(id string, value *int64) map[string]int64 {
	id = strings.TrimSpace(id)
	contextOverrideMu.Lock()
	defer contextOverrideMu.Unlock()
	next := make(map[string]int64, len(contextOverrides)+1)
	for k, v := range contextOverrides {
		next[k] = v
	}
	if id == "" {
		return contextOverrides
	}
	if value == nil || *value <= 0 {
		delete(next, id)
	} else {
		next[id] = *value
	}
	contextOverrides = next
	out := make(map[string]int64, len(next))
	for k, v := range next {
		out[k] = v
	}
	return out
}

// replaceContextOverrides commits a validated configuration snapshot. The
// configure caller preserves an absent key; an explicit empty map clears it.
func replaceContextOverrides(next map[string]int64) {
	contextOverrideMu.Lock()
	if len(next) == 0 {
		contextOverrides = map[string]int64{}
	} else {
		clean := make(map[string]int64, len(next))
		for id, value := range next {
			id = strings.TrimSpace(id)
			if id == "" || value <= 0 || value > maxContextWindowValue {
				continue
			}
			clean[id] = value
		}
		contextOverrides = clean
	}
	contextOverrideMu.Unlock()
}

// normalizedContextOverrideConfig 解析 config.yaml 里的 model_context 映射。
//
// 形状是 {模型ID: 窗口token数}。节点缺失（Kind==0）返回 nil，与"显式空映射"
// 区分开：前者表示这份配置没提这件事，调用方应保留现状；后者是明确要求清空。
func normalizedContextOverrideConfig(node yaml.Node) (map[string]int64, error) {
	if node.Kind == 0 {
		return nil, nil
	}
	if node.Kind == yaml.ScalarNode && node.Tag == "!!null" {
		return nil, nil
	}
	if node.Kind != yaml.MappingNode || node.Tag != "!!map" {
		return nil, &modelConfigError{field: "model_context", msg: "must be a map of model ID to positive integer"}
	}
	out := make(map[string]int64, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
			return nil, &modelConfigError{field: "model_context", msg: "keys must be strings"}
		}
		id := strings.TrimSpace(key.Value)
		if id == "" || len(id) > maxDiscoveredModelIDBytes {
			return nil, &modelConfigError{field: "model_context", msg: "model IDs must be non-empty and bounded"}
		}
		if _, dup := out[id]; dup {
			return nil, &modelConfigError{field: "model_context", msg: "model IDs must not be duplicated"}
		}
		if value.Kind != yaml.ScalarNode || value.Tag != "!!int" {
			return nil, &modelConfigError{field: "model_context", msg: "values must be positive integers"}
		}
		var n int64
		if err := value.Decode(&n); err != nil || n <= 0 || n > maxContextWindowValue {
			return nil, &modelConfigError{field: "model_context", msg: "values must be positive and within the supported maximum"}
		}
		out[id] = n
	}
	return out, nil
}

// supportedContextTiersFor returns the intersection, never the union, of all
// known accounts that serve this ID. An account with missing tier facts vetoes
// a global override. Last-good catalogs still constrain validation after TTL.
func supportedContextTiersFor(id string) []int64 {
	id = strings.TrimSpace(id)
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	var common map[int64]bool
	for _, entry := range dynamicModelsCache.byAccount {
		serves := false
		for _, model := range entry.models {
			if model.ID == id {
				serves = true
				break
			}
		}
		if !serves {
			continue
		}
		offered := map[int64]bool{}
		for _, tier := range entry.region[id].ContextTiers {
			if tier.Tokens > 0 && tier.Tokens <= maxContextWindowValue {
				offered[tier.Tokens] = true
			}
		}
		if common == nil {
			common = offered
		} else {
			for value := range common {
				if !offered[value] {
					delete(common, value)
				}
			}
		}
	}
	out := make([]int64, 0, len(common))
	for value := range common {
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// effectiveContextFacts is the single metadata/body resolver. Cache records
// remain raw; a configuration reload therefore never bakes overrides into disk.
func effectiveContextFacts(id string, raw modelRegionFacts) modelRegionFacts {
	fact := cloneRegionFacts(raw)
	fact.ContextOverride = false
	fact.ContextRequested = nil
	fact.ContextStatus = "metadata_fallback"
	if tokens, ok := contextDefaultTokens(fact.ContextTiers); ok {
		fact.ContextStatus = "default"
		fact.ContextLength = &tokens
	} else if len(fact.ContextTiers) > 0 || fact.ContextLength == nil || *fact.ContextLength <= 0 || *fact.ContextLength > maxContextWindowValue {
		fact.ContextLength = nil
		fact.ContextStatus = "unknown"
	}
	if value, ok := contextOverrideForModel(id); ok {
		fact.ContextRequested = &value
		allowed := false
		for _, tier := range fact.ContextTiers {
			if tier.Tokens == value {
				allowed = true
				break
			}
		}
		if allowed {
			fact.ContextLength = &value
			fact.ContextOverride = true
			fact.ContextStatus = "override"
		} else {
			fact.ContextStatus = "override_rejected"
		}
	}
	return fact
}

// effectiveModelsForAccount applies only this account's own last-good facts.
// Models without context evidence advertise unknown (0), not another account's
// entitlement or an unverified million-token static ceiling.
func effectiveModelsForAccount(models []pluginapi.ModelInfo, facts map[string]modelRegionFacts) []pluginapi.ModelInfo {
	out := cloneModelInfos(models)
	for i := range out {
		fact, ok := facts[out[i].ID]
		if !ok {
			out[i].ContextLength = 0
			continue
		}
		effective := effectiveContextFacts(out[i].ID, fact)
		out[i].ContextLength = 0
		if effective.ContextLength != nil {
			out[i].ContextLength = *effective.ContextLength
		}
	}
	return out
}

// contextTierAllowed validates the common set of known account catalogs.
func contextTierAllowed(id string, tokens int64) bool {
	for _, allowed := range supportedContextTiersFor(id) {
		if allowed == tokens {
			return true
		}
	}
	return false
}

// handleModelContextWrite validates POST /models/context without mutating live
// state. {id,context_length:null} proposes deletion. Host PATCH is the sole
// persistence boundary and configure() is the sole production commit path.
func handleModelContextWrite(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		ID            string `json:"id"`
		Model         string `json:"model"`
		ContextLength *int64 `json:"context_length"`
	}
	if len(req.Body) > 0 {
		if err := jsonUnmarshal(req.Body, &body); err != nil {
			return map[string]any{"success": false, "error": "invalid json body"}
		}
	}
	id := strings.TrimSpace(body.ID)
	if id == "" {
		id = strings.TrimSpace(body.Model)
	}
	if id == "" {
		return map[string]any{"success": false, "error": "id is required"}
	}
	if body.ContextLength != nil {
		value := *body.ContextLength
		if value <= 0 || value > maxContextWindowValue {
			return map[string]any{"success": false, "error": "context_length is out of range"}
		}
		// 只有在拿到该模型的档位表时才校验；目录还没加载时无法判断，此时
		// 放行会让面板与后端各说一套，所以宁可拒绝并让操作者先刷新目录。
		tiers := supportedContextTiersFor(id)
		if len(tiers) == 0 {
			return map[string]any{
				"success": false,
				"error":   "no context tiers reported for this model yet; refresh the catalog first",
			}
		}
		if !contextTierAllowed(id, value) {
			return map[string]any{
				"success":         false,
				"error":           "context_length is not common to all known accounts serving this model",
				"context_options": tiers,
			}
		}
	}
	// Validation is deliberately side-effect free. Only host config PATCH followed
	// by configure() commits; a failed PATCH cannot change live request behavior.
	next := currentContextOverrides()
	if body.ContextLength == nil {
		delete(next, id)
	} else {
		next[id] = *body.ContextLength
	}
	return map[string]any{
		"success":         true,
		"id":              id,
		"context_length":  body.ContextLength,
		"model_context":   next,
		"context_options": supportedContextTiersFor(id),
		"persistent":      false,
		"proposal":        true,
		"applied":         false,
	}
}

// contextOverrideStates separates configured intent from runtime use.
func contextOverrideStates() map[string]map[string]any {
	states := map[string]map[string]any{}
	for id, value := range currentContextOverrides() {
		options := supportedContextTiersFor(id)
		status := "unverified_or_incompatible"
		for _, option := range options {
			if value == option {
				status = "compatible_known_accounts"
				break
			}
		}
		states[id] = map[string]any{"requested": value, "status": status, "context_options": options}
	}
	return states
}
