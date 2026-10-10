// context_override.go 保存「操作者为某个模型手工选定的上下文档位」。
//
// 为什么需要它：上游给每个模型都标了一个默认档（context_config 里的
// is_default，实测 CN 区几乎全是 200K），插件照这个值发请求。但默认档只是
// 上游建议的起点，不是上限 —— 实测 CN 账号发 ~500K token 的请求仍然返回 200，
// 说明网关接受远大于默认档的窗口。于是操作者需要能自己选一档：默认 200K 对
// 长上下文任务是白扔能力，而全量放开又会让每个请求都按 1M 准备。
//
// 语义与 WorkBuddy 的同名机制保持一致（两边本就共享面板布局）：
//   - 覆盖值按【模型 ID】存，不区分区域 —— 面板一列展示两区，让同一模型在两区
//     用同一个值，操作者不必理解"这个模型 CN 是 200K、Intl 是 1M"这类上游差异。
//   - 覆盖是显式的、可清除的。没有覆盖时完全沿用上游默认档，行为与加这个功能前
//     一模一样。
//   - 覆盖值必须是该模型自己上报过的档位之一。上游的档位表是权威的：接受一个
//     表外的值等于替上游承诺一个它没答应的窗口。
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

// replaceContextOverrides 整体替换覆盖表（configure 用）。传入 nil 表示清空，
// 这样配置里删掉 model_context 键就能回到"全部走上游默认档"。
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

// supportedContextTiersFor 返回该模型在任一区域上报过的档位，升序去重。
//
// 面板要把这些档位画成可点击的选项，所以顺序必须稳定：上游的 context_config
// 是 map，遍历顺序随机，直接用会让每次刷新后按钮顺序都在跳。
func supportedContextTiersFor(id string) []int64 {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	seen := map[int64]bool{}
	// 两个区域都要看：同一模型在 CN 与 Intl 上报的档位可能不同（实测 CN 的
	// dmodel 默认 200K、Intl 是 1M），面板一列展示两区，选项取并集才不会让
	// 操作者在某一区看不到另一区已有的档位。
	for _, region := range []string{regionCN, regionIntl} {
		for _, key := range cachedRegionKeysByRegion(region) {
			facts, ok := cachedRegionFactsFor(key)
			if !ok {
				continue
			}
			fact, found := facts[id]
			if !found {
				continue
			}
			for _, tier := range fact.ContextTiers {
				if tier.Tokens > 0 && tier.Tokens <= maxContextWindowValue {
					seen[tier.Tokens] = true
				}
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]int64, 0, len(seen))
	for tokens := range seen {
		out = append(out, tokens)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// contextTierAllowed 判断某个值是否是该模型上报过的档位之一。
// 覆盖值必须落在档位表里：表外的值等于替上游承诺一个它没答应的窗口。
func contextTierAllowed(id string, tokens int64) bool {
	for _, allowed := range supportedContextTiersFor(id) {
		if allowed == tokens {
			return true
		}
	}
	return false
}

// handleModelContextWrite 处理 POST /models/context。
//
// body: {id, context_length}。context_length 省略或为 null 表示清除覆盖，
// 回到上游默认档。写入前校验：值必须落在该模型上报过的档位表里。
//
// 为什么拒绝表外的值：面板把档位画成可点的按钮，操作者能选的就是上游给的那几档；
// 后端再放行一个表外的数，等于让面板显示一个上游从未答应的窗口。上游遇到不认识的
// 档位会回落到自己的限制，而面板仍显示那个大数 —— 正是这个功能要消除的误导。
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
				"error":           "context_length is not a supported tier for this model",
				"context_options": tiers,
			}
		}
	}
	next := setContextOverride(id, body.ContextLength)
	return map[string]any{
		"success":        true,
		"id":             id,
		"context_length": body.ContextLength,
		"model_context":  next,
		"context_options": supportedContextTiersFor(id),
		"persistent":     true,
	}
}
