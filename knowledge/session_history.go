// Package knowledge provides session history recall via a per-session Bleve
// in-memory index, integrated with the knowledge_search / knowledge_fetch tools.
package knowledge

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"

	bleve "github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"
	bquery "github.com/blevesearch/bleve/v2/search/query"
	index "github.com/blevesearch/bleve_index_api"

	_ "github.com/larryhou/llm-go/knowledge/gsetokenizer" // register "gse" tokenizer
	"github.com/larryhou/llm-go/store"
)

// htmlTagRe matches any HTML tag so highlight markup can be stripped from
// Bleve fragment output regardless of which highlighter style is configured.
var htmlTagRe = regexp.MustCompile(`<[^>]+>`)

const (
	sessionHistorySourceID = "session-history"

	// historyAnalyzer is the name registered in the Bleve index mapping.
	// Uses gse for Chinese word segmentation + lowercase for ASCII terms.
	historyAnalyzer = "gse_lowercase"

	// snippet parameters
	snippetFragmentSize = 150
	snippetMaxFragments = 3
	snippetMaxResults   = 5

	// field names in HistoryDoc
	fieldText      = "text"
	fieldRole      = "role"
	fieldToolCalls = "tool_calls"
)

// DefaultMaxCompactions is the default number of compaction rounds whose
// documents are retained in the index. Each round is ~5-6 MB; 8 rounds ≈ 50 MB.
const DefaultMaxCompactions = 8

// CompactionHook is called after a successful Compact(), receiving the head
// messages that were compacted and their associated parts.
// Used by SessionHistorySource to index compacted history without coupling
// compaction logic to the index implementation.
// Nil is a no-op — existing callers are unaffected.
//
// Note: parts is a map keyed by all message IDs in the session, not just those
// in head. Implementations should only access parts[m.ID] for m in head.
type CompactionHook func(head []*store.Message, parts map[string][]*store.Part)

// HistoryDoc is the document indexed per message at compaction time.
type HistoryDoc struct {
	ID            string   `json:"id"`
	Role          string   `json:"role"`
	Text          string   `json:"text"`
	ToolCalls     []string `json:"tool_calls"`
	TurnIndex     int      `json:"turn_index"`
	CompactionSeq int      `json:"compaction_seq"`
	CreatedAt     int64    `json:"created_at"`
}

// SessionHistorySource is a knowledge.Source backed by a per-session Bleve
// in-memory index. It indexes compacted messages via a CompactionHook and
// exposes them through knowledge_search / knowledge_fetch.
//
// Each instance owns a single private Bleve index — cross-session leakage is
// impossible by construction. The index is released when the struct is GC'd.
//
// Memory is bounded by maxCompactions: when a new compaction round would exceed
// the limit, the oldest round's documents are deleted first.
type SessionHistorySource struct {
	sessionID      string
	index          bleve.Index
	maxCompactions int
	compactionDocs map[int][]string // compactionSeq → []docID
	currentSeq     int
	mu             sync.Mutex
}

// NewSessionHistorySource creates a SessionHistorySource with a private
// in-memory Bleve index using gse for Chinese text segmentation.
//
// maxCompactions controls how many compaction rounds are retained (default 8).
// Pass 0 to use DefaultMaxCompactions.
func NewSessionHistorySource(sessionID string, maxCompactions int) (*SessionHistorySource, error) {
	if maxCompactions <= 0 {
		maxCompactions = DefaultMaxCompactions
	}
	m, err := buildIndexMapping()
	if err != nil {
		return nil, fmt.Errorf("session history source: build mapping: %w", err)
	}
	idx, err := bleve.NewMemOnly(m)
	if err != nil {
		return nil, fmt.Errorf("session history source: create index: %w", err)
	}
	return &SessionHistorySource{
		sessionID:      sessionID,
		index:          idx,
		maxCompactions: maxCompactions,
		compactionDocs: make(map[int][]string),
	}, nil
}

// ID implements knowledge.Source.
func (s *SessionHistorySource) ID() string { return sessionHistorySourceID }

// Priority implements knowledge.Source. Returns 0 (highest priority).
func (s *SessionHistorySource) Priority() int { return 0 }

// Accepts implements knowledge.Source.
func (s *SessionHistorySource) Accepts(q Query) bool {
	return q.Type == QueryTypeSearch || q.Type == QueryTypeFetch
}

