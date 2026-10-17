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

// Reflection actions. A reflection may start a conversation (propose, nudge,
// finding, followup, digest), start a research run that sends nothing, or
// do nothing.
const (
	ActionPropose  = "propose"
	ActionNudge    = "nudge"
	ActionResearch = "research"
	ActionFinding  = "finding"
	ActionFollowup = "followup"
	ActionDigest   = "digest"
	ActionNone     = "none"
)

// Actions is every action a reflection may return.
var Actions = []string{ActionPropose, ActionNudge, ActionResearch, ActionFinding, ActionFollowup, ActionDigest, ActionNone}

// Limits of the reflection's input and its research runs.
const (
	// WatchClip and ResearchClip bound the reply of a run shown in the input.
	WatchClip    = 1024
	ResearchClip = 6144
	// ResearchQuestionMax is the longest research question, in runes.
	ResearchQuestionMax = 300
	// ResearchPerDay is how many research runs may start in one day.
	ResearchPerDay = 6
)

// IDList is a list of outreach ids. JSON may give null, one number or an
// array of numbers, so a reply that names one id needs no array.
type IDList []int64

// UnmarshalJSON accepts null, a number, or an array of numbers.
func (l *IDList) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		*l = nil
		return nil
	}
	if strings.HasPrefix(s, "[") {
		var ids []int64
		if err := json.Unmarshal(b, &ids); err != nil {
			return fmt.Errorf("want a list of ids: %w", err)
		}
		*l = ids
		return nil
	}
	var id int64
	if err := json.Unmarshal(b, &id); err != nil {
		return fmt.Errorf("want an id or a list of ids: %w", err)
	}
	*l = IDList{id}
	return nil
}

