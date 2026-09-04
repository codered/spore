# Plan 5b — Weaviate recall backend

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give recall a semantic backend — Weaviate, provisioned by `spore recall setup` — that never degrades the keyword search already shipping.

**Architecture:** The SQLite FTS index stays the record of what is indexed, written inside `AppendMessage`'s transaction as it is today. Weaviate is a mirror, brought forward from a watermark over `recall_fts` by an indexer that runs after the commit. A `Fallback` wrapper searches Weaviate and falls back to `sqlitefts` on any transport failure, which is correct without reconciliation precisely because the keyword index is never behind.

**Tech Stack:** Go 1.26, `github.com/weaviate/weaviate-go-client/v5 v5.7.3`, SQLite (`mattn/go-sqlite3`, `-tags sqlite_fts5`), Docker Compose for provisioning.

**Spec:** `docs/superpowers/specs/2026-08-29-spore-design.md` — sections 5 (Memory / Recall), 9 (Configuration), 10 (Testing), 11 (staging, stage 5b).

## Global Constraints

- Go 1.26. Module `github.com/codered/spore`.
- Every test command in this plan runs through the repo's tags: `go test -tags sqlite_fts5 ./...`, or `make test`. `make fmtcheck vet test` must pass before any commit.
- The default suite must not need a network, a container, or Docker. Container-backed tests live behind `-tags weaviate` and are additionally guarded by a `docker` lookup so they skip rather than fail on a machine without it.
- Pinned images, exact tags, never `latest`: `cr.weaviate.io/semitechnologies/weaviate:1.38.5` and `cr.weaviate.io/semitechnologies/model2vec-inference:minishlab-potion-base-32M`.
- Both services bind `127.0.0.1` only. The daemon binds loopback and carries no auth (spec section 8); a sidecar must not be the thing that opens the machine up.
- Comments say *why*, not what. The codebase uses `--` for an em dash inside comments.
- `internal/store` must not import `internal/recall` — the store repeats the kind strings instead, pinned by a test. Do not "fix" this; it is what keeps the cycle broken.
- Never log or span-record query text when `trace.redact` is on. Reuse `internal/trace`'s existing retriever span helpers.

---

### Task 1: `[recall]` configuration

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `config.RecallConfig{Backend, URL string}`, reachable as `cfg.Recall`. Constants `config.RecallSQLiteFTS = "sqlitefts"` and `config.RecallWeaviate = "weaviate"`. Method `func (c *Config) WeaviateURL() string` returning `c.Recall.URL` or `"http://127.0.0.1:8080"` when it is empty.

- [ ] **Step 1: Write the failing tests**

Add to `internal/config/config_test.go`:

```go
func TestRecallDefaultsToSQLiteFTS(t *testing.T) {
	cfg := Default()
	if cfg.Recall.Backend != RecallSQLiteFTS {
		t.Errorf("backend %q, want %q", cfg.Recall.Backend, RecallSQLiteFTS)
	}
	if cfg.Recall.URL != "" {
		t.Errorf("url %q, want empty so setup owns the address", cfg.Recall.URL)
	}
}

func TestWeaviateURLFallsBackToLoopback(t *testing.T) {
	cfg := Default()
	if got := cfg.WeaviateURL(); got != "http://127.0.0.1:8080" {
		t.Errorf("WeaviateURL() = %q, want the provisioned loopback address", got)
	}
	cfg.Recall.URL = "http://box.local:8080"
	if got := cfg.WeaviateURL(); got != "http://box.local:8080" {
		t.Errorf("WeaviateURL() = %q, want the configured address", got)
	}
}

func TestUnknownRecallBackendIsRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[recall]\nbackend = \"pinecone\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("an unknown backend loaded without error")
	} else if !strings.Contains(err.Error(), "pinecone") {
		t.Errorf("error %q does not name the offending value", err)
	}
}

func TestRecallURLMustParse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[recall]\nurl = \"://nope\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("an unparseable recall.url loaded without error")
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test -tags sqlite_fts5 ./internal/config/ -run 'Recall|WeaviateURL'`
Expected: FAIL — `cfg.Recall` undefined, `RecallSQLiteFTS` undefined.

- [ ] **Step 3: Implement**

In `internal/config/config.go`, add the constants and struct near `TraceConfig`:

```go
// Recall backend names. sqlitefts ships in every build and needs nothing;
// weaviate is a mirror over the same corpus and can be unreachable, which is
// why choosing it never switches the keyword index off.
const (
	RecallSQLiteFTS = "sqlitefts"
	RecallWeaviate  = "weaviate"
)

// RecallConfig selects the search backend. An empty URL means the instance
// `spore recall setup` provisions on loopback; setting it points spore at one
// the operator runs and turns provisioning off.
type RecallConfig struct {
	Backend string `toml:"backend"`
	URL     string `toml:"url"`
}
```

Add the field to `Config` after `Trace`:

```go
	Recall    RecallConfig              `toml:"recall"`
```

In `Default()`, inside the returned literal:

```go
		Recall:    RecallConfig{Backend: RecallSQLiteFTS},
```

Add the accessor:

```go
// WeaviateURL is the address the backend dials. The default is the loopback
// address `spore recall setup` binds, so a machine that ran setup needs no
// configuration at all.
func (c *Config) WeaviateURL() string {
	if c.Recall.URL != "" {
		return c.Recall.URL
	}
	return "http://127.0.0.1:8080"
}
```

In `Load`, alongside the other defaulting, add:

```go
	if cfg.Recall.Backend == "" {
		cfg.Recall.Backend = d.Recall.Backend
	}
```

In the validation function (the one that already rejects a bad `policy.profile.*.default`), add:

```go
	switch c.Recall.Backend {
	case RecallSQLiteFTS, RecallWeaviate:
	default:
		return fmt.Errorf("recall.backend must be %s or %s, got %q",
			RecallSQLiteFTS, RecallWeaviate, c.Recall.Backend)
	}
	if c.Recall.URL != "" {
		if _, err := url.Parse(c.Recall.URL); err != nil {
			return fmt.Errorf("recall.url: %w", err)
		}
	}
```

- [ ] **Step 4: Run the tests**

