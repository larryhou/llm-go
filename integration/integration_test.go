// Package integration contains end-to-end tests against a real OpenAI-compatible API.
//
// Run with the TIMI_API_KEY and TIMI_BASE_URL environment variables set, or
// use the predefined defaults which point at the local test proxy.
//
//	go test ./integration/ -v -count=1
//
// The tests are skipped automatically when the endpoint is unreachable or the
// env var LLM_INTEGRATION=1 is not set, so they never break CI.
package integration

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/larryhou/llm-go/llm"
	openaiProv "github.com/larryhou/llm-go/provider/openai"
	"github.com/larryhou/llm-go/session"
	"github.com/larryhou/llm-go/store"
	"github.com/larryhou/llm-go/store/memory"
	"github.com/larryhou/llm-go/tool"
)

const (
	defaultBaseURL = "http://192.168.3.119:8080/timi-claude/v1"
	defaultAPIKey  = "sk-zzz6FtyLMyuobNNOukwgobP0l1F3TjMO"
	defaultModel   = "claude-sonnet-4.6"
)

// setup returns a configured provider and model, or skips the test if
// the endpoint is unreachable or integration tests are disabled.
func setup(t *testing.T) (llm.Provider, llm.Model) {
	t.Helper()

	if os.Getenv("LLM_INTEGRATION") != "1" {
		t.Skip("set LLM_INTEGRATION=1 to run integration tests")
	}

	baseURL := os.Getenv("TIMI_BASE_URL")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	apiKey := os.Getenv("TIMI_API_KEY")
	if apiKey == "" {
		apiKey = defaultAPIKey
	}
	modelID := os.Getenv("TIMI_MODEL")
	if modelID == "" {
		modelID = defaultModel
	}

	// Quick reachability check: list models endpoint (1 s timeout).
	// The timi proxy exposes models at /chat/completions/models relative to baseURL.
	c := &http.Client{Timeout: time.Second}
	req, _ := http.NewRequest("GET", baseURL+"/chat/completions/models", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := c.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Skipf("API endpoint %s not reachable (status=%v err=%v)", baseURL, func() int {
			if resp != nil {
				return resp.StatusCode
			}
			return 0
		}(), err)
	}
	resp.Body.Close()

	prov := openaiProv.New(apiKey, baseURL, "timi", nil)
	model := llm.Model{
		ID:         modelID,
		ProviderID: "timi",
		APIID:      modelID,
		Limit:      llm.ModelLimit{Context: 200_000, Output: 8_192},
	}
	return prov, model
}

// TestIntegration_SimpleStream verifies that the provider streams text
// back correctly for a trivial prompt.
func TestIntegration_SimpleStream(t *testing.T) {
	prov, model := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client := llm.NewClient(prov)
	req := llm.Request{
		Model:   model,
		System:  []string{"You are a concise assistant."},
		Messages: []llm.Message{llm.NewUserMessage("Reply with exactly: HELLO")},
		Options:  llm.GenerationOptions{MaxTokens: 20},
	}

	var textBuf strings.Builder
	var usage llm.TokenUsage
	var finishReason llm.FinishReason

	for ev := range client.Stream(ctx, req) {
		switch ev.Type {
		case llm.EventTextDelta:
			textBuf.WriteString(ev.Text)
		case llm.EventStepFinish:
			usage = ev.Usage
			finishReason = ev.FinishReason
		case llm.EventError:
			t.Fatalf("stream error: %v", ev.Err)
		}
	}

	text := strings.TrimSpace(textBuf.String())
	t.Logf("response text: %q", text)
	t.Logf("usage: input=%d output=%d total=%d", usage.Input, usage.Output, usage.Total)

	if text == "" {
		t.Error("expected non-empty response text")
	}
	if finishReason != llm.FinishReasonStop {
		t.Errorf("finish_reason = %q, want stop", finishReason)
	}
	// Some proxies don't return usage in the stream — log a warning rather than fail.
	if usage.Effective() == 0 {
		t.Log("WARNING: provider did not return token usage in stream (stream_options.include_usage not supported)")
	}
}

