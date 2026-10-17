package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Outreach kinds: what the companion said on its own. PR 2a sends proposals
// and nudges; the others arrive with research and the digest.
const (
	OutreachProposal = "proposal"
	OutreachNudge    = "nudge"
	OutreachAlert    = "alert"
	OutreachFinding  = "finding"
	OutreachFollowup = "followup"
	OutreachDigest   = "digest"
)

// Outreach outcomes. See the companion spec, section 2.3: a message nobody
// answered is unanswered, never ignored, until reflection decides.
const (
	OutcomePending    = "pending"
	OutcomeUnanswered = "unanswered"
	OutcomeReplied    = "replied"
	OutcomeReacted    = "reacted"
	OutcomeIgnored    = "ignored"
	OutcomeStop       = "stop"
)

// Outreach channels: where a message was delivered. Fallback is the
// terminal after a Discord send failed.
const (
	OutreachChannelDiscord  = "discord"
	OutreachChannelTerminal = "terminal"
	OutreachChannelFallback = "terminal-fallback"
	OutreachChannelDigest   = "digest"
)

// Outreach is one message the companion started.
type Outreach struct {
	ID int64
	// InterestID is the interest it was about, or 0.
	InterestID int64
	Kind       string
	Channel    string
	Text       string
	// Payload is JSON the kind needs later: a proposal's watch.
	Payload string
	SentAt  time.Time
	Outcome string
	// NudgeOf is the outreach a nudge follows up, or 0.
	NudgeOf    int64
	DropReason string
	ResolvedAt time.Time
}

const outreachColumns = `id, COALESCE(interest_id, 0), kind, channel, text, payload, sent_at, outcome,
       COALESCE(nudge_of, 0), drop_reason, resolved_at`

func scanOutreach(r rowScanner) (Outreach, error) {
	var o Outreach
	var sent, resolved string
	if err := r.Scan(&o.ID, &o.InterestID, &o.Kind, &o.Channel, &o.Text, &o.Payload, &sent, &o.Outcome,
		&o.NudgeOf, &o.DropReason, &resolved); err != nil {
		return Outreach{}, err
	}
	o.SentAt, _ = time.Parse(timeFormat, sent)
	o.ResolvedAt, _ = time.Parse(timeFormat, resolved)
	return o, nil
}

// nullID stores 0 as NULL, so an outreach about no interest carries no
// foreign key to break.
func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// AddOutreach records a message as sent and pending.
func (s *Store) AddOutreach(ctx context.Context, o Outreach) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO outreach (interest_id, kind, channel, text, payload, sent_at, outcome, nudge_of)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		nullID(o.InterestID), o.Kind, o.Channel, o.Text, o.Payload, o.SentAt.UTC().Format(timeFormat),
		OutcomePending, nullID(o.NudgeOf))
	if err != nil {
		return 0, fmt.Errorf("add outreach: %w", err)
	}
	return res.LastInsertId()
}

// SetOutreachChannel records where a message was finally delivered.
func (s *Store) SetOutreachChannel(ctx context.Context, id int64, channel string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE outreach SET channel = ? WHERE id = ?`, channel, id); err != nil {
		return fmt.Errorf("set outreach %d channel: %w", id, err)
	}
	return nil
}

// OutreachByID reads one outreach; false when there is none.
func (s *Store) OutreachByID(ctx context.Context, id int64) (Outreach, bool, error) {
	o, err := scanOutreach(s.db.QueryRowContext(ctx, `SELECT `+outreachColumns+` FROM outreach WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Outreach{}, false, nil
	}
	if err != nil {
		return Outreach{}, false, fmt.Errorf("read outreach %d: %w", id, err)
	}
	return o, true, nil
}

func (s *Store) queryOutreach(ctx context.Context, q string, args ...any) ([]Outreach, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("read outreach: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Outreach
	for rows.Next() {
		o, err := scanOutreach(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// OutreachSince lists outreach sent at or after since, oldest first.
func (s *Store) OutreachSince(ctx context.Context, since time.Time) ([]Outreach, error) {
	return s.queryOutreach(ctx, `SELECT `+outreachColumns+` FROM outreach WHERE sent_at >= ? ORDER BY id`,
		since.UTC().Format(timeFormat))
}

// OutreachByOutcome lists outreach in any of the given outcomes, oldest first.
func (s *Store) OutreachByOutcome(ctx context.Context, outcomes ...string) ([]Outreach, error) {
	if len(outcomes) == 0 {
		return nil, nil
	}
	args := make([]any, len(outcomes))
	for i, o := range outcomes {
		args[i] = o
	}
	return s.queryOutreach(ctx, `SELECT `+outreachColumns+` FROM outreach WHERE outcome IN (?`+
		strings.Repeat(`, ?`, len(outcomes)-1)+`) ORDER BY id`, args...)
}

// OpenProposals lists the proposals a button press can still answer, newest
// first: kind proposal, outcome pending or unanswered, and an interest that
// is still proposed. Service.AnswerProposal refuses anything else, so a
// client that lists these never offers a press that fails.
func (s *Store) OpenProposals(ctx context.Context) ([]Outreach, error) {
	return s.queryOutreach(ctx, `SELECT `+outreachColumns+` FROM outreach
		WHERE kind = ? AND outcome IN (?, ?)
		  AND interest_id IN (SELECT id FROM interests WHERE state = ?)
		ORDER BY id DESC`,
		OutreachProposal, OutcomePending, OutcomeUnanswered, InterestProposed)
}

// NudgeFor returns the nudge that follows up id, if there is one. There is
// at most one: reflection may nudge a message once.
func (s *Store) NudgeFor(ctx context.Context, id int64) (Outreach, bool, error) {
	o, err := scanOutreach(s.db.QueryRowContext(ctx,
		`SELECT `+outreachColumns+` FROM outreach WHERE nudge_of = ? ORDER BY id LIMIT 1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Outreach{}, false, nil
	}
	if err != nil {
		return Outreach{}, false, fmt.Errorf("read nudge of %d: %w", id, err)
	}
	return o, true, nil
}

// ResolveOutreach moves an outreach to outcome `to` when its outcome is one
// of from, and reports whether it moved. The from check makes a second,
// racing resolution -- two button presses, a reply and a timeout -- a no-op.
func (s *Store) ResolveOutreach(ctx context.Context, id int64, from []string, to, reason string, at time.Time) (bool, error) {
	if len(from) == 0 {
		return false, errors.New("resolve outreach: no outcome to move from")
	}
	args := []any{to, reason, at.UTC().Format(timeFormat), id}
	for _, f := range from {
		args = append(args, f)
	}
	q := `UPDATE outreach SET outcome = ?, drop_reason = ?, resolved_at = ? WHERE id = ? AND outcome IN (?`
	q += strings.Repeat(`, ?`, len(from)-1) + `)` //nolint:gosec // G202: only placeholders are concatenated; every value is bound
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return false, fmt.Errorf("resolve outreach %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// CompanionState reads one companion singleton; false when it was never set.
func (s *Store) CompanionState(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM companion_state WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read companion state %s: %w", key, err)
	}
	return v, true, nil
}

// SetCompanionState writes one companion singleton.
func (s *Store) SetCompanionState(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO companion_state (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, nowString())
	if err != nil {
		return fmt.Errorf("write companion state %s: %w", key, err)
	}
	return nil
}
