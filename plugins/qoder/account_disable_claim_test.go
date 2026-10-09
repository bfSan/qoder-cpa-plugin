package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// 凭证禁用必须能被操作者手动控制，而且这个意图要活过生命周期自动化——
// shouldReenableCN 只看余额，会把"有余额的已禁用账号"当成意外状态直接恢复。

// fillFixtureToken gives a credential fixture a usable access token: parseStored
// refuses to build a storedAuth without one, which is the right behaviour for
// real files but makes an empty fixture look like a parse failure.
func fillFixtureToken(sa *storedAuth) {
	if sa != nil && strings.TrimSpace(sa.Auth.AccessToken) == "" {
		sa.Auth.AccessToken = "dt-fixture-access-token"
	}
}

func fixtureAuthFile(t *testing.T, sa *storedAuth, disabled bool) []byte {
	t.Helper()
	fillFixtureToken(sa)
	raw, err := buildAuthFileJSON(sa, disabled, "CN · 余100", nil)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func disableRequest(authIndex string, disabled bool) pluginapi.ManagementRequest {
	body, _ := json.Marshal(map[string]any{"auth_index": authIndex, "disabled": disabled})
	return pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/accounts/disabled", Body: body}
}

// stubAuthHost answers the auth RPCs from memory. It stubs hostCall, the single
// lowest-level bridge, so every layer above it (hostAuthList, hostAuthGet,
// host.auth.save) runs its real code instead of a test-only shortcut.
type stubAuthHost struct {
	files map[string][]byte
	names map[string]string
	saved []byte
}

// rpcEnvelope mirrors the host bridge's reply shape. A named struct is required
// rather than a map: json.RawMessage inside a map[string]any is marshalled as
// base64, which would hand the caller a string where it expects an object.
type rpcEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
}

func envelopeJSON(ok bool, result []byte) []byte {
	env := rpcEnvelope{OK: ok}
	if len(result) > 0 {
		env.Result = json.RawMessage(result)
	}
	raw, _ := json.Marshal(env)
	return raw
}

// stubAuthFileEntry is the RPC shape for one auth file listing row.
type stubAuthFileEntry struct {
	ID        string `json:"id"`
	AuthIndex string `json:"auth_index"`
	Name      string `json:"name"`
	Type      string `json:"type"`
}

// stubAuthGetReply is the RPC shape for host.auth.get.
type stubAuthGetReply struct {
	ID        string          `json:"id"`
	AuthIndex string          `json:"auth_index"`
	Name      string          `json:"name"`
	JSON      json.RawMessage `json:"json"`
}

func installStubAuthHost(t *testing.T, entries map[string][]byte) *stubAuthHost {
	t.Helper()
	stub := &stubAuthHost{files: map[string][]byte{}, names: map[string]string{}}
	for idx, raw := range entries {
		stub.files[idx] = append([]byte(nil), raw...)
		stub.names[idx] = "qoder-" + idx + ".json"
	}
	prev := hostCall
	hostCall = func(method string, request []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			files := make([]stubAuthFileEntry, 0, len(stub.files))
			for idx := range stub.files {
				files = append(files, stubAuthFileEntry{
					ID: idx, AuthIndex: idx, Name: stub.names[idx], Type: providerName,
				})
			}
			res, _ := json.Marshal(struct {
				Files []stubAuthFileEntry `json:"files"`
			}{Files: files})
			return envelopeJSON(true, res), nil
		case pluginabi.MethodHostAuthGet:
			var req struct {
				AuthIndex string `json:"auth_index"`
			}
			_ = json.Unmarshal(request, &req)
			raw, ok := stub.files[req.AuthIndex]
			if !ok {
				return envelopeJSON(false, nil), nil
			}
			res, _ := json.Marshal(stubAuthGetReply{
				ID: req.AuthIndex, AuthIndex: req.AuthIndex,
				Name: stub.names[req.AuthIndex], JSON: json.RawMessage(raw),
			})
			return envelopeJSON(true, res), nil
		case pluginabi.MethodHostAuthSave:
			var req struct {
				Name string          `json:"name"`
				JSON json.RawMessage `json:"json"`
			}
			_ = json.Unmarshal(request, &req)
			stub.saved = append([]byte(nil), req.JSON...)
			for idx, name := range stub.names {
				if name == req.Name {
					stub.files[idx] = stub.saved
				}
			}
			res, _ := json.Marshal(struct {
				Name string `json:"name"`
			}{Name: req.Name})
			return envelopeJSON(true, res), nil
		}
		return envelopeJSON(true, nil), nil
	}
	t.Cleanup(func() { hostCall = prev })
	return stub
}

