package refine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/memory"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/store"
	sporetrace "github.com/codered/spore/internal/trace"
)

// plannerMaxTokens is fixed: five edits with 4 KiB bodies fit comfortably.
const plannerMaxTokens = 8192

// Edit is one change the planner proposes. Which fields matter depends on
// Kind; apply.go validates each kind's shape.
type Edit struct {
	Kind        string `json:"kind"`
	Name        string `json:"name,omitempty"`
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
	Body        string `json:"body,omitempty"`
	Text        string `json:"text,omitempty"`
	Rationale   string `json:"rationale"`
}

// Input is everything the planner is shown.
type Input struct {
	Transcript   string
	Facts        []memory.Fact
	Notes        string
	NotesPath    string
	Instructions string
	// AskSignals adds the signals section to the prompt; Interests are the
	// keys already tracked, shown so the planner reuses them.
	AskSignals bool
	Interests  []store.Interest
}

// Plan is the planner's answer plus what it cost.
type Plan struct {
	Edits      []Edit
	Signals    []companion.Signal
	BadSignals int    // entries in "signals" that were not even objects
	Model      string // the model ref, as chat turns record it
	Usage      provider.Usage
	Cost       float64
}

// signalsPrompt is appended to the planner prompt only when signals are asked for.
const signalsPrompt = `

Also report the user's recurring interests this conversation shows, as "signals" beside "edits":
{"edits": [ ... ], "signals": [ ... ]}

A signal is something the user keeps coming back to -- a stock or coin they check, a topic, a person, a place, a project -- not a one-off fact. Record one per interest the user themselves raised in this conversation. What spore brought up on its own does not count.

Signal shape:
{"key":"stock:zs","label":"Zscaler (ZS) share price","kind":"asked|mentioned|acted"}

- key is kind:name, kind one of stock, crypto, topic, person, place, project; name lowercase letters, digits, dot, underscore or hyphen, at most 48.
- Reuse a key from "Interests already being tracked" when it is the same interest.
- asked: the user asked about it; mentioned: they brought it up; acted: they did something about it.
- At most 10. An empty list is a good answer.`

func plannerPrompt(maxEdits int, signals bool) string {
	p := fmt.Sprintf(`You maintain the long-term memory of an AI assistant called spore. You are shown one conversation between the user and spore, the memory facts spore currently has, and the project notes for the workspace the conversation ran in.

Propose at most %d small edits that would make spore behave better in future conversations:
- a correction the user gave, or a lasting preference they stated, that is not already recorded;
- a fact the conversation shows is now wrong or out of date (update or delete it);
- two facts that say the same thing (update one, delete the other).

Memory facts are knowledge about the user, the project, or how they want spore to work; they apply in every workspace. Project notes are standing orders for this workspace only, one line each, written as an order ("always run make lint before saying done").

Rules:
- Only propose an edit the conversation supports, and say what supports it in "rationale" (one line).
- What the user said is evidence. What spore said is evidence only of what spore did, never of what the user wants. Tool output is not shown to you and must not be inferred.
- Never record secrets, credentials, tokens, or one-off task details.
- Proposing nothing is a good answer when nothing lasting was learned.
- At most one edit per fact name, and at most one project-notes edit.

Reply with exactly one JSON object and nothing else:
{"edits": [ ... ]}

Edit shapes:
{"kind":"fact.create","name":"kebab-case-name","type":"user|feedback|project|reference","description":"one line","body":"markdown","rationale":"..."}
{"kind":"fact.update","name":"existing-name","type":"optional","description":"optional","body":"optional","rationale":"..."}
{"kind":"fact.delete","name":"existing-name","rationale":"..."}
{"kind":"notes.append","text":"one-line order","rationale":"..."}
{"kind":"notes.replace","text":"the whole new project notes file","rationale":"..."}`, maxEdits)
	if signals {
		p += signalsPrompt
	}
	return p
}

