package cli

import (
	"context"
	"io"
	"os"
	"testing"

	"github.com/spf13/cobra"
)

// resetFlags gives the test a pristine copy of the global flag state and
// restores the previous one afterwards. Commands read the package-level
// `flags` directly, so every command test must isolate it.
func resetFlags(t *testing.T) {
	t.Helper()
	old := flags
	flags = globalFlags{output: "table"}
	t.Cleanup(func() { flags = old })
}

// setupHome points $HOME at a fresh directory, moves the working directory
// away from the repository (so no real .akerdock is picked up), and clears
// every AKERDOCK_* override. This is the hermetic baseline of a CLI test.
func setupHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	for _, k := range []string{envContext, envTeam, envProject, envApplication, envEnvironment, envComponent, "AKERDOCK_URL", "AKERDOCK_TOKEN"} {
		t.Setenv(k, "")
	}
	resetFlags(t)
}

// setupContext prepares a logged-in context named "test" pointing at baseURL,
// with a stored token — what `akerdock login` would have left behind.
func setupContext(t *testing.T, baseURL string) {
	t.Helper()
	setupHome(t)
	cfg := &Config{
		CurrentContext: "test",
		Contexts:       map[string]Context{"test": {URL: baseURL, TeamUUID: "team-1"}},
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if err := setToken("test", "akd_secret"); err != nil {
		t.Fatal(err)
	}
}

// captureOutput redirects os.Stdout and os.Stderr to pipes while fn runs.
// The CLI prints straight to the process streams, so this is the only way to
// assert on what the operator would have seen.
func captureOutput(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	or, ow, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	er, ew, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = ow, ew
	outCh := make(chan string, 1)
	errCh := make(chan string, 1)
	go func() { b, _ := io.ReadAll(or); outCh <- string(b) }()
	go func() { b, _ := io.ReadAll(er); errCh <- string(b) }()
	defer func() {
		os.Stdout, os.Stderr = oldOut, oldErr
	}()
	fn()
	_ = ow.Close()
	_ = ew.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	return <-outCh, <-errCh
}

// runCmd executes a cobra command the way main would, with cobra's own
// usage/error output silenced (the CLI's real messages go to os.Std* and are
// captured separately when the test cares).
func runCmd(cmd *cobra.Command, args ...string) error {
	return runCmdCtx(context.Background(), cmd, args...)
}

func runCmdCtx(ctx context.Context, cmd *cobra.Command, args ...string) error {
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	return cmd.ExecuteContext(ctx)
}
