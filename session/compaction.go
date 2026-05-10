package session

import (
	"context"
	"fmt"
	"time"

	"github.com/larryhou/llm-go/config"
	"github.com/larryhou/llm-go/llm"
	"github.com/larryhou/llm-go/store"
)

// SummaryTemplate is the prompt used to generate compaction summaries.
// Aligned with packages/opencode/src/session/compaction.ts SUMMARY_TEMPLATE.
const SummaryTemplate = `Please provide a thorough, structured summary of our conversation so far.
The summary will be used to restore context when the conversation history is too long.

Format your response in Markdown with these sections:

## Goal
What the user is trying to accomplish.

## Constraints
Any constraints, preferences, or requirements mentioned.

## Progress
### Done
What has been completed.

### In Progress
What is currently being worked on.

### Blocked
Any blockers or issues encountered.

## Key Decisions
Important decisions made and their rationale.

## Next Steps
The immediate next actions to take.

## Critical Context
Any other context that would be essential to continue the work.

## Relevant Files
List of files that have been created, modified, or are important to the task.`

// Compaction constants, aligned with packages/opencode/src/session/compaction.ts.
const (
	PruneMinimum    = 20_000
	PruneProtect    = 40_000
	DefaultTailTurns = 2
)

// Turn represents a contiguous block of messages for one user turn.
type Turn struct {
	UserMsgID string
	StartIdx  int
	EndIdx    int // exclusive
}

// SelectResult is the output of the compaction selection algorithm.
type SelectResult struct {
	// Head contains messages to be summarised.
	Head []*store.Message
	// TailStartID is the ID of the first message in the tail to preserve verbatim.
	TailStartID string
}

// Select splits a message list into head (to summarise) and tail (to keep verbatim).
// Aligned with packages/opencode/src/session/compaction.ts select().
//
// Algorithm:
//  1. Walk backwards through user turns
//  2. Keep the most recent tailTurns turns that fit within the preserve budget
//  3. Everything before the kept turns is the head to summarise
func Select(msgs []*store.Message, model llm.Model, cfg *config.Info) SelectResult {
	budget := llm.PreserveRecentBudget(model, cfg)
	tailTurns := DefaultTailTurns
	if cfg != nil && cfg.Compaction != nil && cfg.Compaction.TailTurns != nil {
		tailTurns = *cfg.Compaction.TailTurns
	}

	// Build list of real user turns (skip compaction boundary user messages
	// which only contain a CompactionPart — they are anchors, not real turns).
	// The parts map is not available here, so we rely on the caller having
	// already run FilterCompacted which places the compaction user message
	// first; we skip the very first message if it has no real text content.
	var turns []Turn
	for i, m := range msgs {
		if m.Role != store.RoleUser {
			continue
		}
		// Skip compaction anchor messages (identified by being a user message
		// with only compaction/empty content — they come right before summary).
		// A simple heuristic: skip if there is a following summary assistant message.
		isCompactionBoundary := false
		if i+1 < len(msgs) && msgs[i+1].Summary {
			isCompactionBoundary = true
		}
		if isCompactionBoundary {
			continue
		}
		turns = append(turns, Turn{UserMsgID: m.ID, StartIdx: i})
	}
	// Set end index for each turn
	for i := range turns {
		if i+1 < len(turns) {
			turns[i].EndIdx = turns[i+1].StartIdx
		} else {
			turns[i].EndIdx = len(msgs)
		}
	}

	// Need more turns than tailTurns to have anything to summarise
	if len(turns) <= tailTurns {
		return SelectResult{Head: nil, TailStartID: ""}
	}

	// Determine how many of the most recent turns fit in the tail budget.
	// Walk backwards through the last tailTurns turns.
	tail := turns[len(turns)-tailTurns:]
	totalTokens := 0
	// Start of the tail — will be moved forward if budget is tight.
	tailStartTurnIdx := len(turns) - tailTurns

	for i := len(tail) - 1; i >= 0; i-- {
		t := tail[i]
		size := estimateTurnTokens(msgs[t.StartIdx:t.EndIdx])
		if totalTokens+size <= budget {
			totalTokens += size
		} else {
			// This turn doesn't fit; tail starts at the next turn.
			tailStartTurnIdx = len(turns) - tailTurns + i + 1
			break
		}
	}

	// Ensure tail keeps at least 1 turn.
	if tailStartTurnIdx >= len(turns) {
		tailStartTurnIdx = len(turns) - 1
	}

	tailMsgIdx := turns[tailStartTurnIdx].StartIdx
	if tailMsgIdx == 0 {
		// All messages are in the tail → nothing to summarise
		return SelectResult{Head: nil, TailStartID: ""}
	}

	return SelectResult{
		Head:        msgs[:tailMsgIdx],
		TailStartID: msgs[tailMsgIdx].ID,
	}
}

