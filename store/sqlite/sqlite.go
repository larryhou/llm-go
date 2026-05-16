// Package sqlite provides a SQLite-backed implementation of store.Store
// and knowledge.PersistStore, using modernc.org/sqlite (pure Go, no CGO).
//
// Open a store with:
//
//	st, err := sqlite.Open("./data.db")
//
// The database is created automatically if it does not exist. Schema
// migrations are applied on every Open() call via embedded SQL files —
// already-applied migrations are skipped using the schema_migrations table.
//
// Connection pragmas applied on every Open():
//
//	PRAGMA journal_mode=WAL   — write-ahead logging; concurrent reads during writes
//	PRAGMA foreign_keys=ON    — enforce ON DELETE CASCADE for messages/parts
//	PRAGMA busy_timeout=5000  — wait up to 5 s instead of returning SQLITE_BUSY
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	_ "modernc.org/sqlite" // register "sqlite" driver

	"github.com/larryhou/llm-go/knowledge"
	"github.com/larryhou/llm-go/store"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store is the SQLite-backed implementation of store.Store and
// knowledge.PersistStore.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database at path and applies any pending
// migrations. Returns a ready-to-use *Store.
func Open(path string) (*Store, error) {
	// modernc/sqlite DSN pragmas use _pragma=name(value) syntax.
	// We set foreign_keys and journal_mode via PRAGMA after open to be safe.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("sqlite.Open %q: %w", path, err)
	}
	// Single writer at a time is sufficient; WAL mode allows concurrent readers.
	db.SetMaxOpenConns(1)

	// Apply connection-level pragmas.
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("sqlite.Open pragma %q: %w", pragma, err)
		}
	}

	if err := runMigrations(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite.Open migrations: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the underlying database connection.
func (s *Store) Close() error { return s.db.Close() }

// ── migrations ────────────────────────────────────────────────────────────────

func runMigrations(db *sql.DB) error {
	// Ensure the tracking table exists before we read from it.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for version, entry := range entries {
		v := version + 1 // 1-based
		var exists int
		_ = db.QueryRow(`SELECT 1 FROM schema_migrations WHERE version = ?`, v).Scan(&exists)
		if exists == 1 {
			continue
		}
		sql, err := migrationsFS.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}
		if _, err := db.Exec(string(sql)); err != nil {
			return fmt.Errorf("apply migration %s: %w", entry.Name(), err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES (?,?)`,
			v, time.Now().UnixMilli()); err != nil {
			return fmt.Errorf("record migration %s: %w", entry.Name(), err)
		}
	}
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func toMillis(t time.Time) int64 {
	if t.IsZero() {
		return time.Now().UnixMilli()
	}
	return t.UnixMilli()
}

func fromMillis(ms int64) time.Time {
	return time.UnixMilli(ms)
}

func marshalData(v any) (string, error) {
	if v == nil {
		return "{}", nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "{}", err
	}
	return string(b), nil
}

// scanPart reads a Part row. data is a raw JSON string; Part.Data is set to
// the decoded map[string]any so that store.DataAs[T] JSON round-trip works.
func scanPart(id, messageID, sessionID, typ, dataJSON string, createdAt, updatedAt int64) (*store.Part, error) {
	var raw any
	if err := json.Unmarshal([]byte(dataJSON), &raw); err != nil {
		raw = map[string]any{}
	}
	return &store.Part{
		ID:        id,
		MessageID: messageID,
		SessionID: sessionID,
		Type:      typ,
		Data:      raw,
		CreatedAt: fromMillis(createdAt),
		UpdatedAt: fromMillis(updatedAt),
	}, nil
}

// ── store.Store: Session ──────────────────────────────────────────────────────

func (s *Store) CreateSession(ctx context.Context, sess *store.Session) error {
	now := time.Now()
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = now
	}
	sess.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions
			(id, title, model, agent_id, parent_id, cost,
			 tokens_input, tokens_output, tokens_cache_read, tokens_cache_write,
			 created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		sess.ID, sess.Title, sess.Model, sess.AgentID, sess.ParentID, sess.Cost,
		sess.Tokens.Input, sess.Tokens.Output, sess.Tokens.CacheRead, sess.Tokens.CacheWrite,
		toMillis(sess.CreatedAt), toMillis(sess.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("CreateSession %q: %w", sess.ID, err)
	}
	return nil
}

func (s *Store) GetSession(ctx context.Context, id string) (*store.Session, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, title, model, agent_id, parent_id, cost,
		       tokens_input, tokens_output, tokens_cache_read, tokens_cache_write,
		       created_at, updated_at
		FROM sessions WHERE id = ?`, id)
	sess := &store.Session{}
	var createdAt, updatedAt int64
	err := row.Scan(
		&sess.ID, &sess.Title, &sess.Model, &sess.AgentID, &sess.ParentID, &sess.Cost,
		&sess.Tokens.Input, &sess.Tokens.Output, &sess.Tokens.CacheRead, &sess.Tokens.CacheWrite,
		&createdAt, &updatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("session %q not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("GetSession %q: %w", id, err)
	}
	sess.CreatedAt = fromMillis(createdAt)
	sess.UpdatedAt = fromMillis(updatedAt)
	return sess, nil
}

func (s *Store) UpdateSession(ctx context.Context, sess *store.Session) error {
	sess.UpdatedAt = time.Now()
	res, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET
			title=?, model=?, agent_id=?, parent_id=?, cost=?,
			tokens_input=?, tokens_output=?, tokens_cache_read=?, tokens_cache_write=?,
			updated_at=?
		WHERE id=?`,
		sess.Title, sess.Model, sess.AgentID, sess.ParentID, sess.Cost,
		sess.Tokens.Input, sess.Tokens.Output, sess.Tokens.CacheRead, sess.Tokens.CacheWrite,
		toMillis(sess.UpdatedAt), sess.ID,
	)
	if err != nil {
		return fmt.Errorf("UpdateSession %q: %w", sess.ID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("session %q not found", sess.ID)
	}
	return nil
}

func (s *Store) ListSessions(ctx context.Context) ([]*store.Session, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, title, model, agent_id, parent_id, cost,
		       tokens_input, tokens_output, tokens_cache_read, tokens_cache_write,
		       created_at, updated_at
		FROM sessions ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("ListSessions: %w", err)
	}
	defer rows.Close()
	var out []*store.Session
	for rows.Next() {
		sess := &store.Session{}
		var createdAt, updatedAt int64
		if err := rows.Scan(
			&sess.ID, &sess.Title, &sess.Model, &sess.AgentID, &sess.ParentID, &sess.Cost,
			&sess.Tokens.Input, &sess.Tokens.Output, &sess.Tokens.CacheRead, &sess.Tokens.CacheWrite,
			&createdAt, &updatedAt,
		); err != nil {
			return nil, fmt.Errorf("ListSessions scan: %w", err)
		}
		sess.CreatedAt = fromMillis(createdAt)
		sess.UpdatedAt = fromMillis(updatedAt)
		out = append(out, sess)
	}
	return out, rows.Err()
}

// DeleteSession removes the session and — via ON DELETE CASCADE — all its
// messages, parts, and history_docs. Idempotent (no error if not found).
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id); err != nil {
		return fmt.Errorf("DeleteSession %q: %w", id, err)
	}
	// history_docs has no FK to sessions; delete separately.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM history_docs WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("DeleteSession history_docs %q: %w", id, err)
	}
	return nil
}

// ── store.Store: Message ──────────────────────────────────────────────────────

func (s *Store) CreateMessage(ctx context.Context, m *store.Message) error {
	now := time.Now()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	m.UpdatedAt = now

	var errName, errMsg, errData *string
	if m.Error != nil {
		errName = &m.Error.Name
		errMsg = &m.Error.Message
		if m.Error.Data != nil {
			b, _ := json.Marshal(m.Error.Data)
			s := string(b)
			errData = &s
		}
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO messages
			(id, session_id, role, model, summary, status,
			 error_name, error_message, error_data,
			 tokens_input, tokens_output, tokens_cache_read, tokens_cache_write,
			 created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.SessionID, m.Role, m.Model, boolToInt(m.Summary), m.Status,
		errName, errMsg, errData,
		m.Tokens.Input, m.Tokens.Output, m.Tokens.CacheRead, m.Tokens.CacheWrite,
		toMillis(m.CreatedAt), toMillis(m.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("CreateMessage %q: %w", m.ID, err)
	}
	return nil
}

func (s *Store) GetMessage(ctx context.Context, id string) (*store.Message, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, session_id, role, model, summary, status,
		       error_name, error_message, error_data,
		       tokens_input, tokens_output, tokens_cache_read, tokens_cache_write,
		       created_at, updated_at
		FROM messages WHERE id = ?`, id)
	m, err := scanMessage(row)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("message %q not found", id)
	}
	return m, err
}

