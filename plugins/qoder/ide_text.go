// ide_text.go pulls Qoder's dynamic model presentation text.
//
// Why this exists (2026-10-24): the activity panel only showed structured
// offers, so "DeepSeek-Flash 个人版限时免费，至 11月01日 00:00（UTC+8）" was
// invisible even though the desktop client displays it. That sentence is not in
// /api/v2/model/list at all -- the payload there carries is_free and the price
// factors but no wording and no expiry.
//
// Qoder ships the wording through a public, unauthenticated channel used by the
// IDE and desktop client:
//
//	GET https://<gateway>/ide-text/latest?namespace=qoder-ide
//	 -> {"data":{"version":"...","ossUrl":"...","publishTime":"..."}}
//	GET <ossUrl>
//	 -> {"zh":{"modelSelector.item.<key>.markdownDescription":"...",...},"en":{...}}
//
// Nothing here is derived by guessing: if the text is absent, the offer simply
// has no description. The expiry is free-form Chinese prose ("至 11月01日
// 00:00（UTC+8）"), so it is surfaced verbatim rather than parsed into a
// timestamp the upstream never published.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ideTextTTL bounds how often the presentation text is re-pulled. Upstream
// republishes a handful of times a day, so an hour is generous and keeps the
// panel's own hourly refresh meaningful.
const ideTextTTL = time.Hour

// ideTextGateway is the per-region host that serves /ide-text/latest.
//
// Intl is NOT the inference gateway: api3.qoder.sh answers 404 for this path,
// while center.qoder.sh serves it. Verified live 2026-10-24.
const ideTextBaseIntl = "https://center.qoder.sh"

var ideTextGateway = map[string]string{
	regionCN:   gatewayBaseCN,
	regionIntl: ideTextBaseIntl,
}

type ideTextSnapshot struct {
	// byKey maps a bare model key (dfmodel) to its resolved text.
	byKey map[string]ideTextEntry
	at    time.Time
	// err records the last failure so the panel can say "text unavailable"
	// instead of silently showing nothing.
	err string
}

type ideTextEntry struct {
	Name    string `json:"name"`
	Desc    string `json:"description"`
	Detail  string `json:"detail"`
	LinkURL string `json:"link_url"`
	// Lang marks which locale produced this entry.
	Lang string `json:"lang"`
}

var (
	ideTextMu    sync.RWMutex
	ideTextCache = map[string]ideTextSnapshot{}
)

// ideTextKeys are the prefixes the desktop client and IDE use for the same
// model. The desktop keys are model.<key>.*, the IDE keys are
// modelSelector.item.<key>.*. Both are read so one channel dying still leaves
// usable text.
const (
	ideTextPrefixDesktop = "model."
	ideTextPrefixIDE     = "modelSelector.item."
	ideTextSuffixName    = ".label"
	ideTextSuffixDesc    = ".description"
	ideTextSuffixDetail  = ".markdownDescription"
	ideTextSuffixDetailD = ".detail"
)

