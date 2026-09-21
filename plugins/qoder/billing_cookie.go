package main

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
)

// Qoder's billing surface (/sash/api/v1/*, /api/v2/*) no longer authenticates
// a bare dt- device token. Observed live 2026-09-21: every call answers 401
// `{"code":"UNAUTHORIZED","message":"missing cookie header"}`. The desktop
// client sends authenticated requests through an Electron session, which
// attaches the browser cookie jar automatically and mirrors the anti-CSRF
// cookie into an X-CSRF-Token header.
//
// The plugin has no browser session, so it replays the same handshake:
// bootstrap once per account to obtain acw_tc + qoder_csrf_token, then send
// Cookie + X-CSRF-Token + Authorization on every billing call.
//
// Jars are per-account (keyed by credential identity), never shared, so
// multi-account deployments cannot cross-contaminate sessions — the same
// isolation guarantee the previous "no cookie jar" comment required.

const csrfCookieName = "qoder_csrf_token"

var (
	billingJarMu sync.Mutex
	billingJars  sync.Map // accountKey -> *billingJar
)

type billingJar struct {
	mu           sync.Mutex
	jar          *cookiejar.Jar
	bootstrapped bool
}

func (b *billingJar) absorb(baseURL string, headers http.Header) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.jar == nil {
		return
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return
	}
	cookies := readSetCookies(headers)
	if len(cookies) > 0 {
		b.jar.SetCookies(u, cookies)
	}
}

// billingJarFor returns the per-account jar, creating it lazily.
func billingJarFor(sa *storedAuth) *billingJar {
	key := billingAccountKey(sa)
	if v, ok := billingJars.Load(key); ok {
		return v.(*billingJar)
	}
	billingJarMu.Lock()
	defer billingJarMu.Unlock()
	if v, ok := billingJars.Load(key); ok {
		return v.(*billingJar)
	}
	var jar *cookiejar.Jar
	if created, err := cookiejar.New(nil); err == nil {
		jar = created
	}
	b := &billingJar{jar: jar}
	billingJars.Store(key, b)
	return b
}

// billingAccountKey isolates jars per credential. Falls back to the token
// prefix so two auth files for the same account still share one jar.
func billingAccountKey(sa *storedAuth) string {
	if sa == nil {
		return "anonymous"
	}
	if uid := strings.TrimSpace(sa.Account.UID); uid != "" {
		return authRegion(sa) + "|" + uid
	}
	token := strings.TrimSpace(sa.Auth.AccessToken)
	if len(token) > 8 {
		return authRegion(sa) + "|" + token[:8]
	}
	return authRegion(sa) + "|" + token
}

// readSetCookies parses Set-Cookie response headers into cookies the jar can
// store. net/http exposes the reverse (Response.Cookies) but not a header-only
// helper, and the host bridge hands us raw headers rather than a Response.
func readSetCookies(headers http.Header) []*http.Cookie {
	if headers == nil {
		return nil
	}
	raw, ok := headers["Set-Cookie"]
	if !ok {
		raw = headers["Set-cookie"]
	}
	out := make([]*http.Cookie, 0, len(raw))
	for _, line := range raw {
		if c := parseOneCookie(line); c != nil {
			out = append(out, c)
		}
	}
	return out
}

func parseOneCookie(line string) *http.Cookie {
	parts := strings.Split(line, ";")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		return nil
	}
	nameValue := strings.SplitN(strings.TrimSpace(parts[0]), "=", 2)
	if len(nameValue) != 2 || nameValue[0] == "" {
		return nil
	}
	c := &http.Cookie{Name: nameValue[0], Value: nameValue[1], Raw: line}
	for _, attr := range parts[1:] {
		attr = strings.TrimSpace(attr)
		if attr == "" {
			continue
		}
		keyValue := strings.SplitN(attr, "=", 2)
		key := strings.ToLower(strings.TrimSpace(keyValue[0]))
		value := ""
		if len(keyValue) == 2 {
			value = keyValue[1]
		}
		switch key {
		case "path":
			c.Path = value
		case "domain":
			c.Domain = value
		case "secure":
			c.Secure = true
		case "httponly":
			c.HttpOnly = true
		}
	}
	return c
}

// resetBillingJarForTest drops cached jars so tests start clean.
func resetBillingJarForTest() {
	billingJars.Range(func(key, _ any) bool {
		billingJars.Delete(key)
		return true
	})
}

// bootstrapBillingSession performs the one-shot call that makes upstream emit
// its session cookies. Any endpoint works; /api/v1/me is the cheapest. The
// response status is irrelevant — we only need Set-Cookie — but a network
// error means we simply proceed without cookies.
func bootstrapBillingSession(sa *storedAuth) {
	b := billingJarFor(sa)
	b.mu.Lock()
	if b.bootstrapped || b.jar == nil {
		b.mu.Unlock()
		return
	}
	b.bootstrapped = true
	b.mu.Unlock()

	base := upstreamBaseFor(sa)
	req, err := http.NewRequest(http.MethodGet, base+"/api/v1/me", nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return
	}
	b.absorb(base, resp.Headers)
}

// cookiesFor builds the Cookie header value and the CSRF token for a base URL.
func (b *billingJar) cookiesFor(baseURL string) (string, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.jar == nil {
		return "", ""
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", ""
	}
	var parts []string
	csrf := ""
	for _, c := range b.jar.Cookies(u) {
		parts = append(parts, c.Name+"="+c.Value)
		if c.Name == csrfCookieName {
			csrf = c.Value
		}
	}
	return strings.Join(parts, "; "), csrf
}

// applyBillingSessionHeaders attaches Cookie + X-CSRF-Token to a billing
// request, bootstrapping the session on first use.
func applyBillingSessionHeaders(req *http.Request, sa *storedAuth) {
	if req == nil || sa == nil {
		return
	}
	bootstrapBillingSession(sa)
	base := upstreamBaseFor(sa)
	cookie, csrf := billingJarFor(sa).cookiesFor(base)
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
}
