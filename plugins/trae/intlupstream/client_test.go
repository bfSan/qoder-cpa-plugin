package upstream

import "testing"

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