// fetchIDETextLatest resolves the current OSS document for one region. It is a
// variable so tests can serve fixtures without network access.
var fetchIDETextLatest = func(region string) (string, error) {
	base := ideTextGateway[region]
	if base == "" {
		return "", errIDETextUnsupported{}
	}
	url := base + "/ide-text/latest?namespace=qoder-ide"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hostHTTPDo(req)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", errIDETextUnsupported{}
	}
	var out struct {
		Success bool `json:"success"`
		Data    struct {
			OSSURL string `json:"ossUrl"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return "", err
	}
	if strings.TrimSpace(out.Data.OSSURL) == "" {
		return "", errIDETextUnsupported{}
	}
	return strings.TrimSpace(out.Data.OSSURL), nil
}

// fetchIDETextDocument downloads one resolved text document.
var fetchIDETextDocument = func(url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errIDETextUnsupported{}
	}
	return resp.Body, nil
}

type errIDETextUnsupported struct{}

func (errIDETextUnsupported) Error() string { return "ide-text endpoint unsupported" }

// parseIDETextDocument flattens the zh/en maps into per-model entries. Chinese
// wins when both locales exist: the panel is Chinese-first, and the offer text
// is the thing an operator reads.
func parseIDETextDocument(raw []byte) map[string]ideTextEntry {
	var doc struct {
		Zh map[string]string `json:"zh"`
		En map[string]string `json:"en"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	out := map[string]ideTextEntry{}
	// Chinese is the panel's primary locale and the campaign wording is written
	// there first; English only fills fields Chinese left empty. Applying
	// English last would overwrite the Chinese expiry sentence.
	applyLocale(out, doc.En, "en")
	applyLocale(out, doc.Zh, "zh", true)
	return out
}

func applyLocale(dst map[string]ideTextEntry, texts map[string]string, lang string, primary ...bool) {
	overwrite := len(primary) > 0 && primary[0]
	// Map iteration order is random, so "last writer wins" made the result
	// depend on luck when both namespaces describe the same model. The IDE
	// document is the richer one, so apply the desktop keys first and let IDE
	// keys land last.
	for _, wantIDE := range []bool{false, true} {
		for key, value := range texts {
			modelKey, field, isIDE := splitIDETextKeyNS(key)
			if modelKey == "" || field == "" || isIDE != wantIDE {
				continue
			}
			entry := dst[modelKey]
			trimmed := strings.TrimSpace(value)
			set := func(current *string) {
				if strings.TrimSpace(*current) == "" {
					*current = trimmed
					return
				}
				if overwrite || wantIDE {
					*current = trimmed
				}
			}
			switch field {
			case "name":
				set(&entry.Name)
			case "desc":
				set(&entry.Desc)
			case "detail":
				set(&entry.Detail)
				entry.Lang = lang
			}
			dst[modelKey] = entry
		}
	}
}

// splitIDETextKey turns a raw text key into (modelKey, field). Keys that are
// not per-model (there are ~200 of them) return empty and are ignored.
func splitIDETextKey(key string) (string, string) {
	modelKey, field, _ := splitIDETextKeyNS(key)
	return modelKey, field
}

// splitIDETextKeyNS also reports which namespace the key came from.
func splitIDETextKeyNS(key string) (string, string, bool) {
	// Longest prefix first: "modelSelector.item." also starts with "model", so
	// checking the desktop prefix first would parse "dfmodel" as a field path
	// and silently drop every IDE key.
	if strings.HasPrefix(key, ideTextPrefixIDE) {
		modelKey, field := splitIDEField(strings.TrimPrefix(key, ideTextPrefixIDE))
		return modelKey, field, true
	}
	if strings.HasPrefix(key, ideTextPrefixDesktop) {
		modelKey, field := splitIDEField(strings.TrimPrefix(key, ideTextPrefixDesktop))
		return modelKey, field, false
	}
	return "", "", false
}

func splitIDEField(rest string) (string, string) {
	for suffix, field := range map[string]string{
		ideTextSuffixDetail:  "detail",
		ideTextSuffixDetailD: "detail",
		ideTextSuffixDesc:    "desc",
		ideTextSuffixName:    "name",
	} {
		if strings.HasSuffix(rest, suffix) {
			return strings.TrimSuffix(rest, suffix), field
		}
	}
	// The IDE document stores the display name under the bare model key --
	// "modelSelector.item.dfmodel": "DeepSeek-Flash" -- with no suffix at all.
	// A bare key that still contains a dot is a nested namespace we do not
	// render (for example "modelSelector.item.group.title").
	if rest != "" && !strings.Contains(rest, ".") {
		return rest, "name"
	}
	return "", ""
}

// refreshIDEText pulls one region's text if the snapshot is missing or stale.
// Best-effort: a failure keeps whatever was cached and records why.
func refreshIDEText(region string, force bool) ideTextSnapshot {
	ideTextMu.RLock()
	cached, ok := ideTextCache[region]
	ideTextMu.RUnlock()
	if ok && !force && time.Since(cached.at) < ideTextTTL {
		return cached
	}
	ossURL, err := fetchIDETextLatest(region)
	if err != nil {
		snapshot := cached
		snapshot.err = err.Error()
		ideTextMu.Lock()
		ideTextCache[region] = snapshot
		ideTextMu.Unlock()
		return snapshot
	}
	raw, err := fetchIDETextDocument(ossURL)
	if err != nil {
		snapshot := cached
		snapshot.err = err.Error()
		ideTextMu.Lock()
		ideTextCache[region] = snapshot
		ideTextMu.Unlock()
		return snapshot
	}
	byKey := parseIDETextDocument(raw)
	if len(byKey) == 0 {
		snapshot := cached
		snapshot.err = "ide-text document contained no model entries"
		ideTextMu.Lock()
		ideTextCache[region] = snapshot
		ideTextMu.Unlock()
		return snapshot
	}
	snapshot := ideTextSnapshot{byKey: byKey, at: time.Now()}
	ideTextMu.Lock()
	ideTextCache[region] = snapshot
	ideTextMu.Unlock()
	return snapshot
}

// ideTextFor returns one model's text without triggering a network call.
// Callers refresh explicitly, so a panel poll never blocks on upstream.
func ideTextFor(region, modelKey string) (ideTextEntry, bool) {
	ideTextMu.RLock()
	snapshot, ok := ideTextCache[region]
	ideTextMu.RUnlock()
	if !ok || snapshot.byKey == nil {
		return ideTextEntry{}, false
	}
	entry, found := snapshot.byKey[modelKey]
	if !found {
		return ideTextEntry{}, false
	}
	return entry, true
}

// ideTextHasOffer reports whether a model's presentation text describes a
// time-limited offer. This is wording detection, not a price signal: it lets the
// panel list "限时免费，至…" even though the model list has no promotion object.
func ideTextHasOffer(entry ideTextEntry) bool {
	text := entry.Detail
	if text == "" {
		text = entry.Desc
	}
	if text == "" {
		return false
	}
	for _, marker := range []string{"限时", "免费", "折扣", "1 折", "1折", "活动", "Off-Peak", "Limited-Time", "free", "Free", "discount"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// ideTextLink extracts the first http(s) link from markdown or plain text.
func ideTextLink(entry ideTextEntry) string {
	if entry.LinkURL != "" {
		return entry.LinkURL
	}
	text := entry.Detail
	for {
		open := strings.Index(text, "](")
		if open < 0 {
			break
		}
		start := text[open+2:]
		end := strings.Index(start, ")")
		if end < 0 {
			break
		}
		url := strings.TrimSpace(start[:end])
		if strings.HasPrefix(url, "http") {
			return url
		}
		text = start[end:]
	}
	// A bare URL in plain text.
	for _, field := range strings.Fields(entry.Detail) {
		candidate := strings.Trim(field, "()[]，。,.；;")
		if strings.HasPrefix(candidate, "http") {
			return candidate
		}
	}
	return ""
}

// stripMarkdownLinks turns "[查看详情](url)" into "查看详情" for plain-text
// fields, while the URL rides in its own field.
func stripMarkdownLinks(text string) string {
	for {
		open := strings.Index(text, "[")
		if open < 0 {
			break
		}
		closeBracket := strings.Index(text[open:], "]")
		if closeBracket < 0 {
			break
		}
		closeBracket += open
		after := text[closeBracket:]
		if !strings.HasPrefix(after, "](") {
			text = text[:open] + " " + text[open+1:]
			continue
		}
		end := strings.Index(after, ")")
		if end < 0 {
			break
		}
		label := text[open+1 : closeBracket]
		text = text[:open] + label + after[end+1:]
	}
	return strings.TrimSpace(text)
}

func resetIDETextCacheForTest() func() {
	ideTextMu.Lock()
	prev := ideTextCache
	ideTextCache = map[string]ideTextSnapshot{}
	ideTextMu.Unlock()
	return func() {
		ideTextMu.Lock()
		ideTextCache = prev
		ideTextMu.Unlock()
	}
}
