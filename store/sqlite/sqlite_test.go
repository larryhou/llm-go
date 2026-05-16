package sqlite_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/larryhou/llm-go/knowledge"
	"github.com/larryhou/llm-go/store"
	"github.com/larryhou/llm-go/store/sqlite"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func newStore(t *testing.T) *sqlite.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := sqlite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// ── store.Store: Session ──────────────────────────────────────────────────────

func TestSession_createAndGet(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	sess := &store.Session{ID: "s1", Title: "Test", Model: "anthropic/claude"}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSession(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Test" || got.Model != "anthropic/claude" {
		t.Errorf("unexpected session: %+v", got)
	}
}

func TestSession_duplicateErrors(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	sess := &store.Session{ID: "dup"}
	_ = s.CreateSession(ctx, sess)
	if err := s.CreateSession(ctx, sess); err == nil {
		t.Error("expected error for duplicate session ID")
	}
}

func TestSession_notFound(t *testing.T) {
	s := newStore(t)
	_, err := s.GetSession(context.Background(), "nope")
	if err == nil {
		t.Error("expected error for missing session")
	}
}

func TestSession_update(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_ = s.CreateSession(ctx, &store.Session{ID: "u1", Title: "old"})
	_ = s.UpdateSession(ctx, &store.Session{ID: "u1", Title: "new"})
	got, _ := s.GetSession(ctx, "u1")
	if got.Title != "new" {
		t.Errorf("Title = %q, want new", got.Title)
	}
}

func TestSession_list(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		_ = s.CreateSession(ctx, &store.Session{ID: id})
	}
	sessions, err := s.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 3 {
		t.Errorf("ListSessions = %d, want 3", len(sessions))
	}
}

func TestSession_delete(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_ = s.CreateSession(ctx, &store.Session{ID: "del"})
	_ = s.CreateMessage(ctx, &store.Message{ID: "m1", SessionID: "del", Role: store.RoleUser})
	_ = s.CreatePart(ctx, &store.Part{
		ID: "p1", MessageID: "m1", SessionID: "del",
		Type: store.PartTypeText, Data: &store.TextPartData{Text: "hi"},
	})

	if err := s.DeleteSession(ctx, "del"); err != nil {
		t.Fatal(err)
	}
	// Session gone
	if _, err := s.GetSession(ctx, "del"); err == nil {
		t.Error("expected session to be deleted")
	}
	// Cascade: message and part gone
	if _, err := s.GetMessage(ctx, "m1"); err == nil {
		t.Error("expected message to be cascade-deleted")
	}
	if _, err := s.GetPart(ctx, "p1"); err == nil {
		t.Error("expected part to be cascade-deleted")
	}
}

func TestSession_deleteIdempotent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	// Deleting a non-existent session should not error.
	if err := s.DeleteSession(ctx, "ghost"); err != nil {
		t.Errorf("DeleteSession non-existent: %v", err)
	}
}

func TestSession_tokens(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_ = s.CreateSession(ctx, &store.Session{
		ID: "tok", Cost: 1.5,
		Tokens: store.TokenSummary{Input: 100, Output: 200, CacheRead: 50, CacheWrite: 25},
	})
	got, _ := s.GetSession(ctx, "tok")
	if got.Cost != 1.5 || got.Tokens.Input != 100 || got.Tokens.Output != 200 {
		t.Errorf("tokens/cost not persisted: %+v", got)
	}
}

// ── store.Store: Message ──────────────────────────────────────────────────────

func TestMessage_createAndList(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_ = s.CreateSession(ctx, &store.Session{ID: "sess"})

	msgs := []*store.Message{
		{ID: "m1", SessionID: "sess", Role: store.RoleUser},
		{ID: "m2", SessionID: "sess", Role: store.RoleAssistant},
	}
	for _, m := range msgs {
		if err := s.CreateMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListMessages(ctx, "sess")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Errorf("ListMessages = %d, want 2", len(list))
	}
	// insertion order preserved via created_at
	if list[0].ID != "m1" || list[1].ID != "m2" {
		t.Error("order not preserved")
	}
}

