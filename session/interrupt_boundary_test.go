package session_test

// Boundary condition tests for the RunLoopAsync / Cancel() interruption mechanism.
//
// Coverage (BC-1 through BC-8):
//   BC-1: Cancel during tool call (Status=running) → tool part Status=error, Interrupted=true
//   BC-2: Cancel immediately after tool completes → completed tool preserved
//   BC-3: Two consecutive cancels, then normal turn → valid alternating-role history
//   BC-4: Cancel during second step of multi-step loop → first step preserved
//   BC-5: Interrupted turn with all-incomplete tools → next turn sees " " placeholder
//   BC-6: Cancel after RunLoop already completed → no panic, idempotent
//   BC-7: MaxSteps=1 exhausted before cancel → Status="" (normal stop)
//   BC-8: <-h.Done guarantees no tool part is in Status=running
//
// Additional:
//   TestMarkAssistantCancelled_emptyTextPart (STEP F): empty text part → Status=cancelled

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/larryhou/llm-go/llm"
	"github.com/larryhou/llm-go/session"
	"github.com/larryhou/llm-go/store"
	"github.com/larryhou/llm-go/tool"
)

// ── BC-1 ─────────────────────────────────────────────────────────────────────

// BC-1: Cancel while a slow tool is running.
// After <-h.Done the tool part must have Status=error and Interrupted=true.
func TestRunLoopAsync_cancelDuringToolExecution_toolMarkedInterrupted(t *testing.T) {
	s, sessID := newSession(t)

	// blockHeld is used by the tool to wait for explicit release from the test.
	// This guarantees Execute is truly blocking when h.Cancel() is called.
	blockHeld := make(chan struct{})
	toolReady := make(chan struct{})
	prov := &slowToolProvider{toolName: "slow_tool", callID: "c1"}
	slowTool := &latchedBlockingTool{name: "slow_tool", ready: toolReady, release: blockHeld}

	h := session.RunLoopAsync(context.Background(), s, cancelInput(sessID, prov, []tool.Tool{slowTool}))

	// Wait until Execute has been entered and is blocking.
	select {
	case <-toolReady:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for latchedBlockingTool to signal ready")
	}

	// Now we know Execute has been entered and is blocking.
	// Cancel the session. The cleanup() 250ms timeout will fire and mark the
	// tool part as interrupted. After h.Done closes, release the tool goroutine.
	h.Cancel()
	<-h.Done
	close(blockHeld) // release latched goroutine so it can call toolWg.Done()

	asstMsgs := assistantMessages(t, s, sessID)
	if len(asstMsgs) != 1 {
		t.Fatalf("expected 1 assistant message, got %d", len(asstMsgs))
	}
	if asstMsgs[0].Status != store.MessageStatusInterrupted {
		t.Errorf("message Status = %q, want %q", asstMsgs[0].Status, store.MessageStatusInterrupted)
	}

	parts, _ := s.ListParts(context.Background(), asstMsgs[0].ID)
	var found bool
	for _, p := range parts {
		if p.Type != store.PartTypeTool {
			continue
		}
		d, ok := store.DataAs[*store.ToolPartData](p)
		if !ok {
			continue
		}
		found = true
		if d.Status != store.ToolStatusError {
			t.Errorf("tool part Status = %q, want %q", d.Status, store.ToolStatusError)
		}
		if !d.Interrupted {
			t.Errorf("tool part Interrupted = false, want true")
		}
	}
	if !found {
		t.Error("no tool part found in assistant message")
	}
}

// ── BC-2 ─────────────────────────────────────────────────────────────────────

