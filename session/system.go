// Package session provides per-provider system prompt selection.
// Aligned with packages/opencode/src/session/system.ts.
package session

import (
	_ "embed"
	"strings"

	"github.com/larryhou/llm-go/llm"
)

//go:embed anthropic.txt
var promptAnthropic string

//go:embed gpt.txt
var promptGPT string

//go:embed beast.txt
var promptBeast string

//go:embed gemini.txt
var promptGemini string

//go:embed kimi.txt
var promptKimi string

//go:embed codex.txt
var promptCodex string

//go:embed trinity.txt
var promptTrinity string

//go:embed default.txt
var promptDefault string

// SystemPromptForModel returns the appropriate system prompt for a given model.
// Aligned with packages/opencode/src/session/system.ts provider().
//
// Matching logic (by model.APIID):
//   - gpt-4 / o1 / o3          → beast prompt
//   - gpt + codex               → codex prompt
//   - gpt (other)               → gpt prompt
//   - gemini-                   → gemini prompt
//   - claude                    → anthropic prompt
//   - trinity (case-insensitive)→ trinity prompt
//   - kimi (case-insensitive)   → kimi prompt
//   - everything else           → default prompt
func SystemPromptForModel(model llm.Model) string {
	id := model.APIID
	lower := strings.ToLower(id)

	switch {
	case strings.Contains(id, "gpt-4") || strings.Contains(id, "o1") || strings.Contains(id, "o3"):
		return promptBeast
	case strings.Contains(id, "gpt") && strings.Contains(lower, "codex"):
		return promptCodex
	case strings.Contains(id, "gpt"):
		return promptGPT
	case strings.Contains(id, "gemini-"):
		return promptGemini
	case strings.Contains(id, "claude"):
		return promptAnthropic
	case strings.Contains(lower, "trinity"):
		return promptTrinity
	case strings.Contains(lower, "kimi"):
		return promptKimi
	default:
		return promptDefault
	}
}
