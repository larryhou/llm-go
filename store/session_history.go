package store

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"

	bleve "github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"
	bquery "github.com/blevesearch/bleve/v2/search/query"
	index "github.com/blevesearch/bleve_index_api"

	_ "github.com/larryhou/llm-go/knowledge/gsetokenizer" // register "gse" tokenizer
	"github.com/larryhou/llm-go/knowledge"
)

// htmlTagRe matches any HTML tag so highlight markup can be stripped from
// Bleve fragment output regardless of which highlighter style is configured.
var htmlTagRe = regexp.MustCompile(`<[^>]+>`)

const (
	sessionHistorySourceID = "session-history"

	// historyAnalyzer is the name registered in the Bleve index mapping.
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

// DefaultMaxCompactions is the default cap for L1 (Bleve). Each seq ≈ 5-6 MB.
const DefaultMaxCompactions = 8

// DefaultMaxIndexedSeqs is the default cap for L0 (compactionDocs).
// = DefaultMaxCompactions × 10; controls how many seq→docID entries are kept
// in memory before the oldest are evicted (SQLite always retains everything).
const DefaultMaxIndexedSeqs = DefaultMaxCompactions * 10

// CompactionHook is called after a successful Compact(), receiving the head
// messages that were compacted and their associated parts.
type CompactionHook func(head []*Message, parts map[string][]*Part)

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

// SessionHistorySource is a knowledge.Source with a two-level cache:
//
//   - L0 (compactionDocs): seq→[]docID index, bounded to maxIndexedSeqs entries.
//     Evicted entries are removed from memory but remain in SQLite.
//   - L1 (Bleve in-memory): full-text index, bounded to maxCompactions seqs.
//     Evicted seqs are deleted from Bleve but remain in SQLite.
//   - L2 (PersistStore/SQLite): permanent storage, never evicted by the cache.
//
// # Invariant
//
//	loadedSeqs ⊆ compactionDocs  (L1 ⊆ L0)
//	docIndex keys == union of all docIDs in compactionDocs values
//
// # Peek strategy (P3)
//
// Always queries both L1 (Bleve, for hot seqs with gse scoring) and L2
// (SQLite, for full history coverage). Results are merged: Bleve hits first
// (sorted by score), then unique SQLite hits appended as supplement.
// This guarantees no historical memory is lost even when cold seqs are
// not in Bleve.
//
// # Fetch strategy
//
// On a docID lookup: locate the owning seq (from L0 if known, else SQLite
// FindSeqByDocID), page-in that seq into Bleve (L2→L1), update LRU, then
// return the full text from Bleve with gse highlight.
type SessionHistorySource struct {
	sessionID string
	index     bleve.Index

	// L1 cap: max seqs held in Bleve simultaneously.
	maxCompactions int
	// L0 cap: max seqs whose doc IDs are held in compactionDocs.
	maxIndexedSeqs int

	// L0: seq → []docID.  All seqs here are known to exist in SQLite.
	// Bounded by maxIndexedSeqs; LRU eviction removes oldest entry from memory
	// (SQLite copy is untouched).
	compactionDocs map[int][]string

	// docIndex is a reverse index: docID → seq.  Maintained in sync with
	// compactionDocs to make seqForDoc O(1) instead of O(seqs × docs_per_seq).
	docIndex map[string]int

	// currentSeq is the highest seq seen so far (monotonically increasing).
	currentSeq int

	// L1: set of seqs currently indexed in Bleve.
	// Invariant: loadedSeqs ⊆ compactionDocs.
	loadedSeqs map[int]struct{}

	// lruOrder tracks access recency for both L0 and L1.
	// lruOrder[0] = least recently used, lruOrder[len-1] = most recently used.
	// A seq present in compactionDocs is always in lruOrder.
	lruOrder []int

	// L2 persistent backend; nil = pure-memory mode.
	persistStore PersistStore

	mu sync.Mutex
}

// NewSessionHistorySource creates a SessionHistorySource.
//
// maxCompactions: L1 (Bleve) cap; 0 → DefaultMaxCompactions.
// maxIndexedSeqs: L0 (compactionDocs) cap; 0 → DefaultMaxIndexedSeqs.
// ps: persistent backend; nil → pure-memory (history lost on restart).
func NewSessionHistorySource(sessionID string, maxCompactions, maxIndexedSeqs int, ps PersistStore) (*SessionHistorySource, error) {
	if maxCompactions <= 0 {
		maxCompactions = DefaultMaxCompactions
	}
	if maxIndexedSeqs <= 0 {
		maxIndexedSeqs = DefaultMaxIndexedSeqs
	}
	if maxIndexedSeqs < maxCompactions {
		maxIndexedSeqs = maxCompactions
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
		maxIndexedSeqs: maxIndexedSeqs,
		compactionDocs: make(map[int][]string),
		docIndex:       make(map[string]int),
		loadedSeqs:     make(map[int]struct{}),
		persistStore:   ps,
	}

	// Restore L0 from SQLite: load only the most recent maxIndexedSeqs seqs.
	// Bleve stays empty; seqs are page-in on first Fetch.
	if ps != nil {
		seqIndex, err := ps.LoadSeqIndex(context.Background(), sessionID, maxIndexedSeqs)
		if err == nil {
			for seq, ids := range seqIndex {
				if _, dup := src.compactionDocs[seq]; dup {
					continue // defensive: skip duplicate seqs from SQLite
				}
				src.compactionDocs[seq] = ids
				for _, id := range ids {
					src.docIndex[id] = seq
				}
				src.lruOrder = append(src.lruOrder, seq)
				if seq > src.currentSeq {
					src.currentSeq = seq
				}
			}
			// Sort lruOrder ascending so oldest is at front.
			sortInts(src.lruOrder)
		}
	}

	return src, nil
}

// ID implements knowledge.Source.
func (s *SessionHistorySource) ID() string { return sessionHistorySourceID }

// Priority implements knowledge.Source. Returns 0 (highest priority).
func (s *SessionHistorySource) Priority() int { return 0 }

// Accepts implements knowledge.Source.
func (s *SessionHistorySource) Accepts(q knowledge.Query) bool {
	return q.Type == knowledge.QueryTypeSearch || q.Type == knowledge.QueryTypeFetch
}

// Reset discards all cache state and — if a PersistStore is configured —
// permanently deletes all history_docs for this session from SQLite.
func (s *SessionHistorySource) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.persistStore != nil {
		// Use DeleteAllRecords so that seqs evicted from the in-memory L0
		// window (older than maxIndexedSeqs) are also removed from SQLite.
		// The previous approach of iterating s.compactionDocs left those old
		// seqs in SQLite, which caused history to "resurrect" after restart.
		if err := s.persistStore.DeleteAllRecords(context.Background(), s.sessionID); err != nil {
			return fmt.Errorf("session history reset: delete all records: %w", err)
		}
	}

	s.index.Close()
	s.index = nil

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
	s.docIndex = make(map[string]int)
	s.loadedSeqs = make(map[int]struct{})
	s.lruOrder = nil
	s.currentSeq = 0
	return nil
}