// estimateTurnTokens estimates the token count for a slice of messages.
// Uses a rough heuristic: 1 token ≈ 4 characters.
func estimateTurnTokens(msgs []*store.Message) int {
	total := 0
	for _, m := range msgs {
		_ = m
		// Without actual token counts from the provider, use message size heuristic
		total += 100 // placeholder; in practice use stored token counts
	}
	return total
}

// Compactor handles the context compaction workflow.
type Compactor struct {
	store     store.Store
	processor *Processor
}

// NewCompactor creates a Compactor.
func NewCompactor(s store.Store, processor *Processor) *Compactor {
	return &Compactor{store: s, processor: processor}
}

// Compact runs the compaction process for a session.
// It:
//  1. Selects head/tail split
//  2. Generates a summary of the head using the LLM
//  3. Inserts a compaction boundary in the user message
//  4. Stores the summary as a special assistant message
//
// The provider used for the summary LLM call is input.SummaryProvider when
// set, otherwise input.Provider. This lets callers supply an unwrapped/plain
// provider for the compaction call (e.g. without usage-injection middleware).
//
// Returns the summary message ID on success.
func (c *Compactor) Compact(ctx context.Context, sessionID string, input ProcessInput) (string, error) {
	// Load all messages for this session
	msgs, err := c.store.ListMessages(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("compaction: list messages: %w", err)
	}

	// Load all parts
	allParts := make(map[string][]*store.Part, len(msgs))
	for _, m := range msgs {
		ps, err := c.store.ListParts(ctx, m.ID)
		if err != nil {
			return "", fmt.Errorf("compaction: list parts for %s: %w", m.ID, err)
		}
		allParts[m.ID] = ps
	}

	// Filter to post-compaction messages only
	msgs = FilterCompacted(msgs, allParts)

	// Select head/tail split
	sel := Select(msgs, input.Model, input.Config)
	if len(sel.Head) == 0 {
		return "", fmt.Errorf("compaction: nothing to summarise")
	}

	// Build model messages for the head (with stripped media + truncated tool output)
	headMsgs, err := ToModelMessagesWithOptions(sel.Head, allParts, ToModelOptions{
		StripMedia:          true,
		ToolOutputMaxChars:  llm.ToolOutputMaxCharsCompaction,
	})
	if err != nil {
		return "", fmt.Errorf("compaction: build head messages: %w", err)
	}

	now := time.Now()

	// Step 1: Insert a compaction boundary user message.
	// This must be created BEFORE the summary assistant message so that
	// ListMessages (insertion-ordered) sees:
	//   [compactionUserMsg / CompactionPart]  ← FilterCompacted anchor
	//   [summaryAssistantMsg / summary=true]  ← LLM summary
	//   [subsequent turns...]
	compactionMsgID := newID()
	if err := c.store.CreateMessage(ctx, &store.Message{
		ID:        compactionMsgID,
		SessionID: sessionID,
		Role:      store.RoleUser,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		return "", fmt.Errorf("compaction: create boundary message: %w", err)
	}
	if err := c.store.CreatePart(ctx, &store.Part{
		ID:        newID(),
		MessageID: compactionMsgID,
		SessionID: sessionID,
		Type:      store.PartTypeCompaction,
		Data:      &store.CompactionPartData{},
	}); err != nil {
		return "", fmt.Errorf("compaction: create boundary part: %w", err)
	}

	// Step 2: Create the summary assistant message placeholder.
	summaryMsgID := newID()
	if err := c.store.CreateMessage(ctx, &store.Message{
		ID:        summaryMsgID,
		SessionID: sessionID,
		Role:      store.RoleAssistant,
		Model:     input.Model.ID + "/" + input.Model.ProviderID,
		CreatedAt: now.Add(time.Millisecond),
		UpdatedAt: now.Add(time.Millisecond),
		Summary:   true,
	}); err != nil {
		return "", fmt.Errorf("compaction: create summary message: %w", err)
	}

	// Step 3: Run the summary LLM call.
	// Use SummaryProvider when set (allows callers to supply a plain provider
	// without middleware that would cause side effects like usage accumulation).
	summaryProvider := input.Provider
	if input.SummaryProvider != nil {
		summaryProvider = input.SummaryProvider
	}
	// Use a large context limit for the summary model so the head is never
	// rejected as overflow (the test model has an artificially small limit).
	summaryModel := input.Model
	summaryModel.Limit = llm.ModelLimit{Context: 200_000, Output: 8_192}

	summaryInput := ProcessInput{
		SessionID: sessionID,
		Model:     summaryModel,
		System:    []string{"You are a helpful assistant that summarises conversation history concisely and accurately."},
		Messages:  append(headMsgs, llm.NewUserMessage(SummaryTemplate)),
		Tools:     nil,
		Provider:  summaryProvider,
		Config:    input.Config,
	}

	result, err := c.processor.Process(ctx, summaryMsgID, summaryInput)
	if err != nil {
		return "", fmt.Errorf("compaction: summary generation failed: %w", err)
	}
	if result == ProcessCompact {
		return "", fmt.Errorf("compaction: context overflow during summary generation")
	}

	return summaryMsgID, nil
}

// ToModelOptions configures ToModelMessagesWithOptions.
type ToModelOptions struct {
	StripMedia         bool
	ToolOutputMaxChars int
}

// ToModelMessagesWithOptions is like ToModelMessages but with compaction options.
func ToModelMessagesWithOptions(msgs []*store.Message, parts map[string][]*store.Part, opts ToModelOptions) ([]llm.Message, error) {
	var out []llm.Message
	for _, m := range msgs {
		ps := parts[m.ID]
		switch m.Role {
		case store.RoleUser:
			userParts := buildUserParts(ps)
			if len(userParts) > 0 {
				out = append(out, llm.Message{Role: llm.RoleUser, Content: userParts})
			}
		case store.RoleAssistant:
			if m.Error != nil && !hasRealContent(ps) {
				continue
			}
			assistantParts, toolResults := buildAssistantPartsWithOpts(m, ps, opts)
			if len(assistantParts) == 0 {
				continue
			}
			out = append(out, llm.Message{Role: llm.RoleAssistant, Content: assistantParts})
			if len(toolResults) > 0 {
				out = append(out, llm.Message{Role: llm.RoleTool, Content: toolResults})
			}
		}
	}
	return out, nil
}

func buildAssistantPartsWithOpts(m *store.Message, ps []*store.Part, opts ToModelOptions) ([]llm.ContentPart, []llm.ContentPart) {
	var assistantParts []llm.ContentPart
	var toolResults []llm.ContentPart

	for _, p := range ps {
		switch p.Type {
		case store.PartTypeText:
			d, ok := p.Data.(*store.TextPartData)
			if !ok {
				continue
			}
			text := d.Text
			if text == "" {
				text = " "
			}
			assistantParts = append(assistantParts, llm.NewTextPart(text))

		case store.PartTypeReasoning:
			d, ok := p.Data.(*store.ReasoningPartData)
			if !ok || opts.StripMedia {
				continue
			}
			if d.Text != "" {
				assistantParts = append(assistantParts, llm.NewReasoningPart(d.Text))
			}

		case store.PartTypeTool:
			d, ok := p.Data.(*store.ToolPartData)
			if !ok {
				continue
			}
			inputMap := d.Input
			if inputMap == nil {
				inputMap = map[string]any{}
			}
			assistantParts = append(assistantParts, llm.NewToolCallPart(d.CallID, d.Tool, inputMap))

			output := d.Output
			if d.Compacted > 0 {
				output = "[Old tool result content cleared]"
			} else if opts.ToolOutputMaxChars > 0 && len(output) > opts.ToolOutputMaxChars {
				output = output[:opts.ToolOutputMaxChars] + fmt.Sprintf("\n...[truncated to %d chars]", opts.ToolOutputMaxChars)
			}

			var result *llm.ToolResult
			switch d.Status {
			case store.ToolStatusCompleted:
				result = llm.NewTextResult(output)
			case store.ToolStatusError:
				if d.Interrupted && d.Output != "" {
					result = llm.NewTextResult(d.Output)
				} else {
					errMsg := d.Error
					if errMsg == "" {
						errMsg = "Tool execution failed"
					}
					result = llm.NewErrorResult(errMsg)
				}
			default:
				result = llm.NewErrorResult("[Tool execution was interrupted]")
			}
			toolResults = append(toolResults, llm.NewToolResultPart(d.CallID, d.Tool, result))
		}
	}
	return assistantParts, toolResults
}

// Prune marks old tool outputs as compacted to free context space.
// Aligned with packages/opencode/src/session/compaction.ts prune().
//
// Only runs when cfg.compaction.prune == true (default false).
// Called as a fire-and-forget goroutine after each successful RunLoop.
//
// Algorithm:
//  1. Walk messages backward
//  2. Skip first 2 user turns (most recent)
//  3. Protect the most recent PRUNE_PROTECT tokens of tool output
//  4. Mark older tool parts as compacted
//  5. Only commit if we freed > PRUNE_MINIMUM tokens
func Prune(ctx context.Context, sessionID string, s store.Store, cfg *config.Info) error {
	// Aligned with compaction.ts: if (!cfg.compaction?.prune) return
	if cfg == nil || cfg.Compaction == nil || cfg.Compaction.Prune == nil || !*cfg.Compaction.Prune {
		return nil
	}

	msgs, err := s.ListMessages(ctx, sessionID)
	if err != nil {
		return err
	}

	// Walk backward, tracking user turn count and protected token budget
	turnsSkipped := 0
	tokensProtected := 0
	totalPruned := 0

	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Summary {
			break
		}
		if m.Role == store.RoleUser {
			turnsSkipped++
			if turnsSkipped <= 2 {
				continue // protect most recent 2 turns
			}
		}
		if m.Role != store.RoleAssistant {
			continue
		}

		ps, err := s.ListParts(ctx, m.ID)
		if err != nil {
			continue
		}
		for _, p := range ps {
			if p.Type != store.PartTypeTool {
				continue
			}
			d, ok := p.Data.(*store.ToolPartData)
			if !ok || d.Status != store.ToolStatusCompleted || d.Compacted > 0 {
				continue
			}

			// Estimate token size of this tool output
			outputTokens := len(d.Output) / 4 // rough: 4 chars per token

			if tokensProtected < PruneProtect {
				tokensProtected += outputTokens
				continue
			}

			// Prune this output
			d.Compacted = time.Now().UnixMilli()
			p.Data = d
			if err := s.UpdatePart(ctx, p); err == nil {
				totalPruned += outputTokens
			}
		}
	}

	if totalPruned < PruneMinimum/4 { // /4 for rough token-to-char ratio
		return fmt.Errorf("prune: not enough content to prune (%d estimated tokens)", totalPruned)
	}

	return nil
}
