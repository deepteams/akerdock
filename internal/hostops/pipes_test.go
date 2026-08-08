package hostops

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
)

// execPipeRuntime scripts one exec whose multiplexed stream is served from
// server-side writes; the test drives the server end of the pipe.
func execPipeRuntime(t *testing.T, exitCode int) (*fake.Runtime, net.Conn) {
	t.Helper()
	client, server := net.Pipe()
	rt := &fake.Runtime{}
	rt.ContainerExecCreateFn = func(_ context.Context, _ string, opts container.ExecOptions) (container.ExecCreateResponse, error) {
		return container.ExecCreateResponse{ID: "pipe"}, nil
	}
	rt.ContainerExecAttachFn = func(context.Context, string, container.ExecAttachOptions) (types.HijackedResponse, error) {
		return types.HijackedResponse{Conn: client, Reader: bufio.NewReader(client)}, nil
	}
	rt.ContainerExecInspectFn = func(context.Context, string) (container.ExecInspect, error) {
		return container.ExecInspect{ExitCode: exitCode}, nil
	}
	return rt, server
}

// TestExecToFileGzipsAndHashes pins the dump pipe: the exec's stdout lands
// gzipped in the target file, the digest and size describe the file AS
// WRITTEN, and the stderr tail rides the verdict.
func TestExecToFileGzipsAndHashes(t *testing.T) {
	rt, server := execPipeRuntime(t, 0)
	go func() {
		w := stdcopy.NewStdWriter(server, stdcopy.Stdout)
		_, _ = w.Write([]byte("-- PostgreSQL dump\nCREATE TABLE a ();\n"))
		e := stdcopy.NewStdWriter(server, stdcopy.Stderr)
		_, _ = e.Write([]byte("NOTICE: something\n"))
		_ = server.Close()
	}()
	l := &Local{Root: t.TempDir(), RT: rt}
	path := l.Root + "/backups/db/dump.sql.gz"

	res, err := l.ExecToFile(context.Background(), agentwire.ExecToFileParams{
		Container: "db", Cmd: []string{"sh", "-c", "pg_dump"},
		Path: path, Mode: 0o600, MakeDirs: true, DirMode: 0o700, Gzip: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if res.SHA256 != hex.EncodeToString(sum[:]) || res.SizeBytes != int64(len(raw)) {
		t.Fatalf("verdict %+v must describe the file as written (%d bytes)", res, len(raw))
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	content, _ := io.ReadAll(gz)
	if string(content) != "-- PostgreSQL dump\nCREATE TABLE a ();\n" {
		t.Fatalf("gunzipped content = %q", content)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stderr, "NOTICE") {
		t.Fatalf("verdict = %+v", res)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", st.Mode())
	}
}

// TestFileToExecGunzipsIntoStdin pins the restore pipe: the file gunzips
// agent-side into the exec's stdin, and the exit code with the output tail
// comes back typed.
func TestFileToExecGunzipsIntoStdin(t *testing.T) {
	l := &Local{Root: t.TempDir()}
	path := l.Root + "/dump.sql.gz"
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte("CREATE TABLE a ();\n"))
	_ = gz.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	rt, server := execPipeRuntime(t, 3)
	l.RT = rt
	stdin := make(chan string, 1)
	go func() {
		payload := make([]byte, len("CREATE TABLE a ();\n"))
		_, _ = io.ReadFull(server, payload)
		stdin <- string(payload)
		w := stdcopy.NewStdWriter(server, stdcopy.Stderr)
		_, _ = w.Write([]byte("ERROR: relation exists\n"))
		_ = server.Close()
	}()

	res, err := l.FileToExec(context.Background(), agentwire.FileToExecParams{
		Path: path, Gunzip: true, Container: "db",
		Cmd: []string{"psql", "-v", "ON_ERROR_STOP=1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := <-stdin; got != "CREATE TABLE a ();\n" {
		t.Fatalf("stdin = %q — the dump must arrive gunzipped", got)
	}
	if res.ExitCode != 3 || !strings.Contains(res.Output, "ERROR: relation exists") {
		t.Fatalf("verdict = %+v", res)
	}
}

// TestFileToURLUploads pins the presigned upload: a PUT with the file body,
// the signed headers attached, and a non-2xx surfacing the S3 body's tail.
func TestFileToURLUploads(t *testing.T) {
	l := &Local{Root: t.TempDir()}
	path := l.Root + "/dump.sql.gz"
	if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	var gotMethod, gotHeader, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotMethod, gotHeader, gotBody = r.Method, r.Header.Get("X-Amz-Server-Side-Encryption"), string(body)
		if r.ContentLength != int64(len("payload")) {
			t.Errorf("content length = %d", r.ContentLength)
		}
	}))
	defer srv.Close()

	err := l.FileToURL(context.Background(), agentwire.FileToURLParams{
		Path: path, URL: srv.URL + "/bucket/key?signed=1",
		Headers: map[string]string{"X-Amz-Server-Side-Encryption": "AES256"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPut || gotHeader != "AES256" || gotBody != "payload" {
		t.Fatalf("upload = %s %q %q", gotMethod, gotHeader, gotBody)
	}

	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<Error><Code>AccessDenied</Code></Error>"))
	}))
	defer deny.Close()
	err = l.FileToURL(context.Background(), agentwire.FileToURLParams{Path: path, URL: deny.URL})
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("denied upload = %v, want the S3 body's cause", err)
	}
}