// hasID reports whether ids names id.
func hasID(ids IDList, id int64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// Drop is an unanswered message the reflection chose not to follow up, and
// why. Dropping is a decision written down, never a timeout.
type Drop struct {
	Outreach int64  `json:"outreach"`
	Reason   string `json:"reason"`
}

// ResearchAsk is the question a research run looks into.
type ResearchAsk struct {
	Question string `json:"question"`
}

// Decision is one reflection's answer. Nothing in it is trusted until
// Validate has checked it against the View it was made from.
type Decision struct {
	Action   string       `json:"action"`
	Interest string       `json:"interest"`
	Message  string       `json:"message"`
	Watch    *Watch       `json:"watch"`
	Research *ResearchAsk `json:"research"`
	// Runs names the runs a finding reports, or a digest covers.
	Runs []int64 `json:"runs"`
	// SelfMD, when set, replaces self.md whole.
	SelfMD *string `json:"self_md"`
	// NudgeOf is the unanswered outreach a nudge follows up, or the ones a
	// digest folds in.
	NudgeOf IDList `json:"nudge_of"`
	Drops   []Drop `json:"drops"`
}

// RunView is one watch or research run the reflection may see.
type RunView struct {
	Run   store.CompanionRun
	Key   string // interest key
	Reply string // full reply, or "(run deleted)"
	Held  bool   // watch alert with no reported_outreach_id
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
	// Runs are the finished runs the reflection may report on.
	Runs []RunView
	// Followups are the follow-up subjects the reflection may raise.
	Followups []string
	// DigestDue is true in the morning digest slot. Then the digest is the
	// only action the reflection may take.
	DigestDue bool
	// ResearchToday counts the research runs started today;
	// ResearchInFlight is true while one is still running.
	ResearchToday    int
	ResearchInFlight bool
	// BudgetLeft is how many more messages may start today.
	BudgetLeft int
	// Force names an interest the operator's check requires a proposal
	// for; empty on every heartbeat tick.
	Force string
	// ForceResearch names an interest the operator's check requires a
	// research run for; empty on every heartbeat tick.
	ForceResearch string
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

// RunByID finds a shown run by id.
func (v View) RunByID(id int64) (RunView, bool) {
	for _, r := range v.Runs {
		if r.Run.ID == id {
			return r, true
		}
	}
	return RunView{}, false
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
	if v.DigestDue && d.Action != ActionDigest {
		return errors.New("the digest is due: the only valid action is digest")
	}
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
		if len(d.NudgeOf) != 1 {
			return fmt.Errorf("nudge needs one nudge_of, got %d", len(d.NudgeOf))
		}
		id := d.NudgeOf[0]
		o, ok := v.UnansweredByID(id)
		if !ok {
			return fmt.Errorf("nudge_of %d is not an unanswered outreach", id)
		}
		if o.Kind == store.OutreachDigest {
			return fmt.Errorf("outreach %d is a digest: a digest is never nudged", id)
		}
		if v.Nudged[id] {
			return fmt.Errorf("outreach %d already has a nudge", id)
		}
	case ActionResearch:
		in, ok := v.Interest(d.Interest)
		if !ok {
			return fmt.Errorf("research: no interest %q", d.Interest)
		}
		if in.State != store.InterestActive {
			return fmt.Errorf("research: %s is %s, not active", in.Key, in.State)
		}
		if d.Research == nil {
			return errors.New("research needs a question")
		}
		if n := utf8.RuneCountInString(strings.TrimSpace(d.Research.Question)); n < 1 || n > ResearchQuestionMax {
			return fmt.Errorf("research question must be 1..%d characters, got %d", ResearchQuestionMax, n)
		}
		if v.ResearchInFlight {
			return errors.New("research: a run is already in flight")
		}
		if v.ResearchToday >= ResearchPerDay {
			return fmt.Errorf("research: %d runs started today, at most %d", v.ResearchToday, ResearchPerDay)
		}
		if d.Message != "" {
			return errors.New("research sends no message")
		}
	case ActionFinding:
		if len(d.Runs) == 0 {
			return errors.New("finding needs runs")
		}
		for _, id := range d.Runs {
			if _, ok := v.RunByID(id); !ok {
				return fmt.Errorf("finding: run %d is not shown", id)
			}
		}
		if d.Interest != "" {
			if _, ok := v.Interest(d.Interest); !ok {
				return fmt.Errorf("finding: no interest %q", d.Interest)
			}
		}
	case ActionFollowup:
		if d.Interest != "" {
			if _, ok := v.Interest(d.Interest); !ok {
				return fmt.Errorf("followup: no interest %q", d.Interest)
			}
		}
	case ActionDigest:
		if !v.DigestDue {
			return errors.New("the digest is not due this tick")
		}
		for _, id := range d.Runs {
			if _, ok := v.RunByID(id); !ok {
				return fmt.Errorf("digest: run %d is not shown", id)
			}
		}
		for _, id := range d.NudgeOf {
			o, ok := v.UnansweredByID(id)
			if !ok {
				return fmt.Errorf("digest: nudge_of %d is not an unanswered outreach", id)
			}
			if o.Kind == store.OutreachDigest {
				return fmt.Errorf("digest: outreach %d is a digest and is never folded in", id)
			}
			if v.Nudged[id] {
				return fmt.Errorf("digest: outreach %d already has a nudge", id)
			}
		}
	default:
		return fmt.Errorf("action %q is not one of %s", d.Action, strings.Join(Actions, ", "))
	}
	if d.Action != ActionNone && d.Action != ActionResearch {
		if n := utf8.RuneCountInString(d.Message); n < 1 || n > 1200 {
			return fmt.Errorf("message must be 1..1200 characters, got %d", n)
		}
	}
	switch d.Action {
	case ActionPropose, ActionNudge, ActionFinding, ActionFollowup:
		if v.BudgetLeft < 1 {
			return errors.New("no budget left today")
		}
	}
	for _, dr := range d.Drops {
		if _, ok := v.UnansweredByID(dr.Outreach); !ok {
			return fmt.Errorf("drop %d is not an unanswered outreach", dr.Outreach)
		}
		if (d.Action == ActionNudge || d.Action == ActionDigest) && hasID(d.NudgeOf, dr.Outreach) {
			return fmt.Errorf("outreach %d is both nudged or folded in and dropped", dr.Outreach)
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
// out of its cooldown, an unanswered message, a run the user has not seen or
// a held alert, a follow-up, the digest slot, or an operator's check. Nothing
// eligible means no call is made.
func Eligible(v View) bool {
	if len(v.Unanswered) > 0 || len(v.Followups) > 0 || v.DigestDue || v.ForceResearch != "" {
		return true
	}
	for _, r := range v.Runs {
		if r.Run.SeenAt.IsZero() || r.Held {
			return true
		}
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
// unanswered rows and their nudges, the runs the user has not seen, the
// held alerts, the follow-ups, the digest slot, and the week's outcomes. An
// unchanged fingerprint means the last answer still stands, so no call is
// made.
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
	for _, r := range v.Runs {
		if r.Run.SeenAt.IsZero() {
			lines = append(lines, fmt.Sprintf("r %d %t", r.Run.ID, r.Held))
		}
		if r.Held {
			lines = append(lines, fmt.Sprintf("h %d", r.Run.ID))
		}
	}
	for _, s := range v.Followups {
		lines = append(lines, "f "+s)
	}
	lines = append(lines, fmt.Sprintf("digest %t", v.DigestDue))
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
- nudge an unanswered message: one short follow-up that adds something (new information, a simpler question, or an easy way to say no). Never repeat the original. Each message gets at most one nudge. A digest is never nudged.
- research an active interest: ask one question (1..300 characters) that a run of its own answers with read-only tools and sources. It sends the user nothing; you see its answer under "Runs since you last looked" on a later tick. Only one research run at a time, and at most 6 a day.
- report a finding: tell the user what one or more runs found that is worth their attention, and list the run ids in "runs".
- follow up: raise a follow-up subject you are shown, or a thread from your notes that has come due.
- write the digest: only when the input says the digest is due, and then it is the only action you may take. One short morning message: what the runs found since the last digest, anything unanswered folded in (list those ids in "nudge_of"), and what is coming. Send it even when it is short.
- do nothing ("none"). This is the usual answer. Starting a conversation costs the user's attention; only do it when it is worth it.

Every unanswered message you are shown needs a decision eventually: nudge it, drop it in "drops" with a short reason (for example, the moment has passed), or leave it out to decide on a later tick.

Watch runs send their own alerts; you see every run the user has not been told about. "Messages you may still start today" counts proposals, nudges, findings and follow-ups; research and the digest do not.

You may also rewrite your own notes in "self_md": the full new self.md, keeping its four headings. Leave it null unless something is worth recording. "Who you are" is the user's file and wins over your notes.

Reply with one JSON object and nothing else:
{"action": "propose" | "nudge" | "research" | "finding" | "followup" | "digest" | "none",
 "interest": "<interest key: for propose and research; optional for finding and followup>",
 "message": "<what to send, 1..1200 characters; empty for research and none>",
 "watch": {"cron": "<five-field cron, UTC>", "goal": "<what to check, 1..500 characters>"} or null,
 "research": {"question": "<1..300 characters>"} or null,
 "runs": [<run ids, for finding and digest>],
 "nudge_of": <outreach id, for nudge> or [<outreach ids>, for digest],
 "drops": [{"outreach": <id>, "reason": "<1..200 characters>"}],
 "self_md": null or "<the full new self.md>"}`

// RenderInput is the user message of a reflection: the state Validate will
// check the answer against, in the order the prompt explains it.
func RenderInput(v View) string {
	var b strings.Builder
	local := v.Now.In(v.Loc)
	fmt.Fprintf(&b, "Local time: %s (%s). UTC: %s.\n", local.Format("Mon 2006-01-02 15:04"), v.Loc, v.Now.UTC().Format("15:04"))
	fmt.Fprintf(&b, "Messages you may still start today: %d.\n", v.BudgetLeft)
	fmt.Fprintf(&b, "Research today: %d of %d", v.ResearchToday, ResearchPerDay)
	if v.ResearchInFlight {
		b.WriteString("; one is running")
	}
	b.WriteString(".\n\n")

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
		switch {
		case v.Nudged[o.ID]:
			note = " (already nudged: drop it or leave it)"
		case o.Kind == store.OutreachDigest:
			note = " (a digest: never nudge it)"
		}
		fmt.Fprintf(&b, "- #%d %s, sent %dh ago%s: %q\n", o.ID, o.Kind, int(v.Now.Sub(o.SentAt).Hours()), note, clip(o.Text, 300))
	}

	b.WriteString("\n## Runs since you last looked\n")
	if len(v.Runs) == 0 {
		b.WriteString("(none)\n")
	}
	for _, r := range v.Runs {
		fmt.Fprintf(&b, "- run #%d %s %s: %s, %dh ago", r.Run.ID, r.Run.Kind, r.Key, r.Run.Verdict,
			int(v.Now.Sub(r.Run.FinishedAt).Hours()))
		if r.Held {
			b.WriteString(" (held: not sent)")
		}
		if !r.Run.SeenAt.IsZero() {
			b.WriteString(" (seen before)")
		}
		if r.Run.Question != "" {
			fmt.Fprintf(&b, ", question: %q", r.Run.Question)
		}
		b.WriteString("\n")
		limit := WatchClip
		if r.Run.Kind == store.RunResearch {
			limit = ResearchClip
		}
		reply := clipRun(r.Reply, limit, r.Run.SessionID)
		b.WriteString("  " + strings.ReplaceAll(reply, "\n", "\n  ") + "\n")
	}

	b.WriteString("\n## Follow-up subjects\n")
	if len(v.Followups) == 0 {
		b.WriteString("(none)\n")
	}
	for _, s := range v.Followups {
		fmt.Fprintf(&b, "- %s\n", s)
	}
	if v.DigestDue {
		b.WriteString("\n## The digest is due\nThis tick is the morning digest slot: answer with action \"digest\" and nothing else.\n")
	}
	if v.Force != "" {
		fmt.Fprintf(&b, "\n## Operator check\nThis is a verification run started by the user: answer with action \"propose\" for interest %s, whatever its evidence.\n", v.Force)
	}
	if v.ForceResearch != "" {
		fmt.Fprintf(&b, "\n## Operator check\nThis is a verification run started by the user: answer with action \"research\" for interest %s, with a question of your choosing.\n", v.ForceResearch)
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

// clipRun shortens a run's reply to at most n runes. A cut reply names the
// job run that holds the whole of it.
func clipRun(s string, n int, session string) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " (clipped; full reply in job run " + session + ")"
}