// BC-2: Cancel immediately after a fast tool completes.
// The completed tool part must be preserved with Status=completed.
func TestRunLoopAsync_cancelAfterToolCompletes_toolPreserved(t *testing.T) {
	s, sessID := newSession(t)

	toolDone := make(chan struct{}) // closed when instantTool.Execute returns
	prov := &slowToolProvider{toolName: "fast_tool", callID: "c2"}
	fastTool := &signallingInstantTool{name: "fast_tool", done: toolDone}

	h := session.RunLoopAsync(context.Background(), s, cancelInput(sessID, prov, []tool.Tool{fastTool}))

	// Wait until the fast tool has completed execution.
	select {
	case <-toolDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for fast tool to complete")
	}

	// Small pause to let the result be written to the store.
	time.Sleep(20 * time.Millisecond)
	h.Cancel()
	<-h.Done

	asstMsgs := assistantMessages(t, s, sessID)
	if len(asstMsgs) == 0 {
		t.Fatal("no assistant messages found")
	}
	// Check the first assistant message (the one with the tool call).
	parts, _ := s.ListParts(context.Background(), asstMsgs[0].ID)
	for _, p := range parts {
		if p.Type != store.PartTypeTool {
			continue
		}
		d, ok := store.DataAs[*store.ToolPartData](p)
		if !ok {
			continue
		}
		if d.Status == store.ToolStatusCompleted {
			return // found a completed tool part — test passes
		}
	}
	t.Error("expected at least one tool part with Status=completed, none found")
}

// ── BC-3 ─────────────────────────────────────────────────────────────────────

// BC-3: Two consecutive cancels, then a normal turn.
// The third turn's LLM request must not contain two consecutive user messages.
func TestRunLoopAsync_twoConsecutiveCancels_thenNormalTurn_validHistory(t *testing.T) {
	s, sessID := newSession(t)

	// First cancel
	h1 := session.RunLoopAsync(context.Background(), s, cancelInput(sessID, &blockingProvider{}, nil))
	time.Sleep(20 * time.Millisecond)
	h1.Cancel()
	<-h1.Done

	// Second cancel
	h2 := session.RunLoopAsync(context.Background(), s, cancelInput(sessID, &blockingProvider{}, nil))
	time.Sleep(20 * time.Millisecond)
	h2.Cancel()
	<-h2.Done

	// Third turn — capture the LLM request
	var mu sync.Mutex
	var capturedReq llm.Request
	capProv := &captureProvider{
		inner: simpleTextProvider("ok"),
		captured: func(r llm.Request) {
			mu.Lock()
			capturedReq = r
			mu.Unlock()
		},
	}
	h3 := session.RunLoopAsync(context.Background(), s, session.RunInput{
		SessionID:             sessID,
		UserMsg:               "third message",
		Model:                 testModel(),
		Provider:              capProv,
		DisableProviderPrompt: true,
		MaxSteps:              1,
	})
	<-h3.Done

	mu.Lock()
	req := capturedReq
	mu.Unlock()

	if len(req.Messages) == 0 {
		t.Fatal("no messages in captured request")
	}
	for i := 1; i < len(req.Messages); i++ {
		if req.Messages[i].Role == llm.RoleUser && req.Messages[i-1].Role == llm.RoleUser {
			t.Errorf("consecutive user messages at index %d and %d — protocol violation", i-1, i)
		}
	}
}

// ── BC-4 ─────────────────────────────────────────────────────────────────────

// BC-4: Cancel during the second step of a multi-step loop.
// Step 1: LLM emits a fast tool call; tool completes; loop continues.
// Step 2: LLM emits a slow tool call; Cancel() is called while tool is executing.
// After <-h.Done, ListMessages should show exactly 2 assistant messages:
// one with Status="" (step 1 completed) and one with Status=interrupted/cancelled.
func TestRunLoopAsync_cancelDuringSecondStep_firstStepPreserved(t *testing.T) {
	s, sessID := newSession(t)

	step2ToolReady := make(chan struct{}) // closed when step2 latchedBlockingTool.Execute starts
	step2ToolRelease := make(chan struct{})
	prov := &twoStepProvider{
		step1ToolName: "fast_tool",
		step1CallID:   "c4a",
		step2ToolName: "slow_tool",
		step2CallID:   "c4b",
	}
	fastTool := &instantTool{name: "fast_tool"}
	slowTool := &latchedBlockingTool{name: "slow_tool", ready: step2ToolReady, release: step2ToolRelease}

	h := session.RunLoopAsync(context.Background(), s, cancelInput(sessID, prov, []tool.Tool{fastTool, slowTool}))

	select {
	case <-step2ToolReady:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for step2 tool Execute to start")
	}

	h.Cancel()
	<-h.Done
	close(step2ToolRelease) // release latched goroutine so it can call toolWg.Done()

	asstMsgs := assistantMessages(t, s, sessID)
	if len(asstMsgs) != 2 {
		t.Fatalf("expected 2 assistant messages (step1 + step2), got %d", len(asstMsgs))
	}

	// First assistant message must be a normal completed turn.
	if asstMsgs[0].Status != "" {
		t.Errorf("step1 Status = %q, want \"\" (normal)", asstMsgs[0].Status)
	}

	// Second assistant message must be cancelled or interrupted.
	if asstMsgs[1].Status != store.MessageStatusInterrupted &&
		asstMsgs[1].Status != store.MessageStatusCancelled {
		t.Errorf("step2 Status = %q, want interrupted or cancelled", asstMsgs[1].Status)
	}
}

