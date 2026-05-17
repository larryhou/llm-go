package store

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
	// LoadSeqIndex returns a lightweight seq→[]docID map for the most recent
	// `limit` compaction sequences. Only the two index columns (seq, id) are
	// read — no text payload — so this is cheap even for large histories.
	// Used at startup to restore the L0 compactionDocs map without loading
	// document content into Bleve.
	LoadSeqIndex(ctx context.Context, sessionID string, limit int) (map[int][]string, error)

	// LoadRecordsBySeq returns all Records for a single compaction sequence.
	// Called during a Bleve page-in (Fetch path) to load exactly one seq.
	// Replaces the previous full-table LoadRecords call.
	LoadRecordsBySeq(ctx context.Context, sessionID string, seq int) ([]Record, error)

	// FindSeqByDocID returns the compaction_seq that owns docID.
	// Called during Fetch when the docID is not yet in compactionDocs (L0 miss).
	FindSeqByDocID(ctx context.Context, sessionID string, docID string) (seq int, found bool, err error)

	// SaveRecord persists one record. Called synchronously inside the
	// CompactionHook so a clean process exit never loses a round.
	SaveRecord(ctx context.Context, sessionID string, rec Record) error

	// SaveRecords persists a batch of records atomically in a single
	// transaction. All records share the same compaction_seq. Preferred over
	// individual SaveRecord calls inside Hook() to guarantee that a partial
	// write caused by a mid-flush crash never leaves a residual incomplete seq
	// in history_docs.
	SaveRecords(ctx context.Context, sessionID string, recs []Record) error

	// DeleteRecordsBySeq permanently removes all records for the given
	// compaction round from storage. Called during Reset() to honour the
	// user's intent to wipe session memory.
	DeleteRecordsBySeq(ctx context.Context, sessionID string, seq int) error

	// DeleteAllRecords permanently removes all history_docs for this session.
	// Called during Reset() to guarantee a full wipe even when some seqs have
	// been evicted from the in-memory L0 window.
	DeleteAllRecords(ctx context.Context, sessionID string) error
}