func (s *Store) UpdateMessage(ctx context.Context, m *store.Message) error {
	m.UpdatedAt = time.Now()

	var errName, errMsg, errData *string
	if m.Error != nil {
		errName = &m.Error.Name
		errMsg = &m.Error.Message
		if m.Error.Data != nil {
			b, _ := json.Marshal(m.Error.Data)
			str := string(b)
			errData = &str
		}
	}

	res, err := s.db.ExecContext(ctx, `
		UPDATE messages SET
			role=?, model=?, summary=?, status=?,
			error_name=?, error_message=?, error_data=?,
			tokens_input=?, tokens_output=?, tokens_cache_read=?, tokens_cache_write=?,
			updated_at=?
		WHERE id=?`,
		m.Role, m.Model, boolToInt(m.Summary), m.Status,
		errName, errMsg, errData,
		m.Tokens.Input, m.Tokens.Output, m.Tokens.CacheRead, m.Tokens.CacheWrite,
		toMillis(m.UpdatedAt), m.ID,
	)
	if err != nil {
		return fmt.Errorf("UpdateMessage %q: %w", m.ID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("message %q not found", m.ID)
	}
	return nil
}

func (s *Store) ListMessages(ctx context.Context, sessionID string) ([]*store.Message, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, role, model, summary, status,
		       error_name, error_message, error_data,
		       tokens_input, tokens_output, tokens_cache_read, tokens_cache_write,
		       created_at, updated_at
		FROM messages WHERE session_id = ? ORDER BY created_at ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("ListMessages: %w", err)
	}
	defer rows.Close()
	var out []*store.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("ListMessages scan: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanMessage(sc scanner) (*store.Message, error) {
	m := &store.Message{}
	var createdAt, updatedAt int64
	var summary int
	var errName, errMsg, errData *string
	err := sc.Scan(
		&m.ID, &m.SessionID, &m.Role, &m.Model, &summary, &m.Status,
		&errName, &errMsg, &errData,
		&m.Tokens.Input, &m.Tokens.Output, &m.Tokens.CacheRead, &m.Tokens.CacheWrite,
		&createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}
	m.Summary = summary != 0
	m.CreatedAt = fromMillis(createdAt)
	m.UpdatedAt = fromMillis(updatedAt)
	if errName != nil || errMsg != nil {
		m.Error = &store.MessageError{}
		if errName != nil {
			m.Error.Name = *errName
		}
		if errMsg != nil {
			m.Error.Message = *errMsg
		}
		if errData != nil && *errData != "" {
			var d map[string]any
			if json.Unmarshal([]byte(*errData), &d) == nil {
				m.Error.Data = d
			}
		}
	}
	return m, nil
}

// ── store.Store: Part ─────────────────────────────────────────────────────────

func (s *Store) CreatePart(ctx context.Context, p *store.Part) error {
	now := time.Now()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	dataJSON, err := marshalData(p.Data)
	if err != nil {
		return fmt.Errorf("CreatePart marshal %q: %w", p.ID, err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO parts (id, message_id, session_id, type, data, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?)`,
		p.ID, p.MessageID, p.SessionID, p.Type, dataJSON,
		toMillis(p.CreatedAt), toMillis(p.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("CreatePart %q: %w", p.ID, err)
	}
	return nil
}

func (s *Store) GetPart(ctx context.Context, id string) (*store.Part, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, message_id, session_id, type, data, created_at, updated_at
		FROM parts WHERE id = ?`, id)
	var pid, mid, sid, typ, dataJSON string
	var createdAt, updatedAt int64
	if err := row.Scan(&pid, &mid, &sid, &typ, &dataJSON, &createdAt, &updatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("part %q not found", id)
		}
		return nil, fmt.Errorf("GetPart %q: %w", id, err)
	}
	return scanPart(pid, mid, sid, typ, dataJSON, createdAt, updatedAt)
}

func (s *Store) UpdatePart(ctx context.Context, p *store.Part) error {
	p.UpdatedAt = time.Now()
	dataJSON, err := marshalData(p.Data)
	if err != nil {
		return fmt.Errorf("UpdatePart marshal %q: %w", p.ID, err)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE parts SET type=?, data=?, updated_at=? WHERE id=?`,
		p.Type, dataJSON, toMillis(p.UpdatedAt), p.ID,
	)
	if err != nil {
		return fmt.Errorf("UpdatePart %q: %w", p.ID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("part %q not found", p.ID)
	}
	return nil
}

func (s *Store) ListParts(ctx context.Context, messageID string) ([]*store.Part, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, message_id, session_id, type, data, created_at, updated_at
		FROM parts WHERE message_id = ? ORDER BY created_at ASC`, messageID)
	if err != nil {
		return nil, fmt.Errorf("ListParts: %w", err)
	}
	defer rows.Close()
	return scanParts(rows)
}

// ListPartsBySession returns all parts for every message in the session in a
// single SQL query (avoids N+1 when loading a full conversation).
func (s *Store) ListPartsBySession(ctx context.Context, sessionID string) (map[string][]*store.Part, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, message_id, session_id, type, data, created_at, updated_at
		FROM parts WHERE session_id = ? ORDER BY created_at ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("ListPartsBySession: %w", err)
	}
	defer rows.Close()

	result := make(map[string][]*store.Part)
	for rows.Next() {
		var pid, mid, sid, typ, dataJSON string
		var createdAt, updatedAt int64
		if err := rows.Scan(&pid, &mid, &sid, &typ, &dataJSON, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("ListPartsBySession scan: %w", err)
		}
		p, err := scanPart(pid, mid, sid, typ, dataJSON, createdAt, updatedAt)
		if err != nil {
			return nil, err
		}
		result[mid] = append(result[mid], p)
	}
	return result, rows.Err()
}

