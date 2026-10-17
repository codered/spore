package companion_test

import (
	"strings"
	"testing"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/store"
)

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		kind  string
		text  string
	}{
		{"alert with more lines", "ALERT: ZS fell 6%\nmore", store.VerdictAlert, "ZS fell 6%\nmore"},
		{"lower case nothing", "nothing: flat day", store.VerdictNothing, "flat day"},
		{"fenced nothing", "```\nNOTHING: x\n```", store.VerdictNothing, "x"},
		{"bold alert", "**ALERT:** big move", store.VerdictAlert, "big move"},
		{"heading alert", "## Alert: y", store.VerdictAlert, "y"},
		{"leading blank lines", "\n\n  NOTHING: z", store.VerdictNothing, "z"},
		{"no marker", "no marker here", store.VerdictAlert, "no marker here"},
		{"empty", "", store.VerdictAlert, ""},
		{"bold nothing before colon", "**NOTHING**: flat day", store.VerdictNothing, "flat day"},
		{"underscore alert before colon", "__ALERT__: x", store.VerdictAlert, "x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := companion.ParseVerdict(c.reply)
			if got.Kind != c.kind || got.Message != c.text {
				t.Fatalf("ParseVerdict(%q) = %+v, want {%s %q}", c.reply, got, c.kind, c.text)
			}
		})
	}
}

func TestAlertTextClips(t *testing.T) {
	long := strings.Repeat("a", 1300)
	want := strings.Repeat("a", 1200) + "\n\n· the rest is in the Jobs folder"
	if got := companion.AlertText(long); got != want {
		t.Fatalf("AlertText(1300 runes) = %d runes, want %d", len([]rune(got)), len([]rune(want)))
	}
	short := "ZS fell 6%"
	if got := companion.AlertText(short); got != short {
		t.Fatalf("AlertText(short) = %q, want unchanged", got)
	}
	exact := strings.Repeat("a", 1200)
	if got := companion.AlertText(exact); got != exact {
		t.Fatalf("AlertText(exactly 1200 runes) changed, want unchanged")
	}
	multi := strings.Repeat("é", 1300)
	wantMulti := strings.Repeat("é", 1200) + "\n\n· the rest is in the Jobs folder"
	if got := companion.AlertText(multi); got != wantMulti {
		t.Fatalf("AlertText(1300 multi-byte runes) = %d runes, want 1200 é plus footer", len([]rune(got)))
	}
}

func TestResearchPromptTrimsQuestion(t *testing.T) {
	got := companion.ResearchPrompt("ZS", "  what changed?  ")
	if !strings.Contains(got, "on ZS.") || !strings.Contains(got, "Question: what changed?\n") {
		t.Fatalf("prompt = %q", got)
	}
}