// ── BC-5 ─────────────────────────────────────────────────────────────────────

// BC-5: Interrupted turn where all tools are error/incomplete.
// The turn is dropped from history; the next turn must see no consecutive user
// messages in the LLM request (the " " placeholder ensures alternating roles).
func TestRunLoopAsync_interruptedAllIncompleteTools_nextTurnSeesPlaceholder(t *testing.T) {
	s, sessID := newSession(t)

	// Use slowToolProvider with no registered tool — the tool part gets
	// Status=error immediately (tool not found), which counts as non-completed.
	// The provider also blocks after sending events, keeping the loop alive.
	prov := &slowToolProvider{toolName: "never_finishes", callID: "c5"}

	h := session.RunLoopAsync(context.Background(), s, cancelInput(sessID, prov, nil))

	// Give the loop time to start and the tool error to be set.
	time.Sleep(50 * time.Millisecond)
	h.Cancel()
	<-h.Done

	// Now start a second turn and capture what the LLM sees.
	var mu sync.Mutex
	var capturedReq llm.Request
	capProv := &captureProvider{
		inner: simpleTextProvider("ack"),
		captured: func(r llm.Request) {
			mu.Lock()
			capturedReq = r
			mu.Unlock()
		},
	}
	h2 := session.RunLoopAsync(context.Background(), s, session.RunInput{
		SessionID:             sessID,
		UserMsg:               "second message",
		Model:                 testModel(),
		Provider:              capProv,
		DisableProviderPrompt: true,
		MaxSteps:              1,
	})
	<-h2.Done

	mu.Lock()
	req := capturedReq
	mu.Unlock()

	// Must not have consecutive user messages.
	for i := 1; i < len(req.Messages); i++ {
		if req.Messages[i].Role == llm.RoleUser && req.Messages[i-1].Role == llm.RoleUser {
			t.Errorf("consecutive user messages at %d and %d — protocol violation", i-1, i)
		}
	}
}

// ── BC-6 ─────────────────────────────────────────────────────────────────────

// BC-6: Cancel after RunLoop has already completed — must not panic.
func TestRunLoopAsync_cancelAfterCompletion_noPanic(t *testing.T) {
	s, sessID := newSession(t)

	h := session.RunLoopAsync(context.Background(), s, session.RunInput{
		SessionID:             sessID,
		UserMsg:               "hi",
		Model:                 testModel(),
		Provider:              simpleTextProvider("hello"),
		DisableProviderPrompt: true,
		MaxSteps:              1,
	})
	<-h.Done // wait for natural completion

	// Cancel after done — must be a no-op, not panic.
	h.Cancel()
	h.Cancel() // twice for good measure

	if h.Err != nil {
		t.Errorf("unexpected error after natural completion: %v", h.Err)
	}
}

// ── BC-7 ─────────────────────────────────────────────────────────────────────

