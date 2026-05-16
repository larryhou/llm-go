package session

import (
	"fmt"

	"github.com/larryhou/llm-go/llm"
	"github.com/larryhou/llm-go/store"
)

// ToModelMessages converts stored messages and their parts into the canonical
// llm.Message slice to be sent to the provider.
// Aligned with packages/opencode/src/session/message-v2.ts toModelMessagesEffect().
//
// Key rules:
//   - User messages: text parts → text content; compaction parts → "What did we do so far?"
//   - recent-context parts → verbatim excerpt (stripped when StripMedia=true)
//   - Assistant messages: skip if errored (unless AbortedError with content)
//   - Tool parts: completed → output-available; error/interrupted → output-error
//   - Compacted tool output → "[Old tool result content cleared]"
//   - Pending/running tool parts (interrupted) → "[Tool execution was interrupted]"
//
// Interrupted assistant messages (Status=interrupted/cancelled):
//   - cancelled (no content): completely invisible to LLM; a silent " " placeholder
//     is injected between any two consecutive user messages to satisfy protocol.
//   - interrupted + no tool calls: keep partial text as-is; LLM infers naturally.
//   - interrupted + has tool calls: keep completed tools, discard pending ones,
//     append "[Assistant turn was interrupted by user]" so LLM does not retry.
func ToModelMessages(msgs []*store.Message, parts map[string][]*store.Part) ([]llm.Message, error) {
	// Pre-filter: remove user+assistant pairs where the assistant never produced
	// content (cancelled before response, or LLM transport error with no content).
	// These pairs are completely invisible to the LLM. Removing them here prevents
	// the alternating-role guard below from inserting a " " placeholder, which
	// proxies reject as invalid assistant content.
	filtered := make([]*store.Message, 0, len(msgs))
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		if m.Role == store.RoleAssistant {
			ps := parts[m.ID]
			drop := false
			if m.Status == store.MessageStatusCancelled {
				drop = true
			} else if m.Error != nil && !hasRealContent(ps) {
				drop = true
			}
			if drop {
				// Also drop the preceding user message if it is there.
				if len(filtered) > 0 && filtered[len(filtered)-1].Role == store.RoleUser {
					filtered = filtered[:len(filtered)-1]
				}
				continue
			}
		}
		filtered = append(filtered, m)
	}

	var out []llm.Message

	for _, m := range filtered {
		ps := parts[m.ID]

		switch m.Role {
		case store.RoleUser:
			userParts := buildUserParts(ps, ToModelOptions{})
			if len(userParts) == 0 {
				continue
			}
			// Protocol fix: two consecutive user messages are invalid on both
			// Anthropic and OpenAI. This can still happen if an interrupted
			// assistant message had no usable content (buildAssistantPartsInterrupted
			// returns empty). Insert a single-space placeholder — but only as a last
			// resort; the pre-filter above eliminates the common cancelled/error cases.
			if len(out) > 0 && out[len(out)-1].Role == llm.RoleUser {
				out = append(out, llm.Message{
					Role:    llm.RoleAssistant,
					Content: []llm.ContentPart{llm.NewTextPart(" ")},
				})
			}
			out = append(out, llm.Message{
				ID:      m.ID,
				Role:    llm.RoleUser,
				Content: userParts,
			})

		case store.RoleAssistant:
			// Interrupted: partial content exists. Handle tool-call case specially.
			if m.Status == store.MessageStatusInterrupted {
				assistantParts, toolResultMsgs := buildAssistantPartsInterrupted(ps)
				if len(assistantParts) == 0 {
					continue
				}
				out = append(out, llm.Message{
					ID:      m.ID,
					Role:    llm.RoleAssistant,
					Content: assistantParts,
				})
				if len(toolResultMsgs) > 0 {
					out = append(out, llm.Message{
						Role:    llm.RoleTool,
						Content: toolResultMsgs,
					})
				}
				continue
			}

			// Normal path: skip errored/empty assistant messages.
			if m.Error != nil {
				if !hasRealContent(ps) {
					continue
				}
			}

			assistantParts, toolResultMsgs := buildAssistantParts(m, ps)
			if len(assistantParts) == 0 {
				continue
			}
			out = append(out, llm.Message{
				ID:      m.ID,
				Role:    llm.RoleAssistant,
				Content: assistantParts,
			})

			// Inject tool results as a synthetic tool-role message
			// (required by Anthropic; OpenAI expects them interleaved with assistant messages)
			if len(toolResultMsgs) > 0 {
				out = append(out, llm.Message{
					Role:    llm.RoleTool,
					Content: toolResultMsgs,
				})
			}
		}
	}

	return out, nil
}

