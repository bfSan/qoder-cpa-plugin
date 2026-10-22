//go:build unix

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func resetOAuthPendingTest() {
	oauthPending.Lock()
	defer oauthPending.Unlock()
	loginStates.Range(func(k, v any) bool { loginStates.Delete(k); return true })
	if oauthPending.store != nil {
		oauthPending.store.close()
	}
	oauthPending.store = nil
	oauthPending.initErr = nil
}

func setupOAuthPendingTest(t *testing.T, persistent bool) string {
	t.Helper()
	resetOAuthPendingTest()
	oldHTTP, oldSave := oauthHTTPDo, oauthSaveAuthJSON
	oauthHTTPDo = func(*http.Request) (*hostHTTPResponse, error) { return nil, errors.New("unexpected offline test HTTP") }
	oauthSaveAuthJSON = func(string, []byte) error { return errors.New("unexpected test save") }
	t.Cleanup(func() { oauthHTTPDo, oauthSaveAuthJSON = oldHTTP, oldSave; resetOAuthPendingTest() })
	if !persistent {
		return ""
	}
	dir := t.TempDir()
	configureOAuthPendingTest(t, dir)
	return dir
}
func configureOAuthPendingTest(t *testing.T, dir string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"host": map[string]any{"auth_dir": dir}})
	if err := configureOAuthPending(raw); err != nil {
		t.Fatal(err)
	}
}
func startOAuthPendingTest(t *testing.T, owner, region string) pluginapi.AuthLoginStartResponse {
	t.Helper()
	raw, err := startLoginOwned(nil, region, owner)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	var start pluginapi.AuthLoginStartResponse
	if json.Unmarshal(raw, &env) != nil || !env.OK || json.Unmarshal(env.Result, &start) != nil {
		t.Fatal("invalid start response")
	}
	return start
}
func pollOAuthPanelTest(state string) map[string]any {
	raw, _ := json.Marshal(map[string]string{"state": state})
	return handleManagementOAuthPoll(pluginapi.ManagementRequest{Body: raw})
}
func pollOAuthHostTest(state string, metadata map[string]any) ([]byte, error) {
	raw, _ := json.Marshal(pluginapi.AuthLoginPollRequest{State: state, Metadata: metadata})
	return handlePollLogin(raw)
}
func oauthMockToken(user string) *hostHTTPResponse {
	raw, _ := json.Marshal(deviceTokenResponse{Token: "dt-offline-token", RefreshToken: "drt-offline-token", UserID: user, ExpiresIn: 3600000})
	return &hostHTTPResponse{StatusCode: 200, Body: raw}
}

func TestOAuthPendingMemoryModeAndIsolation(t *testing.T) {
	setupOAuthPendingTest(t, false)
	a := startOAuthPendingTest(t, oauthOwnerPanel, regionCN)
	b := startOAuthPendingTest(t, oauthOwnerPanel, regionIntl)
	if a.State == b.State || !validOAuthState(a.State) || a.Metadata["persistent"] != false {
		t.Fatal("state entropy/memory metadata")
	}
	la, err := lookupOAuthPending(a.State)
	if err != nil {
		t.Fatal(err)
	}
	lb, err := lookupOAuthPending(b.State)
	if err != nil {
		t.Fatal(err)
	}
	if la.nonce == lb.nonce || la.verifier == lb.verifier || la.deviceKey == lb.deviceKey || la.region != regionCN || lb.region != regionIntl {
		t.Fatal("cross-session collision")
	}
	if _, err := pollOAuthHostTest(a.State, nil); err == nil {
		t.Fatal("host must not claim panel state")
	}
	if oauthPending.store != nil {
		t.Fatal("memory-only must not initialize storage")
	}
}

