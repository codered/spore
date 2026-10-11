package companion

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/codered/spore/internal/store"
)

// Reflection actions. PR 2a acts on these three; alerts, findings,
// follow-ups, the digest and research arrive in PR 3.
const (
	ActionPropose = "propose"
	ActionNudge   = "nudge"
	ActionNone    = "none"
)

// Actions is every action a reflection may return.
var Actions = []string{ActionPropose, ActionNudge, ActionNone}

// Drop is an unanswered message the reflection chose not to follow up, and
// why. Dropping is a decision written down, never a timeout.
type Drop struct {
	Outreach int64  `json:"outreach"`
	Reason   string `json:"reason"`
}

// Decision is one reflection's answer. Nothing in it is trusted until
// Validate has checked it against the View it was made from.
type Decision struct {
	Action   string `json:"action"`
	Interest string `json:"interest"`
	Message  string `json:"message"`
	Watch    *Watch `json:"watch"`
	// SelfMD, when set, replaces self.md whole.
	SelfMD  *string `json:"self_md"`
	NudgeOf int64   `json:"nudge_of"`
	Drops   []Drop  `json:"drops"`
}

// View is what a reflection sees, and what its decision is checked against.
type View struct {
	Now time.Time
	Loc *time.Location
	// Soul is soul.md, the user's file; Self is self.md, spore's own.
	Soul string
	Self string
	// SelfReadable is false when self.md exists but could not be read: the
	// reflection may not rewrite a file it could not see.
	SelfReadable bool
	SelfMax      int
	// Interests are the candidate, proposed and active ones.
	Interests []store.Interest
	// Days lists each interest's sighting days; Keys names every interest
	// by id, so an outreach about a retired one still shows its key.
	Days map[int64][]string
	Keys map[int64]string
	// Recent is the last week's outreach; Unanswered is every unanswered
	// row, however old; Nudged marks the unanswered rows already nudged.
	Recent     []store.Outreach
	Unanswered []store.Outreach
	Nudged     map[int64]bool
	// BudgetLeft is how many more messages may start today.
	BudgetLeft int
	// Force names an interest the operator's check requires a proposal
	// for; empty on every heartbeat tick.
	Force string
}

// Interest finds a shown interest by key.
func (v View) Interest(key string) (store.Interest, bool) {
	for _, in := range v.Interests {
		if in.Key == key {
			return in, true
		}
	}
	return store.Interest{}, false
}

// UnansweredByID finds a shown unanswered outreach by id.
func (v View) UnansweredByID(id int64) (store.Outreach, bool) {
	for _, o := range v.Unanswered {
		if o.ID == id {
			return o, true
		}
	}
	return store.Outreach{}, false
}

// ParseDecision reads a reflection reply. It tolerates prose or a code fence
// around one JSON object; a reply with no complete object is an error.
func ParseDecision(text string) (Decision, error) {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return Decision{}, fmt.Errorf("reflection reply has no JSON object: %.200q", text)
	}
	var d Decision
	if err := json.Unmarshal([]byte(text[start:end+1]), &d); err != nil {
		return Decision{}, fmt.Errorf("reflection reply is not valid JSON: %w", err)
	}
	d.Action = strings.TrimSpace(d.Action)
	d.Interest = strings.ToLower(strings.TrimSpace(d.Interest))
	d.Message = strings.TrimSpace(d.Message)
	return d, nil
}

