package session

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/larryhou/llm-go/config"
	"github.com/larryhou/llm-go/knowledge"
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

	// OnCompact, when non-nil, is called after each successful Compact().
	// Use knowledge.SessionHistorySource.Hook() to index compacted history
	// for later retrieval via knowledge_search / knowledge_fetch.
	// Nil is a no-op — existing callers are unaffected.
	OnCompact knowledge.CompactionHook

	// WaitFor, when non-nil, is waited on before loadMessages is called.
	// Use the previous turn's StoreDone so the new turn starts immediately
	// but only reads history after the previous turn's store writes are done.
	// Nil means no waiting (default behaviour, fully backward-compatible).
	WaitFor <-chan struct{}
}

// RunHandle is returned by RunLoopAsync. It allows the caller to cancel the
// in-flight loop and observe when it has fully exited.
//
// Usage:
//
//	h := session.RunLoopAsync(ctx, store, input)
//	// ... later ...
//	h.Cancel()   // request cancellation (idempotent)
//	<-h.Done     // wait for full cleanup; safe to start a new RunLoop after this
//	result, err := h.Result, h.Err
//
// Memory model: all writes to Result and Err happen-before close(Done), which
// happens-before any receive on Done. No additional synchronisation is needed.
type RunHandle struct {
	// Done is closed after the loop has fully exited and all cleanup is complete
	// (including Prune). Safe to start a new RunLoop on the same session after
	// receiving from Done.
	Done <-chan struct{}

	// StoreDone is closed once the current turn's store writes are complete —
	// specifically after markAssistantCancelled (on cancel) or after the final
	// ProcessStop return (on normal completion), and before Prune runs.
	// A new RunLoop may pass this as WaitFor to start immediately without waiting
	// for Prune, while still guaranteeing a consistent store view.
	StoreDone <-chan struct{}

	// Result and Err are set before Done is closed. Read them only after <-Done.
	Result RunResult
	Err    error

	cancel    context.CancelFunc
	once      sync.Once
	storeDone chan struct{} // closed by runLoopInternal to signal store consistency
	storeOnce sync.Once    // ensures storeDone is closed exactly once
}

// closeStoreDone signals that the store is in a consistent state.
// Safe to call multiple times (idempotent via storeOnce).
func (h *RunHandle) closeStoreDone() {
	h.storeOnce.Do(func() { close(h.storeDone) })
}

// Cancel requests cancellation of the running loop. It is idempotent and
// safe to call from any goroutine, including concurrently with itself.
// The loop may not stop immediately — tool goroutines are given up to 250ms
// to exit cleanly. Wait on Done for full completion.
func (h *RunHandle) Cancel() {
	h.once.Do(h.cancel)
}

// RunLoopAsync starts the agentic loop in a background goroutine and returns
// a RunHandle immediately. The caller can cancel the loop via h.Cancel() and
// wait on h.Done for full completion (including Prune).
//
// Use h.StoreDone to start a new RunLoop as soon as the store is consistent,
// without waiting for Prune to complete.
//
// The parent ctx controls the maximum lifetime of the loop independently of
// Cancel — if ctx is cancelled, the loop is also cancelled.
func RunLoopAsync(ctx context.Context, s store.Store, input RunInput) *RunHandle {
	cancelCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	storeDone := make(chan struct{})
	h := &RunHandle{Done: done, StoreDone: storeDone, storeDone: storeDone, cancel: cancel}
	go func() {
		defer close(done)
		defer cancel()
		defer h.closeStoreDone() // ensure StoreDone is always closed before Done
		h.Result, h.Err = runLoopInternal(cancelCtx, s, input, h)
	}()
	return h
}

// RunLoop is the synchronous wrapper around RunLoopAsync. It blocks until the
// loop completes and returns the result. Existing callers are unaffected.
func RunLoop(ctx context.Context, s store.Store, input RunInput) (RunResult, error) {
	h := RunLoopAsync(ctx, s, input)
	<-h.Done
	return h.Result, h.Err
}