func TestOAuthPendingRestartRestoresProtocolAndCurrentRoute(t *testing.T) {
	dir := setupOAuthPendingTest(t, true)
	start := startOAuthPendingTest(t, oauthOwnerPanel, regionIntl)
	lc, _ := lookupOAuthPending(start.State)
	nonce, verifier, key := lc.nonce, lc.verifier, lc.deviceKey
	route := "old-proxy"
	var seen []string
	oauthHTTPDo = func(req *http.Request) (*hostHTTPResponse, error) {
		seen = append(seen, route)
		if req.URL.Query().Get("nonce") != nonce || req.URL.Query().Get("verifier") != verifier || !strings.HasPrefix(req.URL.String(), upstreamBaseForRegion(regionIntl)+"/") {
			t.Fatal("restored protocol changed")
		}
		return &hostHTTPResponse{StatusCode: 202}, nil
	}
	if got := pollOAuthPanelTest(start.State); got["status"] != "wait" {
		t.Fatal(got)
	}
	resetOAuthPendingTest()
	configureOAuthPendingTest(t, dir)
	route = "new-proxy"
	if got := pollOAuthPanelTest(start.State); got["status"] != "wait" || got["persistent"] != true {
		t.Fatal(got)
	}
	restored, err := lookupOAuthPending(start.State)
	if err != nil || restored.deviceKey != key {
		t.Fatal("device key not restored", err)
	}
	if strings.Join(seen, ",") != "old-proxy,new-proxy" {
		t.Fatal("cached transport policy", seen)
	}
}

func TestOAuthPendingSaveFailureRestartNeverReexchanges(t *testing.T) {
	dir := setupOAuthPendingTest(t, true)
	start := startOAuthPendingTest(t, oauthOwnerPanel, regionCN)
	var exchanges, saves int
	oauthHTTPDo = func(*http.Request) (*hostHTTPResponse, error) { exchanges++; return oauthMockToken("user-one"), nil }
	fail := true
	oauthSaveAuthJSON = func(name string, raw []byte) error {
		saves++
		if name != "qoder-cn-user-one.json" {
			t.Fatal("wrong file", name)
		}
		if fail {
			return errors.New("secret error dt-must-not-echo")
		}
		return nil
	}
	first := pollOAuthPanelTest(start.State)
	if first["status"] != "error" || first["retryable"] != true || strings.Contains(fmt.Sprint(first), "dt-must-not-echo") {
		t.Fatal(first)
	}
	rec, err := oauthPending.store.read(oauthPendingName(start.State))
	if err != nil || rec.Token == nil || rec.Result == nil || rec.Saved {
		t.Fatal("missing unsaved checkpoints", err)
	}
	resetOAuthPendingTest()
	configureOAuthPendingTest(t, dir)
	fail = false
	if got := pollOAuthPanelTest(start.State); got["status"] != "ok" {
		t.Fatal(got)
	}
	resetOAuthPendingTest()
	configureOAuthPendingTest(t, dir)
	if got := pollOAuthPanelTest(start.State); got["status"] != "ok" {
		t.Fatal(got)
	}
	if exchanges != 1 || saves != 2 {
		t.Fatalf("exchange/save counts %d/%d", exchanges, saves)
	}
}

func TestOAuthPendingConcurrentPollIdempotent(t *testing.T) {
	setupOAuthPendingTest(t, true)
	start := startOAuthPendingTest(t, oauthOwnerPanel, regionIntl)
	var exchanges, saves atomic.Int32
	oauthHTTPDo = func(*http.Request) (*hostHTTPResponse, error) {
		exchanges.Add(1)
		return oauthMockToken("parallel-user"), nil
	}
	oauthSaveAuthJSON = func(string, []byte) error { saves.Add(1); return nil }
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := pollOAuthPanelTest(start.State); got["status"] != "ok" {
				t.Errorf("poll: %v", got)
			}
		}()
	}
	wg.Wait()
	if exchanges.Load() != 1 || saves.Load() != 1 {
		t.Fatal("non-idempotent poll", exchanges.Load(), saves.Load())
	}
}

