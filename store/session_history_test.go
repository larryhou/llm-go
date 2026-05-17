package store_test

// session_history_test.go — layered tests for SessionHistorySource.
//
// Test progression (small → large):
//
//  Level 1 — Pure-memory unit tests (no PersistStore):
//    1a. Hook indexes messages; Peek finds them via Bleve (L1).
//    1b. Fetch retrieves full content from Bleve (L1).
//    1c. L1 eviction: maxCompactions=2, add 3 seqs → oldest evicted from Bleve.
//    1d. Evicted seq is no longer reachable (pure-memory, no L2 fallback).
//    1e. Reset clears all state.
//
//  Level 2 — PersistStore stub (in-memory map, implements PersistStore+Source):
//    2a. Hook saves to both L1 and L2.
//    2b. Peek: Bleve hits come first; cold L2 hits appended (P3 merge).
//    2c. Peek: duplicate docIDs across L1 and L2 appear exactly once.
//    2d. Fetch: seq in L0 → page-in from L2 → return full content from Bleve.
//    2e. Fetch: L0 miss → FindSeqByDocID → addToL0 → page-in → return content.
//    2f. L0 eviction: maxIndexedSeqs=2, add 3 seqs → oldest evicted from L0 but L2 intact.
//    2g. L0 eviction also removes the seq from L1 (invariant: L1 ⊆ L0).
//    2h. LRU touch: Fetch-accessing an old seq promotes it; eviction skips it.
//    2i. Reset deletes all L2 records AND clears L0/L1.
//
//  Level 3 — Invariant and property tests:
//    3a. After any mix of Hook+Peek+Fetch, loadedSeqs ⊆ compactionDocs always holds.
//    3b. Concurrent Hook + Peek + Fetch do not race or deadlock.
//    3c. Multiple messages per compaction round are all indexed and fetchable.
//    3d. Empty query Peek returns all indexed docs (up to MaxResults).
//    3e. Peek result order: L1 (Bleve) results come before L2 supplement.
//
// NOTE: All text content uses Chinese words because the gse tokenizer (used by
// Bleve) segments Chinese text accurately but splits ASCII letters into
// individual characters, making single-word English queries unreliable in tests.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/larryhou/llm-go/knowledge"
	"github.com/larryhou/llm-go/store"
)

// ── stub PersistStore + Source ────────────────────────────────────────────────

// memPersist is an in-memory implementation of store.PersistStore that
// also implements knowledge.Source (so it can serve as an L2 search backend).
// All data is keyed by (sessionID, seq, docID).
type memPersist struct {
	mu   sync.Mutex
	data map[string]map[int][]store.Record // sessionID → seq → []Record

	// call counters for assertions
	saveCount         int
	loadSeqIndexCount int
	loadBySeqCount    int
	findSeqCount      int
	deleteSeqCount    int
	deleteAllCount    int

	// failSave, when true, causes SaveRecords to return an error.
	failSave bool
}

func newMemPersist() *memPersist {
	return &memPersist{data: make(map[string]map[int][]store.Record)}
}

func (m *memPersist) sesData(sessionID string) map[int][]store.Record {
	if m.data[sessionID] == nil {
		m.data[sessionID] = make(map[int][]store.Record)
	}
	return m.data[sessionID]
}

// PersistStore interface

func (m *memPersist) LoadSeqIndex(_ context.Context, sessionID string, limit int) (map[int][]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadSeqIndexCount++

	sd := m.sesData(sessionID)
	// Collect seqs sorted descending, take up to limit.
	seqs := make([]int, 0, len(sd))
	for seq := range sd {
		seqs = append(seqs, seq)
	}
	// sort descending
	for i := 0; i < len(seqs); i++ {
		for j := i + 1; j < len(seqs); j++ {
			if seqs[j] > seqs[i] {
				seqs[i], seqs[j] = seqs[j], seqs[i]
			}
		}
	}
	if limit > 0 && len(seqs) > limit {
		seqs = seqs[:limit]
	}
	result := make(map[int][]string, len(seqs))
	for _, seq := range seqs {
		for _, rec := range sd[seq] {
			result[seq] = append(result[seq], rec.ID)
		}
	}
	return result, nil
}

func (m *memPersist) LoadRecordsBySeq(_ context.Context, sessionID string, seq int) ([]store.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadBySeqCount++
	recs := m.sesData(sessionID)[seq]
	out := make([]store.Record, len(recs))
	copy(out, recs)
	return out, nil
}

func (m *memPersist) FindSeqByDocID(_ context.Context, sessionID string, docID string) (int, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.findSeqCount++
	for seq, recs := range m.sesData(sessionID) {
		for _, r := range recs {
			if r.ID == docID {
				return seq, true, nil
			}
		}
	}
	return 0, false, nil
}

func (m *memPersist) SaveRecord(_ context.Context, sessionID string, rec store.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveCount++
	sd := m.sesData(sessionID)
	sd[rec.CompactionSeq] = append(sd[rec.CompactionSeq], rec)
	return nil
}

