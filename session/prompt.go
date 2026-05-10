package session

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/larryhou/llm-go/config"
	"github.com/larryhou/llm-go/llm"
	"github.com/larryhou/llm-go/store"
	"github.com/larryhou/llm-go/tool"
)

// RunResult is the final outcome of a RunLoop call.
type RunResult string

const (
	RunResultStop     RunResult = "stop"
	RunResultContinue RunResult = "continue"
)

// RunInput is the input for a new user turn.
type RunInput struct {
	SessionID string
	UserMsg   string
	Model     llm.Model
	Tools     []tool.Tool
	Provider  llm.Provider
	Config    *config.Info

	// SummaryProvider, when set, is used for compaction summary generation
	// instead of Provider. Useful when Provider has middleware (e.g. usage
	// injection) that should not apply to the compaction LLM call.
	SummaryProvider llm.Provider

	// System prompt control — aligned with opencode session/llm.ts assembly order:
	//
	//   1. AgentPrompt  (if non-empty, replaces the per-provider prompt entirely)
	//   2. per-provider prompt  (injected only when AgentPrompt == "" and DisableProviderPrompt == false)
	//   3. ExtraSystem  (always appended after the base prompt)
	//
	// This mirrors opencode's logic:
	//   input.agent.prompt ? [input.agent.prompt] : SystemPrompt.provider(input.model)

	// AgentPrompt overrides the embedded per-provider system prompt.
	// When non-empty the provider prompt is NOT injected (same as opencode's agent.prompt behaviour).
	AgentPrompt string

	// DisableProviderPrompt suppresses the embedded per-provider prompt even when
	// AgentPrompt is empty. Use this when you want to supply a fully custom system
	// via ExtraSystem without any built-in preamble.
	DisableProviderPrompt bool

	// ExtraSystem provides additional system instructions appended after the base prompt.
	// Corresponds to input.system in opencode's llm.ts.
	ExtraSystem []string

	// MaxSteps prevents infinite agentic loops (0 = unlimited).
	MaxSteps int
}

