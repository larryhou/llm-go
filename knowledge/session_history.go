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

	// field names in Record
	fieldText      = "text"
	fieldRole      = "role"
	fieldToolCalls = "tool_calls"
)

// DefaultMaxCompactions is the default number of compaction rounds whose
// documents are retained in the Bleve index. Each round is ~5-6 MB; 8 rounds ≈ 50 MB.
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

// Record is the document indexed per message at compaction time.
type Record struct {
	ID            string   `json:"id"`
	Role          string   `json:"role"`
	Text          string   `json:"text"`
	ToolCalls     []string `json:"tool_calls"`
	TurnIndex     int      `json:"turn_index"`
	CompactionSeq int      `json:"compaction_seq"`
	CreatedAt     int64    `json:"created_at"`
}

// SessionHistorySource is a knowledge.Source backed by a per-session Bleve
// in-memory index (L1 hot cache) with an optional PersistStore backend (L2).
//
// # Two-layer cache semantics
//
//   - L1 (Bleve in-memory): fast gse-segmented full-text search; bounded to
//     maxCompactions rounds (~50 MB). Rounds are evicted LRU-style when the
//     cap is reached.
//   - L2 (PersistStore, optional): durable SQLite storage; unlimited capacity;
//     permanent until Reset(). On a cache miss (seq not in Bleve), the missing
//     round is page-faulted in from L2, then the LRU policy re-applied.
//
// # Startup recovery
//
// If a PersistStore is provided, LoadRecords is called once at construction
// to restore the known seq map (compactionDocs keys + doc IDs). The Bleve index
// starts empty; rounds are loaded on first Peek() that touches them.
//
// # Memory bound
//
// The Bleve index never holds more than maxCompactions rounds simultaneously.
// Evicted rounds remain in SQLite and are re-loaded on the next miss.
type SessionHistorySource struct {
	sessionID      string
	index          bleve.Index
	maxCompactions int
	compactionDocs map[int][]string // seq → []docID  (all known seqs, incl. evicted)
	currentSeq     int

	// L2 persistent backend; nil = pure-memory mode
	persistStore PersistStore

	// LRU tracking for the Bleve index (L1 only).
	// loadedSeqs is the set of seqs currently in Bleve.
	// lruOrder[0] = least recently used, lruOrder[len-1] = most recently used.
	loadedSeqs map[int]struct{}
	lruOrder   []int

	mu sync.Mutex
}

// NewSessionHistorySource creates a SessionHistorySource with a private
// in-memory Bleve index using gse for Chinese text segmentation.
//
// maxCompactions controls how many compaction rounds are held in Bleve (default
// DefaultMaxCompactions). Pass 0 to use the default.
//
// ps is the optional persistent backend (knowledge.PersistStore, implemented by
// sqlite.HistorySource). Pass nil for pure-memory mode — history is lost on
// process restart but behaviour is otherwise identical.
func NewSessionHistorySource(sessionID string, maxCompactions int, ps PersistStore) (*SessionHistorySource, error) {
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

	src := &SessionHistorySource{
		sessionID:      sessionID,
		index:          idx,
		maxCompactions: maxCompactions,
		compactionDocs: make(map[int][]string),
		persistStore:   ps,
		loadedSeqs:     make(map[int]struct{}),
	}

	// Restore known seq map from L2 without loading doc content into Bleve.
	if ps != nil {
		docs, err := ps.LoadRecords(context.Background(), sessionID)
		if err == nil {
			for seq, seqDocs := range docs {
				ids := make([]string, len(seqDocs))
				for i, d := range seqDocs {
					ids[i] = d.ID
				}
				src.compactionDocs[seq] = ids
				if seq > src.currentSeq {
					src.currentSeq = seq
				}
			}
		}
	}

	return src, nil
}

// ID implements knowledge.Source.
func (s *SessionHistorySource) ID() string { return sessionHistorySourceID }

