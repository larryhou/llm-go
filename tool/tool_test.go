package tool_test

import (
	"context"
	"strings"
	"testing"

	"github.com/larryhou/llm-go/tool"
)

// --- Truncate ---

func TestTruncate_withinLimits(t *testing.T) {
	text := "line1\nline2\nline3"
	r := tool.Truncate("test", text, nil)
	if r.Truncated {
		t.Error("should not truncate within limits")
	}
	if r.Content != text {
		t.Errorf("content mismatch: %q", r.Content)
	}
	if r.OutputPath != "" {
		t.Error("OutputPath should be empty when not truncated")
	}
}

func TestTruncate_byLineCount_head(t *testing.T) {
	// Build 10 lines, limit to 3
	var sb strings.Builder
	for i := 0; i < 10; i++ {
		sb.WriteString("line\n")
	}
	text := strings.TrimSuffix(sb.String(), "\n")

	r := tool.Truncate("test", text, &tool.TruncateOptions{MaxLines: 3, MaxBytes: 1<<30, Direction: "head"})
	if !r.Truncated {
		t.Fatal("should be truncated")
	}
	// Preview contains first 3 lines
	lines := strings.SplitN(r.Content, "\n", 4)
	if len(lines) < 3 {
		t.Fatalf("expected at least 3 lines in preview, got %d", len(lines))
	}
	if !strings.Contains(r.Content, "truncated") {
		t.Error("truncated content should contain truncation hint")
	}
	if r.OutputPath == "" {
		t.Error("truncated content should have OutputPath set")
	}
}

func TestTruncate_byLineCount_tail(t *testing.T) {
	lines := make([]string, 10)
	for i := range lines {
		lines[i] = "line"
	}
	text := strings.Join(lines, "\n")

	r := tool.Truncate("test", text, &tool.TruncateOptions{MaxLines: 3, MaxBytes: 1<<30, Direction: "tail"})
	if !r.Truncated {
		t.Fatal("should be truncated")
	}
	if !strings.Contains(r.Content, "truncated") {
		t.Error("should contain truncation hint")
	}
}

func TestTruncate_byByteCount(t *testing.T) {
	// 100 lines of "x" = ~200 bytes; limit to 50 bytes
	text := strings.Repeat("x\n", 50)
	r := tool.Truncate("test", text, &tool.TruncateOptions{MaxLines: 10000, MaxBytes: 50})
	if !r.Truncated {
		t.Fatal("should truncate by byte count")
	}
}

func TestTruncate_exactlyAtLimit_noTruncation(t *testing.T) {
	// Exactly 3 lines, limit=3 — should NOT truncate
	text := "a\nb\nc"
	r := tool.Truncate("test", text, &tool.TruncateOptions{MaxLines: 3, MaxBytes: 1 << 20})
	if r.Truncated {
		t.Error("exactly at limit should not truncate")
	}
}

func TestTruncate_emptyString(t *testing.T) {
	r := tool.Truncate("test", "", nil)
	if r.Truncated {
		t.Error("empty string should not truncate")
	}
}

// --- Registry ---

type mockTool struct{ name string }

func (m *mockTool) Name() string                  { return m.name }
func (m *mockTool) Description() string           { return "mock" }
func (m *mockTool) InputSchema() map[string]any   { return map[string]any{"type": "object"} }
func (m *mockTool) Execute(_ context.Context, _ map[string]any) (tool.Result, error) {
	return tool.Result{Output: "ok"}, nil
}

func TestRegistry_registerAndGet(t *testing.T) {
	r := tool.NewRegistry()
	r.Register(&mockTool{name: "foo"})

	got, ok := r.Get("foo")
	if !ok {
		t.Fatal("expected to find tool 'foo'")
	}
	if got.Name() != "foo" {
		t.Errorf("Name = %q, want foo", got.Name())
	}
}

func TestRegistry_missingTool(t *testing.T) {
	r := tool.NewRegistry()
	_, ok := r.Get("nonexistent")
	if ok {
		t.Error("expected not found for nonexistent tool")
	}
}

func TestRegistry_duplicatePanics(t *testing.T) {
	r := tool.NewRegistry()
	r.Register(&mockTool{name: "dup"})
	defer func() {
		if rec := recover(); rec == nil {
			t.Error("expected panic for duplicate tool name")
		}
	}()
	r.Register(&mockTool{name: "dup"})
}

func TestRegistry_all(t *testing.T) {
	r := tool.NewRegistry()
	r.Register(&mockTool{name: "a"})
	r.Register(&mockTool{name: "b"})
	r.Register(&mockTool{name: "c"})
	all := r.All()
	if len(all) != 3 {
		t.Errorf("All() = %d tools, want 3", len(all))
	}
}

func TestRegistry_filter(t *testing.T) {
	r := tool.NewRegistry()
	r.Register(&mockTool{name: "shell"})
	r.Register(&mockTool{name: "read"})
	r.Register(&mockTool{name: "write"})

	allowed := map[string]bool{"shell": true, "read": true, "write": false}
	filtered := r.Filter(allowed)
	if len(filtered) != 2 {
		t.Errorf("Filter() = %d tools, want 2", len(filtered))
	}
	for _, t2 := range filtered {
		if t2.Name() == "write" {
			t.Error("'write' should be filtered out (enabled=false)")
		}
	}
}

func TestRegistry_filterNil(t *testing.T) {
	r := tool.NewRegistry()
	r.Register(&mockTool{name: "a"})
	r.Register(&mockTool{name: "b"})
	// nil allowed = return all
	all := r.Filter(nil)
	if len(all) != 2 {
		t.Errorf("Filter(nil) = %d, want 2", len(all))
	}
}

// --- ToolFailure ---

func TestToolFailure(t *testing.T) {
	err := tool.Fail("something went wrong")
	if err == nil {
		t.Fatal("Fail should return non-nil error")
	}
	tf, ok := tool.IsToolFailure(err)
	if !ok {
		t.Fatal("expected ToolFailure")
	}
	if tf.Message != "something went wrong" {
		t.Errorf("Message = %q, want 'something went wrong'", tf.Message)
	}
}
