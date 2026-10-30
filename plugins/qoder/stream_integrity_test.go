package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func integrityWire(parts []string, nested bool) string {
	var b strings.Builder
	for _, p := range parts {
		if nested {
			raw, _ := json.Marshal(map[string]any{"body": p, "statusCodeValue": 200})
			p = string(raw)
		}
		b.WriteString("data:")
		b.WriteString(p)
		b.WriteString("\n\n")
	}
	return b.String()
}
func integrityRun(t *testing.T, path string, parts []string) ([]string, error) {
	t.Helper()
	wire := integrityWire(parts, path != "fold-plain")
	if path == "fold-plain" || path == "fold-nested" {
		var raw []byte
		var err error
		if path == "fold-plain" {
			raw, err = aggregateCompletion(strings.NewReader(wire), "dfmodel")
		} else {
			raw, err = aggregateQoderSSE(strings.NewReader(wire), "dfmodel")
		}
		if err != nil {
			return nil, err
		}
		return []string{string(raw)}, nil
	}
	resetCooldowns(t)
	fake := installFakeHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, wire)
	}))
	defer server.Close()
	prepare := func(ctx context.Context) (*http.Request, context.CancelFunc, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", server.URL, nil)
		return req, func() {}, err
	}
	if path == "collect" {
		chunks, _, err := collectUpstreamAttempt(prepare, false, &sseUsageCollector{})
		out := []string{}
		for _, c := range chunks {
			out = append(out, string(c.Payload))
		}
		return out, err
	}
	req, cancel, err := prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if retry := pumpUpstreamAttempt(req, cancel, "integrity-test", false, "dfmodel", "dfmodel", "", time.Now(), "integrity-auth", "dfmodel", upstreamRetryMaxAttempts); retry {
		t.Fatal("unexpected retry")
	}
	if es := fake.streamErrors(); len(es) > 0 {
		return fake.chunks(), &integrityTestError{es[0]}
	}
	return fake.chunks(), nil
}

type integrityTestError struct{ s string }

func (e *integrityTestError) Error() string { return e.s }

const integrityText = `{"id":"r","model":"auto","choices":[{"index":0,"delta":{"content":"hello","reasoning_content":"think"}}]}`
const integrityStop = `{"id":"r","model":"auto","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
const integrityToolStart = `{"id":"r","choices":[{"index":0,"delta":{"content":"hello","reasoning_content":"think","tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"echo","arguments":"{\"x\":"}}]}}]}`
const integrityToolEnd = `{"id":"r","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"","function":{"arguments":"1}"}}]},"finish_reason":"tool_calls"}]}`

