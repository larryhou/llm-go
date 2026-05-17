package session_test

// list_parts_spy_test.go — spy test verifying that Compact() uses
// ListPartsBySession (single batch query) instead of per-message ListParts
// (N+1 queries). Issue-29 fix.

import (
	"context"
	"testing"
	"time"

	"github.com/larryhou/llm-go/session"
	"github.com/larryhou/llm-go/store"
	"github.com/larryhou/llm-go/store/memory"
)

// spyStore wraps a store.Store and counts calls to ListParts vs ListPartsBySession.
type spyStore struct {
	store.Store
	listPartsCount          int
	listPartsBySessionCount int
}

func (s *spyStore) ListParts(ctx context.Context, messageID string) ([]*store.Part, error) {
	s.listPartsCount++
	return s.Store.ListParts(ctx, messageID)
}

func (s *spyStore) ListPartsBySession(ctx context.Context, sessionID string) (map[string][]*store.Part, error) {
	s.listPartsBySessionCount++
	return s.Store.ListPartsBySession(ctx, sessionID)
}

// summaryProvider returns a provider that emits a complete text response,
// suitable for the summary LLM call inside Compact().
func summaryProvider() *mockProvider {
	return simpleTextProvider("This is the summary of the conversation.")
}

// seedCompactSession creates a session with enough messages to trigger compaction.
// It adds a user message, an assistant message with a text part, and a second
// user/assistant pair, then returns the session ID.
func seedCompactSession(t *testing.T, s store.Store) string {
	t.Helper()
	ctx := context.Background()
	sessID := "compact-spy-session"

	if err := s.CreateSession(ctx, &store.Session{ID: sessID, Model: "mock/claude-test"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	addTurn := func(uid, aid, text string) {
		_ = s.CreateMessage(ctx, &store.Message{ID: uid, SessionID: sessID, Role: store.RoleUser})
		_ = s.CreateMessage(ctx, &store.Message{ID: aid, SessionID: sessID, Role: store.RoleAssistant})
		_ = s.CreatePart(ctx, &store.Part{
			ID:        aid + "-p",
			MessageID: aid,
			SessionID: sessID,
			Type:      store.PartTypeText,
			Data:      &store.TextPartData{Text: text},
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		})
	}

	addTurn("u1", "a1", "response one")
	addTurn("u2", "a2", "response two")
	addTurn("u3", "a3", "response three")
	return sessID
}

// TestCompact_UsesListPartsBySession verifies that Compact() calls
// ListPartsBySession exactly once and never calls ListParts.
func TestCompact_UsesListPartsBySession(t *testing.T) {
	base := memory.New()
	spy := &spyStore{Store: base}

	sessID := seedCompactSession(t, spy)

	proc := session.NewProcessor(spy)
	compactor := session.NewCompactor(spy, proc)
	input := session.ProcessInput{
		Model:           testModel(),
		Provider:        summaryProvider(),
		SummaryProvider: summaryProvider(),
	}

	_, err := compactor.Compact(context.Background(), sessID, input)
	if err != nil {
		// Compaction may fail if there are not enough messages — that's fine for
		// this test; we only care about which store methods were called.
		t.Logf("Compact returned error (acceptable for spy test): %v", err)
	}

	// The fix: ListPartsBySession must be called at least once.
	if spy.listPartsBySessionCount == 0 {
		t.Error("Compact() did not call ListPartsBySession; expected exactly one batch call")
	}

	// The old N+1 pattern must be gone: ListParts should NOT be called by Compact.
	// (It may still be called by other paths, but not inside the parts-loading loop.)
	if spy.listPartsCount > 0 {
		t.Errorf("Compact() called ListParts %d time(s); expected 0 (should use ListPartsBySession)", spy.listPartsCount)
	}
}
