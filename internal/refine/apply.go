package refine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/codered/spore/internal/memory"
	"github.com/codered/spore/internal/store"
)

// Size caps, in bytes. An edit over its cap is dropped, never truncated: a
// fact cut mid-sentence is worse than no fact.
const (
	maxFactBody     = 4 << 10
	maxNotesAppend  = 2 << 10
	maxNotesReplace = 16 << 10
)

// snapshot is what the planner was shown, captured before the planner call
// so apply can tell whether a target changed underneath it.
type snapshot struct {
	factsDir  string
	facts     []memory.Fact
	raw       map[string]*string // fact name or notes path -> file content; absent key = no file
	notesPath string
	notes     string
}

func (r *Refiner) takeSnapshot(workspace string) (snapshot, error) {
	s := snapshot{factsDir: r.Facts.Dir(), raw: map[string]*string{}}
	facts, _ := memory.Load(s.factsDir) // a malformed file costs that fact, as everywhere else
	for _, f := range facts {
		c, err := readTarget(f.Path)
		if err != nil {
			return snapshot{}, err
		}
		s.raw[f.Name] = c
	}
	s.facts = facts
	s.notesPath = r.Cfg.AgentPath(workspace)
	if s.notesPath != "" {
		c, err := readTarget(s.notesPath)
		if err != nil {
			return snapshot{}, err
		}
		s.raw[s.notesPath] = c
		if c != nil {
			s.notes = *c
		}
	}
	return s, nil
}

// change is a validated edit: what file it touches and its content before
// (as the planner saw it) and after.
type change struct {
	target string
	path   string
	before *string
	after  *string
}

func findFact(facts []memory.Fact, name string) (memory.Fact, bool) {
	for _, f := range facts {
		if f.Name == name {
			return f, true
		}
	}
	return memory.Fact{}, false
}

func checkFact(f memory.Fact) error {
	if err := f.Validate(); err != nil {
		return err
	}
	if len(f.Body) > maxFactBody {
		return fmt.Errorf("body is %d bytes, over the %d-byte cap", len(f.Body), maxFactBody)
	}
	return nil
}

// resolve validates one edit against the snapshot and computes its change.
func resolve(s snapshot, e Edit) (change, error) {
	if strings.TrimSpace(e.Rationale) == "" {
		return change{}, errors.New("no rationale")
	}
	switch e.Kind {
	case KindFactCreate, KindFactUpdate, KindFactDelete:
		path, err := memory.Path(s.factsDir, e.Name)
		if err != nil {
			return change{}, err
		}
		existing, exists := findFact(s.facts, e.Name)
		ch := change{target: e.Name, path: path, before: s.raw[e.Name]}
		switch e.Kind {
		case KindFactCreate:
			if exists {
				return change{}, fmt.Errorf("fact %q already exists", e.Name)
			}
			f := memory.Fact{Name: e.Name, Type: e.Type, Description: e.Description, Body: e.Body}
			if err := checkFact(f); err != nil {
				return change{}, err
			}
			ch.after = ptr(memory.Render(f))
		case KindFactUpdate:
			if !exists {
				return change{}, fmt.Errorf("no fact named %q", e.Name)
			}
			f := existing
			if e.Type != "" {
				f.Type = e.Type
			}
			if e.Description != "" {
				f.Description = e.Description
			}
			if e.Body != "" {
				f.Body = e.Body
			}
			if err := checkFact(f); err != nil {
				return change{}, err
			}
			after := memory.Render(f)
			if sameContent(ch.before, &after) {
				return change{}, errors.New("no change")
			}
			ch.after = &after
		case KindFactDelete:
			if !exists {
				return change{}, fmt.Errorf("no fact named %q", e.Name)
			}
		}
		return ch, nil

	case KindNotesAppend, KindNotesReplace:
		if s.notesPath == "" {
			return change{}, errors.New("this session has no workspace")
		}
		ch := change{target: s.notesPath, path: s.notesPath, before: s.raw[s.notesPath]}
		if e.Kind == KindNotesAppend {
			line := strings.Join(strings.Fields(e.Text), " ")
			if line == "" {
				return change{}, errors.New("empty note")
			}
			if len(line) > maxNotesAppend {
				return change{}, fmt.Errorf("note is %d bytes, over the %d-byte cap", len(line), maxNotesAppend)
			}
			cur := s.notes
			if cur != "" && !strings.HasSuffix(cur, "\n") {
				cur += "\n"
			}
			ch.after = ptr(cur + "- " + line + "\n")
			return ch, nil
		}
		body := strings.TrimSpace(e.Text)
		if body == "" {
			return change{}, errors.New("notes.replace with empty text would erase every order")
		}
		if len(body) > maxNotesReplace {
			return change{}, fmt.Errorf("notes are %d bytes, over the %d-byte cap", len(body), maxNotesReplace)
		}
		after := body + "\n"
		if sameContent(ch.before, &after) {
			return change{}, errors.New("no change")
		}
		ch.after = &after
		return ch, nil
	}
	return change{}, fmt.Errorf("unknown edit kind %q", e.Kind)
}

