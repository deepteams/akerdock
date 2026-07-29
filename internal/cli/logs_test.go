package cli

import "testing"

// `logs --pr N -f` polls a snapshot endpoint, so every poll returns a sliding
// window with lines it already printed. The overlap is found on content: a
// line printed twice is noise, a line dropped is a log the operator will look
// for and not find.
func TestAlreadySeenLines(t *testing.T) {
	tests := map[string]struct {
		previous, current []string
		want              int
	}{
		"nothing new":              {[]string{"a", "b", "c"}, []string{"a", "b", "c"}, 3},
		"window advanced by one":   {[]string{"a", "b", "c"}, []string{"b", "c", "d"}, 2},
		"window advanced past all": {[]string{"a", "b", "c"}, []string{"x", "y", "z"}, 0},
		"appended without sliding": {[]string{"a", "b"}, []string{"a", "b", "c"}, 2},
		"first poll":               {nil, []string{"a", "b"}, 0},
		"container restarted":      {[]string{"a", "b"}, nil, 0},
		// A repeated line ("done" twice) must not make the overlap look longer
		// than it is: the longest match wins, and it is the one anchored at the
		// end of the previous window.
		"repeated lines": {[]string{"done", "done"}, []string{"done", "done", "next"}, 2},
		// The whole previous window fits inside the new one only when the tail
		// matches: a coincidental match on a shorter suffix still holds.
		"partial suffix match": {[]string{"a", "b", "c"}, []string{"c", "d"}, 1},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := alreadySeenLines(tc.previous, tc.current); got != tc.want {
				t.Fatalf("alreadySeenLines(%q, %q) = %d, want %d", tc.previous, tc.current, got, tc.want)
			}
		})
	}
}

// The count is an index into the fresh page: it must never exceed its length,
// or printing page[seen:] would panic on a shrinking window.
func TestAlreadySeenLinesStaysInBounds(t *testing.T) {
	previous := []string{"a", "b", "c", "d"}
	for _, current := range [][]string{nil, {"d"}, {"c", "d"}, {"z"}, {"a", "b", "c", "d", "e"}} {
		if got := alreadySeenLines(previous, current); got < 0 || got > len(current) {
			t.Fatalf("alreadySeenLines(_, %q) = %d, out of [0, %d]", current, got, len(current))
		}
	}
}
