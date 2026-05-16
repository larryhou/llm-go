// Package knowledge provides session history recall via a per-session Bleve
// in-memory index, integrated with the knowledge_search / knowledge_fetch tools.
package knowledge

import "context"

// PersistStore is the persistent storage backend for session-scoped records.
// It stores and retrieves Record values keyed by (sessionID, seq, record.ID)
// without being tied to any particular storage technology.
//
// PersistStore is intentionally separate from Source: a store (e.g. sqlite.Store)
// implements PersistStore for durability, while sqlite.HistorySource wraps it
// and also implements Source so the Manager can query it directly.
//
// Inject a PersistStore into NewSessionHistorySource to enable L2 persistence.
// Pass nil for pure-memory mode (history lost on restart).
//
// Dependency graph is cycle-free:
//
//	knowledge    →  store            (Record uses store.Message/Part types)
//	store/sqlite →  knowledge        (sqlite.Store implements PersistStore;
//	                                  sqlite.HistorySource implements Source)
//	cmd/*        →  store/sqlite     (injection point — no cycle)
type PersistStore interface {
	// LoadRecords returns all persisted records for the session, grouped by
	// compaction_seq. Called once at SessionHistorySource startup to restore
	// the known seq map without loading content into Bleve (lazy page-in).
	LoadRecords(ctx context.Context, sessionID string) (map[int][]Record, error)

	// SaveRecord persists one record. Called synchronously inside the
	// CompactionHook so a clean process exit never loses a round.
	SaveRecord(ctx context.Context, sessionID string, rec Record) error

	// DeleteRecordsBySeq permanently removes all records for the given
	// compaction round from storage. Called during Reset() to honour the
	// user's intent to wipe session memory.
	DeleteRecordsBySeq(ctx context.Context, sessionID string, seq int) error
}
