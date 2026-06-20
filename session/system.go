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

//go:embed max-steps.txt
var PromptMaxSteps string

//go:embed knowledge-recall.txt
var PromptKnowledgeRecall string

// PromptPostCompact is injected as a synthetic user message after a predictive
// (Path A) compaction so the loop can continue without waiting for a real user
// message. It instructs the LLM to resume the current task transparently.
const PromptPostCompact = "Context was compacted. Please continue with the current task."

// PromptContextTooLarge is injected as a synthetic user message after a
// reactive (Path B) compaction when the request was rejected because the
// context was too large. It tells the LLM the likely cause (oversized tool
// result) so it can adjust its approach rather than retrying blindly.
const PromptContextTooLarge = "The previous request was rejected because the context was too large. " +
	"This is most likely caused by a tool result that was too large. " +
	"Please adjust your approach: use more targeted parameters, request smaller results, or break the task into smaller steps."

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
	case strings.Contains(id, "gpt-4") ||
		id == "o1" || strings.HasPrefix(id, "o1-") ||
		id == "o3" || strings.HasPrefix(id, "o3-"):
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
