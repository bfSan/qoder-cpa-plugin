package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// -----------------------------------------------------------------------------
// Test harness: a fake host RPC bridge + a gateway that answers HTTP 200 with a
// refusal inside the SSE envelope (the exact shape that used to be misread).
// -----------------------------------------------------------------------------

// fakeHost records every stream frame the plugin emits so a test can assert
// whether the plugin sent a chunk or a terminal error.
type fakeHost struct {
	mu     sync.Mutex
	frames []map[string]any
}

func (h *fakeHost) record(raw []byte) {
	var frame map[string]any
	if json.Unmarshal(raw, &frame) != nil {
		return
	}
	h.mu.Lock()
	h.frames = append(h.frames, frame)
	h.mu.Unlock()
}

// chunks lists payloads emitted as ordinary data chunks.
//
// rpcStreamEmitRequest.Payload is []byte on the host side, so Go marshals the
// plugin's payload as base64 and the host decodes it back; the fake host does
// the same before asserting on the text.
func (h *fakeHost) chunks() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []string{}
	for _, f := range h.frames {
		payload, ok := f["payload"].(string)
		if !ok || payload == "" {
			continue
		}
		if decoded, err := base64.StdEncoding.DecodeString(payload); err == nil {
			out = append(out, string(decoded))
			continue
		}
		out = append(out, payload)
	}
	return out
}

// streamErrors lists terminal errors emitted via the RPC error field.
func (h *fakeHost) streamErrors() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []string{}
	for _, f := range h.frames {
		if msg, ok := f["error"].(string); ok && msg != "" {
			out = append(out, msg)
		}
	}
	return out
}

// installFakeHost replaces the host RPC bridge for the duration of one test.
func installFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	fake := &fakeHost{}
	prev := hostCall
	hostCall = func(method string, request []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostStreamEmit:
			fake.record(request)
			// Answer like the host bridge: an OK envelope with an empty result.
			return []byte(`{"ok":true}`), nil
		default:
			return []byte(`{"ok":true}`), nil
		}
	}
	t.Cleanup(func() { hostCall = prev })
	return fake
}

// installFastRetry removes the real backoff sleeps so retry tests stay fast,
// while still counting attempts.
func installFastRetry(t *testing.T) *int32 {
	t.Helper()
	prev := retrySleep
	var calls int32
	retrySleep = func(ctx context.Context, wait time.Duration) error {
		atomic.AddInt32(&calls, 1)
		return nil
	}
	t.Cleanup(func() { retrySleep = prev })
	return &calls
}

