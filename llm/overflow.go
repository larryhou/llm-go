package llm

import (
	"math"

	"github.com/larryhou/llm-go/config"
)

// Overflow constants, aligned with packages/opencode/src/session/overflow.ts
// and packages/opencode/src/session/compaction.ts.
const (
	CompactionBuffer            = config.DefaultCompactionBuffer // 20_000 tokens reserved
	PruneMinimum                = 20_000
	PruneProtect                = 40_000
	ToolOutputMaxCharsCompaction = 2_000
	MinPreserveRecentTokens     = config.DefaultCompactionMinPreserveTokens // 2_000
	MaxPreserveRecentTokens     = config.DefaultCompactionMaxPreserveTokens // 8_000
)

// Model describes a provider model's limits and capabilities.
// Subset of provider.Model used by overflow calculations.
type Model struct {
	ID         string
	ProviderID string
	APIID      string // model ID sent to the provider API
	Limit      ModelLimit
	Cost       ModelCost
	Options    map[string]any
}

// ModelLimit holds context window constraints.
// Aligned with Provider.Model.limit in provider.ts.
type ModelLimit struct {
	Context int  // total context window tokens
	Input   *int // if set, max input tokens (separate from output)
	Output  int  // max output tokens
}

// ModelCost holds per-token pricing.
type ModelCost struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
}

// MaxOutputTokens returns the effective max output token count for this model.
// Mirrors ProviderTransform.maxOutputTokens: min(model.limit.output, 32000).
func MaxOutputTokens(m Model) int {
	const cap = 32_000
	if m.Limit.Output < cap {
		return m.Limit.Output
	}
	return cap
}

// Usable computes the number of input tokens available before compaction triggers.
// Aligned with packages/opencode/src/session/overflow.ts usable().
//
//	reserved = cfg.compaction.reserved ?? min(20_000, maxOutputTokens(model))
//	if model.limit.input: return max(0, model.limit.input - reserved)
//	else:                  return max(0, model.limit.context - maxOutputTokens(model))
func Usable(m Model, cfg *config.Info) int {
	if m.Limit.Context == 0 {
		return 0
	}

	reserved := CompactionBuffer
	if cfg != nil && cfg.Compaction != nil && cfg.Compaction.Reserved != nil {
		reserved = *cfg.Compaction.Reserved
	} else {
		if maxOut := MaxOutputTokens(m); maxOut < reserved {
			reserved = maxOut
		}
	}

	if m.Limit.Input != nil {
		v := *m.Limit.Input - reserved
		return max(0, v)
	}
	v := m.Limit.Context - MaxOutputTokens(m)
	return max(0, v)
}

// IsOverflow returns true if the token usage has reached the usable limit.
// Aligned with packages/opencode/src/session/overflow.ts isOverflow().
func IsOverflow(usage TokenUsage, m Model, cfg *config.Info) bool {
	if cfg != nil && cfg.Compaction != nil && cfg.Compaction.Auto != nil && !*cfg.Compaction.Auto {
		return false
	}
	if m.Limit.Context == 0 {
		return false
	}
	count := usage.Effective()
	return count >= Usable(m, cfg)
}

// PreserveRecentBudget returns the token budget for preserving recent turns verbatim.
// Aligned with packages/opencode/src/session/compaction.ts preserveRecentBudget().
//
//	cfg.preserve_recent_tokens ?? min(8000, max(2000, floor(usable * 0.25)))
func PreserveRecentBudget(m Model, cfg *config.Info) int {
	if cfg != nil && cfg.Compaction != nil && cfg.Compaction.PreserveRecentTokens != nil {
		return *cfg.Compaction.PreserveRecentTokens
	}
	usable := Usable(m, cfg)
	budget := int(math.Floor(float64(usable) * 0.25))
	budget = max(budget, MinPreserveRecentTokens)
	budget = min(budget, MaxPreserveRecentTokens)
	return budget
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
