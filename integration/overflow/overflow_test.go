// Package overflow contains an end-to-end integration test that deliberately
// drives a session to context-window overflow and then verifies every expected
// boundary behaviour using REAL token usage from the LLM provider:
//
//  1. Multi-turn conversation with tool calls (each turn accumulates real history)
//  2. Real token counts grow until IsOverflow triggers (ProcessCompact)
//  3. Compaction runs: head summarised, tail preserved, summary stored (summary=true)
//  4. FilterCompacted correctly hides pre-compaction history
//  5. Session continues normally after compaction with fresh token counts
//
// The test uses a small artificial context limit so that real usage from the
// timi-claude proxy overflows it after just 2-3 turns.
//
// Run with:
//
//	LLM_INTEGRATION=1 go test ./integration/overflow/ -v -count=1 -timeout=300s
package overflow

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/larryhou/llm-go/config"
	"github.com/larryhou/llm-go/llm"
	openaiProv "github.com/larryhou/llm-go/provider/openai"
	"github.com/larryhou/llm-go/session"
	"github.com/larryhou/llm-go/store"
	"github.com/larryhou/llm-go/store/memory"
	"github.com/larryhou/llm-go/tool"
)

// ── constants ─────────────────────────────────────────────────────────────────

const (
	defaultBaseURL = "http://192.168.3.119:8080/timi-claude/v1"
	defaultAPIKey  = "sk-zzz6FtyLMyuobNNOukwgobP0l1F3TjMO"
	defaultModel   = "claude-sonnet-4.6"

	// Context window limit — intentionally small to trigger overflow quickly.
	//
	// Empirical token counts from the proxy (with stream_options.include_usage):
	//   turn 1 (system+tool schema+user+tool call+result+reply) ≈ 650-700 total tokens
	//   turn 2 (same pattern with history)                      ≈ 1100-1300 total tokens
	//   turn 3 (with 2 turns of history)                        ≈ 1700-2000 total tokens
	//
	// With testContextLimit=2000 and testOutputLimit=300:
	//   usable = 2000 - min(20000, 300) = 1700 tokens
	//
	// Turn 1 total ≈ 680   → 680  < 1700 → no overflow
	// Turn 2 total ≈ 1200  → 1200 < 1700 → no overflow
	// Turn 3 total ≈ 1800  → 1800 > 1700 → OVERFLOW → compaction
	// Turn 4 (post-compact) starts fresh → should complete normally
	testContextLimit = 2000
	testOutputLimit  = 300
)

// ── test entry point ──────────────────────────────────────────────────────────

