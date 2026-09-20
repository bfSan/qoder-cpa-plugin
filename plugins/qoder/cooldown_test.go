package main

import (
	"encoding/json"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// resetCooldowns empties the table and restores the clock around a test.
func resetCooldowns(t *testing.T) {
	t.Helper()
	cooldownMu.Lock()
	prev := cooldownTable
	cooldownTable = make(map[modelCooldownKey]modelCooldownEntry)
	cooldownMu.Unlock()
	cooldownNowFn = time.Now
	t.Cleanup(func() {
		cooldownMu.Lock()
		cooldownTable = prev
		cooldownMu.Unlock()
		cooldownNowFn = time.Now
	})
}

func TestMarkModelCooldown_RequiresBothKeys(t *testing.T) {
	resetCooldowns(t)
	markModelCooldown("qoder-a", "", cooldownReasonRateLimit)
	markModelCooldown("", "qfmodel", cooldownReasonRateLimit)
	if got := cooldownSnapshotAll(); len(got) != 0 {
		t.Fatalf("blank auth or model must not create an entry, got %+v", got)
	}
}

func TestMarkModelCooldown_IsScopedToModel(t *testing.T) {
	resetCooldowns(t)
	markModelCooldown("qoder-a", "qfmodel", cooldownReasonRateLimit)
	if !modelIsCooling("qoder-a", "qfmodel") {
		t.Fatal("qfmodel should be cooling")
	}
	if modelIsCooling("qoder-a", "gmodel") {
		t.Fatal("another model must stay usable: cooldown is per model, not per account")
	}
	if modelIsCooling("qoder-b", "qfmodel") {
		t.Fatal("another account must stay usable for the same model")
	}
}

func TestModelIsCooling_ExpiresOnItsOwn(t *testing.T) {
	resetCooldowns(t)
	base := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	now := base
	cooldownNowFn = func() time.Time { return now }
	markModelCooldown("qoder-a", "qfmodel", cooldownReasonUnknown)
	if !modelIsCooling("qoder-a", "qfmodel") {
		t.Fatal("should cool right after the failure")
	}
	now = base.Add(modelCooldownUnknown + time.Second)
	if modelIsCooling("qoder-a", "qfmodel") {
		t.Fatal("should recover once the TTL passes")
	}
	if got := cooldownSnapshotAll(); len(got) != 0 {
		t.Fatalf("expired entries should be swept, got %+v", got)
	}
}

func TestRecordUpstreamFailure_EmptyStreamCoolsPair(t *testing.T) {
	resetCooldowns(t)
	recordUpstreamFailure("qoder-a", "qfmodel", 0, "empty_stream: upstream stream closed before first payload")
	if !modelIsCooling("qoder-a", "qfmodel") {
		t.Fatal("empty_stream should cool the specific model pair")
	}
	if modelIsCooling("qoder-a", "gmodel") {
		t.Fatal("a failed model must not cool the whole account")
	}
}

func TestRecordUpstreamFailure_503EmptyStreamCoolsPair(t *testing.T) {
	resetCooldowns(t)
	recordUpstreamFailure("qoder-a", "qoder/qwen3.8-flash", 503,
		`unexpected status 503 Service Unavailable: auth_unavailable: no auth available (last upstream error: empty_stream: upstream stream closed before first payload)`)
	if !modelIsCooling("qoder-a", "qwen3.8-flash") {
		t.Fatal("503 + empty_stream should cool the specific model pair")
	}
	if modelIsCooling("qoder-a", "qfmodel") {
		t.Fatal("a failed model must not cool the whole account")
	}
}

func TestRequestModelForCooldown_PrefersRoutingMetadata(t *testing.T) {
	metadata := map[string]any{
		"requested_model":      "qoder/deepseek-v4.1-flash",
		"auth_selection_model": "qoder/deepseek-v4.1-flash",
	}
	if got := requestModelForCooldown("dfmodel", metadata); got != "deepseek-v4.1-flash" {
		t.Fatalf("requestModelForCooldown() = %q, want routing alias", got)
	}
}

func TestRequestModelForCooldown_FallsBackToExecutorModel(t *testing.T) {
	if got := requestModelForCooldown("qoder/qfmodel", nil); got != "qfmodel" {
		t.Fatalf("requestModelForCooldown() = %q, want provider-prefix-stripped model", got)
	}
}

func TestRecordUpstreamFailure_HardCreditStaysAccountScoped(t *testing.T) {
	resetCooldowns(t)
	recordUpstreamFailure("qoder-a", "qfmodel", 402, `{"message":"insufficient credit"}`)
	if got := cooldownSnapshotAll(); len(got) != 0 {
		t.Fatalf("hard credit errors belong to lifecycle handling, got %+v", got)
	}
}

func TestClearModelCooldown_OnePairOrWholeAccount(t *testing.T) {
	resetCooldowns(t)
	markModelCooldown("qoder-a", "qfmodel", cooldownReasonRateLimit)
	markModelCooldown("qoder-a", "gmodel", cooldownReasonRateLimit)
	markModelCooldown("qoder-b", "qfmodel", cooldownReasonRateLimit)
	if got := clearModelCooldown("qoder-a", "qfmodel"); got != 1 {
		t.Fatalf("clearing one pair removed %d", got)
	}
	if modelIsCooling("qoder-a", "qfmodel") || !modelIsCooling("qoder-a", "gmodel") {
		t.Fatal("only the named pair should be cleared")
	}
	if got := clearModelCooldown("qoder-a", ""); got != 1 {
		t.Fatalf("clearing the account removed %d, want 1", got)
	}
	if modelIsCooling("qoder-a", "gmodel") {
		t.Fatal("account-wide clear should drop every pair")
	}
	if !modelIsCooling("qoder-b", "qfmodel") {
		t.Fatal("clearing qoder-a must not touch qoder-b")
	}
}

func TestCooldownSnapshotFor_OnlyOwnEntries(t *testing.T) {
	resetCooldowns(t)
	markModelCooldown("qoder-a", "gmodel", cooldownReasonRateLimit)
	markModelCooldown("qoder-a", "qfmodel", cooldownReasonRateLimit)
	rows := cooldownSnapshotFor("qoder-a")
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %+v", rows)
	}
	if rows[0]["model"] != "gmodel" {
		t.Fatalf("rows should be sorted by model, got %+v", rows[0])
	}
	for _, r := range rows {
		if r["auth_id"] != nil {
			t.Fatalf("per-account snapshot should not repeat auth_id, got %+v", r)
		}
	}
	if got := cooldownSnapshotFor(""); got != nil {
		t.Fatalf("blank auth should return nil, got %+v", got)
	}
}

