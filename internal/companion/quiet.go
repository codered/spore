package companion

import (
	"time"

	"github.com/codered/spore/internal/config"
)

// InQuietHours reports whether now falls inside the "HH:MM-HH:MM" range,
// read on the clock in loc. The range may wrap midnight; its start is inside
// it and its end is not.
func InQuietHours(now time.Time, loc *time.Location, spec string) (bool, error) {
	start, end, err := config.ParseQuietHours(spec)
	if err != nil {
		return false, err
	}
	l := now.In(loc)
	m := l.Hour()*60 + l.Minute()
	if start < end {
		return m >= start && m < end, nil
	}
	return m >= start || m < end, nil
}

// LocalDayStart is midnight at the start of now's calendar day in loc: where
// the daily budget resets.
func LocalDayStart(now time.Time, loc *time.Location) time.Time {
	l := now.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
}
