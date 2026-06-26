package session

import (
	_ "embed"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/larryhou/llm-go/llm"
	"github.com/larryhou/llm-go/store"
)

//go:embed delegate_summary.txt
var delegateSummaryTemplate string

//go:embed delegate_summary_partial.txt
var delegateSummaryPartialTemplate string

// maxToolOutputLogChars is the per-tool-result character limit when rendering
// the execution log for the summary LLM. Larger than ToolOutputMaxCharsCompaction
// (2000) because the summary LLM needs richer signal to produce a good summary,
// but still bounded to avoid overwhelming its context window.
const maxToolOutputLogChars = 8_000

// maxTextLogRunes is the per-assistant-text rune limit in the execution log.
// Long reasoning text is trimmed so the log stays focused on actions and results.
const maxTextLogRunes = 2_000

// generateDelegateSummary calls an LLM to produce a structured summary of a
// completed sub-session's execution log.
//
// When partial is true, the sub-session was interrupted (e.g. by a timeout)
// before the task finished. The system prompt and user prompt both reflect this
// so the summary LLM reports what was accomplished rather than implying the
// task was completed.
//
// Design: the sub-session history is rendered as a plain-text execution log
// (not forwarded as llm.Message conversation turns). This is injected into the
// system prompt so the summary LLM analyses it as an observer, not as a
// participant continuing the conversation. The user turn carries only the task
// goal and the fixed-structure output template.
//
// On failure, callers should fall back to extractLastAssistantText.
func generateDelegateSummary(
	ctx context.Context,
	subSessionID string,
	goal string,
	s store.Store,
	summaryProvider llm.Provider,
	model llm.Model,
	partial bool,
) (string, error) {
	msgs, allParts, err := loadMessages(ctx, s, subSessionID)
	if err != nil {
		return "", fmt.Errorf("delegate summary: load messages: %w", err)
	}
	if len(msgs) == 0 {
		return "", fmt.Errorf("delegate summary: sub-session produced no messages")
	}

	log := renderExecutionLog(msgs, allParts)
	if log == "" {
		return "", fmt.Errorf("delegate summary: execution log is empty")
	}

	systemPreamble := "You are an analyst summarising the results of an autonomous AI agent execution.\n" +
		"The agent was given a task and ran a series of tool calls to complete it.\n"
	if partial {
		systemPreamble += "NOTE: The agent was interrupted before the task was fully completed (e.g. due to a timeout). " +
			"Summarise only what was actually accomplished. Do not imply the task is done.\n"
	}
	system := []string{
		systemPreamble +
			"Below is the execution log so far:\n\n" +
			"<execution_log>\n" + log + "\n</execution_log>",
	}

	var promptTemplate string
	if partial {
		promptTemplate = delegateSummaryPartialTemplate
	} else {
		promptTemplate = delegateSummaryTemplate
	}
	prompt := strings.ReplaceAll(promptTemplate, "{goal}", goal)

	summaryModel := model
	summaryModel.Limit = llm.ModelLimit{Context: 200_000, Output: 8_192}

	now := time.Now()
	summaryMsgID := newID()
	if err := s.CreateMessage(ctx, &store.Message{
		ID:        summaryMsgID,
		SessionID: subSessionID,
		Role:      store.RoleAssistant,
		Model:     model.ID + "/" + model.ProviderID,
		CreatedAt: now,
		UpdatedAt: now,
		Summary:   true,
	}); err != nil {
		return "", fmt.Errorf("delegate summary: create summary message: %w", err)
	}

	proc := NewProcessor(s)
	result, err := proc.Process(ctx, summaryMsgID, ProcessInput{
		SessionID: subSessionID,
		Model:     summaryModel,
		System:    system,
		Messages:  []llm.Message{llm.NewUserMessage(prompt)},
		Tools:     nil,
		Provider:  summaryProvider,
	})
	if err != nil {
		return "", fmt.Errorf("delegate summary: LLM call failed: %w", err)
	}
	if result == ProcessCompact {
		return "", fmt.Errorf("delegate summary: context overflow during summary generation")
	}

	parts, err := s.ListParts(ctx, summaryMsgID)
	if err != nil {
		return "", fmt.Errorf("delegate summary: list summary parts: %w", err)
	}

	var sb strings.Builder
	for _, p := range parts {
		if p.Type == store.PartTypeText {
			if d, ok := store.DataAs[*store.TextPartData](p); ok {
				sb.WriteString(d.Text)
			}
		}
	}
	text := strings.TrimSpace(sb.String())
	if text == "" {
		return "", fmt.Errorf("delegate summary: LLM returned empty summary")
	}
	return text, nil
}

