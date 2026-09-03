package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/memory"
	"github.com/codered/spore/internal/recall"
	"github.com/codered/spore/internal/recall/sqlitefts"
	"github.com/codered/spore/internal/store"
)

// cmdRecall is the operator's view of the index the model searches through
// recall_search: the same backend, unscoped, because whoever runs the binary
// is the operator.
func cmdRecall(ctx context.Context, cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: spore recall search <query> | status | reindex")
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	backend := sqlitefts.New(st.DB())

	switch args[0] {
	case "search":
		return recallSearchCmd(ctx, backend, args[1:])
	case "status":
		return recallStatusCmd(ctx, backend)
	case "reindex":
		return recallReindexCmd(ctx, cfg, st)
	default:
		return fmt.Errorf("unknown recall command %q: want search, status or reindex", args[0])
	}
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
func recallReindexCmd(ctx context.Context, cfg *config.Config, st *store.Store) error {
	n, err := st.ReindexAll(ctx)
	if err != nil {
		return err
	}
	dir := filepath.Join(cfg.DataDir, "memory")
	facts, errs := memory.Load(dir)
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "skipping malformed fact: %v\n", e)
	}
	for _, f := range facts {
		if err := st.IndexFact(ctx, f.Name, f.Description+"\n"+f.Body); err != nil {
			return err
		}
	}
	fmt.Printf("reindexed %d messages and summaries, %d facts\n", n, len(facts))
	return nil
}
