package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Tool fragments never reach the host until the whole stream has been checked.
// Text/reasoning/usage remain streaming. The bounds apply across all choices.
const integrityMaxToolBytes = 1 << 20
const integrityMaxTools = 128
const integrityMaxChoices = 32

type integrityChoice struct {
	finish string
	tools  map[int]map[string]any
	meta   map[string]any
}
type streamIntegrity struct {
	choices                                                       map[int]*integrityChoice
	frames, textBytes, reasoningBytes, toolBytes, tools, finishes int
	done                                                          bool
	reportedModel, responseID, reason                             string
	nonce                                                         int64
}

func newStreamIntegrity() *streamIntegrity {
	return &streamIntegrity{choices: make(map[int]*integrityChoice), nonce: time.Now().UnixNano()}
}
func (s *streamIntegrity) fail(code string) error {
	s.reason = code
	return fmt.Errorf("qoder stream integrity: %s frames=%d choices=%d finishes=%d done=%t text_bytes=%d reasoning_bytes=%d tools=%d tool_bytes=%d (unexpected EOF)", code, s.frames, len(s.choices), s.finishes, s.done, s.textBytes, s.reasoningBytes, s.tools, s.toolBytes)
}

// decodeIntegrityLine accepts the gateway envelope and standard data chunks.
// SSE comments/event/id lines are harmless; malformed data is never a keepalive.
func (s *streamIntegrity) decodeLine(line string, nested bool) (string, *sseEnvelope, error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") || strings.HasPrefix(line, "id:") || strings.HasPrefix(line, "retry:") {
		return "", nil, nil
	}
	if strings.HasPrefix(line, "data:") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	}
	if line == "[DONE]" {
		s.done = true
		return "[DONE]", nil, nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(line), &obj) != nil || obj == nil {
		return "", nil, s.fail("invalid_json")
	}
	env, ok := parseSSEEnvelope(line)
	if !ok {
		return "", nil, s.fail("invalid_envelope")
	}
	if upstreamEnvelopeStatus(env) >= 400 {
		return "", env, nil
	}
	if nested {
		if raw, has := obj["body"]; has {
			if env.Body == nil || string(raw) == "null" {
				return "", nil, s.fail("invalid_envelope_body")
			}
			if strings.TrimSpace(*env.Body) == "[DONE]" {
				s.done = true
			}
			return strings.TrimSpace(*env.Body), nil, nil
		}
	}
	return line, nil, nil
}

// walkIntegrityStream is the only stream acceptance boundary. Terminal frames
// remain private until EOF/DONE and all choices/tools have passed end().
func walkIntegrityStream(r io.Reader, nested bool, observe func(string), emit func(string) error) (resultErr error) {
	s := newStreamIntegrity()
	defer func() { s.logOutcome(resultErr) }()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		body, env, err := s.decodeLine(scanner.Text(), nested)
		if err != nil {
			return err
		}
		if env != nil {
			status := upstreamEnvelopeStatus(env)
			s.reason = "upstream_refusal"
			return &statusError{status: status, err: attachQuotaHeaders(upstreamEnvelopeError(status, envelopeBodyText(env)), env.Headers)}
		}
		if body == "[DONE]" {
			break
		}
		if body == "" {
			continue
		}
		if observe != nil {
			observe(body)
		}
		cleaned, err := s.feed(body)
		if err != nil {
			return err
		}
		if cleaned != "" {
			if err := emit(cleaned); err != nil {
				return err
			}
		}
	}
	if scanner.Err() != nil {
		return s.fail("read_failure")
	}
	terminal, err := s.end()
	if err != nil {
		return err
	}
	for _, raw := range terminal {
		if err := emit(raw); err != nil {
			return err
		}
	}
	return nil
}

