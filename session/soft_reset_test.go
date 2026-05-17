package session_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/larryhou/llm-go/knowledge"
	"github.com/larryhou/llm-go/session"
	"github.com/larryhou/llm-go/store"
	"github.com/larryhou/llm-go/store/memory"
)
// buildCompactedSession constructs a session that has been compacted once:
//
//	head:  msgH1(user), msgH2(asst)          ← summarised, still in DB
//	tail:  msgT1(user), msgT2(asst)           ← verbatim, TailStartID=msgT1
//	boundary: msgB(user, PartTypeCompaction{TailStartID:"msgT1"})
//	summary:  msgS(asst, Summary=true)
//	post:  msgP1(user), msgP2(asst)           ← new messages after compaction
//
// Returns the store, sessionID, and a map of named message IDs.
func buildCompactedSession(t *testing.T) (*memory.Store, string, map[string]string) {
	t.Helper()
	ctx := context.Background()
	s := memory.New()
	sessID := "sess-soft"
	_ = s.CreateSession(ctx, &store.Session{ID: sessID})

	now := time.Now()
	tick := func(d time.Duration) time.Time { now = now.Add(d); return now }

	ids := map[string]string{}
	newMsg := func(name, role string, summary bool, t time.Time) *store.Message {
		m := &store.Message{
			ID:        name,
			SessionID: sessID,
			Role:      role,
			Summary:   summary,
			CreatedAt: t,
		}
		ids[name] = name
		return m
	}

	// head
	msgH1 := newMsg("msgH1", store.RoleUser, false, tick(time.Millisecond))
	msgH2 := newMsg("msgH2", store.RoleAssistant, false, tick(time.Millisecond))
	// tail (sit before boundary in DB)
	msgT1 := newMsg("msgT1", store.RoleUser, false, tick(time.Millisecond))
	msgT2 := newMsg("msgT2", store.RoleAssistant, false, tick(time.Millisecond))
	// boundary
	msgB := newMsg("msgB", store.RoleUser, false, tick(time.Millisecond))
	// summary
	msgS := newMsg("msgS", store.RoleAssistant, true, tick(time.Millisecond))
	// post-boundary
	msgP1 := newMsg("msgP1", store.RoleUser, false, tick(time.Millisecond))
	msgP2 := newMsg("msgP2", store.RoleAssistant, false, tick(time.Millisecond))

	for _, m := range []*store.Message{msgH1, msgH2, msgT1, msgT2, msgB, msgS, msgP1, msgP2} {
		if err := s.CreateMessage(ctx, m); err != nil {
			t.Fatalf("CreateMessage %s: %v", m.ID, err)
		}
	}

	// attach compaction part to boundary
	_ = s.CreatePart(ctx, &store.Part{
		ID:        "partB",
		MessageID: "msgB",
		SessionID: sessID,
		Type:      store.PartTypeCompaction,
		Data:      &store.CompactionPartData{TailStartID: "msgT1"},
	})
	// attach text part to head messages so they have real content
	_ = s.CreatePart(ctx, &store.Part{
		ID: "partH1", MessageID: "msgH1", SessionID: sessID,
		Type: store.PartTypeText, Data: &store.TextPartData{Text: "head user msg"},
	})
	_ = s.CreatePart(ctx, &store.Part{
		ID: "partH2", MessageID: "msgH2", SessionID: sessID,
		Type: store.PartTypeText, Data: &store.TextPartData{Text: "head asst msg"},
	})

	return s, sessID, ids
}