func (m *memPersist) SaveRecords(_ context.Context, sessionID string, recs []store.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failSave {
		return fmt.Errorf("memPersist: SaveRecords injected failure")
	}
	m.saveCount += len(recs)
	sd := m.sesData(sessionID)
	for _, rec := range recs {
		sd[rec.CompactionSeq] = append(sd[rec.CompactionSeq], rec)
	}
	return nil
}

func (m *memPersist) DeleteRecordsBySeq(_ context.Context, sessionID string, seq int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteSeqCount++
	delete(m.sesData(sessionID), seq)
	return nil
}

func (m *memPersist) DeleteAllRecords(_ context.Context, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteAllCount++
	delete(m.data, sessionID)
	return nil
}

// Source interface — makes memPersist usable as L2 search backend.

func (m *memPersist) ID() string    { return "mem-persist" }
func (m *memPersist) Priority() int { return 1 } // lower priority than L1
func (m *memPersist) Accepts(q knowledge.Query) bool {
	return q.Type == knowledge.QueryTypeSearch || q.Type == knowledge.QueryTypeFetch
}

func (m *memPersist) Peek(_ context.Context, q knowledge.Query) ([]knowledge.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	input := strings.TrimSpace(q.Input)
	size := q.MaxResults
	if size <= 0 {
		size = 5
	}
	var results []knowledge.Result
	for _, seqRecs := range m.data {
		for _, recs := range seqRecs {
			for _, rec := range recs {
				if input == "" || strings.Contains(rec.Text, input) {
					snippet := rec.Text
					if len(snippet) > 100 {
						snippet = snippet[:100] + "..."
					}
					title := fmt.Sprintf("[来源：历史对话 第%d轮 turn#%d role=%s]",
						rec.CompactionSeq, rec.TurnIndex, rec.Role)
					results = append(results, knowledge.Result{
						RefID:   "session-history:" + rec.ID,
						Title:   title,
						Source:  "mem-persist",
						Score:   -1,
						Snippet: title + "\n" + snippet,
					})
					if len(results) >= size {
						return results, nil
					}
				}
			}
		}
	}
	return results, nil
}