// renderExecutionLog converts sub-session messages and parts into a
// human-readable plain-text execution log for the summary LLM.
//
// Format:
//
//	[Step N]
//	ASSISTANT: <text>
//	TOOL CALL: <tool_name>(<json_input>)
//	TOOL RESULT (<tool_name>): <output>
//	...
//
// Structural parts (step-start, step-finish, compaction, recent-context) are
// skipped. Reasoning blocks are omitted — the summary LLM does not need them.
// Tool outputs are capped at maxToolOutputLogChars; assistant text at
// maxTextLogRunes. Both truncations append a "[... truncated]" marker.
func renderExecutionLog(msgs []*store.Message, allParts map[string][]*store.Part) string {
	var sb strings.Builder
	step := 0

	for _, m := range msgs {
		ps := allParts[m.ID]

		switch m.Role {
		case store.RoleUser:
			// Only include the original user goal message (first user message).
			// Skip compaction boundary messages and synthetic post-compact prompts.
			hasCompaction := false
			for _, p := range ps {
				if p.Type == store.PartTypeCompaction {
					hasCompaction = true
					break
				}
			}
			if hasCompaction {
				continue
			}
			var text string
			for _, p := range ps {
				if p.Type == store.PartTypeText {
					if d, ok := store.DataAs[*store.TextPartData](p); ok {
						text += d.Text
					}
				}
			}
			text = strings.TrimSpace(text)
			if text == "" {
				continue
			}
			// Skip synthetic system-injected messages (post-compact prompts).
			if text == PromptPostCompact || text == PromptContextTooLarge {
				continue
			}
			fmt.Fprintf(&sb, "USER GOAL: %s\n\n", text)

		case store.RoleAssistant:
			if m.Summary {
				continue // skip compaction summary messages
			}

			step++
			fmt.Fprintf(&sb, "[Step %d]\n", step)

			for _, p := range ps {
				switch p.Type {
				case store.PartTypeText:
					d, ok := store.DataAs[*store.TextPartData](p)
					if !ok || strings.TrimSpace(d.Text) == "" {
						continue
					}
					text := trimRunes(strings.TrimSpace(d.Text), maxTextLogRunes)
					fmt.Fprintf(&sb, "ASSISTANT: %s\n", text)

				case store.PartTypeTool:
					d, ok := store.DataAs[*store.ToolPartData](p)
					if !ok {
						continue
					}
					// Tool call line
					inputJSON := marshalCompact(d.Input)
					fmt.Fprintf(&sb, "TOOL CALL: %s(%s)\n", d.Tool, inputJSON)

					// Tool result line
					switch d.Status {
					case store.ToolStatusCompleted:
						output := d.Output
						if d.Omitted > 0 || d.Compacted > 0 {
							output = "[output cleared]"
						} else if len(output) > maxToolOutputLogChars {
							output = output[:maxToolOutputLogChars] + fmt.Sprintf(" ... [truncated, full output at: %s]", d.OutputPath)
						}
						fmt.Fprintf(&sb, "TOOL RESULT (%s): %s\n", d.Tool, output)
					case store.ToolStatusError:
						msg := d.Error
						if msg == "" {
							msg = "tool execution failed"
						}
						fmt.Fprintf(&sb, "TOOL ERROR (%s): %s\n", d.Tool, msg)
					}
				}
			}
			sb.WriteByte('\n')
		}
	}

	return strings.TrimSpace(sb.String())
}

// trimRunes returns s truncated to at most n runes, appending " ... [truncated]"
// if truncation occurred.
func trimRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n]) + " ... [truncated]"
}

// marshalCompact returns a compact JSON representation of v, falling back to
// fmt.Sprintf on error. Used to render tool inputs in the execution log.
func marshalCompact(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	s := string(b)
	// Keep it readable: cap at 300 chars
	if len(s) > 300 {
		s = s[:300] + "...}"
	}
	return s
}

// extractLastAssistantText is a fallback that returns the text content of the
// last assistant message in the sub-session. Used when the summary LLM call
// fails so the caller still gets something useful rather than a blank result.
func extractLastAssistantText(ctx context.Context, sessionID string, s store.Store) string {
	msgs, err := s.ListMessages(ctx, sessionID)
	if err != nil {
		return ""
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role != store.RoleAssistant || m.Summary {
			continue
		}
		parts, err := s.ListParts(ctx, m.ID)
		if err != nil {
			continue
		}
		var sb strings.Builder
		for _, p := range parts {
			if p.Type == store.PartTypeText {
				if d, ok := store.DataAs[*store.TextPartData](p); ok {
					sb.WriteString(d.Text)
				}
			}
		}
		if text := strings.TrimSpace(sb.String()); text != "" {
			return text
		}
	}
	return ""
}
