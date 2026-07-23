package jobs

import (
	"errors"
	"strings"
	"testing"
)

// A failed step must keep BOTH the command output and the error: the error
// often carries more than the output — candidateFailure packs the dying
// container's logs into it, and dropping it leaves the operator staring at a
// bare "restarting" with nothing to debug.
func TestAppendFailure(t *testing.T) {
	err := errors.New("container is \"restarting\", expected running\npanic: missing DATABASE_URL")

	if got := appendFailure(nil, err); got == nil || !strings.Contains(*got, "missing DATABASE_URL") {
		t.Fatalf("empty log must become the error, got %v", got)
	}

	inspect := "restarting"
	got := appendFailure(&inspect, err)
	if !strings.Contains(*got, "restarting") || !strings.Contains(*got, "missing DATABASE_URL") {
		t.Fatalf("log must keep output AND error, got %q", *got)
	}

	// An error already embedded in the log (an exit-code error built FROM the
	// output) must not be duplicated.
	full := "some output\n" + err.Error()
	if got := appendFailure(&full, err); *got != full {
		t.Fatalf("embedded error must not duplicate, got %q", *got)
	}
}
