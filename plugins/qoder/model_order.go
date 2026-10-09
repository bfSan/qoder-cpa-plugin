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

// sortPanelCatalog orders the panel's catalog.
//
// adminModelCatalog already applied the operator overlay, which puts Order at
// the front and appends hidden entries last. Running sortModelsForCatalog over
// that would overwrite both: the operator's drag order would snap back to the
// default grouping on every refresh, and hidden models would scatter back among
// the visible ones.
//
// So the only work left here is to sink hidden models. The default grouping is
// applied by the caller only when the operator has not ordered anything, via
// sortModelsForCatalog, which this function must not second-guess.
func sortPanelCatalog(models []pluginapi.ModelInfo, overlay modelOverlay) []pluginapi.ModelInfo {
	return sortHiddenLast(models, overlay)
}

// sortHiddenLast moves hidden models to the bottom, keeping everything else in
// the order it arrived in.
func sortHiddenLast(models []pluginapi.ModelInfo, overlay modelOverlay) []pluginapi.ModelInfo {
	if len(models) < 2 || len(overlay.Hide) == 0 {
		return models
	}
	hidden := make(map[string]struct{}, len(overlay.Hide))
	for _, id := range overlay.Hide {
		if id = strings.TrimSpace(id); id != "" {
			hidden[id] = struct{}{}
		}
	}
	if len(hidden) == 0 {
		return models
	}
	out := cloneModelInfos(models)
	// SliceStable keeps the relative order inside each band, so whatever ordered
	// the visible models earlier still decides how they read.
	sort.SliceStable(out, func(i, j int) bool {
		_, ih := hidden[strings.TrimSpace(out[i].ID)]
		_, jh := hidden[strings.TrimSpace(out[j].ID)]
		if ih == jh {
			return false
		}
		return !ih
	})
	return out
}