// rateLimitGateway serves Qoder's nested SSE shape. The first `failures`
// responses are envelope-level 429 refusals; afterwards it streams one normal
// chunk and a [DONE] terminal frame.
func rateLimitGateway(t *testing.T, failures int32, hits *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if n <= failures {
			// HTTP 200 with the real status buried in the envelope — the shape
			// this plugin used to treat as a normal chunk.
			_, _ = w.Write([]byte("data:" + `{"headers":{},"body":"{\"code\":\"429\",\"message\":\"Too many requests, please rate limit\"}","statusCodeValue":429}` + "\n\n"))
			return
		}
		_, _ = w.Write([]byte("data:" + `{"headers":{},"body":"{\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}","statusCodeValue":200}` + "\n\n"))
		_, _ = w.Write([]byte("data:" + `{"headers":{},"body":"[DONE]","statusCodeValue":200}` + "\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testStoredAuth() *storedAuth {
	return &storedAuth{
		Auth:    storedTokens{AccessToken: "test-access-token"},
		Account: storedAccount{UID: "uid-test"},
	}
}

// -----------------------------------------------------------------------------
// 1. Envelope statusCodeValue:429 → rate-limit branch, no chunk (async path)
// -----------------------------------------------------------------------------

func TestPumpUpstreamStream_EnvelopeRateLimitIsNotAChunk(t *testing.T) {
	resetCooldowns(t)
	fake := installFakeHost(t)
	installFastRetry(t)
	var hits int32
	srv := rateLimitGateway(t, upstreamRetryMaxAttempts, &hits)
	restore := setGatewayBaseForTest(regionCN, srv.URL)
	defer restore()

	sa := testStoredAuth()
	started := time.Now()
	prepare := func(ctx context.Context) (*http.Request, context.CancelFunc, error) {
		return buildChatRequest(ctx, sa, "body", "qmodel")
	}
	pumpUpstreamStream(prepare, func() {}, "stream-1", false, "qoder/qmodel", "qmodel", "uid-test", started, "auth-1", "qmodel")

	if got := fake.chunks(); len(got) != 0 {
		t.Fatalf("rate-limited envelope must not be emitted as a chunk, got %v", got)
	}
	errs := fake.streamErrors()
	if len(errs) != 1 {
		t.Fatalf("want exactly one terminal error frame, got %v", errs)
	}
	if !strings.Contains(errs[0], "429") {
		t.Fatalf("terminal error should name the upstream status, got %q", errs[0])
	}
	if hits != int32(upstreamRetryMaxAttempts) {
		t.Fatalf("the pump should exhaust its retry budget on a sustained rate limit; hits=%d want %d", hits, upstreamRetryMaxAttempts)
	}
	// The plugin must cool only the (account, model) pair, keeping the plugin's
	// own rate-limit reason.
	if !modelIsCooling("auth-1", "qmodel") {
		t.Fatal("a sustained rate limit should cool the specific model pair")
	}
	if modelIsCooling("auth-1", "other-model") {
		t.Fatal("a rate limit on one model must not cool sibling models")
	}
}

// A rate limit that clears within the retry budget must succeed silently.
func TestPumpUpstreamStream_RateLimitRetryRecovers(t *testing.T) {
	resetCooldowns(t)
	fake := installFakeHost(t)
	installFastRetry(t)
	var hits int32
	srv := rateLimitGateway(t, 1, &hits)
	restore := setGatewayBaseForTest(regionCN, srv.URL)
	defer restore()

	sa := testStoredAuth()
	prepare := func(ctx context.Context) (*http.Request, context.CancelFunc, error) {
		return buildChatRequest(ctx, sa, "body", "qmodel")
	}
	pumpUpstreamStream(prepare, func() {}, "stream-1", true, "qoder/qmodel", "qmodel", "uid-test", time.Now(), "auth-1", "qmodel")

	chunks := fake.chunks()
	if len(chunks) != 1 {
		t.Fatalf("want the retried stream to deliver its chunk, got %v", chunks)
	}
	if !strings.HasPrefix(chunks[0], "data: ") {
		t.Fatalf("sseFramed chunk should keep its data: prefix, got %q", chunks[0])
	}
	if errs := fake.streamErrors(); len(errs) != 0 {
		t.Fatalf("a recovered rate limit must not report an error, got %v", errs)
	}
	if modelIsCooling("auth-1", "qmodel") {
		t.Fatal("a recovered rate limit must not cool the pair")
	}
}

// The terminal message must stay distinguishable from the host's own
// synthesized empty_stream, which is what triggers credential-wide cooling.
func TestStreamEnvelopeErrorKeepsLifecycleWording(t *testing.T) {
	msg := streamEnvelopeError(http.StatusTooManyRequests, `{"code":"429","message":"rate limit"}`)
	if !strings.Contains(strings.ToLower(msg), "unexpected eof") {
		t.Fatalf("rate-limit message must carry the lifecycle marker, got %q", msg)
	}
	// And the raw upstream body must not leak credentials through.
	if strings.Contains(msg, "Bearer ") {
		t.Fatalf("terminal message must be redacted, got %q", msg)
	}
}

// -----------------------------------------------------------------------------
// 2. Same envelope handling on the synchronous collection path
// -----------------------------------------------------------------------------

func TestCollectUpstreamStreamQoder_EnvelopeRateLimitReturnsStatus(t *testing.T) {
	var hits int32
	srv := rateLimitGateway(t, upstreamRetryMaxAttempts, &hits)
	restore := setGatewayBaseForTest(regionCN, srv.URL)
	defer restore()
	installFastRetry(t)

	sa := testStoredAuth()
	prepare := func(ctx context.Context) (*http.Request, context.CancelFunc, error) {
		return buildChatRequest(ctx, sa, "body", "qmodel")
	}
	chunks, status, err := collectUpstreamStreamQoder(prepare, false, &sseUsageCollector{})
	if err == nil {
		t.Fatal("a sustained rate limit must return an error")
	}
	if len(chunks) != 0 {
		t.Fatalf("no chunks should be produced, got %v", chunks)
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 so the host scopes the cooldown", status)
	}
	// The status must survive the RPC boundary as http_status.
	raw := errorEnvelopeFor(err)
	var env envelope
	if json.Unmarshal(raw, &env) != nil || env.Error == nil {
		t.Fatalf("expected an error envelope, got %s", raw)
	}
	if env.Error.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("envelope http_status = %d, want 429", env.Error.HTTPStatus)
	}
}

func TestCollectUpstreamStreamQoder_NormalStreamStillPassesThrough(t *testing.T) {
	var hits int32
	srv := rateLimitGateway(t, 0, &hits)
	restore := setGatewayBaseForTest(regionCN, srv.URL)
	defer restore()

	sa := testStoredAuth()
	prepare := func(ctx context.Context) (*http.Request, context.CancelFunc, error) {
		return buildChatRequest(ctx, sa, "body", "qmodel")
	}
	collector := &sseUsageCollector{}
	chunks, status, err := collectUpstreamStreamQoder(prepare, false, collector)
	if err != nil {
		t.Fatalf("normal stream must succeed: %v", err)
	}
	if status != 0 {
		t.Fatalf("successful collection should report status 0, got %d", status)
	}
	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk, got %d (%v)", len(chunks), chunks)
	}
	if !strings.Contains(string(chunks[0].Payload), `"content":"hi"`) {
		t.Fatalf("chunk payload should carry the delta, got %s", chunks[0].Payload)
	}
}