func TestAccountSetDisabledParksAccountAndRecordsIntent(t *testing.T) {
	sa := testStoredAuthCNAccount()
	stub := installStubAuthHost(t, map[string][]byte{"idx-1": fixtureAuthFile(t, sa, false)})

	out := handleAccountSetDisabled(disableRequest("idx-1", true))
	if out["error"] != nil {
		t.Fatalf("disable failed: %v", out["error"])
	}
	if out["disabled"] != true {
		t.Fatalf("response disabled = %v, want true", out["disabled"])
	}
	if len(stub.saved) == 0 {
		t.Fatal("auth file was not written")
	}
	var written map[string]any
	if err := json.Unmarshal(stub.saved, &written); err != nil {
		t.Fatal(err)
	}
	if written["disabled"] != true {
		t.Fatalf("written disabled = %v, want true", written["disabled"])
	}
	// 没有这个标记，下一次生命周期 tick 就会把账号悄悄恢复。
	if written["disabled_reason"] != disableReasonManual {
		t.Fatalf("disabled_reason = %v, want %q", written["disabled_reason"], disableReasonManual)
	}
}

// 启用是禁用的显式撤销，必须清掉意图，否则它会被当成陈旧失败一直读下去。
func TestAccountSetDisabledClearsIntentOnEnable(t *testing.T) {
	sa := testStoredAuthCNAccount()
	// 起始状态是"手动停用"，文件上带着意图标记。
	fillFixtureToken(sa)
	start, err := buildAuthFileJSON(sa, true, "CN · 余100", map[string]any{"disabled_reason": disableReasonManual})
	if err != nil {
		t.Fatal(err)
	}
	stub := installStubAuthHost(t, map[string][]byte{"idx-1": start})

	out := handleAccountSetDisabled(disableRequest("idx-1", false))
	if out["error"] != nil {
		t.Fatalf("enable failed: %v", out["error"])
	}
	if out["disabled"] != false {
		t.Fatalf("response disabled = %v, want false", out["disabled"])
	}
	var written map[string]any
	if err := json.Unmarshal(stub.saved, &written); err != nil {
		t.Fatal(err)
	}
	if written["disabled"] != false {
		t.Fatalf("written disabled = %v, want false", written["disabled"])
	}
	// extra 里写的是 nil，序列化后是 JSON null —— 关键是不能还是 "manual"。
	if written["disabled_reason"] == disableReasonManual {
		t.Fatal("disabled_reason survived enabling and would read as a stale manual intent")
	}
}

// 手动禁用必须能被生命周期识别，从而不被 shouldReenableCN 推翻。
func TestManualDisableReasonDetection(t *testing.T) {
	manual, _ := json.Marshal(map[string]any{"disabled": true, "disabled_reason": disableReasonManual})
	if !manualDisableReason(manual) {
		t.Fatal("manual disable was not recognised")
	}
	// 生命周期自己写的禁用没有这个标记，仍应允许按余额恢复。
	auto, _ := json.Marshal(map[string]any{"disabled": true})
	if manualDisableReason(auto) {
		t.Fatal("an automatic disable was mistaken for a manual one")
	}
	other, _ := json.Marshal(map[string]any{"disabled": true, "disabled_reason": "exhausted"})
	if manualDisableReason(other) {
		t.Fatal("a non-manual reason was mistaken for manual")
	}
	if manualDisableReason(nil) || manualDisableReason([]byte("not json")) {
		t.Fatal("garbage input classified as a manual disable")
	}
}

// 手动停用之后，即使余额充足，生命周期也必须先看到手动意图再决定。
func TestManualDisableSurvivesReenableDecision(t *testing.T) {
	sa := testStoredAuthCNAccount()
	stub := installStubAuthHost(t, map[string][]byte{"idx-1": fixtureAuthFile(t, sa, false)})
	if out := handleAccountSetDisabled(disableRequest("idx-1", true)); out["error"] != nil {
		t.Fatalf("disable failed: %v", out["error"])
	}
	// 余额充足——shouldReenableCN 会说要恢复。
	cr := &creditsSummary{TotalRemain: 500, TotalSize: 500}
	if !shouldReenableCN(true, cr) {
		t.Fatal("precondition: shouldReenableCN should want to re-enable")
	}
	// 但写下去的文件必须带手动意图，生命周期据此跳过恢复。
	if !manualDisableReason(stub.saved) {
		t.Fatal("manual intent was not persisted where the lifecycle reads it")
	}
}

