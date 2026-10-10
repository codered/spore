package refine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/store"
)

var (
	// ErrSubagent refuses a sub-agent session: the parent's review covers it.
	ErrSubagent = errors.New("sub-agent sessions are not refined")
	// ErrBusy means a round is already running for the session.
	ErrBusy = errors.New("a refine is already running for this session")
)

// Result is what one round did.
type Result struct {
	RoundID  string
	Reviewed int
	Applied  []store.Refinement
	Proposed []store.Refinement
	Stale    []store.Refinement
	Failed   []store.Refinement
	Dropped  []string
	Signals  int
	Note     string
}

func (r *Refiner) acquire(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inFlight[id] {
		return false
	}
	r.inFlight[id] = true
	return true
}

func (r *Refiner) release(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.inFlight, id)
}

func newRoundID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Round reviews the session's messages after its watermark (up to through,
// when through > 0) and applies or proposes what the planner suggests.
func (r *Refiner) Round(ctx context.Context, sessionID string, trig Trigger, instructions string, through int) (Result, error) {
	sess, ok, err := r.Store.Session(ctx, sessionID)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{}, fmt.Errorf("no session %s", sessionID)
	}
	if sess.Source == store.SourceSubagent || sess.ParentID != "" {
		return Result{}, ErrSubagent
	}
	if !r.acquire(sessionID) {
		return Result{}, ErrBusy
	}
	defer r.release(sessionID)
	if err := r.Store.MarkRefineAttempt(ctx, sessionID); err != nil {
		return Result{}, err
	}

	from, err := r.Store.RefinedThrough(ctx, sessionID)
	if err != nil {
		return Result{}, err
	}
	rows, err := r.Store.Messages(ctx, sessionID)
	if err != nil {
		return Result{}, err
	}
	var pending []store.Message
	upto, hasUser := from, false
	for _, m := range rows {
		if m.Seq <= from || (through > 0 && m.Seq > through) {
			continue
		}
		upto = m.Seq
		if m.Role == store.RoleNote {
			continue
		}
		hasUser = hasUser || m.Role == string(provider.RoleUser)
		pending = append(pending, m)
	}
	if !hasUser {
		return Result{Note: "nothing new to refine since the last review"}, nil
	}
	transcript, err := Transcript(pending)
	if err != nil {
		return Result{}, err
	}
	snap, err := r.takeSnapshot(sess.Workspace)
	if err != nil {
		return Result{}, err
	}
	in := Input{
		Transcript: transcript, Facts: snap.facts, Notes: snap.notes,
		NotesPath: snap.notesPath, Instructions: instructions,
	}
	asked := r.Signals != nil && r.Signals.Enabled() && companion.Trusted(sess)
	if asked {
		known, err := r.Signals.Known(ctx)
		if err != nil {
			// A store fault costs the signals, not the round's edits.
			slog.Warn("refinement could not read known interests; skipping signals", "session", sessionID, "error", err)
			asked = false
		} else {
			in.AskSignals, in.Interests = true, known
		}
	}
	p, err := r.plan(ctx, in)
	if err != nil {
		return Result{}, err
	}
	if r.beforeApply != nil {
		r.beforeApply()
	}
	res := Result{RoundID: newRoundID(), Reviewed: len(pending)}
	r.apply(ctx, sess, trig, snap, p.Edits, &res)
	r.recordSignals(ctx, sess, p, asked, &res)
	if err := r.Store.SetRefinedThrough(ctx, sessionID, upto); err != nil {
		return res, err
	}
	res.Note = res.summary()
	return res, r.writeNote(ctx, sessionID, res.Note, p)
}

// recordSignals hands the planner's signals to the companion. It does nothing
// unless this round asked for signals, so a job, sub-agent or companion-off
// round reports nothing about them. Failures are logged and dropped: the edits
// already applied, and a lost sighting costs one count, not the round.
func (r *Refiner) recordSignals(ctx context.Context, sess store.Session, p Plan, asked bool, res *Result) {
	if !asked {
		return
	}
	for i := 0; i < p.BadSignals; i++ {
		res.Dropped = append(res.Dropped, "signal: not an object")
	}
	// Record runs even with no signals: the sweep fades interests the user
	// stopped mentioning, and this round did ask.
	rec, err := r.Signals.Record(ctx, sess, p.Signals)
	if err != nil {
		slog.Warn("refinement could not record interest signals", "session", sess.ID, "error", err)
	}
	res.Signals = rec.Recorded
	res.Dropped = append(res.Dropped, rec.Dropped...)
}

func shortTarget(row store.Refinement) string {
	if strings.HasPrefix(row.Kind, "fact.") {
		return row.Kind + " " + row.Target
	}
	return row.Kind + " " + filepath.Base(row.Target)
}

func list(rows []store.Refinement) string {
	parts := make([]string, len(rows))
	for i, row := range rows {
		parts[i] = shortTarget(row)
	}
	return strings.Join(parts, ", ")
}

// summary is the note line a round leaves in the session.
func (res Result) summary() string {
	var parts []string
	if n := len(res.Applied); n > 0 {
		parts = append(parts, fmt.Sprintf("%d applied (%s)", n, list(res.Applied)))
	}
	if n := res.Signals; n > 0 {
		word := "signals"
		if n == 1 {
			word = "signal"
		}
		parts = append(parts, fmt.Sprintf("%d interest %s", n, word))
	}
	if n := len(res.Proposed); n > 0 {
		parts = append(parts, fmt.Sprintf("%d proposed (%s) — review in :refinements", n, list(res.Proposed)))
	}
	if n := len(res.Stale); n > 0 {
		parts = append(parts, fmt.Sprintf("%d stale", n))
	}
	if n := len(res.Failed); n > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", n))
	}
	if n := len(res.Dropped); n > 0 {
		parts = append(parts, fmt.Sprintf("%d dropped", n))
	}
	if len(parts) == 0 {
		return "refined: no changes"
	}
	return "refined: " + strings.Join(parts, ", ")
}

// writeNote records the round in the transcript. The row carries the
// planner's usage so /usage counts refinement with everything else.
func (r *Refiner) writeNote(ctx context.Context, sessionID, text string, p Plan) error {
	blocks, err := json.Marshal([]provider.Block{{Type: provider.BlockText, Text: text}})
	if err != nil {
		return err
	}
	if _, err := r.Store.AppendMessage(ctx, store.Message{
		SessionID: sessionID, Role: store.RoleNote, BlocksJSON: blocks,
		Model: p.Model, CallSite: router.SiteRefinement,
		TokensIn: p.Usage.InputTokens, TokensOut: p.Usage.OutputTokens,
		TokensCacheWrite: p.Usage.CacheWriteTokens, TokensCacheRead: p.Usage.CacheReadTokens,
		CostUSD: p.Cost, Quiet: true,
	}); err != nil {
		return err
	}
	if r.Notify != nil {
		r.Notify(sessionID, text)
	}
	return nil
}
