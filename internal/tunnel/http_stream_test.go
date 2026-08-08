package tunnel

import (
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// duplexConn carries every relayed byte of an HTTP v2 ingress session, so its
// contract is worth pinning directly rather than through the agent: both
// halves are independent, and a write is worthless until it is flushed — an
// unflushed byte is a WebSocket frame the visitor never receives.
func TestDuplexConnRelaysBothHalvesAndFlushesEveryWrite(t *testing.T) {
	inbound, peerWrites := io.Pipe()
	peerReads, outbound := io.Pipe()
	var flushes atomic.Int64
	conn := NewDuplexConn(inbound, outbound, func() error {
		flushes.Add(1)
		return nil
	}, nil)

	go func() { _, _ = peerWrites.Write([]byte("visitor")) }()
	got := make([]byte, len("visitor"))
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != "visitor" {
		t.Fatalf("read = %q, %v", got, err)
	}

	written := make(chan error, 1)
	go func() {
		_, err := conn.Write([]byte("origin"))
		written <- err
	}()
	got = make([]byte, len("origin"))
	if _, err := io.ReadFull(peerReads, got); err != nil || string(got) != "origin" {
		t.Fatalf("peer read = %q, %v", got, err)
	}
	if err := <-written; err != nil {
		t.Fatalf("write: %v", err)
	}
	if flushes.Load() != 1 {
		t.Fatalf("flushes = %d, want one per write", flushes.Load())
	}

	if conn.LocalAddr().Network() != "akerdock-ingress" || conn.RemoteAddr().String() != "http-remote" {
		t.Fatalf("addresses = %v / %v", conn.LocalAddr(), conn.RemoteAddr())
	}
	// Deadlines are accepted and ignored on purpose (see the type's comment):
	// net/http treats a deadline error as a fatal connection error.
	deadline := time.Now().Add(time.Second)
	if conn.SetDeadline(deadline) != nil || conn.SetReadDeadline(deadline) != nil || conn.SetWriteDeadline(deadline) != nil {
		t.Fatal("deadlines must be accepted silently, never refused")
	}
}

// A failed flush is a failed write: reporting success would leave the caller
// believing bytes are on their way to a peer that will never see them.
func TestDuplexConnWriteReportsFlushFailure(t *testing.T) {
	flushErr := errors.New("response already committed")
	conn := NewDuplexConn(io.NopCloser(strings.NewReader("")), nopWriteCloser{io.Discard}, func() error {
		return flushErr
	}, nil)
	if _, err := conn.Write([]byte("x")); !errors.Is(err, flushErr) {
		t.Fatalf("write error = %v, want the flush failure", err)
	}
}

// Close tears down both halves and wakes the owning HTTP handler exactly once:
// the relay closes its side, the handler's select unblocks, and a second Close
// (net/http closes what it dialed) must not double-fire either.
func TestDuplexConnCloseIsIdempotent(t *testing.T) {
	reader := &countingCloser{}
	closeErr := errors.New("writer already gone")
	writer := &countingWriteCloser{err: closeErr}
	var wakes atomic.Int64
	conn := NewDuplexConn(reader, writer, nil, func() { wakes.Add(1) })

	if err := conn.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("first close = %v, want the writer's error", err)
	}
	if err := conn.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("second close = %v, want the memoized error", err)
	}
	if wakes.Load() != 1 {
		t.Fatalf("handler woken %d times, want once", wakes.Load())
	}
	if reader.closes.Load() != 1 || writer.closes.Load() != 1 {
		t.Fatalf("halves closed %d / %d times, want once each", reader.closes.Load(), writer.closes.Load())
	}
}

type countingCloser struct{ closes atomic.Int64 }

func (c *countingCloser) Read([]byte) (int, error) { return 0, io.EOF }
func (c *countingCloser) Close() error             { c.closes.Add(1); return nil }

type countingWriteCloser struct {
	closes atomic.Int64
	err    error
}

func (c *countingWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (c *countingWriteCloser) Close() error                { c.closes.Add(1); return c.err }

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
