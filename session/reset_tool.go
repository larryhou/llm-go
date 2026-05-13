package session

import (
	"context"
	"fmt"

	"github.com/larryhou/llm-go/tool"
)

// ResetTool is a built-in tool that lets the LLM completely reset a session
// on the user's request. It delegates the actual reset work to a resetFn
// callback provided at construction time, allowing the caller to hold any
// necessary locks atomically across the delete + recreate + index-reset
// sequence.
//
// Wire it into RunInput.Tools alongside the knowledge tools:
//
//	session.RunLoop(ctx, st, session.RunInput{
//	    Tools: append(km.Tools(), session.NewResetTool(resetFn)),
//	    ...
//	})
type ResetTool struct {
	resetFn func(ctx context.Context) error
}

// NewResetTool constructs a ResetTool that calls resetFn on execution.
// resetFn is responsible for deleting all session data, recreating the
// session record, and resetting the history index — all atomically under
// whatever lock the caller holds.
func NewResetTool(resetFn func(ctx context.Context) error) *ResetTool {
	return &ResetTool{resetFn: resetFn}
}

func (t *ResetTool) Name() string { return "session_reset" }

func (t *ResetTool) Description() string {
	return `Reset the current session: completely forget all conversation history, tool results, and compacted summaries. The session ID is preserved so you can continue in the same session after the reset.

IMPORTANT: Before calling this tool you MUST explicitly warn the user that all history will be permanently deleted and cannot be recovered, and obtain their clear confirmation. Do not call this tool unless the user has confirmed.`
}

func (t *ResetTool) InputSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func (t *ResetTool) Execute(ctx context.Context, _ map[string]any) (tool.Result, error) {
	if err := t.resetFn(ctx); err != nil {
		return tool.Result{}, fmt.Errorf("session_reset: %w", err)
	}
	return tool.Result{
		Output: "会话已完全重置，所有历史记录已清空。",
		Title:  "session_reset",
	}, nil
}
