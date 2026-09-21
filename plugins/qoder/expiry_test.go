package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The stored expiresAt is seconds by contract, but one write path produced a
// millisecond magnitude. Reading that as seconds resolves to 1970, so the
// token looks long expired and refresh scheduling breaks.

func TestTokenExpiryUnixNormalizesMillis(t *testing.T) {
	nowSeconds := time.Now().Unix()
	if got := tokenExpiryUnix(nowSeconds * 1000); got != nowSeconds {
		t.Fatalf("millis normalised to %d, want %d", got, nowSeconds)
	}
	if got := tokenExpiryUnix(nowSeconds); got != nowSeconds {
		t.Fatalf("seconds changed to %d, want %d", got, nowSeconds)
	}
	if got := tokenExpiryUnix(0); got != 0 {
		t.Fatalf("zero = %d, want 0", got)
	}
}

// A zero expiry must not read as expired: otherwise a credential with a
// missing timestamp would be considered dead and refreshed constantly.
func TestTokenExpiredTreatsUnknownAsAlive(t *testing.T) {
	if tokenExpired(0, time.Now()) {
		t.Fatal("unknown expiry reported as expired")
	}
	if !tokenExpired(time.Now().Add(-time.Hour).Unix(), time.Now()) {
		t.Fatal("past expiry reported as alive")
	}
	if tokenExpired(time.Now().Add(time.Hour).Unix(), time.Now()) {
		t.Fatal("future expiry reported as expired")
	}
}

func TestPreserveExpiryNormalizesBothSides(t *testing.T) {
	nowSeconds := time.Now().Unix()
	if got := preserveExpiry(nowSeconds*1000, 0); got != nowSeconds {
		t.Fatalf("preserveExpiry = %d, want %d", got, nowSeconds)
	}
	if got := preserveExpiry(0, nowSeconds*1000); got != nowSeconds {
		t.Fatalf("preserveExpiry fallback = %d, want %d", got, nowSeconds)
	}
	if got := preserveExpiry(0, 0); got != 0 {
		t.Fatalf("preserveExpiry of unknown = %d, want 0", got)
	}
}

// Billing 401s must be recognised as credential rejections so the panel stops
// presenting a stale snapshot as a successful check-in.
func TestIsAuthRejectedErrorMatchesUpstreamShapes(t *testing.T) {
	cases := []string{
		`checkin status http 401 body={"code":"UNAUTHORIZED"}`,
		`quota/usage http 401 body=...`,
		`{"errorCode":"Unauthorized","errorMessage":"User not authenticated"}`,
		`missing cookie header`,
	}
	for _, msg := range cases {
		if !isAuthRejectedError(errors.New(msg)) {
			t.Fatalf("%q not recognised as auth rejection", msg)
		}
	}
	for _, msg := range []string{"http 500 body=boom", "timeout", ""} {
		if isAuthRejectedError(errors.New(msg)) {
			t.Fatalf("%q wrongly treated as auth rejection", msg)
		}
	}
}

// The cookie handshake must attach Cookie + X-CSRF-Token so the billing
// surface accepts the device token again.
func TestBillingSessionAttachesCookieAndCSRF(t *testing.T) {
	resetBillingJarForTest()
	defer resetBillingJarForTest()

	var seenCookie, seenCSRF string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/me" {
			http.SetCookie(w, &http.Cookie{Name: csrfCookieName, Value: "csrf-abc", Path: "/"})
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		seenCookie = r.Header.Get("Cookie")
		seenCSRF = r.Header.Get("X-CSRF-Token")
		_, _ = w.Write([]byte(`{"status":"CLAIMABLE"}`))
	}))
	defer server.Close()
	restore := setUpstreamBaseForTest(regionCN, server.URL)
	defer restore()

	sa := &storedAuth{Auth: storedTokens{AccessToken: "dt-test", Domain: domainForRegion(regionCN), Region: regionCN}}
	if _, err := fetchCheckinStatus(sa); err != nil {
		t.Fatalf("fetchCheckinStatus error = %v", err)
	}
	if seenCookie == "" {
		t.Fatal("Cookie header not attached to the billing call")
	}
	if seenCSRF != "csrf-abc" {
		t.Fatalf("X-CSRF-Token = %q, want csrf-abc", seenCSRF)
	}
}

func TestReadSetCookiesParsesHeaders(t *testing.T) {
	headers := http.Header{}
	headers.Add("Set-Cookie", "a=1; Path=/")
	headers.Add("Set-Cookie", "b=2; Path=/; HttpOnly; Secure")
	got := readSetCookies(headers)
	if len(got) != 2 {
		t.Fatalf("parsed %d cookies, want 2", len(got))
	}
	if got[0].Name != "a" || got[0].Value != "1" {
		t.Fatalf("first cookie = %+v", got[0])
	}
	if !got[1].HttpOnly || !got[1].Secure {
		t.Fatalf("second cookie flags lost: %+v", got[1])
	}
}
