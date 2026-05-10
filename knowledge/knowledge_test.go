package knowledge_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/larryhou/llm-go/knowledge"
)

// ── stub source ───────────────────────────────────────────────────────────────

type stubSource struct {
	id       string
	priority int
	accepts  func(knowledge.Query) bool
	peekFn   func(context.Context, knowledge.Query) ([]knowledge.Result, error)
	fetchFn  func(context.Context, knowledge.Query) ([]knowledge.Result, error)
}

func (s *stubSource) ID() string       { return s.id }
func (s *stubSource) Priority() int    { return s.priority }
func (s *stubSource) Accepts(q knowledge.Query) bool {
	if s.accepts != nil {
		return s.accepts(q)
	}
	return true
}
func (s *stubSource) Peek(ctx context.Context, q knowledge.Query) ([]knowledge.Result, error) {
	if s.peekFn != nil {
		return s.peekFn(ctx, q)
	}
	return nil, nil
}
func (s *stubSource) Fetch(ctx context.Context, q knowledge.Query) ([]knowledge.Result, error) {
	if s.fetchFn != nil {
		return s.fetchFn(ctx, q)
	}
	return nil, nil
}

func stubResult(sourceID, docID, title, snippet string, score float64) knowledge.Result {
	return knowledge.Result{
		RefID:   sourceID + ":" + docID,
		Title:   title,
		Source:  sourceID,
		Score:   score,
		Snippet: snippet,
	}
}

func newManager(cfg knowledge.ManagerConfig, sources ...knowledge.Source) *knowledge.Manager {
	m := knowledge.NewManager(cfg)
	for _, s := range sources {
		m.Register(s)
	}
	return m
}

// ── Manager.Register ──────────────────────────────────────────────────────────

func TestManager_RegisterSortsByPriority(t *testing.T) {
	// Register in reverse priority order; tools should still work correctly
	// because Manager sorts internally.
	s0 := &stubSource{id: "low", priority: 10}
	s1 := &stubSource{id: "high", priority: 0}

	called := []string{}
	s0.peekFn = func(_ context.Context, _ knowledge.Query) ([]knowledge.Result, error) {
		called = append(called, "low")
		return []knowledge.Result{stubResult("low", "1", "Low", "low snippet", 0.3)}, nil
	}
	s1.peekFn = func(_ context.Context, _ knowledge.Query) ([]knowledge.Result, error) {
		called = append(called, "high")
		return []knowledge.Result{stubResult("high", "1", "High", "high snippet", 0.9)}, nil
	}

	m := newManager(knowledge.ManagerConfig{MaxResults: 10, AllowPartialFailure: true}, s0, s1)
	tools := m.Tools()
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
}

// ── Peek / search tool ────────────────────────────────────────────────────────

func TestManager_PeekReturnsSnippets(t *testing.T) {
	s := &stubSource{
		id:       "wiki",
		priority: 0,
		peekFn: func(_ context.Context, q knowledge.Query) ([]knowledge.Result, error) {
			return []knowledge.Result{
				stubResult("wiki", "doc1", "Go Language", "Go is statically typed.", 0.95),
				stubResult("wiki", "doc2", "Go Concurrency", "Goroutines are lightweight.", 0.80),
			}, nil
		},
	}
	m := newManager(knowledge.ManagerConfig{MaxResults: 5}, s)
	tools := m.Tools()

	var searchTool interface {
		Execute(context.Context, map[string]any) (interface{ GetOutput() string }, error)
	}
	_ = searchTool

	// Execute knowledge_search via tool interface
	ctx := context.Background()
	result, err := tools[0].Execute(ctx, map[string]any{"query": "Go"})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !strings.Contains(result.Output, "Go Language") {
		t.Errorf("expected 'Go Language' in output, got: %s", result.Output)
	}
	if !strings.Contains(result.Output, "wiki:doc1") {
		t.Errorf("expected ref_id 'wiki:doc1' in output, got: %s", result.Output)
	}
}

func TestManager_PeekRespectsMaxResults(t *testing.T) {
	s := &stubSource{
		id:       "db",
		priority: 0,
		peekFn: func(_ context.Context, q knowledge.Query) ([]knowledge.Result, error) {
			results := make([]knowledge.Result, 10)
			for i := range results {
				results[i] = stubResult("db", fmt.Sprintf("doc%d", i), fmt.Sprintf("Doc %d", i), "snippet", float64(10-i)/10.0)
			}
			return results, nil
		},
	}
	m := newManager(knowledge.ManagerConfig{MaxResults: 3}, s)
	ctx := context.Background()
	result, err := m.Tools()[0].Execute(ctx, map[string]any{"query": "anything"})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	// Should mention 3 results
	if !strings.Contains(result.Output, "3") {
		t.Errorf("expected 3 results in output, got: %s", result.Output)
	}
}