// TestURLToFileDownloads pins the presigned fetch: the object lands at the
// recorded path with the requested mode, and an error status never leaves a
// file behind.
func TestURLToFileDownloads(t *testing.T) {
	l := &Local{Root: t.TempDir()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("dump-bytes"))
	}))
	defer srv.Close()
	path := l.Root + "/backups/db/dump.sql.gz"
	if err := l.URLToFile(context.Background(), agentwire.URLToFileParams{
		URL: srv.URL, Path: path, Mode: 0o600, MakeDirs: true, DirMode: 0o700,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "dump-bytes" {
		t.Fatalf("downloaded = %q, %v", got, err)
	}

	missing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "NoSuchKey", http.StatusNotFound)
	}))
	defer missing.Close()
	err = l.URLToFile(context.Background(), agentwire.URLToFileParams{URL: missing.URL, Path: l.Root + "/absent.gz", Mode: 0o600})
	if err == nil || !strings.Contains(err.Error(), "NoSuchKey") {
		t.Fatalf("missing object = %v", err)
	}
	if _, statErr := os.Stat(l.Root + "/absent.gz"); !os.IsNotExist(statErr) {
		t.Fatal("a failed download must not leave a file behind")
	}
}

// TestHashFileDigests pins the integrity primitive.
func TestHashFileDigests(t *testing.T) {
	l := &Local{Root: t.TempDir()}
	path := l.Root + "/dump.sql.gz"
	if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("payload"))
	res, err := l.HashFile(context.Background(), path)
	if err != nil || res.SHA256 != hex.EncodeToString(sum[:]) || res.SizeBytes != int64(len("payload")) {
		t.Fatalf("hash = %+v, %v", res, err)
	}
}

// TestPipesRefuseWithoutRuntime pins the pre-runtime contract: an exec pipe
// on a Local without the daemon handle answers a typed unavailability.
func TestPipesRefuseWithoutRuntime(t *testing.T) {
	l := &Local{Root: t.TempDir()}
	if _, err := l.ExecToFile(context.Background(), agentwire.ExecToFileParams{
		Container: "db", Cmd: []string{"true"}, Path: l.Root + "/f",
	}); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("ExecToFile without runtime = %v", err)
	}
}

// TestTailBufferKeepsTheTail pins the diagnostic bound: only the LAST
// tailLimit bytes ride the verdict, however chatty the exec was.
func TestTailBufferKeepsTheTail(t *testing.T) {
	tb := &tailBuffer{}
	if _, err := tb.Write(bytes.Repeat([]byte("x"), tailLimit)); err != nil {
		t.Fatal(err)
	}
	if _, err := tb.Write([]byte("THE END")); err != nil {
		t.Fatal(err)
	}
	got := tb.String()
	if len(got) != tailLimit || !strings.HasSuffix(got, "THE END") {
		t.Fatalf("tail = %d bytes ending %q, want %d ending in the last write", len(got), got[len(got)-8:], tailLimit)
	}
}