// Priority implements knowledge.Source. Returns 0 (highest priority).
func (s *SessionHistorySource) Priority() int { return 0 }

// Reset discards all indexed history and resets the source to its initial empty
// state. If a PersistStore is configured, all history_docs for this session are
// permanently deleted from SQLite — this is the "wipe memory" operation.
func (s *SessionHistorySource) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Permanently delete from L2 before clearing L1.
	if s.persistStore != nil {
		for seq := range s.compactionDocs {
			_ = s.persistStore.DeleteRecordsBySeq(context.Background(), s.sessionID, seq)
		}
	}

	s.index.Close()
	s.index = nil // guard against use-after-close if rebuild below fails

	m, err := buildIndexMapping()
	if err != nil {
		return fmt.Errorf("session history reset: build mapping: %w", err)
	}
	idx, err := bleve.NewMemOnly(m)
	if err != nil {
		return fmt.Errorf("session history reset: create index: %w", err)
	}
	s.index = idx
	s.compactionDocs = make(map[int][]string)
	s.loadedSeqs = make(map[int]struct{})
	s.lruOrder = nil
	s.currentSeq = 0
	return nil
}

// Accepts implements knowledge.Source.
func (s *SessionHistorySource) Accepts(q Query) bool {
	return q.Type == QueryTypeSearch || q.Type == QueryTypeFetch
}

