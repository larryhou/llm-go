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
	// AgentID optionally identifies which agent is running (affects system prompt)
	AgentID string
	// ExtraSystem provides additional system instructions
	ExtraSystem []string
	// MaxSteps prevents infinite agentic loops (default 0 = unlimited)
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
				SessionID: input.SessionID,
				Model:     input.Model,
				Provider:  input.Provider,
				Config:    input.Config,
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
// Aligned with session/llm.ts system prompt assembly.
func buildSystem(input RunInput) []string {
	var parts []string

	// Provider-specific base prompt (from embedded files)
	base := SystemPromptForModel(input.Model)
	if base != "" {
		parts = append(parts, base)
	}

	// Extra system instructions from caller
	parts = append(parts, input.ExtraSystem...)

	// Join non-empty parts
	var out []string
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// hasToolCalls returns true if any part in the message is a pending/running tool call.
func hasToolCalls(ps []*store.Part) bool {
	for _, p := range ps {
		if p.Type == store.PartTypeTool {
			if d, ok := p.Data.(*store.ToolPartData); ok {
				if d.Status == store.ToolStatusCompleted || d.Status == store.ToolStatusRunning {
					return true
				}
			}
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