// listMsgIDs returns the IDs of all messages in the session in insertion order.
func listMsgIDs(t *testing.T, s store.Store, sessID string) []string {
	t.Helper()
	msgs, err := s.ListMessages(context.Background(), sessID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	return ids
}

// ── tests ─────────────────────────────────────────────────────────────────────

// TestSoftReset_basic verifies that a single SoftReset removes boundary,
// summary, and post-boundary messages, leaving head + tail intact.
func TestSoftReset_basic(t *testing.T) {
	s, sessID, _ := buildCompactedSession(t)
	ctx := context.Background()

	if _, err := session.SoftReset(ctx, sessID, s, false); err != nil {
		t.Fatalf("SoftReset: %v", err)
	}

	got := listMsgIDs(t, s, sessID)
	want := []string{"msgH1", "msgH2", "msgT1", "msgT2"}
	if !equalStringSlice(got, want) {
		t.Errorf("after soft reset: messages = %v, want %v", got, want)
	}
}

// TestSoftReset_noCompaction verifies ErrNoCompactionBoundary when session
// has never been compacted.
func TestSoftReset_noCompaction(t *testing.T) {
	ctx := context.Background()
	s := memory.New()
	_ = s.CreateSession(ctx, &store.Session{ID: "sess-plain"})
	now := time.Now()
	_ = s.CreateMessage(ctx, &store.Message{
		ID: "m1", SessionID: "sess-plain", Role: store.RoleUser,
		CreatedAt: now,
	})
	_ = s.CreateMessage(ctx, &store.Message{
		ID: "m2", SessionID: "sess-plain", Role: store.RoleAssistant,
		CreatedAt: now.Add(time.Millisecond),
	})

	_, err := session.SoftReset(ctx, "sess-plain", s, false)
	if !errors.Is(err, session.ErrNoCompactionBoundary) {
		t.Errorf("got error %v, want ErrNoCompactionBoundary", err)
	}
	// messages must be untouched
	got := listMsgIDs(t, s, "sess-plain")
	if len(got) != 2 {
		t.Errorf("messages should be unchanged, got %v", got)
	}
}

// TestSoftReset_repeated verifies that repeated SoftReset calls each peel
// off one compaction layer. With two compaction layers:
//
//	layer1 head → layer1 boundary/summary → layer2 head → layer2 boundary/summary → post
//
// First call removes layer2; second call removes layer1; third returns
// ErrNoCompactionBoundary.
func TestSoftReset_repeated(t *testing.T) {
	ctx := context.Background()
	s := memory.New()
	sessID := "sess-two"
	_ = s.CreateSession(ctx, &store.Session{ID: sessID})

	now := time.Now()
	tick := func() time.Time { now = now.Add(time.Millisecond); return now }

	newMsg := func(id, role string, summary bool) *store.Message {
		return &store.Message{
			ID: id, SessionID: sessID, Role: role, Summary: summary,
			CreatedAt: tick(),
		}
	}
	mustCreate := func(m *store.Message) {
		if err := s.CreateMessage(ctx, m); err != nil {
			t.Fatalf("CreateMessage %s: %v", m.ID, err)
		}
	}
	mustPart := func(id, msgID, typ string, data any) {
		if err := s.CreatePart(ctx, &store.Part{
			ID: id, MessageID: msgID, SessionID: sessID,
			Type: typ, Data: data,
		}); err != nil {
			t.Fatalf("CreatePart %s: %v", id, err)
		}
	}

	// compaction round 1
	mustCreate(newMsg("L1H1", store.RoleUser, false))      // layer1 head
	mustCreate(newMsg("L1H2", store.RoleAssistant, false)) // layer1 head
	mustCreate(newMsg("L1B", store.RoleUser, false))       // layer1 boundary
	mustPart("pL1B", "L1B", store.PartTypeCompaction, &store.CompactionPartData{TailStartID: ""})
	mustCreate(newMsg("L1S", store.RoleAssistant, true)) // layer1 summary

	// compaction round 2 (head = post of layer1, tail = L2T1..L2T2)
	mustCreate(newMsg("L2H1", store.RoleUser, false))      // layer2 head
	mustCreate(newMsg("L2H2", store.RoleAssistant, false)) // layer2 head
	mustCreate(newMsg("L2T1", store.RoleUser, false))      // layer2 tail
	mustCreate(newMsg("L2T2", store.RoleAssistant, false)) // layer2 tail
	mustCreate(newMsg("L2B", store.RoleUser, false))       // layer2 boundary
	mustPart("pL2B", "L2B", store.PartTypeCompaction, &store.CompactionPartData{TailStartID: "L2T1"})
	mustCreate(newMsg("L2S", store.RoleAssistant, true)) // layer2 summary

	// post-boundary new messages
	mustCreate(newMsg("P1", store.RoleUser, false))
	mustCreate(newMsg("P2", store.RoleAssistant, false))

	// ── first soft reset: peels layer2 ──────────────────────────────────────
	if _, err := session.SoftReset(ctx, sessID, s, false); err != nil {
		t.Fatalf("first SoftReset: %v", err)
	}
	got := listMsgIDs(t, s, sessID)
	// boundary L2B and everything after it (L2B, L2S, P1, P2) should be gone.
	// L2T1, L2T2 are BEFORE L2B in insertion order → they survive.
	want := []string{"L1H1", "L1H2", "L1B", "L1S", "L2H1", "L2H2", "L2T1", "L2T2"}
	if !equalStringSlice(got, want) {
		t.Errorf("after 1st soft reset: %v, want %v", got, want)
	}

	// ── second soft reset: peels layer1 ─────────────────────────────────────
	if _, err := session.SoftReset(ctx, sessID, s, false); err != nil {
		t.Fatalf("second SoftReset: %v", err)
	}
	got = listMsgIDs(t, s, sessID)
	// L1B and everything after (L1B, L1S, L2H1, L2H2, L2T1, L2T2) gone.
	want = []string{"L1H1", "L1H2"}
	if !equalStringSlice(got, want) {
		t.Errorf("after 2nd soft reset: %v, want %v", got, want)
	}

	// ── third soft reset: no boundary left → ErrNoCompactionBoundary ────────
	_, err := session.SoftReset(ctx, sessID, s, false)
	if !errors.Is(err, session.ErrNoCompactionBoundary) {
		t.Errorf("third SoftReset: got %v, want ErrNoCompactionBoundary", err)
	}
	// messages still unchanged
	got = listMsgIDs(t, s, sessID)
	if !equalStringSlice(got, []string{"L1H1", "L1H2"}) {
		t.Errorf("messages after failed soft reset changed unexpectedly: %v", got)
	}
}

// TestSoftReset_partsDeleted verifies that parts belonging to deleted messages
// are also removed (cascade delete).
func TestSoftReset_partsDeleted(t *testing.T) {
	s, sessID, _ := buildCompactedSession(t)
	ctx := context.Background()

	// verify parts exist for boundary before reset
	partsBefore, _ := s.ListParts(ctx, "msgB")
	if len(partsBefore) == 0 {
		t.Fatal("expected parts on msgB before soft reset")
	}

	if _, err := session.SoftReset(ctx, sessID, s, false); err != nil {
		t.Fatalf("SoftReset: %v", err)
	}

	// msgB is gone — ListParts should return empty (not error)
	partsAfter, err := s.ListParts(ctx, "msgB")
	if err != nil {
		t.Fatalf("ListParts after soft reset: %v", err)
	}
	if len(partsAfter) != 0 {
		t.Errorf("parts for deleted boundary message should be empty, got %d", len(partsAfter))
	}
}

// TestSoftReset_filterCompactedAfter verifies that after a soft reset,
// FilterCompacted returns the full message list (no compaction boundary
// remains), so head messages are all visible.
func TestSoftReset_filterCompactedAfter(t *testing.T) {
	s, sessID, _ := buildCompactedSession(t)
	ctx := context.Background()

	if _, err := session.SoftReset(ctx, sessID, s, false); err != nil {
		t.Fatalf("SoftReset: %v", err)
	}

	msgs, _ := s.ListMessages(ctx, sessID)
	allParts, _ := s.ListPartsBySession(ctx, sessID)
	visible := session.FilterCompacted(msgs, allParts)

	// No boundary left → FilterCompacted returns msgs as-is.
	// All 4 remaining messages (H1, H2, T1, T2) should be visible.
	if len(visible) != 4 {
		t.Errorf("FilterCompacted after soft reset: got %d messages, want 4", len(visible))
	}
	ids := make([]string, len(visible))
	for i, m := range visible {
		ids[i] = m.ID
	}
	want := []string{"msgH1", "msgH2", "msgT1", "msgT2"}
	if !equalStringSlice(ids, want) {
		t.Errorf("visible messages = %v, want %v", ids, want)
	}
}

// TestResetTool_softMode verifies the tool executes soft reset via the
// softFn callback when mode="soft" (or default).
func TestResetTool_softMode(t *testing.T) {
	var lastFresh *bool
	hardCalled := false
	softFn := func(_ context.Context, fresh bool) error { lastFresh = &fresh; return nil }
	hardFn := func(_ context.Context) error { hardCalled = true; return nil }

	rt := session.NewResetTool(softFn, hardFn)

	// default (no mode key) → soft, fresh=false
	result, err := rt.Execute(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("Execute default: %v", err)
	}
	if lastFresh == nil || *lastFresh != false {
		t.Errorf("expected softFn called with fresh=false, got %v", lastFresh)
	}
	if hardCalled {
		t.Error("hardFn should NOT have been called for default mode")
	}
	if result.Output == "" {
		t.Error("expected non-empty output")
	}

	// explicit mode="soft" → fresh=false
	lastFresh = nil
	_, err = rt.Execute(context.Background(), map[string]any{"mode": "soft"})
	if err != nil {
		t.Fatalf("Execute soft: %v", err)
	}
	if lastFresh == nil || *lastFresh != false {
		t.Errorf("expected softFn called with fresh=false for mode=soft, got %v", lastFresh)
	}
}

// TestResetTool_freshStartMode verifies mode="soft-refresh" calls softFn with fresh=true.
func TestResetTool_freshStartMode(t *testing.T) {
	var lastFresh *bool
	hardCalled := false
	softFn := func(_ context.Context, fresh bool) error { lastFresh = &fresh; return nil }
	hardFn := func(_ context.Context) error { hardCalled = true; return nil }

	rt := session.NewResetTool(softFn, hardFn)
	result, err := rt.Execute(context.Background(), map[string]any{"mode": "soft-refresh"})
	if err != nil {
		t.Fatalf("Execute soft-refresh: %v", err)
	}
	if lastFresh == nil || *lastFresh != true {
		t.Errorf("expected softFn called with fresh=true for mode=soft-refresh, got %v", lastFresh)
	}
	if hardCalled {
		t.Error("hardFn should NOT have been called for soft-refresh")
	}
	if result.Output == "" {
		t.Error("expected non-empty output")
	}
}

// TestResetTool_hardMode verifies the tool executes hard reset when mode="hard".
func TestResetTool_hardMode(t *testing.T) {
	softCalled := false
	hardCalled := false
	softFn := func(_ context.Context, _ bool) error { softCalled = true; return nil }
	hardFn := func(_ context.Context) error { hardCalled = true; return nil }

	rt := session.NewResetTool(softFn, hardFn)
	_, err := rt.Execute(context.Background(), map[string]any{"mode": "hard"})
	if err != nil {
		t.Fatalf("Execute hard: %v", err)
	}
	if !hardCalled {
		t.Error("hardFn should have been called for mode=hard")
	}
	if softCalled {
		t.Error("softFn should NOT have been called for mode=hard")
	}
}

// TestResetTool_softFallbackToHard verifies that when softFn returns
// ErrNoCompactionBoundary, the tool automatically falls back to hardResetFn.
func TestResetTool_softFallbackToHard(t *testing.T) {
	hardCalled := false
	softFn := func(_ context.Context, _ bool) error { return session.ErrNoCompactionBoundary }
	hardFn := func(_ context.Context) error { hardCalled = true; return nil }

	rt := session.NewResetTool(softFn, hardFn)
	result, err := rt.Execute(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !hardCalled {
		t.Error("hardFn should have been called as fallback")
	}
	if result.Output == "" {
		t.Error("expected non-empty output")
	}
}

// TestResetTool_unknownMode verifies an unknown mode returns a ToolFailure.
func TestResetTool_unknownMode(t *testing.T) {
	rt := session.NewResetTool(
		func(_ context.Context, _ bool) error { return nil },
		func(_ context.Context) error { return nil },
	)
	_, err := rt.Execute(context.Background(), map[string]any{"mode": "nuclear"})
	if err == nil {
		t.Fatal("expected error for unknown mode")
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// ── rollbackPersistStub ───────────────────────────────────────────────────────
// A minimal store.PersistStore that supports seeding records and counting calls.

type rollbackPersistStub struct {
	records      map[int][]store.Record // seq → records
	deletedSeqs  map[int]bool
	findSeqCalls int
}

func newRollbackPersistStub() *rollbackPersistStub {
	return &rollbackPersistStub{
		records:     make(map[int][]store.Record),
		deletedSeqs: make(map[int]bool),
	}
}

func (s *rollbackPersistStub) seedSeq(seq int, recs []store.Record) {
	s.records[seq] = recs
}

func (s *rollbackPersistStub) resetCounts() {
	s.findSeqCalls = 0
	s.deletedSeqs = make(map[int]bool)
}

func (s *rollbackPersistStub) LoadSeqIndex(_ context.Context, _ string, limit int) (map[int][]string, error) {
	result := make(map[int][]string)
	count := 0
	for seq, recs := range s.records {
		if count >= limit {
			break
		}
		ids := make([]string, len(recs))
		for i, r := range recs {
			ids[i] = r.ID
		}
		result[seq] = ids
		count++
	}
	return result, nil
}

func (s *rollbackPersistStub) LoadRecordsBySeq(_ context.Context, _ string, seq int) ([]store.Record, error) {
	return s.records[seq], nil
}

func (s *rollbackPersistStub) FindSeqByDocID(_ context.Context, _ string, docID string) (int, bool, error) {
	s.findSeqCalls++
	for seq, recs := range s.records {
		for _, r := range recs {
			if r.ID == docID {
				return seq, true, nil
			}
		}
	}
	return 0, false, nil
}

func (s *rollbackPersistStub) SaveRecord(_ context.Context, _ string, rec store.Record) error {
	s.records[rec.CompactionSeq] = append(s.records[rec.CompactionSeq], rec)
	return nil
}

func (s *rollbackPersistStub) SaveRecords(_ context.Context, _ string, recs []store.Record) error {
	for _, rec := range recs {
		s.records[rec.CompactionSeq] = append(s.records[rec.CompactionSeq], rec)
	}
	return nil
}

func (s *rollbackPersistStub) DeleteRecordsBySeq(_ context.Context, _ string, seq int) error {
	s.deletedSeqs[seq] = true
	delete(s.records, seq)
	return nil
}

func (s *rollbackPersistStub) DeleteAllRecords(_ context.Context, _ string) error {
	s.records = make(map[int][]store.Record)
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ── soft-refresh tests ─────────────────────────────────────────────────────────

// TestSoftReset_fresh_hidesHead verifies that fresh=true inserts a new
// boundary+summary so FilterCompacted hides all head messages.
func TestSoftReset_fresh_hidesHead(t *testing.T) {
	s, sessID, _ := buildCompactedSession(t)
	ctx := context.Background()

	if _, err := session.SoftReset(ctx, sessID, s, true); err != nil {
		t.Fatalf("SoftReset fresh: %v", err)
	}

	msgs, _ := s.ListMessages(ctx, sessID)
	allParts, _ := s.ListPartsBySession(ctx, sessID)
	visible := session.FilterCompacted(msgs, allParts)

	// fresh=true inserts new boundary+summary → FilterCompacted hides head+tail.
	// Only the new boundary and empty summary are visible (empty summary is
	// skipped by ToModelMessages, so effectively just the boundary user msg).
	// All original head+tail messages (H1,H2,T1,T2) must NOT be in visible.
	visibleIDs := make(map[string]bool, len(visible))
	for _, m := range visible {
		visibleIDs[m.ID] = true
	}
	for _, id := range []string{"msgH1", "msgH2", "msgT1", "msgT2"} {
		if visibleIDs[id] {
			t.Errorf("message %s should be hidden after soft-refresh, but is visible", id)
		}
	}
}

// TestSoftReset_fresh_headStillInDB verifies that fresh=true leaves head
// messages in the store (available for knowledge_search).
func TestSoftReset_fresh_headStillInDB(t *testing.T) {
	s, sessID, _ := buildCompactedSession(t)
	ctx := context.Background()

	if _, err := session.SoftReset(ctx, sessID, s, true); err != nil {
		t.Fatalf("SoftReset fresh: %v", err)
	}

	// All messages still in DB (head + tail + new boundary + new summary).
	allMsgs, _ := s.ListMessages(ctx, sessID)
	allIDs := make(map[string]bool, len(allMsgs))
	for _, m := range allMsgs {
		allIDs[m.ID] = true
	}
	for _, id := range []string{"msgH1", "msgH2", "msgT1", "msgT2"} {
		if !allIDs[id] {
			t.Errorf("message %s should still be in DB after soft-refresh, but is missing", id)
		}
	}
}

// TestSoftReset_softAfterRefresh verifies that a soft reset after a soft-refresh
// correctly peels back the fresh boundary, making the original head visible again.
func TestSoftReset_softAfterRefresh(t *testing.T) {
	s, sessID, _ := buildCompactedSession(t)
	ctx := context.Background()

	// Step 1: soft-refresh → fresh boundary inserted, head hidden.
	if _, err := session.SoftReset(ctx, sessID, s, true); err != nil {
		t.Fatalf("SoftReset fresh: %v", err)
	}

	// Verify head is hidden.
	msgs, _ := s.ListMessages(ctx, sessID)
	allParts, _ := s.ListPartsBySession(ctx, sessID)
	visible := session.FilterCompacted(msgs, allParts)
	for _, m := range visible {
		if m.ID == "msgH1" || m.ID == "msgH2" {
			t.Errorf("after soft-refresh, head msg %s should be hidden", m.ID)
		}
	}

	// Step 2: soft reset → fresh boundary deleted, head visible again.
	if _, err := session.SoftReset(ctx, sessID, s, false); err != nil {
		t.Fatalf("SoftReset after refresh: %v", err)
	}

	msgs, _ = s.ListMessages(ctx, sessID)
	allParts, _ = s.ListPartsBySession(ctx, sessID)
	visible = session.FilterCompacted(msgs, allParts)

	// No boundary left → all remaining messages visible.
	wantIDs := []string{"msgH1", "msgH2", "msgT1", "msgT2"}
	gotIDs := make([]string, len(visible))
	for i, m := range visible {
		gotIDs[i] = m.ID
	}
	if !equalStringSlice(gotIDs, wantIDs) {
		t.Errorf("after soft following soft-refresh: visible=%v, want %v", gotIDs, wantIDs)
	}
}

// ── RollbackTo tests ──────────────────────────────────────────────────────────
// buildHistorySrc creates a SessionHistorySource (pure-memory, no SQLite),
// fires the Hook with the given messages, and returns the source.
func buildHistorySrc(t *testing.T, sessID string, head []*store.Message, parts map[string][]*store.Part) *store.SessionHistorySource {
	t.Helper()
	src, err := store.NewSessionHistorySource(sessID, 0, 0, nil)
	if err != nil {
		t.Fatalf("NewSessionHistorySource: %v", err)
	}
	hook := src.Hook()
	hook(head, parts)
	return src
}

// TestRollbackTo_clearsBleve verifies that after RollbackTo, the head message
// IDs are no longer findable via Peek.
func TestRollbackTo_clearsBleve(t *testing.T) {
	ctx := context.Background()
	sessID := "sess-rb"

	head := []*store.Message{
		{ID: "h1", Role: store.RoleUser, SessionID: sessID},
		{ID: "h2", Role: store.RoleAssistant, SessionID: sessID},
	}
	parts := map[string][]*store.Part{
		"h1": {{Type: store.PartTypeText, Data: &store.TextPartData{Text: "协程调度问题"}}},
		"h2": {{Type: store.PartTypeText, Data: &store.TextPartData{Text: "系统设计讨论"}}},
	}

	src := buildHistorySrc(t, sessID, head, parts)

	// Verify the docs are searchable before rollback.
	results, err := src.Peek(ctx, knowledge.Query{Type: knowledge.QueryTypeSearch, Input: "协程", MaxResults: 5})
	if err != nil {
		t.Fatalf("Peek before rollback: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected search results before rollback")
	}

	// RollbackTo with both head message IDs.
	src.RollbackTo(ctx, []string{"h1", "h2"})

	// After rollback, search should return no results.
	results, err = src.Peek(ctx, knowledge.Query{Type: knowledge.QueryTypeSearch, Input: "协程", MaxResults: 5})
	if err != nil {
		t.Fatalf("Peek after rollback: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results after rollback, got %d", len(results))
	}
}

// TestRollbackTo_noOrphanAfterRecompact verifies that after soft reset +
// rollback, a second compaction of the same head messages does not produce
// duplicate search results.
func TestRollbackTo_noOrphanAfterRecompact(t *testing.T) {
	ctx := context.Background()
	sessID := "sess-recompact"

	head := []*store.Message{
		{ID: "h1", Role: store.RoleUser, SessionID: sessID},
	}
	parts := map[string][]*store.Part{
		"h1": {{Type: store.PartTypeText, Data: &store.TextPartData{Text: "机器学习训练"}}},
	}

	src := buildHistorySrc(t, sessID, head, parts)

	// First compaction indexed h1 as seq=1. Simulate soft reset + rollback.
	src.RollbackTo(ctx, []string{"h1"})

	// Second compaction re-indexes the same message (seq=2 now).
	hook := src.Hook()
	hook(head, parts)

	// Search should return exactly 1 result (not 2 duplicates).
	results, err := src.Peek(ctx, knowledge.Query{Type: knowledge.QueryTypeSearch, Input: "机器学习", MaxResults: 10})
	if err != nil {
		t.Fatalf("Peek after recompact: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("expected 1 result after recompact, got %d (possible duplicate)", len(results))
	}
}

// TestRollbackTo_emptyIDs is a no-op and must not panic.
func TestRollbackTo_emptyIDs(t *testing.T) {
	ctx := context.Background()
	src, _ := store.NewSessionHistorySource("sess-empty", 0, 0, nil)
	src.RollbackTo(ctx, nil)
	src.RollbackTo(ctx, []string{})
}

// TestRollbackTo_coldPath verifies that RollbackTo cleans up history_docs
// even when the seq has been LRU-evicted from L0 (cold path via FindSeqByDocID).
func TestRollbackTo_coldPath(t *testing.T) {
	ctx := context.Background()
	sessID := "sess-cold"

	// Use a stub PersistStore so we can observe FindSeqByDocID and DeleteRecordsBySeq calls.
	ps := newRollbackPersistStub()

	// maxCompactions=1, maxIndexedSeqs=1 → seq 1 will be evicted from L0
	// as soon as seq 2 is indexed.
	src, err := store.NewSessionHistorySource(sessID, 1, 1, ps)
	if err != nil {
		t.Fatalf("NewSessionHistorySource: %v", err)
	}
	hook := src.Hook()

	// Compaction seq=1: index message "h1".
	ps.seedSeq(1, []store.Record{{ID: "h1", Role: "user", Text: "first", CompactionSeq: 1}})
	hook([]*store.Message{{ID: "h1", Role: store.RoleUser, SessionID: sessID}},
		map[string][]*store.Part{"h1": {{Type: store.PartTypeText, Data: &store.TextPartData{Text: "first"}}}})

	// Compaction seq=2: index message "h2". This evicts seq=1 from L0 (maxIndexedSeqs=1).
	ps.seedSeq(2, []store.Record{{ID: "h2", Role: "user", Text: "second", CompactionSeq: 2}})
	hook([]*store.Message{{ID: "h2", Role: store.RoleUser, SessionID: sessID}},
		map[string][]*store.Part{"h2": {{Type: store.PartTypeText, Data: &store.TextPartData{Text: "second"}}}})

	// At this point h1/seq=1 is evicted from L0 (cold).
	// RollbackTo with h1 must still delete seq=1 from SQLite via FindSeqByDocID.
	ps.resetCounts()
	src.RollbackTo(ctx, []string{"h1"})

	if ps.findSeqCalls == 0 {
		t.Error("expected FindSeqByDocID to be called for cold ID h1")
	}
	if !ps.deletedSeqs[1] {
		t.Errorf("expected seq=1 to be deleted from SQLite, deletedSeqs=%v", ps.deletedSeqs)
	}
}
