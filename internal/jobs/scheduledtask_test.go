package jobs

import (
	"strings"
	"testing"
)

func TestClampOutput(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		clamped bool
	}{
		{
			name: "empty output is kept as-is",
			in:   "",
			want: "",
		},
		{
			name: "short output is kept as-is",
			in:   "task done\n",
			want: "task done\n",
		},
		{
			name: "output exactly at the limit is kept as-is",
			in:   strings.Repeat("x", maxTaskOutput),
			want: strings.Repeat("x", maxTaskOutput),
		},
		{
			name:    "one byte over the limit keeps the tail",
			in:      "H" + strings.Repeat("x", maxTaskOutput),
			want:    strings.Repeat("x", maxTaskOutput),
			clamped: true,
		},
		{
			name:    "far over the limit keeps the last maxTaskOutput bytes",
			in:      strings.Repeat("head boilerplate\n", 8192) + "TAIL:" + strings.Repeat("e", maxTaskOutput-5),
			want:    "TAIL:" + strings.Repeat("e", maxTaskOutput-5),
			clamped: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, clamped := clampOutput(tt.in)
			if clamped != tt.clamped {
				t.Errorf("clamped = %v, want %v", clamped, tt.clamped)
			}
			if got != tt.want {
				t.Errorf("output mismatch: got %d bytes, want %d bytes", len(got), len(tt.want))
			}
			// The invariant the function exists for: the END survives.
			if !strings.HasSuffix(tt.in, got) {
				t.Errorf("clamped output is not a suffix of the input")
			}
			if len(got) > maxTaskOutput {
				t.Errorf("clamped output is %d bytes, over the %d limit", len(got), maxTaskOutput)
			}
		})
	}
}
