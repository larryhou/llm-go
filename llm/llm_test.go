package llm_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/larryhou/llm-go/config"
	"github.com/larryhou/llm-go/llm"
)

// --- overflow / usable ---

func TestMaxOutputTokens(t *testing.T) {
	cases := []struct {
		output int
		want   int
	}{
		{8192, 8192},
		{32000, 32000},
		{32001, 32000}, // capped at 32 000
		{65536, 32000},
		{0, 4096},  // unset → fallback to 4096 (avoids sending max_tokens=0 to provider)
	}
	for _, c := range cases {
		m := llm.Model{Limit: llm.ModelLimit{Output: c.output}}
		if got := llm.MaxOutputTokens(m); got != c.want {
			t.Errorf("MaxOutputTokens(output=%d) = %d, want %d", c.output, got, c.want)
		}
	}
}

func TestUsable_noInputLimit(t *testing.T) {
	// context=200_000, output=8192 → usable = 200_000 − 8192 = 191_808
	m := llm.Model{Limit: llm.ModelLimit{Context: 200_000, Output: 8_192}}
	got := llm.Usable(m, nil)
	want := 200_000 - 8_192
	if got != want {
		t.Errorf("Usable = %d, want %d", got, want)
	}
}

func TestUsable_withInputLimit(t *testing.T) {
	input := 180_000
	// reserved = min(20_000, MaxOutputTokens) = min(20_000,8192) = 8192
	// usable = 180_000 − 8192 = 171_808
	m := llm.Model{Limit: llm.ModelLimit{Context: 200_000, Input: &input, Output: 8_192}}
	got := llm.Usable(m, nil)
	want := 180_000 - 8_192
	if got != want {
		t.Errorf("Usable = %d, want %d", got, want)
	}
}

func TestUsable_zeroContext(t *testing.T) {
	m := llm.Model{Limit: llm.ModelLimit{Context: 0, Output: 8_192}}
	if got := llm.Usable(m, nil); got != 0 {
		t.Errorf("Usable(zero context) = %d, want 0", got)
	}
}

func TestIsOverflow(t *testing.T) {
	m := llm.Model{Limit: llm.ModelLimit{Context: 10_000, Output: 1_000}}
	// usable = 10_000 − 1_000 = 9_000

	cases := []struct {
		name  string
		usage llm.TokenUsage
		want  bool
	}{
		{"below limit", llm.TokenUsage{Input: 4000, Output: 1000}, false},
		{"exactly at limit", llm.TokenUsage{Total: 9000}, true},
		{"above limit", llm.TokenUsage{Total: 9001}, true},
		{"uses total field", llm.TokenUsage{Total: 5000, Input: 99999}, false}, // Total wins
		{"sums parts when no total", llm.TokenUsage{Input: 5000, Output: 4000}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := llm.IsOverflow(c.usage, m, nil); got != c.want {
				t.Errorf("IsOverflow = %v, want %v", got, c.want)
			}
		})
	}
}

func TestIsOverflow_autoDisabled(t *testing.T) {
	m := llm.Model{Limit: llm.ModelLimit{Context: 1000, Output: 100}}
	usage := llm.TokenUsage{Total: 999_999}
	autoFalse := false
	cfg := &config.Info{Compaction: &config.CompactionConfig{Auto: &autoFalse}}
	if llm.IsOverflow(usage, m, cfg) {
		t.Error("IsOverflow should be false when compaction.auto=false")
	}
}

func TestTokenUsage_Effective(t *testing.T) {
	// Total takes priority
	u := llm.TokenUsage{Total: 100, Input: 999}
	if u.Effective() != 100 {
		t.Errorf("Effective() = %d, want 100", u.Effective())
	}
	// Sum parts when Total==0
	u2 := llm.TokenUsage{Input: 10, Output: 20, CacheRead: 5, CacheWrite: 3}
	if u2.Effective() != 38 {
		t.Errorf("Effective() = %d, want 38", u2.Effective())
	}
}

// --- error classification ---

