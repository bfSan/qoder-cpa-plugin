// payload.go rewrites the outgoing chat completion request body before it's
// forwarded upstream. The single-pass entry point is prepareUpstreamBody; the
// four *InPlace helpers are the field-level mutations it composes, and the
// legacy *ForUpstream / forceStreamBody wrappers exist for tests and other
// call sites that need them individually.
package main

import (
        "encoding/json"
        "regexp"
        "strings"
)

// neutralPrompt is the substitute for over-long or agent-identity system
// prompts that Tencent CodeBuddy's content filter rejects. Mirrors OmniRoute's
// codebuddy-cn.ts NEUTRAL_PROMPT.
const neutralPrompt = "You are a helpful AI assistant that helps with software engineering tasks."

// agentPattern matches Claude Code / Cursor / Windsurf / Cline / Aider / Continue
// / Copilot / Cody identity lines that Tencent's content filter blocklists.
// Ported verbatim from OmniRoute/open-sse/executors/codebuddy-cn.ts (MIT).
var agentPattern = regexp.MustCompile(`(?i)you are claude code|claude.?code.+official.+cli|anthropic.+official.+cli|anxthxropic.+official.+cli|you are (?:cursor|windsurf|cline|aider|continue|copilot|cody)|you are an? (?:ai )?(?:coding |code )?agent|cc_entrypoint\s*=\s*(?:cli|vscode|jetbrains|gui)|claude.?code.+issues|give feedback.+claude.?code|you are .{0,30}(?:powerful )?ai agent|orchestration capabilities|OhMyOpenCode|<agent-identity>|<Role>|<Behavior_Instructions>`)

// maxSystemPromptBytes is the byte-length threshold above which a system prompt
// is replaced wholesale with neutralPrompt. Tencent's filter rejects very long
// system prompts even when they don't match agentPattern. Mirrors OmniRoute.
const maxSystemPromptBytes = 2000

// toolDescriptionByteLimit is the threshold above which all tool descriptions
// are stripped to avoid Tencent's 64KB body-size filter. Mirrors OmniRoute.
const toolDescriptionByteLimit = 65536

// forceStreamBody returns the request body with "stream":true set, since the
// upstream rejects non-streaming chat requests.
// prepareUpstreamBody composes forceStreamBody + normalizeToolsForUpstream +
// rewriteSystemForUpstream + ensureSystemMessage + rewriteModelInBody into a
// single unmarshal/marshal pass (v0.6.31 perf: was 4-5 full JSON round-trips
// on every chat completion). The 4 legacy helpers remain for tests and other
// call sites that need them individually.
func prepareUpstreamBody(payload, original []byte, sa *storedAuth, upstreamModel string) []byte {
        src := payload
        if len(src) == 0 {
                src = original
        }
        if len(src) == 0 {
                return src
        }
        var obj map[string]any
        if json.Unmarshal(src, &obj) != nil {
                return src
        }

        // 1. forceStream: CodeBuddy rejects non-stream requests.
        obj["stream"] = true

        // 2. normalizeTools: tool_choice object form → string; "none" suppresses tools.
        normalizeToolsInPlace(obj)

        // 2.5 normalizeHistory: OpenAI developer 角色 → system（腾讯后端不认
        // developer，实测判 11128 渠道风控——Coding2API 2026-09 生产实证）；
        // 脏 tool_call（无 function/name）剔除，清空后无内容的 assistant 占位
        // 与悬空 role=tool 结果成对清理（大输入/长会话韧性，同源实证对策）。
        normalizeHistoryInPlace(obj)

        // 3. rewriteSystem: strip blocked Claude Code template phrases + force thinking.
        rewriteSystemInPlace(obj)

        // 4. ensureSystemMessage: inject minimal system msg for Global only.
        ensureSystemMessageInPlace(obj, sa)

        // 5. rewriteModel: swap client model name to upstream model id.
        rewriteModelInPlace(obj, upstreamModel)

        // 6. maxTokenRename: OpenAI-newer clients send max_completion_tokens;
        // the CodeBuddy upstream speaks max_tokens. Copy the value across when
        // max_tokens is absent, then drop the foreign key so the gateway does
        // not see an unknown parameter (wb2api payload.go, PR #116 semantics).
        if v, ok := obj["max_completion_tokens"]; ok {
                if _, has := obj["max_tokens"]; !has && v != nil {
                        obj["max_tokens"] = v
                }
                delete(obj, "max_completion_tokens")
        }

        out, err := json.Marshal(obj)
        if err != nil {
                return src
        }
        return out
}

