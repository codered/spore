package companion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/scheduler"
	"github.com/codered/spore/internal/store"
)

// Proposal answers: the three buttons.
const (
	ChoiceYes    = "yes"
	ChoiceNotNow = "not-now"
	ChoiceNever  = "never"
)

// StopPause is how long "stop" holds outreach.
const StopPause = 7 * 24 * time.Hour

// ErrNotProposal is an answer to something that is not an open proposal:
// not a proposal at all, or one already answered.
var ErrNotProposal = errors.New("not an open proposal")

// Service is the companion's deterministic side: what the user's buttons,
// replies and commands do. Nothing here asks a model.
type Service struct {
	Store *store.Store
	Cfg   *config.Config
	// Now is the clock; tests replace it.
	Now func() time.Time
}

// NewService builds a Service on the wall clock.
func NewService(st *store.Store, cfg *config.Config) *Service {
	return &Service{Store: st, Cfg: cfg, Now: time.Now}
}

func (s *Service) start() int { return s.Cfg.Companion.StartBudget }

// AnswerProposal acts on a proposal's button. Yes creates the watch job --
// the only way an interest becomes active -- Not now cools the interest down
// for two weeks, and Never declines it. It returns a line to show the user.
func (s *Service) AnswerProposal(ctx context.Context, outreachID int64, choice string) (string, error) {
	switch choice {
	case ChoiceYes, ChoiceNotNow, ChoiceNever:
	default:
		return "", fmt.Errorf("choice must be yes, not-now or never, got %q", choice)
	}
	o, ok, err := s.Store.OutreachByID(ctx, outreachID)
	if err != nil {
		return "", err
	}
	if !ok || o.Kind != store.OutreachProposal || o.InterestID == 0 {
		return "", fmt.Errorf("%w: outreach %d", ErrNotProposal, outreachID)
	}
	if o.Outcome != store.OutcomePending && o.Outcome != store.OutcomeUnanswered {
		return "", fmt.Errorf("%w: outreach %d is %s", ErrNotProposal, outreachID, o.Outcome)
	}
	in, ok, err := s.Store.InterestByID(ctx, o.InterestID)
	if err != nil {
		return "", err
	}
	if !ok || in.State != store.InterestProposed {
		return "", fmt.Errorf("%w: outreach %d (its interest is %s)", ErrNotProposal, outreachID, in.State)
	}
	now := s.Now()
	var msg string
	switch choice {
	case ChoiceYes:
		if msg, err = s.startWatch(ctx, in, o, now); err != nil {
			return "", err
		}
	case ChoiceNotNow:
		moved, err := s.Store.SetInterestState(ctx, in.ID, store.InterestProposed, store.InterestObserving)
		if err != nil {
			return "", err
		}
		if !moved {
			return "", fmt.Errorf("%w: %s changed while answering", ErrNotProposal, in.Key)
		}
		if err := s.Store.SetInterestCooldown(ctx, in.ID, now.Add(NotNowCooldown)); err != nil {
			return "", err
		}
		msg = fmt.Sprintf("OK, not now. I won't bring up %s for two weeks.", in.Label)
	case ChoiceNever:
		moved, err := s.Store.DeclineInterest(ctx, in.ID)
		if err != nil {
			return "", err
		}
		if !moved {
			return "", fmt.Errorf("%w: %s changed while answering", ErrNotProposal, in.Key)
		}
		msg = fmt.Sprintf("Understood. I won't offer to watch %s again.", in.Label)
	}
	if _, err := Resolve(ctx, s.Store, s.start(), o.ID, []string{store.OutcomePending, store.OutcomeUnanswered},
		store.OutcomeReacted, "", now); err != nil {
		return "", err
	}
	return msg, nil
}