Run: `make fmtcheck vet test`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/
git commit -m "feat(config): add the [recall] section"
```

---

### Task 2: The watermark and the rows to mirror

**Files:**
- Modify: `internal/store/schema.go`, `internal/store/recall.go`
- Test: `internal/store/recall_test.go`

**Interfaces:**
- Consumes: Task 1's config (not directly; the store takes the backend name as a string key).
- Produces:
  - `type IndexRow struct { RowID int64; Kind, RefID, SessionID, CreatedAt, Text string }`
  - `func (s *Store) IndexRowsSince(ctx context.Context, cursor int64, limit int) ([]IndexRow, error)`
  - `func (s *Store) SyncCursor(ctx context.Context, backend string) (int64, error)`
  - `func (s *Store) SetSyncCursor(ctx context.Context, backend string, cursor int64) error`

- [ ] **Step 1: Write the failing tests**

Add to `internal/store/recall_test.go`:

```go
func TestIndexRowsSinceWalksTheCorpusInOrder(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	id, err := st.CreateSession(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"first message", "second message", "third message"} {
		blocks := []byte(`[{"type":"text","text":"` + text + `"}]`)
		if _, err := st.AppendMessage(ctx, Message{SessionID: id, Role: "user", BlocksJSON: blocks}); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := st.IndexRowsSince(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].RowID <= rows[i-1].RowID {
			t.Errorf("rows are not ascending by rowid: %v", rows)
		}
	}
	if rows[0].Text != "first message" || rows[0].Kind != "message" {
		t.Errorf("first row = %+v, want the first message", rows[0])
	}

	// A cursor at the first row must exclude it and nothing else.
	rest, err := st.IndexRowsSince(ctx, rows[0].RowID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 2 || rest[0].Text != "second message" {
		t.Errorf("after the cursor: %+v, want the last two rows", rest)
	}
}

func TestIndexRowsSinceHonoursTheLimit(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	id, _ := st.CreateSession(ctx, "s")
	for i := 0; i < 5; i++ {
		blocks := []byte(`[{"type":"text","text":"m"}]`)
		if _, err := st.AppendMessage(ctx, Message{SessionID: id, Role: "user", BlocksJSON: blocks}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.IndexRowsSince(ctx, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Errorf("got %d rows, want the limit of 2", len(rows))
	}
}

func TestIndexRowsSinceCarriesFacts(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.IndexFact(ctx, "coffee", "the user takes it black"); err != nil {
		t.Fatal(err)
	}
	rows, err := st.IndexRowsSince(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Kind != "fact" || rows[0].RefID != "coffee" {
		t.Fatalf("rows = %+v, want one fact row named coffee", rows)
	}
}

func TestSyncCursorRoundTripsPerBackend(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	got, err := st.SyncCursor(ctx, "weaviate")
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("a backend that never synced has cursor %d, want 0", got)
	}
	if err := st.SetSyncCursor(ctx, "weaviate", 42); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.SyncCursor(ctx, "weaviate"); got != 42 {
		t.Errorf("cursor = %d, want 42", got)
	}
	if err := st.SetSyncCursor(ctx, "weaviate", 99); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.SyncCursor(ctx, "weaviate"); got != 99 {
		t.Errorf("cursor = %d after a second write, want 99", got)
	}
	if got, _ := st.SyncCursor(ctx, "other"); got != 0 {
		t.Errorf("a different backend saw %d, want its own cursor of 0", got)
	}
}
```

If `openTestStore` does not already exist in the package's tests, use whatever helper the existing tests use to open an in-memory store; do not add a second one.

- [ ] **Step 2: Run them and watch them fail**

Run: `go test -tags sqlite_fts5 ./internal/store/ -run 'IndexRowsSince|SyncCursor'`
Expected: FAIL — `IndexRowsSince` and `SyncCursor` undefined.

- [ ] **Step 3: Implement**

In `internal/store/schema.go`, append to `schemaSQL`:

```sql
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
```

In `internal/store/recall.go`:

```go
// IndexRow is one row of the keyword index, as a mirror backend sees it.
// RowID is recall_fts's own rowid and is the only ordering a mirror may rely
// on: it rises monotonically, and re-indexing a fact deletes its row and
// inserts a new one at the end, so an update looks like an append.
type IndexRow struct {
	RowID     int64
	Kind      string
	RefID     string
	SessionID string
	CreatedAt string
	Text      string
}

// IndexRowsSince returns up to limit rows written after cursor, oldest first.
// It is the mirror's whole view of the corpus: whatever reached the keyword
// index reaches the mirror, so the curation rule -- text blocks only, never a
// tool_result -- is enforced in exactly one place.
func (s *Store) IndexRowsSince(ctx context.Context, cursor int64, limit int) ([]IndexRow, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT rowid, kind, ref_id, session_id, created_at, text
		   FROM recall_fts WHERE rowid > ? ORDER BY rowid LIMIT ?`, cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("read index rows: %w", err)
	}
	defer rows.Close()
	var out []IndexRow
	for rows.Next() {
		var r IndexRow
		if err := rows.Scan(&r.RowID, &r.Kind, &r.RefID, &r.SessionID, &r.CreatedAt, &r.Text); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SyncCursor reports how far a mirror has caught up. A backend that never ran
// reports 0, which means "start at the beginning" and makes a first sync and a
// backfill the same operation.
func (s *Store) SyncCursor(ctx context.Context, backend string) (int64, error) {
	var cursor int64
	err := s.db.QueryRowContext(ctx, `SELECT cursor FROM recall_sync WHERE backend = ?`, backend).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read sync cursor: %w", err)
	}
	return cursor, nil
}

// SetSyncCursor records progress. It is written only after the mirror has
// accepted the batch, so a crash re-sends rows rather than skipping them --
// object ids are derived from kind and ref_id, which makes a re-send an
// overwrite.
func (s *Store) SetSyncCursor(ctx context.Context, backend string, cursor int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO recall_sync (backend, cursor, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(backend) DO UPDATE SET cursor = excluded.cursor, updated_at = excluded.updated_at`,
		backend, cursor, nowString())
	if err != nil {
		return fmt.Errorf("write sync cursor: %w", err)
	}
	return nil
}
```

Add `"database/sql"` and `"errors"` to the imports if they are not already there.

- [ ] **Step 4: Run the tests**

Run: `make fmtcheck vet test`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/
git commit -m "feat(store): add the mirror watermark and its row feed"
```

---

### Task 3: Weaviate mapping — everything a network cannot decide

**Files:**
- Create: `internal/recall/weaviate/schema.go`, `internal/recall/weaviate/mapping.go`
- Test: `internal/recall/weaviate/mapping_test.go`

**Interfaces:**
- Consumes: `recall.Chunk`, `recall.Query`, `recall.Hit`, `recall.ClampK` from `internal/recall`.
- Produces:
  - `const Collection = "SporeChunk"`
  - `func collectionClass() *models.Class`
  - `func objectID(kind, refID string) strfmt.UUID`
  - `func chunkObject(c recall.Chunk) *models.Object`
  - `func whereFilter(q recall.Query) *filters.WhereBuilder` (nil when the query filters nothing)
  - `func decodeHits(resp *models.GraphQLResponse) ([]recall.Hit, error)`
  - `func excerpt(text string) string`

- [ ] **Step 1: Write the failing tests**

Create `internal/recall/weaviate/mapping_test.go`:

```go
package weaviate

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/weaviate/weaviate/entities/models"

	"github.com/codered/spore/internal/recall"
)

func TestObjectIDIsStableAndDistinct(t *testing.T) {
	a := objectID(recall.KindMessage, "17")
	b := objectID(recall.KindMessage, "17")
	if a != b {
		t.Errorf("the same chunk produced two ids: %s and %s", a, b)
	}
	// A stable id is what makes a re-sent batch an overwrite rather than a
	// duplicate, so this property carries the whole sync design.
	if objectID(recall.KindFact, "17") == a {
		t.Error("a fact and a message with the same ref share an id")
	}
	if objectID(recall.KindMessage, "18") == a {
		t.Error("two messages share an id")
	}
	if a == "" {
		t.Error("empty id")
	}
}

func TestChunkObjectCarriesEveryFilterableField(t *testing.T) {
	when := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	obj := chunkObject(recall.Chunk{
		ID: "42", Kind: recall.KindMessage, Text: "hello", SessionID: "sess-1", CreatedAt: when,
	})
	if obj.Class != Collection {
		t.Errorf("class %q, want %q", obj.Class, Collection)
	}
	props, ok := obj.Properties.(map[string]any)
	if !ok {
		t.Fatalf("properties are %T, want a map", obj.Properties)
	}
	for field, want := range map[string]any{
		"text": "hello", "kind": recall.KindMessage, "ref_id": "42", "session_id": "sess-1",
	} {
		if props[field] != want {
			t.Errorf("property %s = %v, want %v", field, props[field], want)
		}
	}
	if props["created_at"] != when.Format(time.RFC3339) {
		t.Errorf("created_at = %v, want an RFC3339 string", props["created_at"])
	}
}

func TestCollectionVectorizesTextAndNothingElse(t *testing.T) {
	class := collectionClass()
	if class.Vectorizer != vectorizer {
		t.Errorf("vectorizer %q, want %q", class.Vectorizer, vectorizer)
	}
	// Metadata in the vector is noise: a session id has no meaning in
	// embedding space and would drag every hit towards whichever session
	// happened to be long.
	for _, p := range class.Properties {
		cfg, _ := p.ModuleConfig.(map[string]any)
		mod, _ := cfg[vectorizer].(map[string]any)
		skip, _ := mod["skip"].(bool)
		if p.Name == "text" && skip {
			t.Error("the text property is skipped, so nothing is vectorized")
		}
		if p.Name != "text" && !skip {
			t.Errorf("property %q is vectorized, want it skipped", p.Name)
		}
	}
}

func TestWhereFilterMatchesTheQueryScope(t *testing.T) {
	if whereFilter(recall.Query{Text: "x"}) != nil {
		t.Error("an unscoped query built a filter")
	}
	// The remote profile narrows to one session and drops facts; that scoping
	// is the tool's, and this is the half that must survive the translation.
	f := whereFilter(recall.Query{Text: "x", SessionID: "sess-1", Kinds: []string{recall.KindMessage}})
	if f == nil {
		t.Fatal("a scoped query built no filter")
	}
	built := f.String()
	if !strings.Contains(built, "sess-1") {
		t.Errorf("filter does not narrow by session: %s", built)
	}
	if !strings.Contains(built, recall.KindMessage) {
		t.Errorf("filter does not narrow by kind: %s", built)
	}
}

func TestDecodeHitsReadsAWeaviateResponse(t *testing.T) {
	raw := `{"Get":{"SporeChunk":[
	  {"text":"the retry logic lives in provider","kind":"message","ref_id":"17",
	   "session_id":"sess-1","created_at":"2026-09-03T10:00:00Z",
	   "_additional":{"certainty":0.87}},
	  {"text":"black coffee","kind":"fact","ref_id":"coffee",
	   "session_id":"","created_at":"2026-09-02T09:00:00Z",
	   "_additional":{"certainty":0.61}}]}}`
	var data map[string]models.JSONObject
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatal(err)
	}
	hits, err := decodeHits(&models.GraphQLResponse{Data: data})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
	if hits[0].ID != "17" || hits[0].Kind != recall.KindMessage || hits[0].SessionID != "sess-1" {
		t.Errorf("first hit = %+v", hits[0].Chunk)
	}
	if hits[0].Score <= hits[1].Score {
		t.Errorf("scores %v and %v are not in the order the server returned", hits[0].Score, hits[1].Score)
	}
	if !strings.Contains(hits[0].Excerpt, "retry logic") {
		t.Errorf("excerpt %q does not carry the text", hits[0].Excerpt)
	}
	if hits[0].CreatedAt.IsZero() {
		t.Error("created_at did not parse")
	}
}

func TestDecodeHitsSurvivesAnErrorResponse(t *testing.T) {
	resp := &models.GraphQLResponse{Errors: []*models.GraphQLError{{Message: "collection not found"}}}
	if _, err := decodeHits(resp); err == nil {
		t.Fatal("a GraphQL error decoded as success")
	} else if !strings.Contains(err.Error(), "collection not found") {
		t.Errorf("error %q drops the server's message", err)
	}
}

func TestExcerptIsBoundedAndWholeWhenShort(t *testing.T) {
	if got := excerpt("short text"); got != "short text" {
		t.Errorf("excerpt(%q) = %q, want it whole", "short text", got)
	}
	long := strings.Repeat("word ", 200)
	got := excerpt(long)
	if len(got) > excerptBytes+1 {
		t.Errorf("excerpt is %d bytes, want at most %d", len(got), excerptBytes+1)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a clipped excerpt should say so: %q", got[len(got)-10:])
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test -tags sqlite_fts5 ./internal/recall/weaviate/`
Expected: FAIL — the package does not exist.

- [ ] **Step 3: Implement the collection**

Create `internal/recall/weaviate/schema.go`:

```go
// Package weaviate is recall's semantic backend. It mirrors the corpus the
// keyword index already holds; it is never the only copy, which is what lets
// a caller fall back to sqlitefts without reconciling anything afterwards.
package weaviate

import "github.com/weaviate/weaviate/entities/models"

// Collection is the one class spore owns. Everything indexed is a chunk, and
// the kind property tells them apart, because a filter over one class is
// cheaper than a query that has to know which of three classes to ask.
const Collection = "SporeChunk"

// vectorizer names the module the compose file starts. It runs as a second
// container: no Weaviate vectorizer runs in-process, and the module-free path
// would mean spore holding an embedding API key.
const vectorizer = "text2vec-model2vec"

// skipVectorizing is the per-property module config for everything that is
// metadata. A session id has no meaning in embedding space, and including it
// would pull hits towards whichever session had the most text.
func skipVectorizing() map[string]any {
	return map[string]any{vectorizer: map[string]any{"skip": true, "vectorizePropertyName": false}}
}

func vectorizeProperty() map[string]any {
	return map[string]any{vectorizer: map[string]any{"skip": false, "vectorizePropertyName": false}}
}

// collectionClass is the schema spore creates on setup. It is written here
// rather than in a JSON fixture so a property added to Chunk fails to compile
// until it is given a vectorization decision.
func collectionClass() *models.Class {
	return &models.Class{
		Class:       Collection,
		Description: "One indexed chunk of spore's corpus: a message, a summary, or a fact.",
		Vectorizer:  vectorizer,
		ModuleConfig: map[string]any{
			vectorizer: map[string]any{"vectorizeClassName": false},
		},
		Properties: []*models.Property{
			{Name: "text", DataType: []string{"text"}, ModuleConfig: vectorizeProperty()},
			{Name: "kind", DataType: []string{"text"}, ModuleConfig: skipVectorizing()},
			{Name: "ref_id", DataType: []string{"text"}, ModuleConfig: skipVectorizing()},
			{Name: "session_id", DataType: []string{"text"}, ModuleConfig: skipVectorizing()},
			{Name: "created_at", DataType: []string{"text"}, ModuleConfig: skipVectorizing()},
		},
	}
}
```

- [ ] **Step 4: Implement the mapping**

Create `internal/recall/weaviate/mapping.go`:

```go
package weaviate

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-openapi/strfmt"
	"github.com/google/uuid"
	"github.com/weaviate/weaviate-go-client/v5/weaviate/filters"
	"github.com/weaviate/weaviate/entities/models"

	"github.com/codered/spore/internal/recall"
)

// excerptBytes bounds what a hit carries back into a prompt. Recall exists to
// point at content, not to inline it; the caller reads the source when it
// needs more.
const excerptBytes = 280

// idNamespace makes object ids deterministic. A re-sent batch must overwrite
// rather than duplicate, because the mirror re-sends whenever it crashed
// between accepting a batch and writing its cursor.
var idNamespace = uuid.MustParse("6f1d0f3c-9a1e-5c7b-8a2f-1f4b6d0c7e21")

func objectID(kind, refID string) strfmt.UUID {
	return strfmt.UUID(uuid.NewSHA1(idNamespace, []byte(kind+"\x00"+refID)).String())
}

func chunkObject(c recall.Chunk) *models.Object {
	return &models.Object{
		Class: Collection,
		ID:    objectID(c.Kind, c.ID),
		Properties: map[string]any{
			"text":       c.Text,
			"kind":       c.Kind,
			"ref_id":     c.ID,
			"session_id": c.SessionID,
			"created_at": c.CreatedAt.Format(time.RFC3339),
		},
	}
}

// whereFilter translates a Query's scope. It returns nil for an unscoped
// query so the caller can leave the filter off the request entirely rather
// than sending a filter that matches everything.
func whereFilter(q recall.Query) *filters.WhereBuilder {
	var operands []*filters.WhereBuilder
	if q.SessionID != "" {
		operands = append(operands, filters.Where().
			WithPath([]string{"session_id"}).WithOperator(filters.Equal).WithValueText(q.SessionID))
	}
	if len(q.Kinds) > 0 {
		var kinds []*filters.WhereBuilder
		for _, k := range q.Kinds {
			kinds = append(kinds, filters.Where().
				WithPath([]string{"kind"}).WithOperator(filters.Equal).WithValueText(k))
		}
		if len(kinds) == 1 {
			operands = append(operands, kinds[0])
		} else {
			operands = append(operands, filters.Where().WithOperator(filters.Or).WithOperands(kinds))
		}
	}
	switch len(operands) {
	case 0:
		return nil
	case 1:
		return operands[0]
	default:
		return filters.Where().WithOperator(filters.And).WithOperands(operands)
	}
}

// hitFields is the projection every search asks for. certainty is Weaviate's
// normalised similarity, which is the only score whose ordering spore
// promises -- the Recall contract makes the value backend-defined.
type rawHit struct {
	Text       string `json:"text"`
	Kind       string `json:"kind"`
	RefID      string `json:"ref_id"`
	SessionID  string `json:"session_id"`
	CreatedAt  string `json:"created_at"`
	Additional struct {
		Certainty float64 `json:"certainty"`
	} `json:"_additional"`
}

// decodeHits reads the GraphQL envelope. A GraphQL response carries errors in
// the body with a 200 status, so a decoder that only checks transport has
// silently returned "no results" for a broken query.
func decodeHits(resp *models.GraphQLResponse) ([]recall.Hit, error) {
	if resp == nil {
		return nil, fmt.Errorf("empty response")
	}
	if len(resp.Errors) > 0 {
		msgs := make([]string, 0, len(resp.Errors))
		for _, e := range resp.Errors {
			if e != nil {
				msgs = append(msgs, e.Message)
			}
		}
		return nil, fmt.Errorf("weaviate: %s", joinNonEmpty(msgs))
	}
	get, ok := resp.Data["Get"]
	if !ok {
		return nil, nil
	}
	blob, err := json.Marshal(get)
	if err != nil {
		return nil, err
	}
	var wrapper map[string][]rawHit
	if err := json.Unmarshal(blob, &wrapper); err != nil {
		return nil, fmt.Errorf("decode hits: %w", err)
	}
	var out []recall.Hit
	for _, r := range wrapper[Collection] {
		when, _ := time.Parse(time.RFC3339, r.CreatedAt)
		out = append(out, recall.Hit{
			Chunk: recall.Chunk{
				ID: r.RefID, Kind: r.Kind, Text: r.Text,
				SessionID: r.SessionID, CreatedAt: when,
			},
			Score:   r.Additional.Certainty,
			Excerpt: excerpt(r.Text),
		})
	}
	return out, nil
}

func joinNonEmpty(msgs []string) string {
	out := ""
	for _, m := range msgs {
		if m == "" {
			continue
		}
		if out != "" {
			out += "; "
		}
		out += m
	}
	if out == "" {
		return "unspecified error"
	}
	return out
}

// excerpt clips on a rune boundary so a multi-byte character is never cut in
// half on its way into a prompt.
func excerpt(text string) string {
	if len(text) <= excerptBytes {
		return text
	}
	runes := []rune(text)
	out := make([]rune, 0, len(runes))
	n := 0
	for _, r := range runes {
		if n+len(string(r)) > excerptBytes {
			break
		}
		out = append(out, r)
		n += len(string(r))
	}
	return string(out) + "…"
}
```

- [ ] **Step 5: Run the tests**

Run: `go test -tags sqlite_fts5 ./internal/recall/weaviate/ -v`
Expected: PASS, every test.

- [ ] **Step 6: Prove the id test has teeth**

Temporarily change `objectID` to ignore `kind` (use only `refID`). Run the tests: `TestObjectIDIsStableAndDistinct` must fail on the fact/message collision. Restore it.

- [ ] **Step 7: Commit**

```bash
git add internal/recall/weaviate/ go.mod go.sum
git commit -m "feat(recall): map spore chunks onto a weaviate collection"
```

---

### Task 4: The backend itself

**Files:**
- Create: `internal/recall/weaviate/weaviate.go`
- Test: `internal/recall/weaviate/weaviate_test.go`

**Interfaces:**
- Consumes: Task 3's mapping helpers.
- Produces:
  - `func New(baseURL string) (*Backend, error)`
  - `func (b *Backend) Index(ctx context.Context, chunks []recall.Chunk) error`
  - `func (b *Backend) Search(ctx context.Context, q recall.Query) ([]recall.Hit, error)`
  - `func (b *Backend) Status(ctx context.Context) (recall.Status, error)`
  - `func (b *Backend) EnsureCollection(ctx context.Context) error`
  - `func (b *Backend) Ready(ctx context.Context) error`
  - `func (b *Backend) DropAll(ctx context.Context) error`
  - `const Name = "weaviate"`

- [ ] **Step 1: Write the failing tests**

Create `internal/recall/weaviate/weaviate_test.go`:

```go
package weaviate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/recall"
)

// unreachable is an address nothing listens on. It exercises the paths that
// matter without a container: every method must fail fast and say which
// backend failed, because that message is what a degraded status reports.
const unreachable = "http://127.0.0.1:1"

func newUnreachable(t *testing.T) *Backend {
	t.Helper()
	b, err := New(unreachable)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

func TestNewRejectsAnUnusableURL(t *testing.T) {
	if _, err := New("::not a url"); err == nil {
		t.Fatal("an unparseable url was accepted")
	}
	if _, err := New(""); err == nil {
		t.Fatal("an empty url was accepted")
	}
}

func TestNewSplitsSchemeAndHost(t *testing.T) {
	b, err := New("http://box.local:8080")
	if err != nil {
		t.Fatal(err)
	}
	if b.host != "box.local:8080" || b.scheme != "http" {
		t.Errorf("host %q scheme %q, want box.local:8080 and http", b.host, b.scheme)
	}
}

func TestSearchFailsLoudlyWhenUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := newUnreachable(t).Search(ctx, recall.Query{Text: "anything"})
	if err == nil {
		t.Fatal("search against a dead address returned no error")
	}
	// The fallback wrapper decides on this error, and an operator reads it in
	// `recall status`, so it must name the backend.
	if !strings.Contains(err.Error(), Name) {
		t.Errorf("error %q does not name the backend", err)
	}
}

func TestEmptyQueryTextSearchesNothing(t *testing.T) {
	// Tokenising already treats a query with no word characters as "no hits";
	// sending it would be a round trip whose answer is known.
	hits, err := newUnreachable(t).Search(context.Background(), recall.Query{Text: "   "})
	if err != nil {
		t.Fatalf("an empty query reached the network: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("got %d hits for an empty query", len(hits))
	}
}

func TestIndexOfNothingIsNotARequest(t *testing.T) {
	if err := newUnreachable(t).Index(context.Background(), nil); err != nil {
		t.Errorf("indexing an empty batch reached the network: %v", err)
	}
}

func TestStatusReportsDegradedWhenUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := newUnreachable(t).Status(ctx)
	if err != nil {
		t.Fatalf("Status returned an error rather than a degraded status: %v", err)
	}
	if st.Backend != Name {
		t.Errorf("backend %q, want %q", st.Backend, Name)
	}
	if !st.Degraded {
		t.Error("an unreachable backend reported healthy")
	}
	if st.Reason == "" {
		t.Error("degraded with no reason")
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test -tags sqlite_fts5 ./internal/recall/weaviate/ -run 'New|Search|Index|Status'`
Expected: FAIL — `New` and `Backend` undefined.

- [ ] **Step 3: Implement**

Create `internal/recall/weaviate/weaviate.go`:

```go
package weaviate

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	client "github.com/weaviate/weaviate-go-client/v5/weaviate"
	"github.com/weaviate/weaviate-go-client/v5/weaviate/graphql"

	"github.com/codered/spore/internal/recall"
)

// Name is the backend's name in config, in `recall status`, and in the
// watermark row. One constant so the three cannot drift.
const Name = "weaviate"

// batchLimit bounds one insert request. Weaviate accepts far larger batches;
// the limit is here so a backfill of a long history streams rather than
// building one request the size of the archive.
const batchLimit = 100

// Backend implements recall.Recall against a Weaviate instance. It holds no
// state beyond the client: the watermark lives in SQLite, so restarting spore
// resumes rather than re-sending.
type Backend struct {
	c      *client.Client
	host   string
	scheme string
}

// New dials nothing. Construction must not fail because a container is down,
// or spore could not start on a machine whose sidecar has not come up yet.
func New(baseURL string) (*Backend, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("%s: no url configured", Name)
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("%s: parse url: %w", Name, err)
	}
	if u.Host == "" || u.Scheme == "" {
		return nil, fmt.Errorf("%s: url %q needs a scheme and a host", Name, baseURL)
	}
	c, err := client.NewClient(client.Config{Host: u.Host, Scheme: u.Scheme})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", Name, err)
	}
	return &Backend{c: c, host: u.Host, scheme: u.Scheme}, nil
}

// Ready reports whether the instance is up and serving. Setup waits on it;
// Status asks it to decide whether search has degraded.
func (b *Backend) Ready(ctx context.Context) error {
	ok, err := b.c.Misc().ReadyChecker().Do(ctx)
	if err != nil {
		return fmt.Errorf("%s at %s: %w", Name, b.host, err)
	}
	if !ok {
		return fmt.Errorf("%s at %s: not ready", Name, b.host)
	}
	return nil
}

// EnsureCollection creates the class when it is missing and leaves an
// existing one alone. It is safe to call on every start, which is what makes
// a half-finished setup recoverable by running setup again.
func (b *Backend) EnsureCollection(ctx context.Context) error {
	exists, err := b.c.Schema().ClassExistenceChecker().WithClassName(Collection).Do(ctx)
	if err != nil {
		return fmt.Errorf("%s: check collection: %w", Name, err)
	}
	if exists {
		return nil
	}
	if err := b.c.Schema().ClassCreator().WithClass(collectionClass()).Do(ctx); err != nil {
		return fmt.Errorf("%s: create collection: %w", Name, err)
	}
	return nil
}

// DropAll removes the collection. `recall reindex` uses it: a rebuild renumbers
// every FTS rowid, so the mirror's watermark is meaningless afterwards and the
// only honest move is to start the mirror over.
func (b *Backend) DropAll(ctx context.Context) error {
	exists, err := b.c.Schema().ClassExistenceChecker().WithClassName(Collection).Do(ctx)
	if err != nil {
		return fmt.Errorf("%s: check collection: %w", Name, err)
	}
	if !exists {
		return nil
	}
	if err := b.c.Schema().ClassDeleter().WithClassName(Collection).Do(ctx); err != nil {
		return fmt.Errorf("%s: delete collection: %w", Name, err)
	}
	return nil
}

func (b *Backend) Index(ctx context.Context, chunks []recall.Chunk) error {
	for start := 0; start < len(chunks); start += batchLimit {
		end := min(start+batchLimit, len(chunks))
		batcher := b.c.Batch().ObjectsBatcher()
		for _, c := range chunks[start:end] {
			batcher = batcher.WithObject(chunkObject(c))
		}
		res, err := batcher.Do(ctx)
		if err != nil {
			return fmt.Errorf("%s: index batch: %w", Name, err)
		}
		// A batch returns 200 with per-object errors inside it, so a caller
		// that checks only err has silently dropped objects.
		for _, r := range res {
			if r.Result != nil && r.Result.Errors != nil && len(r.Result.Errors.Error) > 0 {
				return fmt.Errorf("%s: index object: %s", Name, r.Result.Errors.Error[0].Message)
			}
		}
	}
	return nil
}

func (b *Backend) Search(ctx context.Context, q recall.Query) ([]recall.Hit, error) {
	if strings.TrimSpace(q.Text) == "" {
		return nil, nil
	}
	fields := []graphql.Field{
		{Name: "text"}, {Name: "kind"}, {Name: "ref_id"},
		{Name: "session_id"}, {Name: "created_at"},
		{Name: "_additional", Fields: []graphql.Field{{Name: "certainty"}}},
	}
	near := (&graphql.NearTextArgumentBuilder{}).WithConcepts([]string{q.Text})
	get := b.c.GraphQL().Get().
		WithClassName(Collection).
		WithFields(fields...).
		WithNearText(near).
		WithLimit(recall.ClampK(q.K))
	if where := whereFilter(q); where != nil {
		get = get.WithWhere(where)
	}
	resp, err := get.Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: search: %w", Name, err)
	}
	hits, err := decodeHits(resp)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", Name, err)
	}
	return hits, nil
}

// Status never returns an error for an unreachable server. Being down is the
// condition it exists to report, and returning an error would make every
// caller re-implement the same "is this the degraded case" check.
func (b *Backend) Status(ctx context.Context) (recall.Status, error) {
	st := recall.Status{Backend: Name, Counts: map[string]int{}}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := b.Ready(ctx); err != nil {
		st.Degraded = true
		st.Reason = err.Error()
		return st, nil
	}
	for _, kind := range []string{recall.KindMessage, recall.KindSummary, recall.KindFact} {
		n, err := b.countKind(ctx, kind)
		if err != nil {
			st.Degraded = true
			st.Reason = err.Error()
			return st, nil
		}
		st.Counts[kind] = n
	}
	return st, nil
}

func (b *Backend) countKind(ctx context.Context, kind string) (int, error) {
	resp, err := b.c.GraphQL().Aggregate().
		WithClassName(Collection).
		WithWhere(filterKind(kind)).
		WithFields(graphql.Field{Name: "meta", Fields: []graphql.Field{{Name: "count"}}}).
		Do(ctx)
	if err != nil {
		return 0, fmt.Errorf("%s: count %s: %w", Name, kind, err)
	}
	return decodeCount(resp)
}
```

Add to `mapping.go` the two helpers the count path needs:

```go
// filterKind is the one-property filter Aggregate uses. It is separate from
// whereFilter because a count is never scoped by session.
func filterKind(kind string) *filters.WhereBuilder {
	return filters.Where().WithPath([]string{"kind"}).WithOperator(filters.Equal).WithValueText(kind)
}

// decodeCount reads meta.count out of an Aggregate response. A missing count
// is zero rather than an error: an empty collection legitimately returns no
// aggregate group.
func decodeCount(resp *models.GraphQLResponse) (int, error) {
	if resp == nil {
		return 0, fmt.Errorf("empty response")
	}
	if len(resp.Errors) > 0 {
		msgs := make([]string, 0, len(resp.Errors))
		for _, e := range resp.Errors {
			if e != nil {
				msgs = append(msgs, e.Message)
			}
		}
		return 0, fmt.Errorf("weaviate: %s", joinNonEmpty(msgs))
	}
	agg, ok := resp.Data["Aggregate"]
	if !ok {
		return 0, nil
	}
	blob, err := json.Marshal(agg)
	if err != nil {
		return 0, err
	}
	var wrapper map[string][]struct {
		Meta struct {
			Count int `json:"count"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(blob, &wrapper); err != nil {
		return 0, fmt.Errorf("decode count: %w", err)
	}
	groups := wrapper[Collection]
	if len(groups) == 0 {
		return 0, nil
	}
	return groups[0].Meta.Count, nil
}
```

Add a matching test to `mapping_test.go`:

```go
func TestDecodeCountReadsAnAggregate(t *testing.T) {
	raw := `{"Aggregate":{"SporeChunk":[{"meta":{"count":7}}]}}`
	var data map[string]models.JSONObject
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatal(err)
	}
	n, err := decodeCount(&models.GraphQLResponse{Data: data})
	if err != nil {
		t.Fatal(err)
	}
	if n != 7 {
		t.Errorf("count = %d, want 7", n)
	}
	empty, err := decodeCount(&models.GraphQLResponse{Data: map[string]models.JSONObject{}})
	if err != nil || empty != 0 {
		t.Errorf("an empty aggregate gave (%d, %v), want (0, nil)", empty, err)
	}
}
```

- [ ] **Step 4: Run the tests**

Run: `make fmtcheck vet test`
Expected: PASS. `TestSearchFailsLoudlyWhenUnreachable` and `TestStatusReportsDegradedWhenUnreachable` must complete in under a second or two — if either hangs, the dial timeout needs bounding in `New`.

- [ ] **Step 5: Commit**

```bash
git add internal/recall/weaviate/
git commit -m "feat(recall): implement the weaviate backend"
```

---

### Task 5: The fallback wrapper

**Files:**
- Create: `internal/recall/fallback.go`
- Test: `internal/recall/fallback_test.go`

**Interfaces:**
- Consumes: `recall.Recall`.
- Produces: `func NewFallback(primary, secondary Recall, log *slog.Logger) *Fallback`, implementing `Recall`. Field-free public surface beyond the constructor.

- [ ] **Step 1: Write the failing tests**

Create `internal/recall/fallback_test.go`:

```go
package recall

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

