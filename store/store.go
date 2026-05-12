// Package store defines the persistence interface for sessions, messages, and parts.
// Aligned with opencode's SQLite schema in packages/opencode/src/session/.
package store

import (
	"context"
	"time"
)

// Store is the unified persistence interface.
type Store interface {
	// Session operations
	CreateSession(ctx context.Context, s *Session) error
	GetSession(ctx context.Context, id string) (*Session, error)
	UpdateSession(ctx context.Context, s *Session) error
	ListSessions(ctx context.Context) ([]*Session, error)

	// Message operations
	CreateMessage(ctx context.Context, m *Message) error
	GetMessage(ctx context.Context, id string) (*Message, error)
	UpdateMessage(ctx context.Context, m *Message) error
	ListMessages(ctx context.Context, sessionID string) ([]*Message, error)

	// Part operations
	CreatePart(ctx context.Context, p *Part) error
	GetPart(ctx context.Context, id string) (*Part, error)
	UpdatePart(ctx context.Context, p *Part) error
	ListParts(ctx context.Context, messageID string) ([]*Part, error)
}

// Session is a top-level conversation container.
type Session struct {
	ID            string
	Title         string
	Model         string // "provider/model"
	AgentID       string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	ParentID      string // for sub-sessions
	Cost          float64
	Tokens        TokenSummary
}

// TokenSummary tracks cumulative token usage for a session.
type TokenSummary struct {
	Input      int
	Output     int
	CacheRead  int
	CacheWrite int
}

// Message is a single turn in a conversation.
type Message struct {
	ID        string
	SessionID string
	Role      string    // user | assistant
	Model     string    // model used (for assistant messages)
	CreatedAt time.Time
	UpdatedAt time.Time
	Error     *MessageError // set if assistant turn errored
	Summary   bool          // true if this is a compaction summary message
	Tokens    TokenSummary
}

// MessageError represents an error on an assistant message.
type MessageError struct {
	Name        string
	Message     string
	Data        map[string]any
}

// Part is a typed sub-element of a Message.
type Part struct {
	ID        string
	MessageID string
	SessionID string
	Type      string // text | reasoning | tool | step-start | step-finish | compaction | ...
	CreatedAt time.Time
	UpdatedAt time.Time

	// Type-specific payload (stored as JSON in the DB)
	Data any
}

// PartType discriminators, aligned with message-v2.ts part types.
const (
	PartTypeText       = "text"
	PartTypeReasoning  = "reasoning"
	PartTypeTool       = "tool"
	PartTypeStepStart  = "step-start"
	PartTypeStepFinish = "step-finish"
	PartTypeCompaction = "compaction"
	PartTypeSnapshot   = "snapshot"
	PartTypeRetry      = "retry"
	PartTypeAgent      = "agent"
	PartTypeSubtask    = "subtask"
	PartTypePatch      = "patch"
)

// TextPartData holds data for a text part.
type TextPartData struct {
	Text      string
	TimeStart int64  // unix ms
	TimeEnd   int64  // unix ms; 0 = still streaming
}

// ReasoningPartData holds data for a reasoning/thinking part.
type ReasoningPartData struct {
	Text      string
	Signature string
	TimeStart int64
	TimeEnd   int64
}

// ToolPartStatus discriminators, aligned with message-v2.ts ToolState.
const (
	ToolStatusPending   = "pending"
	ToolStatusRunning   = "running"
	ToolStatusCompleted = "completed"
	ToolStatusError     = "error"
)

// ToolPartData holds data for a tool call part.
// Aligned with message-v2.ts ToolState union.
type ToolPartData struct {
	Tool      string         // tool name
	CallID    string         // tool_call_id from LLM
	Status    string         // pending | running | completed | error
	Input     map[string]any // decoded input arguments
	Output    string         // tool result text (when completed)
	Title     string
	Error     string         // error message (when status=error)
	Metadata  map[string]any
	TimeStart int64
	TimeEnd   int64
	Compacted int64  // unix ms when output was pruned; 0 = not pruned
	Interrupted bool // true if tool was running when session aborted
}

// StepFinishData holds data for a step-finish part.
type StepFinishData struct {
	FinishReason string
	Usage        TokenSummary
	Cost         float64
}

// CompactionPartData marks a point where compaction occurred.
type CompactionPartData struct {
	SummaryMessageID string
}

// RetryPartData records a retry attempt.
type RetryPartData struct {
	Attempt int
	Message string
}

// DataAs extracts the typed payload from a Part.
// It centralises the runtime type assertion so callers get a single point
// of failure rather than 14 silent ok=false branches scattered across the
// session package. When the assertion fails (e.g. after a JSON round-trip
// through a SQL store), the zero value is returned with ok=false.
//
// Usage:
//
//	d, ok := store.DataAs[*store.ToolPartData](p)
func DataAs[T any](p *Part) (T, bool) {
	v, ok := p.Data.(T)
	return v, ok
}