// startWatch creates the proposal's watch job and activates the interest.
func (s *Service) startWatch(ctx context.Context, in store.Interest, o store.Outreach, now time.Time) (string, error) {
	var w Watch
	if err := json.Unmarshal([]byte(o.Payload), &w); err != nil {
		return "", fmt.Errorf("proposal %d has no readable watch: %w", o.ID, err)
	}
	if err := ValidateWatch(w, now); err != nil {
		return "", err
	}
	sched, err := scheduler.Parse(w.Cron)
	if err != nil {
		return "", err
	}
	next := sched.Next(now.UTC())
	if next.IsZero() {
		return "", fmt.Errorf("watch cron %q never fires", w.Cron)
	}
	origin, _, err := s.Store.CompanionState(ctx, StateSessionID)
	if err != nil {
		return "", err
	}
	if origin == "" {
		return "", fmt.Errorf("%w: the companion conversation is gone; wait for its next message", ErrNotProposal)
	}
	if _, found, err := s.Store.Session(ctx, origin); err != nil {
		return "", err
	} else if !found {
		return "", fmt.Errorf("%w: the companion conversation is gone; wait for its next message", ErrNotProposal)
	}
	jobID, err := s.Store.CreateJob(ctx, store.Job{
		Kind: sched.Kind(), Spec: strings.TrimSpace(w.Cron), Prompt: WatchPrompt(in.Label, w.Goal), Enabled: true,
		NextRun: next, OriginSessionID: origin, Notify: store.NotifyEach, CheckedIn: true,
		Profile: config.CompanionProfile,
	})
	if err != nil {
		return "", err
	}
	moved, err := s.Store.ActivateInterest(ctx, in.ID, jobID)
	if err != nil || !moved {
		// Another answer won the race: the watch it created stands, this one
		// must not run beside it.
		_ = s.Store.SetJobEnabled(ctx, jobID, false)
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("%w: %s changed while answering", ErrNotProposal, in.Key)
	}
	return fmt.Sprintf("Watching %s (job %d, cron %s UTC). Every run will land here.", in.Label, jobID, w.Cron), nil
}

// IsCompanionSession reports whether sessionID is the companion's session.
func (s *Service) IsCompanionSession(ctx context.Context, sessionID string) (bool, error) {
	id, ok, err := s.Store.CompanionState(ctx, StateSessionID)
	if err != nil {
		return false, err
	}
	return ok && id != "" && id == sessionID, nil
}

// UserReplied records that the user wrote in the companion session: every
// pending or unanswered message there is answered.
func (s *Service) UserReplied(ctx context.Context, sessionID string) error {
	ok, err := s.IsCompanionSession(ctx, sessionID)
	if err != nil || !ok {
		return err
	}
	rows, err := s.Store.OutreachByOutcome(ctx, store.OutcomePending, store.OutcomeUnanswered)
	if err != nil {
		return err
	}
	now := s.Now()
	for _, o := range rows {
		if _, err := Resolve(ctx, s.Store, s.start(), o.ID, []string{store.OutcomePending, store.OutcomeUnanswered},
			store.OutcomeReplied, "", now); err != nil {
			return err
		}
	}
	return nil
}

// Feedback applies the user's "more", "less" or "stop" to the governor.
// "stop" also pauses outreach for StopPause.
func (s *Service) Feedback(ctx context.Context, kind string) (string, error) {
	if !slices.Contains(FeedbackKinds, kind) {
		return "", fmt.Errorf("feedback must be one of %s, got %q", strings.Join(FeedbackKinds, ", "), kind)
	}
	b, err := LoadBudget(ctx, s.Store, s.start())
	if err != nil {
		return "", err
	}
	nb := ApplyFeedback(b, kind, s.start())
	if err := SaveBudget(ctx, s.Store, nb); err != nil {
		return "", err
	}
	if kind == FeedbackStop {
		until := s.Now().Add(StopPause)
		if err := SetPausedUntil(ctx, s.Store, until); err != nil {
			return "", err
		}
		return fmt.Sprintf("Paused until %s. After that, at most %d unprompted messages a day.",
			until.Format("Mon Jan 2"), int(nb)), nil
	}
	return fmt.Sprintf("Noted: at most %d unprompted messages a day from now on.", int(nb)), nil
}

// Pause holds outreach for d.
func (s *Service) Pause(ctx context.Context, d time.Duration) (time.Time, error) {
	if d <= 0 {
		return time.Time{}, fmt.Errorf("pause must be positive, got %s", d)
	}
	until := s.Now().Add(d)
	return until, SetPausedUntil(ctx, s.Store, until)
}

// Resume ends a pause.
func (s *Service) Resume(ctx context.Context) error {
	return SetPausedUntil(ctx, s.Store, time.Time{})
}

