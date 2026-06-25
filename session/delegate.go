package session

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/larryhou/llm-go/config"
	"github.com/larryhou/llm-go/llm"
	"github.com/larryhou/llm-go/store"
	"github.com/larryhou/llm-go/tool"
)

// DelegateConfig controls the behaviour of DelegateTool's sub-sessions.
type DelegateConfig struct {
	// MaxSteps is the maximum number of LLM agentic steps the sub-session may
	// take. 0 means unlimited (not recommended — use a reasonable cap such as
	// 100 to prevent runaway sub-sessions).
	MaxSteps int

	// AgentPrompt, when non-empty, replaces the per-provider system prompt for
	// the sub-session. Leave empty to inherit the same prompt as the parent.
	AgentPrompt string

	// ExtraSystem provides additional system instructions for the sub-session,
	// appended after the base prompt.
	ExtraSystem []string
}

// DelegateTool implements tool.Tool. When the main LLM calls "delegate_task",
// this tool spins up a fresh sub-session that runs its own full agentic loop.
// The sub-session's tool list is provided by the caller and must not include
// delegate_task itself — that is how recursion is prevented (the sub-agent
// simply has no way to delegate further). See cmd/llm-api/main.go for the
// canonical wiring.
//
// Once the sub-session completes, an LLM call distils its entire execution log
// into a concise summary that is returned as the tool result. The sub-session
// is then deleted (it is ephemeral).
//
// This keeps the parent session context free from tool-call noise: the parent
// LLM only sees the final conclusion, not the intermediate steps.
//
// Construction: use NewDelegateTool. The sseProviderFactory parameter lets the
// caller supply a wrapped provider (e.g. forwarding SSE events tagged with the
// sub_session_id) for the sub-session's main provider, while a plain inner
// provider is used for the summary LLM call to avoid SSE side-effects.
type DelegateTool struct {
	parentSessionID    string
	s                  store.Store
	tools              []tool.Tool // sub-session tool set; must NOT include delegate_task
	sseProviderFactory func(subSessionID string) llm.Provider
	summaryProvider    llm.Provider
	model              llm.Model
	config             *config.Info
	delegateConfig     DelegateConfig
}

// NewDelegateTool creates a DelegateTool.
//
//   - parentSessionID: the ID of the parent session (stored in Session.ParentID
//     of the sub-session for traceability).
//   - s: the shared store (sub-sessions use a separate SessionID but the same store).
//   - tools: the tool set the sub-session will use. Callers must NOT include
//     delegate_task itself — recursion is prevented by simply not making it
//     available to the sub-agent.
//   - sseProviderFactory: called with the new sub-session ID to produce the
//     llm.Provider the sub-session uses. Typically wraps innerProv so that
//     SSE events are forwarded to the client tagged with "sub_session_id".
//   - summaryProvider: plain (unwrapped) provider for the post-execution
//     summary LLM call. Pass innerProv (no SSE middleware) so internal
//     summary events are not leaked to the SSE client.
//   - model / cfg: forwarded to the sub-session RunLoop.
//   - dc: sub-session behavioural config.
func NewDelegateTool(
	parentSessionID string,
	s store.Store,
	tools []tool.Tool,
	sseProviderFactory func(subSessionID string) llm.Provider,
	summaryProvider llm.Provider,
	model llm.Model,
	cfg *config.Info,
	dc DelegateConfig,
) *DelegateTool {
	return &DelegateTool{
		parentSessionID:    parentSessionID,
		s:                  s,
		tools:              tools,
		sseProviderFactory: sseProviderFactory,
		summaryProvider:    summaryProvider,
		model:              model,
		config:             cfg,
		delegateConfig:     dc,
	}
}

// Name returns the tool name as seen by the LLM.
func (d *DelegateTool) Name() string { return "delegate_task" }

// Description explains to the LLM when and how to use this tool.
func (d *DelegateTool) Description() string {
	return `Delegate a self-contained task to a dedicated sub-agent running in its own isolated session.

STRONGLY RECOMMENDED whenever a task is:
- Independent — it does not need back-and-forth with the current conversation to proceed
- Uncertain in scope — you are not sure how many steps, files, or tool calls it will take

When in doubt, delegate. It is always safer to delegate a task that turns out to be
simple than to attempt a complex task inline and pollute the current context.

Do NOT delegate when:
- The task requires information that only exists in the current conversation context
- The task is a single, obviously bounded action (one tool call)

The sub-agent has access to all the same tools (except delegate_task itself).
It runs autonomously and returns a concise summary — you will NOT see its intermediate steps.

Provide a specific goal and include all context the sub-agent needs to work independently.`
}