func TestHandleCooldownList_ReadsQueryScope(t *testing.T) {
	resetCooldowns(t)
	markModelCooldown("qoder-a", "qfmodel", cooldownReasonRateLimit)
	markModelCooldown("qoder-b", "qfmodel", cooldownReasonRateLimit)
	all := handleCooldownList(pluginapi.ManagementRequest{})
	if all["count"] != 2 {
		t.Fatalf("global count = %v, want 2", all["count"])
	}
	one := handleCooldownList(pluginapi.ManagementRequest{Query: url.Values{"auth_id": []string{"qoder-b"}}})
	if one["count"] != 1 {
		t.Fatalf("scoped count = %v, want 1", one["count"])
	}
}

func TestHandleCooldownClear_RefusesGlobalWipe(t *testing.T) {
	resetCooldowns(t)
	markModelCooldown("qoder-a", "qfmodel", cooldownReasonRateLimit)
	if ok, _ := handleCooldownClear(pluginapi.ManagementRequest{})["success"].(bool); ok {
		t.Fatal("clearing without auth_id must fail")
	}
	res := handleCooldownClear(pluginapi.ManagementRequest{
		Body: []byte(`{"auth_id":"qoder-a"}`),
	})
	if ok, _ := res["success"].(bool); !ok {
		t.Fatalf("body-scoped clear failed: %+v", res)
	}
}

func TestCooldownTable_BoundedByMaxEntries(t *testing.T) {
	resetCooldowns(t)
	for i := 0; i < modelCooldownMaxEntries+16; i++ {
		markModelCooldown("qoder-a", "model-"+strconv.Itoa(i), cooldownReasonRateLimit)
	}
	cooldownMu.Lock()
	size := len(cooldownTable)
	cooldownMu.Unlock()
	if size > modelCooldownMaxEntries {
		t.Fatalf("table grew to %d, cap is %d", size, modelCooldownMaxEntries)
	}
}

func TestCooldownSnapshotIsJSONSerializable(t *testing.T) {
	resetCooldowns(t)
	markModelCooldown("qoder-a", "qfmodel", cooldownReasonRateLimit)
	if _, err := json.Marshal(cooldownSnapshotAll()); err != nil {
		t.Fatalf("snapshot must marshal: %v", err)
	}
}