// 缺少 auth_index 或 disabled 时必须报错，不能默默成功。
func TestAccountSetDisabledValidatesInput(t *testing.T) {
	if out := handleAccountSetDisabled(pluginapi.ManagementRequest{}); out["error"] == nil {
		t.Fatal("missing auth_index did not fail")
	}
	body, _ := json.Marshal(map[string]any{"auth_index": "idx-1"})
	if out := handleAccountSetDisabled(pluginapi.ManagementRequest{Body: body}); out["error"] == nil {
		t.Fatal("missing disabled did not fail")
	}
}

// 账号不存在时报错，而不是写坏一个文件。
func TestAccountSetDisabledRejectsUnknownAccount(t *testing.T) {
	oldGet := hostAuthGetPhysicalFn
	hostAuthGetPhysicalFn = func(string) (*hostAuthPhysical, error) { return nil, nil }
	t.Cleanup(func() { hostAuthGetPhysicalFn = oldGet })
	out := handleAccountSetDisabled(disableRequest("idx-missing", true))
	if out["error"] == nil {
		t.Fatal("unknown account did not fail")
	}
}

// 领取 Pro 的批量路径必须跳过 Intl、把已领过的算作正常结果而不是失败，
// 并且给每个账号单独的结果行，方便面板按账号展示。
func TestClaimProBatchClassifiesResults(t *testing.T) {
	cn := testStoredAuthCNAccount()
	cn.Auth.Region = regionCN
	intl := testStoredAuthCNAccount()
	intl.Account.UID = "uid-intl"
	intl.Auth.Region = regionIntl

	installStubAuthHost(t, map[string][]byte{
		"idx-cn":   fixtureAuthFile(t, cn, false),
		"idx-intl": fixtureAuthFile(t, intl, false),
	})

	out := handleClaimPro(pluginapi.ManagementRequest{})
	if out["error"] != nil {
		t.Fatalf("batch claim errored: %v", out["error"])
	}
	results, ok := out["results"].([]map[string]any)
	if !ok || len(results) != 2 {
		t.Fatalf("results = %#v, want one row per account", out["results"])
	}
	sum, ok := out["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary missing: %#v", out["summary"])
	}
	// Intl 不支持，必须被单独归类，不能算作失败——它不是错误。
	if got := sum["unsupported"]; got != 1 {
		t.Fatalf("summary.unsupported = %v, want 1 (the Intl account)", got)
	}
	if got := sum["total"]; got != 2 {
		t.Fatalf("summary.total = %v, want 2", got)
	}
}

// 单账号调用仍要保留旧的顶层字段，避免依赖旧形状的调用方读不到结果。
func TestClaimProSingleAccountKeepsTopLevelShape(t *testing.T) {
	sa := testStoredAuthCNAccount()
	sa.Auth.Region = regionIntl
	installStubAuthHost(t, map[string][]byte{"idx-intl": fixtureAuthFile(t, sa, false)})

	out := handleClaimPro(pluginapi.ManagementRequest{Body: []byte(`{"auth_index":"idx-intl"}`)})
	if out["auth_index"] != "idx-intl" {
		t.Fatalf("auth_index = %v, want idx-intl", out["auth_index"])
	}
	if out["reason"] != "unsupported" {
		t.Fatalf("reason = %v, want unsupported", out["reason"])
	}
}

// 一个账号都没有时明确报错，而不是返回一个空的成功。
func TestClaimProBatchWithoutAccountsFails(t *testing.T) {
	installStubAuthHost(t, nil)
	if out := handleClaimPro(pluginapi.ManagementRequest{}); out["error"] == nil {
		t.Fatal("empty account list did not fail")
	}
}

// 批量结果必须带上耗时，面板据此知道自己等的是批量而不是单个账号。
func TestClaimProBatchReportsElapsed(t *testing.T) {
	sa := testStoredAuthCNAccount()
	sa.Auth.Region = regionIntl
	installStubAuthHost(t, map[string][]byte{"idx-intl": fixtureAuthFile(t, sa, false)})
	out := handleClaimPro(pluginapi.ManagementRequest{})
	sum, _ := out["summary"].(map[string]any)
	if _, ok := sum["elapsed_ms"]; !ok {
		t.Fatalf("summary has no elapsed_ms: %#v", sum)
	}
}
