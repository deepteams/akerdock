package jobs

import (
	"os/exec"
	"strings"
	"testing"
)

// shellQuote guards the boundary between a stored secret and a remote shell.
// A quoting bug here is a shell injection, so the test does not check the
// escaping by eye: it hands the result to a real shell and compares what comes
// back out.
func TestShellQuoteRoundTrip(t *testing.T) {
	values := map[string]string{
		"plain":            "production",
		"spaces":           "hello world",
		"single quote":     "it's fine",
		"double quote":     `say "hi"`,
		"dollar":           "$HOME and ${PATH}",
		"backtick":         "`whoami`",
		"backslash":        `a\b\c`,
		"semicolon":        "x; rm -rf /",
		"newline":          "line1\nline2",
		"pem":              "-----BEGIN KEY-----\nabc'def\nxyz$(id)\n-----END KEY-----",
		"json":             `{"a": 1, "b": "c'd"}`,
		"all of the above": "a'b\"c$d`e\\f;g\nh",
	}
	for name, value := range values {
		t.Run(name, func(t *testing.T) {
			script := "V=" + shellQuote(value) + "\nprintf '%s' \"$V\""
			out, err := exec.Command("sh", "-c", script).Output()
			if err != nil {
				t.Fatalf("the quoted value did not survive a shell: %v", err)
			}
			if string(out) != value {
				t.Errorf("round trip changed the value:\n got %q\nwant %q", out, value)
			}
		})
	}
}

// The whole point of the change: a multiline value must reach the container.
func TestEnvFlagsCarryNoValue(t *testing.T) {
	flags := envFlags([]string{"SECRET_KEY", "APP_MODE"})
	if !strings.Contains(flags, "-e SECRET_KEY") || !strings.Contains(flags, "-e APP_MODE") {
		t.Fatalf("flags = %q", flags)
	}
	// A value in argv is a value in `ps` (INV-003).
	if strings.Contains(flags, "=") {
		t.Errorf("a value leaked into the docker flags: %q", flags)
	}
}
