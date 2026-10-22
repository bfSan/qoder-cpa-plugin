package main

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The per-account 刷新 button must repair a stale model catalog, not just
// credits. These tests pin the two properties that matter:
//   - when the upstream pull fails, the last good registration is left alone
//     (re-registering an empty catalog would drop the account from routing);
//   - when it succeeds, the auth file is written back with disabled_reason and
//     note intact, because that write is what makes the host re-register.

var errProbeUpstreamDown = errors.New("probe: upstream down")

func TestRefreshAccountModelCatalogKeepsRegistrationOnUpstreamFailure(t *testing.T) {
	sa := testStoredAuthCNAccount()
	fillFixtureToken(sa)
	raw, err := buildAuthFileJSON(sa, true, "CN · 已禁用 · 耗尽 · 余0 已用200", map[string]any{
		"disabled_reason": "manual",
	})
	if err != nil {
		t.Fatal(err)
	}
	stub := installStubAuthHost(t, map[string][]byte{"idx-1": raw})
	_ = stub

	prev := callModelsAPIFn
	callModelsAPIFn = func(*storedAuth) ([]pluginapi.ModelInfo, error) {
		return nil, errProbeUpstreamDown
	}
	t.Cleanup(func() { callModelsAPIFn = prev })

	if got := refreshAccountModelCatalog("idx-1"); got != "not-ready" {
		t.Fatalf("status = %q, want not-ready", got)
	}
	if len(stub.saved) != 0 {
		t.Fatal("auth file must not be rewritten when the upstream pull failed")
	}
}

func TestTriggerHostAuthReregisterPreservesMetadata(t *testing.T) {
	sa := testStoredAuthCNAccount()
	fillFixtureToken(sa)
	raw, err := buildAuthFileJSON(sa, true, "CN · 已禁用 · 耗尽 · 余0 已用200", map[string]any{
		"disabled_reason": "manual",
		"custom_key":      "keep-me",
	})
	if err != nil {
		t.Fatal(err)
	}
	stub := installStubAuthHost(t, map[string][]byte{"idx-1": raw})

	phys, err := hostAuthGetPhysical("idx-1")
	if err != nil || phys == nil {
		t.Fatalf("hostAuthGetPhysical: %v", err)
	}
	if err := triggerHostAuthReregister(phys); err != nil {
		t.Fatalf("triggerHostAuthReregister: %v", err)
	}
	if len(stub.saved) == 0 {
		t.Fatal("nothing was written back")
	}
	var written map[string]any
	if err := json.Unmarshal(stub.saved, &written); err != nil {
		t.Fatal(err)
	}
	// The write exists only to bump mtime / re-run registration: every field the
	// operator or lifecycle layer cares about must survive it untouched.
	if written["disabled"] != true {
		t.Fatalf("disabled flipped: %v", written["disabled"])
	}
	if written["disabled_reason"] != "manual" {
		t.Fatalf("disabled_reason lost: %v", written["disabled_reason"])
	}
	if written["note"] != "CN · 已禁用 · 耗尽 · 余0 已用200" {
		t.Fatalf("note changed: %v", written["note"])
	}
	if written["custom_key"] != "keep-me" {
		t.Fatalf("unknown key dropped: %v", written["custom_key"])
	}
}
