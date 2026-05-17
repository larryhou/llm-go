package session

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

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
// PruneMinimum and PruneProtect are defined in llm package; use llm.PruneMinimum / llm.PruneProtect.
const (
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
	// RecentHead contains the messages belonging to the last 2 real user turns
	// within Head (those immediately before the tail). These are the turns most
	// likely to be mis-summarised. Nil when Head has ≤ 2 real turns (in which
	// case there is nothing worth anchoring beyond what is already in the tail).
	RecentHead []*store.Message
}

// Select splits a message list into head (to summarise) and tail (to keep verbatim).
// Aligned with packages/opencode/src/session/compaction.ts select().
//
// Algorithm:
//  1. Walk backwards through user turns
//  2. Keep the most recent tailTurns turns that fit within the preserve budget
//  3. Everything before the kept turns is the head to summarise
//
// allParts is used to identify compaction boundary user messages (those that
// contain a PartTypeCompaction part) so they are skipped, consistent with
// FilterCompacted. This replaces the brittle i+1-is-summary positional heuristic.
func Select(msgs []*store.Message, allParts map[string][]*store.Part, model llm.Model, cfg *config.Info) SelectResult {
	budget := llm.PreserveRecentBudget(model, cfg)
	tailTurns := DefaultTailTurns
	if cfg != nil && cfg.Compaction != nil && cfg.Compaction.TailTurns != nil {
		tailTurns = *cfg.Compaction.TailTurns
	}

	// Build list of real user turns, skipping compaction boundary messages.
	// A boundary is a user message that contains a PartTypeCompaction part —
	// the same criterion used by FilterCompacted.
	var turns []Turn
	for i, m := range msgs {
		if m.Role != store.RoleUser {
			continue
		}
		if hasPartType(allParts[m.ID], store.PartTypeCompaction) {
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

	// Aligned with opencode compaction.ts select():
	// When there are not enough turns to split (≤ tailTurns), or when the budget
	// calculation places the tail start at index 0, summarise ALL messages as the
	// head with no tail preserved verbatim (tail_start_id = "").
	// This avoids "nothing to summarise" errors after repeated compaction rounds
	// where the visible history has already been trimmed to a small number of turns.
	if len(turns) <= tailTurns {
		// Still compute RecentHead (last ≤2 turns of the full head) so that Step 6
		// of Compact() can write a PartTypeRecentContext excerpt onto the boundary
		// message. This gives the LLM a verbatim anchor even when the entire history
		// is summarised.
		var recentHead []*store.Message
		if len(turns) >= 1 {
			startTurnIdx := len(turns) - 2
			if startTurnIdx < 0 {
				startTurnIdx = 0
			}
			recentHead = msgs[turns[startTurnIdx].StartIdx:]
		}
		return SelectResult{Head: msgs, TailStartID: "", RecentHead: recentHead}
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

	// Minimum tail content guard: if the selected tail is less than minKeepRatio
	// of the total session token weight, the verbatim context after compaction
	// would be too thin to aid context recovery. Keep pulling earlier turns into
	// the tail until the threshold is met, leaving at least 1 turn in the head
	// for the summary LLM to compress.
	// Large-turn guard: if a candidate turn exceeds 5% of total session tokens,
	// stop expanding — absorbing it would disproportionately shrink the head.
	minTailRatio := config.DefaultCompactionMinKeepRatio
	if cfg != nil && cfg.Compaction != nil && cfg.Compaction.MinKeepRatio != nil {
		minTailRatio = *cfg.Compaction.MinKeepRatio
	}
	totalSessionTokens := estimateTurnTokens(msgs)
	minTailTokens := int(float64(totalSessionTokens) * minTailRatio)
	maxCandidateTokens := int(float64(totalSessionTokens) * 0.05)
	for totalTokens < minTailTokens && tailStartTurnIdx > 1 {
		candidateIdx := tailStartTurnIdx - 1
		t := turns[candidateIdx]
		size := estimateTurnTokens(msgs[t.StartIdx:t.EndIdx])
		if size > maxCandidateTokens {
			break
		}
		totalTokens += size
		tailStartTurnIdx = candidateIdx
	}

	// Ensure tail keeps at least 1 turn.
	if tailStartTurnIdx >= len(turns) {
		tailStartTurnIdx = len(turns) - 1
	}

	tailMsgIdx := turns[tailStartTurnIdx].StartIdx
	if tailMsgIdx == 0 {
		// Budget covers all turns — summarise everything with no verbatim tail.
		// Aligned with opencode: keep.start == 0 → head = all messages, tail_start_id = undefined.
		// Still compute RecentHead for the last ≤2 turns so Step 6 writes the RC excerpt.
		var recentHead []*store.Message
		if len(turns) >= 1 {
			startTurnIdx := tailStartTurnIdx - 2
			if startTurnIdx < 0 {
				startTurnIdx = 0
			}
			recentHead = msgs[turns[startTurnIdx].StartIdx:]
		}
		return SelectResult{Head: msgs, TailStartID: "", RecentHead: recentHead}
	}

	// Compute RecentHead: the last 2 real turns within Head (those immediately
	// before the tail). Populated whenever the head is non-empty (tailStartTurnIdx >= 1)
	// so the excerpt is always written after any compaction. When head has only 1 turn
	// (tailStartTurnIdx == 1), recentStartIdx clamps to turns[0].StartIdx, capturing
	// the single head turn verbatim.
	var recentHead []*store.Message
	if tailStartTurnIdx >= 1 {
		recentTurnIdx := tailStartTurnIdx - 2
		if recentTurnIdx < 0 {
			recentTurnIdx = 0
		}
		recentStartIdx := turns[recentTurnIdx].StartIdx
		recentHead = msgs[recentStartIdx:tailMsgIdx]
	}

	return SelectResult{
		Head:        msgs[:tailMsgIdx],
		TailStartID: msgs[tailMsgIdx].ID,
		RecentHead:  recentHead,
	}
}

// estimateTurnTokens estimates the token count for a slice of messages.
// Uses stored token counts when available, falling back to a character heuristic.
func estimateTurnTokens(msgs []*store.Message) int {
	total := 0
	for _, m := range msgs {
		t := m.Tokens
		if n := t.Input + t.Output + t.CacheRead + t.CacheWrite; n > 0 {
			total += n
		} else {
			// Conservative fallback: 500 tokens for user messages (typically
			// 500-2000 tokens), 300 for others. The old value of 100 was a
			// severe underestimate that caused Select to retain more messages
			// in the tail than intended, making compaction ineffective.
			if m.Role == store.RoleUser {
				total += 500
			} else {
				total += 300
			}
		}
	}
	return total
}

// hasPartType returns true if any part in ps has the given type.
func hasPartType(ps []*store.Part, partType string) bool {
	for _, p := range ps {
		if p.Type == partType {
			return true
		}
	}
	return false
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
	// Detach from the caller's cancellation so that compaction (especially the
	// summary LLM call) completes even if the HTTP request context is cancelled
	// (e.g. the SSE client disconnects mid-stream while we are summarising).
	// context.WithoutCancel inherits values but ignores cancellation.
	ctx = context.WithoutCancel(ctx)

	// Load all messages for this session
	msgs, err := c.store.ListMessages(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("compaction: list messages: %w", err)
	}

	// Load all parts in a single query (avoids N+1 — one ListParts per message).
	allParts, err := c.store.ListPartsBySession(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("compaction: list parts: %w", err)
	}

	// Filter to post-compaction messages only
	msgs = FilterCompacted(msgs, allParts)

	// Select head/tail split — pass allParts so Select can identify compaction
	// boundaries via PartTypeCompaction (same logic as FilterCompacted).
	sel := Select(msgs, allParts, input.Model, input.Config)
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

	// Step 1: Insert a compaction boundary user message WITHOUT the CompactionPart.
	// The boundary must be in the store BEFORE the summary message so that
	// ListMessages (insertion-ordered) returns them in the correct order for
	// FilterCompacted. However, FilterCompacted identifies a boundary by the
	// presence of a PartTypeCompaction part — so until Step 4 writes that part,
	// this message is invisible to FilterCompacted. If the LLM call fails,
	// the boundary stays part-less and history is fully preserved.
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

	// Step 2: Create the summary assistant message placeholder.
	// Process() will write the LLM output parts to this message ID.
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

	// Step 4: LLM succeeded — now attach the CompactionPart to the boundary message.
	// Only after this write does FilterCompacted recognise the boundary as an anchor.
	// Any failure here is non-fatal: the summary already exists and the next
	// FilterCompacted call will simply not see it as a compaction pair.
	if err := c.store.CreatePart(ctx, &store.Part{
		ID:        newID(),
		MessageID: compactionMsgID,
		SessionID: sessionID,
		Type:      store.PartTypeCompaction,
		Data:      &store.CompactionPartData{TailStartID: sel.TailStartID},
	}); err != nil {
		return "", fmt.Errorf("compaction: create boundary part: %w", err)
	}

	// Step 5: Call the compaction hook (e.g. to index head messages for recall).
	// This is fire-and-forget from the compaction's perspective; hook errors are
	// not surfaced — a failed index write does not invalidate the compaction.
	if input.OnCompact != nil {
		input.OnCompact(sel.Head, allParts)
	}

	// Step 6: Write a PartTypeRecentContext part onto the boundary message.
	// This embeds a verbatim excerpt of the 2 turns immediately preceding the
	// tail so the LLM has a fine-grained anchor after compaction without needing
	// an extra tool call. Non-fatal: failure does not roll back the compaction.
	if len(sel.RecentHead) > 0 {
		excerpt := buildRecentContextExcerpt(sel.RecentHead, allParts)
		if excerpt != "" {
			_ = c.store.CreatePart(ctx, &store.Part{
				ID:        newID(),
				MessageID: compactionMsgID,
				SessionID: sessionID,
				Type:      store.PartTypeRecentContext,
				CreatedAt: now,
				UpdatedAt: now,
				Data:      &store.RecentContextPartData{Excerpt: excerpt},
			})
			// Update boundary message Tokens so the next compaction's
			// estimateTurnTokens does not under-count this message (which
			// would otherwise fall back to the 100-token placeholder).
			// Read-then-update to avoid clobbering other fields.
			// Rough estimate: 1 token ≈ 4 chars.
			if bm, readErr := c.store.GetMessage(ctx, compactionMsgID); readErr == nil {
				bm.Tokens.Input = len(excerpt) / 4
				_ = c.store.UpdateMessage(ctx, bm)
			}
		}
	}

	return summaryMsgID, nil
}

// buildRecentContextExcerpt renders a compact verbatim excerpt of msgs for
// embedding into the compaction boundary message. Each message is rendered as
// a labelled block with text truncated to keep the total size manageable.
// Tool outputs are truncated to 120 runes; message text to 300 runes.
func buildRecentContextExcerpt(msgs []*store.Message, allParts map[string][]*store.Part) string {
	const (
		maxTextRunes = 300
		maxToolRunes = 120
		ellipsis     = "..."
	)

	// truncateRunes returns s truncated to at most n runes, with ellipsis appended
	// when truncation occurs. Operates on rune boundaries to avoid invalid UTF-8.
	truncateRunes := func(s string, n int) string {
		if utf8.RuneCountInString(s) <= n {
			return s
		}
		runes := []rune(s)
		return string(runes[:n]) + ellipsis
	}

	var sb strings.Builder
	sb.WriteString("---\n以下是压缩前最近的对话原文：\n")

	hasContent := false // true once any real text or tool call is written

	for _, m := range msgs {
		ps := allParts[m.ID]

		switch m.Role {
		case store.RoleUser:
			// Skip compaction boundary messages — they carry "What did we do so far?"
			// and would be noise in the excerpt.
			if hasPartType(ps, store.PartTypeCompaction) {
				continue
			}
			wroteLabel := false
			for _, p := range ps {
				if p.Type != store.PartTypeText {
					continue
				}
				d, ok := store.DataAs[*store.TextPartData](p)
				if !ok || d.Text == "" {
					continue
				}
				if !wroteLabel {
					sb.WriteString("\n**[用户]**\n")
					wroteLabel = true
				}
				sb.WriteString(truncateRunes(d.Text, maxTextRunes))
				sb.WriteByte('\n')
				hasContent = true
			}

		case store.RoleAssistant:
			wroteLabel := false
			for _, p := range ps {
				switch p.Type {
				case store.PartTypeText:
					d, ok := store.DataAs[*store.TextPartData](p)
					if !ok || d.Text == "" {
						continue
					}
					if !wroteLabel {
						sb.WriteString("\n**[助手]**\n")
						wroteLabel = true
					}
					sb.WriteString(truncateRunes(d.Text, maxTextRunes))
					sb.WriteByte('\n')
					hasContent = true

				case store.PartTypeTool:
					d, ok := store.DataAs[*store.ToolPartData](p)
					if !ok {
						continue
					}
					if !wroteLabel {
						sb.WriteString("\n**[助手]**\n")
						wroteLabel = true
					}
					output := d.Output
					if d.Compacted > 0 {
						output = "[已清除]"
					} else {
						output = truncateRunes(output, maxToolRunes)
					}
					fmt.Fprintf(&sb, "- 调用工具: %s → %s\n", d.Tool, output)
					hasContent = true
				}
			}
		}
	}

	if !hasContent {
		return ""
	}
	return strings.TrimSpace(sb.String())
}

type ToModelOptions struct {
	StripMedia         bool
	ToolOutputMaxChars int
}

// ToModelMessagesWithOptions is like ToModelMessages but with compaction options.
// Used for building the head message list sent to the summary LLM during compaction.
// Applies the same interrupted/cancelled message handling as ToModelMessages so that
// a head containing partial assistant turns does not send malformed messages to the
// summary LLM (e.g. an assistant tool-call with no matching tool-result).
func ToModelMessagesWithOptions(msgs []*store.Message, parts map[string][]*store.Part, opts ToModelOptions) ([]llm.Message, error) {
	var out []llm.Message
	for _, m := range msgs {
		ps := parts[m.ID]
		switch m.Role {
		case store.RoleUser:
			userParts := buildUserParts(ps, opts)
			if len(userParts) == 0 {
				continue
			}
			// Same consecutive-user-message guard as ToModelMessages.
			if len(out) > 0 && out[len(out)-1].Role == llm.RoleUser {
				out = append(out, llm.Message{
					Role:    llm.RoleAssistant,
					Content: []llm.ContentPart{llm.NewTextPart(" ")},
				})
			}
			out = append(out, llm.Message{Role: llm.RoleUser, Content: userParts})

		case store.RoleAssistant:
			// Cancelled: no content; skip entirely (consecutive-user guard above handles gap).
			if m.Status == store.MessageStatusCancelled {
				continue
			}
			// Interrupted: keep only completed tool calls to avoid dangling tool-call/result pairs.
			if m.Status == store.MessageStatusInterrupted {
				assistantParts, toolResults := buildAssistantPartsInterrupted(ps)
				if len(assistantParts) == 0 {
					continue
				}
				out = append(out, llm.Message{Role: llm.RoleAssistant, Content: assistantParts})
				if len(toolResults) > 0 {
					out = append(out, llm.Message{Role: llm.RoleTool, Content: toolResults})
				}
				continue
			}
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
			d, ok := store.DataAs[*store.TextPartData](p)
			if !ok {
				continue
			}
			text := d.Text
			if text == "" {
				text = " "
			}
			assistantParts = append(assistantParts, llm.NewTextPart(text))

		case store.PartTypeReasoning:
			d, ok := store.DataAs[*store.ReasoningPartData](p)
			if !ok || opts.StripMedia {
				continue
			}
			if d.Text != "" {
				assistantParts = append(assistantParts, llm.NewReasoningPart(d.Text, d.Signature))
			}

		case store.PartTypeTool:
			d, ok := store.DataAs[*store.ToolPartData](p)
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
// Called synchronously at the end of each successful RunLoop (ProcessStop path),
// with a 10-second timeout, so that RunLoopAsync.Done is only closed after
// Prune completes — preventing a concurrent new RunLoop from racing with
// Prune's UpdatePart writes on the same session.
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
			d, ok := store.DataAs[*store.ToolPartData](p)
			if !ok || d.Status != store.ToolStatusCompleted || d.Compacted > 0 {
				continue
			}

			// Estimate token size of this tool output
			outputTokens := len(d.Output) / 4 // rough: 4 chars per token

			if tokensProtected < llm.PruneProtect {
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

	if totalPruned < llm.PruneMinimum { // totalPruned is already in estimated tokens (chars/4)
		return fmt.Errorf("prune: not enough content to prune (%d estimated tokens)", totalPruned)
	}

	return nil
}

// SoftReset rolls back the session to the most recent compaction save-point.
//
// It locates the most recent compaction boundary (the user message carrying a
// PartTypeCompaction part + summary assistant pair), then deletes that boundary
// message and everything after it (summary, tail, post-boundary new messages).
//
// fresh controls what happens after the rollback:
//
//   - fresh=false (normal rollback): the head messages that were summarised are
//     left intact and become visible to the LLM again on the next turn —
//     exactly as if the compaction never happened.
//
//   - fresh=true (soft-refresh): after the rollback a new compaction boundary +
//     empty summary are inserted at the end of the message list. FilterCompacted
//     will hide all head messages (they remain in SQLite and are searchable via
//     knowledge_search), giving the LLM a clean slate while preserving history
//     for recall.
//
// Returns the message IDs that were deleted. Callers should pass these to
// SessionHistorySource.RollbackTo so the corresponding Bleve/L0/SQLite history
// index entries are cleaned up — preventing duplicate search results if the
// same head messages are re-indexed on the next compaction.
//
// If no compaction boundary is found (the session has never been compacted),
// SoftReset returns ErrNoCompactionBoundary so the caller can fall back to a
// hard reset or report a meaningful error to the user.
//
// The caller is responsible for holding any session-level lock across this call
// to prevent concurrent RunLoop writes from interleaving with the delete.
func SoftReset(ctx context.Context, sessionID string, s store.Store, fresh bool) (deletedIDs []string, err error) {
	msgs, err := s.ListMessages(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("soft reset: list messages: %w", err)
	}
	allParts, err := s.ListPartsBySession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("soft reset: list parts: %w", err)
	}

	// Walk backward to find the most recent compaction boundary.
	// A boundary is a user message that has a PartTypeCompaction part AND is
	// immediately followed (possibly with gap) by a summary assistant message.
	// We use the same paired-search as FilterCompacted to avoid mis-matching.
	var boundaryIdx int = -1
	for i := len(msgs) - 1; i >= 1; i-- {
		if msgs[i].Role != store.RoleAssistant || !msgs[i].Summary {
			continue
		}
		// Found a summary — look for its boundary user message just before it.
		for j := i - 1; j >= 0; j-- {
			if msgs[j].Role != store.RoleUser {
				continue
			}
			if hasPartType(allParts[msgs[j].ID], store.PartTypeCompaction) {
				boundaryIdx = j
				break
			}
			// Hit a real user message before finding a boundary — stop search.
			break
		}
		if boundaryIdx >= 0 {
			break
		}
	}

	if boundaryIdx < 0 {
		return nil, ErrNoCompactionBoundary
	}

	// Collect the IDs of all messages that will be deleted (boundary onward).
	for i := boundaryIdx; i < len(msgs); i++ {
		deletedIDs = append(deletedIDs, msgs[i].ID)
	}

	// Delete by explicit ID list — precise and immune to timestamp collisions.
	if err := s.DeleteMessagesByIDs(ctx, sessionID, deletedIDs); err != nil {
		return nil, err
	}

	// fresh=true: insert a new compaction boundary + empty summary so that
	// FilterCompacted hides the now-visible head messages while keeping them
	// in SQLite for knowledge_search recall.
	if fresh {
		now := time.Now()
		freshBoundaryID := newID()
		if err := s.CreateMessage(ctx, &store.Message{
			ID:        freshBoundaryID,
			SessionID: sessionID,
			Role:      store.RoleUser,
			CreatedAt: now,
			UpdatedAt: now,
		}); err != nil {
			// Non-fatal: head messages become visible but nothing is corrupted.
			// Return the already-deleted IDs so RollbackTo still runs.
			return deletedIDs, fmt.Errorf("soft reset fresh: create boundary: %w", err)
		}
		if err := s.CreatePart(ctx, &store.Part{
			ID:        newID(),
			MessageID: freshBoundaryID,
			SessionID: sessionID,
			Type:      store.PartTypeCompaction,
			CreatedAt: now,
			UpdatedAt: now,
			Data:      &store.CompactionPartData{TailStartID: ""},
		}); err != nil {
			return deletedIDs, fmt.Errorf("soft reset fresh: create boundary part: %w", err)
		}
		freshSummaryID := newID()
		if err := s.CreateMessage(ctx, &store.Message{
			ID:        freshSummaryID,
			SessionID: sessionID,
			Role:      store.RoleAssistant,
			Summary:   true,
			CreatedAt: now.Add(time.Millisecond),
			UpdatedAt: now.Add(time.Millisecond),
		}); err != nil {
			return deletedIDs, fmt.Errorf("soft reset fresh: create summary: %w", err)
		}
	}

	return deletedIDs, nil
}

// ErrNoCompactionBoundary is returned by SoftReset when the session has no
// compaction boundary to roll back to.
var ErrNoCompactionBoundary = fmt.Errorf("soft reset: no compaction boundary found")

// RecoverOrphanedTools scans all tool parts in the session and marks any that
// are still in pending or running state as error+interrupted. This repairs
// parts left behind when a process was killed mid-stream (SIGKILL, crash, or
// 250ms cleanup timeout) before tool goroutines could write their final status.
//
// Call this once per session at startup, before the first RunLoop, to ensure
// the store is in a consistent state. It is idempotent and safe to call on
// sessions that have no orphaned parts.
func RecoverOrphanedTools(ctx context.Context, sessionID string, s store.Store) error {
	allParts, err := s.ListPartsBySession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("recover orphaned tools: list parts: %w", err)
	}

	for _, parts := range allParts {
		for _, p := range parts {
			if p.Type != store.PartTypeTool {
				continue
			}
			d, ok := store.DataAs[*store.ToolPartData](p)
			if !ok {
				continue
			}
			if d.Status != store.ToolStatusPending && d.Status != store.ToolStatusRunning {
				continue
			}
			d.Status = store.ToolStatusError
			d.Interrupted = true
			if d.Error == "" {
				d.Error = "Tool execution aborted: process exited before completion"
			}
			p.Data = d
			if err := s.UpdatePart(ctx, p); err != nil {
				return fmt.Errorf("recover orphaned tools: update part %q: %w", p.ID, err)
			}
		}
	}
	return nil
}
