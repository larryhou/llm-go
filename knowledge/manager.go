package knowledge

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/larryhou/llm-go/tool"
)

// ManagerConfig controls Manager dispatch behaviour.
type ManagerConfig struct {
	// SourceTimeout is the per-source deadline for Peek and Fetch calls.
	// 0 means the call inherits the caller's ctx without an extra deadline.
	SourceTimeout time.Duration

	// MaxResults is the total cap on results returned by peek().
	// 0 means unlimited (each source returns its own default).
	MaxResults int

	// SnippetMaxChars truncates Result.Snippet returned by Peek.
	// 0 means no truncation.
	SnippetMaxChars int

	// ContentMaxChars truncates Result.Content returned by Fetch.
	// 0 means no truncation.
	ContentMaxChars int

	// AllowPartialFailure allows peek/fetch to return partial results when
	// one or more sources fail.  When false, any source error aborts the
	// entire dispatch and returns the error to the caller.
	AllowPartialFailure bool
}

// Manager routes knowledge queries to registered Sources, coordinates
// priority-grouped concurrent dispatch, and exposes the results to an LLM
// via two tool.Tool implementations (knowledge_search, knowledge_fetch).
//
// Priority groups:
//
//	Sources are sorted by Priority() ascending (lower = higher priority).
//	All sources sharing the same priority value form one group and are
//	called concurrently.  If a group produces enough results (>= MaxResults),
//	lower-priority groups are skipped.  This lets fast, high-quality sources
//	(e.g. an internal index) shadow slower, general-purpose ones (e.g. web).
//
// Fetch routing:
//
//	Result.RefID is prefixed with "{sourceID}:".  Manager.fetch() strips the
//	prefix and routes the call to the matching source directly, avoiding a
//	linear scan.  If no prefix is present (raw URL / external ID), the first
//	source that Accepts the query is used.
type Manager struct {
	cfg     ManagerConfig
	mu      sync.RWMutex
	sources []Source // sorted ascending by Priority()
}

// NewManager creates a Manager with the given configuration.
func NewManager(cfg ManagerConfig) *Manager {
	return &Manager{cfg: cfg}
}

// Register adds a Source to the manager.
// Sources are kept sorted by Priority() so the dispatch loop needs no sort.
// Safe to call concurrently.
func (m *Manager) Register(s Source) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sources = append(m.sources, s)
	sort.Slice(m.sources, func(i, j int) bool {
		return m.sources[i].Priority() < m.sources[j].Priority()
	})
}

// Tools returns the two tool.Tool implementations the Manager exposes to
// the LLM: knowledge_search and knowledge_fetch.
// Append these to RunInput.Tools — no other changes required.
func (m *Manager) Tools() []tool.Tool {
	return []tool.Tool{
		&searchTool{mgr: m},
		&fetchTool{mgr: m},
	}
}

// Search is the public entry point for a QueryTypeSearch or QueryTypeQuery
// request.  It dispatches to all eligible sources via priority-grouped
// concurrent Peek calls.  Callers such as the HTTP API use this directly
// to obtain structured []Result values instead of formatted Markdown.
func (m *Manager) Search(ctx context.Context, q Query) ([]Result, error) {
	return m.peek(ctx, q)
}

// Fetch is the public entry point for a QueryTypeFetch request.
// It routes to the correct source via the "{sourceID}:" prefix in q.Input
// and returns the full-content result.  Callers such as the HTTP API use
// this directly to obtain a structured Result instead of formatted Markdown.
func (m *Manager) Fetch(ctx context.Context, q Query) ([]Result, error) {
	return m.fetch(ctx, q)
}

// peek dispatches a Peek call across all eligible sources using priority
// grouping and concurrent execution within each group.
func (m *Manager) peek(ctx context.Context, q Query) ([]Result, error) {
	m.mu.RLock()
	sources := append([]Source(nil), m.sources...)
	m.mu.RUnlock()

	groups := groupByPriority(sources, q)
	var accumulated []Result

	for _, group := range groups {
		results, err := m.dispatchGroup(ctx, group, q, true)
		if err != nil && !m.cfg.AllowPartialFailure {
			return nil, err
		}
		// Truncate snippets before accumulating
		for i := range results {
			results[i].Snippet = truncateStr(results[i].Snippet, m.cfg.SnippetMaxChars)
		}
		accumulated = append(accumulated, results...)
		if m.cfg.MaxResults > 0 && len(accumulated) >= m.cfg.MaxResults {
			break
		}
	}

	if m.cfg.MaxResults > 0 && len(accumulated) > m.cfg.MaxResults {
		accumulated = accumulated[:m.cfg.MaxResults]
	}
	// Final global sort by score descending so results from lower-priority
	// groups that scored higher than the tail of a higher-priority group
	// are surfaced correctly.
	sort.Slice(accumulated, func(i, j int) bool {
		return accumulated[i].Score > accumulated[j].Score
	})
	return accumulated, nil
}

