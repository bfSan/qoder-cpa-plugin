package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestDashboardDoesNotExposeManualSelectionFields(t *testing.T) {
	oldList := panelHostAuthList
	panelHostAuthList = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{
			ID:        "qoder-a",
			AuthIndex: "idx-a",
			Name:      "qoder-a.json",
			Label:     "qoder-a",
		}}, nil
	}
	t.Cleanup(func() { panelHostAuthList = oldList })

	resp := buildDashboardEx(false, false)
	if _, exists := resp["active_auth"]; exists {
		t.Fatalf("dashboard exposes active_auth: %#v", resp["active_auth"])
	}
	accounts, ok := resp["accounts"].([]wbAccount)
	if !ok || len(accounts) != 1 {
		t.Fatalf("accounts = %#v", resp["accounts"])
	}
	raw, err := json.Marshal(accounts[0])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"selected"`)) {
		t.Fatalf("account exposes selected: %s", raw)
	}
}

func TestDashboardLightLoadCarriesCachedCreditsSnapshot(t *testing.T) {
	const authID = "qoder-snapshot"
	oldList := panelHostAuthList
	oldBundle := panelHostAuthBundle
	panelHostAuthList = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{
			ID:        authID,
			AuthIndex: "idx-snapshot",
			Name:      "qoder-snapshot.json",
			Label:     "snapshot",
		}}, nil
	}
	panelHostAuthBundle = func(string) (*storedAuth, *hostAuthPhysical, error) {
		return &storedAuth{}, &hostAuthPhysical{}, nil
	}
	t.Cleanup(func() {
		panelHostAuthList = oldList
		panelHostAuthBundle = oldBundle
	})
	accountCache.Store(authID, &accountCacheEntry{
		plan:    "Pro",
		credits: &creditsSummary{TotalRemain: 362, TotalUsed: 38, TotalSize: 400},
		fetched: time.Now(),
	})
	t.Cleanup(func() { accountCache.Delete(authID) })

	resp := buildDashboardEx(false, false)
	accounts, ok := resp["accounts"].([]wbAccount)
	if !ok || len(accounts) != 1 {
		t.Fatalf("accounts = %#v", resp["accounts"])
	}
	if accounts[0].Credits == nil {
		t.Fatal("light load should carry the cached credits snapshot")
	}
	if accounts[0].Credits.TotalRemain != 362 || accounts[0].Credits.TotalUsed != 38 {
		t.Fatalf("credits = %#v, want cached 362/38", accounts[0].Credits)
	}
}

func TestPanelUsesQoderBrandingAndNoManualSelectionUI(t *testing.T) {
	html := string(panelHTML)
	for _, want := range []string{"Qoder 账号面板", "导入 Qoder 凭证"} {
		if !strings.Contains(html, want) {
			t.Fatalf("panel HTML is missing %q", want)
		}
	}
	for _, banned := range []string{"QoderWork 账号面板", "选用", "使用中", "active_auth", "activeAuthId"} {
		if strings.Contains(html, banned) {
			t.Fatalf("panel HTML still contains %q", banned)
		}
	}
}

func TestLabelForAuthUsesQoderBrandingAndRegion(t *testing.T) {
	cn := labelForAuth(&storedAuth{Account: storedAccount{Nickname: "alice"}})
	if cn != "alice [CN]" {
		t.Fatalf("CN label = %q, want alice [CN]", cn)
	}
	intl := labelForAuth(&storedAuth{
		Auth:    storedTokens{Domain: domainIntl},
		Account: storedAccount{Nickname: "bob"},
	})
	if intl != "bob [Intl]" {
		t.Fatalf("Intl label = %q, want bob [Intl]", intl)
	}
}