// TestIntegration_ToolCall verifies that the provider correctly emits
// tool-call events and that the RunLoop executes the tool and gets
// a final answer.
func TestIntegration_ToolCall(t *testing.T) {
	prov, model := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// A simple calculator tool
	calc := &calcTool{}

	s := memory.New()
	_ = s.CreateSession(ctx, &store.Session{ID: "integ-tool", Model: model.ProviderID + "/" + model.ID})

	result, err := session.RunLoop(ctx, s, session.RunInput{
		SessionID:             "integ-tool",
		UserMsg:               "Use the calc tool to compute 123 * 456. Return only the numeric result.",
		Model:                 model,
		Provider:              prov,
		Tools:                 []tool.Tool{calc},
		DisableProviderPrompt: true,
		ExtraSystem:           []string{"You are a precise assistant. Always use tools when asked."},
		MaxSteps:              5,
	})
	if err != nil {
		t.Fatalf("RunLoop error: %v", err)
	}

	t.Logf("RunLoop result: %v", result)
	t.Logf("calc tool called %d time(s)", calc.calls)

	if calc.calls == 0 {
		t.Error("expected calc tool to be called at least once")
	}

	// Inspect stored messages
	msgs, _ := s.ListMessages(ctx, "integ-tool")
	t.Logf("stored %d messages", len(msgs))
	for _, m := range msgs {
		parts, _ := s.ListParts(ctx, m.ID)
		for _, p := range parts {
			if p.Type == store.PartTypeTool {
				if d, ok := p.Data.(*store.ToolPartData); ok {
					t.Logf("  tool=%s status=%s output=%q", d.Tool, d.Status, d.Output)
					if d.Status != store.ToolStatusCompleted {
						t.Errorf("tool part status = %q, want completed", d.Status)
					}
					if !strings.Contains(d.Output, "56088") {
						t.Errorf("calc output = %q, want to contain 56088", d.Output)
					}
				}
			}
			if p.Type == store.PartTypeText {
				if d, ok := p.Data.(*store.TextPartData); ok && d.Text != "" {
					t.Logf("  text: %q", d.Text)
				}
			}
		}
	}
}

// TestIntegration_MultiTurn verifies that conversation history is correctly
// sent across multiple RunLoop calls on the same session.
func TestIntegration_MultiTurn(t *testing.T) {
	prov, model := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	s := memory.New()
	sessID := "integ-multiturn"
	_ = s.CreateSession(ctx, &store.Session{ID: sessID, Model: model.ProviderID + "/" + model.ID})

	runInput := func(msg string) session.RunInput {
		return session.RunInput{
			SessionID:             sessID,
			UserMsg:               msg,
			Model:                 model,
			Provider:              prov,
			DisableProviderPrompt: true,
			ExtraSystem:           []string{"You are a concise assistant. Remember what the user tells you."},
			MaxSteps:              3,
		}
	}

	// Turn 1: give the model a number to remember
	if _, err := session.RunLoop(ctx, s, runInput("Remember the number 7331. Acknowledge with OK.")); err != nil {
		t.Fatalf("turn 1 error: %v", err)
	}

	// Turn 2: ask it to recall
	if _, err := session.RunLoop(ctx, s, runInput("What number did I ask you to remember? Reply with just the number.")); err != nil {
		t.Fatalf("turn 2 error: %v", err)
	}

	// Find the last assistant text part
	msgs, _ := s.ListMessages(ctx, sessID)
	lastText := ""
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != store.RoleAssistant {
			continue
		}
		parts, _ := s.ListParts(ctx, msgs[i].ID)
		for _, p := range parts {
			if p.Type == store.PartTypeText {
				if d, ok := p.Data.(*store.TextPartData); ok && d.Text != "" {
					lastText = d.Text
					break
				}
			}
		}
		if lastText != "" {
			break
		}
	}

	t.Logf("last assistant response: %q", lastText)
	if !strings.Contains(lastText, "7331") {
		t.Errorf("model did not recall the number 7331 from history; got: %q", lastText)
	}
}

// TestIntegration_ContextOverflow verifies that IsOverflow correctly detects
// when real provider-reported usage exceeds a model's context limit.
func TestIntegration_ContextOverflow(t *testing.T) {
	prov, model := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client := llm.NewClient(prov)
	req := llm.Request{
		Model:    model,
		Messages: []llm.Message{llm.NewUserMessage("Say hi.")},
		Options:  llm.GenerationOptions{MaxTokens: 10},
	}

	var gotUsage llm.TokenUsage
	for ev := range client.Stream(ctx, req) {
		if ev.Type == llm.EventStepFinish {
			gotUsage = ev.Usage
		}
		if ev.Type == llm.EventError {
			t.Fatalf("stream error: %v", ev.Err)
		}
	}

	t.Logf("real usage from provider: input=%d output=%d total=%d",
		gotUsage.Input, gotUsage.Output, gotUsage.Total)

	// Should have real token counts now (include_usage is supported).
	if gotUsage.Effective() == 0 {
		t.Error("expected non-zero token usage from provider (include_usage not working?)")
	}

	// IsOverflow with a tiny model limit — real usage must exceed it.
	// context=50, output=10 → usable = 50−10 = 40; real usage is well above 40.
	tinyModel := model
	tinyModel.Limit = llm.ModelLimit{Context: 50, Output: 10}
	if !llm.IsOverflow(gotUsage, tinyModel, nil) {
		t.Errorf("IsOverflow should be true for context=50 with real usage %+v", gotUsage)
	}

	// Normal 200K model should NOT overflow.
	if llm.IsOverflow(gotUsage, model, nil) {
		t.Errorf("IsOverflow should be false for full model with usage %+v", gotUsage)
	}

	t.Logf("PASS: IsOverflow correctly detects real usage=%d vs tiny limit=%d / full limit=%d",
		gotUsage.Effective(), llm.Usable(tinyModel, nil), llm.Usable(model, nil))
}

