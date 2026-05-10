package knowledge

import (
	"context"
	"fmt"
	"strings"

	"github.com/larryhou/llm-go/tool"
)

// fetchTool implements tool.Tool and exposes knowledge_fetch to the LLM.
// The LLM calls this after knowledge_search when it needs the full content
// of a specific item identified by its ref_id.
type fetchTool struct{ mgr *Manager }

func (t *fetchTool) Name() string { return "knowledge_fetch" }

func (t *fetchTool) Description() string {
	return "Fetch the full content of a knowledge item by its ref_id. " +
		"Use ref_id values returned by knowledge_search. " +
		"Returns the complete document body, which may be large."
}

func (t *fetchTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"ref_id": map[string]any{
				"type":        "string",
				"description": "The ref_id of the item to fetch, as returned by knowledge_search.",
			},
			"type": map[string]any{
				"type":        "string",
				"enum":        []string{"fetch", "query"},
				"description": "\"fetch\" for a single document by ID/URL (default); \"query\" for a structured lookup.",
			},
		},
		"required": []string{"ref_id"},
	}
}

func (t *fetchTool) Execute(ctx context.Context, input map[string]any) (tool.Result, error) {
	refID, _ := input["ref_id"].(string)
	if strings.TrimSpace(refID) == "" {
		return tool.Result{}, tool.Fail("knowledge_fetch: \"ref_id\" must be a non-empty string")
	}

	qtype := CallTypeFetch
	if v, _ := input["type"].(string); v == "query" {
		qtype = CallTypeQuery
	}

	q := Query{
		Type:  qtype,
		Input: refID,
	}

	results, err := t.mgr.fetch(ctx, q)
	if err != nil {
		return tool.Result{}, tool.Fail(fmt.Sprintf("knowledge_fetch failed: %v", err))
	}
	if len(results) == 0 {
		return tool.Result{}, tool.Fail(fmt.Sprintf("knowledge_fetch: no content found for ref_id %q", refID))
	}

	r := results[0]
	return tool.Result{
		Output: formatFetchResult(r),
		Title:  fmt.Sprintf("knowledge_fetch: %s", r.Title),
		Metadata: map[string]any{
			"ref_id": refID,
			"source": r.Source,
			"score":  r.Score,
		},
	}, nil
}

// formatFetchResult renders the full content of a single result for the LLM.
func formatFetchResult(r Result) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s\n", r.Title)
	fmt.Fprintf(&sb, "**source**: %s  \n", r.Source)
	fmt.Fprintf(&sb, "**ref_id**: `%s`\n", r.RefID)
	if len(r.Metadata) > 0 {
		sb.WriteString("**metadata**:")
		for k, v := range r.Metadata {
			fmt.Fprintf(&sb, " %s=%v", k, v)
		}
		sb.WriteByte('\n')
	}
	sb.WriteString("\n---\n\n")
	sb.WriteString(r.Content)
	return sb.String()
}