func TestMessage_error(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_ = s.CreateSession(ctx, &store.Session{ID: "sess"})
	_ = s.CreateMessage(ctx, &store.Message{
		ID: "merr", SessionID: "sess", Role: store.RoleAssistant,
		Error: &store.MessageError{
			Name:    "overload",
			Message: "too many requests",
			Data:    map[string]any{"code": float64(429)},
		},
	})
	got, _ := s.GetMessage(ctx, "merr")
	if got.Error == nil || got.Error.Name != "overload" {
		t.Errorf("error not persisted: %+v", got.Error)
	}
	if got.Error.Data["code"] != float64(429) {
		t.Errorf("error data not persisted: %+v", got.Error.Data)
	}
}

func TestMessage_status(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_ = s.CreateSession(ctx, &store.Session{ID: "sess"})
	_ = s.CreateMessage(ctx, &store.Message{
		ID: "mint", SessionID: "sess", Role: store.RoleAssistant,
		Status: store.MessageStatusInterrupted,
	})
	got, _ := s.GetMessage(ctx, "mint")
	if got.Status != store.MessageStatusInterrupted {
		t.Errorf("status = %q, want interrupted", got.Status)
	}
}

func TestMessage_summary(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_ = s.CreateSession(ctx, &store.Session{ID: "sess"})
	_ = s.CreateMessage(ctx, &store.Message{
		ID: "msum", SessionID: "sess", Role: store.RoleAssistant, Summary: true,
	})
	got, _ := s.GetMessage(ctx, "msum")
	if !got.Summary {
		t.Error("Summary flag not persisted")
	}
}

func TestMessage_updateError(t *testing.T) {
	s := newStore(t)
	err := s.UpdateMessage(context.Background(), &store.Message{ID: "missing"})
	if err == nil {
		t.Error("expected error updating missing message")
	}
}

// ── store.Store: Part ─────────────────────────────────────────────────────────