func integrityIndex(v any) (int, bool) {
	if v == nil {
		return 0, true
	}
	f, ok := v.(float64)
	if !ok || f < 0 || f > 65535 || f != float64(int(f)) {
		return 0, false
	}
	return int(f), true
}
func integrityErrorObject(obj map[string]any) bool {
	if v, ok := obj["error"]; ok && v != nil {
		return true
	}
	if v, ok := obj["code"]; ok && v != nil {
		return true
	}
	// A top-level error message is not an OpenAI chunk or usage tail.
	if _, ok := obj["message"]; ok {
		if _, choices := obj["choices"]; !choices {
			return true
		}
	}
	return false
}
func integrityMeta(obj map[string]any) map[string]any {
	m := make(map[string]any)
	for _, k := range []string{"id", "object", "model", "created", "system_fingerprint"} {
		if v, ok := obj[k]; ok {
			m[k] = v
		}
	}
	return m
}
func (s *streamIntegrity) feed(body string) (string, error) {
	s.frames++
	var obj map[string]any
	if json.Unmarshal([]byte(body), &obj) != nil || obj == nil {
		return "", s.fail("invalid_json")
	}
	if integrityErrorObject(obj) {
		return "", s.fail("upstream_error_object")
	}
	if model, ok := obj["model"].(string); ok && safeIntegrityLabel(model) {
		s.reportedModel = model
	}
	if id, ok := obj["id"].(string); ok && safeIntegrityLabel(id) {
		s.responseID = id
	}
	rawChoices, has := obj["choices"]
	if !has {
		if _, ok := obj["usage"]; !ok {
			return "", s.fail("invalid_chunk")
		}
		return body, nil
	}
	choices, ok := rawChoices.([]any)
	if !ok {
		return "", s.fail("invalid_choices")
	}
	visible := make([]any, 0, len(choices))
	seen := make(map[int]bool)
	for _, raw := range choices {
		c, ok := raw.(map[string]any)
		if !ok {
			return "", s.fail("invalid_choice")
		}
		idx, ok := integrityIndex(c["index"])
		if !ok || seen[idx] {
			return "", s.fail("invalid_choice_index")
		}
		seen[idx] = true
		st := s.choices[idx]
		if st == nil {
			if len(s.choices) >= integrityMaxChoices {
				return "", s.fail("choice_limit")
			}
			st = &integrityChoice{tools: make(map[int]map[string]any)}
			s.choices[idx] = st
		}
		if st.finish != "" {
			return "", s.fail("choice_after_finish")
		}
		if st.meta == nil {
			st.meta = make(map[string]any)
		}
		for k, v := range integrityMeta(obj) {
			st.meta[k] = v
		}
		delta := map[string]any{}
		if v, exists := c["delta"]; exists && v != nil {
			var good bool
			delta, good = v.(map[string]any)
			if !good {
				return "", s.fail("invalid_delta")
			}
		}
		for _, k := range []string{"content", "reasoning_content"} {
			if v, exists := delta[k]; exists && v != nil {
				text, good := v.(string)
				if !good {
					return "", s.fail("invalid_text_delta")
				}
				if k == "content" {
					s.textBytes += len(text)
				} else {
					s.reasoningBytes += len(text)
				}
			}
		}
		// Legacy function_call is normalized into the same indexed tool state.
		if v, exists := delta["function_call"]; exists {
			if !isEmptyValue(v) {
				if _, exists := delta["tool_calls"]; exists {
					return "", s.fail("mixed_tool_protocol")
				}
				fn, good := v.(map[string]any)
				if !good {
					return "", s.fail("invalid_legacy_tool")
				}
				delta["tool_calls"] = []any{map[string]any{"index": float64(0), "type": "function", "function": fn}}
			}
			delete(delta, "function_call")
		}
		if v, exists := delta["tool_calls"]; exists {
			if v != nil {
				calls, good := v.([]any)
				if !good {
					return "", s.fail("invalid_tool_array")
				}
				for _, rawCall := range calls {
					call, good := rawCall.(map[string]any)
					if !good {
						return "", s.fail("invalid_tool")
					}
					ti, good := integrityIndex(call["index"])
					if !good {
						return "", s.fail("invalid_tool_index")
					}
					merged := st.tools[ti]
					if merged == nil {
						if s.tools >= integrityMaxTools {
							return "", s.fail("tool_limit")
						}
						merged = map[string]any{"index": ti}
						st.tools[ti] = merged
						s.tools++
					}
					for _, key := range []string{"id", "type"} {
						if val, has := call[key]; has && val != nil {
							text, good := val.(string)
							if !good {
								return "", s.fail("invalid_tool_field")
							}
							if text != "" {
								if old, has := merged[key]; has && old != text {
									return "", s.fail("tool_identity_changed")
								}
								s.toolBytes += len(text)
							}
						}
					}
					if f, has := call["function"]; has && f != nil {
						fn, good := f.(map[string]any)
						if !good {
							return "", s.fail("invalid_tool_function")
						}
						for _, k := range []string{"name", "arguments"} {
							if val, has := fn[k]; has && val != nil {
								text, good := val.(string)
								if !good {
									return "", s.fail("invalid_tool_field")
								}
								s.toolBytes += len(text)
							}
						}
					}
					if s.toolBytes > integrityMaxToolBytes {
						return "", s.fail("tool_buffer_limit")
					}
					mergeToolCallDelta(merged, call)
				}
			}
			delete(delta, "tool_calls")
		}
		if v, has := c["finish_reason"]; has && v != nil {
			f, good := v.(string)
			if !good {
				return "", s.fail("invalid_finish")
			}
			if f != "" {
				switch f {
				case "stop", "tool_calls", "function_call", "length", "content_filter":
				default:
					return "", s.fail("unknown_finish")
				}
				st.finish = f
				s.finishes++
			}
		}
		delete(c, "finish_reason")
		c["delta"] = delta
		if len(delta) > 0 {
			visible = append(visible, c)
		}
	}
	obj["choices"] = visible
	if len(visible) == 0 {
		if _, usage := obj["usage"]; !usage {
			return "", nil
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return "", s.fail("invalid_chunk")
	}
	return cleanChunkJSON(string(out))
}
func validIntegrityName(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.' || c == ':') {
			return false
		}
	}
	return true
}

