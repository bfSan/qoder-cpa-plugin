// sanitize_scope_test.go guards the v0.9.18 neutralPrompt scope fix: the
// wholesale system-prompt replacement must ONLY touch role=system messages.
// The pre-0.9.18 port fed every message through the replacement, so any user
// paste / tool result / assistant history entry over maxSystemPromptBytes (or
// matching agentPattern) was silently rewritten to neutralPrompt — observed
// in the field as "tool output looks truncated / stdout empty", "conversations
// reset every so often", and the model answering with the neutral prompt
// itself. OmniRoute codebuddy-cn.ts (porting source) gates the replacement on
// message.role !== "system" verbatim.
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func scopeTestBody(t *testing.T) map[string]any {
	t.Helper()
	long := strings.Repeat("x", maxSystemPromptBytes+200)
	raw := `{"model":"deepseek-v4-flash","stream":false,"messages":[
		{"role":"system","content":"` + long + `"},
		{"role":"user","content":"please review this log: ` + long + `"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"run_cmd","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"cmd output: ` + long + `"}
	]}`
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return obj
}

func msgContent(t *testing.T, obj map[string]any, i int) string {
	t.Helper()
	msgs, _ := obj["messages"].([]any)
	msg, _ := msgs[i].(map[string]any)
	content, _ := msg["content"].(string)
	return content
}

// TestRewriteSystemScope_NonSystemPreserved is the core regression: the long
// user message, the assistant history entry and the long tool result must
// survive untouched; only the long system prompt is replaced.
func TestRewriteSystemScope_NonSystemPreserved(t *testing.T) {
	obj := scopeTestBody(t)
	rewriteSystemInPlace(obj)

	if c := msgContent(t, obj, 0); c != neutralPrompt {
		t.Errorf("long system prompt should be replaced, got %.40q", c)
	}
	if c := msgContent(t, obj, 1); !strings.Contains(c, "please review this log") || len(c) <= maxSystemPromptBytes {
		t.Errorf("long user message must be preserved verbatim (len=%d)", len(c))
	}
	if c := msgContent(t, obj, 3); !strings.Contains(c, "cmd output") || len(c) <= maxSystemPromptBytes {
		t.Errorf("long tool result must be preserved verbatim (len=%d)", len(c))
	}
}

// TestRewriteSystemScope_AgentIdentityUserPreserved: a user message that
// quotes an agent identity line (e.g. discussing Claude Code) must NOT be
// replaced — the agent-pattern filter is a system-prompt defense, not a
// conversation filter.
func TestRewriteSystemScope_AgentIdentityUserPreserved(t *testing.T) {
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "why does 'you are claude code, anthropic's official cli' trigger my editor?"},
	}}
	rewriteSystemInPlace(obj)
	msgs, _ := obj["messages"].([]any)
	msg, _ := msgs[0].(map[string]any)
	if c, _ := msg["content"].(string); !strings.Contains(c, "trigger my editor") {
		t.Errorf("user message quoting an agent line must be preserved, got %q", c)
	}
}

// TestRewriteSystemScope_ShortSystemKeepsTemplateFix: short system prompts
// that don't trip the wholesale path still get the single-word substitutions.
func TestRewriteSystemScope_ShortSystemKeepsTemplateFix(t *testing.T) {
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "Main branch (you will usually use this for PRs)"},
	}}
	rewriteSystemInPlace(obj)
	msgs, _ := obj["messages"].([]any)
	msg, _ := msgs[0].(map[string]any)
	if c, _ := msg["content"].(string); !strings.Contains(c, "Default branch") {
		t.Errorf("short system template fix should still apply, got %q", c)
	}
}

// TestRewriteSystemScope_SystemArrayCollapses: a long multimodal system
// message collapses into a SINGLE text part (OmniRoute parity), not one
// neutralPrompt per part.
func TestRewriteSystemScope_SystemArrayCollapses(t *testing.T) {
	long := strings.Repeat("y", maxSystemPromptBytes+10)
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": []any{
			map[string]any{"type": "text", "text": long},
			map[string]any{"type": "text", "text": long},
		}},
	}}
	rewriteSystemInPlace(obj)
	msgs, _ := obj["messages"].([]any)
	msg, _ := msgs[0].(map[string]any)
	parts, ok := msg["content"].([]any)
	if !ok {
		t.Fatalf("array content should stay an array, got %T", msg["content"])
	}
	if len(parts) != 1 {
		t.Fatalf("replacement must collapse to ONE part, got %d", len(parts))
	}
	part, _ := parts[0].(map[string]any)
	if part["text"] != neutralPrompt {
		t.Errorf("single part should carry neutralPrompt, got %.40q", part["text"])
	}
}

// TestRewriteSystemScope_ShortSystemArrayUntouched: a short multimodal system
// message keeps its parts (and per-part template fixes still apply).
func TestRewriteSystemScope_ShortSystemArrayUntouched(t *testing.T) {
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": []any{
			map[string]any{"type": "text", "text": "Main branch (you will usually use this for PRs)"},
		}},
	}}
	rewriteSystemInPlace(obj)
	msgs, _ := obj["messages"].([]any)
	msg, _ := msgs[0].(map[string]any)
	parts, ok := msg["content"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("short array must keep its single part, got %T", msg["content"])
	}
	part, _ := parts[0].(map[string]any)
	if txt, _ := part["text"].(string); !strings.Contains(txt, "Default branch") {
		t.Errorf("per-part template fix should still apply, got %q", txt)
	}
}

// TestPrepareUpstreamBody_ScopeEndToEnd runs the full pipeline the executor
// uses, to prove the scope fix holds after all rewrites compose.
func TestPrepareUpstreamBody_ScopeEndToEnd(t *testing.T) {
	obj := scopeTestBody(t)
	raw, _ := json.Marshal(obj)
	out := prepareUpstreamBody(raw, nil, nil, "")
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs, _ := body["messages"].([]any)
	sys, _ := msgs[0].(map[string]any)
	if sys["content"] != neutralPrompt {
		t.Errorf("system should be neutralized end-to-end, got %.40q", sys["content"])
	}
	tool, _ := msgs[3].(map[string]any)
	tc, _ := tool["content"].(string)
	if !strings.Contains(tc, "cmd output") || len(tc) <= maxSystemPromptBytes {
		t.Errorf("tool result must survive the full pipeline (len=%d)", len(tc))
	}
}
