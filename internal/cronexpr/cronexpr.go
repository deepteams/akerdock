// Package cronexpr parses the 5-field cron expressions of the backup plans
// (§7.1) and computes their next occurrence in a given timezone.
//
// The supported grammar is deliberately the one the API contract validates —
// `*`, values, ranges, lists and steps — with no shell metacharacter and no
// extension (no `@reboot`, no seconds field, no `?`/`L`/`#`). Anything the
// API accepts, this package must be able to schedule: parsing here is the
// authority, the regexp at the edge is only a first filter (§23.3).
package cronexpr

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// field bounds, in cron order.
var bounds = [5]struct {
	name     string
	min, max int
}{
	{"minute", 0, 59},
	{"hour", 0, 23},
	{"day of month", 1, 31},
	{"month", 1, 12},
	{"day of week", 0, 6},
}

// Schedule is a parsed expression: one set of allowed values per field.
type Schedule struct {
	minutes  map[int]bool
	hours    map[int]bool
	days     map[int]bool
	months   map[int]bool
	weekdays map[int]bool
	// restricted tells whether day-of-month and day-of-week are constrained.
	// Cron's historical rule: when both are, either one matching is enough.
	dayRestricted, weekdayRestricted bool
}

// Parse compiles a 5-field expression. Fields are separated by runs of
// whitespace.
func Parse(expr string) (*Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron expression must have 5 fields, got %d", len(fields))
	}
	sets := [5]map[int]bool{}
	for i, f := range fields {
		set, err := parseField(f, bounds[i].min, bounds[i].max)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", bounds[i].name, err)
		}
		sets[i] = set
	}
	return &Schedule{
		minutes: sets[0], hours: sets[1], days: sets[2], months: sets[3], weekdays: sets[4],
		dayRestricted:     fields[2] != "*",
		weekdayRestricted: fields[4] != "*",
	}, nil
}

// parseField expands one field into the set of values it allows.
func parseField(field string, minVal, maxVal int) (map[int]bool, error) {
	set := map[int]bool{}
	for _, part := range strings.Split(field, ",") {
		if part == "" {
			return nil, fmt.Errorf("empty value in %q", field)
		}
		step := 1
		if base, stepStr, ok := strings.Cut(part, "/"); ok {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("invalid step in %q", part)
			}
			step, part = n, base
		}

		lo, hi := minVal, maxVal
		switch {
		case part == "*":
			// full range, already set
		case strings.Contains(part, "-"):
			loStr, hiStr, _ := strings.Cut(part, "-")
			var err error
			if lo, err = boundedValue(loStr, minVal, maxVal); err != nil {
				return nil, err
			}
			if hi, err = boundedValue(hiStr, minVal, maxVal); err != nil {
				return nil, err
			}
			if lo > hi {
				return nil, fmt.Errorf("inverted range %q", part)
			}
		default:
			v, err := boundedValue(part, minVal, maxVal)
			if err != nil {
				return nil, err
			}
			// A bare value with a step means "from v to the end of the range",
			// as in `*/15` written `0/15`.
			lo, hi = v, v
			if step > 1 {
				hi = maxVal
			}
		}
		for v := lo; v <= hi; v += step {
			set[v] = true
		}
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("no value matches %q", field)
	}
	return set, nil
}

func boundedValue(s string, minVal, maxVal int) (int, error) {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	if v < minVal || v > maxVal {
		return 0, fmt.Errorf("%d is out of range [%d, %d]", v, minVal, maxVal)
	}
	return v, nil
}

// maxLookahead bounds the search: five years covers every expression this
// grammar can express (the rarest being `0 0 29 2 *`, a leap-year day), and
// guarantees Next terminates on an unsatisfiable one (e.g. 31st of February).
const maxLookahead = 5 * 366 * 24 * 60 * time.Minute

// Next returns the first occurrence strictly after `after`, evaluated in
// `loc`. It returns the zero time when the expression can never match.
//
// The search walks minute by minute in wall-clock time, so a DST jump is
// handled by the standard library: an occurrence inside a skipped hour does
// not fire, and one inside a repeated hour fires once.
func (s *Schedule) Next(after time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	t := after.In(loc).Truncate(time.Minute).Add(time.Minute)
	deadline := t.Add(maxLookahead)
	for ; t.Before(deadline); t = t.Add(time.Minute) {
		if !s.months[int(t.Month())] {
			// Skip to the first minute of the next month rather than stepping
			// through every one of its minutes.
			t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc).AddDate(0, 1, 0).Add(-time.Minute)
			continue
		}
		if !s.matchesDay(t) {
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1).Add(-time.Minute)
			continue
		}
		if !s.hours[t.Hour()] {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, loc).Add(time.Hour - time.Minute)
			continue
		}
		if s.minutes[t.Minute()] {
			return t
		}
	}
	return time.Time{}
}

// matchesDay applies the historical day-of-month / day-of-week rule: when
// both fields are restricted, the day matches if EITHER matches.
func (s *Schedule) matchesDay(t time.Time) bool {
	day, weekday := s.days[t.Day()], s.weekdays[int(t.Weekday())]
	if s.dayRestricted && s.weekdayRestricted {
		return day || weekday
	}
	return day && weekday
}
