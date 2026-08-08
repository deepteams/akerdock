package tunnel

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func newControlPair(t *testing.T) (*LineControl, *LineControl) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() {
		_ = a.Close()
		_ = b.Close()
	})
	return NewLineControl(a, a, nil, a.Close), NewLineControl(b, b, nil, b.Close)
}

func TestHTTPOriginOpensIndependentStream(t *testing.T) {
	agentControl, clientControl := newControlPair(t)
	origin := NewHTTPOrigin(agentControl, Options{MaxStreams: 2})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go origin.Run(ctx, Options{})

	opened := make(chan originOpenResult, 1)
	go func() {
		conn, err := origin.OpenStream(ctx)
		opened <- originOpenResult{conn: conn, err: err}
	}()
	frame, err := clientControl.Receive()
	if err != nil || frame.Type != "open" || frame.ID == 0 {
		t.Fatalf("open frame = %+v, %v", frame, err)
	}
	agentData, clientData := net.Pipe()
	defer func() { _ = clientData.Close() }()
	if !origin.WantsStream(frame.ID) {
		t.Fatal("origin should advertise the pending stream")
	}
	if err := origin.AttachStream(frame.ID, agentData); err != nil {
		t.Fatalf("AttachStream: %v", err)
	}
	result := <-opened
	if result.err != nil {
		t.Fatalf("OpenStream: %v", result.err)
	}
	defer func() { _ = result.conn.Close() }()

	go func() { _, _ = clientData.Write([]byte("response")) }()
	got := make([]byte, len("response"))
	if _, err := io.ReadFull(result.conn, got); err != nil || string(got) != "response" {
		t.Fatalf("read = %q, %v", got, err)
	}
	go func() { _, _ = result.conn.Write([]byte("request")) }()
	got = make([]byte, len("request"))
	if _, err := io.ReadFull(clientData, got); err != nil || string(got) != "request" {
		t.Fatalf("peer read = %q, %v", got, err)
	}
}

func TestHTTPOriginOpenFailureAndQueueBound(t *testing.T) {
	agentControl, clientControl := newControlPair(t)
	origin := NewHTTPOrigin(agentControl, Options{
		MaxStreams: 1, MaxPendingStreams: 1, StreamQueueTimeout: time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go origin.Run(ctx, Options{})

	firstErr := make(chan error, 1)
	go func() {
		_, err := origin.OpenStream(ctx)
		firstErr <- err
	}()
	open, err := clientControl.Receive()
	if err != nil || open.Type != "open" {
		t.Fatalf("open = %+v, %v", open, err)
	}
	if err := clientControl.Send(ctx, HTTPControlFrame{
		Type: "open_err", ID: open.ID, Code: "dial_failed", Msg: "local app refused",
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-firstErr; err == nil {
		t.Fatal("the client's open_err must fail OpenStream")
	}

	// Occupy the only active slot with an attached stream.
	activeResult := make(chan originOpenResult, 1)
	go func() {
		conn, err := origin.OpenStream(ctx)
		activeResult <- originOpenResult{conn: conn, err: err}
	}()
	activeOpen, _ := clientControl.Receive()
	a, b := net.Pipe()
	defer func() { _ = b.Close() }()
	if err := origin.AttachStream(activeOpen.ID, a); err != nil {
		t.Fatal(err)
	}
	active := <-activeResult
	if active.err != nil {
		t.Fatal(active.err)
	}

	queuedCtx, cancelQueued := context.WithCancel(ctx)
	queued := make(chan error, 1)
	go func() {
		_, err := origin.OpenStream(queuedCtx)
		queued <- err
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		origin.admissionMu.Lock()
		count := origin.queued
		origin.admissionMu.Unlock()
		if count == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := origin.OpenStream(ctx); !errors.Is(err, ErrOriginQueueFull) {
		t.Fatalf("open beyond queue = %v, want ErrOriginQueueFull", err)
	}
	cancelQueued()
	if err := <-queued; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled queue = %v", err)
	}
	_ = active.conn.Close()
}

func TestHTTPOriginSessionCloseReason(t *testing.T) {
	agentControl, clientControl := newControlPair(t)
	origin := NewHTTPOrigin(agentControl, Options{})
	done := make(chan EndReason, 1)
	go func() { done <- origin.Run(context.Background(), Options{}) }()
	if err := clientControl.Send(context.Background(), HTTPControlFrame{
		Type: "session_close", Reason: "user_close",
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case reason := <-done:
		if reason != EndUserClose {
			t.Fatalf("reason = %q", reason)
		}
	case <-time.After(time.Second):
		t.Fatal("HTTP origin did not receive the close reason")
	}
}