// InputSchema defines the JSON schema for the tool's input parameters.
func (d *DelegateTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"goal": map[string]any{
				"type":        "string",
				"description": "A clear, specific description of the task the sub-agent should accomplish. Include all necessary context.",
			},
			"context": map[string]any{
				"type":        "string",
				"description": "(Optional) Additional background information from the current conversation that the sub-agent needs to perform the task.",
			},
		},
		"required": []string{"goal"},
	}
}

// Execute runs the sub-session and returns a summary of its output.
// It implements tool.Tool.Execute.
func (d *DelegateTool) Execute(ctx context.Context, input map[string]any) (tool.Result, error) {
	goal, _ := input["goal"].(string)
	if strings.TrimSpace(goal) == "" {
		return tool.Result{}, tool.Fail("goal is required and must be non-empty")
	}

	// Combine goal and optional context into the sub-session's user message.
	userMsg := goal
	if extra, _ := input["context"].(string); strings.TrimSpace(extra) != "" {
		userMsg = goal + "\n\nAdditional context:\n" + strings.TrimSpace(extra)
	}

	// Create the sub-session record in the store.
	subSessionID := newID()
	now := time.Now()
	if err := d.s.CreateSession(ctx, &store.Session{
		ID:        subSessionID,
		ParentID:  d.parentSessionID,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		return tool.Result{}, fmt.Errorf("delegate_task: create sub-session: %w", err)
	}

	log.Printf("[delegate_task] parent=%s sub=%s goal=%q",
		d.parentSessionID, subSessionID, truncateGoal(goal, 80))

	// Build the sub-session provider. The factory wraps innerProv so that
	// streaming events are forwarded to the SSE client tagged with sub_session_id.
	subProvider := d.sseProviderFactory(subSessionID)

	// Build system prompt for the sub-session.
	extraSystem := d.delegateConfig.ExtraSystem

	h := RunLoopAsync(ctx, d.s, RunInput{
		SessionID:             subSessionID,
		UserMsg:               userMsg,
		Model:                 d.model,
		Provider:              subProvider,
		SummaryProvider:       d.summaryProvider,
		Tools:                 d.tools,
		Config:                d.config,
		MaxSteps:              d.delegateConfig.MaxSteps,
		AgentPrompt:           d.delegateConfig.AgentPrompt,
		ExtraSystem:           extraSystem,
		DisableProviderPrompt: d.delegateConfig.AgentPrompt != "",
		// Sub-sessions can run many tool calls. OmitConsumedTools keeps context
		// lean by replacing already-consumed tool results with a placeholder,
		// allowing longer task sequences without hitting the context limit.
		OmitConsumedTools: true,
	})

	// Wait for the sub-session to complete. We intentionally do NOT use
	// context.WithoutCancel here: if the parent session is cancelled (user
	// pressed Ctrl-C), we want the sub-session to be cancelled too.
	<-h.Done

	// Regardless of success/failure, clean up the ephemeral sub-session.
	// Use context.Background() so the delete is not affected by a cancelled ctx.
	defer func() {
		if delErr := d.s.DeleteSession(context.Background(), subSessionID); delErr != nil {
			log.Printf("[delegate_task] sub=%s delete failed (non-fatal): %v", subSessionID, delErr)
		}
	}()

	if h.Err != nil {
		// Sub-session failed with a hard error. Try to salvage a partial result.
		if fallback := extractLastAssistantText(context.Background(), subSessionID, d.s); fallback != "" {
			log.Printf("[delegate_task] sub=%s failed (%v), using last assistant text as fallback", subSessionID, h.Err)
			return tool.Result{
				Output: fallback,
				Title:  "Task (partial): " + truncateGoal(goal, 60),
			}, nil
		}
		return tool.Result{}, fmt.Errorf("delegate_task: sub-session failed: %w", h.Err)
	}

	// Generate a concise summary of the sub-session's execution log.
	// Use context.Background() so a cancelled parent ctx does not abort the
	// summary call (the sub-session has already completed successfully).
	summary, err := generateDelegateSummary(
		context.Background(),
		subSessionID,
		goal,
		d.s,
		d.summaryProvider,
		d.model,
	)
	if err != nil {
		// Summary generation failed. Fall back to the sub-session's last
		// assistant text so the parent LLM gets something useful.
		log.Printf("[delegate_task] sub=%s summary failed (%v), falling back to last text", subSessionID, err)
		if fallback := extractLastAssistantText(context.Background(), subSessionID, d.s); fallback != "" {
			return tool.Result{
				Output: fallback,
				Title:  "Task: " + truncateGoal(goal, 60),
			}, nil
		}
		return tool.Result{}, fmt.Errorf("delegate_task: summary failed and no fallback text: %w", err)
	}

	log.Printf("[delegate_task] sub=%s complete, summary=%d chars", subSessionID, len(summary))
	return tool.Result{
		Output: summary,
		Title:  "Task: " + truncateGoal(goal, 60),
	}, nil
}

// truncateGoal returns the first n runes of s, appending "…" if truncated.
func truncateGoal(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
