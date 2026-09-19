// body.go constructs the QoderWork agent_chat_generation request body from
// OpenAI-style chat completion inputs.
//
// The base template lives in baseprompt.json (embedded). Per-request we
// overwrite request/session ids, timestamps, model key, and the user prompt.
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

//go:embed baseprompt.json
var basepromptJSON []byte

// cpaToUpstreamKey maps CPA-facing model names to upstream keys.
// Unknown names pass through unchanged (server silently routes to auto).
func cpaToUpstreamKey(cpaModel string) string {
	switch cpaModel {
	case "qoder-auto", "auto":
		return "auto"
	case "qwen3.8-max-preview", "qwen3.8-max", "qmodel_preview":
		return "qmodel_preview"
	case "qwen3.8-flash", "qfmodel":
		return "qfmodel"
	case "qwen3.7-max", "qmodel_latest":
		return "qmodel_latest"
	case "qwen3.7-plus", "qmodel":
		return "qmodel"
	case "qwen3.6-flash", "q36fmodel":
		return "q36fmodel"
	case "deepseek-v4-pro", "dmodel":
		return "dmodel"
	case "deepseek-v4-flash", "dfmodel":
		return "dfmodel"
	case "glm-5.2", "gm51model":
		return "gm51model"
	case "kimi-k2.7-code", "kmodel":
		return "kmodel"
	case "minimax-m2.7", "mmodel":
		return "mmodel"
	}
	return cpaModel
}

// openAIMessage is one message in the OpenAI chat completion format. The
// client's fields ride along verbatim: role/content are decoded for routing
// and everything else (tool_calls, tool_call_id, name, structured content
// parts, ...) is preserved byte-for-byte, so multi-turn tool conversations
// and multimodal payloads survive the hop upstream.
type openAIMessage struct {
	Role       string
	Content    string
	contentSet bool
	// rawContent holds the original JSON when content was not a plain string.
	rawContent string
	// raw holds every other client member verbatim.
	raw map[string]json.RawMessage
}

func (m *openAIMessage) UnmarshalJSON(data []byte) error {
	m.Role, m.Content, m.contentSet, m.rawContent, m.raw = "", "", false, "", nil
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if role, ok := fields["role"]; ok {
		_ = json.Unmarshal(role, &m.Role)
	}
	if content, ok := fields["content"]; ok {
		m.contentSet = true
		var s string
		// 0.8.12: JSON null round-trips verbatim — unmarshaling null into a
		// string silently yields "" and would rewrite the member behind the
		// client's back (assistant messages carrying tool_calls commonly have
		// content:null; the protocol authority qoderwork2api preserves null
		// the same way via plain map decode).
		if trimmed := strings.TrimSpace(string(content)); trimmed == "null" {
			m.rawContent = string(content)
		} else if err := json.Unmarshal(content, &s); err == nil {
			m.Content = s
		} else {
			m.rawContent = string(content)
		}
		delete(fields, "content")
	}
	delete(fields, "role")
	if len(fields) > 0 {
		m.raw = fields
	}
	return nil
}

func (m openAIMessage) MarshalJSON() ([]byte, error) {
	out := make(map[string]json.RawMessage, len(m.raw)+2)
	for k, v := range m.raw {
		out[k] = v
	}
	switch {
	case m.rawContent != "":
		out["content"] = json.RawMessage(m.rawContent)
	case m.contentSet:
		enc, err := json.Marshal(m.Content)
		if err != nil {
			return nil, err
		}
		out["content"] = enc
	}
	enc, err := json.Marshal(m.Role)
	if err != nil {
		return nil, err
	}
	out["role"] = enc
	return json.Marshal(out)
}

// messageTextContent renders one message's textual content for the
// chat_context.text mirror of the latest user prompt. Plain strings pass
// through; structured content arrays contribute their text parts.
func messageTextContent(m openAIMessage) string {
	if m.rawContent == "" {
		return m.Content
	}
	if !strings.HasPrefix(m.rawContent, "[") {
		return ""
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(m.rawContent), &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(p.Text)
	}
	return b.String()
}

// openAIRequest is the CPA-facing chat completion request.
type openAIRequest struct {
	Model    string          `json:"model"`
	Messages []openAIMessage `json:"messages"`
	Stream   bool            `json:"stream"`
	// ReasoningEffort is the OpenAI-style thinking dial. Upstream thinking_config
	// accepts low/medium/xhigh (2026-09-18 probe); empty or invalid leaves the
	// parameters block untouched so upstream applies its own default (medium).
	ReasoningEffort string `json:"reasoning_effort"`
	// Tools is the client's OpenAI tools array, forwarded verbatim when present.
	Tools json.RawMessage `json:"tools,omitempty"`
}