func TestOverflow_EndToEnd(t *testing.T) {
	if os.Getenv("LLM_INTEGRATION") != "1" {
		t.Skip("set LLM_INTEGRATION=1 to run integration tests")
	}

	baseURL, apiKey, modelID := resolveEnv()
	if !reachable(t, baseURL, apiKey) {
		return
	}

	prov := openaiProv.New(apiKey, baseURL, "timi", nil)
	model := llm.Model{
		ID:         modelID,
		ProviderID: "timi",
		APIID:      modelID,
		Limit: llm.ModelLimit{
			Context: testContextLimit,
			Output:  testOutputLimit,
		},
	}

	t.Logf("model context limit: %d tokens, output limit: %d tokens", testContextLimit, testOutputLimit)
	t.Logf("usable tokens before overflow: %d", llm.Usable(model, nil))
	t.Log("")

	// Config: tail_turns=1 so compaction keeps only the last user turn verbatim,
	// maximising the head available for summarisation.
	autoTrue := true
	cfg := &config.Info{
		Compaction: &config.CompactionConfig{
			Auto:      &autoTrue,
			TailTurns: intPtr(1),
		},
	}

	s := memory.New()
	ctx := context.Background()
	sessID := "overflow-e2e"
	_ = s.CreateSession(ctx, &store.Session{
		ID:    sessID,
		Model: model.ProviderID + "/" + model.ID,
	})

	counter := &counterTool{}

	t.Log("=== Phase 1: multi-turn conversation with tool calls ===")
	t.Log("Real token usage accumulates until IsOverflow fires naturally.")
	t.Log("")

	var (
		compactionFired      bool
		turnsBeforeCompaction int
		turnsAfterCompaction  int
		lastResult            session.RunResult
		totalUsage            []llm.TokenUsage // usage per turn for logging
	)

	// usageTrackingProvider wraps the real provider and captures usage per turn.
	tracked := &usageTrackingProvider{inner: prov}

	for turn := 1; turn <= 15; turn++ {
		userMsg := fmt.Sprintf(
			"Turn %d: Call the counter tool with n=%d and confirm the new counter value.",
			turn, turn)

		t.Logf("--- turn %d ---", turn)

		result, err := session.RunLoop(ctx, s, session.RunInput{
			SessionID:             sessID,
			UserMsg:               userMsg,
			Model:                 model,
			Provider:              tracked,
			SummaryProvider:       prov, // plain provider for compaction (no tracking)
			Tools:                 []tool.Tool{counter},
			Config:                cfg,
			DisableProviderPrompt: true,
			ExtraSystem:           []string{"You are a helpful assistant. Always use the counter tool when asked."},
			MaxSteps:              6,
		})
		if err != nil {
			t.Fatalf("turn %d: RunLoop error: %v", turn, err)
		}
		lastResult = result

		lastUsage := tracked.lastUsage()
		totalUsage = append(totalUsage, lastUsage)
		t.Logf("    result=%v  usage(input=%d output=%d total=%d)  counter calls=%d",
			result, lastUsage.Input, lastUsage.Output, lastUsage.Effective(), counter.totalCalls)

		if !compactionFired {
			msgs, _ := s.ListMessages(ctx, sessID)
			for _, m := range msgs {
				if m.Summary {
					compactionFired = true
					turnsBeforeCompaction = turn
					t.Logf(">>> COMPACTION FIRED at turn %d  (summary msgID=%s)", turn, m.ID)
					break
				}
			}
		} else {
			turnsAfterCompaction++
			if turnsAfterCompaction >= 2 {
				break
			}
		}
	}

	t.Log("")
	t.Log("=== Phase 2: assertions ===")

	allMsgs, _ := s.ListMessages(ctx, sessID)
	allParts := loadAllParts(ctx, t, s, allMsgs)

	// ── 1. compaction fired ───────────────────────────────────────────────────
	if !compactionFired {
		t.Fatal("FAIL: compaction never fired — context limit may be too large or turns too few")
	}
	t.Logf("PASS: compaction fired after turn %d", turnsBeforeCompaction)

	// ── 2. session continued after compaction ─────────────────────────────────
	if turnsAfterCompaction == 0 {
		t.Error("FAIL: no turns completed after compaction")
	} else {
		t.Logf("PASS: %d turn(s) completed after compaction (result=%v)", turnsAfterCompaction, lastResult)
	}

	// ── 3. counter tool was actually called ───────────────────────────────────
	if counter.totalCalls == 0 {
		t.Error("FAIL: counter tool was never called")
	} else {
		t.Logf("PASS: counter tool called %d time(s) total", counter.totalCalls)
	}

	// ── 4. summary message exists with summary=true and real content ──────────
	var summaryMsgs []*store.Message
	for _, m := range allMsgs {
		if m.Summary {
			summaryMsgs = append(summaryMsgs, m)
		}
	}
	if len(summaryMsgs) == 0 {
		t.Error("FAIL: no summary=true message found in store")
	} else {
		t.Logf("PASS: %d compaction summary message(s) in store", len(summaryMsgs))
		for _, sm := range summaryMsgs {
			parts := allParts[sm.ID]
			for _, p := range parts {
				if p.Type == store.PartTypeText {
					if d, ok := p.Data.(*store.TextPartData); ok && d.Text != "" {
						t.Logf("  summary (%d chars): %s...", len(d.Text), truncate(d.Text, 200))
					}
				}
			}
		}
	}

	// ── 5. compaction boundary user message exists ────────────────────────────
	var compactionBoundaryMsgs []*store.Message
	for _, m := range allMsgs {
		if m.Role != store.RoleUser {
			continue
		}
		ps := allParts[m.ID]
		for _, p := range ps {
			if p.Type == store.PartTypeCompaction {
				compactionBoundaryMsgs = append(compactionBoundaryMsgs, m)
				break
			}
		}
	}
	if len(compactionBoundaryMsgs) == 0 {
		t.Error("FAIL: no compaction boundary user message found")
	} else {
		t.Logf("PASS: %d compaction boundary message(s) found", len(compactionBoundaryMsgs))
	}

	// ── 6. FilterCompacted reduces visible history ────────────────────────────
	filtered := session.FilterCompacted(allMsgs, allParts)
	t.Logf("Message counts: total=%d  filtered=%d", len(allMsgs), len(filtered))

	if len(summaryMsgs) > 0 && len(filtered) >= len(allMsgs) {
		t.Error("FAIL: FilterCompacted did not reduce message count despite compaction boundary")
	} else if len(summaryMsgs) > 0 {
		t.Logf("PASS: FilterCompacted reduced history from %d to %d messages", len(allMsgs), len(filtered))
	}

	// Verify filtered set contains the summary and boundary
	summaryInFiltered := false
	boundaryInFiltered := false
	for _, m := range filtered {
		if m.Summary {
			summaryInFiltered = true
		}
		ps := allParts[m.ID]
		for _, p := range ps {
			if p.Type == store.PartTypeCompaction {
				boundaryInFiltered = true
			}
		}
	}
	if !summaryInFiltered {
		t.Error("FAIL: summary message not in FilterCompacted output")
	} else {
		t.Log("PASS: summary message present in filtered history")
	}
	if !boundaryInFiltered {
		t.Error("FAIL: compaction boundary not in FilterCompacted output")
	} else {
		t.Log("PASS: compaction boundary present in filtered history")
	}

	// ── 7. all tool parts are completed (no stuck pending/running) ────────────
	completedTools := 0
	stuckTools := 0
	for _, m := range allMsgs {
		for _, p := range allParts[m.ID] {
			if p.Type != store.PartTypeTool {
				continue
			}
			d, ok := p.Data.(*store.ToolPartData)
			if !ok {
				continue
			}
			switch d.Status {
			case store.ToolStatusCompleted:
				completedTools++
			case store.ToolStatusPending, store.ToolStatusRunning:
				stuckTools++
				t.Errorf("FAIL: tool part stuck in status=%s (tool=%s callID=%s)", d.Status, d.Tool, d.CallID)
			}
		}
	}
	if completedTools == 0 {
		t.Error("FAIL: no completed tool parts")
	} else {
		t.Logf("PASS: %d completed tool parts, %d stuck", completedTools, stuckTools)
	}

	// ── 8. post-compaction usage does not overflow the full model ─────────────
	// After compaction, the LLM only sees the summary + tail, so token count
	// should be well within the real 200K context of the underlying model.
	realModel := model
	realModel.Limit = llm.ModelLimit{Context: 200_000, Output: 8_192}
	lastUsage := tracked.lastUsage()
	if llm.IsOverflow(lastUsage, realModel, nil) {
		t.Errorf("FAIL: post-compaction usage %+v overflows real 200K model", lastUsage)
	} else {
		t.Logf("PASS: post-compaction usage (total=%d) is within real 200K model limit",
			lastUsage.Effective())
	}

	// ── summary ───────────────────────────────────────────────────────────────
	t.Log("")
	t.Log("=== Summary ===")
	t.Logf("Turns: %d before compaction, %d after", turnsBeforeCompaction, turnsAfterCompaction)
	t.Logf("Counter calls: %d", counter.totalCalls)
	t.Logf("Summary messages: %d", len(summaryMsgs))
	t.Logf("Messages: total=%d  filtered=%d", len(allMsgs), len(filtered))
	t.Logf("Usable context limit: %d tokens", llm.Usable(model, nil))
	for i, u := range totalUsage {
		t.Logf("  turn %d usage: input=%d output=%d total=%d", i+1, u.Input, u.Output, u.Effective())
	}
}