// TestExecToFileErrors walks the dump pipe's refusals: the path guard, the
// filesystem in the way, and each daemon call failing in turn.
func TestExecToFileErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("path guard", func(t *testing.T) {
		l := &Local{Root: t.TempDir(), RT: &fake.Runtime{}}
		if _, err := l.ExecToFile(ctx, agentwire.ExecToFileParams{Path: "relative", Container: "db"}); err == nil {
			t.Fatal("a relative path must be refused")
		}
	})
	t.Run("mkdir through a file", func(t *testing.T) {
		l := &Local{Root: t.TempDir(), RT: &fake.Runtime{}}
		if err := os.WriteFile(l.Root+"/blocker", []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := l.ExecToFile(ctx, agentwire.ExecToFileParams{
			Path: l.Root + "/blocker/sub/dump", Container: "db", MakeDirs: true,
		}); err == nil {
			t.Fatal("MakeDirs through a file must fail")
		}
	})
	t.Run("open without parents", func(t *testing.T) {
		l := &Local{Root: t.TempDir(), RT: &fake.Runtime{}}
		if _, err := l.ExecToFile(ctx, agentwire.ExecToFileParams{
			Path: l.Root + "/nodir/dump", Container: "db",
		}); err == nil {
			t.Fatal("a missing parent without MakeDirs must fail")
		}
	})
	t.Run("exec create fails", func(t *testing.T) {
		rt := &fake.Runtime{}
		rt.ContainerExecCreateFn = func(context.Context, string, container.ExecOptions) (container.ExecCreateResponse, error) {
			return container.ExecCreateResponse{}, context.DeadlineExceeded
		}
		l := &Local{Root: t.TempDir(), RT: rt}
		if _, err := l.ExecToFile(ctx, agentwire.ExecToFileParams{Path: l.Root + "/dump", Container: "db"}); err == nil {
			t.Fatal("the create failure must surface")
		}
	})
	t.Run("exec attach fails", func(t *testing.T) {
		rt := &fake.Runtime{}
		rt.ContainerExecCreateFn = func(context.Context, string, container.ExecOptions) (container.ExecCreateResponse, error) {
			return container.ExecCreateResponse{ID: "e"}, nil
		}
		rt.ContainerExecAttachFn = func(context.Context, string, container.ExecAttachOptions) (types.HijackedResponse, error) {
			return types.HijackedResponse{}, context.DeadlineExceeded
		}
		l := &Local{Root: t.TempDir(), RT: rt}
		if _, err := l.ExecToFile(ctx, agentwire.ExecToFileParams{Path: l.Root + "/dump", Container: "db"}); err == nil {
			t.Fatal("the attach failure must surface")
		}
	})
	t.Run("broken multiplex stream", func(t *testing.T) {
		rt, server := execPipeRuntime(t, 0)
		go func() {
			// An unrecognized stream id breaks the demux.
			_, _ = server.Write([]byte{9, 0, 0, 0, 0, 0, 0, 0})
			_ = server.Close()
		}()
		l := &Local{Root: t.TempDir(), RT: rt}
		if _, err := l.ExecToFile(ctx, agentwire.ExecToFileParams{Path: l.Root + "/dump", Container: "db"}); err == nil || !strings.Contains(err.Error(), "exec stream") {
			t.Fatalf("demux failure = %v", err)
		}
	})
	t.Run("inspect fails after the stream", func(t *testing.T) {
		rt, server := execPipeRuntime(t, 0)
		rt.ContainerExecInspectFn = func(context.Context, string) (container.ExecInspect, error) {
			return container.ExecInspect{}, context.DeadlineExceeded
		}
		go func() {
			w := stdcopy.NewStdWriter(server, stdcopy.Stdout)
			_, _ = w.Write([]byte("data"))
			_ = server.Close()
		}()
		l := &Local{Root: t.TempDir(), RT: rt}
		// Mode 0 also exercises the 0o600 default.
		if _, err := l.ExecToFile(ctx, agentwire.ExecToFileParams{Path: l.Root + "/dump", Container: "db"}); err == nil {
			t.Fatal("the inspect failure must surface")
		}
	})
}

