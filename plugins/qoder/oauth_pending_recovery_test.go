//go:build unix

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestOAuthPendingSavedMarkerFailureDoesNotSaveTwice(t *testing.T) {
	setupOAuthPendingTest(t, true)
	start := startOAuthPendingTest(t, oauthOwnerPanel, regionCN)
	calls, saves := 0, 0
	oauthHTTPDo = func(*http.Request) (*hostHTTPResponse, error) { calls++; return oauthMockToken("saved-marker"), nil }
	blocker := filepath.Join(oauthPending.store.dir, strings.Repeat("c", 64)+".pending")
	oauthSaveAuthJSON = func(string, []byte) error {
		saves++
		return os.WriteFile(blocker, []byte("invalid"), 0600)
	}
	if got := pollOAuthPanelTest(start.State); got["status"] != "error" {
		t.Fatal("expected failed marker checkpoint", got)
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if got := pollOAuthPanelTest(start.State); got["status"] != "ok" {
		t.Fatal(got)
	}
	if calls != 1 || saves != 1 {
		t.Fatal("repeated exchange/save after marker failure", calls, saves)
	}
	rec, err := oauthPending.store.read(oauthPendingName(start.State))
	if err != nil || !rec.Saved {
		t.Fatal("saved marker missing", err)
	}
}

func TestOAuthPendingResultCheckpointFailureRestoresTokenOnly(t *testing.T) {
	dir := setupOAuthPendingTest(t, true)
	start := startOAuthPendingTest(t, oauthOwnerPanel, regionCN)
	exchanges, identities, saves := 0, 0, 0
	blocker := filepath.Join(oauthPending.store.dir, strings.Repeat("d", 64)+".pending")
	oauthHTTPDo = func(req *http.Request) (*hostHTTPResponse, error) {
		if strings.Contains(req.URL.Path, "deviceToken/poll") {
			exchanges++
			return oauthMockToken(""), nil
		}
		identities++
		if identities == 1 {
			if err := os.WriteFile(blocker, []byte("invalid"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		return &hostHTTPResponse{StatusCode: 200, Body: []byte(`{"id":"identity-recovered","name":"offline"}`)}, nil
	}
	oauthSaveAuthJSON = func(string, []byte) error { saves++; return nil }
	if got := pollOAuthPanelTest(start.State); got["status"] != "error" {
		t.Fatal(got)
	}
	rec, err := oauthPending.store.read(oauthPendingName(start.State))
	if err != nil || rec.Token == nil || rec.Result != nil || saves != 0 {
		t.Fatal("token checkpoint ordering broken", err)
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	resetOAuthPendingTest()
	configureOAuthPendingTest(t, dir)
	if got := pollOAuthPanelTest(start.State); got["status"] != "ok" {
		t.Fatal(got)
	}
	if exchanges != 1 || identities != 2 || saves != 1 {
		t.Fatal("wrong recovery stages", exchanges, identities, saves)
	}
}

func TestOAuthPendingTamperRefusedBeforeNetworkEvenWhenCached(t *testing.T) {
	setupOAuthPendingTest(t, true)
	start := startOAuthPendingTest(t, oauthOwnerPanel, regionCN)
	file := filepath.Join(oauthPending.store.dir, oauthPendingName(start.State))
	if err := os.WriteFile(file, []byte(`{"version":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	oauthHTTPDo = func(*http.Request) (*hostHTTPResponse, error) { calls++; return nil, errors.New("must not call") }
	if got := pollOAuthPanelTest(start.State); got["status"] != "error" || calls != 0 {
		t.Fatal("corruption consumed grant", got, calls)
	}
}

func TestOAuthPendingStoreOversizeFIFOAndSubstitution(t *testing.T) {
	setupOAuthPendingTest(t, true)
	a := startOAuthPendingTest(t, oauthOwnerPanel, regionCN)
	b := startOAuthPendingTest(t, oauthOwnerPanel, regionCN)
	store := oauthPending.store
	name := oauthPendingName(a.State)
	file := filepath.Join(store.dir, name)
	original, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	bfile := filepath.Join(store.dir, oauthPendingName(b.State))
	if err := os.WriteFile(bfile, original, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.read(oauthPendingName(b.State)); err == nil {
		t.Fatal("record substitution accepted")
	}
	if err := os.WriteFile(file, make([]byte, oauthPendingMaxBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.read(name); err == nil {
		t.Fatal("oversized disk record accepted")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(file, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.read(name); err == nil {
		t.Fatal("FIFO accepted")
	}
}

func TestOAuthPendingStateMetadataAndRequestDeadline(t *testing.T) {
	setupOAuthPendingTest(t, false)
	raw, err := startLoginOwned(nil, regionCN, oauthOwnerHost)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	var result map[string]any
	if json.Unmarshal(raw, &env) != nil || json.Unmarshal(env.Result, &result) != nil {
		t.Fatal("invalid envelope")
	}
	if persistent, ok := result["persistent"]; !ok || persistent != false || result["ExpiresAt"] == nil || result["save_owner"] != oauthOwnerHost {
		t.Fatal("missing explicit persistence contract")
	}
	oauthHTTPDo = func(req *http.Request) (*hostHTTPResponse, error) {
		deadline, ok := req.Context().Deadline()
		if !ok || time.Until(deadline) > 30*time.Second || time.Until(deadline) < time.Second {
			t.Fatal("login deadline missing")
		}
		return &hostHTTPResponse{StatusCode: 202}, nil
	}
	raw, err = pollOAuthHostTest(result["State"].(string), nil)
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(raw, &env) != nil || json.Unmarshal(env.Result, &result) != nil || result["persistent"] != false || result["expires_at"] == nil {
		t.Fatal("poll resume contract missing")
	}
}

func TestOAuthPendingRecordValidation(t *testing.T) {
	setupOAuthPendingTest(t, true)
	start := startOAuthPendingTest(t, oauthOwnerPanel, regionCN)
	lc, _ := lookupOAuthPending(start.State)
	cases := map[string]func(*oauthPendingRecord){
		"version":             func(r *oauthPendingRecord) { r.Version = 99 },
		"owner":               func(r *oauthPendingRecord) { r.Owner = "attacker" },
		"region":              func(r *oauthPendingRecord) { r.Region = "https://evil.invalid" },
		"state":               func(r *oauthPendingRecord) { r.State = "../../outside" },
		"verifier":            func(r *oauthPendingRecord) { r.Verifier = "bad" },
		"nonce":               func(r *oauthPendingRecord) { r.Nonce = "bad" },
		"device_key":          func(r *oauthPendingRecord) { r.DeviceKey = "bad" },
		"future":              func(r *oauthPendingRecord) { r.Created = time.Now().Add(time.Hour) },
		"ttl":                 func(r *oauthPendingRecord) { r.Expires = r.Created.Add(24 * time.Hour) },
		"saved_without_token": func(r *oauthPendingRecord) { r.Saved = true },
		"incomplete_pair": func(r *oauthPendingRecord) {
			r.Token = &deviceTokenResponse{Token: "dt-offline"}
			r.TokenAt = time.Now()
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			r := oauthRecord(start.State, lc)
			change(r)
			if validOAuthPendingRecord(r) {
				t.Fatal("invalid record accepted")
			}
		})
	}
	r := oauthRecord(start.State, lc)
	r.Token = &deviceTokenResponse{Token: strings.Repeat("x", oauthPendingMaxBytes), RefreshToken: "drt-offline"}
	r.TokenAt = time.Now()
	r.Expires = r.TokenAt.Add(oauthResultTTL)
	if oauthPending.store.write(r) == nil {
		t.Fatal("oversize signed token checkpoint accepted")
	}
}

func TestOAuthPendingMissingKeyCannotSilentlyRekey(t *testing.T) {
	dir := setupOAuthPendingTest(t, true)
	startOAuthPendingTest(t, oauthOwnerPanel, regionCN)
	key := filepath.Join(oauthPending.store.dir, ".key")
	resetOAuthPendingTest()
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"host": map[string]any{"auth_dir": dir}})
	if configureOAuthPending(raw) == nil {
		t.Fatal("silently replaced missing integrity key")
	}
}
