package companion_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/store"
)

func reflectView() companion.View {
	now := time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)
	return companion.View{
		Now: now, Loc: time.UTC, SelfReadable: true, SelfMax: 10240, BudgetLeft: 2,
		Interests: []store.Interest{
			{ID: 1, Key: "stock:zs", Label: "Zscaler (ZS)", State: store.InterestCandidate},
			{ID: 2, Key: "topic:go", Label: "Go", State: store.InterestCandidate, CooldownUntil: now.Add(48 * time.Hour)},
			{ID: 3, Key: "stock:net", Label: "NET", State: store.InterestActive},
		},
		Days:       map[int64][]string{1: {"2026-10-09", "2026-10-10", "2026-10-11"}},
		Keys:       map[int64]string{1: "stock:zs", 2: "topic:go", 3: "stock:net"},
		Unanswered: []store.Outreach{{ID: 10, Kind: store.OutreachProposal, Text: "watch NET?", SentAt: now.Add(-13 * time.Hour), Outcome: store.OutcomeUnanswered}, {ID: 11, Kind: store.OutreachProposal, Text: "old", SentAt: now.Add(-30 * time.Hour), Outcome: store.OutcomeUnanswered}},
		Nudged:     map[int64]bool{11: true},
	}
}

const goodPropose = `{"action":"propose","interest":"stock:zs","message":"You've checked ZS three days running. Want me to watch it?","watch":{"cron":"0 14 * * 1-5","goal":"Check ZS; alert on a move over 5%."}}`

func TestParseDecision(t *testing.T) {
	d, err := companion.ParseDecision("Here you go:\n```json\n" + goodPropose + "\n```")
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != companion.ActionPropose || d.Interest != "stock:zs" || d.Watch == nil || d.Watch.Cron != "0 14 * * 1-5" {
		t.Fatalf("decision = %+v", d)
	}
	if _, err := companion.ParseDecision("I think we should wait."); err == nil {
		t.Fatal("prose with no object must be an error")
	}
	if _, err := companion.ParseDecision(`{"action": "none"`); err == nil {
		t.Fatal("a truncated object must be an error")
	}
}