// end is transactional: no terminal chunk is returned until EVERY choice and
// tool has passed validation. Thus a later choice/error cannot follow a success.
func (s *streamIntegrity) end() ([]string, error) {
	if len(s.choices) == 0 {
		return nil, s.fail("empty_stream")
	}
	indices := make([]int, 0, len(s.choices))
	for i := range s.choices {
		indices = append(indices, i)
	}
	sort.Ints(indices)
	out := make([]string, 0, len(indices))
	ids := make(map[string]bool)
	for _, idx := range indices {
		st := s.choices[idx]
		if st.finish == "" {
			return nil, s.fail("missing_finish")
		}
		delta := map[string]any{}
		if st.finish != "length" && st.finish != "content_filter" {
			if len(st.tools) == 0 && (st.finish == "tool_calls" || st.finish == "function_call") {
				return nil, s.fail("finish_without_tool")
			}
			order := make([]int, 0, len(st.tools))
			for i := range st.tools {
				order = append(order, i)
			}
			sort.Ints(order)
			calls := make([]any, 0, len(order))
			for _, ti := range order {
				call := st.tools[ti]
				fn, _ := call["function"].(map[string]any)
				name, _ := fn["name"].(string)
				args, _ := fn["arguments"].(string)
				if !validIntegrityName(name) {
					return nil, s.fail("invalid_tool_name")
				}
				var arguments map[string]json.RawMessage
				if json.Unmarshal([]byte(args), &arguments) != nil || arguments == nil {
					return nil, s.fail("invalid_tool_arguments")
				}
				if kind, ok := call["type"].(string); ok && kind != "" && kind != "function" {
					return nil, s.fail("invalid_tool_type")
				}
				id, _ := call["id"].(string)
				if id == "" {
					id = fmt.Sprintf("call_qoder_%x_%d_%d", s.nonce, idx, ti)
				}
				if len(id) > 256 || strings.TrimSpace(id) != id || strings.ContainsAny(id, "\r\n\t ") || ids[id] {
					return nil, s.fail("invalid_tool_id")
				}
				ids[id] = true
				calls = append(calls, map[string]any{"index": ti, "id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}})
			}
			if len(calls) > 0 {
				delta["tool_calls"] = calls
			}
		}
		obj := st.meta
		if obj == nil {
			obj = map[string]any{}
		}
		obj["choices"] = []any{map[string]any{"index": idx, "delta": delta, "finish_reason": st.finish}}
		raw, err := json.Marshal(obj)
		if err != nil {
			return nil, s.fail("invalid_terminal")
		}
		out = append(out, string(raw))
	}
	return out, nil
}

// logOutcome reports one content-free record per accepted or rejected stream.
func (s *streamIntegrity) logOutcome(err error) {
	level, message, fields := s.outcomeLogRecord(err)
	hostLog(level, message, fields)
}

// CPA v8's formatter drops unknown structured fields. Put the strictly
// content-free summary in message so the diagnostic survives that formatter.
// Keep construction pure enough to test without installing the C host bridge.
func (s *streamIntegrity) outcomeLogRecord(err error) (string, string, map[string]any) {
	finishes := make([]string, 0, len(s.choices))
	for _, c := range s.choices {
		if c.finish != "" {
			finishes = append(finishes, c.finish)
		}
	}
	sort.Strings(finishes)
	outcome, level := "success", "info"
	if err != nil {
		outcome, level = "failure", "warn"
		if s.reason == "" {
			s.reason = "host_emit_failed"
		}
	}
	fields := map[string]any{"outcome": outcome, "reason": s.reason, "frames": s.frames, "choices": len(s.choices), "tools": s.tools, "tool_bytes": s.toolBytes, "text_bytes": s.textBytes, "reasoning_bytes": s.reasoningBytes, "saw_done": s.done, "finish_reasons": finishes, "reported_model": s.reportedModel, "response_id": s.responseID}
	// All values are fixed strings, validated labels, integers, booleans or
	// slices of validated finish constants; there is no payload/error text.
	summary, marshalErr := json.Marshal(fields)
	if marshalErr != nil {
		return level, "qoder stream outcome summary_encoding_failed", map[string]any{"reason": "summary_encoding_failed"}
	}
	return level, "qoder stream outcome " + string(summary), fields
}

// The async host RPC carries only text, unlike collect/fold's typed status.
// Preserve the old streamEnvelopeError classification for ordinary 429s while
// leaving genuine auth errors and already-unwrapped queue/quota errors alone.
func asyncIntegrityErrorMessage(err error) string {
	public := queueErrorForClient(err)
	if public == nil {
		return ""
	}
	message := public.Error()
	if upstreamStatusFromError(public) == 429 && !strings.Contains(message, "(rate limit, unexpected EOF)") {
		message += " (rate limit, unexpected EOF)"
	}
	return message
}

func safeIntegrityLabel(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_.:/-", c)) {
			return false
		}
	}
	return true
}