// RunLoop is the main agentic loop for a session.
// It handles the complete lifecycle: user message → LLM call → tool execution
// → context compaction → next turn, until the LLM stops or an error occurs.
// Aligned with packages/opencode/src/session/prompt.ts runLoop().
func RunLoop(ctx context.Context, s store.Store, input RunInput) (RunResult, error) {
	// Create the user message
	userMsgID := newID()
	now := time.Now()
	userMsg := &store.Message{
		ID:        userMsgID,
		SessionID: input.SessionID,
		Role:      store.RoleUser,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.CreateMessage(ctx, userMsg); err != nil {
		return RunResultStop, fmt.Errorf("runloop: create user message: %w", err)
	}
	// Store user text as a text part
	textPartID := newID()
	if err := s.CreatePart(ctx, &store.Part{
		ID:        textPartID,
		MessageID: userMsgID,
		SessionID: input.SessionID,
		Type:      store.PartTypeText,
		Data: &store.TextPartData{
			Text:      input.UserMsg,
			TimeStart: now.UnixMilli(),
			TimeEnd:   now.UnixMilli(),
		},
	}); err != nil {
		return RunResultStop, fmt.Errorf("runloop: create user text part: %w", err)
	}

	processor := NewProcessor(s)
	compactor := NewCompactor(s, processor)

	step := 0
	for {
		if input.MaxSteps > 0 && step >= input.MaxSteps {
			return RunResultStop, nil
		}
		step++

		// Load all messages for context
		msgs, allParts, err := loadMessages(ctx, s, input.SessionID)
		if err != nil {
			return RunResultStop, err
		}

		// Apply compaction filter
		msgs = FilterCompacted(msgs, allParts)

		// Build model messages
		modelMsgs, err := ToModelMessages(msgs, allParts)
		if err != nil {
			return RunResultStop, fmt.Errorf("runloop: build messages: %w", err)
		}

		// Build system prompt
		system := buildSystem(input)

		// Create assistant message placeholder
		assistantMsgID := newID()
		assistantMsg := &store.Message{
			ID:        assistantMsgID,
			SessionID: input.SessionID,
			Role:      store.RoleAssistant,
			Model:     input.Model.ProviderID + "/" + input.Model.ID,
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		}
		if err := s.CreateMessage(ctx, assistantMsg); err != nil {
			return RunResultStop, fmt.Errorf("runloop: create assistant message: %w", err)
		}

		// Run one LLM turn
		result, err := processor.Process(ctx, assistantMsgID, ProcessInput{
			SessionID: input.SessionID,
			Model:     input.Model,
			System:    system,
			Messages:  modelMsgs,
			Tools:     input.Tools,
			Provider:  input.Provider,
			Config:    input.Config,
		})
		if err != nil {
			return RunResultStop, err
		}

		switch result {
		case ProcessStop:
			return RunResultStop, nil

		case ProcessCompact:
			// Run compaction, then continue the loop
			_, err := compactor.Compact(ctx, input.SessionID, ProcessInput{
				SessionID:       input.SessionID,
				Model:           input.Model,
				Provider:        input.Provider,
				SummaryProvider: input.SummaryProvider,
				Config:          input.Config,
			})
			if err != nil {
				return RunResultStop, fmt.Errorf("runloop: compaction failed: %w", err)
			}
			continue

		case ProcessContinue:
			// Check if the last assistant message finished with tool calls
			// If so, continue the loop to let the LLM process tool results
			lastMsg, lastParts, err := loadLastAssistantMessage(ctx, s, input.SessionID)
			if err != nil || lastMsg == nil {
				return RunResultStop, nil
			}
			if !hasToolCalls(lastParts) {
				return RunResultContinue, nil
			}
			// Continue loop to let LLM see tool results
			continue
		}
	}
}

// buildSystem constructs the system prompt array for a request.
// Aligned with session/llm.ts system prompt assembly (lines 103-128):
//
//	system = [
//	  agentPrompt || providerPrompt,  // base — exactly one of these
//	  ...input.system,                // ExtraSystem
//	].filter(Boolean).join("\n")
//
// Priority:
//  1. AgentPrompt — if set, used as the sole base; provider prompt is skipped
//  2. Provider prompt — used when AgentPrompt is empty AND DisableProviderPrompt is false
//  3. ExtraSystem — always appended after the base
func buildSystem(input RunInput) []string {
	var parts []string

	switch {
	case input.AgentPrompt != "":
		// Agent has a custom prompt → use it, skip provider prompt entirely
		parts = append(parts, input.AgentPrompt)
	case !input.DisableProviderPrompt:
		// No agent prompt → inject the embedded per-provider prompt
		if base := SystemPromptForModel(input.Model); base != "" {
			parts = append(parts, base)
		}
	// else: DisableProviderPrompt=true, AgentPrompt="" → no base prompt at all
	}

	// Always append caller-supplied extra instructions
	parts = append(parts, input.ExtraSystem...)

	// Filter empty strings
	var out []string
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// hasToolCalls returns true if any part in the message is a tool call part
// (regardless of status — even pending means the LLM emitted a tool call).
func hasToolCalls(ps []*store.Part) bool {
	for _, p := range ps {
		if p.Type == store.PartTypeTool {
			return true
		}
	}
	return false
}

// loadMessages loads all messages and their parts for a session.
func loadMessages(ctx context.Context, s store.Store, sessionID string) ([]*store.Message, map[string][]*store.Part, error) {
	msgs, err := s.ListMessages(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	allParts := make(map[string][]*store.Part, len(msgs))
	for _, m := range msgs {
		ps, err := s.ListParts(ctx, m.ID)
		if err != nil {
			return nil, nil, err
		}
		allParts[m.ID] = ps
	}
	return msgs, allParts, nil
}

// loadLastAssistantMessage returns the most recent assistant message and its parts.
func loadLastAssistantMessage(ctx context.Context, s store.Store, sessionID string) (*store.Message, []*store.Part, error) {
	msgs, allParts, err := loadMessages(ctx, s, sessionID)
	if err != nil {
		return nil, nil, err
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == store.RoleAssistant {
			return msgs[i], allParts[msgs[i].ID], nil
		}
	}
	return nil, nil, nil
}