// Peek implements knowledge.Source — P3 strategy.
//
// Always queries both L1 (Bleve) and L2 (SQLite) to guarantee full history
// coverage regardless of what is currently cached in Bleve.
//
//  1. Bleve.Search → hot hits with gse scoring (may be empty if Bleve is cold)
//  2. SQLite.Peek  → full-history hits via SQL LIKE
//  3. Merge: Bleve hits first (sorted by score), then unique SQLite hits appended
//  4. Touch LRU for every seq that appeared in Bleve results
func (s *SessionHistorySource) Peek(ctx context.Context, q knowledge.Query) ([]knowledge.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.index == nil {
		return nil, fmt.Errorf("session history peek: index unavailable (reset in progress)")
	}

	size := q.MaxResults
	if size <= 0 {
		size = snippetMaxResults
	}

	// ── L1: Bleve search ─────────────────────────────────────────────────────
	bleveResults, bleveSeqs := s.bleveSearch(ctx, q.Input, size)

	// Touch LRU for every seq that contributed a Bleve hit.
	for seq := range bleveSeqs {
		s.touchLRU(seq)
	}

	// ── L2: SQLite search (via persistStore as Source) ────────────────────────
	var sqlResults []knowledge.Result
	if s.persistStore != nil {
		if src, ok := s.persistStore.(knowledge.Source); ok {
			sqlResults, _ = src.Peek(ctx, q)
		}
	}

	// ── Merge: Bleve first, then unique SQLite supplements ────────────────────
	seen := make(map[string]struct{}, len(bleveResults))
	merged := make([]knowledge.Result, 0, size)

	for _, r := range bleveResults {
		seen[r.RefID] = struct{}{}
		merged = append(merged, r)
	}
	for _, r := range sqlResults {
		if _, dup := seen[r.RefID]; dup {
			continue
		}
		seen[r.RefID] = struct{}{}
		merged = append(merged, r)
		if len(merged) >= size {
			break
		}
	}

	return merged, nil
}