// ── usageTrackingProvider ─────────────────────────────────────────────────────

// usageTrackingProvider wraps a real provider and records the most recent
// StepFinish usage. This lets the test log real token counts per turn.
type usageTrackingProvider struct {
	inner  llm.Provider
	latest llm.TokenUsage
}

func (p *usageTrackingProvider) ID() string { return p.inner.ID() }

func (p *usageTrackingProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	inner, err := p.inner.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	out := make(chan llm.Event, 64)
	go func() {
		defer close(out)
		for ev := range inner {
			if ev.Type == llm.EventStepFinish && ev.Usage.Effective() > 0 {
				p.latest = ev.Usage
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func (p *usageTrackingProvider) lastUsage() llm.TokenUsage { return p.latest }

// ── counterTool ───────────────────────────────────────────────────────────────

type counterTool struct {
	totalCalls int
	value      int
}

func (c *counterTool) Name() string        { return "counter" }
func (c *counterTool) Description() string { return "Increments a counter by n and returns the new value" }
func (c *counterTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"n": map[string]any{
				"type":        "integer",
				"description": "amount to add to the counter",
			},
		},
		"required": []string{"n"},
	}
}

func (c *counterTool) Execute(_ context.Context, input map[string]any) (tool.Result, error) {
	c.totalCalls++
	n := 1
	if v, ok := input["n"].(float64); ok {
		n = int(v)
	}
	c.value += n
	return tool.Result{
		Output: fmt.Sprintf("Counter incremented by %d. New value: %d. Total calls: %d.",
			n, c.value, c.totalCalls),
		Title:    fmt.Sprintf("counter=%d", c.value),
		Metadata: map[string]any{"calls": c.totalCalls, "value": c.value},
	}, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func intPtr(n int) *int { return &n }

func resolveEnv() (baseURL, apiKey, modelID string) {
	baseURL = os.Getenv("TIMI_BASE_URL")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	apiKey = os.Getenv("TIMI_API_KEY")
	if apiKey == "" {
		apiKey = defaultAPIKey
	}
	modelID = os.Getenv("TIMI_MODEL")
	if modelID == "" {
		modelID = defaultModel
	}
	return
}

func reachable(t *testing.T, baseURL, apiKey string) bool {
	t.Helper()
	req, _ := http.NewRequest("GET", baseURL+"/chat/completions/models", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Skipf("API endpoint %s not reachable (err=%v)", baseURL, err)
		return false
	}
	resp.Body.Close()
	return true
}

func loadAllParts(ctx context.Context, t *testing.T, s store.Store, msgs []*store.Message) map[string][]*store.Part {
	t.Helper()
	allParts := make(map[string][]*store.Part, len(msgs))
	for _, m := range msgs {
		ps, err := s.ListParts(ctx, m.ID)
		if err != nil {
			t.Fatalf("ListParts(%s): %v", m.ID, err)
		}
		allParts[m.ID] = ps
	}
	return allParts
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n]
}
