// model_region_facts.go holds the per-region model capability facts the Qoder
// gateway advertises but pluginapi.ModelInfo cannot carry.
//
// Two upstream fields were being dropped at the parse boundary:
//
//	thinking_config  the reasoning tiers, as {"disabled":{...},"enabled":
//	                 {"efforts":{"low":{},"medium":{"is_default":true}}, ...}}
//	context_config   the selectable context tiers, as {"200K":{"token_count":
//	                 200000,"is_default":true},"400K":{...},"1M":{...}}
//
// Because they never survived parsing, the panel had nothing to show for either
// and fell back to "未上报" / a bare token count, exactly as WorkBuddy did before
// its reasoning field was decoded. Verified against the live CN gateway
// (2026-10-09): 11 of 14 chat models carry thinking_config and 13 of 14 carry
// context_config, in five and three observed shapes respectively.
package main

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// effortRank orders reasoning tiers by intensity rather than alphabetically.
// The upstream names them low/medium/high/xhigh/max; sorting them as strings
// would render "high, low, max, medium, xhigh", which reads as if medium were
// the strongest.
var effortRank = map[string]int{
	"low":    0,
	"medium": 1,
	"high":   2,
	"xhigh":  3,
	"max":    4,
}

// contextTier is one selectable context length. Label is the upstream's own
// name for the tier ("200K"), kept because the panel shows it verbatim and it
// distinguishes tiers whose token counts could otherwise collide.
type contextTier struct {
	Label     string `json:"label"`
	Tokens    int64  `json:"tokens"`
	IsDefault bool   `json:"is_default"`
}

// thinkingFacts is the reasoning capability for one model in one region.
//
// CanDisable is a pointer because upstream distinguishes three states that a
// bool would collapse: a disabled block (thinking can be turned off), no
// disabled block (it cannot), and no thinking_config at all (unreported). Only
// the first is a claim about capability; the third must stay unknown.
type thinkingFacts struct {
	Status               string   `json:"status"`
	Default              string   `json:"default,omitempty"`
	CanDisable           *bool    `json:"can_disable,omitempty"`
	Levels               []string `json:"levels,omitempty"`
	EffortNames          []string `json:"-"`
	SupportedByAdapter   []string `json:"supported_by_adapter"`
	UnsupportedByAdapter []string `json:"unsupported_by_adapter"`
}

// qoderPromotionFacts is the promotion block published alongside one chat
// model. price_factor is kept as the authoritative current rate; this block is
// explanatory metadata (badge, original factor, schedule and link). The Qoder
// gateway is not consistent about whether price_factor already includes the
// promotion (qmodel_38max does, qmodel may not), so the plugin never multiplies
// it a second time.
type qoderPromotionFacts struct {
	Active                     bool                   `json:"active"`
	BadgeZh                    string                 `json:"badge_zh,omitempty"`
	BadgeEn                    string                 `json:"badge_en,omitempty"`
	DescriptionZh              string                 `json:"description_zh,omitempty"`
	DescriptionEn              string                 `json:"description_en,omitempty"`
	RuleID                     string                 `json:"rule_id,omitempty"`
	DiscountFactor             *float64               `json:"discount_factor,omitempty"`
	BeforePromotionPriceFactor *float64               `json:"before_promotion_price_factor,omitempty"`
	Timezone                   string                 `json:"timezone,omitempty"`
	WindowStart                string                 `json:"window_start,omitempty"`
	WindowEnd                  string                 `json:"window_end,omitempty"`
	LinkZh                     string                 `json:"link_zh,omitempty"`
	LinkEn                     string                 `json:"link_en,omitempty"`
	Windows                    []qoderPromotionWindow `json:"windows,omitempty"`
}

type qoderPromotionWindow struct {
	StartMin    int `json:"startMin"`
	DurationMin int `json:"durationMin"`
}

// qoderPromotionWire mirrors the nested objects returned by model/list.
type qoderPromotionWire struct {
	Active bool `json:"active"`
	Badge  struct {
		En string `json:"en"`
		Zh string `json:"zh"`
	} `json:"badge"`
	Description struct {
		En string `json:"en"`
		Zh string `json:"zh"`
	} `json:"description"`
	RuleID                     string   `json:"rule_id"`
	DiscountFactor             *float64 `json:"discount_factor"`
	BeforePromotionPriceFactor *float64 `json:"before_promotion_price_factor"`
	Timezone                   string   `json:"timezone"`
	WindowStart                string   `json:"window_start"`
	WindowEnd                  string   `json:"window_end"`
	LinkURL                    struct {
		En string `json:"en"`
		Zh string `json:"zh"`
	} `json:"link_url"`
}

