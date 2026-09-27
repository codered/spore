package refine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/codered/spore/internal/memory"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/router"
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
}

// Plan is the planner's answer plus what it cost.
type Plan struct {
	Edits []Edit
	Model string // the model ref, as chat turns record it
	Usage provider.Usage
	Cost  float64
}

func plannerPrompt(maxEdits int) string {
	return fmt.Sprintf(`You maintain the long-term memory of an AI assistant called spore. You are shown one conversation between the user and spore, the memory facts spore currently has, and the project notes for the workspace the conversation ran in.

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
	if s := strings.TrimSpace(in.Instructions); s != "" {
		b.WriteString("\n## Focus\n\n")
		b.WriteString(s)
		b.WriteString("\n")
	}
	return b.String()
}

// ParseEdits reads the planner's reply. It tolerates prose or a code fence
// around the object, but a reply with no complete object is an error: a
// truncated answer must fail the round, not read as "nothing to change".
func ParseEdits(text string) ([]Edit, error) {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return nil, fmt.Errorf("refiner reply has no JSON object (truncated or off-format): %.200q", text)
	}
	var out struct {
		Edits *[]Edit `json:"edits"`
	}
	if err := json.Unmarshal([]byte(text[start:end+1]), &out); err != nil {
		return nil, fmt.Errorf("refiner reply is not valid JSON (truncated or off-format): %w", err)
	}
	if out.Edits == nil {
		return nil, fmt.Errorf("refiner reply has no \"edits\" field: %.200q", text)
	}
	return *out.Edits, nil
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
		System:    []provider.Block{{Type: provider.BlockText, Text: plannerPrompt(r.Cfg.Refine.MaxEdits)}},
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
	edits, err := ParseEdits(text)
	if err != nil {
		span.RecordError(err)
		span.End()
		return Plan{}, err
	}
	cost := price.Cost(usage)
	sporetrace.EndLLM(span, user, text, usage, cost)
	return Plan{Edits: edits, Model: ref, Usage: usage, Cost: cost}, nil
}
