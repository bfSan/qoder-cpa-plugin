package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestErrorEnvelopeForCarriesStatus(t *testing.T) {
	raw := errorEnvelopeFor(&statusError{status: http.StatusTooManyRequests, err: errors.New("upstream 429: quota")})
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.OK || env.Error == nil {
		t.Fatalf("expected error envelope, got ok=%v", env.OK)
	}
	if env.Error.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("http_status = %d, want 429", env.Error.HTTPStatus)
	}
}

func TestErrorEnvelopeForPlainErrorOmitsStatus(t *testing.T) {
	raw := errorEnvelopeFor(errors.New("plain failure"))
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Error == nil || env.Error.HTTPStatus != 0 {
		t.Fatalf("expected http_status omitted, got %+v", env.Error)
	}
}

// TestEmptyStreamErrorCarriesNoHTTPStatus locks the classification fix for
// 2026-09-21: an upstream that accepts the request then closes the SSE stream
// before the first payload is a transport-lifecycle event. It must not carry
// a 401/402/429 status, otherwise CPA promotes the single-model failure into a
// credential-wide cooldown.
func TestEmptyStreamErrorCarriesNoHTTPStatus(t *testing.T) {
	err := emptyStreamError()
	var se *statusError
	if errors.As(err, &se) {
		t.Fatalf("empty stream must not carry an HTTP status, got %d", se.StatusCode())
	}
	if !strings.Contains(err.Error(), "unexpected EOF") {
		t.Fatalf("empty stream should be classified as a connection lifecycle failure, got %q", err.Error())
	}
	// The same error must survive the RPC boundary without a status field.
	raw := errorEnvelopeFor(err)
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Error == nil {
		t.Fatal("expected error envelope")
	}
	if env.Error.HTTPStatus != 0 {
		t.Fatalf("http_status = %d, want omitted", env.Error.HTTPStatus)
	}
}

// TestEmptyStreamErrorStillCoolsOnlyThePair keeps the plugin-side blast radius
// pinned to one (account, model) tuple.
func TestEmptyStreamErrorStillCoolsOnlyThePair(t *testing.T) {
	resetCooldowns(t)
	err := emptyStreamError()
	recordUpstreamFailure("qoder-a", "qfmodel", 0, err.Error())
	if !modelIsCooling("qoder-a", "qfmodel") {
		t.Fatal("empty_stream should still cool the specific model pair")
	}
	if modelIsCooling("qoder-a", "gmodel") {
		t.Fatal("a failed model must not cool sibling models on the same auth")
	}
}

// TestUpstreamStatusErrorPolicy locks the 0.8.13 matrix: only 401/402/429
// pass their status to the host cooldown layer; everything else (413/input
// too large, 403, 5xx) stays status-less — request-level or unevidenced
// shapes must not cool the credential beyond the historical default.
func TestUpstreamStatusErrorPolicy(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   int
	}{
		{"402 payment", 402, 402},
		{"401 dead token", 401, 401},
		{"429 free quota drained", 429, 429},
		{"413 input too large", 413, 0},
		{"403 unknown shape", 403, 0},
		{"400 business", 400, 0},
		{"500 server", 500, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := errors.New("translated")
			err := upstreamStatusError(tc.status, base)
			var se *statusError
			if tc.want == 0 {
				if errors.As(err, &se) {
					t.Fatalf("status %d: expected plain error, got statusError{%d}", tc.status, se.status)
				}
				if !errors.Is(err, base) {
					t.Fatalf("plain error must wrap base")
				}
				return
			}
			if !errors.As(err, &se) {
				t.Fatalf("status %d: expected statusError", tc.status)
			}
			if se.status != tc.want {
				t.Fatalf("status = %d, want %d", se.status, tc.want)
			}
		})
	}
}

// TestStreamErrorFrameUsesTopLevelErrorField pins the wire shape of a terminal
// stream error: the host only classifies failures that arrive in
// rpcStreamEmitRequest.Error. An `{"error":...}` JSON *payload* is a data
// frame as far as the host is concerned, so CPA synthesizes its own
// "empty_stream" error and cools the whole auth instead of the single model.
func TestStreamErrorFrameUsesTopLevelErrorField(t *testing.T) {
	raw, err := streamErrorFrame("stream-1", "empty_stream: upstream stream closed before first payload (unexpected EOF)")
	if err != nil {
		t.Fatalf("streamErrorFrame: %v", err)
	}
	var frame struct {
		StreamID string          `json:"stream_id"`
		Payload  json.RawMessage `json:"payload"`
		Error    string          `json:"error"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if frame.StreamID != "stream-1" {
		t.Fatalf("stream_id = %q, want stream-1", frame.StreamID)
	}
	if len(frame.Payload) != 0 {
		t.Fatalf("payload must stay empty for an error frame, got %s", frame.Payload)
	}
	if !strings.Contains(frame.Error, "unexpected EOF") {
		t.Fatalf("error must carry the lifecycle wording, got %q", frame.Error)
	}
}

// TestNextCheckinTimeSkipsBeforeOpeningHour guards the 10:00 opening-hour
// fix: from 09:59 the next auto tick must be 10:00, never the stale 09:00.
func TestNextCheckinTimeSkipsBeforeOpeningHour(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 9, 20, 9, 59, 0, 0, loc)
	next := nextCheckinTime(now)
	if next.Hour() != 10 || next.Day() != now.Day() {
		t.Fatalf("next = %s, want today 10:00", next)
	}
}