// fake is a Recall whose every method can be told to fail. It is the whole
// test double this package needs: the fallback's job is deciding between two
// of these.
type fake struct {
	name    string
	err     error
	hits    []Hit
	indexed []Chunk
	status  Status
}

func (f *fake) Index(_ context.Context, c []Chunk) error {
	if f.err != nil {
		return f.err
	}
	f.indexed = append(f.indexed, c...)
	return nil
}

func (f *fake) Search(context.Context, Query) ([]Hit, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.hits, nil
}

func (f *fake) Status(context.Context) (Status, error) {
	if f.err != nil {
		return Status{}, f.err
	}
	return f.status, nil
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestFallbackPrefersThePrimary(t *testing.T) {
	primary := &fake{name: "weaviate", hits: []Hit{{Chunk: Chunk{ID: "1"}}}}
	secondary := &fake{name: "sqlitefts", hits: []Hit{{Chunk: Chunk{ID: "2"}}}}
	got, err := NewFallback(primary, secondary, quietLogger()).Search(context.Background(), Query{Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "1" {
		t.Errorf("hits = %+v, want the primary's", got)
	}
}

func TestFallbackServesKeywordHitsWhenThePrimaryFails(t *testing.T) {
	primary := &fake{err: errors.New("connection refused")}
	secondary := &fake{hits: []Hit{{Chunk: Chunk{ID: "2"}}}}
	f := NewFallback(primary, secondary, quietLogger())
	got, err := f.Search(context.Background(), Query{Text: "x"})
	if err != nil {
		t.Fatalf("a failing primary broke the search: %v", err)
	}
	if len(got) != 1 || got[0].ID != "2" {
		t.Errorf("hits = %+v, want the secondary's", got)
	}
}

func TestFallbackReportsDegradedAfterItFellBack(t *testing.T) {
	primary := &fake{err: errors.New("connection refused")}
	secondary := &fake{status: Status{Backend: "sqlitefts", Counts: map[string]int{"message": 3}}}
	f := NewFallback(primary, secondary, quietLogger())
	if _, err := f.Search(context.Background(), Query{Text: "x"}); err != nil {
		t.Fatal(err)
	}
	st, err := f.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Degraded {
		t.Error("status is healthy after a fallback")
	}
	if st.Reason == "" {
		t.Error("degraded with no reason")
	}
	// An operator asking `recall status` while degraded wants to know what is
	// actually searchable, which is the keyword index.
	if st.Counts["message"] != 3 {
		t.Errorf("counts = %v, want the working backend's", st.Counts)
	}
}

func TestFallbackSurfacesAPrimaryStatusWhenHealthy(t *testing.T) {
	primary := &fake{status: Status{Backend: "weaviate", Counts: map[string]int{"message": 9}}}
	secondary := &fake{status: Status{Backend: "sqlitefts"}}
	st, err := NewFallback(primary, secondary, quietLogger()).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Backend != "weaviate" || st.Counts["message"] != 9 || st.Degraded {
		t.Errorf("status = %+v, want the primary's, healthy", st)
	}
}

func TestFallbackIndexesOnlyThePrimary(t *testing.T) {
	// The secondary is written inside AppendMessage's transaction and is never
	// behind. Writing it here again would double every row.
	primary := &fake{}
	secondary := &fake{}
	f := NewFallback(primary, secondary, quietLogger())
	if err := f.Index(context.Background(), []Chunk{{ID: "1", Kind: KindMessage}}); err != nil {
		t.Fatal(err)
	}
	if len(primary.indexed) != 1 {
		t.Errorf("primary got %d chunks, want 1", len(primary.indexed))
	}
	if len(secondary.indexed) != 0 {
		t.Errorf("secondary got %d chunks, want none", len(secondary.indexed))
	}
}

func TestFallbackRecoversWhenThePrimaryComesBack(t *testing.T) {
	primary := &fake{err: errors.New("down")}
	f := NewFallback(primary, &fake{}, quietLogger())
	if _, err := f.Search(context.Background(), Query{Text: "x"}); err != nil {
		t.Fatal(err)
	}
	primary.err = nil
	primary.hits = []Hit{{Chunk: Chunk{ID: "1"}}}
	got, err := f.Search(context.Background(), Query{Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "1" {
		t.Errorf("hits = %+v, want the recovered primary's", got)
	}
	st, _ := f.Status(context.Background())
	if st.Degraded {
		t.Error("still degraded after the primary recovered")
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test -tags sqlite_fts5 ./internal/recall/ -run Fallback`
Expected: FAIL — `NewFallback` undefined.

- [ ] **Step 3: Implement**

Create `internal/recall/fallback.go`:

```go
package recall

import (
	"context"
	"log/slog"
	"sync"
)

// Fallback searches a primary backend and drops to a secondary when the
// primary fails. It is correct without any reconciliation afterwards because
// the secondary is the keyword index, which is written inside the same
// transaction as the message it indexes and is therefore never behind.
type Fallback struct {
	primary   Recall
	secondary Recall
	log       *slog.Logger

	mu     sync.Mutex
	reason string // non-empty while the last primary call failed
}

func NewFallback(primary, secondary Recall, log *slog.Logger) *Fallback {
	if log == nil {
		log = slog.Default()
	}
	return &Fallback{primary: primary, secondary: secondary, log: log}
}

// Index writes only the primary. The secondary already holds every chunk;
// writing it here would duplicate rows that the message transaction wrote.
func (f *Fallback) Index(ctx context.Context, chunks []Chunk) error {
	return f.primary.Index(ctx, chunks)
}

func (f *Fallback) Search(ctx context.Context, q Query) ([]Hit, error) {
	hits, err := f.primary.Search(ctx, q)
	if err == nil {
		f.setReason("")
		return hits, nil
	}
	f.setReason(err.Error())
	// A degraded search is not a failed turn. The spec's rule is that a
	// sidecar being unreachable degrades and never fails a turn, so the
	// error is logged and keyword hits are returned in its place.
	f.log.Warn("vector search unavailable, using keyword search", "error", err)
	return f.secondary.Search(ctx, q)
}

// Status reports the primary when it is healthy and the secondary when it is
// not, because a degraded status should describe what is actually searchable.
func (f *Fallback) Status(ctx context.Context) (Status, error) {
	st, err := f.primary.Status(ctx)
	if err == nil && !st.Degraded && f.lastReason() == "" {
		return st, nil
	}

	reason := f.lastReason()
	switch {
	case err != nil:
		reason = err.Error()
	case st.Degraded && st.Reason != "":
		reason = st.Reason
	}

	fallbackStatus, ferr := f.secondary.Status(ctx)
	if ferr != nil {
		return Status{Backend: st.Backend, Degraded: true, Reason: reason}, nil
	}
	fallbackStatus.Degraded = true
	fallbackStatus.Reason = reason
	return fallbackStatus, nil
}

func (f *Fallback) setReason(r string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reason = r
}

func (f *Fallback) lastReason() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reason
}
```

- [ ] **Step 4: Run the tests**

Run: `make fmtcheck vet test`
Expected: PASS.

- [ ] **Step 5: Prove the fallback test has teeth**

Temporarily make `Search` return `f.primary.Search(ctx, q)` directly. `TestFallbackServesKeywordHitsWhenThePrimaryFails` must fail. Restore.

- [ ] **Step 6: Commit**

```bash
git add internal/recall/
git commit -m "feat(recall): fall back to keyword search when the vector store is down"
```

---

### Task 6: The mirror

**Files:**
- Create: `internal/recall/mirror/mirror.go`
- Test: `internal/recall/mirror/mirror_test.go`

**Interfaces:**
- Consumes: `store.IndexRow`, `store.IndexRowsSince`, `store.SyncCursor`, `store.SetSyncCursor` (Task 2); `recall.Recall` (Task 5's `Fallback` is not used here — the mirror writes the raw backend).
- Produces:
  - `type Source interface { IndexRowsSince(ctx, cursor int64, limit int) ([]store.IndexRow, error); SyncCursor(ctx, backend string) (int64, error); SetSyncCursor(ctx, backend string, cursor int64) error }`
  - `func New(src Source, target recall.Recall, backend string, log *slog.Logger) *Mirror`
  - `func (m *Mirror) Once(ctx context.Context) (int, error)` — pushes every pending row, returns how many chunks it wrote
  - `func (m *Mirror) Run(ctx context.Context, every time.Duration)`
  - `func (m *Mirror) Reset(ctx context.Context) error`

- [ ] **Step 1: Write the failing tests**

Create `internal/recall/mirror/mirror_test.go`:

```go
package mirror

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/codered/spore/internal/recall"
	"github.com/codered/spore/internal/store"
)

// fakeSource is the store as the mirror sees it: rows with rising ids and a
// cursor it can move.
type fakeSource struct {
	rows    []store.IndexRow
	cursors map[string]int64
	err     error
}

func newSource(texts ...string) *fakeSource {
	s := &fakeSource{cursors: map[string]int64{}}
	for i, text := range texts {
		s.rows = append(s.rows, store.IndexRow{
			RowID: int64(i + 1), Kind: recall.KindMessage,
			RefID: text, SessionID: "sess", CreatedAt: "2026-09-03T10:00:00Z", Text: text,
		})
	}
	return s
}

func (s *fakeSource) IndexRowsSince(_ context.Context, cursor int64, limit int) ([]store.IndexRow, error) {
	if s.err != nil {
		return nil, s.err
	}
	var out []store.IndexRow
	for _, r := range s.rows {
		if r.RowID > cursor {
			out = append(out, r)
		}
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *fakeSource) SyncCursor(_ context.Context, backend string) (int64, error) {
	return s.cursors[backend], nil
}

func (s *fakeSource) SetSyncCursor(_ context.Context, backend string, c int64) error {
	s.cursors[backend] = c
	return nil
}

type fakeTarget struct {
	got  []recall.Chunk
	err  error
	fail int // fail this many calls before succeeding
}

func (t *fakeTarget) Index(_ context.Context, chunks []recall.Chunk) error {
	if t.fail > 0 {
		t.fail--
		return errors.New("weaviate: connection refused")
	}
	if t.err != nil {
		return t.err
	}
	t.got = append(t.got, chunks...)
	return nil
}

func (t *fakeTarget) Search(context.Context, recall.Query) ([]recall.Hit, error) { return nil, nil }
func (t *fakeTarget) Status(context.Context) (recall.Status, error)              { return recall.Status{}, nil }

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestMirrorPushesEverythingOnAFirstRun(t *testing.T) {
	src := newSource("one", "two", "three")
	tgt := &fakeTarget{}
	n, err := New(src, tgt, "weaviate", quiet()).Once(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 || len(tgt.got) != 3 {
		t.Fatalf("wrote %d chunks (target saw %d), want 3", n, len(tgt.got))
	}
	if src.cursors["weaviate"] != 3 {
		t.Errorf("cursor = %d, want 3", src.cursors["weaviate"])
	}
	if tgt.got[0].Kind != recall.KindMessage || tgt.got[0].Text != "one" {
		t.Errorf("first chunk = %+v", tgt.got[0])
	}
	if tgt.got[0].CreatedAt.IsZero() {
		t.Error("created_at did not survive the mapping")
	}
}

func TestMirrorSendsOnlyWhatIsNew(t *testing.T) {
	src := newSource("one", "two")
	tgt := &fakeTarget{}
	m := New(src, tgt, "weaviate", quiet())
	if _, err := m.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	src.rows = append(src.rows, store.IndexRow{
		RowID: 3, Kind: recall.KindFact, RefID: "coffee", Text: "black", CreatedAt: "2026-09-03T10:00:00Z",
	})
	n, err := m.Once(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("wrote %d chunks, want only the new one", n)
	}
	if len(tgt.got) != 3 || tgt.got[2].ID != "coffee" {
		t.Errorf("target saw %+v, want the fact appended once", tgt.got)
	}
}

func TestMirrorDoesNotAdvanceTheCursorWhenTheTargetFails(t *testing.T) {
	// This is the property that makes a crash safe: a batch the target never
	// accepted must be sent again, which it only is if the cursor stayed put.
	src := newSource("one", "two")
	tgt := &fakeTarget{fail: 1}
	m := New(src, tgt, "weaviate", quiet())
	if _, err := m.Once(context.Background()); err == nil {
		t.Fatal("a failing target reported success")
	}
	if src.cursors["weaviate"] != 0 {
		t.Fatalf("cursor advanced to %d despite the failure", src.cursors["weaviate"])
	}
	n, err := m.Once(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("retry wrote %d chunks, want both", n)
	}
}

func TestMirrorIsIdleWhenThereIsNothingNew(t *testing.T) {
	src := newSource("one")
	tgt := &fakeTarget{}
	m := New(src, tgt, "weaviate", quiet())
	if _, err := m.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	n, err := m.Once(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("wrote %d chunks with nothing new", n)
	}
	if len(tgt.got) != 1 {
		t.Errorf("target saw %d chunks, want no re-send", len(tgt.got))
	}
}

func TestResetRewindsTheCursor(t *testing.T) {
	src := newSource("one", "two")
	m := New(src, &fakeTarget{}, "weaviate", quiet())
	if _, err := m.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if src.cursors["weaviate"] != 0 {
		t.Errorf("cursor = %d after a reset, want 0", src.cursors["weaviate"])
	}
}

func TestMirrorSkipsEmptyText(t *testing.T) {
	src := newSource("one")
	src.rows = append(src.rows, store.IndexRow{RowID: 2, Kind: recall.KindMessage, RefID: "2", Text: "  "})
	tgt := &fakeTarget{}
	n, err := New(src, tgt, "weaviate", quiet()).Once(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("wrote %d chunks, want the blank row skipped", n)
	}
	// The cursor must still clear the skipped row, or the mirror re-reads it
	// on every pass forever.
	if src.cursors["weaviate"] != 2 {
		t.Errorf("cursor = %d, want it past the skipped row", src.cursors["weaviate"])
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test -tags sqlite_fts5 ./internal/recall/mirror/`
Expected: FAIL — the package does not exist.

- [ ] **Step 3: Implement**

Create `internal/recall/mirror/mirror.go`:

```go
// Package mirror brings a secondary recall backend up to date with the
// keyword index. The keyword index is written inside the transaction that
// writes a message; a vector store cannot join that transaction, so it is
// caught up afterwards from a watermark instead.
package mirror

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/codered/spore/internal/recall"
	"github.com/codered/spore/internal/store"
)

// batchSize is how many rows one pass reads and sends. It bounds memory
// during a backfill of a long history; the pass loops until it is caught up.
const batchSize = 100

// Source is the store as the mirror needs it. The narrow interface is what
// lets the mirror be tested without a database.
type Source interface {
	IndexRowsSince(ctx context.Context, cursor int64, limit int) ([]store.IndexRow, error)
	SyncCursor(ctx context.Context, backend string) (int64, error)
	SetSyncCursor(ctx context.Context, backend string, cursor int64) error
}

type Mirror struct {
	src     Source
	target  recall.Recall
	backend string
	log     *slog.Logger
}

func New(src Source, target recall.Recall, backend string, log *slog.Logger) *Mirror {
	if log == nil {
		log = slog.Default()
	}
	return &Mirror{src: src, target: target, backend: backend, log: log}
}

// Once pushes every row written since the watermark and returns how many
// chunks it wrote. A first run on an existing database is a full backfill,
// which is why setup needs no separate backfill path.
func (m *Mirror) Once(ctx context.Context) (int, error) {
	cursor, err := m.src.SyncCursor(ctx, m.backend)
	if err != nil {
		return 0, err
	}
	written := 0
	for {
		rows, err := m.src.IndexRowsSince(ctx, cursor, batchSize)
		if err != nil {
			return written, err
		}
		if len(rows) == 0 {
			return written, nil
		}

		chunks := make([]recall.Chunk, 0, len(rows))
		last := cursor
		for _, r := range rows {
			last = r.RowID
			if strings.TrimSpace(r.Text) == "" {
				continue // nothing to embed; the cursor still moves past it
			}
			when, _ := time.Parse(time.RFC3339, r.CreatedAt)
			chunks = append(chunks, recall.Chunk{
				ID: r.RefID, Kind: r.Kind, Text: r.Text,
				SessionID: r.SessionID, CreatedAt: when,
			})
		}

		if len(chunks) > 0 {
			// The cursor moves only after the target accepted the batch. A
			// crash in between re-sends rows, and re-sending is harmless
			// because object ids are derived from kind and ref id.
			if err := m.target.Index(ctx, chunks); err != nil {
				return written, err
			}
			written += len(chunks)
		}
		if err := m.src.SetSyncCursor(ctx, m.backend, last); err != nil {
			return written, err
		}
		cursor = last
	}
}

// Run catches up on a timer until ctx is cancelled. A failed pass is logged
// and retried on the next tick: the vector store being down is a degraded
// state to sit in, not a reason to stop the daemon.
func (m *Mirror) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := m.Once(ctx)
			if err != nil {
				m.log.Warn("recall mirror fell behind", "backend", m.backend, "error", err)
				continue
			}
			if n > 0 {
				m.log.Info("recall mirror caught up", "backend", m.backend, "chunks", n)
			}
		}
	}
}

// Reset rewinds the watermark so the next pass re-sends the whole corpus.
// `recall reindex` rebuilds recall_fts from the source tables, which renumbers
// every rowid, so the old watermark points at nothing meaningful afterwards.
func (m *Mirror) Reset(ctx context.Context) error {
	return m.src.SetSyncCursor(ctx, m.backend, 0)
}
```

- [ ] **Step 4: Run the tests**

Run: `make fmtcheck vet test`
Expected: PASS.

- [ ] **Step 5: Prove the crash-safety test has teeth**

Temporarily move `SetSyncCursor` above the `target.Index` call. `TestMirrorDoesNotAdvanceTheCursorWhenTheTargetFails` must fail. Restore.

- [ ] **Step 6: Commit**

```bash
git add internal/recall/mirror/
git commit -m "feat(recall): add the watermark mirror"
```

---

### Task 7: Provisioning

**Files:**
- Create: `internal/recall/weaviate/provision.go`
- Test: `internal/recall/weaviate/provision_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `func ComposeFile() string` — the pinned compose YAML
  - `func WriteCompose(dir string) (string, error)` — writes `<dir>/compose.yml`, returns the path
  - `func DockerAvailable() error`
  - `func Up(ctx context.Context, dir string) error`
  - `func Down(ctx context.Context, dir string, removeVolumes bool) error`
  - `func WaitReady(ctx context.Context, b *Backend, timeout time.Duration) error`
  - `var execCommand = exec.CommandContext` — the seam tests replace

- [ ] **Step 1: Write the failing tests**

Create `internal/recall/weaviate/provision_test.go`:

```go
package weaviate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestComposePinsImagesAndBindsLoopbackOnly(t *testing.T) {
	yml := ComposeFile()
	for _, want := range []string{weaviateImage, model2vecImage} {
		if !strings.Contains(yml, want) {
			t.Errorf("compose file does not pin %q:\n%s", want, yml)
		}
	}
	if strings.Contains(yml, ":latest") {
		t.Error("compose file uses a floating tag")
	}
	// The daemon binds loopback and carries no auth. A sidecar published on
	// 0.0.0.0 would be the thing that opened the machine up.
	for _, line := range strings.Split(yml, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "- \"") || !strings.Contains(trimmed, ":") {
			continue
		}
		if strings.Contains(trimmed, "8080") || strings.Contains(trimmed, "50051") {
			if !strings.Contains(trimmed, "127.0.0.1:") {
				t.Errorf("port publishes beyond loopback: %s", trimmed)
			}
		}
	}
	if !strings.Contains(yml, "MODEL2VEC_INFERENCE_API") {
		t.Error("weaviate is not pointed at the inference service")
	}
	if !strings.Contains(yml, "DEFAULT_VECTORIZER_MODULE: text2vec-model2vec") {
		t.Error("the default vectorizer is not set")
	}
}

func TestWriteComposeCreatesTheFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "weaviate")
	path, err := WriteCompose(dir)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != ComposeFile() {
		t.Error("the file on disk is not the compose file")
	}
	if filepath.Base(path) != "compose.yml" {
		t.Errorf("wrote %q, want compose.yml", path)
	}
}

func TestWriteComposeIsIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "weaviate")
	if _, err := WriteCompose(dir); err != nil {
		t.Fatal(err)
	}
	// Re-running setup must not fail on a directory that already exists.
	if _, err := WriteCompose(dir); err != nil {
		t.Fatalf("second write failed: %v", err)
	}
}

func TestUpRunsComposeInTheRightDirectory(t *testing.T) {
	var gotArgs []string
	var gotDir string
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gotArgs = append([]string{name}, args...)
		c := exec.CommandContext(ctx, "true")
		return c
	}
	defer func() { execCommand = exec.CommandContext }()

	dir := t.TempDir()
	if err := Up(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(gotArgs, " ")
	if !strings.Contains(joined, "compose") || !strings.Contains(joined, "up") {
		t.Errorf("ran %q, want a compose up", joined)
	}
	if !strings.Contains(joined, "-d") {
		t.Errorf("ran %q, want it detached", joined)
	}
	if !strings.Contains(joined, dir) {
		t.Errorf("ran %q, want it to name the compose directory %q", joined, dir)
	}
	_ = gotDir
}

func TestDownCanRemoveVolumes(t *testing.T) {
	var joined string
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		joined = name + " " + strings.Join(args, " ")
		return exec.CommandContext(ctx, "true")
	}
	defer func() { execCommand = exec.CommandContext }()

	if err := Down(context.Background(), t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(joined, "-v") {
		t.Errorf("ran %q, want the data kept by default", joined)
	}
	if err := Down(context.Background(), t.TempDir(), true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(joined, "-v") {
		t.Errorf("ran %q, want the volumes removed when asked", joined)
	}
}

func TestUpReportsWhatFailed(t *testing.T) {
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "echo 'no such image' >&2; exit 1")
	}
	defer func() { execCommand = exec.CommandContext }()

	err := Up(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("a failing compose up reported success")
	}
	// The operator has to fix this by hand, so the command's own words matter
	// more than a wrapped exit status.
	if !strings.Contains(err.Error(), "no such image") {
		t.Errorf("error %q drops docker's output", err)
	}
}

func TestWaitReadyGivesUp(t *testing.T) {
	b, err := New(unreachable)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := WaitReady(context.Background(), b, 300*time.Millisecond); err == nil {
		t.Fatal("waiting on a dead address succeeded")
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("waited %s, want it bounded by the timeout", time.Since(start))
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test -tags sqlite_fts5 ./internal/recall/weaviate/ -run 'Compose|Up|Down|WaitReady'`
Expected: FAIL — `ComposeFile` undefined.

- [ ] **Step 3: Implement**

Create `internal/recall/weaviate/provision.go`:

```go
package weaviate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Pinned images. A floating tag turns a working machine into a broken one on
// a docker pull, and the collection schema is written against a specific
// module, so both are exact.
const (
	weaviateImage  = "cr.weaviate.io/semitechnologies/weaviate:1.38.5"
	model2vecImage = "cr.weaviate.io/semitechnologies/model2vec-inference:minishlab-potion-base-32M"
)

// execCommand is the seam the provisioning tests replace. Running docker for
// real in a unit test would make the suite depend on the machine.
var execCommand = exec.CommandContext

// ComposeFile is the whole sidecar. Two services: Weaviate, and the inference
// container that computes vectors -- no Weaviate vectorizer runs in-process,
// and the alternative to a second container is an embedding API key.
//
// Both publish on 127.0.0.1 only. spore's daemon binds loopback and carries no
// authentication, so a sidecar published on every interface would be the one
// thing that exposed the machine.
func ComposeFile() string {
	return `# Written by "spore recall setup". Edit it if you like; setup will not
# overwrite your changes silently -- it rewrites this file only when you run
# setup again.
services:
  weaviate:
    image: ` + weaviateImage + `
    restart: unless-stopped
    command:
      - --host
      - 0.0.0.0
      - --port
      - "8080"
      - --scheme
      - http
    ports:
      - "127.0.0.1:8080:8080"
      - "127.0.0.1:50051:50051"
    volumes:
      - weaviate_data:/var/lib/weaviate
    environment:
      QUERY_DEFAULTS_LIMIT: 25
      AUTHENTICATION_ANONYMOUS_ACCESS_ENABLED: "true"
      PERSISTENCE_DATA_PATH: /var/lib/weaviate
      DEFAULT_VECTORIZER_MODULE: text2vec-model2vec
      ENABLE_MODULES: text2vec-model2vec
      MODEL2VEC_INFERENCE_API: http://text2vec-model2vec:8080
      CLUSTER_HOSTNAME: node1
    depends_on:
      - text2vec-model2vec

  text2vec-model2vec:
    image: ` + model2vecImage + `
    restart: unless-stopped
    environment:
      ENABLE_CUDA: "0"

volumes:
  weaviate_data:
`
}

// WriteCompose puts the compose file where teardown can find it again.
func WriteCompose(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(path, []byte(ComposeFile()), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// DockerAvailable is the preflight. Failing here with a plain message beats
// half-provisioning and leaving the operator to work out which step broke.
func DockerAvailable() error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker is not on PATH; install Docker or set recall.url to an instance you run")
	}
	return nil
}

func compose(ctx context.Context, dir string, args ...string) error {
	full := append([]string{"compose", "--project-directory", dir, "-f", filepath.Join(dir, "compose.yml")}, args...)
	cmd := execCommand(ctx, "docker", full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return fmt.Errorf("docker %s: %w", strings.Join(full, " "), err)
		}
		return fmt.Errorf("docker %s: %w: %s", strings.Join(full, " "), err, msg)
	}
	return nil
}

func Up(ctx context.Context, dir string) error   { return compose(ctx, dir, "up", "-d") }

// Down stops the services. Volumes survive by default: a teardown that also
// destroyed the index would make "stop this for now" and "throw the data
// away" the same command.
func Down(ctx context.Context, dir string, removeVolumes bool) error {
	args := []string{"down"}
	if removeVolumes {
		args = append(args, "-v")
	}
	return compose(ctx, dir, args...)
}

// WaitReady polls until the instance answers or the timeout passes. A first
// start pulls two images and loads a model, so the caller's timeout is
// generous; the poll itself is cheap.
func WaitReady(ctx context.Context, b *Backend, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		if err := b.Ready(ctx); err == nil {
			return nil
		} else {
			last = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("waiting for %s: %w", Name, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
```

- [ ] **Step 4: Run the tests**

Run: `make fmtcheck vet test`
Expected: PASS. Note `TestUpRunsComposeInTheRightDirectory` relies on `/bin/true` and `sh`; both exist on the platforms spore targets.

- [ ] **Step 5: Commit**

```bash
git add internal/recall/weaviate/
git commit -m "feat(recall): provision weaviate and its inference sidecar"
```

---

### Task 8: Wiring and the CLI verbs

**Files:**
- Modify: `cmd/spore/recall.go`, `cmd/spore/wire.go`, `cmd/spore/main.go` (usage text), `internal/config/write.go`
- Test: `cmd/spore/recall_test.go`, `internal/config/write_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1–7.
- Produces:
  - `func buildRecall(cfg *config.Config, st *store.Store, log *slog.Logger) (recall.Recall, *mirror.Mirror, error)` in `cmd/spore/wire.go` — returns the backend the tools use and the mirror `serve` runs, with a nil mirror when the backend is `sqlitefts`
  - `func config.SetRecallBackend(path, backend string) error` in `internal/config/write.go`
  - `spore recall setup [--timeout 5m]`, `spore recall teardown [--purge]`

- [ ] **Step 1: Write the failing tests**

Add to `internal/config/write_test.go`:

```go
func TestSetRecallBackendWritesAndReloads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("default_model = \"x/y\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetRecallBackend(path, RecallWeaviate); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Recall.Backend != RecallWeaviate {
		t.Errorf("backend %q after the write, want %q", cfg.Recall.Backend, RecallWeaviate)
	}
	// Setup must not eat the rest of the file.
	if cfg.DefaultModel != "x/y" {
		t.Errorf("default_model = %q, want it preserved", cfg.DefaultModel)
	}
	// Running setup twice must not leave two [recall] sections.
	if err := SetRecallBackend(path, RecallWeaviate); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	if n := strings.Count(string(body), "[recall]"); n != 1 {
		t.Errorf("file has %d [recall] sections, want 1:\n%s", n, body)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("the file no longer loads: %v", err)
	}
}
```

Add to `cmd/spore/recall_test.go`:

```go
func TestBuildRecallDefaultsToSQLiteFTSWithNoMirror(t *testing.T) {
	st := openTestStore(t)
	cfg := config.Default()
	backend, m, err := buildRecall(cfg, st, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if backend == nil {
		t.Fatal("no backend")
	}
	if m != nil {
		t.Error("the default backend started a mirror; there is nothing to mirror to")
	}
	status, err := backend.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Backend != "sqlitefts" {
		t.Errorf("backend %q, want sqlitefts", status.Backend)
	}
}

func TestBuildRecallWrapsWeaviateInAFallback(t *testing.T) {
	st := openTestStore(t)
	cfg := config.Default()
	cfg.Recall.Backend = config.RecallWeaviate
	cfg.Recall.URL = "http://127.0.0.1:1" // nothing listens here
	backend, m, err := buildRecall(cfg, st, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		t.Error("no mirror was built for a mirrored backend")
	}
	// The whole point of the fallback: a dead vector store still searches.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := backend.Search(ctx, recall.Query{Text: "anything"}); err != nil {
		t.Errorf("search failed with the vector store down: %v", err)
	}
	status, err := backend.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Degraded {
		t.Error("status is healthy with the vector store down")
	}
}
```

Use the package's existing store-opening helper. If `quietLogger` does not exist in `cmd/spore`'s tests, add it next to the test:

```go
func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test -tags sqlite_fts5 ./cmd/spore/ ./internal/config/ -run 'BuildRecall|SetRecallBackend'`
Expected: FAIL — `buildRecall` and `SetRecallBackend` undefined.

- [ ] **Step 3: Implement the config writer**

In `internal/config/write.go`, following whatever pattern `LearnRule` already uses for editing the file in place:

```go
// SetRecallBackend records the backend `spore recall setup` provisioned. It
// rewrites an existing [recall] section rather than appending a second one:
// two sections of the same name make the file fail to load, which would turn
// a successful setup into a broken install.
func SetRecallBackend(path, backend string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	lines := strings.Split(string(body), "\n")
	inRecall := false
	replaced := false
	sectionAt := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			inRecall = trimmed == "[recall]"
			if inRecall {
				sectionAt = i
			}
			continue
		}
		if inRecall && strings.HasPrefix(trimmed, "backend") {
			lines[i] = fmt.Sprintf("backend = %q", backend)
			replaced = true
		}
	}
	switch {
	case replaced:
	case sectionAt >= 0:
		rest := append([]string{fmt.Sprintf("backend = %q", backend)}, lines[sectionAt+1:]...)
		lines = append(lines[:sectionAt+1], rest...)
	default:
		lines = append(lines, "", "[recall]", fmt.Sprintf("backend = %q", backend), "")
	}
	out := strings.Join(lines, "\n")
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
```

- [ ] **Step 4: Implement the wiring**

In `cmd/spore/wire.go`:

```go
// buildRecall chooses the search backend. sqlitefts is always constructed:
// with weaviate configured it becomes the fallback, so a vector store that is
// down costs semantic ranking and never costs search.
func buildRecall(cfg *config.Config, st *store.Store, log *slog.Logger) (recall.Recall, *mirror.Mirror, error) {
	keyword := sqlitefts.New(st.DB())
	if cfg.Recall.Backend != config.RecallWeaviate {
		return keyword, nil, nil
	}
	vector, err := weaviaterecall.New(cfg.WeaviateURL())
	if err != nil {
		return nil, nil, err
	}
	return recall.NewFallback(vector, keyword, log),
		mirror.New(st, vector, weaviaterecall.Name, log), nil
}
```

Import it as `weaviaterecall "github.com/codered/spore/internal/recall/weaviate"` so the package name does not collide with the client library in files that use both.

In `buildTools`, replace the `recallBackend := sqlitefts.New(st.DB())` line with a parameter: `buildTools` gains a `recallBackend recall.Recall` argument supplied by `buildAgent`, which calls `buildRecall` once and keeps the mirror to hand back. Change `buildAgent`'s signature to return the mirror alongside the agent and host:

```go
func buildAgent(cfg *config.Config, st *store.Store, approver policy.Approver) (*agent.Agent, *mcphost.Host, *mirror.Mirror, error)
```

Update `buildServer` to thread it through and return it too, and update `cmd/spore/serve.go` to start it:

```go
	if m != nil {
		// The mirror runs for the daemon's lifetime. It is not part of a turn:
		// a turn must never wait on a sidecar.
		go m.Run(ctx, 30*time.Second)
	}
```

- [ ] **Step 5: Implement the CLI verbs**

In `cmd/spore/recall.go`, extend the switch and add the two commands:

```go
	case "setup":
		return recallSetupCmd(ctx, cfg, st, args[1:])
	case "teardown":
		return recallTeardownCmd(ctx, cfg, args[1:])
```

```go
// weaviateDir is where the compose file lives, next to the database rather
// than in the workspace: it is spore's state, not the operator's project.
func weaviateDir(cfg *config.Config) string {
	return filepath.Join(cfg.DataDir, "weaviate")
}

func recallSetupCmd(ctx context.Context, cfg *config.Config, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("recall setup", flag.ContinueOnError)
	timeout := fs.Duration("timeout", 5*time.Minute, "how long to wait for the container to be ready")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if cfg.Recall.URL != "" {
		fmt.Printf("recall.url is set to %s, so there is nothing to provision.\n", cfg.Recall.URL)
	} else {
		if err := weaviaterecall.DockerAvailable(); err != nil {
			return err
		}
		dir := weaviateDir(cfg)
		path, err := weaviaterecall.WriteCompose(dir)
		if err != nil {
			return err
		}
		fmt.Println("wrote", path)
		fmt.Println("starting weaviate and its inference sidecar (the first run pulls two images)…")
		if err := weaviaterecall.Up(ctx, dir); err != nil {
			return err
		}
	}

	backend, err := weaviaterecall.New(cfg.WeaviateURL())
	if err != nil {
		return err
	}
	fmt.Print("waiting for it to answer… ")
	if err := weaviaterecall.WaitReady(ctx, backend, *timeout); err != nil {
		fmt.Println("no")
		return err
	}
	fmt.Println("ready")

	if err := backend.EnsureCollection(ctx); err != nil {
		return err
	}
	m := mirror.New(st, backend, weaviaterecall.Name, slog.Default())
	fmt.Print("backfilling… ")
	n, err := m.Once(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("%d chunks\n", n)

	if err := config.SetRecallBackend(cfg.Path, config.RecallWeaviate); err != nil {
		return err
	}
	fmt.Printf("recall.backend is now %q in %s\n", config.RecallWeaviate, cfg.Path)
	fmt.Println("restart the daemon to pick it up: spore serve --stop && spore serve")
	return nil
}

func recallTeardownCmd(ctx context.Context, cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("recall teardown", flag.ContinueOnError)
	purge := fs.Bool("purge", false, "also delete the vector store's data volume")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if cfg.Recall.URL != "" {
		return fmt.Errorf("recall.url points at %s, which spore did not start; stop it yourself", cfg.Recall.URL)
	}
	if err := weaviaterecall.Down(ctx, weaviateDir(cfg), *purge); err != nil {
		return err
	}
	if err := config.SetRecallBackend(cfg.Path, config.RecallSQLiteFTS); err != nil {
		return err
	}
	fmt.Printf("stopped; recall.backend is back to %q\n", config.RecallSQLiteFTS)
	return nil
}
```

Change `cmdRecall` so `status` reports through the configured backend rather than always `sqlitefts`, using `buildRecall`, and so `reindex` resets the mirror after rebuilding:

```go
	case "reindex":
		if err := recallReindexCmd(ctx, cfg, st); err != nil {
			return err
		}
		// A rebuild renumbers every FTS rowid, so the mirror's watermark now
		// points at nothing meaningful. Drop the mirrored copy and start over
		// rather than leaving it half-matched to the new numbering.
		_, m, err := buildRecall(cfg, st, slog.Default())
		if err != nil || m == nil {
			return err
		}
		vector, err := weaviaterecall.New(cfg.WeaviateURL())
		if err != nil {
			return err
		}
		if err := vector.DropAll(ctx); err != nil {
			return err
		}
		if err := vector.EnsureCollection(ctx); err != nil {
			return err
		}
		if err := m.Reset(ctx); err != nil {
			return err
		}
		n, err := m.Once(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("mirrored %d chunks to %s\n", n, weaviaterecall.Name)
		return nil
```

Update the usage block in `cmd/spore/main.go`:

```
  spore recall setup           provision the vector store and backfill it
  spore recall teardown        stop the vector store and fall back to keyword search
```

- [ ] **Step 6: Run everything**

Run: `make fmtcheck vet test`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add cmd/spore/ internal/config/ internal/recall/
git commit -m "feat(recall): wire the weaviate backend and add setup and teardown"
```

---

### Task 9: The container test

**Files:**
- Create: `internal/recall/weaviate/integration_test.go`
- Modify: `Makefile`, `README.md`

**Interfaces:**
- Consumes: everything.
- Produces: `make test-weaviate`.

- [ ] **Step 1: Write the test**

Create `internal/recall/weaviate/integration_test.go`. The build tag is the first line:

```go
//go:build weaviate

package weaviate

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/codered/spore/internal/recall"
)

// This file runs the same properties the unit tests assert about mapping,
// against a real server. It exists because a stub cannot tell you that a
// filter you built is a filter Weaviate accepts.
func liveBackend(t *testing.T) *Backend {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	url := os.Getenv("SPORE_WEAVIATE_URL")
	if url == "" {
		url = "http://127.0.0.1:8080"
	}
	b, err := New(url)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Ready(ctx); err != nil {
		t.Skipf("no weaviate at %s: %v (run: make weaviate-up)", url, err)
	}
	return b
}

func TestLiveRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	b := liveBackend(t)

	if err := b.DropAll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.EnsureCollection(ctx); err != nil {
		t.Fatal(err)
	}
	// A second call must be a no-op, because every daemon start makes it.
	if err := b.EnsureCollection(ctx); err != nil {
		t.Fatalf("EnsureCollection is not idempotent: %v", err)
	}

	now := time.Now().UTC()
	chunks := []recall.Chunk{
		{ID: "1", Kind: recall.KindMessage, SessionID: "sess-a", CreatedAt: now,
			Text: "the retry logic with exponential backoff lives in the provider package"},
		{ID: "2", Kind: recall.KindMessage, SessionID: "sess-b", CreatedAt: now,
			Text: "the cat sat on a warm windowsill in the afternoon"},
		{ID: "coffee", Kind: recall.KindFact, CreatedAt: now,
			Text: "the user drinks their coffee black"},
	}
	if err := b.Index(ctx, chunks); err != nil {
		t.Fatal(err)
	}
	// Indexing is asynchronous server-side; give it a moment to become
	// queryable rather than asserting into a race.
	time.Sleep(2 * time.Second)

	hits, err := b.Search(ctx, recall.Query{Text: "how do we handle failed requests", K: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	// The point of this backend: a query sharing no words with the target
	// still finds it. sqlitefts cannot do this, and that is the whole reason
	// the stage exists.
	if hits[0].ID != "1" {
		t.Errorf("top hit is %q (%q), want the retry message", hits[0].ID, hits[0].Text)
	}

	scoped, err := b.Search(ctx, recall.Query{Text: "anything at all", SessionID: "sess-b", K: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range scoped {
		if h.SessionID != "sess-b" {
			t.Errorf("session filter leaked %q; this filter is what stops a Discord user reading another session", h.SessionID)
		}
	}

	noFacts, err := b.Search(ctx, recall.Query{Text: "coffee", Kinds: []string{recall.KindMessage}, K: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range noFacts {
		if h.Kind == recall.KindFact {
			t.Error("the kind filter leaked a fact")
		}
	}

	// Re-indexing the same chunks must overwrite rather than duplicate.
	if err := b.Index(ctx, chunks); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	st, err := b.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Degraded {
		t.Fatalf("status degraded against a live server: %s", st.Reason)
	}
	if st.Counts[recall.KindMessage] != 2 {
		t.Errorf("message count = %d after re-indexing, want 2 -- ids are not deduplicating",
			st.Counts[recall.KindMessage])
	}
	if st.Counts[recall.KindFact] != 1 {
		t.Errorf("fact count = %d, want 1", st.Counts[recall.KindFact])
	}
}
```

- [ ] **Step 2: Add the Make targets**

In `Makefile`:

```make
WEAVIATE_DIR ?= $(HOME)/.spore/weaviate

weaviate-up:
	go run ./cmd/spore recall setup

test-weaviate:
	go test -tags "sqlite_fts5 weaviate" ./internal/recall/... -v

.PHONY: build install test vet fmt fmtcheck weaviate-up test-weaviate
```

- [ ] **Step 3: Run it for real**

```bash
make build
./spore recall setup
make test-weaviate
```

Expected: `TestLiveRoundTrip` PASSES. If the semantic assertion (`hits[0].ID != "1"`) fails, do not weaken it — that assertion is the reason the stage exists. Report it instead: it means the model or the collection config is wrong.

- [ ] **Step 4: Confirm the default suite is untouched**

Run: `make fmtcheck vet test`
Expected: PASS, with no container running. Then stop the container (`./spore recall teardown`) and run `make test` again — it must still pass.

- [ ] **Step 5: Document it**

In `README.md`, under the recall section, add:

```markdown
### Semantic recall

Keyword search needs nothing. For semantic search:

    spore recall setup

This writes `~/.spore/weaviate/compose.yml`, starts Weaviate and a small
embedding container on loopback, backfills your history, and switches
`recall.backend` to `weaviate`. Restart the daemon afterwards.

`spore recall status` reports which backend answered and whether it has
degraded; `spore recall teardown` stops the containers and goes back to
keyword search, keeping the data volume unless you pass `--purge`.

If you already run Weaviate, set `recall.url` and skip setup entirely.

Weaviate being down is never fatal: search falls back to the keyword index,
which is written in the same transaction as the message it indexes and is
therefore never behind.
```

- [ ] **Step 6: Commit**

```bash
git add internal/recall/weaviate/integration_test.go Makefile README.md
git commit -m "test(recall): exercise the weaviate backend against a real server"
```

---

## Self-review

**Spec coverage.** Section 5's `Recall` interface — unchanged, Task 4 implements it. Backends list — Task 8 wires both. Sync — Tasks 2 and 6. Provisioning (compose, pinned versions, loopback, readiness, collection, backfill, config write-back) — Tasks 7 and 8. `status`/`teardown` — Task 8. Degradation — Task 5. Configuration (`[recall] backend`, `url`) — Task 1. Testing (pure parts in the default suite, container behind `-tags weaviate`) — Tasks 3–7 and 9. The agent-callable provisioning tool is deferred by the amended spec, so it has no task by design.

**Not covered, and deliberately.** Deleting a fact leaves its object in Weaviate until the next `recall reindex`. `UnindexFact` removes the FTS row, and the mirror only moves forward, so the vector copy goes stale for deletions. It is bounded — a stale fact can surface in a semantic search until a reindex — and fixing it properly means a delete path through the mirror, which is its own task. Raise it after this plan lands rather than smuggling it in.

**Type consistency.** `recall.Chunk.ID` is the ref id everywhere; the store's row calls it `RefID` and the mirror maps `RefID → Chunk.ID` (Task 6), which matches `objectID(kind, refID)` in Task 3 and `decodeHits` reading `ref_id` back into `Chunk.ID`. `Name` is `"weaviate"` in Task 4 and is the watermark key in Tasks 6 and 8. `buildRecall` returns `(recall.Recall, *mirror.Mirror, error)` in Task 8 and is called with that shape in the same task's tests.
