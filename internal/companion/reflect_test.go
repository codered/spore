package companion_test

import (
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