// BC-7: MaxSteps=1 with a tool-calling provider — step limit reached before cancel.
// Result must be RunResultStop, not an interrupt; message Status must be "".
func TestRunLoopAsync_maxStepsExhausted_notCancelled(t *testing.T) {
	s, sessID := newSession(t)

	// Provider that would normally trigger a tool call but MaxSteps=1 suppresses tools.
	prov := toolCallProvider("some_tool", "cx7", map[string]any{"x": "y"})

	h := session.RunLoopAsync(context.Background(), s, session.RunInput{
		SessionID:             sessID,
		UserMsg:               "do stuff",
		Model:                 testModel(),
		Provider:              prov,
		DisableProviderPrompt: true,
		MaxSteps:              1,
	})
	<-h.Done

	if h.Err != nil {
		t.Errorf("unexpected error: %v", h.Err)
	}
	if h.Result != session.RunResultStop {
		t.Errorf("Result = %v, want RunResultStop", h.Result)
	}

	asstMsgs := assistantMessages(t, s, sessID)
	if len(asstMsgs) == 0 {
		t.Fatal("no assistant message found")
	}
	// Status must be "" — normal stop, not cancelled/interrupted.
	if asstMsgs[0].Status != "" {
		t.Errorf("message Status = %q, want \"\" (normal stop)", asstMsgs[0].Status)
	}
}

// ── BC-8 ─────────────────────────────────────────────────────────────────────

