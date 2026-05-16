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

	docs := []knowledge.HistoryDoc{
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
		_ = s.SaveRecord(ctx, "sess1", knowledge.HistoryDoc{
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

	doc := knowledge.HistoryDoc{
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

	_ = s.SaveRecord(ctx, "sess1", knowledge.HistoryDoc{ID: "d1", CompactionSeq: 1, CreatedAt: time.Now().UnixMilli()})
	_ = s.SaveRecord(ctx, "sess2", knowledge.HistoryDoc{ID: "d1", CompactionSeq: 1, CreatedAt: time.Now().UnixMilli()})

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

	docs := []knowledge.HistoryDoc{
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
		_ = s.SaveRecord(ctx, "sess1", knowledge.HistoryDoc{
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

	_ = s.SaveRecord(ctx, "sess1", knowledge.HistoryDoc{
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
	_ = st1.SaveRecord(ctx, "sess1", knowledge.HistoryDoc{
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