// buildAssistantPartsInterrupted handles an assistant message whose turn was
// cut short by Cancel() after some content had been emitted.
//
// Rules (per design):
//   - Text/reasoning parts: always kept as-is (LLM infers naturally from partial text).
//   - Tool parts with Status=completed: kept with their output.
//   - Tool parts with Status=pending/running/error(interrupted): discarded from
//     the assistant side AND from the tool-result side (no dangling tool calls).
//   - If any tool call was present (completed or not), a single
//     "[Assistant turn was interrupted by user]" notice is inserted BEFORE the
//     tool call parts so the LLM does not attempt to retry incomplete tool calls.
//     Text before tool use is the safe ordering for Anthropic.
func buildAssistantPartsInterrupted(ps []*store.Part) ([]llm.ContentPart, []llm.ContentPart) {
	// Collect text/reasoning and tool parts separately so we can order them:
	// [text/reasoning] → [interruption notice] → [tool calls]
	var textParts []llm.ContentPart
	var toolCallParts []llm.ContentPart
	var toolResults []llm.ContentPart
	hadAnyTool := false

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
			textParts = append(textParts, llm.NewTextPart(text))

		case store.PartTypeReasoning:
			d, ok := store.DataAs[*store.ReasoningPartData](p)
			if !ok {
				continue
			}
			if d.Text != "" {
				textParts = append(textParts, llm.NewReasoningPart(d.Text, d.Signature))
			}

		case store.PartTypeTool:
			hadAnyTool = true
			d, ok := store.DataAs[*store.ToolPartData](p)
			if !ok {
				continue
			}
			// Only include tool calls that completed — drop pending/running/interrupted.
			if d.Status != store.ToolStatusCompleted {
				continue
			}
			inputMap := d.Input
			if inputMap == nil {
				inputMap = map[string]any{}
			}
			toolCallParts = append(toolCallParts, llm.NewToolCallPart(d.CallID, d.Tool, inputMap))
			toolResults = append(toolResults, llm.NewToolResultPart(d.CallID, d.Tool, buildToolResult(d)))
		}
	}

	// Assemble: text/reasoning first, then interruption notice (if tools were
	// involved), then tool call parts. Text before tool use is the safe
	// content ordering for Anthropic's validation rules.
	//
	// Special case: the LLM emitted tool calls (hadAnyTool=true) but none
	// completed before cancellation (toolCallParts is empty). We cannot keep
	// the text parts alone because the assistant message would then reference
	// tool calls that have no corresponding tool-result, creating a broken
	// history fragment. Drop the entire turn; the consecutive-user-message
	// guard in ToModelMessages inserts a " " placeholder so the protocol
	// alternating-role requirement is still satisfied.
	//
	// Note: this is distinct from the "interrupted + no tool calls" case
	// (hadAnyTool=false), where partial text is kept as-is because the LLM
	// never issued a tool call and there is no tool-result gap to worry about.
	if hadAnyTool && len(toolCallParts) == 0 {
		return nil, nil
	}

	assistantParts := textParts
	if hadAnyTool {
		assistantParts = append(assistantParts, llm.NewTextPart("[Assistant turn was interrupted by user]"))
		assistantParts = append(assistantParts, toolCallParts...)
	}

	return assistantParts, toolResults
}

// buildUserParts converts user message parts into LLM content parts.
// opts.StripMedia=true suppresses PartTypeRecentContext (used during summary
// generation to avoid feeding previous excerpts to the summary LLM).
func buildUserParts(ps []*store.Part, opts ToModelOptions) []llm.ContentPart {
	var parts []llm.ContentPart
	for _, p := range ps {
		switch p.Type {
		case store.PartTypeText:
			d, ok := store.DataAs[*store.TextPartData](p)
			if !ok {
				continue
			}
			if d.Text != "" {
				parts = append(parts, llm.NewTextPart(d.Text))
			}
		case store.PartTypeCompaction:
			// Compaction boundary — represents summarised history
			parts = append(parts, llm.NewTextPart("What did we do so far?"))
		case store.PartTypeRecentContext:
			if opts.StripMedia {
				continue // omit excerpt when building input for the summary LLM
			}
			d, ok := store.DataAs[*store.RecentContextPartData](p)
			if ok && d.Excerpt != "" {
				parts = append(parts, llm.NewTextPart(d.Excerpt))
			}
		case store.PartTypeAgent:
			parts = append(parts, llm.NewTextPart("The following tool was executed by the user"))
		}
	}
	return parts
}