// Fetch implements knowledge.Source.
//
// Locates the owning seq for docID (L0 → SQLite FindSeqByDocID on miss),
// page-ins that seq into Bleve if needed, then returns the full text.
func (s *SessionHistorySource) Fetch(ctx context.Context, q knowledge.Query) ([]knowledge.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.index == nil {
		return nil, fmt.Errorf("session history fetch: index unavailable (reset in progress)")
	}

	docID := q.Input
	if pfx := sessionHistorySourceID + ":"; strings.HasPrefix(docID, pfx) {
		docID = strings.TrimPrefix(docID, pfx)
	}

	// ── Locate owning seq ─────────────────────────────────────────────────────
	seq, found := s.seqForDoc(docID)
	if !found {
		// L0 miss: ask SQLite.
		if s.persistStore == nil {
			return nil, fmt.Errorf("session history fetch %q: not found", docID)
		}
		sqlSeq, ok, err := s.persistStore.FindSeqByDocID(ctx, s.sessionID, docID)
		if err != nil {
			return nil, fmt.Errorf("session history fetch %q: %w", docID, err)
		}
		if !ok {
			return nil, fmt.Errorf("session history fetch %q: not found", docID)
		}
		seq = sqlSeq
		// Promote seq into L0.
		s.addToL0(ctx, seq)
	}

	// ── Page-in seq into Bleve if not already loaded ──────────────────────────
	if _, loaded := s.loadedSeqs[seq]; !loaded {
		if err := s.pageIn(ctx, seq); err != nil {
			// page-in failed; fall back to SQLite direct fetch.
			return s.fetchFromSQLite(ctx, docID)
		}
	}
	s.touchLRU(seq)

	// ── Return from Bleve ─────────────────────────────────────────────────────
	return s.fetchFromBleve(docID)
}

// Hook returns a CompactionHook that indexes the compacted head messages.
// Each new compaction round is persisted to SQLite (L2) and indexed in Bleve
// (L1), then LRU eviction is applied to both L0 and L1 if caps are exceeded.
func (s *SessionHistorySource) Hook() CompactionHook {
	return func(head []*Message, parts map[string][]*Part) {
		s.mu.Lock()
		defer s.mu.Unlock()

		if s.index == nil {
			return
		}

		s.currentSeq++
		seq := s.currentSeq
		var ids []string
		var recs []Record

		for i, m := range head {
			rec := buildDoc(m, parts[m.ID], seq, i)
			ids = append(ids, m.ID)
			recs = append(recs, rec)
		}

		// Persist atomically: all records for this seq in one transaction.
		// Index into Bleve only after SQLite confirms the write, so that a
		// failed persist never leaves ghost documents in L1 that cannot be
		// evicted (they would violate loadedSeqs ⊆ compactionDocs).
		if s.persistStore != nil {
			if err := s.persistStore.SaveRecords(context.Background(), s.sessionID, recs); err != nil {
				log.Printf("session history hook: SaveRecords seq=%d: %v", seq, err)
				s.currentSeq--
				return
			}
		}

		// SQLite write succeeded (or no persist store); now index into Bleve.
		for i, m := range head {
			_ = s.index.Index(m.ID, recs[i])
		}

		// Add to L0.
		s.compactionDocs[seq] = ids
		for _, id := range ids {
			s.docIndex[id] = seq
		}
		s.lruOrder = append(s.lruOrder, seq)
		s.evictL0IfNeeded()

		// Mark as loaded in L1.
		// NOTE: loadedSeqs is updated AFTER evictL0IfNeeded so that the new seq
		// is not considered a candidate for L1 eviction in the same hook call.
		s.loadedSeqs[seq] = struct{}{}
		s.evictL1IfNeeded()
	}
}

// ── internal: LRU + eviction ──────────────────────────────────────────────────

// touchLRU moves seq to the tail of lruOrder (most recently used).
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

