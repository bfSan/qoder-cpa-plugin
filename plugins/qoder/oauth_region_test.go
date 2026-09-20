package main

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestRequestedLoginRegion(t *testing.T) {
	tests := []struct {
		name     string
		metadata map[string]any
		want     string
	}{
		{name: "nil", metadata: nil, want: ""},
		{name: "cn string", metadata: map[string]any{"region": "cn"}, want: regionCN},
		{name: "intl string", metadata: map[string]any{"region": "intl"}, want: regionIntl},
		{name: "global alias", metadata: map[string]any{"region": "GLOBAL"}, want: regionIntl},
		{name: "string slice", metadata: map[string]any{"region": []string{"intl", "cn"}}, want: regionIntl},
		{name: "any slice", metadata: map[string]any{"region": []any{"cn"}}, want: regionCN},
		{name: "invalid falls back", metadata: map[string]any{"region": "eu"}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := requestedLoginRegion(tt.metadata); got != tt.want {
				t.Fatalf("requestedLoginRegion() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestManagementOAuthStartPreservesRequestedRegion(t *testing.T) {
	for _, region := range []string{regionCN, regionIntl} {
		t.Run(region, func(t *testing.T) {
			res := handleManagementOAuthStart(pluginapi.ManagementRequest{
				Body: []byte(`{"region":"` + region + `"}`),
			})
			if res["error"] != nil {
				t.Fatalf("start error = %v", res["error"])
			}
			if got := res["region"]; got != region {
				t.Fatalf("region = %v, want %q", got, region)
			}
			rawURL, _ := res["url"].(string)
			wantPrefix := qoderWebsiteFor(region) + "/device/selectAccounts?"
			if !strings.HasPrefix(rawURL, wantPrefix) {
				t.Fatalf("url = %q, want prefix %q", rawURL, wantPrefix)
			}
			parsed, err := url.Parse(rawURL)
			if err != nil {
				t.Fatalf("url parse: %v", err)
			}
			if parsed.Query().Get("challenge") == "" || parsed.Query().Get("nonce") == "" {
				t.Fatalf("login URL is missing PKCE/nonce parameters: %q", rawURL)
			}
			base, _ := res["url_base"].(string)
			if strings.Contains(base, "?") || strings.Contains(base, "&amp;") {
				t.Fatalf("url_base must be query-free and unescaped: %q", base)
			}
			params, ok := res["query"].(map[string]string)
			if !ok {
				t.Fatalf("query = %#v, want map[string]string", res["query"])
			}
			if params["challenge"] != parsed.Query().Get("challenge") ||
				params["nonce"] != parsed.Query().Get("nonce") {
				t.Fatalf("structured query does not match login URL: %#v vs %q", params, rawURL)
			}
			state, _ := res["state"].(string)
			if state == "" {
				t.Fatal("start response is missing state")
			}
			loginStates.Delete(state) // avoid leaking test state into other tests
		})
	}
}

func TestManagementOAuthPollRejectsUnknownState(t *testing.T) {
	res := handleManagementOAuthPoll(pluginapi.ManagementRequest{
		Body: []byte(`{"state":"missing"}`),
	})
	if res["status"] != "error" {
		t.Fatalf("status = %v, want error for unknown state", res["status"])
	}
}

func TestStartLoginWithRegionKeepsBothRegions(t *testing.T) {
	for _, region := range []string{regionCN, regionIntl} {
		t.Run(region, func(t *testing.T) {
			raw, err := startLoginWithRegion(nil, region)
			if err != nil {
				t.Fatalf("startLoginWithRegion() error = %v", err)
			}
			var env envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatalf("envelope parse: %v", err)
			}
			if !env.OK {
				t.Fatalf("login start returned error: %s", raw)
			}
			var resp pluginapi.AuthLoginStartResponse
			if err := json.Unmarshal(env.Result, &resp); err != nil {
				t.Fatalf("response parse: %v", err)
			}
			wantURL := qoderWebsiteFor(region)
			if !strings.HasPrefix(resp.URL, wantURL+"/device/selectAccounts?") {
				t.Fatalf("URL = %q, want prefix %q", resp.URL, wantURL)
			}
			if got := resp.Metadata["region"]; got != region {
				t.Fatalf("metadata region = %v, want %q", got, region)
			}
		})
	}
}
