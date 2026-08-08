package hostops

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/deepteams/akerdock/internal/agentwire"
)

// fakeSender records the one command (or stream) it carries and answers with
// the scripted result — the seam the client rides in production is the agent
// channel, here a plain in-memory double.
type fakeSender struct {
	method string
	params any
	result json.RawMessage
	err    error
	stream io.ReadCloser
}

func (f *fakeSender) Command(_ context.Context, method string, params any) (json.RawMessage, error) {
	f.method, f.params = method, params
	return f.result, f.err
}

func (f *fakeSender) Stream(_ context.Context, method string, params any) (io.ReadCloser, error) {
	f.method, f.params = method, params
	return f.stream, f.err
}

// TestClientUnaryMethods pins the wire mapping: every Ops call travels as its
// ADR-054 method name with the typed params passed through untouched, and the
// sender's error surfaces as-is.
func TestClientUnaryMethods(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		method string
		call   func(Ops) error
	}{
		{"WriteFile", agentwire.MethodFileWrite, func(o Ops) error {
			return o.WriteFile(ctx, agentwire.FileWriteParams{Path: "/var/lib/akerdock/f", Mode: 0o600})
		}},
		{"Remove", agentwire.MethodFileRemove, func(o Ops) error {
			return o.Remove(ctx, agentwire.FileRemoveParams{Path: "/var/lib/akerdock/f"})
		}},
		{"Chown", agentwire.MethodFileChown, func(o Ops) error {
			return o.Chown(ctx, agentwire.FileChownParams{Path: "/var/lib/akerdock/f", UID: 1, GID: 2})
		}},
		{"CopyFile", agentwire.MethodFileCopy, func(o Ops) error {
			return o.CopyFile(ctx, agentwire.FileCopyParams{Src: "/var/lib/akerdock/a", Dst: "/var/lib/akerdock/b"})
		}},
		{"EnsureDir", agentwire.MethodDirEnsure, func(o Ops) error {
			return o.EnsureDir(ctx, agentwire.DirEnsureParams{Path: "/var/lib/akerdock/d", Mode: 0o700})
		}},
		{"FileToURL", agentwire.MethodFileToURL, func(o Ops) error {
			return o.FileToURL(ctx, agentwire.FileToURLParams{Path: "/var/lib/akerdock/f", URL: "https://bucket/key"})
		}},
		{"URLToFile", agentwire.MethodURLToFile, func(o Ops) error {
			return o.URLToFile(ctx, agentwire.URLToFileParams{Path: "/var/lib/akerdock/f", URL: "https://bucket/key"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &fakeSender{}
			if err := tc.call(NewClient(s)); err != nil {
				t.Fatal(err)
			}
			if s.method != tc.method {
				t.Fatalf("method = %q, want %q", s.method, tc.method)
			}
			s.err = errors.New("channel down")
			if err := tc.call(NewClient(s)); err == nil || !strings.Contains(err.Error(), "channel down") {
				t.Fatalf("sender error must surface, got %v", err)
			}
		})
	}
}

// TestClientTypedResults pins the result decoding: the command's JSON body
// lands in the typed wire result, an empty body answers the zero value, and a
// sender error short-circuits.
func TestClientTypedResults(t *testing.T) {
	ctx := context.Background()

	t.Run("ReadFile", func(t *testing.T) {
		s := &fakeSender{result: json.RawMessage(`{"content":"aGk=","found":true,"truncated":true}`)}
		res, err := NewClient(s).ReadFile(ctx, agentwire.FileReadParams{Path: "/var/lib/akerdock/f"})
		if err != nil || !res.Found || !res.Truncated || string(res.Content) != "hi" {
			t.Fatalf("res = %+v, %v", res, err)
		}
		if s.method != agentwire.MethodFileRead {
			t.Fatalf("method = %q", s.method)
		}
	})
	t.Run("Stat", func(t *testing.T) {
		s := &fakeSender{result: json.RawMessage(`{"found":true,"is_dir":true,"size":42}`)}
		res, err := NewClient(s).Stat(ctx, "/var/lib/akerdock/d")
		if err != nil || !res.Found || !res.IsDir || res.Size != 42 {
			t.Fatalf("res = %+v, %v", res, err)
		}
		if s.method != agentwire.MethodFileStat {
			t.Fatalf("method = %q", s.method)
		}
		if p, ok := s.params.(agentwire.FileStatParams); !ok || p.Path != "/var/lib/akerdock/d" {
			t.Fatalf("params = %#v — the path must ride the typed params", s.params)
		}
	})
	t.Run("ExecToFile", func(t *testing.T) {
		s := &fakeSender{result: json.RawMessage(`{"exit_code":3,"stderr":"ERROR"}`)}
		res, err := NewClient(s).ExecToFile(ctx, agentwire.ExecToFileParams{Path: "/var/lib/akerdock/f"})
		if err != nil || res.ExitCode != 3 || res.Stderr != "ERROR" {
			t.Fatalf("res = %+v, %v", res, err)
		}
		if s.method != agentwire.MethodExecToFile {
			t.Fatalf("method = %q", s.method)
		}
	})
	t.Run("FileToExec", func(t *testing.T) {
		s := &fakeSender{result: json.RawMessage(`{"exit_code":1,"output":"done"}`)}
		res, err := NewClient(s).FileToExec(ctx, agentwire.FileToExecParams{Path: "/var/lib/akerdock/f"})
		if err != nil || res.ExitCode != 1 || res.Output != "done" {
			t.Fatalf("res = %+v, %v", res, err)
		}
		if s.method != agentwire.MethodFileToExec {
			t.Fatalf("method = %q", s.method)
		}
	})
	t.Run("HashFile", func(t *testing.T) {
		s := &fakeSender{result: json.RawMessage(`{"sha256":"abc","size_bytes":7}`)}
		res, err := NewClient(s).HashFile(ctx, "/var/lib/akerdock/f")
		if err != nil || res.SHA256 != "abc" || res.SizeBytes != 7 {
			t.Fatalf("res = %+v, %v", res, err)
		}
		if s.method != agentwire.MethodFileHash {
			t.Fatalf("method = %q", s.method)
		}
	})
	t.Run("empty body answers the zero value", func(t *testing.T) {
		res, err := NewClient(&fakeSender{}).Stat(ctx, "/var/lib/akerdock/absent")
		if err != nil || res.Found {
			t.Fatalf("res = %+v, %v", res, err)
		}
	})
	t.Run("sender error short-circuits", func(t *testing.T) {
		s := &fakeSender{err: errors.New("agent unreachable")}
		if _, err := NewClient(s).ReadFile(ctx, agentwire.FileReadParams{Path: "/var/lib/akerdock/f"}); err == nil || !strings.Contains(err.Error(), "agent unreachable") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("malformed body fails", func(t *testing.T) {
		s := &fakeSender{result: json.RawMessage(`{not json`)}
		if _, err := NewClient(s).HashFile(ctx, "/var/lib/akerdock/f"); err == nil {
			t.Fatal("a malformed result body must fail the call")
		}
	})
}

// TestClientBuildImageStreams pins the one streamed call: the build travels
// as MethodImageBuild and the sender's stream comes back verbatim.
func TestClientBuildImageStreams(t *testing.T) {
	stream := io.NopCloser(strings.NewReader("#1 DONE\n"))
	s := &fakeSender{stream: stream}
	rc, err := NewClient(s).BuildImage(context.Background(), agentwire.ImageBuildParams{
		ContextDir: "/var/lib/akerdock/builds/x", Dockerfile: "Dockerfile", Tags: []string{"t"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.method != agentwire.MethodImageBuild {
		t.Fatalf("method = %q", s.method)
	}
	out, _ := io.ReadAll(rc)
	if string(out) != "#1 DONE\n" {
		t.Fatalf("stream = %q", out)
	}

	s = &fakeSender{err: errors.New("agent unreachable")}
	if _, err := NewClient(s).BuildImage(context.Background(), agentwire.ImageBuildParams{}); err == nil {
		t.Fatal("the stream error must surface")
	}
}
