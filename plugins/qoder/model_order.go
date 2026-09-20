// model_order.go provides the operator-facing catalog order:
//  1. aggregate/default presets,
//  2. GPT-family models,
//  3. remaining families, alphabetical within each family.
//
// The ordering is shape-based so new upstream models fall into a sensible
// group without a hard-coded catalog.
package main

import (
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var aggregateModelIDs = map[string]struct{}{
	"auto":           {},
	"default":        {},
	"balance":        {},
	"balanced-model": {},
	"fast-model":     {},
	"deep-model":     {},
	"ultimate":       {},
	"performance":    {},
	"efficient":      {},
}

type modelSortKey struct {
	group  int
	family string
	id     string
}

func modelSortKeyFor(id string) modelSortKey {
	id = strings.TrimSpace(id)
	lower := strings.ToLower(id)
	if _, ok := aggregateModelIDs[lower]; ok {
		return modelSortKey{group: 0, family: "", id: lower}
	}
	family := modelFamily(lower)
	if family == "gpt" {
		return modelSortKey{group: 1, family: family, id: lower}
	}
	return modelSortKey{group: 2, family: family, id: lower}
}

func modelFamily(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return ""
	}
	if strings.HasPrefix(id, "gpt-") || id == "gpt" {
		return "gpt"
	}
	if strings.HasPrefix(id, "qwen") || strings.HasPrefix(id, "qmodel") || strings.HasPrefix(id, "qfmodel") {
		return "qwen"
	}
	if strings.HasPrefix(id, "kimi") || strings.HasPrefix(id, "kmodel") {
		return "kimi"
	}
	if strings.HasPrefix(id, "glm") || strings.HasPrefix(id, "gmodel") || strings.HasPrefix(id, "gfmodel") {
		return "glm"
	}
	if strings.HasPrefix(id, "deepseek") || strings.HasPrefix(id, "dmodel") {
		return "deepseek"
	}
	if strings.HasPrefix(id, "minimax") || strings.HasPrefix(id, "mmodel") {
		return "minimax"
	}
	if strings.HasPrefix(id, "claude") {
		return "claude"
	}
	if strings.HasPrefix(id, "gemini") {
		return "gemini"
	}
	return id
}

func sortModelsForCatalog(models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	if len(models) < 2 {
		return models
	}
	out := cloneModelInfos(models)
	sort.SliceStable(out, func(i, j int) bool {
		a := modelSortKeyFor(out[i].ID)
		b := modelSortKeyFor(out[j].ID)
		if a.group != b.group {
			return a.group < b.group
		}
		if a.family != b.family {
			return a.family < b.family
		}
		if a.id != b.id {
			return a.id < b.id
		}
		return false
	})
	return out
}