func TestClassifyHTTPError_overflow(t *testing.T) {
	overflowMessages := []string{
		"prompt is too long",
		"This input exceeds the context window for this model",
		"input token count exceeds the maximum allowed",
		"maximum prompt length is 100000",
		"reduce the length of the messages",
		"maximum context length is 4096 tokens",
		"exceeds the limit of 128000",
		"exceeds the available context size",
		"greater than the context length",
		"context window exceeds limit",
		"exceeded model token limit",
		"context_length_exceeded",
		"request entity too large",
		"context length is only 4096 tokens",
		"input length 99999 exceeds context length 8192",
		"prompt too long; exceeded max context length",
		"too large for model with 8192 maximum context length",
		"model_context_window_exceeded",
		"input is too long for requested model",
	}
	for _, msg := range overflowMessages {
		err := llm.ClassifyHTTPError("test", 400, msg, nil)
		if err.Kind != llm.ErrContextOverflow {
			t.Errorf("msg=%q: got Kind=%q, want %q", msg, err.Kind, llm.ErrContextOverflow)
		}
		if err.IsRetryable {
			t.Errorf("msg=%q: overflow should not be retryable", msg)
		}
	}
}

func TestClassifyHTTPError_status413(t *testing.T) {
	err := llm.ClassifyHTTPError("test", 413, "Payload Too Large", nil)
	if err.Kind != llm.ErrContextOverflow {
		t.Errorf("413 should classify as context_overflow, got %q", err.Kind)
	}
}

func TestClassifyHTTPError_emptyBodyPattern(t *testing.T) {
	// "400 (no body)" style — Cerebras/Mistral
	cases := []string{
		"400 (no body)",
		"413 (no body)",
		"400 status code (no body)",
	}
	for _, msg := range cases {
		err := llm.ClassifyHTTPError("test", 400, msg, nil)
		if err.Kind != llm.ErrContextOverflow {
			t.Errorf("msg=%q: got %q, want context_overflow", msg, err.Kind)
		}
	}
}

func TestClassifyHTTPError_auth(t *testing.T) {
	for _, code := range []int{401, 403} {
		err := llm.ClassifyHTTPError("test", code, "Unauthorized", nil)
		if err.Kind != llm.ErrAuth {
			t.Errorf("HTTP %d: got %q, want auth", code, err.Kind)
		}
		if err.IsRetryable {
			t.Errorf("HTTP %d: auth should not be retryable", code)
		}
	}
}

func TestClassifyHTTPError_rateLimit(t *testing.T) {
	err := llm.ClassifyHTTPError("test", 429, "Rate limit exceeded", nil)
	if err.Kind != llm.ErrRateLimit {
		t.Errorf("429: got %q, want rate_limit", err.Kind)
	}
	if !err.IsRetryable {
		t.Error("429 rate limit should be retryable")
	}
}

func TestClassifyHTTPError_quotaExceeded(t *testing.T) {
	err := llm.ClassifyHTTPError("test", 429, "quota exceeded for billing plan", nil)
	if err.Kind != llm.ErrQuotaExceeded {
		t.Errorf("quota body: got %q, want quota_exceeded", err.Kind)
	}
}

func TestClassifyHTTPError_serverError(t *testing.T) {
	for _, code := range []int{500, 502, 503, 529} {
		err := llm.ClassifyHTTPError("test", code, "Internal Server Error", nil)
		if err.Kind != llm.ErrProviderInternal {
			t.Errorf("HTTP %d: got %q, want provider_internal", code, err.Kind)
		}
		if !err.IsRetryable {
			t.Errorf("HTTP %d: 5xx should be retryable", code)
		}
	}
}

func TestClassifyHTTPError_htmlBody(t *testing.T) {
	err := llm.ClassifyHTTPError("test", 401, "<!DOCTYPE html><html>Access Denied</html>", nil)
	if err.Kind != llm.ErrAuth {
		t.Errorf("got %q, want auth", err.Kind)
	}
	if !strings.Contains(err.Message, "Unauthorized") {
		t.Errorf("HTML body should produce human-readable message, got: %q", err.Message)
	}
}