// Validate checks a decision against the view it was made from. Any error
// skips the tick: nothing is sent and nothing is written.
func Validate(d Decision, v View) error {
	switch d.Action {
	case ActionNone:
	case ActionPropose:
		in, ok := v.Interest(d.Interest)
		if !ok {
			return fmt.Errorf("propose: no interest %q", d.Interest)
		}
		if in.State != store.InterestCandidate {
			return fmt.Errorf("propose: %s is %s, not candidate", in.Key, in.State)
		}
		if v.Now.Before(in.CooldownUntil) {
			return fmt.Errorf("propose: %s is cooling down until %s", in.Key, in.CooldownUntil.Format(time.RFC3339))
		}
		if d.Watch == nil {
			return errors.New("propose needs a watch")
		}
		if err := ValidateWatch(*d.Watch, v.Now); err != nil {
			return err
		}
	case ActionNudge:
		if d.NudgeOf == 0 {
			return errors.New("nudge needs nudge_of")
		}
		if _, ok := v.UnansweredByID(d.NudgeOf); !ok {
			return fmt.Errorf("nudge_of %d is not an unanswered outreach", d.NudgeOf)
		}
		if v.Nudged[d.NudgeOf] {
			return fmt.Errorf("outreach %d already has a nudge", d.NudgeOf)
		}
	default:
		return fmt.Errorf("action %q is not one of %s", d.Action, strings.Join(Actions, ", "))
	}
	if d.Action != ActionNone {
		if n := utf8.RuneCountInString(d.Message); n < 1 || n > 1200 {
			return fmt.Errorf("message must be 1..1200 characters, got %d", n)
		}
		if v.BudgetLeft < 1 {
			return errors.New("no budget left today")
		}
	}
	for _, dr := range d.Drops {
		if _, ok := v.UnansweredByID(dr.Outreach); !ok {
			return fmt.Errorf("drop %d is not an unanswered outreach", dr.Outreach)
		}
		if d.Action == ActionNudge && dr.Outreach == d.NudgeOf {
			return fmt.Errorf("outreach %d is both nudged and dropped", dr.Outreach)
		}
		if n := utf8.RuneCountInString(strings.TrimSpace(dr.Reason)); n < 1 || n > 200 {
			return fmt.Errorf("drop %d: reason must be 1..200 characters, got %d", dr.Outreach, n)
		}
	}
	if d.SelfMD != nil {
		if !v.SelfReadable {
			return errors.New("self.md is unavailable this tick and may not be rewritten")
		}
		if len(*d.SelfMD) > v.SelfMax {
			return fmt.Errorf("self_md is %d bytes, over companion.self_max_bytes %d", len(*d.SelfMD), v.SelfMax)
		}
		for _, h := range SelfHeadings {
			if !hasHeading(*d.SelfMD, h) {
				return fmt.Errorf("self_md is missing the heading %q", h)
			}
		}
	}
	return nil
}

func hasHeading(body, h string) bool {
	for _, l := range strings.Split(body, "\n") {
		if strings.TrimSpace(l) == "## "+h {
			return true
		}
	}
	return false
}

// Eligible reports whether a reflection could act on anything: a candidate
// out of its cooldown, or an unanswered message. Nothing eligible means no
// call is made.
func Eligible(v View) bool {
	if len(v.Unanswered) > 0 {
		return true
	}
	for _, in := range v.Interests {
		if in.State == store.InterestCandidate && !v.Now.Before(in.CooldownUntil) {
			return true
		}
	}
	return false
}

