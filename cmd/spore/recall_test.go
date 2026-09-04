package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
)

func recallFixture(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "memory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory", "prefers-tabs.md"),
		[]byte("---\nname: prefers-tabs\ndescription: formatting\ntype: user\n---\n\nTabs, always.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(`
default_model = "p/m"
data_dir = "`+dir+`"

[providers.p]
kind = "anthropic"
api_key = "x"

[policy]
workspace = "`+dir+`"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	sid, err := st.CreateSession(ctx, "t", "")
	if err != nil {
		t.Fatal(err)
	}
	blocks, _ := json.Marshal([]provider.Block{{Type: provider.BlockText, Text: "exponential backoff and jitter"}})
	if _, err := st.AppendMessage(ctx, store.Message{SessionID: sid, Role: "user", BlocksJSON: blocks}); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func captureStdout(t *testing.T, fn func() error) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := fn()
	w.Close()
	os.Stdout = old
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if runErr != nil {
		t.Fatalf("command failed: %v", runErr)
	}
	return sb.String()
}

func TestRecallSearchCommandFindsAMessage(t *testing.T) {
	cfg := recallFixture(t)
	out := captureStdout(t, func() error {
		return cmdRecall(context.Background(), cfg, []string{"search", "backoff"})
	})
	if !strings.Contains(out, "backoff") {
		t.Fatalf("search found nothing:\n%s", out)
	}
}

// reindex rebuilds messages from SQLite and facts from disk, which is the
// documented repair for an index that has drifted.
func TestRecallReindexRestoresBothSources(t *testing.T) {
	cfg := recallFixture(t)
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`DELETE FROM recall_fts`); err != nil {
		t.Fatal(err)
	}
	st.Close()

	captureStdout(t, func() error { return cmdRecall(context.Background(), cfg, []string{"reindex"}) })

	out := captureStdout(t, func() error {
		return cmdRecall(context.Background(), cfg, []string{"search", "Tabs"})
	})
	if !strings.Contains(out, "prefers-tabs") {
		t.Fatalf("facts were not reindexed:\n%s", out)
	}
	out = captureStdout(t, func() error {
		return cmdRecall(context.Background(), cfg, []string{"search", "backoff"})
	})
	if !strings.Contains(out, "backoff") {
		t.Fatalf("messages were not reindexed:\n%s", out)
	}
}

// Spec 5 says the fact file is the source of truth and SQLite "never owns"
// its text. Deleting the file by hand -- including a sensitive fact -- must
// make it unsearchable once the index is repaired, not keep it retrievable
// forever because nothing ever removes the orphaned row.
func TestRecallReindexDropsAFactWhoseFileWasDeleted(t *testing.T) {
	cfg := recallFixture(t)
	captureStdout(t, func() error { return cmdRecall(context.Background(), cfg, []string{"reindex"}) })
	out := captureStdout(t, func() error {
		return cmdRecall(context.Background(), cfg, []string{"search", "Tabs"})
	})
	if !strings.Contains(out, "prefers-tabs") {
		t.Fatalf("fixture fact was not indexed to begin with:\n%s", out)
	}

	if err := os.Remove(filepath.Join(cfg.DataDir, "memory", "prefers-tabs.md")); err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() error { return cmdRecall(context.Background(), cfg, []string{"reindex"}) })

	out = captureStdout(t, func() error {
		return cmdRecall(context.Background(), cfg, []string{"search", "Tabs"})
	})
	if strings.Contains(out, "prefers-tabs") {
		t.Fatalf("deleted fact is still searchable after reindex:\n%s", out)
	}
}

// A directory that cannot be read is not evidence the facts are gone -- it
// might be a transient permission problem or an unmounted volume -- so a
// reindex that hits it must leave the existing index rows untouched and
// report failure, rather than clearing the fact index and re-indexing
// nothing.
func TestRecallReindexLeavesIndexAloneWhenFactDirIsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits, so the directory would still be readable")
	}
	cfg := recallFixture(t)
	captureStdout(t, func() error { return cmdRecall(context.Background(), cfg, []string{"reindex"}) })
	out := captureStdout(t, func() error {
		return cmdRecall(context.Background(), cfg, []string{"search", "Tabs"})
	})
	if !strings.Contains(out, "prefers-tabs") {
		t.Fatalf("fixture fact was not indexed to begin with:\n%s", out)
	}

	memDir := filepath.Join(cfg.DataDir, "memory")
	if err := os.Chmod(memDir, 0o000); err != nil {
		t.Fatal(err)
	}
	// Restore permissions so t.TempDir()'s own cleanup can remove the
	// directory; this Cleanup was registered after the fixture's, so it
	// runs first (Cleanup is LIFO).
	t.Cleanup(func() { os.Chmod(memDir, 0o700) })

	if err := cmdRecall(context.Background(), cfg, []string{"reindex"}); err == nil {
		t.Fatal("reindex with an unreadable fact directory returned no error")
	}

	out = captureStdout(t, func() error {
		return cmdRecall(context.Background(), cfg, []string{"search", "Tabs"})
	})
	if !strings.Contains(out, "prefers-tabs") {
		t.Fatalf("previously indexed fact did not survive a reindex that hit an unreadable directory:\n%s", out)
	}
}

func TestRecallStatusReportsCounts(t *testing.T) {
	cfg := recallFixture(t)
	captureStdout(t, func() error { return cmdRecall(context.Background(), cfg, []string{"reindex"}) })
	out := captureStdout(t, func() error {
		return cmdRecall(context.Background(), cfg, []string{"status"})
	})
	if !strings.Contains(out, "sqlitefts") || !strings.Contains(out, "message") {
		t.Fatalf("status is not informative:\n%s", out)
	}
}

func TestRecallRejectsAnUnknownSubcommand(t *testing.T) {
	cfg := recallFixture(t)
	if err := cmdRecall(context.Background(), cfg, []string{"frobnicate"}); err == nil {
		t.Fatal("unknown subcommand accepted")
	}
	if err := cmdRecall(context.Background(), cfg, nil); err == nil {
		t.Fatal("missing subcommand accepted")
	}
	if err := cmdRecall(context.Background(), cfg, []string{"search"}); err == nil {
		t.Fatal("search with no query accepted")
	}
}
