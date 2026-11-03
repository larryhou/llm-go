package knowledge

import (
	"context"
	"fmt"
	"strings"

	"github.com/larryhou/llm-go/tool"
)

// searchTool implements tool.Tool and exposes knowledge_search to the LLM.
// The LLM calls this when it needs to explore a topic across knowledge sources.
// Results are compact snippets; the LLM decides whether to follow up with
// knowledge_fetch for full content.
type searchTool struct{ mgr *Manager }

func (t *searchTool) Name() string { return "knowledge_search" }

func (t *searchTool) Description() string {
	return "检索知识库，涵盖个人记忆（memory）、个人待办（todo）、团队SOP经验（sop）以及压缩后的会话历史（history）等来源。" +
		"返回摘要列表，每条结果包含 ref_id、来源（source）和内容摘要。" +
		"需要完整内容时，用结果中的 ref_id 调用 knowledge_fetch。" +
		"**必须主动调用的场景**：回答涉及用户个人背景的问题前；用户询问待办清单时；回答技术/工具/流程类问题前。"
}

func (t *searchTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "The search terms or query expression.",
			},
			"max_results": map[string]any{
				"type":        "integer",
				"description": "Maximum number of results to return (default: 5).",
				"minimum":     1,
				"maximum":     20,
			},
			"cut_score": map[string]any{
				"type":        "number",
				"description": fmt.Sprintf("Minimum relevance score threshold (0–1). Results below this score are discarded. Default: %.2f. Pass a negative value to disable filtering.", DefaultCutScore),
				"minimum":     -1,
				"maximum":     1,
			},
			"hit_score": map[string]any{
				"type":        "number",
				"description": fmt.Sprintf("'Good enough' score threshold (0–1). When at least one result reaches this score and max_results is satisfied, lower-priority sources are skipped. Default: %.2f. Pass a negative value to disable early stopping.", DefaultHitScore),
				"minimum":     -1,
				"maximum":     1,
			},
			"filters": map[string]any{
				"type":                 "object",
				"description":          "Optional key-value filters passed to the source (field names depend on the backend).",
				"additionalProperties": true,
			},
		},
		"required": []string{"query"},
	}
}

func (t *searchTool) Execute(ctx context.Context, input map[string]any) (tool.Result, error) {
	queryStr, _ := input["query"].(string)
	if strings.TrimSpace(queryStr) == "" {
		return tool.Result{}, tool.Fail("knowledge_search: \"query\" must be a non-empty string")
	}

	maxResults := 5
	const maxResultsCap = 20
	if v, ok := input["max_results"].(float64); ok && int(v) > 0 {
		maxResults = int(v)
		if maxResults > maxResultsCap {
			maxResults = maxResultsCap
		}
	}

	// cut_score: 0 → default; negative → disable
	cutScore := 0.0
	if v, ok := input["cut_score"].(float64); ok {
		cutScore = v
	}

	// hit_score: 0 → default; negative → disable
	hitScore := 0.0
	if v, ok := input["hit_score"].(float64); ok {
		hitScore = v
	}

	var filters map[string]any
	if f, ok := input["filters"].(map[string]any); ok {
		filters = f
	}

	q := Query{
		Type:       QueryTypeSearch,
		Input:      queryStr,
		Filters:    filters,
		MaxResults: maxResults,
		CutScore:   cutScore,
		HitScore:   hitScore,
	}

	results, err := t.mgr.peek(ctx, q)
	if err != nil {
		return tool.Result{}, tool.Fail(fmt.Sprintf("knowledge_search failed: %v", err))
	}

	return tool.Result{
		Output: formatSearchResults(results),
		Title:  fmt.Sprintf("knowledge_search: %d result(s) for %q", len(results), queryStr),
		Metadata: map[string]any{
			"query": queryStr,
			"count": len(results),
		},
	}, nil
}

// formatSearchResults renders results as a Markdown list for the LLM.
// Each entry shows the title, ref_id (for follow-up fetch), source, score,
// and snippet.  The format is intentionally compact to minimise context growth.
func formatSearchResults(results []Result) string {
	if len(results) == 0 {
		return "No results found."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "## Search Results (%d)\n\n", len(results))
	for i, r := range results {
		fmt.Fprintf(&sb, "### %d. %s\n", i+1, r.Title)
		fmt.Fprintf(&sb, "- **ref_id**: `%s`\n", r.RefID)
		fmt.Fprintf(&sb, "- **source**: %s\n", r.Source)
		if r.Score >= 0 {
			fmt.Fprintf(&sb, "- **score**: %.2f\n", r.Score)
		}
		if r.Snippet != "" {
			fmt.Fprintf(&sb, "\n> %s\n", strings.ReplaceAll(r.Snippet, "\n", "\n> "))
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}
