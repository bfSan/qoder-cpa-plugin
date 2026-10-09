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
	"strings"
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
	Status      string   `json:"status"`
	Default     string   `json:"default,omitempty"`
	CanDisable  *bool    `json:"can_disable,omitempty"`
	Levels      []string `json:"levels,omitempty"`
	EffortNames []string `json:"-"`
}

// modelRegionFacts is everything the panel shows for one model in one region
// beyond what pluginapi.ModelInfo already carries.
type modelRegionFacts struct {
	PriceFactor   *float64       `json:"price_factor,omitempty"`
	ContextLength *int64         `json:"context_length,omitempty"`
	ContextTiers  []contextTier  `json:"context_tiers,omitempty"`
	Thinking      *thinkingFacts `json:"thinking,omitempty"`
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
	var source qoderThinkingOptionWire
	if cfg.Enabled != nil {
		source = *cfg.Enabled
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
		// The tier marked is_default wins; otherwise the model's enabled block
		// default, then the strongest named tier. Upstream omits the marker on
		// some models, and showing a tier with no indication of which one is
		// current is the state this field exists to fix.
		for _, name := range levels {
			if efforts[name].IsDefault {
				facts.Default = name
				break
			}
		}
		if facts.Default == "" && source.IsDefault {
			facts.Default = levels[len(levels)-1]
		}
	case cfg.Enabled != nil:
		// A pure on/off switch: the gateway offers thinking but names no tiers.
		// Reporting this as supported with no levels would misrepresent it as a
		// tiered model, so it gets its own status.
		facts.Status = "off_only"
	default:
		facts.Status = "unknown"
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
		if label == "" || entry.TokenCount <= 0 {
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
	for _, tier := range tiers {
		if tier.IsDefault {
			return tier.Tokens, true
		}
	}
	return 0, false
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

func cloneRegionFacts(facts modelRegionFacts) modelRegionFacts {
	return modelRegionFacts{
		PriceFactor:   cloneFloat64Ptr(facts.PriceFactor),
		ContextLength: cloneInt64Ptr(facts.ContextLength),
		ContextTiers:  cloneContextTiers(facts.ContextTiers),
		Thinking:      cloneThinkingFacts(facts.Thinking),
	}
}
