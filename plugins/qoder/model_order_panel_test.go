package main

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func panelIDs(models []pluginapi.ModelInfo) []string {
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	return ids
}

func sameIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// 面板目录的真实调用顺序是 adminModelCatalog（应用 overlay）→ sortPanelCatalog。
// 操作者的拖拽顺序必须活过第二步：此前 sortModelsForCatalog 会把它整个推翻，
// 于是拖完一刷新就变回默认顺序，看起来像"排序没保存"。
func TestPanelCatalogHonoursOperatorOrder(t *testing.T) {
	base := []pluginapi.ModelInfo{
		{ID: "auto"}, {ID: "dmodel"}, {ID: "gmodel"}, {ID: "kimi-k3"},
	}
	overlay := modelOverlay{Order: []string{"kimi-k3", "auto", "gmodel", "dmodel"}}
	// 第一步：overlay 把 Order 里的模型按指定次序提到前面。
	served := applyModelOverlayForAdmin(base, overlay)
	// 第二步：面板排序不得推翻它。
	got := panelIDs(sortPanelCatalog(served, overlay))
	want := []string{"kimi-k3", "auto", "gmodel", "dmodel"}
	if !sameIDs(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// 没排过序时仍用默认分组顺序（由 sortModelsForCatalog 提供，面板排序不插手）。
func TestPanelCatalogFallsBackToDefaultOrder(t *testing.T) {
	base := []pluginapi.ModelInfo{
		{ID: "kimi-k3"}, {ID: "auto"}, {ID: "dmodel"},
	}
	overlay := modelOverlay{}
	served := applyModelOverlayForAdmin(base, overlay)
	got := panelIDs(sortPanelCatalog(sortModelsForCatalog(served), overlay))
	// auto 属于聚合档，永远排在最前。
	if len(got) == 0 || got[0] != "auto" {
		t.Fatalf("default order = %v, want auto first", got)
	}
}

// 隐藏的模型必须沉到底部，且不影响可见模型的先后。
func TestPanelCatalogSinksHiddenModels(t *testing.T) {
	base := []pluginapi.ModelInfo{
		{ID: "auto"}, {ID: "dmodel"}, {ID: "gmodel"}, {ID: "kimi-k3"},
	}
	overlay := modelOverlay{Hide: []string{"dmodel"}}
	served := applyModelOverlayForAdmin(base, overlay)
	got := panelIDs(sortPanelCatalog(sortModelsForCatalog(served), overlay))
	if got[len(got)-1] != "dmodel" {
		t.Fatalf("hidden model did not sink: %v", got)
	}
	// 可见部分保持默认分组：auto 在最前。
	if got[0] != "auto" {
		t.Fatalf("visible order changed: %v", got)
	}
	if len(got) != 4 {
		t.Fatalf("hidden model was dropped instead of sunk: %v", got)
	}
}

// 排序 + 隐藏同时存在时，两条规则都要生效：先按操作者顺序，再把隐藏项沉底。
func TestPanelCatalogCombinesOrderAndHidden(t *testing.T) {
	base := []pluginapi.ModelInfo{
		{ID: "auto"}, {ID: "dmodel"}, {ID: "gmodel"}, {ID: "kimi-k3"},
	}
	overlay := modelOverlay{
		Order: []string{"kimi-k3", "auto", "gmodel", "dmodel"},
		Hide:  []string{"gmodel"},
	}
	served := applyModelOverlayForAdmin(base, overlay)
	got := panelIDs(sortPanelCatalog(served, overlay))
	want := []string{"kimi-k3", "auto", "dmodel", "gmodel"}
	if !sameIDs(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// 隐藏列表里的 ID 不在目录中也必须安全。
func TestPanelCatalogIgnoresUnknownHiddenIDs(t *testing.T) {
	base := []pluginapi.ModelInfo{{ID: "auto"}, {ID: "dmodel"}}
	overlay := modelOverlay{Hide: []string{"not-in-catalog"}}
	got := panelIDs(sortPanelCatalog(applyModelOverlayForAdmin(base, overlay), overlay))
	if len(got) != 2 {
		t.Fatalf("catalog changed: %v", got)
	}
}

// 排序输入不能被就地修改：调用方可能还在用原切片。
func TestPanelCatalogDoesNotMutateInput(t *testing.T) {
	base := []pluginapi.ModelInfo{{ID: "auto"}, {ID: "dmodel"}, {ID: "kimi-k3"}}
	before := panelIDs(base)
	sortPanelCatalog(base, modelOverlay{Order: []string{"kimi-k3", "dmodel", "auto"}, Hide: []string{"dmodel"}})
	if after := panelIDs(base); !sameIDs(after, before) {
		t.Fatalf("input was mutated: %v -> %v", before, after)
	}
}