// normalizeHistoryInPlace normalizes message history before the upstream
// sees it (v0.9.15, large-input resilience):
//   - role "developer" → "system": the Tencent backend does not recognize
//     the OpenAI developer role and rejects the whole request as channel
//     risk-control 11128 (reasoning-model clients like PI convert system
//     messages to developer; Coding2API production evidence 2026-09).
//   - dirty tool_calls (missing function or empty name) are dropped; an
//     assistant placeholder left with no content after the cleanup is
//     dropped entirely; dangling role=tool results whose tool_call_id
//     points at a dropped call are dropped with it (same shape as the
//     Coding2API _clean_history_tool_calls defense). Oversized agent
//     histories trimmed by the client mid-conversation are the main
//     source of such orphans.
func normalizeHistoryInPlace(obj map[string]any) bool {
        msgs, ok := obj["messages"].([]any)
        if !ok || len(msgs) == 0 {
                return false
        }
        changed := false
        orphan := map[string]struct{}{}
        rewritten := make([]any, 0, len(msgs))
        for _, mi := range msgs {
                m, ok := mi.(map[string]any)
                if !ok {
                        rewritten = append(rewritten, mi)
                        continue
                }
                if role, _ := m["role"].(string); strings.EqualFold(role, "developer") {
                        m["role"] = "system"
                        changed = true
                }
                if tcs, ok := m["tool_calls"].([]any); ok {
                        kept := make([]any, 0, len(tcs))
                        for _, tci := range tcs {
                                tc, ok := tci.(map[string]any)
                                if !ok {
                                        continue
                                }
                                fn, _ := tc["function"].(map[string]any)
                                name := ""
                                if fn != nil {
                                        name, _ = fn["name"].(string)
                                }
                                if strings.TrimSpace(name) == "" {
                                        if id, _ := tc["id"].(string); id != "" {
                                                orphan[id] = struct{}{}
                                        }
                                        changed = true
                                        continue
                                }
                                kept = append(kept, tc)
                        }
                        if len(kept) == 0 {
                                delete(m, "tool_calls")
                                changed = true
                                if cv, has := m["content"]; !has || cv == nil {
                                        continue // 清空后无内容的占位 assistant 整条丢弃
                                }
                        } else {
                                m["tool_calls"] = kept
                        }
                }
                rewritten = append(rewritten, m)
        }
        if len(orphan) > 0 {
                filtered := make([]any, 0, len(rewritten))
                for _, mi := range rewritten {
                        m, ok := mi.(map[string]any)
                        if !ok {
                                filtered = append(filtered, mi)
                                continue
                        }
                        if role, _ := m["role"].(string); strings.EqualFold(role, "tool") {
                                id, _ := m["tool_call_id"].(string)
                                if _, bad := orphan[id]; bad {
                                        changed = true
                                        continue
                                }
                        }
                        filtered = append(filtered, m)
                }
                rewritten = filtered
        }
        if !changed {
                return false
        }
        obj["messages"] = rewritten
        return true
}