// modelRegionFacts is everything the panel shows for one model in one region
// beyond what pluginapi.ModelInfo already carries.
type modelRegionFacts struct {
	PriceFactor         *float64             `json:"price_factor,omitempty"`
	OriginalPriceFactor *float64             `json:"original_price_factor,omitempty"`
	IsFree              bool                 `json:"is_free,omitempty"`
	Promotion           *qoderPromotionFacts `json:"promotion,omitempty"`
	ContextLength       *int64               `json:"context_length,omitempty"`
	ContextStatus       string               `json:"context_status,omitempty"`
	ContextOverride     bool                 `json:"context_override,omitempty"`
	ContextRequested    *int64               `json:"context_requested,omitempty"`
	MaxInputTokens      *int64               `json:"max_input_tokens,omitempty"`
	ContextTiers        []contextTier        `json:"context_tiers,omitempty"`
	Thinking            *thinkingFacts       `json:"thinking,omitempty"`
}

// wire types mirroring the upstream JSON. Field presence is what carries the
// meaning in thinking_config, so the optional members are pointers or maps
// rather than plain values.
type qoderThinkingConfigWire struct {
	Disabled *qoderThinkingOptionWire `json:"disabled"`
	Enabled  *qoderThinkingOptionWire `json:"enabled"`
}

type qoderThinkingOptionWire struct {
	Description string                             `json:"description"`
	IsDefault   bool                               `json:"is_default"`
	Efforts     map[string]qoderThinkingEffortWire `json:"efforts"`
}

type qoderThinkingEffortWire struct {
	Description string `json:"description"`
	IsDefault   bool   `json:"is_default"`
}

type qoderContextTierWire struct {
	TokenCount int64 `json:"token_count"`
	IsDefault  bool  `json:"is_default"`
}

func normalizeQoderPromotion(w *qoderPromotionWire, now time.Time) *qoderPromotionFacts {
	if w == nil {
		return nil
	}
	p := &qoderPromotionFacts{
		Active:                     w.Active,
		BadgeZh:                    strings.TrimSpace(w.Badge.Zh),
		BadgeEn:                    strings.TrimSpace(w.Badge.En),
		DescriptionZh:              strings.TrimSpace(w.Description.Zh),
		DescriptionEn:              strings.TrimSpace(w.Description.En),
		RuleID:                     strings.TrimSpace(w.RuleID),
		DiscountFactor:             cloneFloat64Ptr(w.DiscountFactor),
		BeforePromotionPriceFactor: cloneFloat64Ptr(w.BeforePromotionPriceFactor),
		Timezone:                   strings.TrimSpace(w.Timezone),
		WindowStart:                strings.TrimSpace(w.WindowStart),
		WindowEnd:                  strings.TrimSpace(w.WindowEnd),
		LinkZh:                     strings.TrimSpace(w.LinkURL.Zh),
		LinkEn:                     strings.TrimSpace(w.LinkURL.En),
	}
	if p.Timezone == "" {
		p.Timezone = "UTC"
	}
	if p.WindowStart != "" && p.WindowEnd != "" {
		if window, ok := normalizeQoderWindow(p.WindowStart, p.WindowEnd, p.Timezone, now); ok {
			p.Windows = []qoderPromotionWindow{window}
		}
	}
	return p
}

func normalizeQoderWindow(start, end, zoneName string, now time.Time) (qoderPromotionWindow, bool) {
	startLocal, okStart := parseQoderClockMinute(start)
	endLocal, okEnd := parseQoderClockMinute(end)
	if !okStart || !okEnd {
		return qoderPromotionWindow{}, false
	}
	duration := ((endLocal-startLocal)%1440 + 1440) % 1440
	if duration == 0 {
		duration = 1440
	}
	loc, err := time.LoadLocation(zoneName)
	if err != nil {
		loc = time.UTC
	}
	_, offsetSeconds := now.In(loc).Zone()
	startUTC := ((startLocal-offsetSeconds/60)%1440 + 1440) % 1440
	return qoderPromotionWindow{StartMin: startUTC, DurationMin: duration}, true
}

