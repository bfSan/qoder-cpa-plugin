package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type payloadThenError struct{ done bool }

func (r *payloadThenError) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	return copy(p, "partial"), io.ErrUnexpectedEOF
}
func (r *payloadThenError) Close() error { return nil }

func TestHostStreamReaderPreservesPartialDataAndTerminalError(t *testing.T) {
	s := &hostHTTPStream{body: &payloadThenError{}, ctx: context.Background()}
	defer s.Close()
	b, err := io.ReadAll(newHostStreamReader(s))
	if string(b) != "partial" || err == nil || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("data=%q error=%v", b, err)
	}
}

func TestRedactQuotedJSONAndShortPKCEFields(t *testing.T) {
	for _, s := range []string{
		`{"accessToken":"shortsecret","refresh_token":"longsecret","cookie":"a=private; b=private"}`,
		`{"nonce":"veryshort","verifier":"vsecret","authorization":"Bearer short"}`,
		`https://example.test/poll?nonce=noncesecret&code_verifier=verifiersecret&public=keep`,
		`{"personalToken":"pt-secret","access_token":"abc\\\"def"}`,
	} {
		out := redactSecrets(s)
		for _, secret := range []string{"shortsecret", "longsecret", "private", "veryshort", "vsecret", "Bearer short", "noncesecret", "verifiersecret", "pt-secret", "abc"} {
			if strings.Contains(out, secret) {
				t.Fatalf("secret %q remained in redacted text", secret)
			}
		}
	}
	if out := redactSecrets(`{"code":110,"message":"billing daily count exceeded"}`); !strings.Contains(out, "billing daily count exceeded") {
		t.Fatal("redactor destroyed quota semantics")
	}
}
