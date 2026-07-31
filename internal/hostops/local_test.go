package hostops

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cerrdefs "github.com/containerd/errdefs"

	"github.com/deepteams/akerdock/internal/agentwire"
)

func testLocal(t *testing.T) (*Local, string) {
	t.Helper()
	root := t.TempDir()
	return &Local{Root: root}, root
}

// TestLocalPathGuard pins the ADR-054 allowlist: relative, dirty and
// out-of-root paths are refused by every primitive — the guard is
// authoritative on the agent side, whatever the control plane sent.
func TestLocalPathGuard(t *testing.T) {
	l, root := testLocal(t)
	ctx := context.Background()
	for name, path := range map[string]string{
		"relative":        "etc/passwd",
		"dot-dot escape":  root + "/../escape",
		"sibling prefix":  root + "sibling/file",
		"outside":         "/etc/passwd",
		"unclean segment": root + "/a/./b",
	} {
		t.Run(name, func(t *testing.T) {
			if err := l.WriteFile(ctx, agentwire.FileWriteParams{Path: path, Mode: 0o600}); !cerrdefs.IsInvalidArgument(err) {
				t.Fatalf("WriteFile(%q) = %v, want an invalid-argument refusal", path, err)
			}
			if _, err := l.ReadFile(ctx, agentwire.FileReadParams{Path: path}); !cerrdefs.IsInvalidArgument(err) {
				t.Fatalf("ReadFile(%q) = %v, want an invalid-argument refusal", path, err)
			}
			if err := l.Remove(ctx, agentwire.FileRemoveParams{Path: path, Recursive: true}); !cerrdefs.IsInvalidArgument(err) {
				t.Fatalf("Remove(%q) = %v, want an invalid-argument refusal", path, err)
			}
		})
	}
	// The root itself may be statted but never removed.
	if err := l.Remove(ctx, agentwire.FileRemoveParams{Path: root, Recursive: true}); !cerrdefs.IsInvalidArgument(err) {
		t.Fatalf("Remove(root) = %v, want a refusal", err)
	}
}

// TestLocalWriteFile pins the write semantics: parents on demand, the mode
// applied against the umask, and the atomic variant staging next to the
// target so a reader never sees a partial file.
func TestLocalWriteFile(t *testing.T) {
	l, root := testLocal(t)
	ctx := context.Background()
	path := root + "/proxy/dynamic/app.yaml"

	if err := l.WriteFile(ctx, agentwire.FileWriteParams{Path: path, Content: []byte("v1"), Mode: 0o600}); err == nil {
		t.Fatal("a write without MakeDirs must not invent parents")
	}
	if err := l.WriteFile(ctx, agentwire.FileWriteParams{
		Path: path, Content: []byte("v1"), Mode: 0o600, MakeDirs: true, DirMode: 0o700, Atomic: true,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "v1" {
		t.Fatalf("content = %q, %v", got, err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v", st.Mode(), err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("the atomic staging file must not survive")
	}
	// Overwrite keeps working and re-applies the mode.
	if err := l.WriteFile(ctx, agentwire.FileWriteParams{Path: path, Content: []byte("v2"), Mode: 0o644, Atomic: true}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "v2" {
		t.Fatalf("overwrite lost: %q", got)
	}
}

// TestLocalReadFile pins the read semantics: absence is data (Found=false,
// nil error), and MaxBytes bounds the payload with the truncation flagged.
func TestLocalReadFile(t *testing.T) {
	l, root := testLocal(t)
	ctx := context.Background()

	res, err := l.ReadFile(ctx, agentwire.FileReadParams{Path: root + "/absent.json"})
	if err != nil || res.Found {
		t.Fatalf("absent file = %+v, %v — absence is data, not an error", res, err)
	}

	path := root + "/store.json"
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 100), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = l.ReadFile(ctx, agentwire.FileReadParams{Path: path, MaxBytes: 10})
	if err != nil || !res.Found || !res.Truncated || len(res.Content) != 10 {
		t.Fatalf("bounded read = %+v, %v", res, err)
	}
	res, err = l.ReadFile(ctx, agentwire.FileReadParams{Path: path})
	if err != nil || res.Truncated || len(res.Content) != 100 {
		t.Fatalf("full read = %+v, %v", res, err)
	}
}

// TestLocalRemoveAndStat pins idempotent removal and the existence probe.
func TestLocalRemoveAndStat(t *testing.T) {
	l, root := testLocal(t)
	ctx := context.Background()
	dir := root + "/applications/app"
	if err := os.MkdirAll(dir+"/env", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/env/build.env", []byte("A=1"), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := l.Stat(ctx, dir)
	if err != nil || !st.Found || !st.IsDir {
		t.Fatalf("stat = %+v, %v", st, err)
	}
	if err := l.Remove(ctx, agentwire.FileRemoveParams{Path: dir, Recursive: true}); err != nil {
		t.Fatal(err)
	}
	if st, _ := l.Stat(ctx, dir); st.Found {
		t.Fatal("the tree must be gone")
	}
	// Idempotent, recursive or not.
	if err := l.Remove(ctx, agentwire.FileRemoveParams{Path: dir, Recursive: true}); err != nil {
		t.Fatalf("recursive replay = %v", err)
	}
	if err := l.Remove(ctx, agentwire.FileRemoveParams{Path: root + "/absent.yaml"}); err != nil {
		t.Fatalf("plain replay = %v", err)
	}
}

// TestLocalCopyAndChown pins the backup-then-restart flow of the ACME store
// and the key handover to the database uid.
func TestLocalCopyAndChown(t *testing.T) {
	l, root := testLocal(t)
	ctx := context.Background()
	src := root + "/proxy/acme.json"
	if err := os.MkdirAll(filepath.Dir(src), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte(`{"le":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.CopyFile(ctx, agentwire.FileCopyParams{Src: src, Dst: src + ".bak"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(src + ".bak")
	if err != nil || string(got) != `{"le":{}}` {
		t.Fatalf("copy = %q, %v", got, err)
	}
	st, _ := os.Stat(src + ".bak")
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("the copy must keep the source mode, got %v", st.Mode())
	}
	if err := l.CopyFile(ctx, agentwire.FileCopyParams{Src: root + "/absent", Dst: src + ".bak"}); err == nil {
		t.Fatal("copying an absent source must fail")
	}
	// Chown to the current uid/gid: a no-op that exercises the full path
	// without requiring root.
	if err := l.Chown(ctx, agentwire.FileChownParams{Path: src, UID: os.Getuid(), GID: os.Getgid()}); err != nil {
		t.Fatal(err)
	}
}

func TestLocalEnsureDir(t *testing.T) {
	l, root := testLocal(t)
	if err := l.EnsureDir(context.Background(), agentwire.DirEnsureParams{Path: root + "/databases/db", Mode: 0o700}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(root + "/databases/db")
	if err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		t.Fatalf("dir = %v, %v", st.Mode(), err)
	}
}

// TestDetectLocal pins the mount detection: an agent without the host tree
// answers nil so the executor reports unavailability instead of ENOENT.
func TestDetectLocal(t *testing.T) {
	if got := DetectLocal(); got != nil {
		// The developer machine MAY have /var/lib/akerdock; both answers are
		// legitimate — only the nil contract on absence is testable portably.
		if got.Root != Root {
			t.Fatalf("DetectLocal root = %q", got.Root)
		}
	}
	if !strings.HasPrefix(Root, "/") {
		t.Fatalf("Root must be absolute: %q", Root)
	}
}
