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
	return "根据 knowledge_search 返回的 ref_id 获取知识条目的完整内容。" +
		"ref_id 格式为 source_id:内部编号（如 sop:42、memory_larryhou:7）。" +
		"当 knowledge_search 的摘要不足以回答问题时调用此工具。"
}

func (t *fetchTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"ref_id": map[string]any{
				"type":        "string",
				"description": "The ref_id of the item to fetch, as returned by knowledge_search.",
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

	results, err := t.mgr.fetch(ctx, Query{Type: QueryTypeFetch, Input: refID})
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