// buildAssistantParts converts assistant message parts into LLM content parts.
// Returns (assistantParts, toolResultParts).
// Tool results are returned separately so they can be injected as a tool-role message.
func buildAssistantParts(m *store.Message, ps []*store.Part) ([]llm.ContentPart, []llm.ContentPart) {
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
				text = " " // preserve empty text to avoid Anthropic validation errors
			}
			assistantParts = append(assistantParts, llm.NewTextPart(text))

		case store.PartTypeReasoning:
			d, ok := store.DataAs[*store.ReasoningPartData](p)
			if !ok {
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

			// Tool call on the assistant side
			assistantParts = append(assistantParts, llm.NewToolCallPart(d.CallID, d.Tool, inputMap))

			// Tool result on the tool side (will be injected separately)
			result := buildToolResult(d)
			toolResults = append(toolResults, llm.NewToolResultPart(d.CallID, d.Tool, result))

		case store.PartTypeStepStart:
			// step-start parts are structural; not sent to LLM
		}
	}

	return assistantParts, toolResults
}

// buildToolResult converts a ToolPartData into a ToolResult for the LLM context.
// Aligned with message-v2.ts tool part conversion logic.
func buildToolResult(d *store.ToolPartData) *llm.ToolResult {
	switch d.Status {
	case store.ToolStatusCompleted:
		output := d.Output
		if d.Compacted > 0 {
			// Output was pruned to save context space
			output = "[Old tool result content cleared]"
		}
		return llm.NewTextResult(output)

	case store.ToolStatusError:
		// If interrupted but has partial output, use output-available
		if d.Interrupted && d.Output != "" {
			return llm.NewTextResult(d.Output)
		}
		errMsg := d.Error
		if errMsg == "" {
			errMsg = "Tool execution failed"
		}
		return llm.NewErrorResult(errMsg)

	case store.ToolStatusPending, store.ToolStatusRunning:
		// Tool was interrupted before completing
		return llm.NewErrorResult("[Tool execution was interrupted]")
	}

	return llm.NewErrorResult(fmt.Sprintf("unknown tool status: %s", d.Status))
}

// hasRealContent returns true if the message has non-tool, non-empty parts.
// Used to decide whether to keep an errored assistant message in context.
func hasRealContent(ps []*store.Part) bool {
	for _, p := range ps {
		if p.Type == store.PartTypeText {
			if d, ok := store.DataAs[*store.TextPartData](p); ok && d.Text != "" {
				return true
			}
		}
		if p.Type == store.PartTypeReasoning {
			if d, ok := store.DataAs[*store.ReasoningPartData](p); ok && d.Text != "" {
				return true
			}
		}
	}
	return false
}

// FilterCompacted returns only the messages that are visible after compaction.
// Aligned with message-v2.ts filterCompacted().
//
// Rule: find the most recent *complete* compaction pair (boundary user message
// + summary assistant message). Walking backward avoids mis-pairing a boundary
// from one round with a summary from another on partial failures.
func FilterCompacted(msgs []*store.Message, parts map[string][]*store.Part) []*store.Message {
	for i := len(msgs) - 1; i >= 1; i-- {
		if msgs[i].Role != store.RoleAssistant || !msgs[i].Summary {
			continue
		}
		// Found a summary — look for its boundary user message just before it.
		for j := i - 1; j >= 0; j-- {
			if msgs[j].Role != store.RoleUser {
				continue
			}
			ps := parts[msgs[j].ID]
			for _, p := range ps {
				if p.Type == store.PartTypeCompaction {
					out := make([]*store.Message, 0, len(msgs)-j)
					out = append(out, msgs[j])    // compaction boundary user msg
					out = append(out, msgs[i:]...) // summary + tail
					return out
				}
			}
			// A real user message between summary and boundary — no match, stop.
			break
		}
	}
	return msgs
}

// Message role constants (reused from store for convenience).
const (
	roleUser      = "user"
	roleAssistant = "assistant"
)