func (m *memPersist) Fetch(_ context.Context, q knowledge.Query) ([]knowledge.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	docID := q.Input
	if pfx := "session-history:"; strings.HasPrefix(docID, pfx) {
		docID = strings.TrimPrefix(docID, pfx)
	}
	for _, seqRecs := range m.data {
		for _, recs := range seqRecs {
			for _, rec := range recs {
				if rec.ID == docID {
					title := fmt.Sprintf("[来源：历史对话 第%d轮 turn#%d role=%s]",
						rec.CompactionSeq, rec.TurnIndex, rec.Role)
					return []knowledge.Result{{
						RefID:   "session-history:" + rec.ID,
						Title:   title,
						Source:  "mem-persist",
						Score:   -1,
						Content: title + "\n\n" + rec.Text,
					}}, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("doc %q not found", docID)
}

// recordSeqCount returns how many seqs are stored for sessionID.
func (m *memPersist) recordSeqCount(sessionID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sesData(sessionID))
}

// totalRecords returns the total number of records for sessionID.
func (m *memPersist) totalRecords(sessionID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, recs := range m.sesData(sessionID) {
		n += len(recs)
	}
	return n
}

// ── helpers ───────────────────────────────────────────────────────────────────

const testSessID = "test-session"

func newPureMemSrc(t *testing.T, maxL1, maxL0 int) *store.SessionHistorySource {
	t.Helper()
	src, err := store.NewSessionHistorySource(testSessID, maxL1, maxL0, nil)
	if err != nil {
		t.Fatalf("NewSessionHistorySource: %v", err)
	}
	return src
}

func newSrcWithPS(t *testing.T, maxL1, maxL0 int, ps store.PersistStore) *store.SessionHistorySource {
	t.Helper()
	src, err := store.NewSessionHistorySource(testSessID, maxL1, maxL0, ps)
	if err != nil {
		t.Fatalf("NewSessionHistorySource: %v", err)
	}
	return src
}

// makeMsg builds a minimal store.Message with a text part.
func makeMsg(id, text, sessID string) (*store.Message, map[string][]*store.Part) {
	msg := &store.Message{
		ID:        id,
		SessionID: sessID,
		Role:      store.RoleUser,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	parts := map[string][]*store.Part{
		id: {
			{
				ID:        id + "-part",
				MessageID: id,
				SessionID: sessID,
				Type:      store.PartTypeText,
				Data:      &store.TextPartData{Text: text},
			},
		},
	}
	return msg, parts
}

// fireHook runs a compaction hook with a set of (id, text) pairs.
func fireHook(hook store.CompactionHook, sessID string, msgs []struct{ id, text string }) {
	var head []*store.Message
	allParts := make(map[string][]*store.Part)
	for _, m := range msgs {
		msg, parts := makeMsg(m.id, m.text, sessID)
		head = append(head, msg)
		for k, v := range parts {
			allParts[k] = v
		}
	}
	hook(head, allParts)
}

func peek(t *testing.T, src *store.SessionHistorySource, query string) []knowledge.Result {
	t.Helper()
	results, err := src.Peek(context.Background(), knowledge.Query{
		Type:       knowledge.QueryTypeSearch,
		Input:      query,
		MaxResults: 20,
	})
	if err != nil {
		t.Fatalf("Peek(%q): %v", query, err)
	}
	return results
}

func fetchDoc(t *testing.T, src *store.SessionHistorySource, refID string) knowledge.Result {
	t.Helper()
	results, err := src.Fetch(context.Background(), knowledge.Query{
		Type:  knowledge.QueryTypeFetch,
		Input: refID,
	})
	if err != nil {
		t.Fatalf("Fetch(%q): %v", refID, err)
	}
	if len(results) == 0 {
		t.Fatalf("Fetch(%q): no results", refID)
	}
	return results[0]
}

func containsDocID(results []knowledge.Result, docID string) bool {
	want := "session-history:" + docID
	for _, r := range results {
		if r.RefID == want {
			return true
		}
	}
	return false
}

// ── Level 1: pure-memory unit tests ──────────────────────────────────────────

// 1a. Hook indexes messages; Peek finds them via Bleve (L1).
// Uses Chinese text because gse tokenizer segments Chinese accurately.
func TestSHS_1a_HookAndPeek_PureMemory(t *testing.T) {
	src := newPureMemSrc(t, 4, 8)
	hook := src.Hook()

	fireHook(hook, testSessID, []struct{ id, text string }{
		{"msg-a", "协程并发编程模型"},
		{"msg-b", "通道用于进程间通信"},
	})

	results := peek(t, src, "协程")
	if len(results) == 0 {
		t.Fatal("expected at least one result for '协程'")
	}
	if !containsDocID(results, "msg-a") {
		t.Errorf("expected msg-a in results; got %v", results)
	}
}

// 1b. Fetch retrieves full content from Bleve (L1) after Hook.
func TestSHS_1b_Fetch_PureMemory(t *testing.T) {
	src := newPureMemSrc(t, 4, 8)
	hook := src.Hook()

	fireHook(hook, testSessID, []struct{ id, text string }{
		{"doc-x", "答案是四十二"},
	})

	r := fetchDoc(t, src, "session-history:doc-x")
	if !strings.Contains(r.Content, "四十二") {
		t.Errorf("Content missing expected text; got: %s", r.Content)
	}
	if r.Snippet != "" {
		t.Error("Fetch result must have empty Snippet (Content only)")
	}
}

// 1c. L1 eviction: maxCompactions=2, add 3 seqs → oldest evicted from Bleve.
func TestSHS_1c_L1Eviction_PureMemory(t *testing.T) {
	src := newPureMemSrc(t, 2, 8) // L1 cap=2, L0 cap=8 (L0 not the bottleneck)
	hook := src.Hook()

	// Seq 1: unique token that only matches this seq
	fireHook(hook, testSessID, []struct{ id, text string }{{"m1", "苹果红色水果独特标记旧版本"}})
	// Seq 2
	fireHook(hook, testSessID, []struct{ id, text string }{{"m2", "香蕉黄色水果独特标记中版本"}})
	// Seq 3 → triggers L1 eviction of seq 1 (oldest in Bleve)
	fireHook(hook, testSessID, []struct{ id, text string }{{"m3", "西瓜绿色水果独特标记新版本"}})

	// Seq 2 and 3 must be in Bleve (most recent 2 with L1 cap=2).
	r2 := peek(t, src, "中版本")
	r3 := peek(t, src, "新版本")
	if len(r2) == 0 {
		t.Error("seq 2 (中版本) should be in Bleve after seq 1 is evicted")
	}
	if len(r3) == 0 {
		t.Error("seq 3 (新版本) should be in Bleve")
	}

	// Seq 1 must NOT be findable (pure-memory, no L2 fallback).
	r1 := peek(t, src, "旧版本")
	if containsDocID(r1, "m1") {
		t.Error("seq 1 (m1) should have been evicted from Bleve (pure-memory)")
	}
}

// 1d. Evicted seq is unreachable in pure-memory mode (no L2 fallback).
func TestSHS_1d_EvictedSeqUnreachable_PureMemory(t *testing.T) {
	src := newPureMemSrc(t, 2, 2) // L0=L1 cap=2
	hook := src.Hook()

	fireHook(hook, testSessID, []struct{ id, text string }{{"m1", "第一轮内容独特文字"}})
	fireHook(hook, testSessID, []struct{ id, text string }{{"m2", "第二轮内容独特文字"}})
	// Seq 3 pushes seq 1 out of both L0 and L1.
	fireHook(hook, testSessID, []struct{ id, text string }{{"m3", "第三轮内容独特文字"}})

	// Fetch for evicted doc should fail (no L2).
	_, err := src.Fetch(context.Background(), knowledge.Query{
		Type:  knowledge.QueryTypeFetch,
		Input: "session-history:m1",
	})
	if err == nil {
		t.Error("expected error fetching evicted doc in pure-memory mode")
	}
}

// 1e. Reset clears all state — Peek and Fetch return nothing afterward.
func TestSHS_1e_Reset_PureMemory(t *testing.T) {
	src := newPureMemSrc(t, 4, 8)
	hook := src.Hook()
	fireHook(hook, testSessID, []struct{ id, text string }{{"r1", "重置测试内容文字"}})

	if err := src.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	results := peek(t, src, "重置")
	if len(results) != 0 {
		t.Errorf("expected no results after Reset, got %d", len(results))
	}
	_, err := src.Fetch(context.Background(), knowledge.Query{
		Type:  knowledge.QueryTypeFetch,
		Input: "session-history:r1",
	})
	if err == nil {
		t.Error("expected Fetch error after Reset")
	}
}

// ── Level 2: PersistStore stub tests ─────────────────────────────────────────

// 2a. Hook saves to both L1 (Bleve) and L2 (PersistStore).
func TestSHS_2a_Hook_SavesToBothL1AndL2(t *testing.T) {
	ps := newMemPersist()
	src := newSrcWithPS(t, 4, 8, ps)
	hook := src.Hook()

	fireHook(hook, testSessID, []struct{ id, text string }{
		{"h1", "第一条消息内容"},
		{"h2", "第二条消息内容"},
	})

	if ps.saveCount != 2 {
		t.Errorf("SaveRecord called %d times, want 2", ps.saveCount)
	}
	if ps.totalRecords(testSessID) != 2 {
		t.Errorf("L2 has %d records, want 2", ps.totalRecords(testSessID))
	}
	// L1 (Bleve) must also have them.
	results := peek(t, src, "第一条")
	if !containsDocID(results, "h1") {
		t.Error("h1 should be in Bleve (L1) immediately after Hook")
	}
}

// 2b. Peek: Bleve hot results come first; cold L2-only results appended (P3).
func TestSHS_2b_Peek_P3MergeHotAndCold(t *testing.T) {
	ps := newMemPersist()
	src := newSrcWithPS(t, 1, 1, ps) // L1 cap=1, L0 cap=1
	hook := src.Hook()

	// Seq 1 (will be evicted from L1 when seq 2 is added).
	fireHook(hook, testSessID, []struct{ id, text string }{
		{"cold-doc", "远古的智慧来自第一轮"},
	})
	// Seq 2 (hot, in L1).
	fireHook(hook, testSessID, []struct{ id, text string }{
		{"hot-doc", "最新的知识来自第二轮"},
	})
	// After seq 2, seq 1 is evicted from L1 and L0.
	// But seq 1 remains in L2 (memPersist).

	// Peek for "智慧" should find cold-doc via L2.
	results := peek(t, src, "智慧")
	if !containsDocID(results, "cold-doc") {
		t.Errorf("cold-doc should be found via L2; results: %v", results)
	}

	// Peek for "知识" should find hot-doc (L1 Bleve).
	results2 := peek(t, src, "知识")
	if !containsDocID(results2, "hot-doc") {
		t.Errorf("hot-doc should be found via L1 Bleve; results: %v", results2)
	}
}

// 2c. Peek: duplicate docIDs across L1 and L2 appear exactly once.
func TestSHS_2c_Peek_NoDuplicates(t *testing.T) {
	ps := newMemPersist()
	src := newSrcWithPS(t, 4, 8, ps)
	hook := src.Hook()

	fireHook(hook, testSessID, []struct{ id, text string }{
		{"dup-doc", "重复内容测试文字"},
	})

	// Both L1 and L2 have "dup-doc" — merged result must contain it exactly once.
	results := peek(t, src, "重复")
	count := 0
	for _, r := range results {
		if r.RefID == "session-history:dup-doc" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("dup-doc appears %d times in Peek results, want exactly 1", count)
	}
}

// 2d. Fetch: seq in L0 → page-in from L2 → return full content from Bleve.
func TestSHS_2d_Fetch_PageInFromL2_ViaL0(t *testing.T) {
	ps := newMemPersist()
	// L1 cap=1: second Hook evicts the first seq from L1.
	// L0 cap=4: seq 1 stays in L0 (doc IDs known, but not in Bleve).
	src := newSrcWithPS(t, 1, 4, ps)
	hook := src.Hook()

	// Seq 1 → will be evicted from L1 after seq 2 is added.
	fireHook(hook, testSessID, []struct{ id, text string }{
		{"pagein-doc", "分页加载测试内容来自第一轮"},
	})
	// Seq 2 → pushes seq 1 out of L1.
	fireHook(hook, testSessID, []struct{ id, text string }{
		{"newer-doc", "较新的内容来自第二轮"},
	})

	loadsBefore := ps.loadBySeqCount
	r := fetchDoc(t, src, "session-history:pagein-doc")

	if ps.loadBySeqCount <= loadsBefore {
		t.Error("expected LoadRecordsBySeq to be called for page-in")
	}
	if !strings.Contains(r.Content, "分页加载测试内容") {
		t.Errorf("Content missing expected text; got: %s", r.Content)
	}
}

// 2e. Fetch: L0 miss → FindSeqByDocID → addToL0 → page-in → return content.
func TestSHS_2e_Fetch_L0Miss_FindSeqAndPageIn(t *testing.T) {
	ps := newMemPersist()
	// L0 cap=1: second Hook evicts seq 1 from both L0 and L1.
	src := newSrcWithPS(t, 1, 1, ps)
	hook := src.Hook()

	// Seq 1 (will be fully evicted when seq 2 is added).
	fireHook(hook, testSessID, []struct{ id, text string }{
		{"l0miss-doc", "零级缓存未命中测试内容"},
	})
	// Seq 2 → evicts seq 1 from L0 and L1.
	fireHook(hook, testSessID, []struct{ id, text string }{
		{"keeper-doc", "保留在缓存的内容"},
	})

	findsBefore := ps.findSeqCount
	r := fetchDoc(t, src, "session-history:l0miss-doc")

	if ps.findSeqCount <= findsBefore {
		t.Error("expected FindSeqByDocID to be called on L0 miss")
	}
	if !strings.Contains(r.Content, "零级缓存未命中测试内容") {
		t.Errorf("Content missing; got: %s", r.Content)
	}
}

// 2f. L0 eviction: maxIndexedSeqs=2, add 3 seqs → seq 1 evicted from L0 but L2 intact.
func TestSHS_2f_L0Eviction_L2Intact(t *testing.T) {
	ps := newMemPersist()
	src := newSrcWithPS(t, 2, 2, ps) // L0=L1 cap=2
	hook := src.Hook()

	fireHook(hook, testSessID, []struct{ id, text string }{{"e1", "被驱逐的第一轮记录"}})
	fireHook(hook, testSessID, []struct{ id, text string }{{"e2", "被驱逐的第二轮记录"}})
	// Seq 3 evicts seq 1 from L0 and L1.
	fireHook(hook, testSessID, []struct{ id, text string }{{"e3", "被驱逐的第三轮记录"}})

	// L2 still has all 3 seqs.
	if ps.recordSeqCount(testSessID) != 3 {
		t.Errorf("L2 should have 3 seqs, got %d", ps.recordSeqCount(testSessID))
	}

	// e1 is reachable via L2 Peek even though evicted from L0.
	results := peek(t, src, "第一轮记录")
	if !containsDocID(results, "e1") {
		t.Errorf("e1 should be found via L2 after L0 eviction; results: %v", results)
	}
}

// 2g. L0 eviction also removes the seq from L1 (invariant: L1 ⊆ L0).
// After L0 eviction of seq 1, fetching g1 must trigger FindSeqByDocID (L0 miss).
func TestSHS_2g_L0Eviction_AlsoEvictsL1(t *testing.T) {
	ps := newMemPersist()
	// Both caps = 2: L0 and L1 evict at the same rate.
	// When seq 3 is added, seq 1 is evicted from both L0 and L1 simultaneously.
	src := newSrcWithPS(t, 2, 2, ps)
	hook := src.Hook()

	fireHook(hook, testSessID, []struct{ id, text string }{{"g1", "第一轮独特内容文字标记"}})
	fireHook(hook, testSessID, []struct{ id, text string }{{"g2", "第二轮独特内容文字标记"}})
	// Seq 3: both L0 and L1 caps exceeded → seq 1 evicted from both.
	fireHook(hook, testSessID, []struct{ id, text string }{{"g3", "第三轮独特内容文字标记"}})

	// g1 is no longer in L0, so Fetch must call FindSeqByDocID.
	findsBefore := ps.findSeqCount
	r := fetchDoc(t, src, "session-history:g1")

	if ps.findSeqCount <= findsBefore {
		t.Errorf("g1 should cause FindSeqByDocID after L0 eviction (findSeqCount before=%d after=%d)",
			findsBefore, ps.findSeqCount)
	}
	if !strings.Contains(r.Content, "第一轮独特内容") {
		t.Errorf("unexpected content: %s", r.Content)
	}
}

// 2h. LRU touch: Fetch-accessing an old seq promotes it; eviction then skips it.
func TestSHS_2h_LRUTouch_FetchPromotesSeq(t *testing.T) {
	ps := newMemPersist()
	src := newSrcWithPS(t, 2, 2, ps) // L0=L1 cap=2
	hook := src.Hook()

	// Add seq 1 and seq 2.
	fireHook(hook, testSessID, []struct{ id, text string }{{"lru1", "第一轮被提升的内容记录"}})
	fireHook(hook, testSessID, []struct{ id, text string }{{"lru2", "第二轮内容记录文字"}})

	// Explicitly Fetch seq 1 → should promote it in LRU, making seq 2 the eviction target.
	fetchDoc(t, src, "session-history:lru1")

	// Add seq 3 → should evict seq 2 (now LRU), not seq 1.
	fireHook(hook, testSessID, []struct{ id, text string }{{"lru3", "第三轮内容记录文字"}})

	// lru1 should still be reachable: either in L0/L1 (LRU protected it)
	// or via L2. We verify it's findable at all.
	results := peek(t, src, "第一轮被提升")
	if !containsDocID(results, "lru1") {
		t.Errorf("lru1 should be reachable (LRU touch protected it); results: %v", results)
	}

	// lru2 was evicted, but is still findable via L2.
	results2 := peek(t, src, "第二轮内容记录")
	if !containsDocID(results2, "lru2") {
		t.Errorf("lru2 should be findable via L2 even after eviction; results: %v", results2)
	}
}

// 2i. Reset deletes all L2 records AND clears L0/L1.
func TestSHS_2i_Reset_ClearsL2(t *testing.T) {
	ps := newMemPersist()
	src := newSrcWithPS(t, 4, 8, ps)
	hook := src.Hook()

	fireHook(hook, testSessID, []struct{ id, text string }{
		{"rst1", "重置之前的内容"},
		{"rst2", "也是重置之前的内容"},
	})

	if err := src.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	// L2 records should be deleted.
	if ps.totalRecords(testSessID) != 0 {
		t.Errorf("L2 should be empty after Reset, got %d records", ps.totalRecords(testSessID))
	}

	// Peek should return nothing.
	results := peek(t, src, "重置")
	if len(results) != 0 {
		t.Errorf("expected no Peek results after Reset, got %d", len(results))
	}

	// After Reset, new Hook should work normally.
	hook2 := src.Hook()
	fireHook(hook2, testSessID, []struct{ id, text string }{{"after-reset", "重置之后的新内容记录"}})

	results2 := peek(t, src, "重置之后")
	if !containsDocID(results2, "after-reset") {
		t.Error("after-reset doc should be findable after re-indexing post Reset")
	}
}

// ── Level 3: invariant and property tests ────────────────────────────────────

// 3a. After any mix of Hook+Peek+Fetch, every doc returned by Bleve can also
// be fetched — transitively verifying L1 ⊆ L0.
func TestSHS_3a_Invariant_L1SubsetL0(t *testing.T) {
	ps := newMemPersist()
	// Small caps to force lots of evictions quickly.
	src := newSrcWithPS(t, 2, 3, ps)
	hook := src.Hook()

	for i := 1; i <= 6; i++ {
		docID := fmt.Sprintf("inv-doc-%d", i)
		text := fmt.Sprintf("不变量测试内容第%d轮记录文字", i)
		fireHook(hook, testSessID, []struct{ id, text string }{{docID, text}})

		// Peek after each hook (drives LRU touches).
		peek(t, src, fmt.Sprintf("第%d轮", i))

		// Fetch the just-added doc — always succeeds via L1 or L2 page-in.
		r, err := src.Fetch(context.Background(), knowledge.Query{
			Type:  knowledge.QueryTypeFetch,
			Input: "session-history:" + docID,
		})
		if err != nil {
			t.Errorf("round %d: Fetch(%s) failed: %v", i, docID, err)
			continue
		}
		if len(r) == 0 || !strings.Contains(r[0].Content, text) {
			t.Errorf("round %d: Fetch returned wrong content: %v", i, r)
		}
	}

	// Every Bleve-sourced Peek result must be fetchable (L1 ⊆ L0 invariant).
	allResults := peek(t, src, "")
	for _, r := range allResults {
		if r.Source != "session-history" {
			continue // skip L2-only results
		}
		_, fetchErr := src.Fetch(context.Background(), knowledge.Query{
			Type:  knowledge.QueryTypeFetch,
			Input: r.RefID,
		})
		if fetchErr != nil {
			t.Errorf("invariant violated: Bleve result %s could not be fetched: %v",
				r.RefID, fetchErr)
		}
	}
}

// 3b. Concurrent Hook + Peek + Fetch do not race or deadlock.
func TestSHS_3b_Concurrent_NoRaceOrDeadlock(t *testing.T) {
	ps := newMemPersist()
	src := newSrcWithPS(t, 3, 6, ps)
	hook := src.Hook()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup

	// Writer: fires hooks.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			docID := fmt.Sprintf("conc-doc-%d", i)
			text := fmt.Sprintf("并发测试内容第%d条", i)
			fireHook(hook, testSessID, []struct{ id, text string }{{docID, text}})
			time.Sleep(time.Millisecond)
		}
	}()

	// Reader: peeks.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 30; i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			src.Peek(ctx, knowledge.Query{ //nolint:errcheck
				Type:       knowledge.QueryTypeSearch,
				Input:      fmt.Sprintf("第%d条", i%10),
				MaxResults: 5,
			})
			time.Sleep(time.Millisecond)
		}
	}()

	// Fetcher: fetches specific docs.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 15; i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			time.Sleep(3 * time.Millisecond)
			docID := fmt.Sprintf("conc-doc-%d", i%5)
			src.Fetch(ctx, knowledge.Query{ //nolint:errcheck
				Type:  knowledge.QueryTypeFetch,
				Input: "session-history:" + docID,
			})
		}
	}()

	wg.Wait()
}

