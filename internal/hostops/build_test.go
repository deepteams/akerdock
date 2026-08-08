package hostops

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
)

// progressSink records how many writes reached it, and what they carried.
type progressSink struct {
	mu     sync.Mutex
	writes int
	buf    strings.Builder
}

func (c *progressSink) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes++
	c.buf.Write(p)
	return len(p), nil
}

// TestLineWriterCoalesces pins why lineWriter exists: the progress display
// writes per status line, each downstream write becomes one wire frame, and a
// frame per line overruns the stream's buffer on a verbose build. Nothing may
// be lost or reordered in the process.
func TestLineWriterCoalesces(t *testing.T) {
	var sink progressSink
	lw := &lineWriter{w: &sink}

	const lines = 2000
	var want strings.Builder
	for i := range lines {
		line := "#12 " + strings.Repeat("x", 20) + string(rune('a'+i%26)) + "\n"
		want.WriteString(line)
		if n, err := lw.Write([]byte(line)); err != nil || n != len(line) {
			t.Fatalf("write %d: n=%d err=%v", i, n, err)
		}
	}
	lw.Flush()

	if sink.buf.String() != want.String() {
		t.Fatal("the coalesced output does not reproduce what was written")
	}
	// 2000 × ~25 B ≈ 50 KiB: two ChunkSize flushes and the tail, nowhere near
	// one write per line.
	maxWrites := want.Len()/agentwire.ChunkSize + 2
	if sink.writes > maxWrites {
		t.Fatalf("got %d writes for %d lines, want at most %d", sink.writes, lines, maxWrites)
	}
}

// failingWriter refuses every write — the pipe after a CloseWithError.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("pipe closed") }

// TestLineWriterSurfacesFlushError pins the failure side of the coalescer: a
// write that crosses ChunkSize flushes, and the downstream error comes back to
// the display instead of being swallowed.
func TestLineWriterSurfacesFlushError(t *testing.T) {
	lw := &lineWriter{w: failingWriter{}}
	if _, err := lw.Write(make([]byte, agentwire.ChunkSize+1)); err == nil {
		t.Fatal("the flush error must surface through Write")
	}
	// Flush on the same broken pipe stays a no-panic no-op.
	lw.Flush()
}

// hijackRuntime is the fake runtime dressed as a Hijacker: every session dial
// stalls briefly then answers the scripted error — the daemon is unreachable,
// but only once the solve actually dials.
type hijackRuntime struct {
	*fake.Runtime
	dialErr error
}

func (h *hijackRuntime) DialHijack(context.Context, string, string, map[string][]string) (net.Conn, error) {
	// Long enough for at least one flush tick to fire while the solve is in
	// flight, far below any test budget.
	time.Sleep(2 * flushInterval)
	return nil, h.dialErr
}

// TestBuildImageStreamCarriesSolveFailure drives BuildImage past every guard
// with a hijack-capable runtime whose dials fail: the call itself succeeds —
// the client is lazy — and the solve's failure arrives as the progress
// stream's terminal error, exactly the contract the executor relies on.
func TestBuildImageStreamCarriesSolveFailure(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(root+"/Dockerfile", []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := &Local{Root: root, RT: &hijackRuntime{Runtime: &fake.Runtime{}, dialErr: errors.New("daemon gone")}}
	rc, err := l.BuildImage(context.Background(), agentwire.ImageBuildParams{
		ContextDir: root, Dockerfile: "Dockerfile", Tags: []string{"app:latest", "app:v1"},
		BuildArgs: map[string]string{"WHO": "akerdock"},
		Labels:    map[string]string{"akerdock.managed": "true"},
		Target:    "final", NoCache: true,
		Secrets: map[string][]byte{"TOKEN": []byte("hush")},
	})
	if err != nil {
		t.Fatalf("the dial is lazy; BuildImage itself must succeed, got %v", err)
	}
	type verdict struct {
		out []byte
		err error
	}
	done := make(chan verdict, 1)
	go func() {
		out, readErr := io.ReadAll(rc)
		done <- verdict{out, readErr}
	}()
	select {
	case v := <-done:
		if v.err == nil {
			t.Fatalf("the solve failure must be the stream's terminal error; stream: %q", v.out)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the progress stream never terminated")
	}
	_ = rc.Close()
}

// TestLineWriterFlushIsIdempotent covers the shutdown path: the ticker and the
// final flush race by construction, and the second one must be a no-op rather
// than a duplicated tail.
func TestLineWriterFlushIsIdempotent(t *testing.T) {
	var sink progressSink
	lw := &lineWriter{w: &sink}
	if _, err := lw.Write([]byte("done\n")); err != nil {
		t.Fatal(err)
	}
	lw.Flush()
	lw.Flush()
	if got := sink.buf.String(); got != "done\n" {
		t.Fatalf("got %q, want %q", got, "done\n")
	}
	if sink.writes != 1 {
		t.Fatalf("got %d writes, want 1", sink.writes)
	}
}
