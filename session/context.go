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
//   - Assistant messages: skip if errored (unless AbortedError with content)
//   - Tool parts: completed → output-available; error/interrupted → output-error
//   - Compacted tool output → "[Old tool result content cleared]"
//   - Pending/running tool parts (interrupted) → "[Tool execution was interrupted]"
func ToModelMessages(msgs []*store.Message, parts map[string][]*store.Part) ([]llm.Message, error) {
	var out []llm.Message

	for _, m := range msgs {
		ps := parts[m.ID]

		switch m.Role {
		case store.RoleUser:
			userParts := buildUserParts(ps)
			if len(userParts) == 0 {
				continue
			}
			out = append(out, llm.Message{
				ID:      m.ID,
				Role:    llm.RoleUser,
				Content: userParts,
			})

		case store.RoleAssistant:
			// Skip errored assistant messages (unless they have real content)
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

// buildUserParts converts user message parts into LLM content parts.
func buildUserParts(ps []*store.Part) []llm.ContentPart {
	var parts []llm.ContentPart
	for _, p := range ps {
		switch p.Type {
		case store.PartTypeText:
			d, ok := p.Data.(*store.TextPartData)
			if !ok {
				continue
			}
			if d.Text != "" {
				parts = append(parts, llm.NewTextPart(d.Text))
			}
		case store.PartTypeCompaction:
			// Compaction boundary — represents summarised history
			parts = append(parts, llm.NewTextPart("What did we do so far?"))
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
			d, ok := p.Data.(*store.TextPartData)
			if !ok {
				continue
			}
			text := d.Text
			if text == "" {
				text = " " // preserve empty text to avoid Anthropic validation errors
			}
			assistantParts = append(assistantParts, llm.NewTextPart(text))

		case store.PartTypeReasoning:
			d, ok := p.Data.(*store.ReasoningPartData)
			if !ok {
				continue
			}
			if d.Text != "" {
				assistantParts = append(assistantParts, llm.NewReasoningPart(d.Text, d.Signature))
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
			if d, ok := p.Data.(*store.TextPartData); ok && d.Text != "" {
				return true
			}
		}
		if p.Type == store.PartTypeReasoning {
			if d, ok := p.Data.(*store.ReasoningPartData); ok && d.Text != "" {
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
