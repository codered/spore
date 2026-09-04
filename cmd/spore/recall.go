package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/memory"
	"github.com/codered/spore/internal/recall"
	"github.com/codered/spore/internal/recall/mirror"
	weaviaterecall "github.com/codered/spore/internal/recall/weaviate"
	"github.com/codered/spore/internal/store"
)

// cmdRecall is the operator's view of the index the model searches through
// recall_search: the same backend, unscoped, because whoever runs the binary
// is the operator.
func cmdRecall(ctx context.Context, cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: spore recall search <query> | status | reindex | setup | teardown")
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()

	// The operator sees what the model sees, so search and status go through
	// the configured backend rather than always through the keyword index.
	backend, mir, err := buildRecall(cfg, st, slog.Default())
	if err != nil {
		return err
	}

	switch args[0] {
	case "search":
		return recallSearchCmd(ctx, backend, args[1:])
	case "status":
		return recallStatusCmd(ctx, backend)
	case "reindex":
		return recallReindexCmd(ctx, cfg, st, mir)
	case "setup":
		return recallSetupCmd(ctx, cfg, st, args[1:])
	case "teardown":
		return recallTeardownCmd(ctx, cfg, args[1:])
	default:
		return fmt.Errorf("unknown recall command %q: want search, status, reindex, setup or teardown", args[0])
	}
}

// weaviateDir is where the compose file lives: next to the database rather
// than in the workspace, because it is spore's state and not the operator's
// project.
func weaviateDir(cfg *config.Config) string {
	return filepath.Join(cfg.DataDir, "weaviate")
}

func recallSetupCmd(ctx context.Context, cfg *config.Config, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("recall setup", flag.ContinueOnError)
	timeout := fs.Duration("timeout", 5*time.Minute, "how long to wait for the container to become ready")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if cfg.Recall.URL != "" {
		fmt.Printf("recall.url is %s, so there is nothing to provision.\n", cfg.Recall.URL)
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
		fmt.Println("starting weaviate and its embedding sidecar (a first run pulls two images)...")
		if err := weaviaterecall.Up(ctx, dir); err != nil {
			return err
		}
	}

	backend, err := weaviaterecall.New(cfg.WeaviateURL())
	if err != nil {
		return err
	}
	fmt.Print("waiting for it to answer... ")
	if err := weaviaterecall.WaitReady(ctx, backend, *timeout); err != nil {
		fmt.Println("no")
		return err
	}
	fmt.Println("ready")

	if err := backend.EnsureCollection(ctx); err != nil {
		return err
	}
	fmt.Print("backfilling... ")
	n, err := mirror.New(st, backend, weaviaterecall.Name, slog.Default()).Once(ctx)
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

func recallSearchCmd(ctx context.Context, backend recall.Recall, args []string) error {
	fs := flag.NewFlagSet("recall search", flag.ContinueOnError)
	kind := fs.String("kind", "", "restrict to one kind: message, summary or fact")
	session := fs.String("session", "", "restrict to one session id")
	k := fs.Int("k", recall.DefaultK, "maximum hits")
	if err := fs.Parse(args); err != nil {
		return err
	}
	query := strings.Join(fs.Args(), " ")
	if strings.TrimSpace(query) == "" {
		return fmt.Errorf("usage: spore recall search [--kind K] [--session ID] [-k N] <query>")
	}
	q := recall.Query{Text: query, K: *k, SessionID: *session}
	if *kind != "" {
		q.Kinds = []string{*kind}
	}
	hits, err := backend.Search(ctx, q)
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		fmt.Println("no matches")
		return nil
	}
	for _, h := range hits {
		fmt.Printf("%s\t%s\t%s\n", h.Kind, h.ID, h.CreatedAt.Format("2006-01-02"))
		body := h.Excerpt
		if h.Kind == recall.KindFact {
			body = h.Text
		}
		for _, line := range strings.Split(body, "\n") {
			fmt.Printf("    %s\n", line)
		}
		fmt.Println()
	}
	return nil
}

func recallStatusCmd(ctx context.Context, backend recall.Recall) error {
	st, err := backend.Status(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("backend: %s\n", st.Backend)
	if st.Degraded {
		fmt.Printf("degraded: %s\n", st.Reason)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "KIND\tINDEXED")
	kinds := make([]string, 0, len(st.Counts))
	for k := range st.Counts {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		fmt.Fprintf(w, "%s\t%d\n", k, st.Counts[k])
	}
	return w.Flush()
}

// recallReindexCmd rebuilds both halves: messages and summaries from SQLite,
// facts from the files that own them.
func recallReindexCmd(ctx context.Context, cfg *config.Config, st *store.Store, mir *mirror.Mirror) error {
	n, err := st.ReindexAll(ctx)
	if err != nil {
		return err
	}
	dir := filepath.Join(cfg.DataDir, "memory")
	facts, errs := memory.Load(dir)
	for _, e := range errs {
		if errors.Is(e, memory.ErrReadDir) {
			// The directory could not be read at all (permission denied, an
			// unmounted volume). That is not evidence the facts are gone, so
			// it must not be treated as "the directory is empty": clearing
			// the index here would erase every indexed fact and re-index
			// nothing. Leave the index untouched and fail loudly instead of
			// reporting a reindex that silently dropped everything.
			fmt.Fprintf(os.Stderr, "fact index left unchanged: cannot read fact directory %s: %v\n", dir, e)
			return e
		}
		fmt.Fprintf(os.Stderr, "skipping malformed fact: %v\n", e)
	}
	// Facts are file-owned and ReindexAll leaves them alone, so a hand-deleted
	// fact file has no row anywhere to clear it. Wipe the fact index before
	// re-indexing what is still on disk, so the result matches the directory
	// exactly instead of keeping rows for files that are gone.
	if err := st.ClearFactIndex(ctx); err != nil {
		return err
	}
	for _, f := range facts {
		if err := st.IndexFact(ctx, f.Name, f.Description+"\n"+f.Body); err != nil {
			return err
		}
	}
	fmt.Printf("reindexed %d messages and summaries, %d facts\n", n, len(facts))

	if mir == nil {
		return nil
	}
	// A rebuild renumbers every FTS rowid, so the mirror's watermark now
	// points at nothing meaningful. Drop the mirrored copy and start it over
	// rather than leaving it half-matched to the new numbering.
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
	if err := mir.Reset(ctx); err != nil {
		return err
	}
	mirrored, err := mir.Once(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("mirrored %d chunks to %s\n", mirrored, weaviaterecall.Name)
	return nil
}