// normalizeToolsInPlace is the in-place form of normalizeToolsForUpstream.
// Returns true when obj was modified.
func normalizeToolsInPlace(obj map[string]any) bool {
        changed := false
        suppressTools := func() {
                if _, ok := obj["tools"]; ok {
                        delete(obj, "tools")
                        changed = true
                }
                if _, ok := obj["functions"]; ok {
                        delete(obj, "functions")
                        changed = true
                }
        }
        if tc, present := obj["tool_choice"]; present {
                switch v := tc.(type) {
                case string:
                        if strings.EqualFold(strings.TrimSpace(v), "none") {
                                delete(obj, "tool_choice")
                                suppressTools()
                                changed = true
                        }
                case map[string]any:
                        typ, _ := v["type"].(string)
                        typ = strings.ToLower(strings.TrimSpace(typ))
                        switch typ {
                        case "none":
                                delete(obj, "tool_choice")
                                suppressTools()
                                changed = true
                        case "auto", "required":
                                obj["tool_choice"] = typ
                                changed = true
                        case "function":
                                name := ""
                                if fn, ok := v["function"].(map[string]any); ok {
                                        name, _ = fn["name"].(string)
                                }
                                if name == "" {
                                        name, _ = v["name"].(string)
                                }
                                name = strings.TrimSpace(name)
                                if name != "" {
                                        obj["tool_choice"] = name
                                } else {
                                        obj["tool_choice"] = "auto"
                                }
                                changed = true
                        default:
                                delete(obj, "tool_choice")
                                changed = true
                        }
                default:
                        delete(obj, "tool_choice")
                        changed = true
                }
        }
        return changed
}

// rewriteSystemInPlace is the in-place form of rewriteSystemForUpstream.
// It does three things:
//  1. For each system message: if length > maxSystemPromptBytes or matches
//     agentPattern, replace content wholesale with neutralPrompt. Otherwise,
//     apply sanitizeBlockedTemplates (single-word substitutions).
//  2. Strip reasoning_effort "none"/"off" (Tencent rejects them); mirror
//     other values to reasoning_summary="auto" (OmniRoute codebuddy-cn.ts).
//  3. forceMaxThinking for hy3/hy4-family models.
func rewriteSystemInPlace(obj map[string]any) bool {
        changed := rewriteSystemMessagesInPlace(obj)
        if mirrorReasoningEffort(obj) {
                changed = true
        }
        if forceMaxThinking(obj) {
                changed = true
        }
        if compactToolDescriptions(obj) {
                changed = true
        }
        return changed
}

// rewriteSystemMessagesInPlace applies the content-filter defenses to SYSTEM
// messages only. v0.9.18 scope fix: the pre-0.9.18 loop fed EVERY message
// through the wholesale replacement, so any user paste / tool result /
// assistant history entry over maxSystemPromptBytes (or matching agentPattern)
// was silently rewritten to neutralPrompt and the upstream model saw a
// history full of hollow "You are a helpful AI assistant..." messages. Field
// reports mapping to exactly this: tool output that "looks truncated / stdout
// empty", conversations that "reset every so often", and the model answering
// with the neutral prompt itself on interruption. OmniRoute codebuddy-cn.ts
// (the porting source) gates the replacement on
// `message.role !== "system" -> return message` verbatim; this restores it.
func rewriteSystemMessagesInPlace(obj map[string]any) bool {
        messages, _ := obj["messages"].([]any)
        changed := false
        for _, m := range messages {
                msg, ok := m.(map[string]any)
                if !ok {
                        continue
                }
                if role, _ := msg["role"].(string); !strings.EqualFold(role, "system") {
                        continue
                }
                if rewriteSystemContentField(msg) {
                        changed = true
                }
        }
        return changed
}

// rewriteSystemContentField applies the filter defenses to one SYSTEM
// message's content. Mirrors OmniRoute codebuddy-cn.ts: a string content is
// replaced wholesale with neutralPrompt when it exceeds maxSystemPromptBytes
// or matches agentPattern; an array content is flattened (text parts joined
// with newlines) for the same decision and, when replaced, collapses into a
// SINGLE {"type":"text"} part — not one neutralPrompt per part. Short clean
// content only goes through the single-word template substitutions.
func rewriteSystemContentField(msg map[string]any) bool {
        switch c := msg["content"].(type) {
        case string:
                if len(c) > maxSystemPromptBytes || agentPattern.MatchString(c) {
                        msg["content"] = neutralPrompt
                        return true
                }
                if r := sanitizeBlockedTemplates(c); r != c {
                        msg["content"] = r
                        return true
                }
        case []any:
                text := flattenSystemParts(c)
                if len(text) > maxSystemPromptBytes || agentPattern.MatchString(text) {
                        msg["content"] = []any{map[string]any{"type": "text", "text": neutralPrompt}}
                        return true
                }
                modified := false
                for _, p := range c {
                        part, ok := p.(map[string]any)
                        if !ok {
                                continue
                        }
                        if t, ok := part["text"].(string); ok {
                                if r := sanitizeBlockedTemplates(t); r != t {
                                        part["text"] = r
                                        modified = true
                                }
                        }
                }
                return modified
        }
        return false
}

