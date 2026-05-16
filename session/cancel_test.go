package session_test

// Tests for the RunLoopAsync / Cancel() interruption mechanism.
//
// Coverage:
//   Unit (ToModelMessages):
//     1. cancelled assistant → skipped; consecutive user msgs get " " placeholder
//     2. interrupted, text only → text kept, no interruption notice
//     3. interrupted, completed tool → text + notice + tool call kept
//     4. interrupted, all-incomplete tools → entire turn dropped
//     5. interrupted, mixed tools → only completed tools kept
//   Integration (RunLoopAsync):
//     6. Cancel before any LLM response → assistantMsg Status=cancelled
//     7. Cancel after text but no tools → assistantMsg Status=interrupted, text preserved
//     8. Done channel closes only after store is in consistent state
//     9. New RunLoop after Cancel sees clean history (no dangling assistant message)
//    10. Cancel is idempotent — calling twice does not panic

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/larryhou/llm-go/llm"
	"github.com/larryhou/llm-go/session"
	"github.com/larryhou/llm-go/store"
	"github.com/larryhou/llm-go/store/memory"
	"github.com/larryhou/llm-go/tool"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func newSession(t *testing.T) (store.Store, string) {
	t.Helper()
	s := memory.New()
	id := "sess-" + t.Name()
	if err := s.CreateSession(context.Background(), &store.Session{ID: id}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return s, id
}

func cancelInput(sessID string, prov llm.Provider, tools []tool.Tool) session.RunInput {
	return session.RunInput{
		SessionID:             sessID,
		UserMsg:               "test message",
		Model:                 testModel(),
		Provider:              prov,
		Tools:                 tools,
		DisableProviderPrompt: true,
		MaxSteps:              10,
	}
}

// assistantMessages returns all assistant (non-summary) messages for sessID.
func assistantMessages(t *testing.T, s store.Store, sessID string) []*store.Message {
	t.Helper()
	msgs, err := s.ListMessages(context.Background(), sessID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var out []*store.Message
	for _, m := range msgs {
		if m.Role == store.RoleAssistant && !m.Summary {
			out = append(out, m)
		}
	}
	return out
}

// ── Unit: ToModelMessages — cancelled / interrupted ───────────────────────────

// Test 1: cancelled assistant message → skipped; consecutive user messages get " " placeholder.
func TestToModelMessages_cancelledAssistant_placeholder(t *testing.T) {
	msgs := []*store.Message{
		{ID: "u1", Role: store.RoleUser},
		{ID: "a1", Role: store.RoleAssistant, Status: store.MessageStatusCancelled},
		{ID: "u2", Role: store.RoleUser},
	}
	parts := map[string][]*store.Part{
		"u1": {{Type: store.PartTypeText, Data: &store.TextPartData{Text: "first message"}}},
		"a1": {}, // no parts — cancelled before any content
		"u2": {{Type: store.PartTypeText, Data: &store.TextPartData{Text: "correction"}}},
	}

	out, err := session.ToModelMessages(msgs, parts)
	if err != nil {
		t.Fatal(err)
	}

	// Expected: user("first message") → assistant(" ") → user("correction")
	if len(out) != 3 {
		t.Fatalf("got %d messages, want 3 [user, assistant-placeholder, user]; roles: %v", len(out), roles(out))
	}
	if out[0].Role != llm.RoleUser {
		t.Errorf("[0] role = %q, want user", out[0].Role)
	}
	if out[1].Role != llm.RoleAssistant {
		t.Errorf("[1] role = %q, want assistant (placeholder)", out[1].Role)
	}
	if len(out[1].Content) != 1 || out[1].Content[0].Text != " " {
		t.Errorf("[1] content = %+v, want single space placeholder", out[1].Content)
	}
	if out[2].Role != llm.RoleUser {
		t.Errorf("[2] role = %q, want user", out[2].Role)
	}
	if out[2].Content[0].Text != "correction" {
		t.Errorf("[2] text = %q, want 'correction'", out[2].Content[0].Text)
	}
}

// Test 2: interrupted assistant, text only → text kept, no interruption notice appended.
func TestToModelMessages_interruptedTextOnly(t *testing.T) {
	msgs := []*store.Message{
		{ID: "a1", Role: store.RoleAssistant, Status: store.MessageStatusInterrupted},
	}
	parts := map[string][]*store.Part{
		"a1": {
			{Type: store.PartTypeText, Data: &store.TextPartData{Text: "partial answer so far..."}},
		},
	}

	out, err := session.ToModelMessages(msgs, parts)
	if err != nil {
		t.Fatal(err)
	}

	if len(out) != 1 {
		t.Fatalf("got %d messages, want 1 (assistant with text)", len(out))
	}
	if out[0].Role != llm.RoleAssistant {
		t.Errorf("role = %q, want assistant", out[0].Role)
	}
	if len(out[0].Content) != 1 {
		t.Fatalf("content len = %d, want 1 (text only)", len(out[0].Content))
	}
	if out[0].Content[0].Text != "partial answer so far..." {
		t.Errorf("text = %q, want 'partial answer so far...'", out[0].Content[0].Text)
	}
	// No interruption notice should be appended when there are no tool calls
	for _, p := range out[0].Content {
		if strings.Contains(p.Text, "interrupted") {
			t.Errorf("unexpected interruption notice in text-only turn: %q", p.Text)
		}
	}
}

// Test 3: interrupted assistant, completed tool → text + interruption notice + tool call;
// tool result is present; text appears before tool call (Anthropic ordering).
func TestToModelMessages_interruptedWithCompletedTool(t *testing.T) {
	msgs := []*store.Message{
		{ID: "a1", Role: store.RoleAssistant, Status: store.MessageStatusInterrupted},
	}
	parts := map[string][]*store.Part{
		"a1": {
			{Type: store.PartTypeText, Data: &store.TextPartData{Text: "let me check"}},
			{Type: store.PartTypeTool, Data: &store.ToolPartData{
				Tool:   "read",
				CallID: "c1",
				Status: store.ToolStatusCompleted,
				Input:  map[string]any{"path": "/foo.go"},
				Output: "package main",
			}},
		},
	}

	out, err := session.ToModelMessages(msgs, parts)
	if err != nil {
		t.Fatal(err)
	}

	// Expected: 1 assistant message + 1 tool-role message
	if len(out) != 2 {
		t.Fatalf("got %d messages, want 2 (assistant + tool-result); roles: %v", len(out), roles(out))
	}

	asst := out[0]
	if asst.Role != llm.RoleAssistant {
		t.Errorf("[0] role = %q, want assistant", asst.Role)
	}

	// Content ordering: text → interruption notice → tool call
	if len(asst.Content) != 3 {
		t.Fatalf("assistant content len = %d, want 3 [text, notice, tool-call]", len(asst.Content))
	}
	if asst.Content[0].Text != "let me check" {
		t.Errorf("[0] text = %q, want 'let me check' (text before tool)", asst.Content[0].Text)
	}
	if !strings.Contains(asst.Content[1].Text, "interrupted") {
		t.Errorf("[1] notice = %q, want interruption notice", asst.Content[1].Text)
	}
	if asst.Content[2].Type != "tool-call" {
		t.Errorf("[2] type = %q, want tool-call", asst.Content[2].Type)
	}

	// Tool result message
	tr := out[1]
	if tr.Role != llm.RoleTool {
		t.Errorf("[1] role = %q, want tool", tr.Role)
	}
	if tr.Content[0].Result == nil || tr.Content[0].Result.Value != "package main" {
		t.Errorf("tool result = %+v, want 'package main'", tr.Content[0].Result)
	}
}

// Test 4: interrupted assistant, all tools incomplete → entire turn dropped.
func TestToModelMessages_interruptedAllToolsIncomplete_dropped(t *testing.T) {
	msgs := []*store.Message{
		{ID: "a1", Role: store.RoleAssistant, Status: store.MessageStatusInterrupted},
	}
	parts := map[string][]*store.Part{
		"a1": {
			{Type: store.PartTypeText, Data: &store.TextPartData{Text: "about to search..."}},
			{Type: store.PartTypeTool, Data: &store.ToolPartData{
				Tool:   "glob",
				CallID: "c1",
				Status: store.ToolStatusRunning, // still running — not completed
				Input:  map[string]any{"pattern": "**/*.go"},
			}},
		},
	}

	out, err := session.ToModelMessages(msgs, parts)
	if err != nil {
		t.Fatal(err)
	}

	// Entire turn dropped — text discarded to avoid dangling tool-call with no result
	if len(out) != 0 {
		t.Errorf("got %d messages, want 0 (entire interrupted-incomplete-tools turn dropped); roles: %v", len(out), roles(out))
	}
}

// Test 5: interrupted assistant, mixed tools → only completed tools kept.
func TestToModelMessages_interruptedMixedTools_onlyCompletedKept(t *testing.T) {
	msgs := []*store.Message{
		{ID: "a1", Role: store.RoleAssistant, Status: store.MessageStatusInterrupted},
	}
	parts := map[string][]*store.Part{
		"a1": {
			{Type: store.PartTypeTool, Data: &store.ToolPartData{
				Tool:   "read",
				CallID: "c1",
				Status: store.ToolStatusCompleted,
				Input:  map[string]any{"path": "/a.go"},
				Output: "content a",
			}},
			{Type: store.PartTypeTool, Data: &store.ToolPartData{
				Tool:   "glob",
				CallID: "c2",
				Status: store.ToolStatusPending, // not completed
				Input:  map[string]any{"pattern": "**/*.go"},
			}},
		},
	}

	out, err := session.ToModelMessages(msgs, parts)
	if err != nil {
		t.Fatal(err)
	}

	if len(out) != 2 {
		t.Fatalf("got %d messages, want 2 (assistant + tool-result); roles: %v", len(out), roles(out))
	}

	// Only one tool call in the assistant message (c1, not c2)
	var toolCallParts []llm.ContentPart
	for _, p := range out[0].Content {
		if p.Type == "tool-call" {
			toolCallParts = append(toolCallParts, p)
		}
	}
	if len(toolCallParts) != 1 {
		t.Errorf("assistant has %d tool-call parts, want 1 (only completed c1)", len(toolCallParts))
	}
	if toolCallParts[0].ToolCallID != "c1" {
		t.Errorf("kept tool call ID = %q, want c1", toolCallParts[0].ToolCallID)
	}

	// Only one tool result
	if len(out[1].Content) != 1 {
		t.Errorf("tool-result message has %d parts, want 1", len(out[1].Content))
	}
	if out[1].Content[0].Result.Value != "content a" {
		t.Errorf("tool result = %q, want 'content a'", out[1].Content[0].Result.Value)
	}
}

// ── Integration: RunLoopAsync + Cancel() ─────────────────────────────────────

// Test 6: Cancel() before any LLM content → assistantMsg Status=cancelled.
func TestRunLoopAsync_cancelBeforeResponse_statusCancelled(t *testing.T) {
	s, sessID := newSession(t)

	// blockingProvider blocks until ctx is cancelled, never sends events.
	prov := &blockingProvider{}

	h := session.RunLoopAsync(context.Background(), s, cancelInput(sessID, prov, nil))

	// Give the goroutine time to reach Process() and block
	time.Sleep(20 * time.Millisecond)
	h.Cancel()
	<-h.Done

	asstMsgs := assistantMessages(t, s, sessID)
	if len(asstMsgs) != 1 {
		t.Fatalf("expected 1 assistant message, got %d", len(asstMsgs))
	}
	if asstMsgs[0].Status != store.MessageStatusCancelled {
		t.Errorf("Status = %q, want %q", asstMsgs[0].Status, store.MessageStatusCancelled)
	}

	// No parts should have been written (LLM never responded)
	parts, _ := s.ListParts(context.Background(), asstMsgs[0].ID)
	if len(parts) != 0 {
		t.Errorf("expected 0 parts for cancelled message, got %d", len(parts))
	}
}

// Test 7: Cancel() after LLM emits text but no tool calls → Status=interrupted, text preserved.
func TestRunLoopAsync_cancelAfterText_statusInterrupted(t *testing.T) {
	s, sessID := newSession(t)

	// Provider sends text then blocks (simulates slow LLM still generating)
	prov := &textThenBlockProvider{text: "I was about to say something..."}

	h := session.RunLoopAsync(context.Background(), s, cancelInput(sessID, prov, nil))

	// Wait for text to be written, then cancel
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		asstMsgs := assistantMessages(t, s, sessID)
		if len(asstMsgs) > 0 {
			parts, _ := s.ListParts(context.Background(), asstMsgs[0].ID)
			if hasTextPart(parts) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.Cancel()
	<-h.Done

	asstMsgs := assistantMessages(t, s, sessID)
	if len(asstMsgs) != 1 {
		t.Fatalf("expected 1 assistant message, got %d", len(asstMsgs))
	}
	if asstMsgs[0].Status != store.MessageStatusInterrupted {
		t.Errorf("Status = %q, want %q", asstMsgs[0].Status, store.MessageStatusInterrupted)
	}

	parts, _ := s.ListParts(context.Background(), asstMsgs[0].ID)
	if !hasTextPart(parts) {
		t.Error("expected text part to be preserved after interruption")
	}
}

// Test 8: Done closes only after store is in consistent state —
// Status is set and all tool parts are finalised before <-Done.
func TestRunLoopAsync_doneAfterStoreConsistent(t *testing.T) {
	s, sessID := newSession(t)
	prov := &blockingProvider{}

	h := session.RunLoopAsync(context.Background(), s, cancelInput(sessID, prov, nil))
	time.Sleep(20 * time.Millisecond)
	h.Cancel()
	<-h.Done // must not return until store writes are complete

	// After Done, the assistant message must have a non-empty Status
	asstMsgs := assistantMessages(t, s, sessID)
	if len(asstMsgs) == 0 {
		t.Fatal("no assistant messages found after Done")
	}
	status := asstMsgs[0].Status
	if status != store.MessageStatusCancelled && status != store.MessageStatusInterrupted {
		t.Errorf("Status = %q after Done; want cancelled or interrupted", status)
	}
}

// Test 9: New RunLoop after Cancel sees clean history — no dangling assistant message
// causes protocol errors; the LLM request must have valid alternating roles.
func TestRunLoopAsync_newRunLoopAfterCancel_cleanHistory(t *testing.T) {
	s, sessID := newSession(t)

	// First turn: cancel before any response
	h := session.RunLoopAsync(context.Background(), s, cancelInput(sessID, &blockingProvider{}, nil))
	time.Sleep(20 * time.Millisecond)
	h.Cancel()
	<-h.Done

	// Second turn: capture the LLM request and verify role sequence
	var capturedReq llm.Request
	var mu sync.Mutex
	captureProv := &captureProvider{
		inner: simpleTextProvider("correction acknowledged"),
		captured: func(r llm.Request) {
			mu.Lock()
			capturedReq = r
			mu.Unlock()
		},
	}

	h2 := session.RunLoopAsync(context.Background(), s, session.RunInput{
		SessionID:             sessID,
		UserMsg:               "actually ignore that, do this instead",
		Model:                 testModel(),
		Provider:              captureProv,
		DisableProviderPrompt: true,
		MaxSteps:              1,
	})
	<-h2.Done

	mu.Lock()
	req := capturedReq
	mu.Unlock()

	if len(req.Messages) == 0 {
		t.Fatal("no messages in captured request")
	}

	// Verify alternating roles: must not have two consecutive user messages
	for i := 1; i < len(req.Messages); i++ {
		if req.Messages[i].Role == llm.RoleUser && req.Messages[i-1].Role == llm.RoleUser {
			t.Errorf("consecutive user messages at index %d and %d — protocol violation", i-1, i)
		}
	}
}

// Test 10: Cancel() is idempotent — calling multiple times concurrently does not panic.
func TestRunLoopAsync_cancelIdempotent(t *testing.T) {
	s, sessID := newSession(t)
	prov := &blockingProvider{}

	h := session.RunLoopAsync(context.Background(), s, cancelInput(sessID, prov, nil))
	time.Sleep(10 * time.Millisecond)

	// Call Cancel from multiple goroutines simultaneously
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.Cancel()
		}()
	}
	wg.Wait()
	<-h.Done // must complete without panic
}

// ── helpers ───────────────────────────────────────────────────────────────────

func roles(msgs []llm.Message) []string {
	r := make([]string, len(msgs))
	for i, m := range msgs {
		r[i] = m.Role
	}
	return r
}

func hasTextPart(parts []*store.Part) bool {
	for _, p := range parts {
		if p.Type == store.PartTypeText {
			if d, ok := store.DataAs[*store.TextPartData](p); ok && d.Text != "" {
				return true
			}
		}
	}
	return false
}

// textThenBlockProvider sends text events then blocks until ctx is cancelled.
type textThenBlockProvider struct {
	text string
}

func (p *textThenBlockProvider) ID() string { return "text-then-block" }
func (p *textThenBlockProvider) Stream(ctx context.Context, _ llm.Request) (<-chan llm.Event, error) {
	ch := make(chan llm.Event, 8)
	go func() {
		defer close(ch)
		// Send text events immediately
		ch <- llm.Event{Type: llm.EventRequestStart}
		ch <- llm.Event{Type: llm.EventStepStart}
		ch <- llm.Event{Type: llm.EventTextStart}
		ch <- llm.Event{Type: llm.EventTextDelta, Text: p.text}
		// Then block until cancelled
		<-ctx.Done()
		ch <- llm.Event{Type: llm.EventError, Err: ctx.Err()}
	}()
	return ch, nil
}

// captureProvider (function-based callback, avoids name clash with capturingProvider in session_test.go)
type captureProvider struct {
	inner    llm.Provider
	captured func(llm.Request)
}

func (c *captureProvider) ID() string { return c.inner.ID() }
func (c *captureProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	if c.captured != nil {
		c.captured(req)
	}
	return c.inner.Stream(ctx, req)
}
