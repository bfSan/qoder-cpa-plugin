package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func decodeIntegrityLogMessage(t *testing.T, message string) map[string]any {
	t.Helper()
	const prefix = "qoder stream outcome "
	if !strings.HasPrefix(message, prefix) {
		t.Fatalf("missing stable prefix: %q", message)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(message, prefix)), &summary); err != nil {
		t.Fatalf("summary absent from formatter-visible message: %v", err)
	}
	return summary
}
func TestIntegrityOutcomeSummarySurvivesFieldDroppingFormatter(t *testing.T) {
	s := newStreamIntegrity()
	raw := `{"id":"response_diag-123","model":"qoder/auto","choices":[{"index":0,"delta":{"content":"PRIVATE_OUTPUT","reasoning_content":"PRIVATE_THOUGHT","tool_calls":[{"index":0,"id":"call_private","function":{"name":"private_tool_name","arguments":"{\"text\":\"PRIVATE_ARGS\"}"}}]},"finish_reason":"tool_calls"}]}`
	if _, err := s.feed(raw); err != nil {
		t.Fatal(err)
	}
	s.done = true
	if _, err := s.end(); err != nil {
		t.Fatal(err)
	}
	level, message, _ := s.outcomeLogRecord(nil)
	if level != "info" {
		t.Fatalf("success level=%s", level)
	}
	// Deliberately discard fields exactly as the deployed formatter does.
	summary := decodeIntegrityLogMessage(t, message)
	for _, key := range []string{"frames", "choices", "tools", "tool_bytes", "text_bytes", "reasoning_bytes"} {
		value, ok := summary[key].(float64)
		if !ok || value <= 0 {
			t.Fatalf("missing/nonpositive %s: %v", key, summary[key])
		}
	}
	if summary["saw_done"] != true || summary["response_id"] != "response_diag-123" || summary["reported_model"] != "qoder/auto" || summary["outcome"] != "success" {
		t.Fatalf("lost safe metadata: %v", summary)
	}
	finishes, ok := summary["finish_reasons"].([]any)
	if !ok || len(finishes) != 1 || finishes[0] != "tool_calls" {
		t.Fatalf("lost finish: %v", summary["finish_reasons"])
	}
	for _, secret := range []string{"PRIVATE_OUTPUT", "PRIVATE_THOUGHT", "PRIVATE_ARGS", "private_tool_name", "call_private", "\"arguments\"", "\"content\"", "\"prompt\""} {
		if strings.Contains(message, secret) {
			t.Fatalf("message leaked private field %q", secret)
		}
	}
}
func TestIntegrityOutcomeFailureOmitsUntrustedTextAndLabels(t *testing.T) {
	s := newStreamIntegrity()
	raw := `{"id":"response\nPRIVATE_ID","model":"model PRIVATE_MODEL","choices":[{"index":0,"delta":{"content":"PRIVATE_CONTENT"}}]}`
	if _, err := s.feed(raw); err != nil {
		t.Fatal(err)
	}
	err := s.fail("missing_finish")
	level, message, _ := s.outcomeLogRecord(err)
	summary := decodeIntegrityLogMessage(t, message)
	if level != "warn" || summary["reason"] != "missing_finish" || summary["outcome"] != "failure" || summary["saw_done"] != false {
		t.Fatalf("bad failure summary %s", message)
	}
	if summary["reported_model"] != "" || summary["response_id"] != "" {
		t.Fatal("invalid label accepted")
	}
	if strings.Contains(message, "PRIVATE_") {
		t.Fatal("untrusted text leaked")
	}
	fresh := newStreamIntegrity()
	_, message, _ = fresh.outcomeLogRecord(errors.New("Bearer PRIVATE_TOKEN; PRIVATE_PROMPT; PRIVATE_ARGUMENTS"))
	if strings.Contains(message, "PRIVATE_") || strings.Contains(message, "Bearer") {
		t.Fatal("error text leaked")
	}
	if decodeIntegrityLogMessage(t, message)["reason"] != "host_emit_failed" {
		t.Fatal("missing fixed fallback reason")
	}
}