// flattenSystemParts mirrors OmniRoute's flatten(): join the text of all
// {"type":"text","text":...} parts with newlines; non-text parts contribute
// nothing (an image-only system message stays empty and is never replaced).
func flattenSystemParts(parts []any) string {
        texts := make([]string, 0, len(parts))
        for _, p := range parts {
                part, ok := p.(map[string]any)
                if !ok {
                        continue
                }
                if t, ok := part["text"].(string); ok {
                        texts = append(texts, t)
                }
        }
        return strings.Join(texts, "\n")
}

// mirrorReasoningEffort implements OmniRoute codebuddy-cn.ts reasoning_effort
// handling:
//   - "none"/"off" → delete field (Tencent has no "none")
//   - other value → set reasoning_summary="auto" (mirror)
//   - absent → no-op (forcing reasoning triggers content filter)
func mirrorReasoningEffort(obj map[string]any) bool {
        eff, ok := obj["reasoning_effort"].(string)
        if !ok {
                return false
        }
        effLower := strings.ToLower(strings.TrimSpace(eff))
        if effLower == "none" || effLower == "off" {
                delete(obj, "reasoning_effort")
                return true
        }
        if effLower != "" {
                obj["reasoning_summary"] = "auto"
                return true
        }
        return false
}

// compactToolDescriptions strips tool.function.description when the serialized
// tools array exceeds toolDescriptionByteLimit (64KB). Tencent's body-size
// filter rejects large tool descriptions. Mirrors OmniRoute codebuddy-cn.ts.
func compactToolDescriptions(obj map[string]any) bool {
        tools, ok := obj["tools"].([]any)
        if !ok || len(tools) == 0 {
                return false
        }
        serialized, err := json.Marshal(tools)
        if err != nil {
                return false
        }
        if len(serialized) < toolDescriptionByteLimit {
                return false
        }
        changed := false
        for _, t := range tools {
                tool, ok := t.(map[string]any)
                if !ok {
                        continue
                }
                fn, ok := tool["function"].(map[string]any)
                if !ok {
                        continue
                }
                if _, hasDesc := fn["description"]; hasDesc {
                        delete(fn, "description")
                        changed = true
                }
        }
        return changed
}

// ensureSystemMessageInPlace is the in-place form of ensureSystemMessage.
// Returns true when obj was modified.
func ensureSystemMessageInPlace(obj map[string]any, sa *storedAuth) bool {
        if sa == nil || !isGlobalDomain(sa.Auth.Domain) {
                return false
        }
        messages, ok := obj["messages"].([]any)
        if !ok || len(messages) == 0 {
                return false
        }
        for _, m := range messages {
                msg, ok := m.(map[string]any)
                if !ok {
                        continue
                }
                if role, _ := msg["role"].(string); strings.EqualFold(role, "system") {
                        return false
                }
        }
        systemMsg := map[string]any{
                "role":    "system",
                "content": "You are a helpful assistant.",
        }
        obj["messages"] = append([]any{systemMsg}, messages...)
        return true
}

// rewriteModelInPlace swaps obj["model"] to upstreamModel when non-empty.
// Mirrors rewriteModelInBody's behavior (case-insensitive compare); returns
// true when modified.
func rewriteModelInPlace(obj map[string]any, upstreamModel string) bool {
        upstreamModel = strings.TrimSpace(upstreamModel)
        if upstreamModel == "" {
                return false
        }
        cur, _ := obj["model"].(string)
        if strings.EqualFold(strings.TrimSpace(cur), upstreamModel) {
                return false
        }
        obj["model"] = upstreamModel
        return true
}