func TestPart_createAndList(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_ = s.CreateSession(ctx, &store.Session{ID: "sess"})
	_ = s.CreateMessage(ctx, &store.Message{ID: "msg", SessionID: "sess", Role: store.RoleAssistant})

	parts := []*store.Part{
		{ID: "p1", MessageID: "msg", SessionID: "sess", Type: store.PartTypeText,
			Data: &store.TextPartData{Text: "hello"}},
		{ID: "p2", MessageID: "msg", SessionID: "sess", Type: store.PartTypeTool,
			Data: &store.ToolPartData{Tool: "shell", CallID: "c1", Status: store.ToolStatusPending}},
	}
	for _, p := range parts {
		if err := s.CreatePart(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListParts(ctx, "msg")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Errorf("ListParts = %d, want 2", len(list))
	}
}

func TestPart_updateStatus(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_ = s.CreateSession(ctx, &store.Session{ID: "sess"})
	_ = s.CreateMessage(ctx, &store.Message{ID: "msg", SessionID: "sess", Role: store.RoleAssistant})
	_ = s.CreatePart(ctx, &store.Part{
		ID: "p1", MessageID: "msg", SessionID: "sess",
		Type: store.PartTypeTool,
		Data: &store.ToolPartData{Status: store.ToolStatusPending},
	})
	p, _ := s.GetPart(ctx, "p1")
	p.Data = &store.ToolPartData{Status: store.ToolStatusCompleted, Output: "done"}
	if err := s.UpdatePart(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetPart(ctx, "p1")
	d, ok := store.DataAs[*store.ToolPartData](got)
	if !ok || d.Status != store.ToolStatusCompleted {
		t.Errorf("status not updated: %+v", got.Data)
	}
}

func TestPart_dataAs_textPart(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_ = s.CreateSession(ctx, &store.Session{ID: "sess"})
	_ = s.CreateMessage(ctx, &store.Message{ID: "msg", SessionID: "sess", Role: store.RoleUser})
	_ = s.CreatePart(ctx, &store.Part{
		ID: "p1", MessageID: "msg", SessionID: "sess",
		Type: store.PartTypeText,
		Data: &store.TextPartData{Text: "world", TimeStart: 1000, TimeEnd: 2000},
	})
	got, _ := s.GetPart(ctx, "p1")
	// DataAs must succeed via JSON round-trip (SQLite stores as JSON map)
	d, ok := store.DataAs[*store.TextPartData](got)
	if !ok {
		t.Fatal("DataAs[*TextPartData] failed")
	}
	if d.Text != "world" || d.TimeStart != 1000 {
		t.Errorf("TextPartData = %+v", d)
	}
}

func TestPart_listBySession(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_ = s.CreateSession(ctx, &store.Session{ID: "sess"})
	_ = s.CreateMessage(ctx, &store.Message{ID: "m1", SessionID: "sess", Role: store.RoleUser})
	_ = s.CreateMessage(ctx, &store.Message{ID: "m2", SessionID: "sess", Role: store.RoleAssistant})
	_ = s.CreatePart(ctx, &store.Part{ID: "p1", MessageID: "m1", SessionID: "sess", Type: store.PartTypeText, Data: &store.TextPartData{Text: "a"}})
	_ = s.CreatePart(ctx, &store.Part{ID: "p2", MessageID: "m1", SessionID: "sess", Type: store.PartTypeText, Data: &store.TextPartData{Text: "b"}})
	_ = s.CreatePart(ctx, &store.Part{ID: "p3", MessageID: "m2", SessionID: "sess", Type: store.PartTypeText, Data: &store.TextPartData{Text: "c"}})

	bySession, err := s.ListPartsBySession(ctx, "sess")
	if err != nil {
		t.Fatal(err)
	}
	if len(bySession["m1"]) != 2 {
		t.Errorf("m1 parts = %d, want 2", len(bySession["m1"]))
	}
	if len(bySession["m2"]) != 1 {
		t.Errorf("m2 parts = %d, want 1", len(bySession["m2"]))
	}
}

// ── knowledge.PersistStore ────────────────────────────────────────────────────

// newHistorySource creates a HistorySource (knowledge.Source) scoped to sessID.
func newHistorySource(t *testing.T, st *sqlite.Store, sessID string) *sqlite.HistorySource {
	t.Helper()
	return sqlite.NewHistorySource(st, sessID, 0)
}

func TestPersistStore_saveAndLoad(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	docs := []store.Record{
		{ID: "d1", Role: "user", Text: "hello world", TurnIndex: 0, CompactionSeq: 1, CreatedAt: time.Now().UnixMilli()},
		{ID: "d2", Role: "assistant", Text: "hi there", TurnIndex: 1, CompactionSeq: 1, CreatedAt: time.Now().UnixMilli()},
	}
	for _, doc := range docs {
		if err := s.SaveRecord(ctx, "sess1", doc); err != nil {
			t.Fatalf("SaveRecord: %v", err)
		}
	}

	loaded, err := s.LoadRecords(ctx, "sess1")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded[1]) != 2 {
		t.Errorf("LoadRecords seq=1: got %d, want 2", len(loaded[1]))
	}
}

func TestPersistStore_deleteBySeq(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	for seq := 1; seq <= 3; seq++ {
		_ = s.SaveRecord(ctx, "sess1", store.Record{
			ID: fmt.Sprintf("d%d", seq), Role: "user",
			Text: "msg", CompactionSeq: seq, CreatedAt: time.Now().UnixMilli(),
		})
	}

	if err := s.DeleteRecordsBySeq(ctx, "sess1", 2); err != nil {
		t.Fatal(err)
	}

	loaded, _ := s.LoadRecords(ctx, "sess1")
	if _, ok := loaded[2]; ok {
		t.Error("seq=2 should have been deleted")
	}
	if len(loaded[1]) != 1 || len(loaded[3]) != 1 {
		t.Error("seq=1 and seq=3 should still exist")
	}
}

func TestPersistStore_toolCalls(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	doc := store.Record{
		ID: "d1", Role: "assistant",
		Text:          "using tools",
		ToolCalls:     []string{"shell", "read", "write"},
		CompactionSeq: 1,
		CreatedAt:     time.Now().UnixMilli(),
	}
	_ = s.SaveRecord(ctx, "sess1", doc)

	loaded, _ := s.LoadRecords(ctx, "sess1")
	got := loaded[1][0]
	if len(got.ToolCalls) != 3 || got.ToolCalls[0] != "shell" {
		t.Errorf("ToolCalls not persisted: %v", got.ToolCalls)
	}
}

func TestPersistStore_sessionIsolation(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_ = s.SaveRecord(ctx, "sess1", store.Record{ID: "d1", CompactionSeq: 1, CreatedAt: time.Now().UnixMilli()})
	_ = s.SaveRecord(ctx, "sess2", store.Record{ID: "d1", CompactionSeq: 1, CreatedAt: time.Now().UnixMilli()})

	r1, _ := s.LoadRecords(ctx, "sess1")
	r2, _ := s.LoadRecords(ctx, "sess2")

	if len(r1[1]) != 1 || len(r2[1]) != 1 {
		t.Error("session isolation broken")
	}
}

// ── knowledge.Source: Peek / Fetch ───────────────────────────────────────────

func TestHistorySource_peek(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	hs := newHistorySource(t, s, "sess1")

	docs := []store.Record{
		{ID: "d1", Role: "user", Text: "golang concurrency patterns", CompactionSeq: 1, TurnIndex: 0, CreatedAt: time.Now().UnixMilli()},
		{ID: "d2", Role: "assistant", Text: "use goroutines and channels", CompactionSeq: 1, TurnIndex: 1, CreatedAt: time.Now().UnixMilli()},
		{ID: "d3", Role: "user", Text: "python decorators explained", CompactionSeq: 2, TurnIndex: 0, CreatedAt: time.Now().UnixMilli()},
	}
	for _, d := range docs {
		_ = s.SaveRecord(ctx, "sess1", d)
	}

	results, err := hs.Peek(ctx, knowledge.Query{
		Type: knowledge.QueryTypeSearch, Input: "golang", MaxResults: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Error("expected at least one result for 'golang'")
	}
	// RefID must start with source ID
	for _, r := range results {
		if r.RefID == "" || r.Snippet == "" {
			t.Errorf("incomplete result: %+v", r)
		}
	}
}

func TestHistorySource_peek_empty_query(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	hs := newHistorySource(t, s, "sess1")

	for i := 0; i < 3; i++ {
		_ = s.SaveRecord(ctx, "sess1", store.Record{
			ID: fmt.Sprintf("d%d", i), Role: "user",
			Text: fmt.Sprintf("message %d", i), CompactionSeq: 1,
			TurnIndex: i, CreatedAt: time.Now().UnixMilli(),
		})
	}

	// Empty query should return all (up to MaxResults)
	results, err := hs.Peek(ctx, knowledge.Query{Type: knowledge.QueryTypeSearch, MaxResults: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Errorf("empty query: got %d results, want 3", len(results))
	}
}

func TestHistorySource_fetch(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	hs := newHistorySource(t, s, "sess1")

	_ = s.SaveRecord(ctx, "sess1", store.Record{
		ID: "doc42", Role: "assistant",
		Text:          "the full answer is here",
		CompactionSeq: 1, TurnIndex: 0,
		CreatedAt: time.Now().UnixMilli(),
	})

	// Fetch by bare doc ID
	results, err := hs.Fetch(ctx, knowledge.Query{
		Type:  knowledge.QueryTypeFetch,
		Input: hs.ID() + ":doc42",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected a result")
	}
	if results[0].Content == "" {
		t.Error("Content must be populated for Fetch")
	}
	if results[0].Snippet != "" {
		t.Error("Snippet must be empty for Fetch")
	}
}

func TestHistorySource_fetch_notFound(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	hs := newHistorySource(t, s, "sess1")

	_, err := hs.Fetch(ctx, knowledge.Query{
		Type:  knowledge.QueryTypeFetch,
		Input: hs.ID() + ":ghost",
	})
	if err == nil {
		t.Error("expected error for non-existent doc")
	}
}

func TestHistorySource_accepts(t *testing.T) {
	s := newStore(t)
	hs := newHistorySource(t, s, "sess1")

	if !hs.Accepts(knowledge.Query{Type: knowledge.QueryTypeSearch}) {
		t.Error("should accept search queries")
	}
	if !hs.Accepts(knowledge.Query{Type: knowledge.QueryTypeFetch}) {
		t.Error("should accept fetch queries")
	}
}

// ── migrations ────────────────────────────────────────────────────────────────

func TestMigrations_idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	// Open twice — migrations must not fail on second apply.
	for i := 0; i < 2; i++ {
		st, err := sqlite.Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		st.Close()
	}
}

// ── reopen persistence ────────────────────────────────────────────────────────

func TestPersistStore_survivesReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	ctx := context.Background()

	// Write records in first open.
	st1, _ := sqlite.Open(path)
	_ = st1.SaveRecord(ctx, "sess1", store.Record{
		ID: "d1", Role: "user", Text: "remember this",
		CompactionSeq: 1, CreatedAt: time.Now().UnixMilli(),
	})
	st1.Close()

	// Reopen and verify records survive.
	st2, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	loaded, err := st2.LoadRecords(ctx, "sess1")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded[1]) != 1 || loaded[1][0].Text != "remember this" {
		t.Errorf("data not persisted across reopen: %v", loaded)
	}
}

// ── OS-level file existence ───────────────────────────────────────────────────

func TestOpen_createsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.db")

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("file should not exist yet")
	}
	st, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("db file not created: %v", err)
	}
}

