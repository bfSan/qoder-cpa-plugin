package upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// v0.12.37: advertised Intl ids carry the "-intl" namespace suffix;
// resolveMode must send the bare model name upstream.
func TestResolveModeStripsIntlSuffix(t *testing.T) {
	mode, strategy, name := resolveMode("gpt-5.2-intl")
	if mode != "code" || strategy != "manual" || name != "gpt-5.2" {
		t.Errorf("resolveMode(gpt-5.2-intl)=%q/%q/%q", mode, strategy, name)
	}
	if _, _, name := resolveMode("auto"); name != "" {
		t.Errorf("auto should stay virtual, got %q", name)
	}
	if _, _, name := resolveMode("work"); name != "" {
		t.Errorf("work should stay virtual, got %q", name)
	}
	if _, strategy, name := resolveMode("claude-sonnet-4-5-intl"); strategy != "manual" || name != "claude-sonnet-4-5" {
		t.Errorf("resolveMode(claude-sonnet-4-5-intl) strategy/name=%q/%q", strategy, name)
	}
}

// v0.12.47: the backend validates Origin/Referer against the JWT session's
// real web origin (now work.trae.ai, formerly solo.trae.ai) and answers a
// bare 401 on mismatch. Default must be the new origin; a per-account
// RefererOrigin override wins; trailing slashes are normalized.
func TestBuildHeadersWebOrigin(t *testing.T) {
	h := buildHeaders(&Auth{AccessToken: "tok"})
	if got := h.Get("Origin"); got != "https://work.trae.ai" {
		t.Errorf("default Origin = %q, want https://work.trae.ai", got)
	}
	if got := h.Get("Referer"); got != "https://work.trae.ai/" {
		t.Errorf("default Referer = %q, want https://work.trae.ai/", got)
	}
	if got := h.Get("Authorization"); got != "Cloud-IDE-JWT tok" {
		t.Errorf("Authorization = %q", got)
	}

	h2 := buildHeaders(&Auth{RefererOrigin: "https://solo.trae.ai/"})
	if got := h2.Get("Origin"); got != "https://solo.trae.ai" {
		t.Errorf("override Origin = %q, want normalized solo host", got)
	}
	if got := h2.Get("Referer"); got != "https://solo.trae.ai/" {
		t.Errorf("override Referer = %q", got)
	}
}

// v0.12.48: x-trae-user-timezone is forwarded only when the auth file carries
// a timezone (OmniRoute #13255 ships it as the second half of the 401 fix);
// empty must omit the header to match upstream's conditional send.
func TestBuildHeadersUserTimezone(t *testing.T) {
	h := buildHeaders(&Auth{AccessToken: "tok", Timezone: "Asia/Shanghai"})
	if got := h.Get("x-trae-user-timezone"); got != "Asia/Shanghai" {
		t.Errorf("timezone header = %q, want Asia/Shanghai", got)
	}

	h2 := buildHeaders(&Auth{AccessToken: "tok"})
	if got := h2.Get("x-trae-user-timezone"); got != "" {
		t.Errorf("timezone header should be omitted, got %q", got)
	}
}

// v0.12.53: a 200 envelope carrying a business error code, or one with an
// empty data list, must FAIL — previously both silently produced zero
// dynamic models, so model.for_auth advertised just the auto/work virtuals
// with no diagnostic (field report: "trae 国际服务模型拉取失效"). Errors make
// the caller fall back to the static catalog and log the real reason.
func TestFetchModelsBusinessAndEmptyEnvelopes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "bizerr"):
			_, _ = w.Write([]byte(`{"code":1001,"message":"We're sorry, but we are not able to authenticate you."}`))
		case strings.Contains(r.URL.Path, "empty"):
			_, _ = w.Write([]byte(`{"code":0,"data":[]}`))
		case strings.Contains(r.URL.Path, "nulldata"):
			_, _ = w.Write([]byte(`{"code":0,"data":null}`))
		default: // ok
			_, _ = w.Write([]byte(`{"code":0,"data":[{"name":"m1"},{"name":""},{"name":"m2"}]}`))
		}
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}
	a := &Auth{AccessToken: "tok", RefererOrigin: "https://work.trae.ai"}

	c.BaseURL = srv.URL + "/bizerr"
	if _, err := c.FetchModels(a); err == nil || !strings.Contains(err.Error(), "1001") {
		t.Errorf("bizerr envelope: want code 1001 error, got %v", err)
	}
	c.BaseURL = srv.URL + "/empty"
	if _, err := c.FetchModels(a); err == nil || !strings.Contains(err.Error(), "empty list") {
		t.Errorf("empty data: want empty-list error, got %v", err)
	}
	c.BaseURL = srv.URL + "/nulldata"
	if _, err := c.FetchModels(a); err == nil || !strings.Contains(err.Error(), "empty list") {
		t.Errorf("null data: want empty-list error, got %v", err)
	}
	c.BaseURL = srv.URL + "/ok"
	got, err := c.FetchModels(a)
	if err != nil {
		t.Fatalf("ok envelope: unexpected error %v", err)
	}
	if len(got) != 2 || got[0] != "m1" || got[1] != "m2" {
		t.Errorf("ok envelope: want [m1 m2], got %v", got)
	}
}
