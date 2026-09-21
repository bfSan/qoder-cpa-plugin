package main

import (
	"encoding/json"
	"testing"
	"time"

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

// TestBuildAuthFileJSONPersistsHostLabel guards the auth-page regression: CPA's
// file store builds its label from the top-level metadata map, so the host label
// must be written to disk rather than only returned from AuthParse.
func TestBuildAuthFileJSONPersistsHostLabel(t *testing.T) {
	sa := testStoredAuthCNAccount()
	raw, err := buildAuthFileJSON(sa, false, "CN · 余1 已用2", nil)
	if err != nil {
		t.Fatalf("buildAuthFileJSON: %v", err)
	}
	var parsed struct {
		Label string `json:"label"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal auth file: %v", err)
	}
	if want := labelForAuth(sa); parsed.Label != want {
		t.Fatalf("label = %q, want %q", parsed.Label, want)
	}
}

func TestAccountRenameSyncsHostLabel(t *testing.T) {
	oldGet := hostAuthGetPhysicalFn
	oldSave := hostAuthPersistMigrateFn
	var saved []byte
	hostAuthGetPhysicalFn = func(authIndex string) (*hostAuthPhysical, error) {
		raw, _ := json.Marshal(map[string]any{
			"type":    providerName,
			"account": map[string]any{"uid": "uid-rename", "nickname": "old"},
			"auth":    map[string]any{"accessToken": "tok", "region": regionCN},
			"note":    "CN · 余1 已用2",
		})
		return &hostAuthPhysical{AuthIndex: authIndex, Name: "qoder-cn-uid-rename.json", JSON: raw}, nil
	}
	hostAuthPersistMigrateFn = func(name, path, legacyPath string, raw []byte) error {
		saved = append([]byte(nil), raw...)
		return nil
	}
	t.Cleanup(func() {
		hostAuthGetPhysicalFn = oldGet
		hostAuthPersistMigrateFn = oldSave
	})

	resp := handleAccountRename(pluginapi.ManagementRequest{
		Body: []byte(`{"auth_index":"idx-rename","name":"新名字"}`),
	})
	if errValue, ok := resp["error"]; ok {
		t.Fatalf("rename error: %v", errValue)
	}
	var parsed struct {
		Label   string `json:"label"`
		Account struct {
			Nickname string `json:"nickname"`
		} `json:"account"`
	}
	if err := json.Unmarshal(saved, &parsed); err != nil {
		t.Fatalf("unmarshal saved auth: %v", err)
	}
	if parsed.Account.Nickname != "新名字" {
		t.Fatalf("nickname = %q", parsed.Account.Nickname)
	}
	if want := "新名字 [CN]"; parsed.Label != want {
		t.Fatalf("label = %q, want %q", parsed.Label, want)
	}
}

func TestWaitForRuntimeAuthLabelWaitsForReparse(t *testing.T) {
	oldRuntime := runtimeAuthLabelFn
	reads := 0
	runtimeAuthLabelFn = func(authIndex string) (string, error) {
		if authIndex != "idx-rename" {
			t.Fatalf("auth index = %q", authIndex)
		}
		reads++
		if reads < 3 {
			return "qoder", nil
		}
		return "新名字 [CN]", nil
	}
	t.Cleanup(func() { runtimeAuthLabelFn = oldRuntime })

	if err := waitForRuntimeAuthLabel("idx-rename", "新名字 [CN]", time.Second); err != nil {
		t.Fatalf("waitForRuntimeAuthLabel: %v", err)
	}
	if reads < 3 {
		t.Fatalf("runtime label reads = %d, want at least 3", reads)
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

// TestAuthParseKeepsKnownCredits pins the reload regression: CPA rebuilds the
// in-memory auth metadata from AuthParse on every restart/reload, so parsing
// must reuse the note already stored in the file rather than emitting the
// cr==nil placeholder (which showed live credits as "积分未知").
func TestAuthParseKeepsKnownCredits(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"provider": providerName,
		"type":     providerName,
		"note":     "CN · 余360 已用40 池400",
		"account":  map[string]any{"uid": "uid-parse", "nickname": "tester"},
		"auth":     map[string]any{"region": regionCN, "accessToken": "tok"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := json.Marshal(pluginapi.AuthParseRequest{
		Provider: providerName,
		FileName: "qoder-cn-uid-parse.json",
		RawJSON:  raw,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	body, err := handleParseAuth(req)
	if err != nil {
		t.Fatalf("handleParseAuth: %v", err)
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Handled bool `json:"handled"`
			Auth    struct {
				Metadata map[string]any `json:"metadata"`
			} `json:"auth"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if !env.OK || !env.Result.Handled {
		t.Fatalf("parse not handled: %s", body)
	}
	if got, _ := env.Result.Auth.Metadata["note"].(string); got != "CN · 余360 已用40 池400" {
		t.Fatalf("note = %q, want the stored credit segment", got)
	}
}

// TestDisplayNoteWithPrevFallsBackForGarbage keeps the guard honest: a note
// carrying only placeholders must still render the unknown state.
func TestDisplayNoteWithPrevFallsBackForGarbage(t *testing.T) {
	if got := displayNoteWithPrev(testStoredAuthCNAccount(), nil, false, "CN · 积分未知"); got != "CN · 积分未知" {
		t.Fatalf("note = %q, want placeholder", got)
	}
	if got := displayNoteWithPrev(testStoredAuthCNAccount(), nil, false, "CN · 余9 已用1"); got != "CN · 余9 已用1" {
		t.Fatalf("note = %q, want preserved credits", got)
	}
}