func TestIntegrityAllExecutorPaths(t *testing.T) {
	cases := []struct {
		name   string
		parts  []string
		err    string
		finish string
		tool   bool
	}{
		{"empty", nil, "empty_stream", "", false},
		{"eof", []string{integrityText}, "missing_finish", "", false},
		{"done_without_finish", []string{integrityText, "[DONE]"}, "missing_finish", "", false},
		{"valid_eof", []string{integrityText, integrityStop}, "", "stop", false},
		{"valid_done", []string{integrityText, integrityStop, `{"choices":[],"usage":{"total_tokens":8}}`, "[DONE]"}, "", "stop", false},
		{"nonnumeric_error", []string{integrityText, `{"error":{"code":"bad_request","message":"secret-payload"}}`}, "upstream_error_object", "", false},
		{"finish_then_error", []string{integrityText, integrityStop, `{"error":"secret-payload"}`}, "upstream_error_object", "", false},
		{"bad_json", []string{integrityText, `{"secret-payload":`}, "invalid_json", "", false},
		{"empty_tool", []string{`{"choices":[{"index":0,"delta":{"tool_calls":[{}]},"finish_reason":"tool_calls"}]}`, "[DONE]"}, "invalid_tool_name", "", false},
		{"nameless_tool", []string{`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"","arguments":"{}"}}]},"finish_reason":"stop"}]}`}, "invalid_tool_name", "", false},
		{"finish_only_tool", []string{`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`}, "finish_without_tool", "", false},
		{"invalid_args", []string{integrityToolStart, `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`}, "invalid_tool_arguments", "", false},
		{"split_tool", []string{integrityToolStart, integrityToolEnd, "[DONE]"}, "", "tool_calls", true},
		{"length_partial", []string{integrityToolStart, `{"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`, "[DONE]"}, "", "length", false},
		{"filter_partial", []string{integrityToolStart, `{"choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}`}, "", "content_filter", false},
		{"empty_padding", []string{`{"choices":[{"index":0,"delta":{"content":"hello","function_call":{"name":"","arguments":""},"tool_calls":[]}}]}`, integrityStop}, "", "stop", false},
	}
	for _, tc := range cases {
		for _, path := range []string{"fold-plain", "fold-nested", "collect", "async"} {
			t.Run(tc.name+"/"+path, func(t *testing.T) {
				out, err := integrityRun(t, path, tc.parts)
				joined := strings.Join(out, "\n")
				if tc.err != "" {
					if err == nil || !strings.Contains(err.Error(), tc.err) || !strings.Contains(err.Error(), "unexpected EOF") {
						t.Fatalf("want safe %s, got %v", tc.err, err)
					}
					if strings.Contains(err.Error(), "secret-payload") {
						t.Fatal("leaked payload")
					}
					if strings.Contains(joined, `"finish_reason"`) || strings.Contains(joined, `"tool_calls"`) {
						t.Fatalf("terminal/tool escaped before failure: %s", joined)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(joined, `"finish_reason":"`+tc.finish+`"`) {
					t.Fatalf("lost finish %s: %s", tc.finish, joined)
				}
				if tc.tool {
					if !strings.Contains(joined, `"name":"echo"`) || !strings.Contains(joined, `"arguments":"{\"x\":1}"`) {
						t.Fatalf("lost valid tool %s", joined)
					}
				} else if strings.Contains(joined, `"function":`) {
					t.Fatalf("incomplete tool released %s", joined)
				}
			})
		}
	}
}

func TestIntegrityStreamingBeforeTerminalAndMixedFrame(t *testing.T) {
	state := newStreamIntegrity()
	frame, err := state.feed(integrityToolStart)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(frame, `"content":"hello"`) || !strings.Contains(frame, `"reasoning_content":"think"`) || strings.Contains(frame, "tool_calls") {
		t.Fatalf("mixed frame handling %s", frame)
	}
	frame, err = state.feed(integrityToolEnd)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(frame, "finish_reason") || strings.Contains(frame, "tool_calls") {
		t.Fatal("terminal emitted early")
	}
	done, err := state.end()
	if err != nil || len(done) != 1 {
		t.Fatalf("end %v %v", done, err)
	}
}
func TestIntegrityMultipleChoicesTransactional(t *testing.T) {
	parts := []string{`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"},{"index":1,"delta":{"content":"second","tool_calls":[{}]},"finish_reason":"tool_calls"}]}`}
	out, err := integrityRun(t, "async", parts)
	if err == nil {
		t.Fatal("invalid second choice accepted")
	}
	if strings.Contains(strings.Join(out, ""), "finish_reason") {
		t.Fatal("first choice finished before second validated")
	}
	valid := []string{`{"choices":[{"index":0,"delta":{"content":"first"},"finish_reason":"stop"},{"index":1,"delta":{"content":"second"},"finish_reason":"stop"}]}`}
	raw, err := aggregateCompletion(strings.NewReader(integrityWire(valid, false)), "m")
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	json.Unmarshal(raw, &obj)
	if len(obj["choices"].([]any)) != 2 {
		t.Fatal("merged choices")
	}
}
func TestIntegrityCleanMultiChoiceAndErrors(t *testing.T) {
	raw, err := cleanChunkJSON(`{"choices":[{"index":0,"delta":{"function_call":null}},{"index":1,"delta":{"content":"keep"}}],"usage":{"total_tokens":1}}`)
	if err != nil || !strings.Contains(raw, "keep") || !strings.Contains(raw, "usage") {
		t.Fatalf("lost sibling/usage: %s %v", raw, err)
	}
	for _, bad := range []string{`{"error":"secret"}`, `{`, `null`} {
		if _, err := cleanChunkJSON(bad); err == nil {
			t.Fatal("clean accepted invalid input")
		}
	}
}
func TestIntegrityToolLimitsAndGeneratedID(t *testing.T) {
	s := newStreamIntegrity()
	_, err := s.feed(`{"choices":[{"delta":{"tool_calls":[{"function":{"name":"echo","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.end()
	if err != nil || !strings.Contains(strings.Join(out, ""), "call_qoder_") {
		t.Fatalf("missing generated id %v %v", out, err)
	}
	s = newStreamIntegrity()
	raw := map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"function": map[string]any{"name": "echo", "arguments": strings.Repeat("x", integrityMaxToolBytes+1)}}}}}}}
	b, _ := json.Marshal(raw)
	if _, err := s.feed(string(b)); err == nil || !strings.Contains(err.Error(), "tool_buffer_limit") {
		t.Fatalf("no buffer bound: %v", err)
	}
}
func TestIntegrityRawChatAndTrailingGatewayStats(t *testing.T) {
	body := ": heartbeat\nevent:message\n" + integrityWire([]string{integrityText, integrityStop, "[DONE]"}, false) + "event:finish\n{gateway-stats-not-json}\n"
	if _, err := aggregateQoderSSE(strings.NewReader(body), "dfmodel"); err != nil {
		t.Fatal(err)
	}
}

// Real replay is opt-in: don't commit captured reasoning or user content to git.
func TestIntegrityCapturedStream(t *testing.T) {
	path := os.Getenv("QODER_CAPTURE_FILE")
	if path == "" {
		t.Skip("set QODER_CAPTURE_FILE to a diagnostic line capture")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var wire strings.Builder
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 64*1024), 4<<20)
	for scan.Scan() {
		var row struct {
			Line string `json:"line"`
		}
		if err := json.Unmarshal(scan.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		wire.WriteString(row.Line)
		wire.WriteByte('\n')
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	for _, nested := range []bool{true} {
		var frames []string
		err := walkIntegrityStream(strings.NewReader(wire.String()), nested, nil, func(s string) error { frames = append(frames, s); return nil })
		if err != nil {
			t.Fatal(err)
		}
		raw, err := aggregateQoderSSE(strings.NewReader(wire.String()), "dfmodel")
		if err != nil {
			t.Fatal(err)
		}
		var obj map[string]any
		json.Unmarshal(raw, &obj)
		choice := obj["choices"].([]any)[0].(map[string]any)
		msg := choice["message"].(map[string]any)
		tools, _ := msg["tool_calls"].([]any)
		if choice["finish_reason"] == "tool_calls" {
			if len(tools) == 0 {
				t.Fatal("missing replay tool")
			}
			for _, tool := range tools {
				fn := tool.(map[string]any)["function"].(map[string]any)
				name, _ := fn["name"].(string)
				args, _ := fn["arguments"].(string)
				if !validIntegrityName(name) || !json.Valid([]byte(args)) {
					t.Fatal("invalid replay tool")
				}
			}
		} else if choice["finish_reason"] != "stop" || !strings.Contains(msg["content"].(string), "END_OF_200") {
			t.Fatalf("invalid long replay finish=%v", choice["finish_reason"])
		}
		t.Logf("captured replay accepted: output_frames=%d tools=%d model=%v finish=%v", len(frames), len(tools), obj["model"], choice["finish_reason"])
	}
}

func TestIntegrityAsyncFinalRefusalClassification(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		rate       bool
	}{
		{"429", `{"code":"429","message":"Too many requests"}`, true},
		{"401", `{"code":"401","message":"Invalid credential"}`, false},
		{"403", `{"code":"403","message":"Invalid signature"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := integrityRun(t, "async", []string{tc.body})
			if err == nil || len(out) != 0 {
				t.Fatalf("expected refusal only, out=%v err=%v", out, err)
			}
			if !strings.Contains(err.Error(), "upstream "+tc.name) {
				t.Fatalf("lost HTTP status: %v", err)
			}
			if got := strings.Contains(err.Error(), "(rate limit, unexpected EOF)"); got != tc.rate {
				t.Fatalf("rate classification=%t want=%t: %v", got, tc.rate, err)
			}
			if !tc.rate && strings.Contains(err.Error(), "unexpected EOF") {
				t.Fatalf("auth refusal became lifecycle error: %v", err)
			}
		})
	}
}
func TestIntegrityAsyncTypedRefusalRenderingUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"queue", &statusError{status: 429, err: &queueAdmissionError{retryAfterSeconds: 30}}},
		{"daily", &statusError{status: 429, err: &dailyQuotaError{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := queueErrorForClient(tc.err).Error()
			if got := asyncIntegrityErrorMessage(tc.err); got != want {
				t.Fatalf("typed rejection changed: got=%q want=%q", got, want)
			}
		})
	}
}

