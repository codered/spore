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