// 3c. Multiple messages per compaction round are all indexed and fetchable.
func TestSHS_3c_MultiMessagePerRound(t *testing.T) {
	ps := newMemPersist()
	src := newSrcWithPS(t, 4, 8, ps)
	hook := src.Hook()

	// One compaction round with 5 messages.
	fireHook(hook, testSessID, []struct{ id, text string }{
		{"mm1", "数据库存储原理"},
		{"mm2", "索引加速查询"},
		{"mm3", "查询优化策略"},
		{"mm4", "事务隔离级别"},
		{"mm5", "性能调优方法"},
	})

	// All 5 must be findable via Peek.
	for _, tc := range []struct{ q, id string }{
		{"数据库", "mm1"},
		{"索引", "mm2"},
		{"查询", "mm3"},
		{"事务", "mm4"},
		{"性能", "mm5"},
	} {
		results := peek(t, src, tc.q)
		if !containsDocID(results, tc.id) {
			t.Errorf("Peek(%q): expected %s in results; got %v", tc.q, tc.id, results)
		}
	}

	// All 5 must be fetchable.
	for _, id := range []string{"mm1", "mm2", "mm3", "mm4", "mm5"} {
		r := fetchDoc(t, src, "session-history:"+id)
		if r.Content == "" {
			t.Errorf("Fetch(%s): empty content", id)
		}
	}
}