func TestOAuthPendingCheckpointFailureKeepsTokenInMemory(t *testing.T) {
	setupOAuthPendingTest(t, true)
	start := startOAuthPendingTest(t, oauthOwnerPanel, regionCN)
	var exchanges, saves int
	blocker := filepath.Join(oauthPending.store.dir, strings.Repeat("a", 64)+".pending")
	oauthHTTPDo = func(*http.Request) (*hostHTTPResponse, error) {
		exchanges++
		if err := os.WriteFile(blocker, []byte("corrupt"), 0600); err != nil {
			t.Fatal(err)
		}
		return oauthMockToken("checkpoint-user"), nil
	}
	oauthSaveAuthJSON = func(string, []byte) error { saves++; return nil }
	if got := pollOAuthPanelTest(start.State); got["status"] != "error" {
		t.Fatal(got)
	}
	lc, _ := lookupOAuthPending(start.State)
	if lc.token == nil || saves != 0 {
		t.Fatal("token not retained or saved before checkpoint")
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if got := pollOAuthPanelTest(start.State); got["status"] != "ok" {
		t.Fatal(got)
	}
	if exchanges != 1 || saves != 1 {
		t.Fatal("token exchanged twice", exchanges, saves)
	}
}

func TestOAuthPendingHostReplayNoPluginSave(t *testing.T) {
	dir := setupOAuthPendingTest(t, true)
	start := startOAuthPendingTest(t, oauthOwnerHost, regionIntl)
	var exchanges int
	oauthHTTPDo = func(*http.Request) (*hostHTTPResponse, error) { exchanges++; return oauthMockToken("host-user"), nil }
	oauthSaveAuthJSON = func(string, []byte) error { t.Fatal("host RPC must not save"); return nil }
	one, err := pollOAuthHostTest(start.State, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Simulated host save failure followed by process restart: result replays.
	resetOAuthPendingTest()
	configureOAuthPendingTest(t, dir)
	two, err := pollOAuthHostTest(start.State, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(one, two) || exchanges != 1 {
		t.Fatal("host checkpoint replay changed or reexchanged")
	}
	if got := pollOAuthPanelTest(start.State); got["status"] != "error" {
		t.Fatal("panel claimed host state")
	}
}

func TestOAuthPendingIdentityFailureRetriesOnlyQoderIdentity(t *testing.T) {
	dir := setupOAuthPendingTest(t, true)
	start := startOAuthPendingTest(t, oauthOwnerHost, regionCN)
	var exchanges, identities int
	oauthHTTPDo = func(req *http.Request) (*hostHTTPResponse, error) {
		if strings.Contains(req.URL.Path, "jobToken/exchange") {
			exchanges++
			return &hostHTTPResponse{StatusCode: 200, Body: []byte(`{"token":"jt-offline","refresh_token":"jrt-offline"}`)}, nil
		}
		if strings.Contains(req.URL.Path, "userinfo") {
			identities++
			if identities == 1 {
				return nil, errors.New("offline identity failure")
			}
			return &hostHTTPResponse{StatusCode: 200, Body: []byte(`{"id":"pat-user","name":"offline"}`)}, nil
		}
		t.Fatal("unexpected endpoint", req.URL.Path)
		return nil, nil
	}
	if _, err := pollOAuthHostTest(start.State, map[string]any{"pat": "pt-offline"}); err == nil {
		t.Fatal("expected identity failure")
	}
	resetOAuthPendingTest()
	configureOAuthPendingTest(t, dir)
	if _, err := pollOAuthHostTest(start.State, nil); err != nil {
		t.Fatal(err)
	}
	if exchanges != 1 || identities != 2 {
		t.Fatal("wrong retry stage", exchanges, identities)
	}
}

func TestOAuthPendingSecurityBoundaries(t *testing.T) {
	setupOAuthPendingTest(t, true)
	start := startOAuthPendingTest(t, oauthOwnerPanel, regionCN)
	store := oauthPending.store
	file := filepath.Join(store.dir, oauthPendingName(start.State))
	for path, mode := range map[string]os.FileMode{store.dir: 0700, file: 0600, filepath.Join(store.dir, ".key"): 0600, filepath.Join(store.dir, ".lock"): 0600} {
		st, err := os.Stat(path)
		if err != nil || st.Mode().Perm() != mode {
			t.Fatalf("permissions %v %v", st, err)
		}
	}
	if second, err := openOAuthPendingStore(store.dir); err == nil {
		if second != nil {
			second.close()
		}
		t.Fatal("second process ownership accepted")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, bytes.Replace(raw, []byte(`"region":"cn"`), []byte(`"region":"intl"`), 1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.read(oauthPendingName(start.State)); err == nil {
		t.Fatal("tampered valid JSON accepted")
	}
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.read(oauthPendingName(start.State)); err == nil {
		t.Fatal("public file accepted")
	}
	if err := os.Chmod(file, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(store.dir, strings.Repeat("b", 64)+".pending")
	if err := os.Link(file, link); err != nil {
		t.Fatal(err)
	}
	if _, err := store.read(oauthPendingName(start.State)); err == nil {
		t.Fatal("hardlinked credential accepted")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if _, err := store.read(filepath.Base(link)); err == nil {
		t.Fatal("symlink credential accepted")
	}
	if _, err := store.read("../escape.pending"); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func TestOAuthPendingRejectsConfiguredBadDirectory(t *testing.T) {
	for _, kind := range []string{"relative", "symlink", "mode"} {
		t.Run(kind, func(t *testing.T) {
			setupOAuthPendingTest(t, false)
			dir := t.TempDir()
			switch kind {
			case "relative":
				dir = "relative-auth"
			case "symlink":
				target := t.TempDir()
				link := filepath.Join(dir, "alias")
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
				dir = link
			case "mode":
				if err := os.Mkdir(filepath.Join(dir, oauthPendingDir), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(filepath.Join(dir, oauthPendingDir), 0755); err != nil {
					t.Fatal(err)
				}
			}
			raw, _ := json.Marshal(map[string]any{"host": map[string]any{"auth_dir": dir}})
			if configureOAuthPending(raw) == nil {
				t.Fatal("unsafe directory accepted")
			}
			if _, err := startLoginWithRegion(nil, regionCN); err == nil {
				t.Fatal("bad directory silently fell back to memory")
			}
		})
	}
}

func TestOAuthPendingSizeCountExpiryAndCorruption(t *testing.T) {
	setupOAuthPendingTest(t, true)
	start := startOAuthPendingTest(t, oauthOwnerPanel, regionCN)
	lc, _ := lookupOAuthPending(start.State)
	rec := oauthRecord(start.State, lc)
	rec.Verifier = strings.Repeat("x", oauthPendingMaxBytes)
	if oauthPending.store.write(rec) == nil {
		t.Fatal("oversized record accepted")
	}
	// Capacity includes unknown files; never scan an unbounded directory.
	for i := 0; i < oauthPendingMaxFiles; i++ {
		if err := os.WriteFile(filepath.Join(oauthPending.store.dir, fmt.Sprintf("unknown-%d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := startLoginWithRegion(nil, regionCN); err == nil {
		t.Fatal("file count limit ignored")
	}
	lc.mu.Lock()
	lc.expires = time.Now().Add(-time.Second)
	lc.mu.Unlock()
	pruneOAuthPending(time.Now())
	if _, err := lookupOAuthPending(start.State); err == nil {
		t.Fatal("expired state restored")
	}
}

func TestOAuthPendingTransportErrorNoSecrets(t *testing.T) {
	setupOAuthPendingTest(t, false)
	start := startOAuthPendingTest(t, oauthOwnerPanel, regionCN)
	oauthHTTPDo = func(req *http.Request) (*hostHTTPResponse, error) {
		return nil, fmt.Errorf("url=%s dt-secret drt-secret", req.URL.String())
	}
	res := pollOAuthPanelTest(start.State)
	errText := fmt.Sprint(res["error"])
	lc, _ := lookupOAuthPending(start.State)
	for _, secret := range []string{lc.verifier, lc.nonce, "dt-secret", "drt-secret"} {
		if strings.Contains(errText, secret) {
			t.Fatal("secret leaked in error")
		}
	}
}