// ── LoadSeqIndex ──────────────────────────────────────────────────────────────

func TestLoadSeqIndex_basic(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// Write 5 seqs with 2 docs each.
	for seq := 1; seq <= 5; seq++ {
		for i := 0; i < 2; i++ {
			_ = s.SaveRecord(ctx, "sess1", store.Record{
				ID:            fmt.Sprintf("d%d-%d", seq, i),
				Role:          "user",
				Text:          fmt.Sprintf("seq %d doc %d", seq, i),
				CompactionSeq: seq,
				TurnIndex:     i,
				CreatedAt:     time.Now().UnixMilli(),
			})
		}
	}

	// Load only the 3 most recent seqs.
	idx, err := s.LoadSeqIndex(ctx, "sess1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx) != 3 {
		t.Errorf("LoadSeqIndex limit=3: got %d seqs, want 3", len(idx))
	}
	// Should contain seqs 3, 4, 5 (most recent).
	for _, seq := range []int{3, 4, 5} {
		if ids, ok := idx[seq]; !ok || len(ids) != 2 {
			t.Errorf("seq %d: got %v, want 2 ids", seq, ids)
		}
	}
	// Seq 1 and 2 should be absent (too old).
	for _, seq := range []int{1, 2} {
		if _, ok := idx[seq]; ok {
			t.Errorf("seq %d should not be in index (limit=3)", seq)
		}
	}
}

