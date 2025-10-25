package runtime

import "math"

// ContextBudget tracks the conversational budget for the runtime. The
// compactor triggers once the estimated usage crosses the configured
// percentage of the available tokens.
type ContextBudget struct {
	MaxTokens          int
	CompactWhenPercent float64
}

// normalizedPercent returns a 0-1 value even when the caller supplied a whole
// percentage (e.g. 85 for 85%).
func (b ContextBudget) normalizedPercent() float64 {
	percent := b.CompactWhenPercent
	if percent > 1 {
		percent = percent / 100
	}
	if percent < 0 {
		percent = 0
	}
	if percent > 1 {
		percent = 1
	}
	return percent
}

// triggerTokens computes the token usage that should trigger compaction.
func (b ContextBudget) triggerTokens() int {
	if b.MaxTokens <= 0 {
		return 0
	}
	percent := b.normalizedPercent()
	if percent <= 0 {
		return 0
	}
	threshold := int(math.Ceil(percent * float64(b.MaxTokens)))
	if threshold < 1 {
		threshold = 1
	}
	if threshold > b.MaxTokens {
		threshold = b.MaxTokens
	}
	return threshold
}

var defaultModelContextBudgets = map[string]ContextBudget{
	"gpt-4.1":      {MaxTokens: 128000, CompactWhenPercent: 0.85},
	"gpt-4.1-mini": {MaxTokens: 64000, CompactWhenPercent: 0.85},
	"gpt-4.1-nano": {MaxTokens: 32000, CompactWhenPercent: 0.85},
	"gpt-4o":       {MaxTokens: 128000, CompactWhenPercent: 0.85},
	"gpt-4o-mini":  {MaxTokens: 64000, CompactWhenPercent: 0.85},
	"o1":           {MaxTokens: 128000, CompactWhenPercent: 0.8},
	"o1-preview":   {MaxTokens: 128000, CompactWhenPercent: 0.8},
	"o1-mini":      {MaxTokens: 64000, CompactWhenPercent: 0.8},
}