func scanParts(rows *sql.Rows) ([]*store.Part, error) {
	var out []*store.Part
	for rows.Next() {
		var pid, mid, sid, typ, dataJSON string
		var createdAt, updatedAt int64
		if err := rows.Scan(&pid, &mid, &sid, &typ, &dataJSON, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scanParts: %w", err)
		}
		p, err := scanPart(pid, mid, sid, typ, dataJSON, createdAt, updatedAt)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ── knowledge.PersistStore ────────────────────────────────────────────────────

// LoadHistoryDocs returns all history documents for a session, grouped by
// compaction_seq. Called once at SessionHistorySource startup to restore the
// known seq map without loading doc content into Bleve (lazy-load).
func (s *Store) LoadHistoryDocs(ctx context.Context, sessionID string) (map[int][]knowledge.HistoryDoc, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, role, text, tool_calls, turn_index, compaction_seq, created_at
		FROM history_docs WHERE session_id = ?
		ORDER BY compaction_seq ASC, turn_index ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("LoadHistoryDocs %q: %w", sessionID, err)
	}
	defer rows.Close()

	result := make(map[int][]knowledge.HistoryDoc)
	for rows.Next() {
		var doc knowledge.HistoryDoc
		var toolCallsJSON string
		if err := rows.Scan(
			&doc.ID, &doc.Role, &doc.Text, &toolCallsJSON,
			&doc.TurnIndex, &doc.CompactionSeq, &doc.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("LoadHistoryDocs scan: %w", err)
		}
		_ = json.Unmarshal([]byte(toolCallsJSON), &doc.ToolCalls)
		result[doc.CompactionSeq] = append(result[doc.CompactionSeq], doc)
	}
	return result, rows.Err()
}

// SaveHistoryDoc persists a single HistoryDoc for future recall.
// Called synchronously inside the CompactionHook.
func (s *Store) SaveHistoryDoc(ctx context.Context, sessionID string, doc knowledge.HistoryDoc) error {
	toolCallsJSON, _ := json.Marshal(doc.ToolCalls)
	_, err := s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO history_docs
			(id, session_id, role, text, tool_calls, turn_index, compaction_seq, created_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		doc.ID, sessionID, doc.Role, doc.Text,
		string(toolCallsJSON), doc.TurnIndex, doc.CompactionSeq, doc.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("SaveHistoryDoc %q: %w", doc.ID, err)
	}
	return nil
}

// DeleteHistoryDocsForSeq removes all history docs for a specific compaction
// round. Called during Reset() to honour the user's intent to wipe memory.
func (s *Store) DeleteHistoryDocsForSeq(ctx context.Context, sessionID string, seq int) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM history_docs WHERE session_id = ? AND compaction_seq = ?`,
		sessionID, seq)
	if err != nil {
		return fmt.Errorf("DeleteHistoryDocsForSeq %q seq=%d: %w", sessionID, seq, err)
	}
	return nil
}

// ── knowledge.Source (HistorySource) ─────────────────────────────────────────

// HistorySource is a knowledge.PersistStore (and therefore knowledge.Source)
// scoped to a single session.  It queries history_docs via SQL and also
// provides the SaveHistoryDoc / DeleteHistoryDocsForSeq persistence methods
// that SessionHistorySource calls to keep the SQLite copy in sync.
//
// Create one per session with NewHistorySource; register it with the Manager
// AND pass it to NewSessionHistorySource as the PersistStore backend.
type HistorySource struct {
	store     *Store
	sessionID string
	priority  int
}

const historySourceIDPrefix = "session-history"

// NewHistorySource creates a HistorySource for the given session.
// priority controls dispatch order inside knowledge.Manager (lower = higher priority).
func NewHistorySource(st *Store, sessionID string, priority int) *HistorySource {
	return &HistorySource{store: st, sessionID: sessionID, priority: priority}
}

// ID implements knowledge.Source.  Returns "session-history:<sessionID>" so
// that Manager can route knowledge_fetch calls directly to this source.
func (h *HistorySource) ID() string { return historySourceIDPrefix + ":" + h.sessionID }

// Priority implements knowledge.Source.
func (h *HistorySource) Priority() int { return h.priority }

// Accepts implements knowledge.Source.
func (h *HistorySource) Accepts(q knowledge.Query) bool {
	return q.Type == knowledge.QueryTypeSearch || q.Type == knowledge.QueryTypeFetch
}

// Peek implements knowledge.Source.  Performs a case-insensitive substring
// search over the text field using SQL LIKE.  Returns up to q.MaxResults
// results with a short snippet.
func (h *HistorySource) Peek(ctx context.Context, q knowledge.Query) ([]knowledge.Result, error) {
	limit := q.MaxResults
	if limit <= 0 {
		limit = 5
	}

	input := q.Input
	var rows *sql.Rows
	var err error
	if input == "" {
		rows, err = h.store.db.QueryContext(ctx, `
			SELECT id, role, text, turn_index, compaction_seq
			FROM history_docs
			WHERE session_id = ?
			ORDER BY compaction_seq DESC, turn_index ASC
			LIMIT ?`, h.sessionID, limit)
	} else {
		rows, err = h.store.db.QueryContext(ctx, `
			SELECT id, role, text, turn_index, compaction_seq
			FROM history_docs
			WHERE session_id = ? AND text LIKE ? ESCAPE '\'
			ORDER BY compaction_seq DESC, turn_index ASC
			LIMIT ?`, h.sessionID, "%"+sqlLikeEscape(input)+"%", limit)
	}
	if err != nil {
		return nil, fmt.Errorf("HistorySource.Peek: %w", err)
	}
	defer rows.Close()

	var results []knowledge.Result
	for rows.Next() {
		var id, role, text string
		var turnIdx, seq int
		if err := rows.Scan(&id, &role, &text, &turnIdx, &seq); err != nil {
			return nil, fmt.Errorf("HistorySource.Peek scan: %w", err)
		}
		snippet := text
		if len(snippet) > 200 {
			snippet = snippet[:200] + "..."
		}
		title := fmt.Sprintf("[来源：历史对话 第%d轮 turn#%d role=%s]", seq, turnIdx, role)
		results = append(results, knowledge.Result{
			RefID:   h.ID() + ":" + id,
			Title:   title,
			Source:  h.ID(),
			Score:   -1,
			Snippet: title + "\n" + snippet,
		})
	}
	return results, rows.Err()
}

// Fetch implements knowledge.Source.  Retrieves the full text of a single
// history document by its ref_id.
func (h *HistorySource) Fetch(ctx context.Context, q knowledge.Query) ([]knowledge.Result, error) {
	// Strip source prefix variants: "session-history:<sessID>:<docID>" or just "<docID>"
	docID := q.Input
	prefix := h.ID() + ":"
	if len(docID) > len(prefix) && docID[:len(prefix)] == prefix {
		docID = docID[len(prefix):]
	}
	// Also strip the bare "session-history:" prefix used by Manager routing.
	if len(docID) > len(historySourceIDPrefix)+1 && docID[:len(historySourceIDPrefix)+1] == historySourceIDPrefix+":" {
		docID = docID[len(historySourceIDPrefix)+1:]
	}

	row := h.store.db.QueryRowContext(ctx, `
		SELECT id, role, text, turn_index, compaction_seq
		FROM history_docs
		WHERE session_id = ? AND id = ?`, h.sessionID, docID)

	var id, role, text string
	var turnIdx, seq int
	if err := row.Scan(&id, &role, &text, &turnIdx, &seq); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("history doc %q not found", docID)
		}
		return nil, fmt.Errorf("HistorySource.Fetch: %w", err)
	}

	title := fmt.Sprintf("[来源：历史对话 第%d轮 turn#%d role=%s]", seq, turnIdx, role)
	return []knowledge.Result{{
		RefID:   h.ID() + ":" + id,
		Title:   title,
		Source:  h.ID(),
		Score:   -1,
		Content: title + "\n\n" + text,
		Metadata: map[string]any{
			"compaction_seq": seq,
			"turn_index":     turnIdx,
			"role":           role,
		},
	}}, nil
}

// LoadRecords implements knowledge.PersistStore.
func (s *Store) LoadRecords(ctx context.Context, sessionID string) (map[int][]knowledge.HistoryDoc, error) {
	return s.LoadHistoryDocs(ctx, sessionID)
}

// SaveRecord implements knowledge.PersistStore.
func (s *Store) SaveRecord(ctx context.Context, sessionID string, doc knowledge.HistoryDoc) error {
	return s.SaveHistoryDoc(ctx, sessionID, doc)
}

// DeleteRecordsBySeq implements knowledge.PersistStore.
func (s *Store) DeleteRecordsBySeq(ctx context.Context, sessionID string, seq int) error {
	return s.DeleteHistoryDocsForSeq(ctx, sessionID, seq)
}

// sqlLikeEscape escapes special LIKE characters in a search term.
func sqlLikeEscape(s string) string {
	var out []byte
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '%', '_', '\\':
			out = append(out, '\\', s[i])
		default:
			out = append(out, s[i])
		}
	}
	return string(out)
}

// ── internal helpers ──────────────────────────────────────────────────────────

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
