package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/larryhou/llm-go/tool"
)

// ResetTool is a built-in tool that lets the LLM reset a session on the
// user's request. Three modes are supported via the "mode" parameter:
//
//   - soft (default): roll back to the most recent compaction save-point.
//     Head messages become visible again. Falls back to hard if no boundary.
//
//   - soft-refresh: same rollback, but re-hides head with a new empty boundary.
//     LLM gets a clean context; history remains searchable via knowledge_search.
//     Falls back to hard if no boundary.
//
//   - hard: completely wipe all conversation history.
//
// softFn(ctx, fresh) handles both soft and soft-refresh by passing fresh=false
// or fresh=true. hardResetFn handles the hard mode and the fallback path.
type ResetTool struct {
	softFn      func(ctx context.Context, fresh bool) error
	hardResetFn func(ctx context.Context) error
}

// NewResetTool constructs a ResetTool.
//
//   - softFn(ctx, fresh): calls SoftReset + RollbackTo under session lock.
//     fresh=false → normal rollback; fresh=true → rollback + new empty boundary.
//   - hardResetFn: deletes and recreates the session (full wipe).
func NewResetTool(softFn func(ctx context.Context, fresh bool) error, hardResetFn func(ctx context.Context) error) *ResetTool {
	return &ResetTool{softFn: softFn, hardResetFn: hardResetFn}
}

func (t *ResetTool) Name() string { return "session_reset" }

func (t *ResetTool) Description() string {
	return `Reset the current session history.

Three modes are available via the "mode" parameter:

- "soft" (default): roll back to the most recent compaction save-point. All
  messages that were summarised during the last compaction become visible again.
  Use this to restore context after a compaction discarded too much history.
  If no compaction save-point exists the tool falls back to a hard reset.

- "soft-refresh": roll back to the most recent compaction save-point but start
  with a clean context. The head messages are hidden from the LLM but remain
  searchable via knowledge_search. Use this to start a new task without
  carrying old context into the active window.
  If no compaction save-point exists the tool falls back to a hard reset.

- "hard": completely forget all conversation history, tool results, and
  compacted summaries. The session ID is preserved.

IMPORTANT: Before calling this tool you MUST explicitly warn the user about
what will be deleted and obtain their clear confirmation.`
}

func (t *ResetTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mode": map[string]any{
				"type":    "string",
				"enum":    []string{"soft", "soft-refresh", "hard"},
				"default": "soft",
				"description": `"soft" rolls back to the last compaction save-point (default); ` +
					`"soft-refresh" rolls back but hides head for a clean context; ` +
					`"hard" wipes all history.`,
			},
		},
	}
}

func (t *ResetTool) Execute(ctx context.Context, input map[string]any) (tool.Result, error) {
	mode, _ := input["mode"].(string)
	if mode == "" {
		mode = "soft"
	}

	// softOrFallback runs softFn and — on ErrNoCompactionBoundary — falls back to hard.
	softOrFallback := func(fresh bool, successMsg string) (tool.Result, error) {
		err := t.softFn(ctx, fresh)
		if err == nil {
			return tool.Result{Output: successMsg, Title: "session_reset"}, nil
		}
		if errors.Is(err, ErrNoCompactionBoundary) {
			if hardErr := t.hardResetFn(ctx); hardErr != nil {
				return tool.Result{}, fmt.Errorf("session_reset %s (fallback hard): %w", mode, hardErr)
			}
			return tool.Result{
				Output: "没有找到压缩存档点，已执行完全重置，所有历史记录已清空。",
				Title:  "session_reset",
			}, nil
		}
		return tool.Result{}, fmt.Errorf("session_reset %s: %w", mode, err)
	}

	switch mode {
	case "soft":
		return softOrFallback(false, "已回退到最近一次压缩存档点，之前的对话历史重新可见。")
	case "soft-refresh":
		return softOrFallback(true, "已回退到最近一次压缩存档点，历史记录保留但上下文已清空，可通过 knowledge_search 检索历史。")
	case "hard":
		if err := t.hardResetFn(ctx); err != nil {
			return tool.Result{}, fmt.Errorf("session_reset hard: %w", err)
		}
		return tool.Result{Output: "会话已完全重置，所有历史记录已清空。", Title: "session_reset"}, nil
	default:
		return tool.Result{}, tool.Fail(fmt.Sprintf("unknown mode %q — use \"soft\", \"soft-refresh\", or \"hard\"", mode))
	}
}
