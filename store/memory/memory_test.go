package memory_test

import (
	"context"
	"sync"
	"testing"

	"github.com/larryhou/llm-go/store"
	"github.com/larryhou/llm-go/store/memory"
)

func newStore() *memory.Store { return memory.New() }

// --- Session ---

func TestSession_createAndGet(t *testing.T) {
	s := newStore()
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
	s := newStore()
	ctx := context.Background()
	sess := &store.Session{ID: "dup"}
	_ = s.CreateSession(ctx, sess)
	if err := s.CreateSession(ctx, sess); err == nil {
		t.Error("expected error for duplicate session ID")
	}
}

func TestSession_notFound(t *testing.T) {
	s := newStore()
	_, err := s.GetSession(context.Background(), "nope")
	if err == nil {
		t.Error("expected error for missing session")
	}
}

func TestSession_update(t *testing.T) {
	s := newStore()
	ctx := context.Background()
	_ = s.CreateSession(ctx, &store.Session{ID: "u1", Title: "old"})
	_ = s.UpdateSession(ctx, &store.Session{ID: "u1", Title: "new"})
	got, _ := s.GetSession(ctx, "u1")
	if got.Title != "new" {
		t.Errorf("Title = %q, want new", got.Title)
	}
}

func TestSession_list(t *testing.T) {
	s := newStore()
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

// --- Message ---

func TestMessage_createAndList(t *testing.T) {
	s := newStore()
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
	// insertion order preserved
	if list[0].ID != "m1" || list[1].ID != "m2" {
		t.Error("order not preserved")
	}
}

func TestMessage_updateError(t *testing.T) {
	s := newStore()
	err := s.UpdateMessage(context.Background(), &store.Message{ID: "missing"})
	if err == nil {
		t.Error("expected error updating missing message")
	}
}

// --- Part ---

func TestPart_createAndList(t *testing.T) {
	s := newStore()
	ctx := context.Background()
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
	s := newStore()
	ctx := context.Background()
	_ = s.CreatePart(ctx, &store.Part{
		ID:        "p1",
		MessageID: "msg",
		Type:      store.PartTypeTool,
		Data:      &store.ToolPartData{Status: store.ToolStatusPending},
	})
	// Update status to completed
	p, _ := s.GetPart(ctx, "p1")
	p.Data = &store.ToolPartData{Status: store.ToolStatusCompleted, Output: "done"}
	if err := s.UpdatePart(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetPart(ctx, "p1")
	d, ok := got.Data.(*store.ToolPartData)
	if !ok || d.Status != store.ToolStatusCompleted {
		t.Errorf("status not updated: %+v", got.Data)
	}
}

func TestPart_isolation(t *testing.T) {
	// Ensure returned parts are copies — mutating returned value does not affect store
	s := newStore()
	ctx := context.Background()
	_ = s.CreatePart(ctx, &store.Part{ID: "p1", MessageID: "m1", Type: store.PartTypeText,
		Data: &store.TextPartData{Text: "original"}})

	p, _ := s.GetPart(ctx, "p1")
	p.Type = "mutated"

	p2, _ := s.GetPart(ctx, "p1")
	if p2.Type == "mutated" {
		t.Error("store returned a reference instead of a copy")
	}
}

// --- Concurrency ---

func TestConcurrentWrites(t *testing.T) {
	s := newStore()
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := string(rune('A' + n%26))
			_ = s.CreateSession(ctx, &store.Session{ID: id + string(rune('0'+n))})
		}(i)
	}
	wg.Wait()
	sessions, _ := s.ListSessions(ctx)
	if len(sessions) != 50 {
		t.Errorf("concurrent creates: got %d sessions, want 50", len(sessions))
	}
}