// -----------------------------------------------------------------------------
// 3. Same envelope handling on the /v1/responses folding path
// -----------------------------------------------------------------------------

func TestAggregateQoderSSE_EnvelopeRateLimitIsAnError(t *testing.T) {
	body := "data:" + `{"body":"{\"code\":\"429\",\"message\":\"too many requests\"}","statusCodeValue":429}` + "\n\n"
	_, err := aggregateQoderSSE(strings.NewReader(body), "qmodel")
	if err == nil {
		t.Fatal("an envelope-level 429 must fail the fold instead of producing an empty completion")
	}
	if upstreamStatusFromError(err) != http.StatusTooManyRequests {
		t.Fatalf("fold error should carry 429, got %d (%v)", upstreamStatusFromError(err), err)
	}
}

// A body-level error code with no envelope status must still be caught.
func TestAggregateQoderSSE_BodyOnlyErrorCodeIsAnError(t *testing.T) {
	body := "data:" + `{"body":"{\"error\":{\"code\":429,\"message\":\"rate limit exceeded\"}}"}` + "\n\n"
	_, err := aggregateQoderSSE(strings.NewReader(body), "qmodel")
	if err == nil {
		t.Fatal("a body-level 429 must not be folded into a normal completion")
	}
	if upstreamStatusFromError(err) != http.StatusTooManyRequests {
		t.Fatalf("fold error should carry 429, got %d (%v)", upstreamStatusFromError(err), err)
	}
}

func TestAggregateQoderSSE_NormalStreamStillFolds(t *testing.T) {
	body := "data:" + `{"body":"{\"id\":\"chatcmpl-9\",\"model\":\"qmodel\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"}}]}"}` + "\n\n" +
		"data:" + `{"body":"{\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}"}` + "\n\n" +
		"data:" + `{"body":"[DONE]"}` + "\n\n"
	out, err := aggregateQoderSSE(strings.NewReader(body), "qmodel")
	if err != nil {
		t.Fatalf("normal fold must succeed: %v", err)
	}
	var completion map[string]any
	if err := json.Unmarshal(out, &completion); err != nil {
		t.Fatalf("folded completion must be valid JSON: %v", err)
	}
	if usage, ok := completion["usage"].(map[string]any); !ok || usage["total_tokens"] != float64(10) {
		t.Fatalf("usage must survive the fold unchanged, got %v", completion["usage"])
	}
	choices, _ := completion["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("want one choice, got %v", choices)
	}
}

// -----------------------------------------------------------------------------
// 4. cleanChunkJSON drops error envelopes
// -----------------------------------------------------------------------------

func TestCleanChunkJSON_DropsErrorEnvelopes(t *testing.T) {
	cases := []string{
		`{"code":"429","message":"rate limit"}`,
		`{"code":429,"message":"too many requests"}`,
		`{"error":{"message":"boom","code":429}}`,
		`{"error":{"message":"rate limit exceeded","type":"rate_limit_error"}}`,
		`{"error":"upstream failed"}`,
		`{"message":"throttled, slow down"}`,
	}
	for _, tc := range cases {
		if got := cleanChunkJSON(tc); got != "" {
			t.Fatalf("cleanChunkJSON(%s) = %q, want \"\" (must not leak as a chunk)", tc, got)
		}
	}
}