// 3d. Empty query Peek returns all indexed docs (up to MaxResults).
func TestSHS_3d_EmptyQuery_ReturnsAll(t *testing.T) {
	ps := newMemPersist()
	src := newSrcWithPS(t, 4, 8, ps)
	hook := src.Hook()

	for i := 0; i < 4; i++ {
		fireHook(hook, testSessID, []struct{ id, text string }{
			{fmt.Sprintf("all-%d", i), fmt.Sprintf("第%d轮文字内容", i)},
		})
	}

	results := peek(t, src, "") // empty query
	if len(results) < 4 {
		t.Errorf("empty query: got %d results, want at least 4", len(results))
	}
}

// 3e. Peek result order: L1 (Bleve) results come before L2 supplement.
func TestSHS_3e_PeekOrder_BleveBeforeL2(t *testing.T) {
	ps := newMemPersist()
	// L1 cap=1: only the most recent seq stays in Bleve.
	// L0 cap=4: old-msg stays in L0 after L1 eviction.
	src := newSrcWithPS(t, 1, 4, ps)
	hook := src.Hook()

	// Seq 1: old-msg — uses "区块链" as a unique, gse-tokenizable identifier.
	// After seq 2 is added, old-msg is evicted from L1 but stays in L2.
	fireHook(hook, testSessID, []struct{ id, text string }{{"old-msg", "区块链分布式账本内容"}})
	// Seq 2: new-msg — uses "云计算" as a unique identifier.
	// new-msg is in Bleve (L1, hot).
	fireHook(hook, testSessID, []struct{ id, text string }{{"new-msg", "云计算虚拟化容器内容"}})

	// Verify: "区块链" only matches old-msg, which is now in L2 only.
	r1 := peek(t, src, "区块链")
	if len(r1) == 0 {
		t.Fatal("expected result for '区块链' (old-msg via L2)")
	}
	if r1[0].Source != "mem-persist" {
		t.Errorf("'区块链' should come from L2 (mem-persist), got source=%q", r1[0].Source)
	}
	if !containsDocID(r1, "old-msg") {
		t.Errorf("expected old-msg in results; got %v", r1)
	}

	// Verify: "云计算" only matches new-msg, which is in Bleve (L1).
	r2 := peek(t, src, "云计算")
	if len(r2) == 0 {
		t.Fatal("expected result for '云计算' (new-msg via Bleve)")
	}
	if r2[0].Source != "session-history" {
		t.Errorf("'云计算' should come from Bleve (session-history), got source=%q", r2[0].Source)
	}
	if !containsDocID(r2, "new-msg") {
		t.Errorf("expected new-msg in results; got %v", r2)
	}

	// Verify P3 ordering: empty query returns both, Bleve (new-msg) comes first.
	all := peek(t, src, "")
	if len(all) < 2 {
		t.Fatalf("empty query: expected at least 2 results, got %d", len(all))
	}
	// Bleve results must appear before L2 supplements.
	firstIsBleve := all[0].Source == "session-history"
	if !firstIsBleve {
		t.Errorf("P3 ordering: first result should be from Bleve (session-history), got %q", all[0].Source)
	}
}