// Peek implements knowledge.Source for QueryTypeSearch.
// It runs a Bleve highlight search and returns up to snippetMaxResults snippets,
// each showing snippetFragmentSize characters around matched terms (max snippetMaxFragments fragments).
func (s *SessionHistorySource) Peek(ctx context.Context, q Query) ([]Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	input := strings.TrimSpace(q.Input)
	var bq bquery.Query
	if input == "" {
		bq = bleve.NewMatchAllQuery()
	} else {
		bq = bleve.NewQueryStringQuery(input)
	}

	size := q.MaxResults
	if size <= 0 {
		size = snippetMaxResults
	}

	req := bleve.NewSearchRequestOptions(bq, size, 0, false)
	req.Fields = []string{fieldRole, "turn_index", "compaction_seq"}
	req.Highlight = bleve.NewHighlightWithStyle("html")
	req.Highlight.AddField(fieldText)

	res, err := s.index.SearchInContext(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("session history peek: %w", err)
	}

	results := make([]Result, 0, len(res.Hits))
	for _, hit := range res.Hits {
		seq := int(toFloat64(hit.Fields["compaction_seq"]))
		turnIdx := int(toFloat64(hit.Fields["turn_index"]))
		role := fmt.Sprintf("%v", hit.Fields[fieldRole])

		snippet := extractFragments(hit.Fragments, fieldText)
		if snippet == "" {
			// fallback: fetch stored text and truncate
			snippet = s.storedText(hit.ID)
			if len(snippet) > snippetFragmentSize {
				snippet = snippet[:snippetFragmentSize] + "..."
			}
		}

		score := -1.0
		if res.MaxScore > 0 {
			score = hit.Score / res.MaxScore
		}

		title := fmt.Sprintf("[来源：历史对话 第%d轮 turn#%d role=%s]", seq, turnIdx, role)

		results = append(results, Result{
			RefID:   sessionHistorySourceID + ":" + hit.ID,
			Title:   title,
			Source:  sessionHistorySourceID,
			Score:   score,
			Snippet: title + "\n" + snippet,
		})
	}
	return results, nil
}

// Fetch implements knowledge.Source for QueryTypeFetch.
// It retrieves the full text of a previously indexed message by its ref_id.
func (s *SessionHistorySource) Fetch(ctx context.Context, q Query) ([]Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	docID := q.Input
	if pfx := sessionHistorySourceID + ":"; strings.HasPrefix(docID, pfx) {
		docID = strings.TrimPrefix(docID, pfx)
	}

	doc, err := s.index.Document(docID)
	if err != nil {
		return nil, fmt.Errorf("session history fetch %q: %w", docID, err)
	}
	if doc == nil {
		return nil, fmt.Errorf("session history fetch %q: not found", docID)
	}

	// VisitFields may visit the same logical field multiple times if Bleve stores
	// it in multiple physical index columns. Accumulate all values per field.
	fieldVals := make(map[string]*strings.Builder)
	doc.VisitFields(func(f index.Field) {
		b := fieldVals[f.Name()]
		if b == nil {
			b = &strings.Builder{}
			fieldVals[f.Name()] = b
		}
		b.Write(f.Value())
	})
	get := func(key string) string {
		if b := fieldVals[key]; b != nil {
			return b.String()
		}
		return ""
	}

	seq := get("compaction_seq")
	turnIdx := get("turn_index")
	role := get(fieldRole)
	text := get(fieldText)

	title := fmt.Sprintf("[来源：历史对话 第%s轮 turn#%s role=%s]", seq, turnIdx, role)

	meta := map[string]any{
		"compaction_seq": seq,
		"turn_index":     turnIdx,
		"role":           role,
	}

	return []Result{{
		RefID:   sessionHistorySourceID + ":" + docID,
		Title:   title,
		Source:  sessionHistorySourceID,
		Score:   -1,
		Content: title + "\n\n" + text,
		Metadata: meta,
	}}, nil
}