func TestManager_PeekSnippetTruncation(t *testing.T) {
	longSnippet := strings.Repeat("x", 500)
	s := &stubSource{
		id:       "src",
		priority: 0,
		peekFn: func(_ context.Context, _ knowledge.Query) ([]knowledge.Result, error) {
			return []knowledge.Result{stubResult("src", "1", "Title", longSnippet, 0.5)}, nil
		},
	}
	m := newManager(knowledge.ManagerConfig{SnippetMaxChars: 100, MaxResults: 5}, s)
	ctx := context.Background()
	result, err := m.Tools()[0].Execute(ctx, map[string]any{"query": "x"})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	// Output must not contain the full 500-char snippet
	if strings.Contains(result.Output, longSnippet) {
		t.Error("snippet was not truncated")
	}
	if !strings.Contains(result.Output, "truncated") {
		t.Error("expected truncation marker in output")
	}
}

// ── Priority groups ───────────────────────────────────────────────────────────

func TestManager_HighPriorityGroupShadowsLow(t *testing.T) {
	lowCalled := false
	high := &stubSource{
		id: "high", priority: 0,
		peekFn: func(_ context.Context, q knowledge.Query) ([]knowledge.Result, error) {
			// Return MaxResults results — low-priority group should be skipped.
			return []knowledge.Result{
				stubResult("high", "a", "A", "snip", 0.9),
				stubResult("high", "b", "B", "snip", 0.8),
				stubResult("high", "c", "C", "snip", 0.7),
			}, nil
		},
	}
	low := &stubSource{
		id: "low", priority: 1,
		peekFn: func(_ context.Context, _ knowledge.Query) ([]knowledge.Result, error) {
			lowCalled = true
			return []knowledge.Result{stubResult("low", "x", "X", "snip", 0.5)}, nil
		},
	}
	m := newManager(knowledge.ManagerConfig{MaxResults: 3}, high, low)
	ctx := context.Background()
	_, err := m.Tools()[0].Execute(ctx, map[string]any{"query": "test"})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if lowCalled {
		t.Error("low-priority source should have been skipped")
	}
}

func TestManager_FallsToLowPriorityWhenHighInsufficient(t *testing.T) {
	lowCalled := false
	high := &stubSource{
		id: "high", priority: 0,
		peekFn: func(_ context.Context, _ knowledge.Query) ([]knowledge.Result, error) {
			return []knowledge.Result{stubResult("high", "a", "A", "snip", 0.9)}, nil
		},
	}
	low := &stubSource{
		id: "low", priority: 1,
		peekFn: func(_ context.Context, _ knowledge.Query) ([]knowledge.Result, error) {
			lowCalled = true
			return []knowledge.Result{stubResult("low", "x", "X", "snip", 0.5)}, nil
		},
	}
	// MaxResults=5 but high only returns 1 → low must be queried
	m := newManager(knowledge.ManagerConfig{MaxResults: 5}, high, low)
	ctx := context.Background()
	_, err := m.Tools()[0].Execute(ctx, map[string]any{"query": "test"})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !lowCalled {
		t.Error("low-priority source should have been called to fill remaining slots")
	}
}

// ── Fetch / fetch tool ────────────────────────────────────────────────────────

func TestManager_FetchRoutesViaRefIDPrefix(t *testing.T) {
	wikiCalled := false
	webCalled := false

	wiki := &stubSource{
		id: "wiki", priority: 0,
		fetchFn: func(_ context.Context, q knowledge.Query) ([]knowledge.Result, error) {
			wikiCalled = true
			return []knowledge.Result{{
				RefID:   "wiki:" + q.Input,
				Title:   "Wiki Doc",
				Source:  "wiki",
				Score:   -1,
				Content: "Full wiki content.",
			}}, nil
		},
	}
	web := &stubSource{
		id: "web", priority: 1,
		fetchFn: func(_ context.Context, _ knowledge.Query) ([]knowledge.Result, error) {
			webCalled = true
			return []knowledge.Result{{RefID: "web:x", Title: "Web", Source: "web", Content: "web content"}}, nil
		},
	}
	m := newManager(knowledge.ManagerConfig{AllowPartialFailure: true}, wiki, web)
	ctx := context.Background()

	// ref_id has "wiki:" prefix → should route directly to wiki source
	result, err := m.Tools()[1].Execute(ctx, map[string]any{"ref_id": "wiki:doc42"})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !wikiCalled {
		t.Error("wiki source should have been called")
	}
	if webCalled {
		t.Error("web source should NOT have been called")
	}
	if !strings.Contains(result.Output, "Full wiki content.") {
		t.Errorf("expected full content in output, got: %s", result.Output)
	}
}