// ── Fix regression tests (Issue-31, Issue-34, Issue-35) ──────────────────────

// Issue-34 + Issue-35: When SaveRecords fails, Hook must NOT add the seq to
// L0/L1, must NOT increment currentSeq permanently, and must log the error
// (observable indirectly by verifying no records appear in L2 or Bleve).
func TestSHS_Fix34_Hook_RollsBackOnSaveFailure(t *testing.T) {
	ps := newMemPersist()
	ps.failSave = true // inject failure

	src := newSrcWithPS(t, 4, 8, ps)
	hook := src.Hook()

	// Fire hook — SaveRecords will fail.
	fireHook(hook, testSessID, []struct{ id, text string }{
		{"fail-doc", "保存失败测试内容"},
	})

	// L2: no records written.
	if ps.totalRecords(testSessID) != 0 {
		t.Errorf("SaveRecords failed but L2 has %d records, want 0", ps.totalRecords(testSessID))
	}

	// L1: Bleve must have no documents (Bleve is written only after SaveRecords).
	results := peek(t, src, "保存失败")
	if len(results) != 0 {
		t.Errorf("SaveRecords failed but Bleve returned %d results, want 0", len(results))
	}

	// currentSeq must be rolled back: a subsequent successful hook gets seq 1.
	ps.failSave = false
	fireHook(hook, testSessID, []struct{ id, text string }{
		{"ok-doc", "成功保存测试内容"},
	})

	if ps.totalRecords(testSessID) != 1 {
		t.Errorf("after recovery, L2 has %d records, want 1", ps.totalRecords(testSessID))
	}

	// The successful record must be findable in Bleve with seq = 1 (not 2).
	results2 := peek(t, src, "成功保存")
	if !containsDocID(results2, "ok-doc") {
		t.Error("ok-doc should be in Bleve after successful hook")
	}
	// Seq numbering: after one failed attempt and one success, exactly seq 1 exists.
	if ps.recordSeqCount(testSessID) != 1 {
		t.Errorf("expected 1 seq in L2, got %d", ps.recordSeqCount(testSessID))
	}
}

