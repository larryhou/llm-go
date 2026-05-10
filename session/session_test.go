package session_test

import (
	"context"
	"testing"
	"time"

	"github.com/larryhou/llm-go/llm"
	"github.com/larryhou/llm-go/session"
	"github.com/larryhou/llm-go/store"
	"github.com/larryhou/llm-go/store/memory"
	"github.com/larryhou/llm-go/tool"
)

// ---- mock provider ----

type mockProvider struct {
	id     string
	events []llm.Event
}

func (m *mockProvider) ID() string { return m.id }
func (m *mockProvider) Stream(_ context.Context, _ llm.Request) (<-chan llm.Event, error) {
	ch := make(chan llm.Event, len(m.events)+1)
	for _, ev := range m.events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// simpleTextProvider returns a provider that emits a single text response.
func simpleTextProvider(text string) *mockProvider {
	return &mockProvider{
		id: "mock",
		events: []llm.Event{
			{Type: llm.EventRequestStart},
			{Type: llm.EventStepStart},
			{Type: llm.EventTextStart},
			{Type: llm.EventTextDelta, Text: text},
			{Type: llm.EventTextEnd},
			{Type: llm.EventStepFinish, Usage: llm.TokenUsage{Input: 10, Output: 5}, FinishReason: llm.FinishReasonStop},
			{Type: llm.EventRequestFinish, FinishReason: llm.FinishReasonStop},
		},
	}
}

// toolCallProvider returns a provider that emits one tool call then stops.
func toolCallProvider(toolName, callID string, input map[string]any) *mockProvider {
	return &mockProvider{
		id: "mock",
		events: []llm.Event{
			{Type: llm.EventRequestStart},
			{Type: llm.EventStepStart},
			{Type: llm.EventToolInputStart, ToolName: toolName, ToolCallID: callID},
			{Type: llm.EventToolCall, ToolName: toolName, ToolCallID: callID, Input: input},
			{Type: llm.EventStepFinish, Usage: llm.TokenUsage{Input: 10, Output: 2}, FinishReason: llm.FinishReasonToolCalls},
			{Type: llm.EventRequestFinish, FinishReason: llm.FinishReasonToolCalls},
		},
	}
}

func testModel() llm.Model {
	return llm.Model{
		ID:         "claude-test",
		ProviderID: "mock",
		APIID:      "claude-test",
		Limit:      llm.ModelLimit{Context: 200_000, Output: 8_192},
	}
}

// ---- buildSystem (via RunInput) ----

func TestBuildSystem_defaultProviderPrompt(t *testing.T) {
	// When AgentPrompt is empty and DisableProviderPrompt is false,
	// the embedded provider prompt should be injected for "claude" models.
	s := memory.New()
	ctx := context.Background()
	_ = s.CreateSession(ctx, &store.Session{ID: "sess"})

	prov := simpleTextProvider("hello")
	// Use a model APIID containing "claude" — should get anthropic.txt
	model := testModel()

	result, err := session.RunLoop(ctx, s, session.RunInput{
		SessionID: "sess",
		UserMsg:   "hi",
		Model:     model,
		Provider:  prov,
		// AgentPrompt empty, DisableProviderPrompt false → provider prompt injected
	})
	if err != nil {
		t.Fatal(err)
	}
	if result != session.RunResultContinue {
		t.Errorf("result = %v, want Continue", result)
	}
}

func TestBuildSystem_agentPromptOverrides(t *testing.T) {
	// When AgentPrompt is set, it replaces the provider prompt entirely.
	// We verify by checking the system that was sent — use a capturing provider.
	capturedReqs := make([]llm.Request, 0, 1)
	prov := &capturingProvider{
		inner:    simpleTextProvider("ok"),
		captured: &capturedReqs,
	}

	s := memory.New()
	ctx := context.Background()
	_ = s.CreateSession(ctx, &store.Session{ID: "sess2"})

	_, err := session.RunLoop(ctx, s, session.RunInput{
		SessionID:   "sess2",
		UserMsg:     "test",
		Model:       testModel(),
		Provider:    prov,
		AgentPrompt: "custom agent prompt",
		ExtraSystem: []string{"extra instruction"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(capturedReqs) == 0 {
		t.Fatal("no requests captured")
	}
	req := capturedReqs[0]
	// Should have exactly 2 system parts: AgentPrompt + ExtraSystem
	if len(req.System) != 2 {
		t.Errorf("System len = %d, want 2; system = %v", len(req.System), req.System)
	}
	if req.System[0] != "custom agent prompt" {
		t.Errorf("System[0] = %q, want 'custom agent prompt'", req.System[0])
	}
	if req.System[1] != "extra instruction" {
		t.Errorf("System[1] = %q, want 'extra instruction'", req.System[1])
	}
}

func TestBuildSystem_disableProviderPrompt(t *testing.T) {
	capturedReqs := make([]llm.Request, 0, 1)
	prov := &capturingProvider{
		inner:    simpleTextProvider("ok"),
		captured: &capturedReqs,
	}

	s := memory.New()
	ctx := context.Background()
	_ = s.CreateSession(ctx, &store.Session{ID: "sess3"})

	_, err := session.RunLoop(ctx, s, session.RunInput{
		SessionID:             "sess3",
		UserMsg:               "test",
		Model:                 testModel(),
		Provider:              prov,
		DisableProviderPrompt: true,
		ExtraSystem:           []string{"only this"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := capturedReqs[0]
	if len(req.System) != 1 || req.System[0] != "only this" {
		t.Errorf("System = %v, want [only this]", req.System)
	}
}

// capturingProvider wraps another provider and records every Request.
type capturingProvider struct {
	inner    llm.Provider
	captured *[]llm.Request
}

func (c *capturingProvider) ID() string { return c.inner.ID() }
func (c *capturingProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	*c.captured = append(*c.captured, req)
	return c.inner.Stream(ctx, req)
}

// ---- ToModelMessages ----

func TestToModelMessages_userOnly(t *testing.T) {
	msgs := []*store.Message{
		{ID: "m1", Role: store.RoleUser},
	}
	parts := map[string][]*store.Part{
		"m1": {
			{ID: "p1", Type: store.PartTypeText, Data: &store.TextPartData{Text: "hello world"}},
		},
	}
	out, err := session.ToModelMessages(msgs, parts)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d messages, want 1", len(out))
	}
	if out[0].Role != llm.RoleUser {
		t.Errorf("role = %q, want user", out[0].Role)
	}
	if len(out[0].Content) != 1 || out[0].Content[0].Text != "hello world" {
		t.Errorf("unexpected content: %+v", out[0].Content)
	}
}

func TestToModelMessages_compactionPart(t *testing.T) {
	msgs := []*store.Message{
		{ID: "m1", Role: store.RoleUser},
	}
	parts := map[string][]*store.Part{
		"m1": {
			{ID: "p1", Type: store.PartTypeCompaction, Data: &store.CompactionPartData{}},
		},
	}
	out, err := session.ToModelMessages(msgs, parts)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Content[0].Text != "What did we do so far?" {
		t.Errorf("compaction part should produce 'What did we do so far?', got: %+v", out)
	}
}

func TestToModelMessages_toolCompleted(t *testing.T) {
	msgs := []*store.Message{
		{ID: "m1", Role: store.RoleAssistant},
	}
	parts := map[string][]*store.Part{
		"m1": {
			{Type: store.PartTypeTool, Data: &store.ToolPartData{
				Tool:   "shell",
				CallID: "c1",
				Status: store.ToolStatusCompleted,
				Input:  map[string]any{"cmd": "ls"},
				Output: "file1.txt\nfile2.txt",
			}},
		},
	}
	out, err := session.ToModelMessages(msgs, parts)
	if err != nil {
		t.Fatal(err)
	}
	// Should produce: 1 assistant message (with tool-call) + 1 tool-role message (with tool-result)
	if len(out) != 2 {
		t.Fatalf("got %d messages, want 2 (assistant + tool)", len(out))
	}
	if out[0].Role != llm.RoleAssistant {
		t.Errorf("first message role = %q, want assistant", out[0].Role)
	}
	if out[1].Role != llm.RoleTool {
		t.Errorf("second message role = %q, want tool", out[1].Role)
	}
	result := out[1].Content[0].Result
	if result == nil || result.Value != "file1.txt\nfile2.txt" {
		t.Errorf("unexpected tool result: %+v", result)
	}
}

func TestToModelMessages_toolCompacted(t *testing.T) {
	msgs := []*store.Message{{ID: "m1", Role: store.RoleAssistant}}
	parts := map[string][]*store.Part{
		"m1": {
			{Type: store.PartTypeTool, Data: &store.ToolPartData{
				Tool:      "shell",
				CallID:    "c1",
				Status:    store.ToolStatusCompleted,
				Input:     map[string]any{},
				Output:    "large output",
				Compacted: time.Now().UnixMilli(),
			}},
		},
	}
	out, _ := session.ToModelMessages(msgs, parts)
	result := out[1].Content[0].Result
	if result.Value != "[Old tool result content cleared]" {
		t.Errorf("compacted output = %v, want '[Old tool result content cleared]'", result.Value)
	}
}

func TestToModelMessages_toolInterrupted(t *testing.T) {
	msgs := []*store.Message{{ID: "m1", Role: store.RoleAssistant}}
	parts := map[string][]*store.Part{
		"m1": {
			{Type: store.PartTypeTool, Data: &store.ToolPartData{
				Tool:   "shell",
				CallID: "c1",
				Status: store.ToolStatusRunning, // still running → interrupted
				Input:  map[string]any{},
			}},
		},
	}
	out, _ := session.ToModelMessages(msgs, parts)
	result := out[1].Content[0].Result
	if result.Type != llm.ToolResultTypeError {
		t.Errorf("interrupted tool should be error result, got type=%q", result.Type)
	}
}

func TestToModelMessages_skipsErroredAssistant(t *testing.T) {
	msgs := []*store.Message{
		{ID: "m1", Role: store.RoleAssistant, Error: &store.MessageError{Name: "APIError", Message: "oops"}},
	}
	parts := map[string][]*store.Part{"m1": {}} // no real content
	out, _ := session.ToModelMessages(msgs, parts)
	if len(out) != 0 {
		t.Errorf("errored assistant with no content should be skipped, got %d messages", len(out))
	}
}

// ---- FilterCompacted ----

func TestFilterCompacted_noCompaction(t *testing.T) {
	msgs := []*store.Message{
		{ID: "m1", Role: store.RoleUser},
		{ID: "m2", Role: store.RoleAssistant},
		{ID: "m3", Role: store.RoleUser},
	}
	parts := map[string][]*store.Part{
		"m1": {{Type: store.PartTypeText, Data: &store.TextPartData{Text: "q1"}}},
		"m2": {},
		"m3": {{Type: store.PartTypeText, Data: &store.TextPartData{Text: "q2"}}},
	}
	out := session.FilterCompacted(msgs, parts)
	if len(out) != 3 {
		t.Errorf("no compaction: FilterCompacted returned %d, want 3", len(out))
	}
}

func TestFilterCompacted_withCompaction(t *testing.T) {
	// History: m1(user) m2(assistant) m3(user/compaction) m4(assistant/summary) m5(user) m6(assistant)
	msgs := []*store.Message{
		{ID: "m1", Role: store.RoleUser},
		{ID: "m2", Role: store.RoleAssistant},
		{ID: "m3", Role: store.RoleUser},
		{ID: "m4", Role: store.RoleAssistant, Summary: true},
		{ID: "m5", Role: store.RoleUser},
		{ID: "m6", Role: store.RoleAssistant},
	}
	parts := map[string][]*store.Part{
		"m1": {},
		"m2": {},
		"m3": {{Type: store.PartTypeCompaction, Data: &store.CompactionPartData{}}},
		"m4": {},
		"m5": {},
		"m6": {},
	}
	out := session.FilterCompacted(msgs, parts)
	// Should return: m3 (compaction user) + m4 (summary) + m5 + m6 (tail)
	if len(out) != 4 {
		ids := make([]string, len(out))
		for i, m := range out {
			ids[i] = m.ID
		}
		t.Errorf("FilterCompacted returned %d messages %v, want 4 [m3,m4,m5,m6]", len(out), ids)
	}
	if out[0].ID != "m3" || out[1].ID != "m4" {
		t.Errorf("first two messages should be m3,m4; got %s,%s", out[0].ID, out[1].ID)
	}
}

// ---- doom-loop detection ----

type countingTool struct {
	calls int
}

func (c *countingTool) Name() string        { return "noop" }
func (c *countingTool) Description() string { return "noop" }
func (c *countingTool) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (c *countingTool) Execute(_ context.Context, _ map[string]any) (tool.Result, error) {
	c.calls++
	return tool.Result{Output: "ok"}, nil
}

func TestDoomLoop_threshold(t *testing.T) {
	// Provide a provider that keeps calling the same tool with the same args
	// DoomLoopThreshold=3: on the 3rd identical call the processor should stop.
	sameInput := map[string]any{}
	const callID = "c1"

	// We'll serve 4 identical tool-call rounds; processor should stop at 3.
	eventsPerRound := func() []llm.Event {
		return []llm.Event{
			{Type: llm.EventToolInputStart, ToolName: "noop", ToolCallID: callID},
			{Type: llm.EventToolCall, ToolName: "noop", ToolCallID: callID, Input: sameInput},
			{Type: llm.EventStepFinish, FinishReason: llm.FinishReasonToolCalls},
		}
	}

	var allEvents []llm.Event
	allEvents = append(allEvents, llm.Event{Type: llm.EventRequestStart})
	allEvents = append(allEvents, llm.Event{Type: llm.EventStepStart})
	for i := 0; i < 4; i++ {
		allEvents = append(allEvents, eventsPerRound()...)
	}
	allEvents = append(allEvents, llm.Event{Type: llm.EventRequestFinish})

	prov := &mockProvider{id: "mock", events: allEvents}
	ct := &countingTool{}

	s := memory.New()
	ctx := context.Background()
	_ = s.CreateSession(ctx, &store.Session{ID: "doom"})

	result, _ := session.RunLoop(ctx, s, session.RunInput{
		SessionID:             "doom",
		UserMsg:               "go",
		Model:                 testModel(),
		Provider:              prov,
		Tools:                 []tool.Tool{ct},
		DisableProviderPrompt: true,
		MaxSteps:              10,
	})
	// Doom loop should cause ProcessStop
	if result != session.RunResultStop {
		t.Errorf("doom loop: result = %v, want Stop", result)
	}
	// Tool should have been called at most DoomLoopThreshold times
	if ct.calls > session.DoomLoopThreshold {
		t.Errorf("tool called %d times, doom loop should stop at %d", ct.calls, session.DoomLoopThreshold)
	}
}

// ---- RunLoop: simple round trip ----

func TestRunLoop_simpleResponse(t *testing.T) {
	s := memory.New()
	ctx := context.Background()
	_ = s.CreateSession(ctx, &store.Session{ID: "r1"})

	result, err := session.RunLoop(ctx, s, session.RunInput{
		SessionID:             "r1",
		UserMsg:               "hello",
		Model:                 testModel(),
		Provider:              simpleTextProvider("world"),
		DisableProviderPrompt: true,
		MaxSteps:              5,
	})
	if err != nil {
		t.Fatalf("RunLoop error: %v", err)
	}
	if result != session.RunResultContinue {
		t.Errorf("result = %v, want Continue", result)
	}

	// Verify messages were stored
	msgs, _ := s.ListMessages(ctx, "r1")
	if len(msgs) < 2 {
		t.Errorf("expected at least 2 messages (user + assistant), got %d", len(msgs))
	}
}

func TestRunLoop_maxSteps(t *testing.T) {
	// Provider returns tool-calls finish reason with an actual tool call part,
	// causing RunLoop to loop. MaxSteps should stop it.
	callID := "c-loop"
	loopProv := &mockProvider{
		id: "mock",
		events: []llm.Event{
			{Type: llm.EventRequestStart},
			{Type: llm.EventStepStart},
			{Type: llm.EventToolInputStart, ToolName: "noop", ToolCallID: callID},
			{Type: llm.EventToolCall, ToolName: "noop", ToolCallID: callID, Input: map[string]any{}},
			{Type: llm.EventStepFinish, FinishReason: llm.FinishReasonToolCalls},
			{Type: llm.EventRequestFinish, FinishReason: llm.FinishReasonToolCalls},
		},
	}

	loopTool := &countingTool{}
	s := memory.New()
	ctx := context.Background()
	_ = s.CreateSession(ctx, &store.Session{ID: "ms"})

	result, _ := session.RunLoop(ctx, s, session.RunInput{
		SessionID:             "ms",
		UserMsg:               "go",
		Model:                 testModel(),
		Provider:              loopProv,
		Tools:                 []tool.Tool{loopTool},
		DisableProviderPrompt: true,
		MaxSteps:              3,
	})
	if result != session.RunResultStop {
		t.Errorf("MaxSteps: result = %v, want Stop", result)
	}
}

func TestRunLoop_contextCancellation(t *testing.T) {
	// Provider blocks until context is cancelled
	blockProv := &blockingProvider{}
	s := memory.New()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_ = s.CreateSession(ctx, &store.Session{ID: "cc"})
	_, _ = session.RunLoop(ctx, s, session.RunInput{
		SessionID:             "cc",
		UserMsg:               "hi",
		Model:                 testModel(),
		Provider:              blockProv,
		DisableProviderPrompt: true,
	})
	// Should return without hanging
}

type blockingProvider struct{}

func (b *blockingProvider) ID() string { return "blocking" }
func (b *blockingProvider) Stream(ctx context.Context, _ llm.Request) (<-chan llm.Event, error) {
	ch := make(chan llm.Event)
	go func() {
		<-ctx.Done()
		ch <- llm.Event{Type: llm.EventError, Err: ctx.Err()}
		close(ch)
	}()
	return ch, nil
}

// ---- SystemPromptForModel ----

func TestSystemPromptForModel_claude(t *testing.T) {
	m := llm.Model{APIID: "claude-sonnet-4-5"}
	prompt := session.SystemPromptForModel(m)
	if prompt == "" {
		t.Error("claude model should have a non-empty provider prompt")
	}
}

func TestSystemPromptForModel_gpt4(t *testing.T) {
	m := llm.Model{APIID: "gpt-4o"}
	prompt := session.SystemPromptForModel(m)
	if prompt == "" {
		t.Error("gpt-4 model should have a non-empty provider prompt")
	}
}

func TestSystemPromptForModel_unknown(t *testing.T) {
	m := llm.Model{APIID: "some-unknown-model"}
	prompt := session.SystemPromptForModel(m)
	if prompt == "" {
		t.Error("unknown model should fall back to default prompt")
	}
}