func TestManager_FetchContentTruncation(t *testing.T) {
	longContent := strings.Repeat("y", 2000)
	s := &stubSource{
		id: "src", priority: 0,
		fetchFn: func(_ context.Context, q knowledge.Query) ([]knowledge.Result, error) {
			return []knowledge.Result{{
				RefID: q.Input, Title: "Big Doc", Source: "src", Score: -1, Content: longContent,
			}}, nil
		},
	}
	m := newManager(knowledge.ManagerConfig{ContentMaxChars: 500}, s)
	ctx := context.Background()
	result, err := m.Tools()[1].Execute(ctx, map[string]any{"ref_id": "src:bigdoc"})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if strings.Contains(result.Output, longContent) {
		t.Error("content was not truncated")
	}
	if !strings.Contains(result.Output, "truncated") {
		t.Error("expected truncation marker in output")
	}
}

// ── Accepts filtering ─────────────────────────────────────────────────────────

func TestManager_AcceptsFiltersOutIncompatibleSources(t *testing.T) {
	called := false
	s := &stubSource{
		id:       "db",
		priority: 0,
		accepts: func(q knowledge.Query) bool {
			return q.Type == knowledge.CallTypeQuery // only handles structured queries
		},
		peekFn: func(_ context.Context, _ knowledge.Query) ([]knowledge.Result, error) {
			called = true
			return nil, nil
		},
	}
	m := newManager(knowledge.ManagerConfig{}, s)
	ctx := context.Background()
	// knowledge_search defaults to CallTypeSearch → source should be skipped
	_, _ = m.Tools()[0].Execute(ctx, map[string]any{"query": "anything"})
	if called {
		t.Error("source with Accepts(Search)=false should not have been called")
	}
}

// ── AllowPartialFailure ───────────────────────────────────────────────────────

func TestManager_PartialFailureAllowed(t *testing.T) {
	good := &stubSource{
		id: "good", priority: 0,
		peekFn: func(_ context.Context, _ knowledge.Query) ([]knowledge.Result, error) {
			return []knowledge.Result{stubResult("good", "1", "Good Result", "snippet", 0.9)}, nil
		},
	}
	bad := &stubSource{
		id: "bad", priority: 0, // same priority group → concurrent
		peekFn: func(_ context.Context, _ knowledge.Query) ([]knowledge.Result, error) {
			return nil, fmt.Errorf("source unavailable")
		},
	}
	m := newManager(knowledge.ManagerConfig{AllowPartialFailure: true, MaxResults: 5}, good, bad)
	ctx := context.Background()
	result, err := m.Tools()[0].Execute(ctx, map[string]any{"query": "test"})
	if err != nil {
		t.Fatalf("unexpected error with AllowPartialFailure=true: %v", err)
	}
	if !strings.Contains(result.Output, "Good Result") {
		t.Errorf("expected partial result in output, got: %s", result.Output)
	}
}

// ── SourceTimeout ─────────────────────────────────────────────────────────────

func TestManager_SourceTimeoutCancelsSlowSource(t *testing.T) {
	slow := &stubSource{
		id: "slow", priority: 0,
		peekFn: func(ctx context.Context, _ knowledge.Query) ([]knowledge.Result, error) {
			select {
			case <-time.After(5 * time.Second):
				return []knowledge.Result{stubResult("slow", "1", "Slow", "s", 0.5)}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}
	m := newManager(knowledge.ManagerConfig{
		SourceTimeout:       50 * time.Millisecond,
		AllowPartialFailure: true,
		MaxResults:          5,
	}, slow)
	ctx := context.Background()
	start := time.Now()
	_, err := m.Tools()[0].Execute(ctx, map[string]any{"query": "test"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed > time.Second {
		t.Errorf("slow source was not cancelled in time (elapsed=%v)", elapsed)
	}
}

// ── tool input validation ─────────────────────────────────────────────────────

func TestSearchTool_EmptyQueryFails(t *testing.T) {
	m := newManager(knowledge.ManagerConfig{})
	ctx := context.Background()
	_, err := m.Tools()[0].Execute(ctx, map[string]any{"query": ""})
	if err == nil {
		t.Error("expected error for empty query")
	}
}

func TestFetchTool_EmptyRefIDFails(t *testing.T) {
	m := newManager(knowledge.ManagerConfig{})
	ctx := context.Background()
	_, err := m.Tools()[1].Execute(ctx, map[string]any{"ref_id": ""})
	if err == nil {
		t.Error("expected error for empty ref_id")
	}
}

func TestFetchTool_NoSourceFails(t *testing.T) {
	m := newManager(knowledge.ManagerConfig{})
	ctx := context.Background()
	_, err := m.Tools()[1].Execute(ctx, map[string]any{"ref_id": "nowhere:doc1"})
	if err == nil {
		t.Error("expected error when no source can handle fetch")
	}
}