func TestClassifyStreamError(t *testing.T) {
	cases := []struct {
		errType string
		errCode string
		wantKind llm.ErrorKind
		wantRetry bool
	}{
		{"", "context_length_exceeded", llm.ErrContextOverflow, false},
		{"context_length_exceeded", "", llm.ErrContextOverflow, false},
		{"", "insufficient_quota", llm.ErrQuotaExceeded, false},
		{"", "usage_not_included", llm.ErrInvalidRequest, false},
		{"", "invalid_prompt", llm.ErrInvalidRequest, false},
		{"", "server_is_overloaded", llm.ErrProviderInternal, true},
		{"server_error", "", llm.ErrProviderInternal, true},
	}
	for _, c := range cases {
		err := llm.ClassifyStreamError(c.errType, c.errCode, "some message")
		if err == nil {
			t.Errorf("type=%q code=%q: got nil error", c.errType, c.errCode)
			continue
		}
		if err.Kind != c.wantKind {
			t.Errorf("type=%q code=%q: got Kind=%q, want %q", c.errType, c.errCode, err.Kind, c.wantKind)
		}
		if err.IsRetryable != c.wantRetry {
			t.Errorf("type=%q code=%q: IsRetryable=%v, want %v", c.errType, c.errCode, err.IsRetryable, c.wantRetry)
		}
	}
}

func TestShouldRetry(t *testing.T) {
	cases := []struct {
		err  *llm.LLMError
		want bool
	}{
		{nil, false},
		{&llm.LLMError{Kind: llm.ErrContextOverflow}, false},
		{&llm.LLMError{Kind: llm.ErrRateLimit, IsRetryable: true}, true},
		{&llm.LLMError{Kind: llm.ErrProviderInternal, StatusCode: 500}, true},
		{&llm.LLMError{Kind: llm.ErrAuth, StatusCode: 401}, false},
		// 5xx always retryable regardless of IsRetryable flag
		{&llm.LLMError{Kind: llm.ErrUnknown, StatusCode: 503, IsRetryable: false}, true},
	}
	for _, c := range cases {
		if got := llm.ShouldRetry(c.err); got != c.want {
			t.Errorf("ShouldRetry(%+v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// --- RetryDelay ---

func TestRetryDelay_retryAfterMS(t *testing.T) {
	h := http.Header{"Retry-After-Ms": []string{"1500"}}
	d := llm.RetryDelay(1, h)
	if d != 1500*time.Millisecond {
		t.Errorf("got %v, want 1500ms", d)
	}
}

func TestRetryDelay_retryAfterSeconds(t *testing.T) {
	h := http.Header{"Retry-After": []string{"3"}}
	d := llm.RetryDelay(1, h)
	if d != 3*time.Second {
		t.Errorf("got %v, want 3s", d)
	}
}

func TestRetryDelay_exponentialBackoff(t *testing.T) {
	// attempt 1 → 2s, attempt 2 → 4s, attempt 3 → 8s, attempt 4 → 16s, attempt 5 → 30s (capped)
	want := []time.Duration{
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second, // capped
	}
	for i, w := range want {
		d := llm.RetryDelay(i+1, nil)
		if d != w {
			t.Errorf("attempt %d: got %v, want %v", i+1, d, w)
		}
	}
}

// --- message construction helpers ---

func TestNewTextPart(t *testing.T) {
	p := llm.NewTextPart("hello")
	if p.Type != llm.PartTypeText || p.Text != "hello" {
		t.Errorf("unexpected part: %+v", p)
	}
}

func TestNewToolCallPart(t *testing.T) {
	p := llm.NewToolCallPart("id1", "my_tool", map[string]any{"k": "v"})
	if p.Type != llm.PartTypeToolCall || p.ToolCallID != "id1" || p.ToolName != "my_tool" {
		t.Errorf("unexpected part: %+v", p)
	}
}

func TestNewToolResult(t *testing.T) {
	r := llm.NewTextResult("ok")
	if r.Type != llm.ToolResultTypeText || r.Value != "ok" {
		t.Errorf("unexpected result: %+v", r)
	}
	e := llm.NewErrorResult("oops")
	if e.Type != llm.ToolResultTypeError {
		t.Errorf("unexpected error result: %+v", e)
	}
}

func TestTokenUsage_Add(t *testing.T) {
	a := llm.TokenUsage{Input: 10, Output: 5, CacheRead: 2, CacheWrite: 1}
	b := llm.TokenUsage{Input: 20, Output: 10, CacheRead: 3, CacheWrite: 4}
	sum := a.Add(b)
	if sum.Input != 30 || sum.Output != 15 || sum.CacheRead != 5 || sum.CacheWrite != 5 {
		t.Errorf("Add result: %+v", sum)
	}
}

// --- helpers ---

// init verifies overflow constant alignment at compile time.
func init() {
	_ = llm.CompactionBuffer        // 20_000
	_ = llm.MinPreserveRecentTokens // 2_000
	_ = llm.MaxPreserveRecentTokens // 8_000
}
