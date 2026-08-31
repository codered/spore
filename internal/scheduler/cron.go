// Package scheduler fires stored jobs. It knows about the jobs table and a
// Runner callback and nothing else — in particular it never imports the
// agent, so its whole surface tests against an injected clock.
package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule answers one question: given a time, when does this next fire?
type Schedule interface {
	// Next returns the first fire time strictly after t, or the zero time
	// when the schedule has no further runs.
	Next(t time.Time) time.Time
	// Kind is "cron" or "once"; it is stored on the job row.
	Kind() string
}

// maxHorizon bounds the search in Next. A schedule with no match inside a
// leap-year cycle (29 February on a weekday that never coincides, say) is
// reported as having no next run rather than looping forever.
const maxHorizon = 5 * 366 * 24 * time.Hour

// Parse accepts either an RFC3339 instant (a one-shot job) or a five-field
// cron expression: minute hour day-of-month month day-of-week.
func Parse(spec string) (Schedule, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, fmt.Errorf("schedule is empty")
	}
	if at, err := time.Parse(time.RFC3339, spec); err == nil {
		return onceSchedule{at: at.UTC()}, nil
	}
	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return nil, fmt.Errorf("schedule %q must be an RFC3339 instant or five cron fields, got %d fields", spec, len(fields))
	}
	var c cronSchedule
	var err error
	if c.minute, err = parseField(fields[0], 0, 59); err != nil {
		return nil, fmt.Errorf("minute field: %w", err)
	}
	if c.hour, err = parseField(fields[1], 0, 23); err != nil {
		return nil, fmt.Errorf("hour field: %w", err)
	}
	if c.dom, err = parseField(fields[2], 1, 31); err != nil {
		return nil, fmt.Errorf("day-of-month field: %w", err)
	}
	if c.month, err = parseField(fields[3], 1, 12); err != nil {
		return nil, fmt.Errorf("month field: %w", err)
	}
	// Both 0 and 7 mean Sunday, which is the one place the two common cron
	// dialects agree, so both are accepted and normalised to 0.
	if c.dow, err = parseField(fields[4], 0, 7); err != nil {
		return nil, fmt.Errorf("day-of-week field: %w", err)
	}
	if c.dow[7] {
		c.dow[0] = true
	}
	c.restrictedDOM = fields[2] != "*"
	c.restrictedDOW = fields[4] != "*"
	return c, nil
}

// parseField expands one cron field into a membership set. Supported forms:
// "*", "*/n", "a", "a-b", "a-b/n", and any comma-separated list of those.
func parseField(field string, min, max int) (map[int]bool, error) {
	out := map[int]bool{}
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("empty element in %q", field)
		}
		step := 1
		if slash := strings.Index(part, "/"); slash >= 0 {
			var err error
			step, err = strconv.Atoi(part[slash+1:])
			if err != nil || step <= 0 {
				return nil, fmt.Errorf("step in %q must be a positive number", part)
			}
			part = part[:slash]
		}
		lo, hi := min, max
		if part != "*" {
			if dash := strings.Index(part, "-"); dash >= 0 {
				var err error
				if lo, err = strconv.Atoi(part[:dash]); err != nil {
					return nil, fmt.Errorf("range start in %q: %w", part, err)
				}
				if hi, err = strconv.Atoi(part[dash+1:]); err != nil {
					return nil, fmt.Errorf("range end in %q: %w", part, err)
				}
				if lo > hi {
					return nil, fmt.Errorf("range %q runs backwards", part)
				}
			} else {
				v, err := strconv.Atoi(part)
				if err != nil {
					return nil, fmt.Errorf("%q is not a number", part)
				}
				lo, hi = v, v
			}
		}
		if lo < min || hi > max {
			return nil, fmt.Errorf("%q is outside the valid range %d-%d", part, min, max)
		}
		for v := lo; v <= hi; v += step {
			out[v] = true
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("field %q matches nothing", field)
	}
	return out, nil
}

type cronSchedule struct {
	minute, hour, dom, month, dow map[int]bool
	// Standard cron oddity: when BOTH day-of-month and day-of-week are
	// restricted, a day matching EITHER fires. When only one is restricted,
	// only it applies.
	restrictedDOM, restrictedDOW bool
}

func (c cronSchedule) Kind() string { return "cron" }

func (c cronSchedule) matches(t time.Time) bool {
	if !c.minute[t.Minute()] || !c.hour[t.Hour()] || !c.month[int(t.Month())] {
		return false
	}
	domOK, dowOK := c.dom[t.Day()], c.dow[int(t.Weekday())]
	switch {
	case c.restrictedDOM && c.restrictedDOW:
		return domOK || dowOK
	case c.restrictedDOM:
		return domOK
	case c.restrictedDOW:
		return dowOK
	default:
		return true
	}
}

// Next steps minute by minute. A year of minutes is about half a million
// cheap comparisons and this runs once per fire, so the simplicity is worth
// more than calendar arithmetic that has to be right about leap years.
func (c cronSchedule) Next(t time.Time) time.Time {
	t = t.UTC().Truncate(time.Minute).Add(time.Minute)
	limit := t.Add(maxHorizon)
	for ; t.Before(limit); t = t.Add(time.Minute) {
		if c.matches(t) {
			return t
		}
	}
	return time.Time{}
}

type onceSchedule struct{ at time.Time }

func (o onceSchedule) Kind() string { return "once" }

func (o onceSchedule) Next(t time.Time) time.Time {
	if t.UTC().Before(o.at) {
		return o.at
	}
	return time.Time{}
}