// TestIntegration_ErrorClassification verifies that a bad API key produces
// an auth error via ClassifyHTTPError.
func TestIntegration_ErrorClassification(t *testing.T) {
	if os.Getenv("LLM_INTEGRATION") != "1" {
		t.Skip("set LLM_INTEGRATION=1 to run integration tests")
	}

	baseURL := defaultBaseURL
	badProv := openaiProv.New("invalid-key", baseURL, "timi-bad", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client := llm.NewClient(badProv)
	req := llm.Request{
		Model:    llm.Model{APIID: defaultModel, Limit: llm.ModelLimit{Context: 200_000, Output: 100}},
		Messages: []llm.Message{llm.NewUserMessage("hi")},
		Options:  llm.GenerationOptions{MaxTokens: 5},
	}

	var gotErr error
	for ev := range client.Stream(ctx, req) {
		if ev.Type == llm.EventError {
			gotErr = ev.Err
		}
	}

	if gotErr == nil {
		t.Skip("expected an auth error but got none — endpoint may accept any key")
	}

	llmErr, ok := llm.AsLLMError(gotErr)
	if !ok {
		t.Logf("got non-LLMError: %v (%T)", gotErr, gotErr)
		return
	}
	t.Logf("error kind=%q status=%d message=%q", llmErr.Kind, llmErr.StatusCode, llmErr.Message)
	if llmErr.Kind != llm.ErrAuth && llmErr.Kind != llm.ErrInvalidRequest {
		t.Errorf("bad key: expected auth or invalid_request error, got %q", llmErr.Kind)
	}
}

// --- calcTool ---

type calcTool struct{ calls int }

func (c *calcTool) Name() string        { return "calc" }
func (c *calcTool) Description() string { return "Evaluate a simple arithmetic expression" }
func (c *calcTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"expr": map[string]any{
				"type":        "string",
				"description": "arithmetic expression to evaluate, e.g. '2+2'",
			},
		},
		"required": []string{"expr"},
	}
}

func (c *calcTool) Execute(_ context.Context, input map[string]any) (tool.Result, error) {
	c.calls++
	expr, _ := input["expr"].(string)
	result := evalSimple(expr)
	return tool.Result{
		Output: result,
		Title:  "calc: " + expr,
	}, nil
}

// evalSimple evaluates very basic integer arithmetic (+ - * /) without eval.
func evalSimple(expr string) string {
	expr = strings.ReplaceAll(expr, " ", "")
	for _, op := range []string{"*", "/", "+", "-"} {
		idx := strings.LastIndex(expr, op)
		if idx <= 0 {
			continue
		}
		a, b := expr[:idx], expr[idx+1:]
		var x, y int
		if _, err := parseInts(a, b, &x, &y); err != nil {
			continue
		}
		switch op {
		case "+":
			return itoa(x + y)
		case "-":
			return itoa(x - y)
		case "*":
			return itoa(x * y)
		case "/":
			if y == 0 {
				return "division by zero"
			}
			return itoa(x / y)
		}
	}
	return expr // fallback: return as-is
}

func parseInts(a, b string, x, y *int) (int, error) {
	var err error
	*x, err = atoi(a)
	if err != nil {
		return 0, err
	}
	*y, err = atoi(b)
	return 0, err
}

func atoi(s string) (int, error) {
	n := 0
	neg := false
	for i, c := range s {
		if i == 0 && c == '-' {
			neg = true
			continue
		}
		if c < '0' || c > '9' {
			return 0, &parseError{s}
		}
		n = n*10 + int(c-'0')
	}
	if neg {
		return -n, nil
	}
	return n, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := make([]byte, 0, 20)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}

type parseError struct{ s string }

func (e *parseError) Error() string { return "cannot parse: " + e.s }
