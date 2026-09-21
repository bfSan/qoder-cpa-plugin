// model_config.go implements the plugin-owned model catalog overlay.
//
// The base catalog comes from the gateway (or the static fallback). This file
// applies three operator-controlled transforms on top of it:
//
//	hide  - remove model IDs from the client-facing catalog
//	order - pin model IDs to the front of the catalog
//	add   - append catalog entries that upstream did not report
//
// Hide is persisted through the plugin's `hidden_models` config. Order and add
// are process-local; both are intentionally cheap to rebuild after a restart.
package main

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const maxDiscoveredModelIDBytes = 256

type modelOverlay struct {
	Hide  []string `json:"hide"`
	Order []string `json:"order"`
	Add   []string `json:"add"`
}

type modelOverlayState struct {
	Overlay  modelOverlay `json:"overlay"`
	Revision int64        `json:"revision"`
}

var (
	modelOverlayMu           sync.RWMutex
	currentModelOverlayState = modelOverlayState{}
)

func setModelOverlayForTest(o modelOverlay) func() {
	modelOverlayMu.Lock()
	prev := currentModelOverlayState
	currentModelOverlayState.Overlay = o
	currentModelOverlayState.Revision++
	modelOverlayMu.Unlock()
	return func() {
		modelOverlayMu.Lock()
		currentModelOverlayState = prev
		modelOverlayMu.Unlock()
	}
}

func loadedModelOverlay() (modelOverlay, int64) {
	modelOverlayMu.RLock()
	defer modelOverlayMu.RUnlock()
	o := currentModelOverlayState.Overlay
	return modelOverlay{
		Hide:  append([]string(nil), o.Hide...),
		Order: append([]string(nil), o.Order...),
		Add:   append([]string(nil), o.Add...),
	}, currentModelOverlayState.Revision
}

func loadedModelOverlayForRead() modelOverlay {
	o, _ := loadedModelOverlay()
	return o
}

type modelConfigError struct {
	field string
	msg   string
}

func (e *modelConfigError) Error() string {
	if e.field == "" {
		return e.msg
	}
	return e.field + ": " + e.msg
}

