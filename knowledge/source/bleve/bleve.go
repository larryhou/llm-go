// Package bleve provides a knowledge.Source implementation backed by a
// Bleve full-text search index (github.com/blevesearch/bleve/v2).
//
// Supported call types:
//
//	CallTypeSearch → Peek(): QueryStringQuery + Highlight → snippet fragments
//	CallTypeFetch  → Fetch(): index.Document(id) → stored field content
//	CallTypeQuery  → Peek(): BooleanQuery built from Query.Filters
//
// RefID format: "{sourceID}:{docID}"
// The sourceID prefix is stripped before passing the internal docID to
// the Bleve index.
package bleve

import (
	"context"
	"fmt"
	"strings"

	bleve "github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/search/query"
	index "github.com/blevesearch/bleve_index_api"

	"github.com/larryhou/llm-go/knowledge"
)

const (
	defaultMaxResults   = 5
	defaultTitleField   = "title"
	defaultContentField = "content"
)

// Source is a knowledge.Source backed by a Bleve index.
type Source struct {
	index        bleve.Index
	id           string
	priority     int
	titleField   string // index field used as Result.Title (default: "title")
	contentField string // index field used as Result.Content/Snippet (default: "content")
}

// Config holds optional configuration for a BleveSource.
type Config struct {
	// TitleField is the Bleve document field to use as Result.Title.
	// Defaults to "title".
	TitleField string

	// ContentField is the Bleve document field to use as Result.Content
	// (for Fetch) and snippet source (for Peek).
	// Defaults to "content".
	ContentField string
}

// New creates a BleveSource wrapping an open Bleve index.
//
//	id       — unique source identifier, used as RefID prefix
//	priority — dispatch priority (lower = higher priority)
//	cfg      — optional field-name overrides (nil = defaults)
func New(index bleve.Index, id string, priority int, cfg *Config) *Source {
	s := &Source{
		index:        index,
		id:           id,
		priority:     priority,
		titleField:   defaultTitleField,
		contentField: defaultContentField,
	}
	if cfg != nil {
		if cfg.TitleField != "" {
			s.titleField = cfg.TitleField
		}
		if cfg.ContentField != "" {
			s.contentField = cfg.ContentField
		}
	}
	return s
}

// ID implements knowledge.Source.
func (s *Source) ID() string { return s.id }

// Priority implements knowledge.Source.
func (s *Source) Priority() int { return s.priority }

// Accepts implements knowledge.Source.
// BleveSource handles Search (full-text), Fetch (by doc ID), and Query
// (structured field filters via BooleanQuery).
func (s *Source) Accepts(q knowledge.Query) bool {
	return q.Type == knowledge.CallTypeSearch ||
		q.Type == knowledge.CallTypeFetch ||
		q.Type == knowledge.CallTypeQuery
}

// Peek implements knowledge.Source for CallTypeSearch and CallTypeQuery.
// It runs a Bleve search with highlighting and returns compact snippets.
func (s *Source) Peek(ctx context.Context, q knowledge.Query) ([]knowledge.Result, error) {
	bq, err := s.buildQuery(q)
	if err != nil {
		return nil, fmt.Errorf("bleve source %q: build query: %w", s.id, err)
	}

	size := q.MaxResults
	if size <= 0 {
		size = defaultMaxResults
	}

	req := bleve.NewSearchRequestOptions(bq, size, 0, false)
	req.Fields = []string{s.titleField}
	req.Highlight = bleve.NewHighlight()
	req.Highlight.AddField(s.contentField)

	res, err := s.index.SearchInContext(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("bleve source %q: search: %w", s.id, err)
	}

	results := make([]knowledge.Result, 0, len(res.Hits))
	for _, hit := range res.Hits {
		title := fmt.Sprintf("%v", hit.Fields[s.titleField])
		snippet := extractFragments(hit.Fragments, s.contentField)
		// Normalise score: Bleve scores are unbounded; clamp to [0,1] via
		// dividing by max score (first hit has highest score).
		score := -1.0
		if res.MaxScore > 0 {
			score = hit.Score / res.MaxScore
		}
		results = append(results, knowledge.Result{
			RefID:   s.refID(hit.ID),
			Title:   title,
			Source:  s.id,
			Score:   score,
			Snippet: snippet,
		})
	}
	return results, nil
}

// Fetch implements knowledge.Source for CallTypeFetch.
// It loads the stored document from the Bleve index by doc ID and returns
// the full content field.
func (s *Source) Fetch(ctx context.Context, q knowledge.Query) ([]knowledge.Result, error) {
	// Strip the "{sourceID}:" prefix if present.
	docID := q.Input
	if pfx := s.id + ":"; strings.HasPrefix(docID, pfx) {
		docID = strings.TrimPrefix(docID, pfx)
	}

	doc, err := s.index.Document(docID)
	if err != nil {
		return nil, fmt.Errorf("bleve source %q: document %q: %w", s.id, docID, err)
	}
	if doc == nil {
		return nil, fmt.Errorf("bleve source %q: document %q not found", s.id, docID)
	}

	// Extract fields from the Bleve document via VisitFields.
	fields := make(map[string]string)
	doc.VisitFields(func(f index.Field) {
		fields[f.Name()] = string(f.Value())
	})

	title := fields[s.titleField]
	if title == "" {
		title = docID
	}
	content := fields[s.contentField]

	// Collect remaining fields as metadata.
	meta := make(map[string]any, len(fields))
	for k, v := range fields {
		if k != s.titleField && k != s.contentField {
			meta[k] = v
		}
	}

	return []knowledge.Result{{
		RefID:    s.refID(docID),
		Title:    title,
		Source:   s.id,
		Score:    -1,
		Content:  content,
		Metadata: meta,
	}}, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

// refID builds the canonical "{sourceID}:{docID}" reference string.
func (s *Source) refID(docID string) string {
	return s.id + ":" + docID
}

// buildQuery converts a knowledge.Query into a Bleve query.Query.
//
//	CallTypeSearch → QueryStringQuery on q.Input
//	CallTypeQuery  → BooleanQuery built from q.Filters (must-match terms)
//	                 plus optional QueryStringQuery on q.Input if non-empty
func (s *Source) buildQuery(q knowledge.Query) (query.Query, error) {
	switch q.Type {
	case knowledge.CallTypeSearch:
		if strings.TrimSpace(q.Input) == "" {
			return bleve.NewMatchAllQuery(), nil
		}
		return bleve.NewQueryStringQuery(q.Input), nil

	case knowledge.CallTypeQuery:
		bq := bleve.NewBooleanQuery()
		if strings.TrimSpace(q.Input) != "" {
			bq.AddMust(bleve.NewQueryStringQuery(q.Input))
		}
		for field, val := range q.Filters {
			term := fmt.Sprintf("%v", val)
			tq := bleve.NewTermQuery(term)
			tq.SetField(field)
			bq.AddMust(tq)
		}
		return bq, nil

	default:
		return nil, fmt.Errorf("unsupported call type %q for Peek", q.Type)
	}
}

// extractFragments joins the highlighted fragments for a given field.
func extractFragments(fragments map[string][]string, field string) string {
	frags, ok := fragments[field]
	if !ok || len(frags) == 0 {
		return ""
	}
	return strings.Join(frags, " … ")
}