// TestFileToExecErrors walks the restore pipe's refusals: guard, absent or
// corrupt dump, each daemon call, and both stream legs failing.
func TestFileToExecErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("path guard", func(t *testing.T) {
		l := &Local{Root: t.TempDir(), RT: &fake.Runtime{}}
		if _, err := l.FileToExec(ctx, agentwire.FileToExecParams{Path: "relative", Container: "db"}); err == nil {
			t.Fatal("a relative path must be refused")
		}
	})
	t.Run("absent dump", func(t *testing.T) {
		l := &Local{Root: t.TempDir(), RT: &fake.Runtime{}}
		if _, err := l.FileToExec(ctx, agentwire.FileToExecParams{Path: l.Root + "/absent.gz", Container: "db"}); err == nil {
			t.Fatal("an absent dump must fail")
		}
	})
	t.Run("without runtime", func(t *testing.T) {
		l := &Local{Root: t.TempDir()}
		path := l.Root + "/dump"
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := l.FileToExec(ctx, agentwire.FileToExecParams{Path: path, Container: "db"}); err == nil || !strings.Contains(err.Error(), "unavailable") {
			t.Fatalf("without runtime = %v", err)
		}
	})
	t.Run("dump is not gzip", func(t *testing.T) {
		l := &Local{Root: t.TempDir(), RT: &fake.Runtime{}}
		path := l.Root + "/dump.gz"
		if err := os.WriteFile(path, []byte("plain text"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := l.FileToExec(ctx, agentwire.FileToExecParams{Path: path, Gunzip: true, Container: "db"}); err == nil || !strings.Contains(err.Error(), "gzip") {
			t.Fatalf("bad gzip = %v", err)
		}
	})
	t.Run("exec create fails", func(t *testing.T) {
		rt := &fake.Runtime{}
		rt.ContainerExecCreateFn = func(context.Context, string, container.ExecOptions) (container.ExecCreateResponse, error) {
			return container.ExecCreateResponse{}, context.DeadlineExceeded
		}
		l := &Local{Root: t.TempDir(), RT: rt}
		path := l.Root + "/dump"
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := l.FileToExec(ctx, agentwire.FileToExecParams{Path: path, Container: "db"}); err == nil {
			t.Fatal("the create failure must surface")
		}
	})
	t.Run("exec attach fails", func(t *testing.T) {
		rt := &fake.Runtime{}
		rt.ContainerExecCreateFn = func(context.Context, string, container.ExecOptions) (container.ExecCreateResponse, error) {
			return container.ExecCreateResponse{ID: "e"}, nil
		}
		rt.ContainerExecAttachFn = func(context.Context, string, container.ExecAttachOptions) (types.HijackedResponse, error) {
			return types.HijackedResponse{}, context.DeadlineExceeded
		}
		l := &Local{Root: t.TempDir(), RT: rt}
		path := l.Root + "/dump"
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := l.FileToExec(ctx, agentwire.FileToExecParams{Path: path, Container: "db"}); err == nil {
			t.Fatal("the attach failure must surface")
		}
	})
	t.Run("feeding a dead exec", func(t *testing.T) {
		rt, server := execPipeRuntime(t, 0)
		_ = server.Close() // the exec died before reading anything
		l := &Local{Root: t.TempDir(), RT: rt}
		path := l.Root + "/dump"
		if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := l.FileToExec(ctx, agentwire.FileToExecParams{Path: path, Container: "db"}); err == nil || !strings.Contains(err.Error(), "feeding the exec") {
			t.Fatalf("dead exec = %v", err)
		}
	})
	t.Run("broken multiplex stream", func(t *testing.T) {
		rt, server := execPipeRuntime(t, 0)
		go func() {
			buf := make([]byte, len("payload"))
			_, _ = io.ReadFull(server, buf)
			_, _ = server.Write([]byte{9, 0, 0, 0, 0, 0, 0, 0})
			_ = server.Close()
		}()
		l := &Local{Root: t.TempDir(), RT: rt}
		path := l.Root + "/dump"
		if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := l.FileToExec(ctx, agentwire.FileToExecParams{Path: path, Container: "db"}); err == nil || !strings.Contains(err.Error(), "exec stream") {
			t.Fatalf("demux failure = %v", err)
		}
	})
	t.Run("inspect fails after the stream", func(t *testing.T) {
		rt, server := execPipeRuntime(t, 0)
		rt.ContainerExecInspectFn = func(context.Context, string) (container.ExecInspect, error) {
			return container.ExecInspect{}, context.DeadlineExceeded
		}
		go func() {
			buf := make([]byte, len("payload"))
			_, _ = io.ReadFull(server, buf)
			_ = server.Close()
		}()
		l := &Local{Root: t.TempDir(), RT: rt}
		path := l.Root + "/dump"
		if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := l.FileToExec(ctx, agentwire.FileToExecParams{Path: path, Container: "db"}); err == nil {
			t.Fatal("the inspect failure must surface")
		}
	})
}

// TestFileToURLErrors pins the upload's refusals: guard, absent file, an
// unparseable URL and an unreachable endpoint.
func TestFileToURLErrors(t *testing.T) {
	l := &Local{Root: t.TempDir()}
	ctx := context.Background()
	if err := l.FileToURL(ctx, agentwire.FileToURLParams{Path: "relative", URL: "https://x"}); err == nil {
		t.Fatal("a relative path must be refused")
	}
	if err := l.FileToURL(ctx, agentwire.FileToURLParams{Path: l.Root + "/absent", URL: "https://x"}); err == nil {
		t.Fatal("an absent file must fail")
	}
	path := l.Root + "/dump"
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.FileToURL(ctx, agentwire.FileToURLParams{Path: path, URL: "http://bad host/"}); err == nil {
		t.Fatal("an unparseable URL must fail")
	}
	gone := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	gone.Close() // nothing listens there anymore
	if err := l.FileToURL(ctx, agentwire.FileToURLParams{Path: path, URL: gone.URL}); err == nil {
		t.Fatal("an unreachable endpoint must fail")
	}
}

// TestURLToFileErrors pins the download's refusals: guard, a file squatting
// the parent, an unparseable URL, an unreachable endpoint, a missing parent,
// and a body cut mid-flight — which must not leave a truncated file behind.
func TestURLToFileErrors(t *testing.T) {
	l := &Local{Root: t.TempDir()}
	ctx := context.Background()
	if err := l.URLToFile(ctx, agentwire.URLToFileParams{Path: "relative", URL: "https://x"}); err == nil {
		t.Fatal("a relative path must be refused")
	}
	if err := os.WriteFile(l.Root+"/blocker", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.URLToFile(ctx, agentwire.URLToFileParams{
		Path: l.Root + "/blocker/sub/f", URL: "https://x", MakeDirs: true,
	}); err == nil {
		t.Fatal("MakeDirs through a file must fail")
	}
	if err := l.URLToFile(ctx, agentwire.URLToFileParams{Path: l.Root + "/f", URL: "http://bad host/"}); err == nil {
		t.Fatal("an unparseable URL must fail")
	}
	gone := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	gone.Close()
	if err := l.URLToFile(ctx, agentwire.URLToFileParams{Path: l.Root + "/f", URL: gone.URL}); err == nil {
		t.Fatal("an unreachable endpoint must fail")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	if err := l.URLToFile(ctx, agentwire.URLToFileParams{Path: l.Root + "/nodir/f", URL: srv.URL}); err == nil {
		t.Fatal("a missing parent without MakeDirs must fail")
	}

	// The body dies mid-flight: the declared length never arrives, and the
	// partial file must be cleaned up rather than left looking whole.
	cut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte("partial"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}))
	defer cut.Close()
	err := l.URLToFile(ctx, agentwire.URLToFileParams{Path: l.Root + "/cut.gz", URL: cut.URL})
	if err == nil {
		t.Fatal("a truncated body must fail the download")
	}
	if _, statErr := os.Stat(l.Root + "/cut.gz"); !os.IsNotExist(statErr) {
		t.Fatal("a failed download must not leave a file behind")
	}
}

// TestHashFileErrors pins the digest's refusals: guard and absent file.
func TestHashFileErrors(t *testing.T) {
	l := &Local{Root: t.TempDir()}
	ctx := context.Background()
	if _, err := l.HashFile(ctx, "relative"); err == nil {
		t.Fatal("a relative path must be refused")
	}
	if _, err := l.HashFile(ctx, l.Root+"/absent"); err == nil {
		t.Fatal("an absent file must fail")
	}
}

// TestBuildImageGuards pins the ADR-055 preconditions: context inside the
// root, a relative dockerfile, at least one tag, and a runtime capable of
// carrying the session — each refused with a typed cause before anything
// dials the daemon.
func TestBuildImageGuards(t *testing.T) {
	l := &Local{Root: t.TempDir()}
	ctx := context.Background()
	if _, err := l.BuildImage(ctx, agentwire.ImageBuildParams{
		ContextDir: "/etc", Dockerfile: "Dockerfile", Tags: []string{"t"},
	}); err == nil {
		t.Fatal("a context outside the root must be refused")
	}
	for name, dockerfile := range map[string]string{"absolute": "/tmp/Dockerfile", "escape": "../Dockerfile", "empty": ""} {
		if _, err := l.BuildImage(ctx, agentwire.ImageBuildParams{
			ContextDir: l.Root, Dockerfile: dockerfile, Tags: []string{"t"},
		}); err == nil {
			t.Fatalf("dockerfile %s must be refused", name)
		}
	}
	if _, err := l.BuildImage(ctx, agentwire.ImageBuildParams{
		ContextDir: l.Root, Dockerfile: "Dockerfile",
	}); err == nil {
		t.Fatal("a build without a tag must be refused")
	}
	// A Local without the daemon handle (or with a non-hijacking runtime)
	// refuses before dialing anything.
	if _, err := l.BuildImage(ctx, agentwire.ImageBuildParams{
		ContextDir: l.Root, Dockerfile: "Dockerfile", Tags: []string{"t"},
	}); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("no runtime = %v", err)
	}
	l.RT = &fake.Runtime{}
	if _, err := l.BuildImage(ctx, agentwire.ImageBuildParams{
		ContextDir: l.Root, Dockerfile: "Dockerfile", Tags: []string{"t"},
	}); err == nil || !strings.Contains(err.Error(), "session") {
		t.Fatalf("non-hijacking runtime = %v", err)
	}
}
