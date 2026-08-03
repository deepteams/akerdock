package hostops

import (
	"strings"
	"sync"
	"testing"

	"github.com/deepteams/akerdock/internal/agentwire"
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
