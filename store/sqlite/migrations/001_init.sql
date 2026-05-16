-- 001_init.sql: initial schema for sessions, messages, parts, and history_docs.

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
    id                 TEXT    PRIMARY KEY,
    title              TEXT    NOT NULL DEFAULT '',
    model              TEXT    NOT NULL DEFAULT '',
    agent_id           TEXT    NOT NULL DEFAULT '',
    parent_id          TEXT    NOT NULL DEFAULT '',
    cost               REAL    NOT NULL DEFAULT 0,
    tokens_input       INTEGER NOT NULL DEFAULT 0,
    tokens_output      INTEGER NOT NULL DEFAULT 0,
    tokens_cache_read  INTEGER NOT NULL DEFAULT 0,
    tokens_cache_write INTEGER NOT NULL DEFAULT 0,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
    id                 TEXT    PRIMARY KEY,
    session_id         TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    role               TEXT    NOT NULL,
    model              TEXT    NOT NULL DEFAULT '',
    summary            INTEGER NOT NULL DEFAULT 0,
    status             TEXT    NOT NULL DEFAULT '',
    error_name         TEXT,
    error_message      TEXT,
    error_data         TEXT,
    tokens_input       INTEGER NOT NULL DEFAULT 0,
    tokens_output      INTEGER NOT NULL DEFAULT 0,
    tokens_cache_read  INTEGER NOT NULL DEFAULT 0,
    tokens_cache_write INTEGER NOT NULL DEFAULT 0,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, created_at);

CREATE TABLE IF NOT EXISTS parts (
    id         TEXT    PRIMARY KEY,
    message_id TEXT    NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    session_id TEXT    NOT NULL,
    type       TEXT    NOT NULL,
    data       TEXT    NOT NULL DEFAULT '{}',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_parts_message ON parts(message_id, created_at);
CREATE INDEX IF NOT EXISTS idx_parts_session ON parts(session_id,  created_at);

-- history_docs stores compacted session history for knowledge recall.
-- Rows are permanent until the user explicitly resets the session.
CREATE TABLE IF NOT EXISTS history_docs (
    id             TEXT    NOT NULL,
    session_id     TEXT    NOT NULL,
    role           TEXT    NOT NULL DEFAULT '',
    text           TEXT    NOT NULL DEFAULT '',
    tool_calls     TEXT    NOT NULL DEFAULT '[]',
    turn_index     INTEGER NOT NULL DEFAULT 0,
    compaction_seq INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (session_id, compaction_seq, id)
);
CREATE INDEX IF NOT EXISTS idx_history_session ON history_docs(session_id, compaction_seq);