// evictL0IfNeeded removes the LRU-oldest seq from compactionDocs (and from
// Bleve if it happens to be loaded) until len(compactionDocs) ≤ maxIndexedSeqs.
// SQLite data is never touched.
// Must be called with s.mu held.
func (s *SessionHistorySource) evictL0IfNeeded() {
	for len(s.compactionDocs) > s.maxIndexedSeqs {
		oldest := s.lruOldestInL0()
		if oldest < 0 {
			break
		}
		// If also in L1, evict from Bleve first to maintain L1 ⊆ L0.
		ids := s.compactionDocs[oldest]
		if _, inL1 := s.loadedSeqs[oldest]; inL1 {
			for _, id := range ids {
				_ = s.index.Delete(id)
			}
			delete(s.loadedSeqs, oldest)
		}
		delete(s.compactionDocs, oldest)
		for _, id := range ids {
			delete(s.docIndex, id)
		}
		s.removeLRU(oldest)
	}
}

// evictL1IfNeeded removes the LRU-oldest seq from Bleve until
// len(loadedSeqs) ≤ maxCompactions. compactionDocs entry is kept.
// Must be called with s.mu held.
func (s *SessionHistorySource) evictL1IfNeeded() {
	for len(s.loadedSeqs) > s.maxCompactions {
		oldest := s.lruOldestInL1()
		if oldest < 0 {
			break
		}
		for _, id := range s.compactionDocs[oldest] {
			_ = s.index.Delete(id)
		}
		delete(s.loadedSeqs, oldest)
		// lruOrder entry stays — seq is still in L0.
	}
}

// lruOldestInL0 returns the seq at the head of lruOrder that is in compactionDocs.
func (s *SessionHistorySource) lruOldestInL0() int {
	for _, seq := range s.lruOrder {
		if _, ok := s.compactionDocs[seq]; ok {
			return seq
		}
	}
	return -1
}

// lruOldestInL1 returns the seq at the head of lruOrder that is in loadedSeqs.
func (s *SessionHistorySource) lruOldestInL1() int {
	for _, seq := range s.lruOrder {
		if _, ok := s.loadedSeqs[seq]; ok {
			return seq
		}
	}
	return -1
}

// removeLRU removes seq from lruOrder.
func (s *SessionHistorySource) removeLRU(seq int) {
	for i, v := range s.lruOrder {
		if v == seq {
			s.lruOrder = append(s.lruOrder[:i], s.lruOrder[i+1:]...)
			return
		}
	}
}

// ── internal: page-in / L0 promotion ─────────────────────────────────────────

// pageIn loads all Records for seq from SQLite into Bleve.
// Assumes seq is already in compactionDocs (L0).
// Must be called with s.mu held.
func (s *SessionHistorySource) pageIn(ctx context.Context, seq int) error {
	if s.persistStore == nil {
		return fmt.Errorf("no persist store for page-in")
	}
	recs, err := s.persistStore.LoadRecordsBySeq(ctx, s.sessionID, seq)
	if err != nil {
		return fmt.Errorf("pageIn seq=%d: %w", seq, err)
	}
	for _, rec := range recs {
		_ = s.index.Index(rec.ID, rec)
	}
	s.loadedSeqs[seq] = struct{}{}
	// Touch LRU so the freshly page-in'd seq is treated as most recently used
	// and is not immediately evicted by evictL1IfNeeded.
	s.touchLRU(seq)
	s.evictL1IfNeeded()
	return nil
}

// addToL0 fetches the doc IDs for seq from SQLite and inserts the seq into
// compactionDocs, then applies L0 eviction if needed.
// Must be called with s.mu held.
func (s *SessionHistorySource) addToL0(ctx context.Context, seq int) {
	if _, exists := s.compactionDocs[seq]; exists {
		return
	}
	if s.persistStore == nil {
		return
	}
	// Load just the ID list for this seq (lightweight: uses LoadSeqIndex with limit=1 seq).
	// We re-use LoadRecordsBySeq and extract IDs since there's no single-seq index method.
	recs, err := s.persistStore.LoadRecordsBySeq(ctx, s.sessionID, seq)
	if err != nil || len(recs) == 0 {
		return
	}
	ids := make([]string, len(recs))
	for i, r := range recs {
		ids[i] = r.ID
	}
	s.compactionDocs[seq] = ids
	for _, id := range ids {
		s.docIndex[id] = seq
	}
	s.lruOrder = append(s.lruOrder, seq)
	s.evictL0IfNeeded()
}

