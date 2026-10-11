// Package companiontool holds the two tools of the companion conversation:
// interest_update, which adjusts or retires a watch, and companion_feedback,
// which tells the governor "more", "less" or "stop". They exist only while
// the companion is on, and act only in the companion's own session: a chat
// about something else must not retire a watch by accident.
package companiontool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/tool"
)

// Service is the slice of companion.Service the tools use.
type Service interface {
	UpdateInterest(ctx context.Context, key, cron, goal, state string) (string, error)
	Feedback(ctx context.Context, kind string) (string, error)
	IsCompanionSession(ctx context.Context, sessionID string) (bool, error)
}

// New builds the tools; none while the companion is off.
func New(cfg *config.Config, svc Service) []tool.Tool {
	if !cfg.Companion.Enabled || svc == nil {
		return nil
	}
	return []tool.Tool{interestUpdate{svc}, feedback{svc}}
}

var errElsewhere = errors.New("this tool only works in the companion conversation, where spore reaches out to the user")

// inCompanion refuses a call from any session but the companion's.
func inCompanion(ctx context.Context, svc Service) error {
	ok, err := svc.IsCompanionSession(ctx, policy.SessionFrom(ctx).ID)
	if err != nil {
		return err
	}
	if !ok {
		return errElsewhere
	}
	return nil
}

func decode(args json.RawMessage, v any) error {
	if len(args) == 0 {
		return fmt.Errorf("no arguments supplied")
	}
	if err := json.Unmarshal(args, v); err != nil {
		return fmt.Errorf("bad arguments: %w", err)
	}
	return nil
}

type interestUpdate struct{ svc Service }

func (interestUpdate) Name() string { return "interest_update" }

func (interestUpdate) Description() string {
	return "Change an active watch the user approved -- its cron (five fields, UTC, at most hourly) or its goal -- " +
		"or retire an interest (state \"retired\") when the user wants it stopped. It cannot start a watch: " +
		"only the user's Yes button on a proposal does that."
}

func (interestUpdate) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "key": {"type": "string", "description": "The interest key, e.g. stock:zs."},
	    "cron": {"type": "string", "description": "New five-field cron, UTC."},
	    "goal": {"type": "string", "description": "New goal: what to check and when it is worth telling the user."},
	    "state": {"type": "string", "enum": ["retired"]}
	  },
	  "required": ["key"]
	}`)
}

// ReadOnly is false: it changes a job and an interest.
func (interestUpdate) ReadOnly() bool { return false }

func (t interestUpdate) Call(ctx context.Context, args json.RawMessage) (string, error) {
	if err := inCompanion(ctx, t.svc); err != nil {
		return "", err
	}
	var in struct {
		Key   string `json:"key"`
		Cron  string `json:"cron"`
		Goal  string `json:"goal"`
		State string `json:"state"`
	}
	if err := decode(args, &in); err != nil {
		return "", err
	}
	return t.svc.UpdateInterest(ctx, in.Key, in.Cron, in.Goal, in.State)
}

type feedback struct{ svc Service }

func (feedback) Name() string { return "companion_feedback" }

func (feedback) Description() string {
	return "Record how the user wants to hear from you unprompted: \"more\", \"less\", or \"stop\" " +
		"(which also pauses your messages for a week). Use it when the user says so, in any words."
}

func (feedback) Schema() json.RawMessage {
	enum, _ := json.Marshal(companion.FeedbackKinds)
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {"kind": {"type": "string", "enum": ` + string(enum) + `}},
	  "required": ["kind"]
	}`)
}

// ReadOnly is false: it changes the governor.
func (feedback) ReadOnly() bool { return false }

func (t feedback) Call(ctx context.Context, args json.RawMessage) (string, error) {
	if err := inCompanion(ctx, t.svc); err != nil {
		return "", err
	}
	var in struct {
		Kind string `json:"kind"`
	}
	if err := decode(args, &in); err != nil {
		return "", err
	}
	return t.svc.Feedback(ctx, in.Kind)
}