func forceStreamBody(payload, original []byte) []byte {
        src := payload
        if len(src) == 0 {
                src = original
        }
        var obj map[string]any
        if json.Unmarshal(src, &obj) != nil {
                return src
        }
        obj["stream"] = true
        out, err := json.Marshal(obj)
        if err != nil {
                return src
        }
        return out
}

// normalizeToolsForUpstream adapts OpenAI tools / tool_choice fields to
// CodeBuddy's chat schema before the request is forwarded.
//
// Live-verified against /v2/chat/completions (2026-07):
//  1. tool_choice is typed as string on the upstream Go struct. OpenAI's object
//     form {"type":"function","function":{"name":"..."}} returns 400 code 11101
//     ("cannot unmarshal object into Go struct field Request.tool_choice of
//     type string"). Convert known object shapes to the matching string.
//  2. tool_choice "none" is accepted but ignored when tools[] is non-empty —
//     the model still emits tool_calls. The only reliable way to suppress tools
//     is to omit tools (and functions) entirely.
//
// String values auto / required / <function name> are left untouched.
func normalizeToolsForUpstream(payload []byte) []byte {
        if len(payload) == 0 {
                return payload
        }
        var obj map[string]any
        if json.Unmarshal(payload, &obj) != nil {
                return payload
        }
        changed := false

        suppressTools := func() {
                if _, ok := obj["tools"]; ok {
                        delete(obj, "tools")
                        changed = true
                }
                if _, ok := obj["functions"]; ok {
                        delete(obj, "functions")
                        changed = true
                }
        }

        if tc, present := obj["tool_choice"]; present {
                switch v := tc.(type) {
                case string:
                        if strings.EqualFold(strings.TrimSpace(v), "none") {
                                delete(obj, "tool_choice")
                                suppressTools()
                                changed = true
                        }
                case map[string]any:
                        typ, _ := v["type"].(string)
                        typ = strings.ToLower(strings.TrimSpace(typ))
                        switch typ {
                        case "none":
                                delete(obj, "tool_choice")
                                suppressTools()
                                changed = true
                        case "auto", "required":
                                obj["tool_choice"] = typ
                                changed = true
                        case "function":
                                name := ""
                                if fn, ok := v["function"].(map[string]any); ok {
                                        name, _ = fn["name"].(string)
                                }
                                if name == "" {
                                        name, _ = v["name"].(string)
                                }
                                name = strings.TrimSpace(name)
                                if name != "" {
                                        obj["tool_choice"] = name
                                } else {
                                        // Object force without a name: fall back to auto instead of 400.
                                        obj["tool_choice"] = "auto"
                                }
                                changed = true
                        default:
                                // Unknown object shape → drop rather than forward a 400.
                                delete(obj, "tool_choice")
                                changed = true
                        }
                default:
                        // null / array / number — drop to keep upstream happy.
                        delete(obj, "tool_choice")
                        changed = true
                }
        }

        if !changed {
                return payload
        }
        out, err := json.Marshal(obj)
        if err != nil {
                return payload
        }
        return out
}

// rewriteSystemForUpstream neutralizes Claude Code template phrases that
// Tencent CodeBuddy's content filter blocklists verbatim — the agent identity
// line ("You are Claude Code, Anthropic's official CLI for Claude.") and the
// git injection ("Main branch (you will usually use this for PRs)"). Each
// rewrite is a single-word change so the prompt's meaning is preserved while
// dodging the exact-match filter.
func rewriteSystemForUpstream(payload []byte) []byte {
        if len(payload) == 0 {
                return payload
        }
        var obj map[string]any
        if json.Unmarshal(payload, &obj) != nil {
                return payload
        }
        changed := rewriteSystemMessagesInPlace(obj)
        if forceMaxThinking(obj) {
                changed = true
        }
        if !changed {
                return payload
        }
        out, err := json.Marshal(obj)
        if err != nil {
                return payload
        }
        return out
}

