package knowledge

import (
	"context"
	"fmt"
	"strings"

	"github.com/larryhou/llm-go/tool"
)

// searchTool implements tool.Tool and exposes knowledge_search to the LLM.
// The LLM calls this when it needs to explore a topic or run a structured
// query across knowledge sources.  Results are compact snippets; the LLM
// decides whether to follow up with knowledge_fetch for full content.
type searchTool struct{ mgr *Manager }

func (t *searchTool) Name() string { return "knowledge_search" }

func (t *searchTool) Description() string {
	return "Search for information across configured knowledge sources. " +
		"Returns a ranked list of titles and short snippets. " +
		"Use knowledge_fetch with a result's ref_id to retrieve the full content of a specific item."
}

func (t *searchTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "The search terms or query expression.",
			},
			"type": map[string]any{
				"type":        "string",
				"enum":        []string{"search", "query"},
				"description": "\"search\" for broad exploratory queries (default); \"query\" for structured field-filter queries.",
			},
			"max_results": map[string]any{
				"type":        "integer",
				"description": "Maximum number of results to return (default: 5).",
				"minimum":     1,
				"maximum":     20,
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

	qtype := CallTypeSearch
	if v, _ := input["type"].(string); v == "query" {
		qtype = CallTypeQuery
	}

	maxResults := 5
	if v, ok := input["max_results"].(float64); ok && int(v) > 0 {
		maxResults = int(v)
	}

	var filters map[string]any
	if f, ok := input["filters"].(map[string]any); ok {
		filters = f
	}

	q := Query{
		Type:       qtype,
		Input:      queryStr,
		Filters:    filters,
		MaxResults: maxResults,
	}

	results, err := t.mgr.peek(ctx, q)
	if err != nil {
		return tool.Result{}, tool.Fail(fmt.Sprintf("knowledge_search failed: %v", err))
	}

	return tool.Result{
		Output: formatSearchResults(results),
		Title:  fmt.Sprintf("knowledge_search: %d result(s) for %q", len(results), queryStr),
		Metadata: map[string]any{
			"query":   queryStr,
			"count":   len(results),
		"type":    string(qtype),
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
