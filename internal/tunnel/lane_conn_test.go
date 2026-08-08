package tunnel

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMultiLaneConnPinsDataAndStreamControlsToLane(t *testing.T) {
	primary := newFakeConn()
	secondary := newFakeConn()
	var closed atomic.Int64
	lanes := NewMultiLaneConn(primary, func() error {
		closed.Add(1)
		return nil
	}, 4)
	if err := lanes.AddLane(1, secondary, func() error {
		closed.Add(1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lanes.Close() }()

	// Stream 1 prefers lane 1. Its data and eof must share that physical
	// connection so eof cannot overtake the final data frame.
	if err := lanes.Write(context.Background(), MessageBinary, dataFrame(1, []byte("payload"))); err != nil {
		t.Fatal(err)
	}
	eof, _ := json.Marshal(ctrl{T: "eof", ID: 1})
	if err := lanes.Write(context.Background(), MessageText, eof); err != nil {
		t.Fatal(err)
	}
	for _, wantType := range []MessageType{MessageBinary, MessageText} {
		select {
		case got := <-secondary.out:
			if got.typ != wantType {
				t.Fatalf("secondary frame type = %v, want %v", got.typ, wantType)
			}
		case <-time.After(time.Second):
			t.Fatal("secondary lane received no frame")
		}
	}
	select {
	case got := <-primary.out:
		t.Fatalf("stream data leaked onto primary lane: %v", got.typ)
	default:
	}

	open, _ := json.Marshal(ctrl{T: "open", ID: 2})
	if err := lanes.Write(context.Background(), MessageText, open); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-primary.out:
		if got.typ != MessageText {
			t.Fatalf("open frame type = %v", got.typ)
		}
	case <-time.After(time.Second):
		t.Fatal("primary lane received no open control")
	}

	secondary.in <- frame{typ: MessageBinary, data: dataFrame(9, []byte("incoming"))}
	typ, got, err := lanes.Read(context.Background())
	if err != nil || typ != MessageBinary || string(got[4:]) != "incoming" {
		t.Fatalf("merged read = type %v data %q err %v", typ, got, err)
	}

	if err := lanes.Close(); err != nil {
		t.Fatal(err)
	}
	if got := closed.Load(); got != 2 {
		t.Fatalf("closed %d lanes, want 2", got)
	}
}

func TestMultiLaneConnRejectsDuplicateAndOutOfRangeLane(t *testing.T) {
	lanes := NewMultiLaneConn(newFakeConn(), nil, 4)
	defer func() { _ = lanes.Close() }()
	if err := lanes.AddLane(1, newFakeConn(), nil); err != nil {
		t.Fatal(err)
	}
	if err := lanes.AddLane(1, newFakeConn(), nil); err == nil {
		t.Fatal("duplicate lane was accepted")
	}
	if err := lanes.AddLane(4, newFakeConn(), nil); err == nil {
		t.Fatal("out-of-range lane was accepted")
	}
}

func TestMultiLaneConnBlockedLaneDoesNotHoldSessionWriter(t *testing.T) {
	primary := newFakeConn()
	secondary := newFakeConn()
	lanes := NewMultiLaneConn(primary, nil, 4)
	if err := lanes.AddLane(1, secondary, nil); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lanes.Close() }()
	for len(secondary.out) < cap(secondary.out) {
		secondary.out <- frame{typ: MessageBinary}
	}

	var legacySessionWriter sync.Mutex
	blocked := make(chan error, 1)
	go func() {
		blocked <- writeTunnelMessage(context.Background(), lanes, &legacySessionWriter,
			MessageBinary, dataFrame(1, []byte("blocked")))
	}()
	time.Sleep(10 * time.Millisecond)

	siblingCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := writeTunnelMessage(siblingCtx, lanes, &legacySessionWriter,
		MessageBinary, dataFrame(4, []byte("sibling"))); err != nil {
		t.Fatalf("sibling lane was blocked: %v", err)
	}
	select {
	case got := <-primary.out:
		if string(got.data[4:]) != "sibling" {
			t.Fatalf("primary got %q", got.data)
		}
	case <-siblingCtx.Done():
		t.Fatal("sibling frame did not reach primary lane")
	}

	<-secondary.out
	if err := <-blocked; err != nil {
		t.Fatalf("blocked lane did not resume: %v", err)
	}
}