// ensureSystemMessage injects a minimal system message if none is present.
// Global (www.workbuddy.ai) rejects user-only requests with code 11101
// "Parse message failed: 11101:invalid request". CN (copilot.tencent.com)
// does not require a system message but tolerates one. Inserting a
// harmless system message unifies both paths.
func ensureSystemMessage(payload []byte, sa *storedAuth) []byte {
        if len(payload) == 0 {
                return payload
        }
        // Only inject for Global; CN doesn't need it and we minimize diff.
        if sa == nil || !isGlobalDomain(sa.Auth.Domain) {
                return payload
        }
        var obj map[string]any
        if json.Unmarshal(payload, &obj) != nil {
                return payload
        }
        messages, ok := obj["messages"].([]any)
        if !ok || len(messages) == 0 {
                return payload
        }
        for _, m := range messages {
                msg, ok := m.(map[string]any)
                if !ok {
                        continue
                }
                if role, _ := msg["role"].(string); strings.EqualFold(role, "system") {
                        return payload // already has system message
                }
        }
        systemMsg := map[string]any{
                "role":    "system",
                "content": "You are a helpful assistant.",
        }
        obj["messages"] = append([]any{systemMsg}, messages...)
        out, err := json.Marshal(obj)
        if err != nil {
                return payload
        }
        return out
}

// (rewriteContentField / sanitizeContentText were removed in v0.9.18: they
// applied the wholesale neutralPrompt replacement to messages of ANY role.
// The system-scoped replacements live in rewriteSystemContentField above;
// non-system messages are never rewritten — see rewriteSystemMessagesInPlace.)

func sanitizeBlockedTemplates(s string) string {
        s = strings.ReplaceAll(s,
                "You are Claude Code, Anthropic's official CLI for Claude.",
                "You are Claude Code, Anthropic's official CLI tool for Claude.")
        s = strings.ReplaceAll(s,
                "Main branch (you will usually use this for PRs)",
                "Default branch (you will usually use this for PRs)")
        return s
}

// forceMaxThinking pins reasoning_effort to "high" for hy3/hy4-family models
// (Tencent Hunyuan) so they always reason at maximum depth. CodeBuddy only
// honors "high" for deep thinking (medium/low/max/xhigh/ultra all fall back to
// no reasoning), so we override whatever the client sent. Matching is
// case-insensitive because this runs before rewriteModelInPlace swaps the
// client-facing model name for the upstream ID. Returns true if changed.
func forceMaxThinking(obj map[string]any) bool {
        model, _ := obj["model"].(string)
        lm := strings.ToLower(model)
        if !strings.HasPrefix(lm, "hy3") && !strings.HasPrefix(lm, "hy4") {
                return false
        }
        if eff, _ := obj["reasoning_effort"].(string); eff == "high" {
                return false
        }
        obj["reasoning_effort"] = "high"
        return true
}

// rewriteModelInBody replaces the "model" field of a chat-completions body
// with the resolved upstream model ID.
func rewriteModelInBody(body []byte, upstreamModel string) []byte {
        if len(body) == 0 || strings.TrimSpace(upstreamModel) == "" {
                return body
        }
        var obj map[string]any
        if json.Unmarshal(body, &obj) != nil {
                return body
        }
        cur, _ := obj["model"].(string)
        if strings.EqualFold(strings.TrimSpace(cur), strings.TrimSpace(upstreamModel)) {
                return body
        }
        obj["model"] = upstreamModel
        out, err := json.Marshal(obj)
        if err != nil {
                return body
        }
        return out
}

func isEmptyValue(v any) bool {
        switch x := v.(type) {
        case nil:
                return true
        case string:
                return x == ""
        case []any:
                return len(x) == 0
        case map[string]any:
                if len(x) == 0 {
                        return true
                }
                // Legacy function_call shell: {"name":"","arguments":""} is the
                // upstream's terminal-chunk artifact, not a real call — treat as empty
                // when every value is itself empty.
                for _, val := range x {
                        if !isEmptyValue(val) {
                                return false
                        }
                }
                return true
        }
        return false
}