func normalizeModelIDList(in []string, field string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		id := strings.TrimSpace(raw)
		if id == "" {
			return nil, &modelConfigError{field: field, msg: "entries must not be empty"}
		}
		if strings.IndexFunc(id, func(r rune) bool {
			return r == '\r' || r == '\n' || r == 0x85 || r == 0x2028 || r == 0x2029
		}) >= 0 {
			return nil, &modelConfigError{field: field, msg: "entries must be single-line strings"}
		}
		if len(id) > maxDiscoveredModelIDBytes {
			return nil, &modelConfigError{field: field, msg: "entry exceeds maximum ID length"}
		}
		if _, exists := seen[id]; exists {
			return nil, &modelConfigError{field: field, msg: "entries must not be duplicated"}
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

func storeModelOverlay(next modelOverlay) (modelOverlayState, error) {
	hide, err := normalizeModelIDList(next.Hide, "hide")
	if err != nil {
		return modelOverlayState{}, err
	}
	order, err := normalizeModelIDList(next.Order, "order")
	if err != nil {
		return modelOverlayState{}, err
	}
	add, err := normalizeModelIDList(next.Add, "add")
	if err != nil {
		return modelOverlayState{}, err
	}
	modelOverlayMu.Lock()
	defer modelOverlayMu.Unlock()
	currentModelOverlayState.Overlay = modelOverlay{Hide: hide, Order: order, Add: add}
	currentModelOverlayState.Revision++
	return modelOverlayState{
		Overlay: modelOverlay{
			Hide:  append([]string(nil), hide...),
			Order: append([]string(nil), order...),
			Add:   append([]string(nil), add...),
		},
		Revision: currentModelOverlayState.Revision,
	}, nil
}

func cloneModelInfos(models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	if models == nil {
		return nil
	}
	out := make([]pluginapi.ModelInfo, len(models))
	for i, m := range models {
		out[i] = m
		out[i].SupportedGenerationMethods = append([]string(nil), m.SupportedGenerationMethods...)
		out[i].SupportedParameters = append([]string(nil), m.SupportedParameters...)
		out[i].SupportedInputModalities = append([]string(nil), m.SupportedInputModalities...)
		out[i].SupportedOutputModalities = append([]string(nil), m.SupportedOutputModalities...)
		if m.Thinking != nil {
			thinking := *m.Thinking
			thinking.Levels = append([]string(nil), m.Thinking.Levels...)
			out[i].Thinking = &thinking
		}
	}
	return out
}

func defaultModelInfo(id, name string) pluginapi.ModelInfo {
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	if name == "" {
		name = id
	}
	return pluginapi.ModelInfo{
		ID:                         id,
		Name:                       name,
		OwnedBy:                    providerName,
		SupportedGenerationMethods: []string{"chat"},
	}
}

// applyModelOverlay applies hide/order/add to a base catalog. The input slice
// is never mutated. Pinned IDs come first in the operator's requested order,
// then the remaining base entries in their existing order, then additions.
func applyModelOverlay(base []pluginapi.ModelInfo, o modelOverlay) []pluginapi.ModelInfo {
	hidden := make(map[string]struct{}, len(o.Hide))
	for _, id := range o.Hide {
		hidden[strings.TrimSpace(id)] = struct{}{}
	}

	pinned := make(map[string]int, len(o.Order))
	for i, id := range o.Order {
		id = strings.TrimSpace(id)
		if _, dup := pinned[id]; dup {
			continue
		}
		pinned[id] = i
	}

	kept := make([]pluginapi.ModelInfo, 0, len(base)+len(o.Add))
	pinnedOut := make([]pluginapi.ModelInfo, 0, len(o.Order))
	for _, model := range base {
		id := strings.TrimSpace(model.ID)
		if _, gone := hidden[id]; gone {
			continue
		}
		if _, isPinned := pinned[id]; isPinned {
			pinnedOut = append(pinnedOut, model)
			continue
		}
		kept = append(kept, model)
	}
	sort.SliceStable(pinnedOut, func(i, j int) bool {
		return pinned[strings.TrimSpace(pinnedOut[i].ID)] < pinned[strings.TrimSpace(pinnedOut[j].ID)]
	})

	out := make([]pluginapi.ModelInfo, 0, len(pinnedOut)+len(kept)+len(o.Add))
	out = append(out, pinnedOut...)
	out = append(out, kept...)
	for _, id := range o.Add {
		id = strings.TrimSpace(id)
		if _, gone := hidden[id]; gone {
			continue
		}
		out = append(out, defaultModelInfo(id, ""))
	}
	return out
}

// applyModelOverlayForAdmin keeps hidden entries visible so the panel can
// restore them. Serving paths must call applyModelOverlay instead.
func applyModelOverlayForAdmin(base []pluginapi.ModelInfo, o modelOverlay) []pluginapi.ModelInfo {
	visible := applyModelOverlay(base, o)
	hidden := make(map[string]struct{}, len(o.Hide))
	for _, id := range o.Hide {
		hidden[strings.TrimSpace(id)] = struct{}{}
	}
	if len(hidden) == 0 {
		return visible
	}
	seen := make(map[string]struct{}, len(visible)+len(hidden))
	out := make([]pluginapi.ModelInfo, 0, len(visible)+len(hidden))
	for _, model := range visible {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, model)
	}
	for _, model := range base {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		if _, isHidden := hidden[id]; !isHidden {
			continue
		}
		if _, already := seen[id]; already {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, model)
	}
	return out
}

func baseModelCatalog() []pluginapi.ModelInfo {
	models := fetchDynamicModels()
	return cloneModelInfos(models)
}

func effectiveModelCatalog() []pluginapi.ModelInfo {
	return applyModelOverlay(baseModelCatalog(), loadedModelOverlayForRead())
}

func adminModelCatalog() []pluginapi.ModelInfo {
	return applyModelOverlayForAdmin(baseModelCatalog(), loadedModelOverlayForRead())
}

func overlayHidden(o modelOverlay, id string) bool {
	for _, hidden := range o.Hide {
		if strings.TrimSpace(hidden) == id {
			return true
		}
	}
	return false
}

func overlayAdded(o modelOverlay, id string) bool {
	for _, added := range o.Add {
		if strings.TrimSpace(added) == id {
			return true
		}
	}
	return false
}

func buildModelListQuery() map[string]any {
	models := sortModelsForCatalog(adminModelCatalog())
	overlay, revision := loadedModelOverlay()
	items := make([]map[string]any, 0, len(models))
	for i, m := range models {
		id := strings.TrimSpace(m.ID)
		item := map[string]any{
			"id":              id,
			"name":            m.Name,
			"displayName":     m.DisplayName,
			"position":        i,
			"hidden":          overlayHidden(overlay, id),
			"custom":          overlayAdded(overlay, id),
			"coolingAccounts": cooldownModelCount(id),
		}
		if factor, ok := priceFactorForModel(id); ok {
			item["priceFactor"] = factor
		}
		items = append(items, item)
	}
	return map[string]any{
		"models":           items,
		"count":            len(items),
		"source":           "dynamic",
		"overlay":          overlay,
		"revision":         revision,
		"persistent":       true,
		"persistentFields": []string{"hide"},
	}
}

func handleModelOverlayWrite(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		Overlay *modelOverlay `json:"overlay"`
		Hide    []string      `json:"hide"`
		Order   []string      `json:"order"`
		Add     []string      `json:"add"`
	}
	if len(req.Body) > 0 {
		if err := jsonUnmarshal(req.Body, &body); err != nil {
			return map[string]any{"success": false, "error": "invalid json body"}
		}
	}
	next := modelOverlay{Hide: body.Hide, Order: body.Order, Add: body.Add}
	if body.Overlay != nil {
		next = *body.Overlay
	}
	stored, err := storeModelOverlay(next)
	if err != nil {
		return map[string]any{"success": false, "error": err.Error()}
	}
	syncHiddenModelsConfig(stored.Overlay.Hide)
	return map[string]any{
		"success":          true,
		"overlay":          stored.Overlay,
		"revision":         stored.Revision,
		"persistent":       false,
		"persistentFields": []string{"hide"},
	}
}