// Fingerprint is everything a reflection's answer depends on that changes
// between ticks: the local day, each candidate's evidence and cooldown, the
// unanswered rows and their nudges, and the week's outcomes. An unchanged
// fingerprint means the last answer still stands, so no call is made.
func Fingerprint(v View) string {
	lines := []string{"day " + v.Now.In(v.Loc).Format("2006-01-02")}
	for _, in := range v.Interests {
		if in.State == store.InterestCandidate {
			lines = append(lines, fmt.Sprintf("c %d %d %t", in.ID, len(v.Days[in.ID]), v.Now.Before(in.CooldownUntil)))
		}
	}
	for _, o := range v.Unanswered {
		lines = append(lines, fmt.Sprintf("u %d %t", o.ID, v.Nudged[o.ID]))
	}
	for _, o := range v.Recent {
		lines = append(lines, fmt.Sprintf("o %d %s", o.ID, o.Outcome))
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// ReflectionPrompt is the system prompt of a heartbeat reflection.
const ReflectionPrompt = `You are spore's companion reflection. Spore is a personal assistant that lives on one person's machine. Between conversations you look at what spore has noticed about the user and decide whether to start a conversation.

You may:
- propose watching a candidate interest: a habit the user has shown on several days. Only a candidate that is not cooling down may be proposed. The user answers with buttons: Yes starts the watch, Not now, or Never. Write a short, specific message that says what you noticed, and give a watch: a five-field cron in UTC that fires at most hourly (daily or on weekdays is usual), and a goal saying what to check and when it is worth telling them.
- nudge an unanswered message: one short follow-up that adds something (new information, a simpler question, or an easy way to say no). Never repeat the original. Each message gets at most one nudge.
- do nothing ("none"). This is the usual answer. Starting a conversation costs the user's attention; only do it when it is worth it.

Every unanswered message you are shown needs a decision eventually: nudge it, drop it in "drops" with a short reason (for example, the moment has passed), or leave it out to decide on a later tick.

You may also rewrite your own notes in "self_md": the full new self.md, keeping its four headings. Leave it null unless something is worth recording. "Who you are" is the user's file and wins over your notes.

Reply with one JSON object and nothing else:
{"action": "propose" | "nudge" | "none",
 "interest": "<interest key, for propose>",
 "message": "<what to send, 1..1200 characters; empty for none>",
 "watch": {"cron": "<five-field cron, UTC>", "goal": "<what to check, 1..500 characters>"} or null,
 "nudge_of": <outreach id, for nudge>,
 "drops": [{"outreach": <id>, "reason": "<1..200 characters>"}],
 "self_md": null or "<the full new self.md>"}`

// RenderInput is the user message of a reflection: the state Validate will
// check the answer against, in the order the prompt explains it.
func RenderInput(v View) string {
	var b strings.Builder
	local := v.Now.In(v.Loc)
	fmt.Fprintf(&b, "Local time: %s (%s). UTC: %s.\n", local.Format("Mon 2006-01-02 15:04"), v.Loc, v.Now.UTC().Format("15:04"))
	fmt.Fprintf(&b, "Messages you may still start today: %d.\n\n", v.BudgetLeft)

	b.WriteString("## Who you are (soul.md)\n")
	b.WriteString(orNone(v.Soul))
	b.WriteString("\n\n## Your own notes (self.md)\n")
	switch {
	case !v.SelfReadable:
		b.WriteString("(unavailable this tick: do not rewrite it)")
	default:
		b.WriteString(orNone(v.Self))
	}

	b.WriteString("\n\n## Interests\n")
	if len(v.Interests) == 0 {
		b.WriteString("(none)\n")
	}
	for _, in := range v.Interests {
		fmt.Fprintf(&b, "- %s %q: %s", in.Key, in.Label, in.State)
		if in.State == store.InterestCandidate && v.Now.Before(in.CooldownUntil) {
			fmt.Fprintf(&b, " (cooling down until %s; do not propose)", in.CooldownUntil.In(v.Loc).Format("2006-01-02"))
		}
		if days := v.Days[in.ID]; len(days) > 0 {
			fmt.Fprintf(&b, "; seen on %s", strings.Join(days, ", "))
		}
		b.WriteString("\n")
	}

	b.WriteString("\n## Messages you started in the last 7 days\n")
	if len(v.Recent) == 0 {
		b.WriteString("(none)\n")
	}
	for _, o := range v.Recent {
		about := ""
		if k := v.Keys[o.InterestID]; k != "" {
			about = " about " + k
		}
		fmt.Fprintf(&b, "- #%d %s%s, %s, sent %s: %q\n", o.ID, o.Kind, about, o.Outcome,
			o.SentAt.In(v.Loc).Format("Mon 15:04"), clip(o.Text, 300))
	}

	b.WriteString("\n## Unanswered: nudge, drop, or leave for later\n")
	if len(v.Unanswered) == 0 {
		b.WriteString("(none)\n")
	}
	for _, o := range v.Unanswered {
		note := ""
		if v.Nudged[o.ID] {
			note = " (already nudged: drop it or leave it)"
		}
		fmt.Fprintf(&b, "- #%d %s, sent %dh ago%s: %q\n", o.ID, o.Kind, int(v.Now.Sub(o.SentAt).Hours()), note, clip(o.Text, 300))
	}
	if v.Force != "" {
		fmt.Fprintf(&b, "\n## Operator check\nThis is a verification run started by the user: answer with action \"propose\" for interest %s, whatever its evidence.\n", v.Force)
	}
	return b.String()
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(empty)"
	}
	return strings.TrimSpace(s)
}

// clip shortens s to at most n runes, marking the cut.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
