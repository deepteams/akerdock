package scheduler

import (
	"testing"
	"time"
)

func TestIdlePastWindow(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		last   time.Time
		window int32
		want   bool
	}{
		{"idle beyond window", now.Add(-45 * time.Minute), 30, true},
		{"exactly at window", now.Add(-30 * time.Minute), 30, true},
		{"still active", now.Add(-5 * time.Minute), 30, false},
		{"just active", now, 30, false},
		{"zero window falls back to default (not idle)", now.Add(-10 * time.Minute), 0, false},
		{"zero window falls back to default (idle)", now.Add(-31 * time.Minute), 0, true},
		{"negative window falls back to default", now.Add(-31 * time.Minute), -5, true},
	}
	for _, c := range cases {
		if got := idlePastWindow(c.last, c.window, now); got != c.want {
			t.Errorf("%s: idlePastWindow(%v, %d) = %v, want %v", c.name, c.last, c.window, got, c.want)
		}
	}
}
