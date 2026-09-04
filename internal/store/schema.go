package store

// schemaSQL is applied inside one transaction at Open. Every statement is
// idempotent, so Open doubles as the migration path for v1.
const schemaSQL = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS sessions (
  id         TEXT PRIMARY KEY,
  title      TEXT NOT NULL DEFAULT '',
  workspace  TEXT NOT NULL DEFAULT '',
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

-- approvals is the decision log: every answer a human gave, for audit and
-- for "always this session" lookups. scope is once | session | pattern.
CREATE TABLE IF NOT EXISTS approvals (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  tool       TEXT NOT NULL,
  args       TEXT NOT NULL,
  decision   TEXT NOT NULL,
  scope      TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_approvals_session ON approvals(session_id, tool, id);

-- pending_calls is suspension made durable: a turn waiting on an approval
-- writes a row here before it blocks, so the process can restart mid-turn
-- and still know what it was asking about. state is pending until answered.
CREATE TABLE IF NOT EXISTS pending_calls (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  tool_use_id TEXT NOT NULL,
  tool        TEXT NOT NULL,
  args        TEXT NOT NULL,
  profile     TEXT NOT NULL DEFAULT '',
  rule        TEXT NOT NULL DEFAULT '',
  state       TEXT NOT NULL DEFAULT 'pending',
  created_at  TEXT NOT NULL,
  decided_at  TEXT
);

CREATE INDEX IF NOT EXISTS idx_pending_session ON pending_calls(session_id, state);

-- jobs drives the scheduler. kind is cron | once; spec is a five-field cron
-- expression or an RFC3339 instant. next_run is the computed fire time and is
-- the only column the tick loop queries on. A job always opens a FRESH
-- session, so last_session_id is a record of what it produced, never a target.
CREATE TABLE IF NOT EXISTS jobs (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  kind            TEXT NOT NULL,
  spec            TEXT NOT NULL,
  prompt          TEXT NOT NULL,
  enabled         INTEGER NOT NULL DEFAULT 1,
  next_run        TEXT NOT NULL,
  last_run        TEXT,
  last_session_id TEXT NOT NULL DEFAULT '',
  created_at      TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_jobs_due ON jobs(enabled, next_run);

-- bridge_bindings maps a chat surface's own identifier — a Discord thread or
-- DM channel id — to a spore session, so a thread you replied in yesterday is
-- still that session after the daemon restarts. bridge namespaces the id,
-- because two bridges may hand out the same-looking string.
CREATE TABLE IF NOT EXISTS bridge_bindings (
  bridge      TEXT NOT NULL,
  external_id TEXT NOT NULL,
  session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  created_at  TEXT NOT NULL,
  PRIMARY KEY (bridge, external_id)
);

-- bridge_seen deduplicates inbound events. A gateway that resumes redelivers,
-- and running a turn twice for one message is worse than dropping one.
CREATE TABLE IF NOT EXISTS bridge_seen (
  bridge     TEXT NOT NULL,
  event_id   TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (bridge, event_id)
);

CREATE INDEX IF NOT EXISTS idx_bridge_seen_age ON bridge_seen(created_at);

-- recall_fts is the keyword index behind spore recall and the recall_search
-- tool. Content lives in this table, so 'rebuild' is a no-op here: repairing
-- the index means deleting and reinserting from the source tables.
--
-- ref_id is TEXT for every kind: a message id cast to text, a session id for a
-- summary, a fact name for a fact.
CREATE VIRTUAL TABLE IF NOT EXISTS recall_fts USING fts5(
  text,
  kind       UNINDEXED,
  ref_id     UNINDEXED,
  session_id UNINDEXED,
  created_at UNINDEXED
);

-- Deletion is the one sync path a trigger can own, because it needs no
-- knowledge of the block format. Insertion happens in Go, where the block
-- types are real types rather than JSON to be re-parsed in SQL.
CREATE TRIGGER IF NOT EXISTS recall_fts_messages_ad AFTER DELETE ON messages BEGIN
  DELETE FROM recall_fts WHERE kind = 'message' AND ref_id = CAST(old.id AS TEXT);
END;

CREATE TRIGGER IF NOT EXISTS recall_fts_summaries_ad AFTER DELETE ON summaries BEGIN
  DELETE FROM recall_fts WHERE kind = 'summary' AND ref_id = old.session_id;
END;

-- recall_sync is the watermark for a mirror backend. A vector store cannot
-- join the transaction that writes recall_fts -- an HTTP call inside an open
-- write transaction is how a database gets wedged -- so it is brought forward
-- afterwards from the last rowid it saw. One row per backend, because two
-- mirrors would otherwise fight over one cursor.
CREATE TABLE IF NOT EXISTS recall_sync (
  backend    TEXT PRIMARY KEY,
  cursor     INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL
);
`