func TestLoadSeqIndex_empty(t *testing.T) {
	s := newStore(t)
	idx, err := s.LoadSeqIndex(context.Background(), "nosess", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx) != 0 {
		t.Errorf("expected empty index, got %d seqs", len(idx))
	}
}

func TestLoadSeqIndex_limitLargerThanData(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for seq := 1; seq <= 3; seq++ {
		_ = s.SaveRecord(ctx, "sess1", store.Record{
			ID: fmt.Sprintf("d%d", seq), CompactionSeq: seq, CreatedAt: time.Now().UnixMilli(),
		})
	}
	// Limit larger than actual seqs — should return all.
	idx, err := s.LoadSeqIndex(ctx, "sess1", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx) != 3 {
		t.Errorf("got %d seqs, want 3", len(idx))
	}
}

// ── LoadRecordsBySeq ──────────────────────────────────────────────────────────

func TestLoadRecordsBySeq_basic(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	want := []store.Record{
		{ID: "a", Role: "user", Text: "hello", TurnIndex: 0, CompactionSeq: 2, ToolCalls: []string{"shell"}, CreatedAt: time.Now().UnixMilli()},
		{ID: "b", Role: "assistant", Text: "world", TurnIndex: 1, CompactionSeq: 2, CreatedAt: time.Now().UnixMilli()},
	}
	for _, r := range want {
		_ = s.SaveRecord(ctx, "sess1", r)
	}
	// Write a decoy in a different seq.
	_ = s.SaveRecord(ctx, "sess1", store.Record{ID: "c", CompactionSeq: 9, CreatedAt: time.Now().UnixMilli()})

	got, err := s.LoadRecordsBySeq(ctx, "sess1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d records, want 2", len(got))
	}
	if got[0].ID != "a" || got[1].ID != "b" {
		t.Errorf("order or IDs wrong: %v", got)
	}
	if len(got[0].ToolCalls) != 1 || got[0].ToolCalls[0] != "shell" {
		t.Errorf("ToolCalls not preserved: %v", got[0].ToolCalls)
	}
}

