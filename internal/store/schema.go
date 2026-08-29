package store

// schemaSQL is applied inside one transaction at Open. Every statement is
// idempotent, so Open doubles as the migration path for v1.
const schemaSQL = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS sessions (
  id         TEXT PRIMARY KEY,
  title      TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  seq         INTEGER NOT NULL,
  role        TEXT NOT NULL,
  blocks      TEXT NOT NULL,
  model       TEXT NOT NULL DEFAULT '',
  call_site   TEXT NOT NULL DEFAULT '',
  tokens_in   INTEGER NOT NULL DEFAULT 0,
  tokens_out  INTEGER NOT NULL DEFAULT 0,
  cost_usd    REAL NOT NULL DEFAULT 0,
  created_at  TEXT NOT NULL,
  UNIQUE (session_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, seq);

CREATE TABLE IF NOT EXISTS summaries (
  session_id  TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
  text        TEXT NOT NULL,
  through_seq INTEGER NOT NULL,
  created_at  TEXT NOT NULL
);

-- Populated in Plan 2 (policy) and Plan 3 (scheduler); created now so the
-- schema has one definition point.
CREATE TABLE IF NOT EXISTS approvals (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  tool       TEXT NOT NULL,
  args       TEXT NOT NULL,
  decision   TEXT NOT NULL,
  scope      TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS jobs (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  schedule   TEXT NOT NULL,
  prompt     TEXT NOT NULL,
  session_id TEXT,
  enabled    INTEGER NOT NULL DEFAULT 1,
  last_run   TEXT,
  created_at TEXT NOT NULL
);
`