func TestCleanChunkJSON_KeepsLegitimateChunks(t *testing.T) {
	cases := []string{
		`{"id":"1","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`{"id":"1","choices":[{"index":0,"delta":{"content":""}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		// A usage-only tail chunk has no choices but is not an error.
		`{"usage":{"prompt_tokens":5,"total_tokens":5}}`,
	}
	for _, tc := range cases {
		if got := cleanChunkJSON(tc); got == "" {
			t.Fatalf("cleanChunkJSON(%s) must be preserved, got \"\"", tc)
		}
	}
}

// -----------------------------------------------------------------------------
// 5. Concurrency gate
// -----------------------------------------------------------------------------

func TestUpstreamGate_SerializesPerAuth(t *testing.T) {
	gate := &upstreamGate{slots: map[string]chan struct{}{}}
	released := make([]func(), 0, upstreamMaxConcurrentPerAuth)

	// Fill the allowance for one credential.
	for i := 0; i < upstreamMaxConcurrentPerAuth; i++ {
		release, err := gate.acquire("auth-a", time.Second)
		if err != nil {
			t.Fatalf("slot %d should be available: %v", i, err)
		}
		released = append(released, release)
	}
	// The next request for the same credential must wait, not overshoot.
	if _, err := gate.acquire("auth-a", 50*time.Millisecond); err == nil {
		t.Fatal("the (N+1)th request should not get a slot while all are held")
	}
	// A different credential has an independent allowance.
	otherRelease, err := gate.acquire("auth-b", time.Second)
	if err != nil {
		t.Fatalf("a different auth must not be blocked: %v", err)
	}
	// Freeing one slot lets the next request in.
	released[0]()
	nextRelease, err := gate.acquire("auth-a", time.Second)
	if err != nil {
		t.Fatalf("a freed slot should be reusable: %v", err)
	}
	nextRelease()
	otherRelease()
	for _, r := range released[1:] {
		r()
	}
}

func TestUpstreamGate_ReleaseIsIdempotent(t *testing.T) {
	gate := &upstreamGate{slots: map[string]chan struct{}{}}
	release, err := gate.acquire("auth-a", time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	release()
	release() // must not hand back a second slot
	taken := make([]func(), 0, upstreamMaxConcurrentPerAuth)
	for i := 0; i < upstreamMaxConcurrentPerAuth; i++ {
		r, err := gate.acquire("auth-a", time.Second)
		if err != nil {
			t.Fatalf("slot %d should be free after a double release: %v", i, err)
		}
		taken = append(taken, r)
	}
	if _, err := gate.acquire("auth-a", 50*time.Millisecond); err == nil {
		t.Fatal("gate drifted past its cap after a double release")
	}
	for _, r := range taken {
		r()
	}
}

// A saturated gate must fail as a retryable local condition that carries no
// HTTP status: it is not an upstream credential fault.
func TestAcquireUpstreamSlot_TimeoutCarriesNoHTTPStatus(t *testing.T) {
	gate := &upstreamGate{slots: map[string]chan struct{}{}}
	held := make([]func(), 0, upstreamMaxConcurrentPerAuth)
	for i := 0; i < upstreamMaxConcurrentPerAuth; i++ {
		r, err := gate.acquire("auth-z", time.Second)
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		held = append(held, r)
	}
	_, err := gate.acquire("auth-z", 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected the gate to time out while saturated")
	}
	if upstreamStatusFromError(err) != 0 {
		t.Fatalf("a local gate timeout must not carry an HTTP status, got %d", upstreamStatusFromError(err))
	}
	for _, r := range held {
		r()
	}
}

// -----------------------------------------------------------------------------
// 6. Regression: usage accounting is unchanged on the happy path
// -----------------------------------------------------------------------------

func TestEnvelopeHelpers_IgnoreHealthyFrames(t *testing.T) {
	env, ok := parseSSEEnvelope(`{"headers":{},"body":"{\"choices\":[]}","statusCodeValue":200}`)
	if !ok {
		t.Fatal("a normal envelope must parse")
	}
	if got := upstreamEnvelopeStatus(env); got != 0 {
		t.Fatalf("status = %d, want 0 for a healthy envelope", got)
	}
	if _, ok := parseSSEEnvelope("not-json"); ok {
		t.Fatal("a non-JSON frame must be reported as not-an-envelope")
	}
	if _, ok := parseSSEEnvelope(`[1,2,3]`); ok {
		t.Fatal("a JSON array must not be treated as an envelope")
	}
}

func TestUpstreamEnvelopeStatus_CodeAsStringIsRecognized(t *testing.T) {
	env, _ := parseSSEEnvelope(`{"body":"{\"code\":\"429\",\"message\":\"rate limit\"}","statusCodeValue":"429"}`)
	if got := upstreamEnvelopeStatus(env); got != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 even when rendered as a string", got)
	}
}

// A refusal without an envelope status must still be caught via the body.
func TestUpstreamEnvelopeStatus_BodyCodeWithoutEnvelopeStatus(t *testing.T) {
	env, _ := parseSSEEnvelope(`{"body":"{\"code\":429,\"message\":\"rate limit\"}","statusCodeValue":200}`)
	if got := upstreamEnvelopeStatus(env); got != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 from the body code", got)
	}
}

func TestBuildChatRequestCannotBeBuiltWithoutToken(t *testing.T) {
	_, _, err := buildChatRequest(context.Background(), &storedAuth{}, "body", "qmodel")
	if err == nil {
		t.Fatal("building a signed request without an access token must fail")
	}
	if !errors.Is(err, errEmptyToken) {
		t.Fatalf("error should wrap the empty-token cause, got %v", err)
	}
}