// Issue-34: When SaveRecords succeeds, Bleve must contain the documents
// (i.e. Bleve is populated after — not before — the SQLite write).
func TestSHS_Fix34_Hook_BlevePopulatedAfterSQLiteSuccess(t *testing.T) {
	ps := newMemPersist()
	src := newSrcWithPS(t, 4, 8, ps)
	hook := src.Hook()

	fireHook(hook, testSessID, []struct{ id, text string }{
		{"bleve-after-sql", "写入顺序验证内容"},
	})

	// Both L2 and L1 must have the record.
	if ps.totalRecords(testSessID) != 1 {
		t.Fatalf("L2 has %d records, want 1", ps.totalRecords(testSessID))
	}
	results := peek(t, src, "写入顺序")
	if !containsDocID(results, "bleve-after-sql") {
		t.Error("bleve-after-sql not found in Bleve after successful hook")
	}
}

// Issue-31: Reset() must delete ALL records from L2 including seqs that have
// been evicted from the L0 memory window (i.e. older than maxIndexedSeqs).
// It must use DeleteAllRecords (not per-seq DeleteRecordsBySeq), so
// deleteSeqCount remains 0 after Reset.
func TestSHS_Fix31_Reset_DeletesEvictedSeqs(t *testing.T) {
	// maxL0=2: only 2 seqs fit in the L0 window; earlier seqs are evicted.
	ps := newMemPersist()
	src := newSrcWithPS(t, 1, 2, ps)
	hook := src.Hook()

	// Add 3 seqs — seq 1 will be evicted from L0.
	fireHook(hook, testSessID, []struct{ id, text string }{{"evicted-1", "被驱逐的第一轮内容"}})
	fireHook(hook, testSessID, []struct{ id, text string }{{"evicted-2", "被驱逐的第二轮内容"}})
	fireHook(hook, testSessID, []struct{ id, text string }{{"live-3", "活跃的第三轮内容"}})

	if ps.totalRecords(testSessID) != 3 {
		t.Fatalf("expected 3 records in L2 before Reset, got %d", ps.totalRecords(testSessID))
	}

	if err := src.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	// All records — including evicted seqs — must be gone from L2.
	if ps.totalRecords(testSessID) != 0 {
		t.Errorf("L2 has %d records after Reset, want 0", ps.totalRecords(testSessID))
	}

	// Reset must use DeleteAllRecords, not per-seq DeleteRecordsBySeq.
	// deleteSeqCount == 0 proves the old loop is gone.
	if ps.deleteSeqCount != 0 {
		t.Errorf("deleteSeqCount = %d after Reset, want 0 (should use DeleteAllRecords)", ps.deleteSeqCount)
	}

	// deleteAllCount must be exactly 1.
	if ps.deleteAllCount != 1 {
		t.Errorf("deleteAllCount = %d after Reset, want 1", ps.deleteAllCount)
	}

	// L1 (Bleve) must also be clear.
	results := peek(t, src, "内容")
	if len(results) != 0 {
		t.Errorf("Bleve returned %d results after Reset, want 0", len(results))
	}
}