// Hook returns a CompactionHook that indexes the compacted head messages into
// this source. Pass the result to RunInput.OnCompact.
//
// When the number of retained compaction rounds reaches maxCompactions, the
// oldest round's documents are deleted from the index before indexing the new round.
func (s *SessionHistorySource) Hook() CompactionHook {
	return func(head []*store.Message, parts map[string][]*store.Part) {
		s.mu.Lock()
		defer s.mu.Unlock()

		s.currentSeq++

		// Prune oldest round if we've hit the limit.
		if len(s.compactionDocs) >= s.maxCompactions {
			oldest := s.oldestSeq()
			for _, id := range s.compactionDocs[oldest] {
				_ = s.index.Delete(id)
			}
			delete(s.compactionDocs, oldest)
		}

		// Index all messages in this compaction round.
		var ids []string
		for i, m := range head {
			doc := buildDoc(m, parts[m.ID], s.currentSeq, i)
			_ = s.index.Index(m.ID, doc)
			ids = append(ids, m.ID)
		}
		s.compactionDocs[s.currentSeq] = ids
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func (s *SessionHistorySource) oldestSeq() int {
	oldest := int(^uint(0) >> 1) // max int
	for seq := range s.compactionDocs {
		if seq < oldest {
			oldest = seq
		}
	}
	return oldest
}

// storedText retrieves the raw text field of a document from the index.
// Must be called with s.mu held (same lock as Peek/Fetch).
func (s *SessionHistorySource) storedText(docID string) string {
	doc, err := s.index.Document(docID)
	if err != nil || doc == nil {
		return ""
	}
	var sb strings.Builder
	doc.VisitFields(func(f index.Field) {
		if f.Name() == fieldText {
			sb.Write(f.Value())
		}
	})
	return sb.String()
}

// buildDoc constructs a HistoryDoc from a store.Message and its parts.
// Text is assembled from all text parts; tool call names are collected separately.
func buildDoc(m *store.Message, parts []*store.Part, seq, turnIdx int) HistoryDoc {
	var textParts []string
	var toolCalls []string

	for _, p := range parts {
		switch p.Type {
		case store.PartTypeText:
			d, ok := store.DataAs[*store.TextPartData](p)
			if ok && d.Text != "" {
				textParts = append(textParts, d.Text)
			}
		case store.PartTypeTool:
			d, ok := store.DataAs[*store.ToolPartData](p)
			if ok && d.Tool != "" {
				toolCalls = append(toolCalls, d.Tool)
			}
		}
	}

	return HistoryDoc{
		ID:            m.ID,
		Role:          m.Role,
		Text:          strings.Join(textParts, "\n"),
		ToolCalls:     toolCalls,
		TurnIndex:     turnIdx,
		CompactionSeq: seq,
		CreatedAt:     m.CreatedAt.UnixMilli(),
	}
}

// buildIndexMapping constructs a Bleve IndexMapping that uses:
//   - gse_lowercase analyzer for the "text" field (Chinese + ASCII)
//   - keyword analyzer for role, tool_calls, id fields
func buildIndexMapping() (mapping.IndexMapping, error) {
	im := bleve.NewIndexMapping()

	// Register gse_lowercase analyzer: gse tokenizer (registered by gsetokenizer
	// init()) + lowercase filter. The "custom" type and "to_lower" filter name
	// are Bleve built-ins; we use string literals to avoid importing the packages.
	if err := im.AddCustomAnalyzer(historyAnalyzer, map[string]interface{}{
		"type":          "custom",
		"tokenizer":     "gse",
		"token_filters": []string{"to_lower"},
	}); err != nil {
		return nil, fmt.Errorf("register %s analyzer: %w", historyAnalyzer, err)
	}

	dm := bleve.NewDocumentMapping()

	// text field: gse_lowercase for Chinese + ASCII search
	textField := bleve.NewTextFieldMapping()
	textField.Analyzer = historyAnalyzer
	textField.Store = true
	dm.AddFieldMappingsAt(fieldText, textField)

	// role field: keyword (exact match)
	roleField := bleve.NewTextFieldMapping()
	roleField.Analyzer = "keyword"
	roleField.Store = true
	dm.AddFieldMappingsAt(fieldRole, roleField)

	// tool_calls field: keyword (tool names must not be split)
	toolField := bleve.NewTextFieldMapping()
	toolField.Analyzer = "keyword"
	toolField.Store = true
	dm.AddFieldMappingsAt(fieldToolCalls, toolField)

	// numeric fields: stored for retrieval
	seqField := bleve.NewNumericFieldMapping()
	seqField.Store = true
	dm.AddFieldMappingsAt("compaction_seq", seqField)

	turnField := bleve.NewNumericFieldMapping()
	turnField.Store = true
	dm.AddFieldMappingsAt("turn_index", turnField)

	im.AddDocumentMapping("_default", dm)
	im.DefaultAnalyzer = historyAnalyzer

	return im, nil
}

func extractFragments(fragments map[string][]string, field string) string {
	frags, ok := fragments[field]
	if !ok || len(frags) == 0 {
		return ""
	}
	// Strip all HTML tags inserted by bleve's highlighter (e.g. <mark>, <em>,
	// <strong>) regardless of which highlight style is configured.
	var clean []string
	for _, f := range frags {
		clean = append(clean, htmlTagRe.ReplaceAllString(f, ""))
	}
	return strings.Join(clean, " … ")
}

func toFloat64(v any) float64 {
	if v == nil {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}