// BC-8: After <-h.Done, no tool part should be in Status=running.
func TestRunLoopAsync_doneGuaranteesNoRunningToolParts(t *testing.T) {
	s, sessID := newSession(t)

	toolReady := make(chan struct{})
	toolRelease := make(chan struct{})
	prov := &slowToolProvider{toolName: "slow_tool", callID: "c8"}
	slowTool := &latchedBlockingTool{name: "slow_tool", ready: toolReady, release: toolRelease}

	h := session.RunLoopAsync(context.Background(), s, cancelInput(sessID, prov, []tool.Tool{slowTool}))

	select {
	case <-toolReady:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for tool Execute to start")
	}
	h.Cancel()
	<-h.Done
	close(toolRelease) // release latched goroutine so it can call toolWg.Done()

	// After Done, inspect all tool parts — none must be in Status=running.
	msgs, err := s.ListMessages(context.Background(), sessID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	for _, m := range msgs {
		parts, _ := s.ListParts(context.Background(), m.ID)
		for _, p := range parts {
			if p.Type != store.PartTypeTool {
				continue
			}
			d, ok := store.DataAs[*store.ToolPartData](p)
			if !ok {
				continue
			}
			if d.Status == store.ToolStatusRunning {
				t.Errorf("tool part %s still in Status=running after <-h.Done", p.ID)
			}
		}
	}
}

// ── STEP F ────────────────────────────────────────────────────────────────────

// TestMarkAssistantCancelled_emptyTextPart: an empty text part must not cause
// the message to be classified as interrupted (hasRealContent=false → cancelled).
func TestMarkAssistantCancelled_emptyTextPart(t *testing.T) {
	s, sessID := newSession(t)

	// Use a provider that sends an empty text delta then blocks.
	prov := &emptyTextThenBlockProvider{}

	h := session.RunLoopAsync(context.Background(), s, cancelInput(sessID, prov, nil))

	// Wait for the empty text event to be processed.
	time.Sleep(50 * time.Millisecond)
	h.Cancel()
	<-h.Done

	asstMsgs := assistantMessages(t, s, sessID)
	if len(asstMsgs) != 1 {
		t.Fatalf("expected 1 assistant message, got %d", len(asstMsgs))
	}
	// An empty text part has no real content → Status must be cancelled, not interrupted.
	if asstMsgs[0].Status != store.MessageStatusCancelled {
		t.Errorf("Status = %q, want %q (empty text part should not count as real content)",
			asstMsgs[0].Status, store.MessageStatusCancelled)
	}
}

// ── provider helpers ──────────────────────────────────────────────────────────

// slowToolProvider emits all tool call events including EventRequestFinish.
// It respects context cancellation: if ctx is already done when Stream() is
// called, it returns an error immediately (mimicking real provider behavior).
type slowToolProvider struct {
	toolName string
	callID   string
}

func (p *slowToolProvider) ID() string { return "slow-tool" }
func (p *slowToolProvider) Stream(ctx context.Context, _ llm.Request) (<-chan llm.Event, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	ch := make(chan llm.Event, 16)
	go func() {
		defer close(ch)
		ch <- llm.Event{Type: llm.EventRequestStart}
		ch <- llm.Event{Type: llm.EventStepStart}
		ch <- llm.Event{Type: llm.EventToolInputStart, ToolName: p.toolName, ToolCallID: p.callID}
		ch <- llm.Event{Type: llm.EventToolCall, ToolName: p.toolName, ToolCallID: p.callID,
			Input: map[string]any{"arg": "value"}}
		ch <- llm.Event{Type: llm.EventStepFinish, Usage: llm.TokenUsage{Input: 5, Output: 2},
			FinishReason: llm.FinishReasonToolCalls}
		ch <- llm.Event{Type: llm.EventRequestFinish, FinishReason: llm.FinishReasonToolCalls}
	}()
	return ch, nil
}

// twoStepProvider emits:
//   - Step 1 (call 1): a fast tool call + EventRequestFinish → Process() returns normally
//   - Step 2+ (call ≥2): a slow tool call + EventRequestFinish → Process() returns,
//     latchedBlockingTool.Execute starts blocking (ignoring toolCancel), cleanup
//     times out after 250ms, marks part as interrupted.
type twoStepProvider struct {
	step1ToolName string
	step1CallID   string
	step2ToolName string
	step2CallID   string

	mu        sync.Mutex
	callCount int
}

func (p *twoStepProvider) ID() string { return "two-step" }
func (p *twoStepProvider) Stream(_ context.Context, _ llm.Request) (<-chan llm.Event, error) {
	p.mu.Lock()
	p.callCount++
	n := p.callCount
	p.mu.Unlock()

	ch := make(chan llm.Event, 16)
	go func() {
		defer close(ch)
		if n == 1 {
			// Step 1: emit a fast tool call then finish normally.
			ch <- llm.Event{Type: llm.EventRequestStart}
			ch <- llm.Event{Type: llm.EventStepStart}
			ch <- llm.Event{Type: llm.EventToolInputStart, ToolName: p.step1ToolName, ToolCallID: p.step1CallID}
			ch <- llm.Event{Type: llm.EventToolCall, ToolName: p.step1ToolName, ToolCallID: p.step1CallID,
				Input: map[string]any{"step": "1"}}
			ch <- llm.Event{Type: llm.EventStepFinish,
				Usage: llm.TokenUsage{Input: 10, Output: 5}, FinishReason: llm.FinishReasonToolCalls}
			ch <- llm.Event{Type: llm.EventRequestFinish, FinishReason: llm.FinishReasonToolCalls}
		} else {
			// Step 2+: emit a slow tool call + EventRequestFinish.
			// latchedBlockingTool ignores toolCancel() so cleanup times out
			// and marks the part as interrupted.
			ch <- llm.Event{Type: llm.EventRequestStart}
			ch <- llm.Event{Type: llm.EventStepStart}
			ch <- llm.Event{Type: llm.EventToolInputStart, ToolName: p.step2ToolName, ToolCallID: p.step2CallID}
			ch <- llm.Event{Type: llm.EventToolCall, ToolName: p.step2ToolName, ToolCallID: p.step2CallID,
				Input: map[string]any{"step": "2"}}
			ch <- llm.Event{Type: llm.EventStepFinish,
				Usage: llm.TokenUsage{Input: 8, Output: 3}, FinishReason: llm.FinishReasonToolCalls}
			ch <- llm.Event{Type: llm.EventRequestFinish, FinishReason: llm.FinishReasonToolCalls}
		}
	}()
	return ch, nil
}

// emptyTextThenBlockProvider sends an empty text delta then blocks.
// Used to test the hasRealContent(Text=="") edge case.
type emptyTextThenBlockProvider struct{}

func (p *emptyTextThenBlockProvider) ID() string { return "empty-text-block" }
func (p *emptyTextThenBlockProvider) Stream(ctx context.Context, _ llm.Request) (<-chan llm.Event, error) {
	ch := make(chan llm.Event, 8)
	go func() {
		defer close(ch)
		ch <- llm.Event{Type: llm.EventRequestStart}
		ch <- llm.Event{Type: llm.EventStepStart}
		ch <- llm.Event{Type: llm.EventTextStart}
		ch <- llm.Event{Type: llm.EventTextDelta, Text: ""} // empty delta
		<-ctx.Done()
		ch <- llm.Event{Type: llm.EventError, Err: ctx.Err()}
	}()
	return ch, nil
}

// ── tool helpers ──────────────────────────────────────────────────────────────

// blockingTool blocks until its context is cancelled — simulates a slow
// external tool that never returns on its own.
type blockingTool struct {
	name string
}

func (t *blockingTool) Name() string        { return t.name }
func (t *blockingTool) Description() string { return "blocks until cancelled" }
func (t *blockingTool) InputSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}
func (t *blockingTool) Execute(ctx context.Context, _ map[string]any) (tool.Result, error) {
	<-ctx.Done()
	return tool.Result{}, ctx.Err()
}