// seqForDoc returns the seq that owns docID using the reverse index (O(1)).
func (s *SessionHistorySource) seqForDoc(docID string) (int, bool) {
	seq, ok := s.docIndex[docID]
	return seq, ok
}

// ── internal: Bleve helpers ───────────────────────────────────────────────────

// bleveSearch runs a search against the Bleve index and returns results +
// the set of seqs that contributed hits.
// Must be called with s.mu held.
func (s *SessionHistorySource) bleveSearch(ctx context.Context, input string, size int) ([]knowledge.Result, map[int]struct{}) {
	input = strings.TrimSpace(input)
	var bq bquery.Query
	if input == "" {
		bq = bleve.NewMatchAllQuery()
	} else {
		bq = bleve.NewQueryStringQuery(input)
	}

	req := bleve.NewSearchRequestOptions(bq, size, 0, false)
	req.Fields = []string{fieldRole, "turn_index", "compaction_seq"}
	req.Highlight = bleve.NewHighlightWithStyle("html")
	req.Highlight.AddField(fieldText)

	res, err := s.index.SearchInContext(ctx, req)
	if err != nil || res == nil {
		return nil, nil
	}

	results := make([]knowledge.Result, 0, len(res.Hits))
	seqs := make(map[int]struct{})

	for _, hit := range res.Hits {
		seq := int(toFloat64(hit.Fields["compaction_seq"]))
		turnIdx := int(toFloat64(hit.Fields["turn_index"]))
		role := fmt.Sprintf("%v", hit.Fields[fieldRole])
		seqs[seq] = struct{}{}

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
		results = append(results, knowledge.Result{
			RefID:   sessionHistorySourceID + ":" + hit.ID,
			Title:   title,
			Source:  sessionHistorySourceID,
			Score:   score,
			Snippet: title + "\n" + snippet,
		})
	}
	return results, seqs
}

// fetchFromBleve retrieves the full document from Bleve by docID.
// Must be called with s.mu held.
func (s *SessionHistorySource) fetchFromBleve(docID string) ([]knowledge.Result, error) {
	doc, err := s.index.Document(docID)
	if err != nil || doc == nil {
		return nil, fmt.Errorf("session history fetch %q: not found in bleve", docID)
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

	return []knowledge.Result{{
		RefID:   sessionHistorySourceID + ":" + docID,
		Title:   title,
		Source:  sessionHistorySourceID,
		Score:   -1,
		Content: title + "\n\n" + text,
		Metadata: map[string]any{
			"compaction_seq": seq,
			"turn_index":     turnIdx,
			"role":           role,
		},
	}}, nil
}

// fetchFromSQLite falls back to the persistStore Source.Fetch when Bleve
// page-in failed. Must be called with s.mu held (releases mu temporarily
// is not needed since SQLite access is via the store, not re-entrant).
func (s *SessionHistorySource) fetchFromSQLite(ctx context.Context, docID string) ([]knowledge.Result, error) {
	if s.persistStore == nil {
		return nil, fmt.Errorf("session history fetch %q: no fallback available", docID)
	}
	src, ok := s.persistStore.(knowledge.Source)
	if !ok {
		return nil, fmt.Errorf("session history fetch %q: persist store is not a Source", docID)
	}
	return src.Fetch(ctx, knowledge.Query{
		Type:  knowledge.QueryTypeFetch,
		Input: sessionHistorySourceID + ":" + docID,
	})
}

// storedText retrieves the raw text field of a document from Bleve.
// Must be called with s.mu held.
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

// ── document builder ──────────────────────────────────────────────────────────

func buildDoc(m *Message, parts []*Part, seq, turnIdx int) Record {
	var textParts []string
	var toolCalls []string

	for _, p := range parts {
		switch p.Type {
		case PartTypeText:
			d, ok := DataAs[*TextPartData](p)
			if ok && d.Text != "" {
				textParts = append(textParts, d.Text)
			}
		case PartTypeTool:
			d, ok := DataAs[*ToolPartData](p)
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

// ── Bleve index mapping ───────────────────────────────────────────────────────

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

// ── misc helpers ──────────────────────────────────────────────────────────────

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

// sortInts sorts a slice of ints in ascending order (insertion sort; seqs
// slices are small so stdlib sort overhead is not needed).
func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		key := a[i]
		j := i - 1
		for j >= 0 && a[j] > key {
			a[j+1] = a[j]
			j--
		}
		a[j+1] = key
	}
}
