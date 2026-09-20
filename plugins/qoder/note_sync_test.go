package main

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// stubLifecycleHost points the auth-file indirection at an in-memory file so
// note-writing paths can be tested without a live host RPC bridge.
func stubLifecycleHost(t *testing.T, initialNote string) (*string, *string) {
	t.Helper()
	stored := initialNote
	savedName := ""
	oldGet := hostAuthGetPhysicalFn
	oldSave := hostAuthPersistMigrateFn
	hostAuthGetPhysicalFn = func(authIndex string) (*hostAuthPhysical, error) {
		raw, _ := json.Marshal(map[string]any{"note": stored})
		return &hostAuthPhysical{AuthIndex: authIndex, Name: "qoder-cn-test.json", JSON: raw}, nil
	}
	hostAuthPersistMigrateFn = func(name, path, legacyPath string, raw []byte) error {
		savedName = name
		var decoded struct {
			Note string `json:"note"`
		}
		_ = json.Unmarshal(raw, &decoded)
		stored = decoded.Note
		return nil
	}
	t.Cleanup(func() {
		hostAuthGetPhysicalFn = oldGet
		hostAuthPersistMigrateFn = oldSave
	})
	return &stored, &savedName
}

func testStoredAuthCNAccount() *storedAuth {
	return &storedAuth{
		Account: storedAccount{Nickname: "tester", UID: "uid-1"},
		Auth:    storedTokens{Region: regionCN},
	}
}

// TestSyncAuthNoteWritesFreshCredits is the regression guard for the reported
// bug: the plugin panel showed live credits while CPA's native auth card stayed
// on "积分未知" because only the dashboard path wrote the note.
func TestSyncAuthNoteWritesFreshCredits(t *testing.T) {
	stored, savedName := stubLifecycleHost(t, "CN · 积分未知")
	lifecycleState.Delete("uid-1")
	t.Cleanup(func() { lifecycleState.Delete("uid-1") })

	cr := &creditsSummary{TotalRemain: 360, TotalUsed: 40, TotalSize: 400}
	if err := syncAuthNote("idx-1", "uid-1", testStoredAuthCNAccount(), cr, false); err != nil {
		t.Fatalf("syncAuthNote: %v", err)
	}
	if want := "CN · 余360 已用40 池400"; *stored != want {
		t.Fatalf("stored note = %q, want %q", *stored, want)
	}
	if *savedName == "" {
		t.Fatal("expected the auth file to be persisted")
	}
}

// TestSyncAuthNoteKeepsKnownCreditsOnFailure guards the other half: a transient
// credits-query failure must not overwrite a good note with "积分未知".
func TestSyncAuthNoteKeepsKnownCreditsOnFailure(t *testing.T) {
	stored, _ := stubLifecycleHost(t, "CN · 余360 已用40 池400")
	lifecycleState.Delete("uid-2")
	t.Cleanup(func() { lifecycleState.Delete("uid-2") })

	sa := testStoredAuthCNAccount()
	sa.Account.UID = "uid-2"
	if err := syncAuthNote("idx-2", "uid-2", sa, nil, false); err != nil {
		t.Fatalf("syncAuthNote: %v", err)
	}
	if want := "CN · 余360 已用40 池400"; *stored != want {
		t.Fatalf("stored note = %q, want preserved %q", *stored, want)
	}
}

// TestCreditSegmentFromNoteIgnoresPlaceholders ensures placeholder text never
// counts as a real credit snapshot worth preserving.
func TestCreditSegmentFromNoteIgnoresPlaceholders(t *testing.T) {
	cases := []struct {
		note string
		want string
	}{
		{"CN · 积分未知", ""},
		{"INTL · 余500 已用0 池500", "余500 已用0 池500"},
		{"CN · 已禁用 · 耗尽 · 余0 已用100", "耗尽 · 余0 已用100"},
		{"CN", ""},
	}
	for _, tc := range cases {
		if got := creditSegmentFromNote(tc.note); got != tc.want {
			t.Errorf("creditSegmentFromNote(%q) = %q, want %q", tc.note, got, tc.want)
		}
	}
}

// TestCreditsQueryWritesAuthNote covers the handler wiring itself: the lazy
// /credits refresh must propagate to the native auth file.
func TestCreditsQueryWritesAuthNote(t *testing.T) {
	stored, _ := stubLifecycleHost(t, "CN · 积分未知")
	lifecycleState.Delete("uid-3")
	t.Cleanup(func() { lifecycleState.Delete("uid-3") })

	sa := testStoredAuthCNAccount()
	sa.Account.UID = "uid-3"
	raw, _ := json.Marshal(map[string]any{
		"provider": providerName,
		"type":     providerName,
		"account":  map[string]any{"uid": "uid-3", "nickname": "tester"},
		"auth":     map[string]any{"region": regionCN},
	})

	oldList := panelHostAuthList
	oldBundle := panelHostAuthBundle
	oldFetch := fetchUserResourceFn
	oldPlan := fetchPaymentTypeFn
	panelHostAuthList = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{
			ID: "uid-3", AuthIndex: "idx-3", Name: "qoder-cn-uid-3.json",
		}}, nil
	}
	panelHostAuthBundle = func(string) (*storedAuth, *hostAuthPhysical, error) {
		return sa, &hostAuthPhysical{AuthIndex: "idx-3", JSON: raw}, nil
	}
	fetchUserResourceFn = func(*storedAuth) (*creditsSummary, error) {
		return &creditsSummary{TotalRemain: 111, TotalUsed: 5, TotalSize: 116}, nil
	}
	fetchPaymentTypeFn = func(*storedAuth) string { return "Pro" }
	t.Cleanup(func() {
		panelHostAuthList = oldList
		panelHostAuthBundle = oldBundle
		fetchUserResourceFn = oldFetch
		fetchPaymentTypeFn = oldPlan
	})

	resp := handleCreditsQuery(pluginapi.ManagementRequest{
		Query: map[string][]string{"auth_index": {"idx-3"}},
	})
	if errVal, ok := resp["error"]; ok {
		t.Fatalf("handleCreditsQuery error: %v", errVal)
	}
	if want := "CN · 余111 已用5 池116"; *stored != want {
		t.Fatalf("stored note = %q, want %q", *stored, want)
	}
}