// latchedBlockingTool signals ready when Execute is entered, then blocks on
// release ONLY — it deliberately does NOT listen to ctx.Done(). This ensures
// cleanup()'s toolCancel() cannot make it exit early, so cleanup's 250ms
// timeout fires and sets Status=error + Interrupted=true for this tool part.
// The test must close release after h.Cancel() to unblock the goroutine;
// the tool returns a non-nil error so executeTool does NOT call updateToolCompleted
// (which would overwrite the Interrupted=true set by cleanup).
type latchedBlockingTool struct {
	name      string
	ready     chan struct{}
	release   chan struct{}
	readyOnce sync.Once
}

func (t *latchedBlockingTool) Name() string        { return t.name }
func (t *latchedBlockingTool) Description() string { return "signals ready then blocks until released" }
func (t *latchedBlockingTool) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (t *latchedBlockingTool) Execute(_ context.Context, _ map[string]any) (tool.Result, error) {
	// Deliberately ignore ctx — only unblocks when release is closed.
	// Return a context-error so executeTool marks the part as error
	// (not completed), consistent with the interrupted status.
	t.readyOnce.Do(func() { close(t.ready) })
	<-t.release
	return tool.Result{}, context.Canceled
}

// signallingBlockingTool closes ready when Execute is entered, then blocks.
// Used to get a reliable signal that the tool goroutine is actually running.
type signallingBlockingTool struct {
	name      string
	ready     chan struct{}
	readyOnce sync.Once
}

func (t *signallingBlockingTool) Name() string        { return t.name }
func (t *signallingBlockingTool) Description() string { return "signals ready then blocks" }
func (t *signallingBlockingTool) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (t *signallingBlockingTool) Execute(ctx context.Context, _ map[string]any) (tool.Result, error) {
	t.readyOnce.Do(func() { close(t.ready) })
	<-ctx.Done()
	return tool.Result{}, ctx.Err()
}

// signallingInstantTool closes done after Execute returns — used to reliably
// detect that the fast tool has completed before Cancel() is called.
type signallingInstantTool struct {
	name     string
	done     chan struct{}
	doneOnce sync.Once
}

func (t *signallingInstantTool) Name() string        { return t.name }
func (t *signallingInstantTool) Description() string { return "completes immediately and signals" }
func (t *signallingInstantTool) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (t *signallingInstantTool) Execute(_ context.Context, _ map[string]any) (tool.Result, error) {
	defer t.doneOnce.Do(func() { close(t.done) })
	return tool.Result{Output: "done"}, nil
}

// instantTool completes immediately — simulates a fast tool.
type instantTool struct {
	name string
}

func (t *instantTool) Name() string        { return t.name }
func (t *instantTool) Description() string { return "completes immediately" }
func (t *instantTool) InputSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}
func (t *instantTool) Execute(_ context.Context, _ map[string]any) (tool.Result, error) {
	return tool.Result{Output: "done"}, nil
}