func label(e Edit) string {
	if strings.HasPrefix(e.Kind, "fact.") {
		return e.Kind + " " + e.Name
	}
	return e.Kind
}

// apply validates the planner's edits and applies or proposes each one.
func (r *Refiner) apply(ctx context.Context, sess store.Session, trig Trigger, s snapshot, edits []Edit, res *Result) {
	trusted := sess.Source == store.SourceChat
	seen := map[string]bool{}
	valid := 0
	for _, e := range edits {
		ch, err := resolve(s, e)
		if err != nil {
			res.Dropped = append(res.Dropped, fmt.Sprintf("%s: %v", label(e), err))
			continue
		}
		if seen[ch.target] {
			res.Dropped = append(res.Dropped, fmt.Sprintf("%s: a second edit to the same target", label(e)))
			continue
		}
		if valid >= r.Cfg.Refine.MaxEdits {
			res.Dropped = append(res.Dropped, fmt.Sprintf("%s: over max_edits (%d)", label(e), r.Cfg.Refine.MaxEdits))
			continue
		}
		seen[ch.target] = true
		valid++
		row := store.Refinement{
			RoundID: res.RoundID, SessionID: sess.ID, Trigger: string(trig), Kind: e.Kind,
			Target: ch.target, Before: ch.before, After: ch.after,
			Rationale: strings.Join(strings.Fields(e.Rationale), " "),
		}
		if !trusted {
			row.Status = store.RefineProposed
			if row.ID, err = r.Store.AddRefinement(ctx, row); err != nil {
				res.Dropped = append(res.Dropped, fmt.Sprintf("%s: %v", label(e), err))
				continue
			}
			res.Proposed = append(res.Proposed, row)
			continue
		}
		r.applyOne(ctx, row, ch.path, res)
	}
}

// applyOne writes one trusted edit: staleness check, ledger row, then file.
func (r *Refiner) applyOne(ctx context.Context, row store.Refinement, path string, res *Result) {
	r.fileMu.Lock()
	defer r.fileMu.Unlock()
	cur, err := readTarget(path)
	if err != nil || !sameContent(cur, row.Before) {
		row.Status = store.RefineStale
		id, err := r.Store.AddRefinement(ctx, row) // ledger the stale edit
		if err != nil {
			res.Dropped = append(res.Dropped, fmt.Sprintf("%s %s: %v", row.Kind, row.Target, err))
			return
		}
		row.ID = id
		res.Stale = append(res.Stale, row)
		return
	}
	row.Status = store.RefineApplied
	id, err := r.Store.AddRefinement(ctx, row) // the row first: no write goes unrecorded
	if err != nil {
		res.Dropped = append(res.Dropped, fmt.Sprintf("%s %s: %v", row.Kind, row.Target, err))
		return
	}
	row.ID = id
	if err := r.write(ctx, row.Kind, row.Target, path, row.After); err != nil {
		_, _ = r.Store.SetRefinementStatus(ctx, id, store.RefineApplied, store.RefineFailed)
		row.Status = store.RefineFailed
		res.Failed = append(res.Failed, row)
		return
	}
	res.Applied = append(res.Applied, row)
}

// write puts content on disk and, for a fact, brings the prompt cache and
// the recall index up to date, exactly as the memory tool does. Only a
// writeTarget error is returned; index errors are logged but not propagated,
// since the file write succeeded and leaving the index stale is better than
// marking the whole edit as failed.
func (r *Refiner) write(ctx context.Context, kind, target, path string, content *string) error {
	if err := writeTarget(path, content); err != nil {
		return err
	}
	if !strings.HasPrefix(kind, "fact.") {
		return nil
	}
	r.Facts.Reload()
	for _, f := range r.Facts.Facts() {
		if f.Name == target {
			if err := r.index.IndexFact(ctx, target, f.Description+"\n"+f.Body); err != nil {
				slog.Warn("refinement wrote the file but could not update the recall index", "target", target, "error", err)
			}
			return nil
		}
	}
	if err := r.index.UnindexFact(ctx, target); err != nil {
		slog.Warn("refinement wrote the file but could not update the recall index", "target", target, "error", err)
	}
	return nil
}

// pathFor maps a ledger row back to its file. A notes target is an absolute
// path that must still look like an agent.md: the row is ours, but a path
// read back from a database is checked before it is written to.
func (r *Refiner) pathFor(row store.Refinement) (string, error) {
	if strings.HasPrefix(row.Kind, "fact.") {
		return memory.Path(r.Facts.Dir(), row.Target)
	}
	if row.Kind == KindSelfUpdate {
		if row.Target != r.Cfg.SelfPath() {
			return "", fmt.Errorf("refinement %d has an unexpected self.md target %q", row.ID, row.Target)
		}
		return row.Target, nil
	}
	if !filepath.IsAbs(row.Target) || filepath.Base(row.Target) != "agent.md" || filepath.Base(filepath.Dir(row.Target)) != ".spore" {
		return "", fmt.Errorf("refinement %d has an unexpected notes target %q", row.ID, row.Target)
	}
	return row.Target, nil
}