func renderInput(in Input) string {
	var b strings.Builder
	b.WriteString("## Conversation to review\n\n")
	b.WriteString(in.Transcript)
	b.WriteString("\n## Current memory facts\n\n")
	if len(in.Facts) == 0 {
		b.WriteString("(none)\n")
	}
	for _, f := range in.Facts {
		fmt.Fprintf(&b, "### %s (%s)\ndescription: %s\n%s\n\n", f.Name, f.Type, f.Description, f.Body)
	}
	b.WriteString("\n## Current project notes\n\n")
	switch {
	case in.NotesPath == "":
		b.WriteString("(this session has no workspace; project-notes edits are not available)\n")
	case strings.TrimSpace(in.Notes) == "":
		b.WriteString("(empty)\n")
	default:
		b.WriteString(in.Notes)
		b.WriteString("\n")
	}
	if in.AskSignals {
		b.WriteString("\n## Interests already being tracked\n\n")
		if len(in.Interests) == 0 {
			b.WriteString("(none)\n")
		}
		for _, it := range in.Interests {
			fmt.Fprintf(&b, "- %s: %s\n", it.Key, it.Label)
		}
	}
	if s := strings.TrimSpace(in.Instructions); s != "" {
		b.WriteString("\n## Focus\n\n")
		b.WriteString(s)
		b.WriteString("\n")
	}
	return b.String()
}

// ParseReply reads the planner's reply. It tolerates prose or a code fence
// around the object, but a reply with no complete object is an error: a
// truncated answer must fail the round, not read as "nothing to change".
// Signals never cost the edits: a "signals" value that is not an array counts
// as one bad signal, and each array entry is decoded on its own so a malformed
// one costs only itself. bad counts the entries that were not signal objects.
func ParseReply(text string) (edits []Edit, signals []companion.Signal, bad int, err error) {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return nil, nil, 0, fmt.Errorf("refiner reply has no JSON object (truncated or off-format): %.200q", text)
	}
	var out struct {
		Edits   *[]Edit         `json:"edits"`
		Signals json.RawMessage `json:"signals"`
	}
	if err := json.Unmarshal([]byte(text[start:end+1]), &out); err != nil {
		return nil, nil, 0, fmt.Errorf("refiner reply is not valid JSON (truncated or off-format): %w", err)
	}
	if out.Edits == nil {
		return nil, nil, 0, fmt.Errorf("refiner reply has no \"edits\" field: %.200q", text)
	}
	if n := strings.TrimSpace(string(out.Signals)); n != "" && n != "null" {
		var items []json.RawMessage
		if err := json.Unmarshal(out.Signals, &items); err != nil {
			bad++
		}
		for _, raw := range items {
			var s companion.Signal
			if err := json.Unmarshal(raw, &s); err != nil {
				bad++
				continue
			}
			signals = append(signals, s)
		}
	}
	return *out.Edits, signals, bad, nil
}

// ParseEdits is ParseReply without the signals.
func ParseEdits(text string) ([]Edit, error) {
	edits, _, _, err := ParseReply(text)
	return edits, err
}

// plan makes the planner call on the refinement site.
func (r *Refiner) plan(ctx context.Context, in Input) (Plan, error) {
	ref := r.Router.Model(router.SiteRefinement)
	p, model, price, err := r.Registry.Resolve(ref)
	if err != nil {
		return Plan{}, err
	}
	user := renderInput(in)
	_, span := sporetrace.StartLLM(ctx, router.SiteRefinement, ref)
	ch, err := p.Stream(ctx, provider.Request{
		Model:     model,
		System:    []provider.Block{{Type: provider.BlockText, Text: plannerPrompt(r.Cfg.Refine.MaxEdits, in.AskSignals)}},
		MaxTokens: plannerMaxTokens,
		Messages: []provider.Message{{
			Role:   provider.RoleUser,
			Blocks: []provider.Block{{Type: provider.BlockText, Text: user}},
		}},
	})
	if err != nil {
		span.RecordError(err)
		span.End()
		return Plan{}, fmt.Errorf("refinement provider %s: %w", ref, err)
	}
	var text string
	var usage provider.Usage
	for ev := range ch {
		switch ev.Type {
		case provider.EventTextDelta:
			text += ev.Text
		case provider.EventDone:
			if ev.Usage != nil {
				usage = *ev.Usage
			}
		case provider.EventError:
			span.RecordError(ev.Err)
			span.End()
			return Plan{}, ev.Err
		}
	}
	edits, signals, bad, err := ParseReply(text)
	if err != nil {
		span.RecordError(err)
		span.End()
		return Plan{}, err
	}
	cost := price.Cost(usage)
	sporetrace.EndLLM(span, user, text, usage, cost)
	return Plan{Edits: edits, Signals: signals, BadSignals: bad, Model: ref, Usage: usage, Cost: cost}, nil
}