func parseQoderClockMinute(value string) (int, bool) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) < 1 || len(parts) > 2 {
		return 0, false
	}
	hour, err := strconv.Atoi(parts[0])
	if err != nil || hour < 0 || hour > 24 {
		return 0, false
	}
	minute := 0
	if len(parts) == 2 {
		minute, err = strconv.Atoi(parts[1])
		if err != nil || minute < 0 || minute > 59 {
			return 0, false
		}
	}
	if hour == 24 && minute != 0 {
		return 0, false
	}
	return hour*60 + minute, true
}

// qoderPromotionActiveAt applies only the upstream rule's recurring window.
// price_factor itself remains authoritative; this helper is for panel badges and
// the /promotions response's current-state hint only.
func qoderPromotionActiveAt(p *qoderPromotionFacts, now time.Time) bool {
	if p == nil || !p.Active {
		return false
	}
	if len(p.Windows) == 0 {
		return true
	}
	minute := now.UTC().Hour()*60 + now.UTC().Minute()
	for _, w := range p.Windows {
		if w.DurationMin >= 1440 {
			return true
		}
		delta := (minute - w.StartMin + 1440) % 1440
		if delta < w.DurationMin {
			return true
		}
	}
	return false
}

// parseThinkingFacts converts the upstream thinking_config into the panel's
// vocabulary. It never invents a capability: an absent config, or one whose
// enabled block names no efforts, reports unknown rather than unsupported,
// because "the gateway did not say" and "this model cannot think" are
// different claims and only the second should ever tell an operator to stop
// looking for a tier.
func parseThinkingFacts(cfg *qoderThinkingConfigWire, supportsReasoning bool) *thinkingFacts {
	if cfg == nil {
		// No thinking_config at all. is_reasoning=false is the gateway's
		// explicit statement that the model does not reason; is_reasoning=true
		// with no tier data only means the tiers were not published.
		if !supportsReasoning {
			return &thinkingFacts{Status: "unsupported"}
		}
		return &thinkingFacts{Status: "unknown"}
	}

	facts := &thinkingFacts{}
	// Only a payload that actually describes the switch may claim it: a
	// disabled block means thinking can be turned off, and a described enabled
	// block means the switch exists at all. An empty config leaves this nil,
	// because "not described" is not "cannot be disabled".
	if cfg.Disabled != nil {
		canDisable := true
		facts.CanDisable = &canDisable
	} else if cfg.Enabled != nil {
		canDisable := false
		facts.CanDisable = &canDisable
	}

	// Upstream has only ever been observed putting efforts under enabled. A
	// config with no enabled block therefore carries no tiers, whatever else it
	// holds; reading a root-level efforts map would be guessing at a shape that
	// has not been seen.
	efforts := map[string]qoderThinkingEffortWire{}
	if cfg.Enabled != nil {
		for name, effort := range cfg.Enabled.Efforts {
			efforts[name] = effort
		}
	}

	levels := make([]string, 0, len(efforts))
	for name := range efforts {
		if name = strings.TrimSpace(name); name != "" {
			levels = append(levels, name)
		}
	}
	sortEffortLevels(levels)

	switch {
	case len(levels) > 0:
		facts.Status = "supported"
		facts.Levels = levels
		for _, level := range levels {
			if normalizeReasoningEffort(level) != "" {
				facts.SupportedByAdapter = append(facts.SupportedByAdapter, level)
			} else {
				facts.UnsupportedByAdapter = append(facts.UnsupportedByAdapter, level)
			}
		}
		// Only an explicitly marked effort is a default; enabled.is_default
		// describes the switch and cannot select a reasoning intensity.
		for _, name := range levels {
			if !efforts[name].IsDefault {
				continue
			}
			if facts.Default != "" {
				facts.Default = ""
				break
			}
			facts.Default = name
		}

	case cfg.Enabled != nil:
		// A pure on/off switch: the gateway offers thinking but names no tiers.
		// Reporting this as supported with no levels would misrepresent it as a
		// tiered model, so it gets its own status.
		facts.Status = "off_only"
	default:
		facts.Status = "unknown"
	}
	if cfg.Disabled != nil && cfg.Disabled.IsDefault {
		if cfg.Enabled != nil && cfg.Enabled.IsDefault {
			facts.Default = ""
		} else {
			facts.Default = "disabled"
		}
	}
	return facts
}

// sortEffortLevels orders tiers by intensity, then alphabetically for any name
// the gateway adds that this plugin does not know.
func sortEffortLevels(levels []string) {
	sort.SliceStable(levels, func(i, j int) bool {
		left, leftKnown := effortRank[levels[i]]
		right, rightKnown := effortRank[levels[j]]
		switch {
		case leftKnown && rightKnown:
			return left < right
		case leftKnown:
			return true
		case rightKnown:
			return false
		default:
			return levels[i] < levels[j]
		}
	})
}

