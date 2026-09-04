package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/codered/spore/internal/provider"
)

func blocks(t *testing.T, bs ...provider.Block) []byte {
	t.Helper()
	b, err := json.Marshal(bs)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func countFTS(t *testing.T, st *Store, where string, args ...any) int {
	t.Helper()
	var n int
	q := `SELECT count(*) FROM recall_fts`
	if where != "" {
		q += " WHERE " + where
	}
	if err := st.DB().QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAppendMessageIndexesText(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	sid, err := st.CreateSession(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendMessage(ctx, Message{
		SessionID: sid, Role: "user",
		BlocksJSON: blocks(t, provider.Block{Type: provider.BlockText, Text: "exponential backoff with jitter"}),
	}); err != nil {
		t.Fatal(err)
	}
	if n := countFTS(t, st, `kind = 'message' AND recall_fts MATCH '"backoff"'`); n != 1 {
		t.Fatalf("message text not indexed: n=%d", n)
	}
}

// The trust boundary: tool results carry third-party text -- a fetched web
// page, an MCP server's reply -- and must never become searchable.
func TestToolResultBlocksAreNeverIndexed(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	sid, _ := st.CreateSession(ctx, "t")
	if _, err := st.AppendMessage(ctx, Message{
		SessionID: sid, Role: "tool",
		BlocksJSON: blocks(t,
			provider.Block{Type: provider.BlockToolResult, ID: "1", Content: "poisonedmarker from a web page"},
			provider.Block{Type: provider.BlockToolUse, ID: "1", Name: "web_fetch", Input: json.RawMessage(`{"url":"poisonedmarker"}`)},
		),
	}); err != nil {
		t.Fatal(err)
	}
	if n := countFTS(t, st, `recall_fts MATCH '"poisonedmarker"'`); n != 0 {
		t.Fatalf("tool result reached the index: n=%d", n)
	}
	if n := countFTS(t, st, ""); n != 0 {
		t.Fatalf("a message with no text blocks still wrote a row: n=%d", n)
	}
}

// A mixed message indexes its prose and drops the rest, rather than being
// skipped wholesale.
func TestMixedMessageIndexesOnlyItsText(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	sid, _ := st.CreateSession(ctx, "t")
	if _, err := st.AppendMessage(ctx, Message{
		SessionID: sid, Role: "assistant",
		BlocksJSON: blocks(t,
			provider.Block{Type: provider.BlockText, Text: "checking the keepthis file"},
			provider.Block{Type: provider.BlockToolResult, Content: "dropthis"},
		),
	}); err != nil {
		t.Fatal(err)
	}
	if n := countFTS(t, st, `recall_fts MATCH '"keepthis"'`); n != 1 {
		t.Fatalf("prose not indexed: n=%d", n)
	}
	if n := countFTS(t, st, `recall_fts MATCH '"dropthis"'`); n != 0 {
		t.Fatalf("tool result indexed alongside prose: n=%d", n)
	}
}

func TestSummaryIndexReplacesRatherThanAccumulates(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	sid, _ := st.CreateSession(ctx, "t")
	if err := st.SetSummary(ctx, sid, "first summary", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSummary(ctx, sid, "second summary", 2); err != nil {
		t.Fatal(err)
	}
	if n := countFTS(t, st, `kind = 'summary'`); n != 1 {
		t.Fatalf("summary rows accumulated: n=%d", n)
	}
	if n := countFTS(t, st, `recall_fts MATCH '"first"'`); n != 0 {
		t.Fatal("the superseded summary is still searchable")
	}
}

func TestDeletingASessionClearsItsIndexRows(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	sid, _ := st.CreateSession(ctx, "t")
	if _, err := st.AppendMessage(ctx, Message{SessionID: sid, Role: "user",
		BlocksJSON: blocks(t, provider.Block{Type: provider.BlockText, Text: "vanishing text"})}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSummary(ctx, sid, "vanishing summary", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`DELETE FROM sessions WHERE id = ?`, sid); err != nil {
		t.Fatal(err)
	}
	if n := countFTS(t, st, ""); n != 0 {
		t.Fatalf("index rows outlived their session: n=%d", n)
	}
}

func TestFactIndexWriteReplaceAndDelete(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	if err := st.IndexFact(ctx, "prefers-tabs", "tabs not spaces"); err != nil {
		t.Fatal(err)
	}
	if err := st.IndexFact(ctx, "prefers-tabs", "tabs always"); err != nil {
		t.Fatal(err)
	}
	if n := countFTS(t, st, `kind = 'fact'`); n != 1 {
		t.Fatalf("reindexing a fact accumulated rows: n=%d", n)
	}
	if err := st.UnindexFact(ctx, "prefers-tabs"); err != nil {
		t.Fatal(err)
	}
	if n := countFTS(t, st, `kind = 'fact'`); n != 0 {
		t.Fatalf("fact row survived deletion: n=%d", n)
	}
}

func TestReindexRebuildsAfterCorruption(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	sid, _ := st.CreateSession(ctx, "t")
	if _, err := st.AppendMessage(ctx, Message{SessionID: sid, Role: "user",
		BlocksJSON: blocks(t, provider.Block{Type: provider.BlockText, Text: "recoverable text"})}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSummary(ctx, sid, "recoverable summary", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`DELETE FROM recall_fts`); err != nil {
		t.Fatal(err)
	}
	n, err := st.ReindexAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("reindexed %d rows, want 2", n)
	}
	if got := countFTS(t, st, `recall_fts MATCH '"recoverable"'`); got != 2 {
		t.Fatalf("reindex did not restore both rows: n=%d", got)
	}
}

// A fact's file is its source of truth, so deleting the file must be able to
// make the fact unsearchable too. ReindexAll deliberately does not touch
// fact rows (it does not know where the files live), so the caller that
// re-indexes facts from disk needs a way to drop whatever the index
// currently holds first.
func TestClearFactIndexDropsOnlyFacts(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	sid, _ := st.CreateSession(ctx, "t")
	if _, err := st.AppendMessage(ctx, Message{SessionID: sid, Role: "user",
		BlocksJSON: blocks(t, provider.Block{Type: provider.BlockText, Text: "surviving message"})}); err != nil {
		t.Fatal(err)
	}
	if err := st.IndexFact(ctx, "prefers-tabs", "tabs not spaces"); err != nil {
		t.Fatal(err)
	}
	if err := st.ClearFactIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countFTS(t, st, `kind = 'fact'`); n != 0 {
		t.Fatalf("fact row survived ClearFactIndex: n=%d", n)
	}
	if n := countFTS(t, st, `kind = 'message'`); n != 1 {
		t.Fatalf("ClearFactIndex touched non-fact rows: n=%d", n)
	}
}

// ReindexAll is a second write path for the same trust boundary
// TestToolResultBlocksAreNeverIndexed proves for AppendMessage. Only the
// AppendMessage path had a test; a reviewer proved the gap by swapping
// ReindexAll's call to indexableText for the raw blocks JSON and watching the
// whole suite stay green. backfillRecall calls ReindexAll for its own work,
// so this test covers that startup path too.
func TestReindexAllNeverIndexesToolResultBlocks(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	sid, _ := st.CreateSession(ctx, "t")
	if _, err := st.AppendMessage(ctx, Message{
		SessionID: sid, Role: "tool",
		BlocksJSON: blocks(t,
			provider.Block{Type: provider.BlockText, Text: "legitimate reply text"},
			provider.Block{Type: provider.BlockToolResult, ID: "1", Content: "poisonedmarker from a fetched page"},
		),
	}); err != nil {
		t.Fatal(err)
	}
	// Wipe the index and force ReindexAll to rebuild it from the messages
	// table, the way `spore recall reindex` and a fresh-database backfill do.
	if _, err := st.DB().Exec(`DELETE FROM recall_fts`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReindexAll(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countFTS(t, st, `recall_fts MATCH '"poisonedmarker"'`); n != 0 {
		t.Fatalf("reindex made a tool_result block searchable: n=%d", n)
	}
	if n := countFTS(t, st, `recall_fts MATCH '"legitimate"'`); n != 1 {
		t.Fatalf("reindex dropped the message's real text: n=%d", n)
	}
}

// A recall index that cannot even be queried must not stop spore from
// starting, because cmdRecall opens the store the same way the daemon does --
// a fatal Open would mean the index breaks the one command that repairs it.
// Dropping an FTS5 shadow table is a real corruption, not a stand-in: the
// virtual table itself becomes unreadable, which is a stronger failure than
// the drifted-or-wiped-rows case ReindexAll repairs.
func TestOpenSurvivesACorruptRecallIndex(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "spore.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	sid, _ := st.CreateSession(ctx, "t")
	if _, err := st.AppendMessage(ctx, Message{SessionID: sid, Role: "user",
		BlocksJSON: blocks(t, provider.Block{Type: provider.BlockText, Text: "before the corruption"})}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`DROP TABLE recall_fts_data`); err != nil {
		t.Fatalf("corrupt the fts5 shadow table: %v", err)
	}
	st.Close()

	st2, err := Open(path)
	if err != nil {
		t.Fatalf("Open failed on a corrupt recall index, so cmdRecall could never repair it: %v", err)
	}
	defer st2.Close()

	// The store itself -- unrelated to recall -- must still work.
	if _, _, err := st2.Session(ctx, sid); err != nil {
		t.Fatalf("store is unusable after a degraded recall backfill: %v", err)
	}
}

// An existing database predates the index, so opening it must backfill.
func TestOpenBackfillsAnExistingDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "spore.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	sid, _ := st.CreateSession(ctx, "t")
	if _, err := st.AppendMessage(ctx, Message{SessionID: sid, Role: "user",
		BlocksJSON: blocks(t, provider.Block{Type: provider.BlockText, Text: "historic text"})}); err != nil {
		t.Fatal(err)
	}
	// Simulate a database written before recall existed.
	if _, err := st.DB().Exec(`DELETE FROM recall_fts`); err != nil {
		t.Fatal(err)
	}
	st.Close()

	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if n := countFTS(t, st2, `recall_fts MATCH '"historic"'`); n != 1 {
		t.Fatalf("open did not backfill: n=%d", n)
	}
}

func TestIndexRowsSinceWalksTheCorpusInOrder(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	id, err := st.CreateSession(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"first message", "second message", "third message"} {
		bs := blocks(t, provider.Block{Type: provider.BlockText, Text: text})
		if _, err := st.AppendMessage(ctx, Message{SessionID: id, Role: "user", BlocksJSON: bs}); err != nil {
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
			t.Errorf("rows are not ascending by rowid: %+v", rows)
		}
	}
	if rows[0].Text != "first message" || rows[0].Kind != kindMessage {
		t.Errorf("first row = %+v, want the first message", rows[0])
	}

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
		bs := blocks(t, provider.Block{Type: provider.BlockText, Text: "m"})
		if _, err := st.AppendMessage(ctx, Message{SessionID: id, Role: "user", BlocksJSON: bs}); err != nil {
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
	if len(rows) != 1 || rows[0].Kind != kindFact || rows[0].RefID != "coffee" {
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