type integrityFailReader struct{ r *strings.Reader }

func (r *integrityFailReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err == io.EOF {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}
func TestIntegrityReadFailureAfterFinishDoesNotRelease(t *testing.T) {
	r := &integrityFailReader{strings.NewReader(integrityWire([]string{integrityToolStart, integrityToolEnd}, false))}
	var frames []string
	err := walkIntegrityStream(r, false, nil, func(s string) error { frames = append(frames, s); return nil })
	if err == nil || !strings.Contains(err.Error(), "read_failure") {
		t.Fatalf("expected read failure: %v", err)
	}
	if strings.Contains(strings.Join(frames, ""), "tool_calls") || strings.Contains(strings.Join(frames, ""), "finish_reason") {
		t.Fatal("premature terminal on read error")
	}
}
func TestIntegrityInterleavedChoicesAndNameFragments(t *testing.T) {
	parts := []string{
		`{"choices":[{"index":1,"delta":{"tool_calls":[{"index":0,"id":"b","function":{"name":"ec","arguments":"{"}}]}},{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"echo","arguments":"{}"}}]}}]}`,
		`{"choices":[{"index":1,"delta":{"tool_calls":[{"index":0,"function":{"name":"ho","arguments":"}"}}]},"finish_reason":"tool_calls"},{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	raw, err := aggregateCompletion(strings.NewReader(integrityWire(parts, false)), "m")
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	json.Unmarshal(raw, &obj)
	for _, v := range obj["choices"].([]any) {
		c := v.(map[string]any)
		tools := c["message"].(map[string]any)["tool_calls"].([]any)
		tool := tools[0].(map[string]any)
		if tool["function"].(map[string]any)["name"] != "echo" {
			t.Fatal("name fragments lost")
		}
	}
}