func TestValidateDecision(t *testing.T) {
	selfOK := "## What I'm curious about\n\n## Threads with you\n\n## How you like to be talked to\n\n## Opinions I've formed\n"
	selfMissing := "## What I'm curious about\n"
	for _, tc := range []struct {
		name string
		json string
		edit func(*companion.View)
		want string // "" means valid
	}{
		{"propose", goodPropose, nil, ""},
		{"none", `{"action":"none"}`, nil, ""},
		{"none with no budget", `{"action":"none"}`, func(v *companion.View) { v.BudgetLeft = 0 }, ""},
		{"nudge", `{"action":"nudge","nudge_of":10,"message":"Still want that NET watch? Yes or no is fine."}`, nil, ""},
		{"drop", `{"action":"none","drops":[{"outreach":10,"reason":"the earnings moment passed"}]}`, nil, ""},
		{"self rewrite", `{"action":"none","self_md":` + quote(selfOK) + `}`, nil, ""},
		{"unknown action", `{"action":"alert","interest":"stock:zs","message":"down 6%"}`, nil, "action"},
		{"unknown interest", strings.Replace(goodPropose, "stock:zs", "stock:nope", 1), nil, "no interest"},
		{"not candidate", strings.Replace(goodPropose, "stock:zs", "stock:net", 1), nil, "not candidate"},
		{"cooling down", strings.Replace(goodPropose, "stock:zs", "topic:go", 1), nil, "cooling down"},
		{"no watch", `{"action":"propose","interest":"stock:zs","message":"hi"}`, nil, "watch"},
		{"bad cron", strings.Replace(goodPropose, "0 14 * * 1-5", "*/10 * * * *", 1), nil, "more than once an hour"},
		{"empty message", `{"action":"nudge","nudge_of":10,"message":" "}`, nil, "message"},
		{"long message", `{"action":"nudge","nudge_of":10,"message":"` + strings.Repeat("x", 1201) + `"}`, nil, "message"},
		{"no budget", goodPropose, func(v *companion.View) { v.BudgetLeft = 0 }, "budget"},
		{"nudge of nothing", `{"action":"nudge","nudge_of":99,"message":"hi"}`, nil, "not an unanswered"},
		{"second nudge", `{"action":"nudge","nudge_of":11,"message":"hi again"}`, nil, "already has a nudge"},
		{"nudge without id", `{"action":"nudge","message":"hi"}`, nil, "nudge_of"},
		{"drop of nothing", `{"action":"none","drops":[{"outreach":99,"reason":"x"}]}`, nil, "not an unanswered"},
		{"drop without reason", `{"action":"none","drops":[{"outreach":10,"reason":""}]}`, nil, "reason"},
		{"nudged and dropped", `{"action":"nudge","nudge_of":10,"message":"hi","drops":[{"outreach":10,"reason":"x"}]}`, nil, "both"},
		{"self missing heading", `{"action":"none","self_md":` + quote(selfMissing) + `}`, nil, "heading"},
		{"self too large", `{"action":"none","self_md":` + quote(selfOK) + `}`, func(v *companion.View) { v.SelfMax = 20 }, "self_max_bytes"},
		{"self unreadable", `{"action":"none","self_md":` + quote(selfOK) + `}`, func(v *companion.View) { v.SelfReadable = false }, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := reflectView()
			if tc.edit != nil {
				tc.edit(&v)
			}
			d, err := companion.ParseDecision(tc.json)
			if err != nil {
				t.Fatal(err)
			}
			err = companion.Validate(d, v)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("valid decision refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func quote(s string) string {
	b := strings.ReplaceAll(s, "\n", `\n`)
	return `"` + strings.ReplaceAll(b, `"`, `\"`) + `"`
}

func TestRenderInputAndFingerprint(t *testing.T) {
	v := reflectView()
	in := companion.RenderInput(v)
	for _, want := range []string{"stock:zs", "candidate", "2026-10-09, 2026-10-10, 2026-10-11", "cooling down", "#10", "already nudged", "Messages you may still start today: 2"} {
		if !strings.Contains(in, want) {
			t.Errorf("input lacks %q:\n%s", want, in)
		}
	}
	if !companion.Eligible(v) {
		t.Fatal("a candidate and two unanswered rows are eligible")
	}
	fp := companion.Fingerprint(v)
	if fp != companion.Fingerprint(reflectView()) {
		t.Fatal("the fingerprint must be deterministic")
	}
	v2 := reflectView()
	v2.Days[1] = append(v2.Days[1], "2026-10-12")
	if companion.Fingerprint(v2) == fp {
		t.Fatal("new evidence for a candidate must change the fingerprint")
	}
	v3 := reflectView()
	v3.Now = v3.Now.Add(24 * time.Hour)
	if companion.Fingerprint(v3) == fp {
		t.Fatal("a new local day must change the fingerprint")
	}
	empty := companion.View{Now: v.Now, Loc: time.UTC, Interests: []store.Interest{{ID: 2, Key: "topic:go", State: store.InterestCandidate, CooldownUntil: v.Now.Add(time.Hour)}}}
	if companion.Eligible(empty) {
		t.Fatal("only a cooling-down candidate is not eligible")
	}
}

func TestIDListAcceptsNumberOrArray(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []int64
	}{
		{"number", `{"ids":5}`, []int64{5}},
		{"array", `{"ids":[5,6]}`, []int64{5, 6}},
		{"null", `{"ids":null}`, nil},
		{"absent", `{}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got struct {
				IDs companion.IDList `json:"ids"`
			}
			if err := json.Unmarshal([]byte(tc.body), &got); err != nil {
				t.Fatal(err)
			}
			if len(got.IDs) != len(tc.want) {
				t.Fatalf("ids = %v, want %v", got.IDs, tc.want)
			}
			for i := range tc.want {
				if got.IDs[i] != tc.want[i] {
					t.Fatalf("ids = %v, want %v", got.IDs, tc.want)
				}
			}
		})
	}
	var bad struct {
		IDs companion.IDList `json:"ids"`
	}
	if err := json.Unmarshal([]byte(`{"ids":"five"}`), &bad); err == nil {
		t.Fatal("a string is not an id list")
	}
}

// pr3View is reflectView with one run, a follow-up and the digest slot
// open, for the rules PR 3 adds.
func pr3View() companion.View {
	v := reflectView()
	v.Runs = []companion.RunView{{Run: store.CompanionRun{ID: 7, Kind: store.RunWatch, FinishedAt: v.Now.Add(-time.Hour)}, Key: "stock:net", Reply: "moved"}}
	v.Followups = []string{"the Go question"}
	v.ResearchToday = 2
	return v
}

func TestValidatePR3Actions(t *testing.T) {
	long := strings.Repeat("q", 301)
	for _, tc := range []struct {
		name string
		json string
		edit func(*companion.View)
		want string // "" means valid
	}{
		{"digest due, propose refused", goodPropose, func(v *companion.View) { v.DigestDue = true }, "digest is due"},
		{"digest due, digest allowed", `{"action":"digest","message":"Morning."}`, func(v *companion.View) { v.DigestDue = true }, ""},
		{"digest outside its slot", `{"action":"digest","message":"Morning."}`, nil, "not due"},
		{"digest with no budget", `{"action":"digest","message":"Morning."}`, func(v *companion.View) { v.DigestDue = true; v.BudgetLeft = 0 }, ""},
		{"digest folds an unanswered row", `{"action":"digest","message":"Morning.","nudge_of":[10]}`, func(v *companion.View) { v.DigestDue = true }, ""},
		{"digest folds a nudged row", `{"action":"digest","message":"Morning.","nudge_of":[11]}`, func(v *companion.View) { v.DigestDue = true }, "already has a nudge"},
		{"digest folds a digest", `{"action":"digest","message":"Morning.","nudge_of":[10]}`, func(v *companion.View) {
			v.DigestDue = true
			v.Unanswered[0].Kind = store.OutreachDigest
		}, "is a digest"},
		{"digest with a shown run", `{"action":"digest","message":"Morning.","runs":[7]}`, func(v *companion.View) { v.DigestDue = true }, ""},
		{"digest with an unknown run", `{"action":"digest","message":"Morning.","runs":[8]}`, func(v *companion.View) { v.DigestDue = true }, "run 8"},
		{"digest with an empty message", `{"action":"digest","message":" "}`, func(v *companion.View) { v.DigestDue = true }, "message"},
		{"nudge a digest", `{"action":"nudge","nudge_of":10,"message":"hi"}`, func(v *companion.View) { v.Unanswered[0].Kind = store.OutreachDigest }, "digest"},
		{"nudge of two ids", `{"action":"nudge","nudge_of":[10,11],"message":"hi"}`, nil, "nudge_of"},
		{"research", `{"action":"research","interest":"stock:net","research":{"question":"What moved NET this week?"}}`, nil, ""},
		{"research with no budget", `{"action":"research","interest":"stock:net","research":{"question":"What moved NET?"}}`, func(v *companion.View) { v.BudgetLeft = 0 }, ""},
		{"research on a candidate", `{"action":"research","interest":"stock:zs","research":{"question":"What moved ZS?"}}`, nil, "not active"},
		{"research of an unknown interest", `{"action":"research","interest":"stock:nope","research":{"question":"What?"}}`, nil, "no interest"},
		{"research with no question", `{"action":"research","interest":"stock:net","research":{"question":"  "}}`, nil, "question"},
		{"research without the research object", `{"action":"research","interest":"stock:net"}`, nil, "question"},
		{"research question too long", `{"action":"research","interest":"stock:net","research":{"question":"` + long + `"}}`, nil, "1..300"},
		{"research with a message", `{"action":"research","interest":"stock:net","message":"hi","research":{"question":"What?"}}`, nil, "no message"},
		{"research while one is in flight", `{"action":"research","interest":"stock:net","research":{"question":"What?"}}`, func(v *companion.View) { v.ResearchInFlight = true }, "in flight"},
		{"research seventh of the day", `{"action":"research","interest":"stock:net","research":{"question":"What?"}}`, func(v *companion.View) { v.ResearchToday = 6 }, "at most"},
		{"finding of a shown run", `{"action":"finding","runs":[7],"message":"NET moved."}`, nil, ""},
		{"finding with no runs", `{"action":"finding","message":"NET moved."}`, nil, "runs"},
		{"finding of an unknown run", `{"action":"finding","runs":[8],"message":"NET moved."}`, nil, "run 8"},
		{"finding with an unknown interest", `{"action":"finding","runs":[7],"interest":"stock:nope","message":"NET moved."}`, nil, "no interest"},
		{"finding with budget 0", `{"action":"finding","runs":[7],"message":"NET moved."}`, func(v *companion.View) { v.BudgetLeft = 0 }, "budget"},
		{"followup", `{"action":"followup","message":"Back to the Go question?"}`, nil, ""},
		{"followup with an unknown interest", `{"action":"followup","interest":"stock:nope","message":"Back?"}`, nil, "no interest"},
		{"followup with budget 0", `{"action":"followup","message":"Back?"}`, func(v *companion.View) { v.BudgetLeft = 0 }, "budget"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := pr3View()
			if tc.edit != nil {
				tc.edit(&v)
			}
			d, err := companion.ParseDecision(tc.json)
			if err != nil {
				t.Fatal(err)
			}
			err = companion.Validate(d, v)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("valid decision refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestRenderInputRuns(t *testing.T) {
	v := reflectView()
	watchReply := strings.Repeat("w", 2000)
	researchReply := strings.Repeat("r", 7000)
	v.Runs = []companion.RunView{
		{Run: store.CompanionRun{ID: 7, Kind: store.RunWatch, Verdict: store.VerdictAlert, SessionID: "sess-w", FinishedAt: v.Now.Add(-5 * time.Hour)}, Key: "stock:net", Reply: watchReply, Held: true},
		{Run: store.CompanionRun{ID: 8, Kind: store.RunResearch, Verdict: store.VerdictDone, SessionID: "sess-r", Question: "What is ZS guidance?", FinishedAt: v.Now.Add(-5 * time.Hour)}, Key: "stock:zs", Reply: researchReply},
		{Run: store.CompanionRun{ID: 9, Kind: store.RunWatch, Verdict: store.VerdictNothing, FinishedAt: v.Now.Add(-time.Hour), SeenAt: v.Now}, Key: "topic:go", Reply: "(run deleted)"},
	}
	in := companion.RenderInput(v)
	for _, want := range []string{
		"## Runs since you last looked\n",
		"- run #7 watch stock:net: alert, 5h ago (held: not sent)\n  " + strings.Repeat("w", 1024) + " (clipped; full reply in job run sess-w)",
		"- run #8 research stock:zs: done, 5h ago, question: \"What is ZS guidance?\"\n  " + strings.Repeat("r", 6144) + " (clipped; full reply in job run sess-r)",
		"- run #9 watch topic:go: nothing, 1h ago (seen before)\n  (run deleted)",
	} {
		if !strings.Contains(in, want) {
			t.Errorf("input lacks %q", want)
		}
	}
	for _, bad := range []string{strings.Repeat("w", 1025), strings.Repeat("r", 6145), "…"} {
		if strings.Contains(in, bad) {
			t.Errorf("input must not contain the unclipped text or the old marker (%d runes)", len([]rune(bad)))
		}
	}
	if !strings.Contains(companion.RenderInput(reflectView()), "## Runs since you last looked\n(none)\n") {
		t.Error("no runs must say (none)")
	}
}

func TestRenderInputDigestAndOperatorResearch(t *testing.T) {
	v := pr3View()
	v.DigestDue = true
	v.ResearchInFlight = true
	in := companion.RenderInput(v)
	for _, want := range []string{
		"Research today: 2 of 6; one is running.",
		"## Follow-up subjects\n- the Go question\n",
		"## The digest is due\nThis tick is the morning digest slot: answer with action \"digest\" and nothing else.",
	} {
		if !strings.Contains(in, want) {
			t.Errorf("input lacks %q:\n%s", want, in)
		}
	}
	if !strings.Contains(companion.RenderInput(reflectView()), "## Follow-up subjects\n(none)\n") {
		t.Error("no follow-ups must say (none)")
	}
	if !strings.Contains(companion.RenderInput(reflectView()), "Research today: 0 of 6.") {
		t.Error("the research line must show the count with nothing running")
	}

	d := reflectView()
	d.Unanswered[0].Kind = store.OutreachDigest
	if !strings.Contains(companion.RenderInput(d), "(a digest: never nudge it)") {
		t.Error("an unanswered digest must say it is never nudged")
	}

	op := reflectView()
	op.ForceResearch = "stock:net"
	if !strings.Contains(companion.RenderInput(op), "## Operator check\nThis is a verification run started by the user: answer with action \"research\" for interest stock:net") {
		t.Error("an operator research check must ask for a research action")
	}
}

func TestEligibleAndFingerprintSeeRuns(t *testing.T) {
	now := time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)
	base := companion.View{Now: now, Loc: time.UTC}
	run := companion.RunView{Run: store.CompanionRun{ID: 7, FinishedAt: now.Add(-time.Hour)}, Key: "stock:net", Reply: "x"}

	unseen := base
	unseen.Runs = []companion.RunView{run}
	seen := base
	seenRun := run
	seenRun.Run.SeenAt = now
	seen.Runs = []companion.RunView{seenRun}
	if !companion.Eligible(unseen) {
		t.Fatal("an unseen run is eligible")
	}
	if companion.Eligible(seen) {
		t.Fatal("a seen, unheld run alone is not eligible")
	}
	if companion.Fingerprint(unseen) == companion.Fingerprint(seen) || companion.Fingerprint(unseen) == companion.Fingerprint(base) {
		t.Fatal("an unseen run must change the fingerprint")
	}

	held := base
	heldRun := seenRun
	heldRun.Held = true
	held.Runs = []companion.RunView{heldRun}
	if !companion.Eligible(held) || companion.Fingerprint(held) == companion.Fingerprint(seen) {
		t.Fatal("a held run is eligible and changes the fingerprint")
	}

	for name, v := range map[string]companion.View{
		"follow-up": {Now: now, Loc: time.UTC, Followups: []string{"the Go question"}},
		"digest":    {Now: now, Loc: time.UTC, DigestDue: true},
		"operator":  {Now: now, Loc: time.UTC, ForceResearch: "stock:net"},
	} {
		if !companion.Eligible(v) {
			t.Errorf("%s must make the view eligible", name)
		}
	}
}
