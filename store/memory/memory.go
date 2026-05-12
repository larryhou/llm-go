// Package memory provides an in-memory implementation of store.Store.
// Suitable for testing and short-lived sessions.
package memory

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/larryhou/llm-go/store"
)

// Store is a thread-safe in-memory store.
type Store struct {
	mu       sync.RWMutex
	sessions map[string]*store.Session
	messages map[string]*store.Message
	parts    map[string]*store.Part
	// index: insertion-order session IDs (mirrors sessionMsgs design)
	sessionOrder []string
	// index: sessionID -> []messageID (insertion order)
	sessionMsgs map[string][]string
	// index: messageID -> []partID (insertion order)
	messageParts map[string][]string
}

// New creates a new in-memory Store.
func New() *Store {
	return &Store{
		sessions:     make(map[string]*store.Session),
		messages:     make(map[string]*store.Message),
		parts:        make(map[string]*store.Part),
		sessionMsgs:  make(map[string][]string),
		messageParts: make(map[string][]string),
	}
}

// --- Session ---

func (s *Store) CreateSession(_ context.Context, sess *store.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.sessions[sess.ID]; exists {
		return fmt.Errorf("session %q already exists", sess.ID)
	}
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = time.Now()
	}
	sess.UpdatedAt = time.Now()
	cp := *sess
	s.sessions[sess.ID] = &cp
	s.sessionOrder = append(s.sessionOrder, sess.ID)
	return nil
}

func (s *Store) GetSession(_ context.Context, id string) (*store.Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session %q not found", id)
	}
	cp := *sess
	return &cp, nil
}

func (s *Store) UpdateSession(_ context.Context, sess *store.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[sess.ID]; !ok {
		return fmt.Errorf("session %q not found", sess.ID)
	}
	sess.UpdatedAt = time.Now()
	cp := *sess
	s.sessions[sess.ID] = &cp
	return nil
}

func (s *Store) ListSessions(_ context.Context) ([]*store.Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*store.Session, 0, len(s.sessionOrder))
	for _, id := range s.sessionOrder {
		if sess, ok := s.sessions[id]; ok {
			cp := *sess
			out = append(out, &cp)
		}
	}
	return out, nil
}

// --- Message ---

func (s *Store) CreateMessage(_ context.Context, m *store.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.messages[m.ID]; exists {
		return fmt.Errorf("message %q already exists", m.ID)
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now()
	}
	m.UpdatedAt = time.Now()
	cp := *m
	s.messages[m.ID] = &cp
	s.sessionMsgs[m.SessionID] = append(s.sessionMsgs[m.SessionID], m.ID)
	return nil
}

func (s *Store) GetMessage(_ context.Context, id string) (*store.Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.messages[id]
	if !ok {
		return nil, fmt.Errorf("message %q not found", id)
	}
	cp := *m
	return &cp, nil
}

func (s *Store) UpdateMessage(_ context.Context, m *store.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.messages[m.ID]; !ok {
		return fmt.Errorf("message %q not found", m.ID)
	}
	m.UpdatedAt = time.Now()
	cp := *m
	s.messages[m.ID] = &cp
	return nil
}

func (s *Store) ListMessages(_ context.Context, sessionID string) ([]*store.Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := s.sessionMsgs[sessionID]
	out := make([]*store.Message, 0, len(ids))
	for _, id := range ids {
		if m, ok := s.messages[id]; ok {
			cp := *m
			out = append(out, &cp)
		}
	}
	return out, nil
}

// --- Part ---

func (s *Store) CreatePart(_ context.Context, p *store.Part) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.parts[p.ID]; exists {
		return fmt.Errorf("part %q already exists", p.ID)
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now()
	}
	p.UpdatedAt = time.Now()
	cp := *p
	s.parts[p.ID] = &cp
	s.messageParts[p.MessageID] = append(s.messageParts[p.MessageID], p.ID)
	return nil
}

func (s *Store) GetPart(_ context.Context, id string) (*store.Part, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.parts[id]
	if !ok {
		return nil, fmt.Errorf("part %q not found", id)
	}
	cp := *p
	return &cp, nil
}

func (s *Store) UpdatePart(_ context.Context, p *store.Part) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.parts[p.ID]; !ok {
		return fmt.Errorf("part %q not found", p.ID)
	}
	p.UpdatedAt = time.Now()
	cp := *p
	s.parts[p.ID] = &cp
	return nil
}

func (s *Store) ListParts(_ context.Context, messageID string) ([]*store.Part, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := s.messageParts[messageID]
	out := make([]*store.Part, 0, len(ids))
	for _, id := range ids {
		if p, ok := s.parts[id]; ok {
			cp := *p
			out = append(out, &cp)
		}
	}
	return out, nil
}