// fetch routes a Fetch call to the appropriate source.
// It uses the "{sourceID}:" prefix in q.Input to route directly;
// falls back to the first source that Accepts the query.
func (m *Manager) fetch(ctx context.Context, q Query) ([]Result, error) {
	m.mu.RLock()
	sources := append([]Source(nil), m.sources...)
	m.mu.RUnlock()

	// Attempt direct routing via sourceID prefix.
	sourceID, internalKey, hasPfx := strings.Cut(q.Input, ":")
	if hasPfx {
		for _, s := range sources {
			if s.ID() == sourceID {
				routed := q
				routed.Input = internalKey
				results, err := m.callSource(ctx, s, routed, false)
				if err != nil {
					return nil, err
				}
				return m.truncateContent(results), nil
			}
		}
		// sourceID prefix present but no registered source matched —
		// return a clear error instead of silently falling back to an
		// unrelated source, which would produce a confusing result.
		return nil, fmt.Errorf("knowledge: source %q not available for ref_id %q", sourceID, q.Input)
	}

	// Fallback: first accepting source (only reached when no "sourceID:" prefix).
	for _, s := range sources {
		if !s.Accepts(q) {
			continue
		}
		results, err := m.callSource(ctx, s, q, false)
		if err != nil {
			return nil, err
		}
		return m.truncateContent(results), nil
	}
	return nil, fmt.Errorf("knowledge: no source can handle fetch query %q", q.Input)
}

// ── internal helpers ──────────────────────────────────────────────────────────

// groupByPriority partitions sources that Accept q into priority slices.
// peekMode=true filters by Accepts; false also filters by Accepts (same).
func groupByPriority(sources []Source, q Query) [][]Source {
	var groups [][]Source
	var cur []Source
	curPri := -1
	for _, s := range sources {
		if !s.Accepts(q) {
			continue
		}
		if s.Priority() != curPri {
			if len(cur) > 0 {
				groups = append(groups, cur)
			}
			cur = nil
			curPri = s.Priority()
		}
		cur = append(cur, s)
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	return groups
}

// dispatchGroup concurrently calls Peek or Fetch on all sources in the group.
func (m *Manager) dispatchGroup(ctx context.Context, group []Source, q Query, peek bool) ([]Result, error) {
	type outcome struct {
		results []Result
		err     error
	}
	ch := make(chan outcome, len(group))

	for _, s := range group {
		s := s
		go func() {
			r, e := m.callSource(ctx, s, q, peek)
			ch <- outcome{r, e}
		}()
	}

	var all []Result
	var firstErr error
	for range group {
		o := <-ch
		if o.err != nil {
			if firstErr == nil {
				firstErr = o.err
			}
			continue
		}
		all = append(all, o.results...)
	}

	// Sort within group by Score descending.
	sort.Slice(all, func(i, j int) bool {
		return all[i].Score > all[j].Score
	})

	// Always return partial results plus error; caller decides based on AllowPartialFailure.
	return all, firstErr
}

// callSource calls Peek or Fetch on a single source with optional timeout.
func (m *Manager) callSource(ctx context.Context, s Source, q Query, peek bool) ([]Result, error) {
	callCtx := ctx
	var cancel context.CancelFunc
	if m.cfg.SourceTimeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, m.cfg.SourceTimeout)
		defer cancel()
	}
	if peek {
		return s.Peek(callCtx, q)
	}
	return s.Fetch(callCtx, q)
}

func (m *Manager) truncateContent(results []Result) []Result {
	for i := range results {
		results[i].Content = truncateStr(results[i].Content, m.cfg.ContentMaxChars)
	}
	return results
}

func truncateStr(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("\n…[truncated at %d chars]", max)
}