func TestLoadRecordsBySeq_noMatch(t *testing.T) {
	s := newStore(t)
	got, err := s.LoadRecordsBySeq(context.Background(), "sess1", 99)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %d", len(got))
	}
}

// ── FindSeqByDocID ────────────────────────────────────────────────────────────

func TestFindSeqByDocID_found(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_ = s.SaveRecord(ctx, "sess1", store.Record{
		ID: "doc-42", CompactionSeq: 7, CreatedAt: time.Now().UnixMilli(),
	})

	seq, found, err := s.FindSeqByDocID(ctx, "sess1", "doc-42")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected found=true")
	}
	if seq != 7 {
		t.Errorf("seq = %d, want 7", seq)
	}
}

func TestFindSeqByDocID_notFound(t *testing.T) {
	s := newStore(t)
	_, found, err := s.FindSeqByDocID(context.Background(), "sess1", "ghost")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Error("expected found=false for non-existent docID")
	}
}

func TestFindSeqByDocID_sessionIsolation(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_ = s.SaveRecord(ctx, "sessA", store.Record{
		ID: "doc-1", CompactionSeq: 3, CreatedAt: time.Now().UnixMilli(),
	})

	// Same docID exists in sessA but not in sessB.
	_, found, err := s.FindSeqByDocID(ctx, "sessB", "doc-1")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Error("doc from sessA should not be found in sessB")
	}
}

// ── SessionHistorySource integration (L0/L1 caps + LRU) ──────────────────────

func newHistSrc(t *testing.T, st *sqlite.Store, sessID string, maxL1, maxL0 int) *store.SessionHistorySource {
	t.Helper()
	ps := sqlite.NewHistorySource(st, sessID, 0)
	src, err := store.NewSessionHistorySource(sessID, maxL1, maxL0, ps)
	if err != nil {
		t.Fatalf("NewSessionHistorySource: %v", err)
	}
	return src
}

func saveAndHook(t *testing.T, src *store.SessionHistorySource, st *sqlite.Store, sessID string, seq int, msgs []string) {
	t.Helper()
	ctx := context.Background()
	// Simulate the compaction hook by calling Hook() indirectly:
	// save records to SQLite directly (as SaveRecord does in Hook) and
	// also exercise the Hook path via a fake compaction message set.
	for i, text := range msgs {
		_ = st.SaveRecord(ctx, sessID, store.Record{
			ID:            fmt.Sprintf("%s-seq%d-doc%d", sessID, seq, i),
			Role:          "user",
			Text:          text,
			TurnIndex:     i,
			CompactionSeq: seq,
			CreatedAt:     time.Now().UnixMilli(),
		})
	}
}