// Peek implements knowledge.Source for QueryTypeSearch.
// It runs a Bleve highlight search and returns up to snippetMaxResults snippets.
// On a cache miss (a known seq is not yet in Bleve), the missing rounds are
// page-faulted in from the PersistStore before the search is executed.
func (s *SessionHistorySource) Peek(ctx context.Context, q Query) ([]Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.index == nil {
		return nil, fmt.Errorf("session history peek: index unavailable (reset in progress)")
	}

	// Page-in any known seqs that are not yet in Bleve (L2 → L1).
	if s.persistStore != nil {
		for seq := range s.compactionDocs {
			if _, loaded := s.loadedSeqs[seq]; !loaded {
				s.pageIn(ctx, seq)
			}
		}
	}

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
// If the doc's seq is not in Bleve, it is page-faulted in first.
func (s *SessionHistorySource) Fetch(ctx context.Context, q Query) ([]Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.index == nil {
		return nil, fmt.Errorf("session history fetch: index unavailable (reset in progress)")
	}

	docID := q.Input
	if pfx := sessionHistorySourceID + ":"; strings.HasPrefix(docID, pfx) {
		docID = strings.TrimPrefix(docID, pfx)
	}

	// If not in Bleve yet, page-in the seq that owns this doc.
	if _, err := s.index.Document(docID); err != nil || func() bool {
		d, _ := s.index.Document(docID)
		return d == nil
	}() {
		if s.persistStore != nil {
			s.pageInByDocID(ctx, docID)
		}
	}

	doc, err := s.index.Document(docID)
	if err != nil {
		return nil, fmt.Errorf("session history fetch %q: %w", docID, err)
	}
	if doc == nil {
		return nil, fmt.Errorf("session history fetch %q: not found", docID)
	}

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
// this source. If a PersistStore is configured, each doc is also persisted to
// SQLite synchronously before the function returns.
//
// When the number of Bleve-held rounds reaches maxCompactions, the LRU-oldest
// round is evicted from Bleve (but kept in SQLite).
func (s *SessionHistorySource) Hook() CompactionHook {
	return func(head []*store.Message, parts map[string][]*store.Part) {
		s.mu.Lock()
		defer s.mu.Unlock()

		if s.index == nil {
			return // reset in progress; skip indexing
		}

		s.currentSeq++

		// Index all messages in this compaction round into Bleve + SQLite.
		var ids []string
		for i, m := range head {
			doc := buildDoc(m, parts[m.ID], s.currentSeq, i)
			_ = s.index.Index(m.ID, doc)
			ids = append(ids, m.ID)

			// Persist to L2 synchronously.
			if s.persistStore != nil {
				_ = s.persistStore.SaveRecord(
					context.Background(), s.sessionID, doc)
			}
		}

		s.compactionDocs[s.currentSeq] = ids
		s.loadedSeqs[s.currentSeq] = struct{}{}
		s.lruOrder = append(s.lruOrder, s.currentSeq)

		// LRU eviction from Bleve if we're over the cap.
		s.evictIfNeeded()
	}
}

// ── LRU / page-in helpers ─────────────────────────────────────────────────────

// pageIn loads all docs for the given seq from L2 into Bleve and updates the
// LRU order. Must be called with s.mu held.
func (s *SessionHistorySource) pageIn(ctx context.Context, seq int) {
	if _, loaded := s.loadedSeqs[seq]; loaded {
		return
	}
	allDocs, err := s.persistStore.LoadRecords(ctx, s.sessionID)
	if err != nil {
		return
	}
	for _, doc := range allDocs[seq] {
		_ = s.index.Index(doc.ID, doc)
	}
	s.loadedSeqs[seq] = struct{}{}
	s.lruOrder = append(s.lruOrder, seq)
	s.evictIfNeeded()
}

// pageInByDocID finds which seq owns docID and pages it in.
// Must be called with s.mu held.
func (s *SessionHistorySource) pageInByDocID(ctx context.Context, docID string) {
	for seq, ids := range s.compactionDocs {
		for _, id := range ids {
			if id == docID {
				s.pageIn(ctx, seq)
				return
			}
		}
	}
}

// evictIfNeeded removes the LRU-oldest seq from Bleve when loadedSeqs exceeds
// maxCompactions. The SQLite copy is untouched.
// Must be called with s.mu held.
func (s *SessionHistorySource) evictIfNeeded() {
	for len(s.loadedSeqs) > s.maxCompactions && len(s.lruOrder) > 0 {
		oldest := s.lruOrder[0]
		s.lruOrder = s.lruOrder[1:]
		if _, ok := s.loadedSeqs[oldest]; !ok {
			continue // already evicted
		}
		for _, id := range s.compactionDocs[oldest] {
			_ = s.index.Delete(id)
		}
		delete(s.loadedSeqs, oldest)
	}
}

// touchLRU marks seq as most-recently used in lruOrder.
// Must be called with s.mu held.
func (s *SessionHistorySource) touchLRU(seq int) {
	for i, v := range s.lruOrder {
		if v == seq {
			s.lruOrder = append(s.lruOrder[:i], s.lruOrder[i+1:]...)
			break
		}
	}
	s.lruOrder = append(s.lruOrder, seq)
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
	if s.index == nil {
		return ""
	}
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

// buildDoc constructs a Record from a store.Message and its parts.
// Text is assembled from all text parts; tool call names are collected separately.
func buildDoc(m *store.Message, parts []*store.Part, seq, turnIdx int) Record {
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

	return Record{
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

	if err := im.AddCustomAnalyzer(historyAnalyzer, map[string]interface{}{
		"type":          "custom",
		"tokenizer":     "gse",
		"token_filters": []string{"to_lower"},
	}); err != nil {
		return nil, fmt.Errorf("register %s analyzer: %w", historyAnalyzer, err)
	}

	dm := bleve.NewDocumentMapping()

	textField := bleve.NewTextFieldMapping()
	textField.Analyzer = historyAnalyzer
	textField.Store = true
	dm.AddFieldMappingsAt(fieldText, textField)

	roleField := bleve.NewTextFieldMapping()
	roleField.Analyzer = "keyword"
	roleField.Store = true
	dm.AddFieldMappingsAt(fieldRole, roleField)

	toolField := bleve.NewTextFieldMapping()
	toolField.Analyzer = "keyword"
	toolField.Store = true
	dm.AddFieldMappingsAt(fieldToolCalls, toolField)

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