// parseContextTiers converts context_config into an ordered tier list. The
// default tier is what the panel bolds, so a payload that marks none leaves
// every tier unmarked rather than promoting an arbitrary one.
func parseContextTiers(raw map[string]qoderContextTierWire) []contextTier {
	if len(raw) == 0 {
		return nil
	}
	tiers := make([]contextTier, 0, len(raw))
	for label, entry := range raw {
		label = strings.TrimSpace(label)
		if label == "" || entry.TokenCount <= 0 || entry.TokenCount > maxContextWindowValue {
			continue
		}
		tiers = append(tiers, contextTier{
			Label:     label,
			Tokens:    entry.TokenCount,
			IsDefault: entry.IsDefault,
		})
	}
	if len(tiers) == 0 {
		return nil
	}
	// Order by size so the panel reads small-to-large; label order would put
	// "1M" before "200K" and look arbitrary.
	sort.SliceStable(tiers, func(i, j int) bool {
		if tiers[i].Tokens != tiers[j].Tokens {
			return tiers[i].Tokens < tiers[j].Tokens
		}
		return tiers[i].Label < tiers[j].Label
	})
	return tiers
}

// contextDefaultTokens reports the token count of the tier marked default, used
// to resolve the context length the gateway would apply on its own.
func contextDefaultTokens(tiers []contextTier) (int64, bool) {
	var value int64
	for _, tier := range tiers {
		if !tier.IsDefault || tier.Tokens <= 0 || tier.Tokens > maxContextWindowValue {
			continue
		}
		if value != 0 && value != tier.Tokens {
			return 0, false
		}
		value = tier.Tokens
	}
	return value, value > 0
}

// cloneThinkingFacts deep-copies so a caller mutating a response cannot corrupt
// the cached catalog another request reads.
func cloneThinkingFacts(facts *thinkingFacts) *thinkingFacts {
	if facts == nil {
		return nil
	}
	copied := *facts
	copied.Levels = append([]string(nil), facts.Levels...)
	copied.EffortNames = append([]string(nil), facts.EffortNames...)
	copied.SupportedByAdapter = []string{}
	copied.UnsupportedByAdapter = []string{}
	// Adapter policy is local, not an upstream/disk capability. Recompute for
	// legacy caches and never trust stale adapter claims persisted by a build.
	for _, level := range copied.Levels {
		if normalizeReasoningEffort(level) != "" {
			copied.SupportedByAdapter = append(copied.SupportedByAdapter, level)
		} else {
			copied.UnsupportedByAdapter = append(copied.UnsupportedByAdapter, level)
		}
	}
	if facts.CanDisable != nil {
		value := *facts.CanDisable
		copied.CanDisable = &value
	}
	return &copied
}

func cloneContextTiers(tiers []contextTier) []contextTier {
	if len(tiers) == 0 {
		return nil
	}
	return append([]contextTier(nil), tiers...)
}

func cloneFloat64Ptr(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func cloneInt64Ptr(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func cloneQoderPromotion(p *qoderPromotionFacts) *qoderPromotionFacts {
	if p == nil {
		return nil
	}
	copy := *p
	copy.DiscountFactor = cloneFloat64Ptr(p.DiscountFactor)
	copy.BeforePromotionPriceFactor = cloneFloat64Ptr(p.BeforePromotionPriceFactor)
	copy.Windows = append([]qoderPromotionWindow(nil), p.Windows...)
	return &copy
}

func cloneRegionFacts(facts modelRegionFacts) modelRegionFacts {
	return modelRegionFacts{
		PriceFactor:         cloneFloat64Ptr(facts.PriceFactor),
		OriginalPriceFactor: cloneFloat64Ptr(facts.OriginalPriceFactor),
		IsFree:              facts.IsFree,
		Promotion:           cloneQoderPromotion(facts.Promotion),
		ContextLength:       cloneInt64Ptr(facts.ContextLength),
		ContextStatus:       facts.ContextStatus,
		ContextOverride:     facts.ContextOverride,
		ContextRequested:    cloneInt64Ptr(facts.ContextRequested),
		MaxInputTokens:      cloneInt64Ptr(facts.MaxInputTokens),
		ContextTiers:        cloneContextTiers(facts.ContextTiers),
		Thinking:            cloneThinkingFacts(facts.Thinking),
	}
}