// runLoopInternal is the main agentic loop implementation.
// Aligned with packages/opencode/src/session/prompt.ts runLoop().
func runLoopInternal(ctx context.Context, s store.Store, input RunInput, h *RunHandle) (RunResult, error) {
	processor := NewProcessor(s)
	compactor := NewCompactor(s, processor)

	// If WaitFor is set, block until the previous turn's store writes are done
	// before writing the user message or reading history. This prevents orphaned
	// user messages: if ctx is cancelled during the wait, nothing has been written.
	if input.WaitFor != nil {
		select {
		case <-input.WaitFor:
		case <-ctx.Done():
			return RunResultStop, ctx.Err()
		}
	}

	// Create the user message (after WaitFor so history order is correct and
	// no orphan is left if the turn is cancelled before the wait completes).
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

	// Load messages once and cache across steps.
	// Only reloaded after compaction (which restructures history).
	msgs, allParts, err := loadMessages(ctx, s, input.SessionID)
	if err != nil {
		return RunResultStop, err
	}

	// Initialise lastInputTokens from the most recent assistant message in the
	// store so that cross-turn drops (e.g. compact between two user messages)
	// are detected even though each RunLoop call starts fresh.
	var lastInputTokens int
	{
		filtered := FilterCompacted(msgs, allParts)
		for i := len(filtered) - 1; i >= 0; i-- {
			if filtered[i].Role == store.RoleAssistant && !filtered[i].Summary {
				if n := filtered[i].Tokens.Input; n > 0 {
					lastInputTokens = n
				}
				break
			}
		}
	}

	step := 0
	for {
		isLastStep := input.MaxSteps > 0 && step >= input.MaxSteps
		step++

		// Apply compaction filter using cached msgs/allParts.
		filtered := FilterCompacted(msgs, allParts)

		// Build model messages
		modelMsgs, err := ToModelMessages(filtered, allParts)
		if err != nil {
			return RunResultStop, fmt.Errorf("runloop: build messages: %w", err)
		}

		// On the last step, append a prefilled assistant message instructing the
		// LLM to summarise instead of calling more tools — aligned with
		// opencode packages/opencode/src/session/prompt.ts isLastStep handling.
		if isLastStep {
			modelMsgs = append(modelMsgs, llm.Message{
				Role: "assistant",
				Content: []llm.ContentPart{
					{Type: "text", Text: PromptMaxSteps},
				},
			})
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
		// Add the new assistant message to the cache immediately so the next
		// step sees it without a full reload.
		msgs = append(msgs, assistantMsg)
		allParts[assistantMsgID] = nil // placeholder; filled below after the LLM call

		// On the last step, disable tools so the LLM is forced to respond in text.
		tools := input.Tools
		if isLastStep {
			tools = nil
		}

		// Run one LLM turn
		result, err := processor.Process(ctx, assistantMsgID, ProcessInput{
			SessionID: input.SessionID,
			Model:     input.Model,
			System:    system,
			Messages:  modelMsgs,
			Tools:     tools,
			Provider:  input.Provider,
			Config:    input.Config,
		})
		if err != nil {
			// Only mark the assistant message as cancelled/interrupted when the
			// error is due to context cancellation (i.e. Cancel() was called).
			if ctx.Err() != nil {
				markAssistantCancelled(s, assistantMsgID)
				h.closeStoreDone() // store is consistent; new turn may proceed
			} else {
				// For LLM/transport errors, mark the message with Error so that
				// ToModelMessages skips it cleanly (via the m.Error+no-content guard)
				// without leaving a bare empty assistant message in history.
				// Without this, repeated LLM errors accumulate empty assistant
				// messages that force " " placeholders and corrupt session history.
				if m, merr := s.GetMessage(context.Background(), assistantMsgID); merr == nil {
					m.Error = &store.MessageError{
						Name:    "llm_error",
						Message: err.Error(),
					}
					_ = s.UpdateMessage(context.Background(), m)
				}
				h.closeStoreDone()
			}
			return RunResultStop, err
		}
		// Even when Process() returns a nil error, the session may have been
		// cancelled during the LLM turn (e.g. h.Cancel() called while cleanup()
		// was running its 250ms tool-goroutine wait). Check ctx.Err() and treat
		// cancellation as an interrupt so we don't start a new iteration.
		if ctx.Err() != nil {
			markAssistantCancelled(s, assistantMsgID)
			h.closeStoreDone() // store is consistent; new turn may proceed
			return RunResultStop, ctx.Err()
		}

		// Refresh the cached parts for this assistant message now that the
		// LLM call has written its parts to the store.
		if assistantParts, listErr := s.ListParts(ctx, assistantMsgID); listErr == nil {
			allParts[assistantMsgID] = assistantParts
		}

		// Refresh the assistant message itself to pick up token counts written
		// by finaliseAssistantMessage.
		if am, getErr := s.GetMessage(ctx, assistantMsgID); getErr == nil {
			msgs[len(msgs)-1] = am
			cur := am.Tokens.Input
			if lastInputTokens > 0 && cur < lastInputTokens {
				log.Printf("[session] input tokens dropped: %d → %d (delta=%d)",
					lastInputTokens, cur, cur-lastInputTokens)
			}
			if cur > 0 {
				lastInputTokens = cur
			}
		}

		switch result {
		case ProcessStop:
			// Signal store consistency before Prune so that a new turn waiting
			// on StoreDone can start loadMessages while Prune runs concurrently.
			// Prune only deletes old pruned parts; it does not affect the new
			// turn's history view.
			h.closeStoreDone()
			// Run prune synchronously so that Done is only closed after Prune
			// completes — prevents a concurrent new RunLoop from racing with
			// Prune's UpdatePart writes on the same session.
			// Prune is a pure store operation (no LLM call); in practice it
			// completes in milliseconds. A 10s timeout is a safety net for
			// slow database backends.
			pruneCtx, pruneCancel := context.WithTimeout(context.Background(), 10*time.Second)
			log.Printf("[session] prune triggered: input=%d", lastInputTokens)
			if err := Prune(pruneCtx, input.SessionID, s, input.Config); err != nil {
				log.Printf("[session] prune skipped: %v", err)
			} else {
				log.Printf("[session] prune done")
			}
			pruneCancel() // explicit call — defer inside loop delays release until function return
			return RunResultStop, nil

		case ProcessCompact:
			log.Printf("[session] compact triggered: input=%d", lastInputTokens)
			// Run compaction, then reload the full message cache because
			// compaction restructures history (inserts boundary + summary).
			_, err := compactor.Compact(ctx, input.SessionID, ProcessInput{
				SessionID:       input.SessionID,
				Model:           input.Model,
				Provider:        input.Provider,
				SummaryProvider: input.SummaryProvider,
				Config:          input.Config,
				OnCompact:       input.OnCompact,
			})
			if err != nil {
				log.Printf("[session] compact failed: %v", err)
				return RunResultStop, fmt.Errorf("runloop: compaction failed: %w", err)
			}
			log.Printf("[session] compact done")
			// Invalidate cache — compaction rewrote history.
			msgs, allParts, err = loadMessages(ctx, s, input.SessionID)
			if err != nil {
				return RunResultStop, err
			}
			continue

		case ProcessContinue:
			// Last step always terminates — the LLM has been asked to summarise.
			if isLastStep {
				h.closeStoreDone()
				return RunResultStop, nil
			}
			// Check if the last assistant message finished with tool calls.
			// If so, continue the loop to let the LLM process tool results.
			// allParts[assistantMsgID] was refreshed above after the LLM call.
			if !hasToolCalls(allParts[assistantMsgID]) {
				h.closeStoreDone()
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

	// When session history recall is enabled (OnCompact is set), inject
	// the knowledge_search guidance so the LLM knows it can retrieve
	// compacted history on demand.
	if input.OnCompact != nil {
		parts = append(parts, PromptKnowledgeRecall)
	}

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
// Uses ListPartsBySession to fetch all parts in a single operation,
// avoiding N+1 queries against real database backends.
func loadMessages(ctx context.Context, s store.Store, sessionID string) ([]*store.Message, map[string][]*store.Part, error) {
	msgs, err := s.ListMessages(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	allParts, err := s.ListPartsBySession(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	return msgs, allParts, nil
}

// markAssistantCancelled sets the Status on an assistant message after an
// incomplete Process() call due to ctx cancellation. It always uses
// context.Background() because the caller's ctx is already cancelled.
//
// Status logic:
//   - Has tool calls or real text content → MessageStatusInterrupted
//     (partial content exists; ToModelMessages preserves completed tools)
//   - No content at all → MessageStatusCancelled
//     (invisible to LLM; consecutive-user-message guard inserts a placeholder)
//
// Known race: cleanup() inside Process() waits up to 250ms for tool goroutines.
// After that timeout, goroutines may still be running and could write
// ToolStatusCompleted after ListParts reads here. In that case a completed
// tool result may be misclassified as Cancelled rather than Interrupted.
// This window is small and the consequence is limited to the next turn seeing
// a slightly degraded history. Eliminating it would require an unconditional
// wait for all tool goroutines, which is a larger change left for future work.
func markAssistantCancelled(s store.Store, assistantMsgID string) {
	ctx := context.Background()
	parts, _ := s.ListParts(ctx, assistantMsgID)
	status := store.MessageStatusCancelled
	if hasToolCalls(parts) || hasRealContent(parts) {
		status = store.MessageStatusInterrupted
	}
	if m, err := s.GetMessage(ctx, assistantMsgID); err == nil {
		m.Status = status
		_ = s.UpdateMessage(ctx, m)
	}
}

