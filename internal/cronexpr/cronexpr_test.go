package cronexpr

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, expr string) *Schedule {
	t.Helper()
	s, err := Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q): %v", expr, err)
	}
	return s
}

func TestNext(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatalf("timezone: %v", err)
	}
	cases := []struct {
		name string
		expr string
		loc  *time.Location
		from string
		want string
	}{
		{"every minute", "* * * * *", time.UTC, "2026-07-11T10:00:30Z", "2026-07-11T10:01:00Z"},
		{"hourly", "0 * * * *", time.UTC, "2026-07-11T10:00:00Z", "2026-07-11T11:00:00Z"},
		{"daily 3am", "0 3 * * *", time.UTC, "2026-07-11T10:00:00Z", "2026-07-12T03:00:00Z"},
		{"step", "*/15 * * * *", time.UTC, "2026-07-11T10:01:00Z", "2026-07-11T10:15:00Z"},
		{"list and range", "0 9-11,20 * * *", time.UTC, "2026-07-11T09:30:00Z", "2026-07-11T10:00:00Z"},
		{"monthly", "0 3 1 * *", time.UTC, "2026-07-11T10:00:00Z", "2026-08-01T03:00:00Z"},
		{"leap day", "0 0 29 2 *", time.UTC, "2026-07-11T10:00:00Z", "2028-02-29T00:00:00Z"},
		// Sunday: the day-of-week field alone restricts the day.
		{"weekly", "0 3 * * 0", time.UTC, "2026-07-11T10:00:00Z", "2026-07-12T03:00:00Z"},
		// Both day fields restricted: either matching is enough (11 July 2026
		// is a Saturday, so the 15th comes after the next Sunday, the 12th).
		{"day or weekday", "0 3 15 * 0", time.UTC, "2026-07-11T10:00:00Z", "2026-07-12T03:00:00Z"},
		// 03:00 Paris on a normal day is 01:00 UTC.
		{"timezone", "0 3 * * *", paris, "2026-07-11T10:00:00Z", "2026-07-12T01:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from, err := time.Parse(time.RFC3339, tc.from)
			if err != nil {
				t.Fatalf("from: %v", err)
			}
			got := mustParse(t, tc.expr).Next(from, tc.loc)
			want, err := time.Parse(time.RFC3339, tc.want)
			if err != nil {
				t.Fatalf("want: %v", err)
			}
			if !got.Equal(want) {
				t.Errorf("Next(%q, %s) = %s, want %s", tc.expr, tc.from, got.UTC().Format(time.RFC3339), want.Format(time.RFC3339))
			}
		})
	}
}

// An expression no date can satisfy must terminate rather than spin.
func TestNextUnsatisfiable(t *testing.T) {
	from := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	if got := mustParse(t, "0 0 31 2 *").Next(from, time.UTC); !got.IsZero() {
		t.Errorf("31 February resolved to %s, want the zero time", got)
	}
}

// A DST spring-forward skips a wall-clock hour: an occurrence that falls
// inside it never happens, so the schedule must resume at the following day
// rather than fire twice or stall.
func TestNextAcrossDST(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatalf("timezone: %v", err)
	}
	// On 29 March 2026 the Paris clocks jump 02:00 → 03:00: 02:30 does not
	// exist that day, so the daily 02:30 backup is skipped once.
	from := time.Date(2026, 3, 28, 3, 0, 0, 0, paris)
	next := mustParse(t, "30 2 * * *").Next(from, paris)
	if want := time.Date(2026, 3, 30, 2, 30, 0, 0, paris); !next.Equal(want) {
		t.Errorf("next = %s, want %s", next, want)
	}
}

func TestParseRejects(t *testing.T) {
	for _, expr := range []string{
		"",
		"* * * *",
		"* * * * * *",
		"60 * * * *",
		"* 24 * * *",
		"* * 0 * *",
		"* * * 13 *",
		"* * * * 7",
		"10-5 * * * *",
		"*/0 * * * *",
		"a * * * *",
		"@daily",
	} {
		if _, err := Parse(expr); err == nil {
			t.Errorf("Parse(%q) succeeded, want an error", expr)
		}
	}
}