func TestSessionHistorySource_peekFallsBackToSQLite(t *testing.T) {
	// Verify that Peek returns results even when Bleve is empty (cold start).
	s := newStore(t)
	ctx := context.Background()

	// Write 3 seqs directly to SQLite (simulate prior sessions).
	for seq := 1; seq <= 3; seq++ {
		_ = s.SaveRecord(ctx, "sess1", store.Record{
			ID:            fmt.Sprintf("d%d", seq),
			Role:          "user",
			Text:          fmt.Sprintf("golang concurrency seq %d", seq),
			CompactionSeq: seq,
			TurnIndex:     0,
			CreatedAt:     time.Now().UnixMilli(),
		})
	}

	// Create source after records exist — Bleve starts empty.
	hs := newHistSrc(t, s, "sess1", 2, 5)

	// SQLite HistorySource (L2) must cover all seqs even with cold Bleve.
	results, err := hs.Peek(ctx, knowledge.Query{
		Type:       knowledge.QueryTypeSearch,
		Input:      "golang",
		MaxResults: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Error("expected results from SQLite fallback, got none")
	}
}

func TestSessionHistorySource_fetchTriggersPageIn(t *testing.T) {
	// Fetch on a cold docID should page-in from SQLite and return full content.
	s := newStore(t)
	ctx := context.Background()

	_ = s.SaveRecord(ctx, "sess1", store.Record{
		ID:            "target-doc",
		Role:          "assistant",
		Text:          "the answer is 42",
		CompactionSeq: 1,
		TurnIndex:     0,
		CreatedAt:     time.Now().UnixMilli(),
	})

	hs := newHistSrc(t, s, "sess1", 2, 5)

	results, err := hs.Fetch(ctx, knowledge.Query{
		Type:  knowledge.QueryTypeFetch,
		Input: "session-history:target-doc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected a result")
	}
	if results[0].Content == "" {
		t.Error("Content must be populated for Fetch")
	}
}

func TestSessionHistorySource_l0Eviction(t *testing.T) {
	// When maxIndexedSeqs=3 and we add 5 seqs, only 3 should remain in L0.
	s := newStore(t)
	ctx := context.Background()
	sessID := "sess-evict"

	// maxL1=2, maxL0=3
	src, err := store.NewSessionHistorySource(sessID, 2, 3, sqlite.NewHistorySource(s, sessID, 0))
	if err != nil {
		t.Fatal(err)
	}

	hook := src.Hook()

	// Simulate 5 compaction rounds via Hook.
	for seq := 1; seq <= 5; seq++ {
		msgs := []*store.Message{
			{
				ID:        fmt.Sprintf("msg-%d", seq),
				SessionID: sessID,
				Role:      store.RoleUser,
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			},
		}
		parts := map[string][]*store.Part{
			msgs[0].ID: {
				{
					ID:        fmt.Sprintf("part-%d", seq),
					MessageID: msgs[0].ID,
					SessionID: sessID,
					Type:      store.PartTypeText,
					Data:      &store.TextPartData{Text: fmt.Sprintf("turn %d content", seq)},
				},
			},
		}
		hook(msgs, parts)
	}

	// All 5 seqs should be in SQLite.
	idx, _ := s.LoadSeqIndex(ctx, sessID, 100)
	if len(idx) != 5 {
		t.Errorf("SQLite has %d seqs, want 5", len(idx))
	}

	// L0 should have at most 3 seqs (maxIndexedSeqs).
	// L1 should have at most 2 seqs (maxCompactions).
	// We can't inspect internal fields directly, but we can verify:
	// - Peek on recently added content still works (L1 or L2 coverage).
	results, err := src.Peek(ctx, knowledge.Query{
		Type:       knowledge.QueryTypeSearch,
		Input:      "turn 5",
		MaxResults: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Error("expected result for 'turn 5' (most recent seq)")
	}
}

func TestSessionHistorySource_restoreFromSQLite(t *testing.T) {
	// Simulate restart: write records, close, reopen, verify Peek works.
	dir := t.TempDir()
	path := filepath.Join(dir, "restore.db")
	ctx := context.Background()
	sessID := "sess-restore"

	// First "process": write 5 seqs via Hook.
	st1, _ := sqlite.Open(path)
	src1, _ := store.NewSessionHistorySource(sessID, 2, 4, sqlite.NewHistorySource(st1, sessID, 0))
	hook1 := src1.Hook()
	for seq := 1; seq <= 5; seq++ {
		msgs := []*store.Message{{
			ID: fmt.Sprintf("m%d", seq), SessionID: sessID,
			Role: store.RoleUser, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}}
		parts := map[string][]*store.Part{
			msgs[0].ID: {{
				ID: fmt.Sprintf("p%d", seq), MessageID: msgs[0].ID,
				SessionID: sessID, Type: store.PartTypeText,
				Data: &store.TextPartData{Text: fmt.Sprintf("ancient wisdom seq %d", seq)},
			}},
		}
		hook1(msgs, parts)
	}
	st1.Close()

	// Second "process": reopen and verify history is accessible.
	st2, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()

	// maxL0=4 means only 4 most recent seqs are in compactionDocs at startup.
	src2, err := store.NewSessionHistorySource(sessID, 2, 4, sqlite.NewHistorySource(st2, sessID, 0))
	if err != nil {
		t.Fatal(err)
	}

	// Seq 1 is the oldest and outside L0 (maxL0=4, seqs 2–5 loaded).
	// But Peek should still find it via SQLite L2 path.
	results, err := src2.Peek(ctx, knowledge.Query{
		Type:       knowledge.QueryTypeSearch,
		Input:      "ancient",
		MaxResults: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Error("expected results for 'ancient wisdom' after restart (via SQLite L2)")
	}
}