func handleModelOverlayAction(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		Action string `json:"action"`
		ID     string `json:"id"`
		IDs    string `json:"ids"`
		Offset int    `json:"offset"`
	}
	if len(req.Body) > 0 {
		if err := jsonUnmarshal(req.Body, &body); err != nil {
			return map[string]any{"success": false, "error": "invalid json body"}
		}
	}
	current, _ := loadedModelOverlay()
	id := strings.TrimSpace(body.ID)
	if id == "" {
		id = strings.TrimSpace(body.IDs)
	}
	action := strings.ToLower(strings.TrimSpace(body.Action))
	switch action {
	case "hide":
		if id == "" {
			return map[string]any{"success": false, "error": "id is required"}
		}
		if !overlayHidden(current, id) {
			current.Hide = append(current.Hide, id)
		}
		current.Order = removeModelID(current.Order, id)
		current.Add = removeModelID(current.Add, id)
	case "restore":
		if id == "" {
			return map[string]any{"success": false, "error": "id is required"}
		}
		current.Hide = removeModelID(current.Hide, id)
	case "move":
		if id == "" {
			return map[string]any{"success": false, "error": "id is required"}
		}
		var err error
		current.Order, err = moveModelID(current.Order, id, body.Offset)
		if err != nil {
			return map[string]any{"success": false, "error": err.Error()}
		}
	case "add":
		if id == "" {
			return map[string]any{"success": false, "error": "id is required"}
		}
		if !overlayAdded(current, id) {
			current.Add = append(current.Add, id)
		}
		current.Hide = removeModelID(current.Hide, id)
	default:
		return map[string]any{"success": false, "error": "action must be hide, restore, move or add"}
	}
	stored, err := storeModelOverlay(current)
	if err != nil {
		return map[string]any{"success": false, "error": err.Error()}
	}
	syncHiddenModelsConfig(stored.Overlay.Hide)
	return map[string]any{
		"success":          true,
		"overlay":          stored.Overlay,
		"revision":         stored.Revision,
		"persistent":       action == "hide" || action == "restore",
		"persistentFields": []string{"hide"},
	}
}

func syncOverlayHiddenModels(hidden []string) {
	next, err := normalizeModelIDList(hidden, "hidden_models")
	if err != nil {
		return
	}
	modelOverlayMu.Lock()
	defer modelOverlayMu.Unlock()
	if sameStringList(currentModelOverlayState.Overlay.Hide, next) {
		return
	}
	currentModelOverlayState.Overlay.Hide = append([]string(nil), next...)
	currentModelOverlayState.Revision++
}

// currentHiddenModels returns the persistent hide list. The overlay is the
// in-process source of truth; management writes also mirror it into the
// plugin's hidden_models config through the host PATCH in the panel.
func currentHiddenModels() []string {
	modelOverlayMu.RLock()
	defer modelOverlayMu.RUnlock()
	return append([]string(nil), currentModelOverlayState.Overlay.Hide...)
}

// syncHiddenModelsConfig is a compatibility hook for callers that used the
// WorkBuddy feature runtime. Qoder keeps hidden_models in the overlay and
// relies on the host config reload to make it durable.
func syncHiddenModelsConfig([]string) {}

func sameStringList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func removeModelID(list []string, id string) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		if strings.TrimSpace(v) == id {
			continue
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func moveModelID(list []string, id string, offset int) ([]string, error) {
	if offset != -1 && offset != 1 {
		return nil, &modelConfigError{field: "offset", msg: "must be -1 (up) or 1 (down)"}
	}
	idx := -1
	for i, v := range list {
		if strings.TrimSpace(v) == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		list = append(append([]string(nil), list...), id)
		idx = len(list) - 1
	}
	target := idx + offset
	if target < 0 || target >= len(list) {
		return list, nil
	}
	out := append([]string(nil), list...)
	out[idx], out[target] = out[target], out[idx]
	return out, nil
}

func jsonUnmarshal(raw []byte, dst any) error {
	return json.Unmarshal(raw, dst)
}

func cooldownModelCount(model string) int {
	model = normalizeCooldownModel(model)
	if model == "" {
		return 0
	}
	count := 0
	for _, row := range cooldownSnapshotAll() {
		mid, _ := row["model"].(string)
		if strings.TrimSpace(mid) == model {
			count++
		}
	}
	return count
}