// extractLatestUserPrompt returns the textual content of the last user
// message (structured content arrays contribute their text parts).
func extractLatestUserPrompt(messages []openAIMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messageTextContent(messages[i])
		}
	}
	return ""
}

// runeSafePrefix truncates to n runes without splitting UTF-8 sequences
// (upstream truncates business.name to 30 runes the same way).
func runeSafePrefix(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// normalizeReasoningEffort validates the OpenAI-style reasoning_effort dial.
// Upstream accepts low/medium/xhigh; anything else returns "" = inject nothing.
// All models share the qfmodel dial set (2026-09-19 unified reasoning chain).
func normalizeReasoningEffort(s string) string {
	effort := strings.ToLower(strings.TrimSpace(s))
	switch effort {
	case "low", "medium", "xhigh":
		return effort
	}
	return ""
}

// buildQoderBody renders the upstream agent_chat_generation body for one request.
// modelKey is the upstream key (already mapped via cpaToUpstreamKey).
func buildQoderBody(req *openAIRequest, modelKey, userType string) ([]byte, error) {
	var base map[string]any
	if err := json.Unmarshal(basepromptJSON, &base); err != nil {
		return nil, fmt.Errorf("baseprompt decode: %w", err)
	}

	prompt := extractLatestUserPrompt(req.Messages)
	if prompt == "" {
		return nil, fmt.Errorf("no user message in request")
	}

	nid := uuid.NewString()
	base["request_id"] = nid
	base["chat_record_id"] = nid
	base["request_set_id"] = uuid.NewString()
	base["session_id"] = uuid.NewString()
	base["stream"] = true
	base["aliyun_user_type"] = userType
	base["agent_id"] = "agent_common"

	// model_config. 2026-09-19 upstream behavior: every request sends
	// is_reasoning=true + source="system" (the qwen3.8-flash reasoning chain).
	// Models that cannot think ignore these fields upstream (verified by the
	// reference proxy with a fabricated model key). source="system" is the real
	// reasoning trigger — without it the gateway never streams reasoning_content.
	if mc, ok := base["model_config"].(map[string]any); ok {
		mc["key"] = modelKey
		mc["is_reasoning"] = true
		if _, ok := mc["source"]; !ok {
			mc["source"] = "system"
		}
	}

	// chat_context.text.text + chat_context.extra.originalContent.text
	if cc, ok := base["chat_context"].(map[string]any); ok {
		if txt, ok := cc["text"].(map[string]any); ok {
			txt["text"] = prompt
		}
		if extra, ok := cc["extra"].(map[string]any); ok {
			if oc, ok := extra["originalContent"].(map[string]any); ok {
				oc["text"] = prompt
			}
			if mc, ok := extra["modelConfig"].(map[string]any); ok {
				mc["key"] = modelKey
				mc["is_reasoning"] = true
			}
		}
	}

	// messages: slim passthrough when the client carries its own system prompt
	// (every harness does) — forward the conversation verbatim and drop the
	// 10657-token template system prompt plus template tools. Upstream verified
	// 2026-09-18 that neither is required (baseline prompt_tokens ~10K → ~60),
	// which directly extends the headroom before oversized requests fail.
	// Bare prompts without a system message keep the template for parity.
	slim := false
	for _, m := range req.Messages {
		if m.Role == "system" || m.Role == "developer" {
			slim = true
			break
		}
	}
	var outMsgs []any
	if slim {
		delete(base, "tools")
		for _, m := range req.Messages {
			outMsgs = append(outMsgs, m)
		}
	} else {
		if msgs, ok := base["messages"].([]any); ok {
			for _, m := range msgs {
				if mm, ok := m.(map[string]any); ok {
					if role, _ := mm["role"].(string); role == "system" {
						outMsgs = append(outMsgs, m)
					}
				}
			}
		}
		for _, m := range req.Messages {
			outMsgs = append(outMsgs, m)
		}
	}
	base["messages"] = outMsgs

	// Client tools win; template tools only ride along in template mode.
	if len(req.Tools) > 0 && string(req.Tools) != "null" {
		base["tools"] = req.Tools
	}

	// business
	if biz, ok := base["business"].(map[string]any); ok {
		biz["id"] = uuid.NewString()
		biz["begin_at"] = time.Now().UnixMilli()
		biz["name"] = runeSafePrefix(prompt, 30)
	}

	// Reasoning dial: inject upstream thinking parameters only when the client
	// supplied a valid reasoning_effort. Absent/invalid keeps upstream defaults;
	// the template parameters (max_tokens) are preserved either way.
	if effort := normalizeReasoningEffort(req.ReasoningEffort); effort != "" {
		params, _ := base["parameters"].(map[string]any)
		if params == nil {
			params = map[string]any{}
			base["parameters"] = params
		}
		params["enable_thinking"] = true
		params["reasoning_effort"] = effort
	}

	return json.Marshal(base)
}