// Undecline lets a declined interest be noticed again.
func (s *Service) Undecline(ctx context.Context, key string) error {
	key = strings.ToLower(strings.TrimSpace(key))
	in, ok, err := s.Store.InterestByKey(ctx, key)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no interest %q", key)
	}
	moved, err := s.Store.SetInterestState(ctx, in.ID, store.InterestDeclined, store.InterestObserving)
	if err != nil {
		return err
	}
	if !moved {
		return fmt.Errorf("%s is %s, not declined", key, in.State)
	}
	return nil
}

// UpdateInterest is interest_update: adjust an active watch's cron or goal,
// or retire an interest. It can never make an interest active; only the
// user's Yes on a proposal does that.
func (s *Service) UpdateInterest(ctx context.Context, key, cron, goal, state string) (string, error) {
	key = strings.ToLower(strings.TrimSpace(key))
	in, ok, err := s.Store.InterestByKey(ctx, key)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("no interest %q", key)
	}
	if state != "" {
		if state != store.InterestRetired {
			return "", fmt.Errorf("state may only be %q: only the user's Yes on a proposal starts a watch", store.InterestRetired)
		}
		if in.State == store.InterestRetired {
			return in.Label + " is already retired.", nil
		}
		if in.WatchJobID != 0 {
			if _, found, err := s.Store.Job(ctx, in.WatchJobID); err != nil {
				return "", err
			} else if found {
				if err := s.Store.SetJobEnabled(ctx, in.WatchJobID, false); err != nil {
					return "", err
				}
			}
		}
		moved, err := s.Store.SetInterestState(ctx, in.ID, in.State, store.InterestRetired)
		if err != nil {
			return "", err
		}
		if !moved {
			return "", fmt.Errorf("%s changed while retiring it; try again", key)
		}
		return fmt.Sprintf("Retired %s.", in.Label), nil
	}
	if cron == "" && goal == "" {
		return "", errors.New("nothing to change: give cron, goal or state")
	}
	if in.State != store.InterestActive || in.WatchJobID == 0 {
		return "", fmt.Errorf("%s is %s; only an active watch can be changed", key, in.State)
	}
	job, found, err := s.Store.Job(ctx, in.WatchJobID)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("the watch job for %s is gone", key)
	}
	now := s.Now()
	spec, prompt := job.Spec, job.Prompt
	if cron != "" {
		if err := ValidateCron(cron, now); err != nil {
			return "", err
		}
		spec = strings.TrimSpace(cron)
	}
	if goal != "" {
		if err := ValidateGoal(goal); err != nil {
			return "", err
		}
		prompt = WatchPrompt(in.Label, goal)
	}
	sched, err := scheduler.Parse(spec)
	if err != nil {
		return "", err
	}
	if err := s.Store.UpdateJobSchedule(ctx, job.ID, spec, prompt, sched.Next(now.UTC())); err != nil {
		return "", err
	}
	return fmt.Sprintf("Updated the %s watch (job %d, cron %s UTC).", in.Label, job.ID, spec), nil
}

// Status is what `spore companion status` shows.
type Status struct {
	Enabled        bool
	Channel        string
	PausedUntil    time.Time
	Budget         float64
	SpentToday     int
	LastReflection time.Time
	LastResult     string
	Failures       int
	SessionID      string
}

// Status reads the companion's state. It works with or without a daemon.
func (s *Service) Status(ctx context.Context) (Status, error) {
	c := s.Cfg.Companion
	st := Status{Enabled: c.Enabled, Channel: s.Cfg.CompanionChannel()}
	loc, err := c.Location()
	if err != nil {
		return Status{}, err
	}
	if st.PausedUntil, err = PausedUntil(ctx, s.Store); err != nil {
		return Status{}, err
	}
	if st.Budget, err = LoadBudget(ctx, s.Store, s.start()); err != nil {
		return Status{}, err
	}
	if st.SpentToday, err = SpentToday(ctx, s.Store, s.Now(), loc); err != nil {
		return Status{}, err
	}
	if st.LastReflection, err = StateTime(ctx, s.Store, StateLastReflection); err != nil {
		return Status{}, err
	}
	if st.LastResult, _, err = s.Store.CompanionState(ctx, StateLastResult); err != nil {
		return Status{}, err
	}
	if v, ok, err := s.Store.CompanionState(ctx, StateFailures); err != nil {
		return Status{}, err
	} else if ok {
		st.Failures, _ = strconv.Atoi(v)
	}
	if st.SessionID, _, err = s.Store.CompanionState(ctx, StateSessionID); err != nil {
		return Status{}, err
	}
	return st, nil
}
