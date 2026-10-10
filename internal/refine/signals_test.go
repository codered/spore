package refine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/store"
)

// withSignals attaches a Recorder to the fixture, companion on.
func withSignals(f *fix) *companion.Recorder {
	f.cfg.Companion.Enabled = true
	rec := companion.NewRecorder(f.st, f.cfg)
	rec.Now = func() time.Time { return time.Date(2026, 10, 10, 16, 0, 0, 0, time.UTC) }
	f.r.Signals = rec
	return rec
}

func TestParseReplyReadsSignalsAndCountsBadOnes(t *testing.T) {
	edits, sigs, bad, err := ParseReply(`{"edits":[],"signals":[{"key":"stock:zs","label":"ZS","kind":"asked"}, 42, {"key":"x"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) != 0 || len(sigs) != 2 || sigs[0].Key != "stock:zs" || bad != 1 {
		t.Fatalf("edits=%v sigs=%+v bad=%d", edits, sigs, bad)
	}
	// {"key":"x"} decodes as a Signal; Normalize rejects it later.
}

func TestParseReplyWithoutSignalsIsFine(t *testing.T) {
	_, sigs, bad, err := ParseReply(`{"edits":[]}`)
	if err != nil || sigs != nil || bad != 0 {
		t.Fatalf("sigs=%v bad=%d err=%v", sigs, bad, err)
	}
}

func TestRoundRecordsSignals(t *testing.T) {
	f := newFix(t, store.SourceChat, `{"edits":[],"signals":[{"key":"stock:zs","label":"Zscaler (ZS)","kind":"asked"}]}`)
	withSignals(f)
	f.say(t, "user", text("what's ZS at?"))
	res, err := f.r.Round(context.Background(), f.sid, TriggerManual, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Signals != 1 || !strings.Contains(res.Note, "1 interest signal") {
		t.Fatalf("res = %+v", res)
	}
	in, ok, _ := f.st.InterestByKey(context.Background(), "stock:zs")
	if !ok || in.DaysSeen != 1 {
		t.Fatalf("interest = %+v ok=%v", in, ok)
	}
	sys := f.script.Requests()[0].System[0].Text
	if !strings.Contains(sys, `"signals"`) {
		t.Fatal("the planner was not asked for signals")
	}
}

func TestRoundKeepsEditsWhenSignalsAreMalformed(t *testing.T) {
	f := newFix(t, store.SourceChat,
		`{"edits":[{"kind":"fact.create","name":"likes-tea","type":"user","description":"tea","body":"Likes tea.","rationale":"user said so"}],
		  "signals":[7, {"key":"weather:x","label":"x","kind":"asked"}, {"key":"topic:tea","label":"Tea","kind":"loved"}]}`)
	withSignals(f)
	f.say(t, "user", text("I like tea"))
	res, err := f.r.Round(context.Background(), f.sid, TriggerManual, "", 0)
	if err != nil {
		t.Fatalf("malformed signals failed the round: %v", err)
	}
	if len(res.Applied) != 1 || res.Signals != 0 || len(res.Dropped) != 3 {
		t.Fatalf("applied=%d signals=%d dropped=%q", len(res.Applied), res.Signals, res.Dropped)
	}
}

func TestRoundKeepsEditsWhenSignalsIsNotAnArray(t *testing.T) {
	f := newFix(t, store.SourceChat,
		`{"edits":[{"kind":"fact.create","name":"likes-tea","type":"user","description":"tea","body":"Likes tea.","rationale":"user said so"}],"signals":"none"}`)
	withSignals(f)
	f.say(t, "user", text("I like tea"))
	res, err := f.r.Round(context.Background(), f.sid, TriggerManual, "", 0)
	if err != nil {
		t.Fatalf("a non-array signals field failed the round: %v", err)
	}
	if len(res.Applied) != 1 || res.Signals != 0 || len(res.Dropped) != 1 {
		t.Fatalf("applied=%d signals=%d dropped=%q", len(res.Applied), res.Signals, res.Dropped)
	}
}

func TestRoundShowsKnownInterestsToThePlanner(t *testing.T) {
	f := newFix(t, store.SourceChat, `{"edits":[]}`)
	withSignals(f)
	_, _ = f.st.TouchInterest(context.Background(), "stock:zs", "Zscaler (ZS)")
	f.say(t, "user", text("hi"))
	if _, err := f.r.Round(context.Background(), f.sid, TriggerManual, "", 0); err != nil {
		t.Fatal(err)
	}
	user := f.script.Requests()[0].Messages[0].Blocks[0].Text
	if !strings.Contains(user, "stock:zs: Zscaler (ZS)") {
		t.Fatalf("known interests missing from planner input:\n%s", user)
	}
}

func TestRoundOnJobSessionDoesNotAskForSignals(t *testing.T) {
	f := newFix(t, store.SourceJob, `{"edits":[],"signals":[{"key":"stock:zs","label":"ZS","kind":"asked"}, 7]}`)
	withSignals(f)
	f.say(t, "user", text("check ZS"))
	res, err := f.r.Round(context.Background(), f.sid, TriggerManual, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.script.Requests()[0].System[0].Text, `"signals"`) {
		t.Error("a job session's planner was asked for signals")
	}
	if res.Signals != 0 {
		t.Errorf("a job session recorded %d signals", res.Signals)
	}
	for _, d := range res.Dropped {
		if strings.Contains(strings.ToLower(d), "signal") {
			t.Errorf("a job session's round reported a signal: %q", d)
		}
	}
	if strings.Contains(res.Note, "signal") {
		t.Errorf("a job session's round note mentions signals: %q", res.Note)
	}
	if all, _ := f.st.Interests(context.Background()); len(all) != 0 {
		t.Errorf("a job session created interests: %+v", all)
	}
}

func TestRoundWithCompanionOffDoesNotAskForSignals(t *testing.T) {
	f := newFix(t, store.SourceChat, `{"edits":[]}`)
	rec := withSignals(f)
	rec.Cfg.Companion.Enabled = false
	f.say(t, "user", text("hi"))
	if _, err := f.r.Round(context.Background(), f.sid, TriggerManual, "", 0); err != nil {
		t.Fatal(err)
	}
	req := f.script.Requests()[0]
	if strings.Contains(req.System[0].Text, `"signals"`) || strings.Contains(req.Messages[0].Blocks[0].Text, "Interests already") {
		t.Fatal("companion off, but the planner prompt changed")
	}
}

func TestRoundKeepsEditsWhenKnownFails(t *testing.T) {
	f := newFix(t, store.SourceChat,
		`{"edits":[{"kind":"fact.create","name":"likes-tea","type":"user","description":"tea","body":"Likes tea.","rationale":"user said so"}],"signals":[{"key":"topic:tea","label":"Tea","kind":"asked"}]}`)
	rec := withSignals(f)
	// A recorder whose store is closed: every Known call fails.
	broken, err := store.Open(filepath.Join(t.TempDir(), "broken.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := broken.Close(); err != nil {
		t.Fatal(err)
	}
	rec.Store = broken
	f.say(t, "user", text("I like tea"))
	res, err := f.r.Round(context.Background(), f.sid, TriggerManual, "", 0)
	if err != nil {
		t.Fatalf("a Known failure aborted the round: %v", err)
	}
	if len(res.Applied) != 1 {
		t.Fatalf("applied=%d, want the valid edit to land", len(res.Applied))
	}
	if res.Signals != 0 {
		t.Fatalf("signals=%d, want none recorded when Known failed", res.Signals)
	}
	req := f.script.Requests()[0]
	if strings.Contains(req.System[0].Text, `"signals"`) || strings.Contains(req.Messages[0].Blocks[0].Text, "Interests already") {
		t.Fatal("Known failed, but the planner prompt still has a signals section")
	}
}
