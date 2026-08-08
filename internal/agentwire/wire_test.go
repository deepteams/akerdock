package agentwire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
)

func TestErrorSurvivesTheWire(t *testing.T) {
	cases := []struct {
		name  string
		in    error
		check func(error) bool
	}{
		{"not found", fmt.Errorf("inspect: %w", cerrdefs.ErrNotFound), cerrdefs.IsNotFound},
		{"conflict", fmt.Errorf("create: %w", cerrdefs.ErrConflict), cerrdefs.IsConflict},
		{"not modified", fmt.Errorf("start: %w", cerrdefs.ErrNotModified), cerrdefs.IsNotModified},
		{"invalid", fmt.Errorf("params: %w", cerrdefs.ErrInvalidArgument), cerrdefs.IsInvalidArgument},
		{"unimplemented", fmt.Errorf("method: %w", cerrdefs.ErrNotImplemented), cerrdefs.IsNotImplemented},
		{"unavailable", fmt.Errorf("dial: %w", cerrdefs.ErrUnavailable), cerrdefs.IsUnavailable},
		{"canceled", fmt.Errorf("op: %w", context.Canceled), cerrdefs.IsCanceled},
	}
	for _, tc := range cases {
		wire := WireError(tc.in)
		data, err := json.Marshal(wire)
		if err != nil {
			t.Fatalf("%s: marshal: %v", tc.name, err)
		}
		var back Error
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatalf("%s: unmarshal: %v", tc.name, err)
		}
		rebuilt := back.Err()
		if !tc.check(rebuilt) {
			t.Fatalf("%s: predicate lost across the wire: %v", tc.name, rebuilt)
		}
	}
}

func TestWireErrorUntypedFallsBackToInternal(t *testing.T) {
	wire := WireError(errors.New("daemon exploded"))
	if wire.Code != CodeInternal {
		t.Fatalf("code = %q, want %q", wire.Code, CodeInternal)
	}
	if wire.Message != "daemon exploded" {
		t.Fatalf("message = %q", wire.Message)
	}
}

func TestErrorUnknownCodeStaysPlain(t *testing.T) {
	rebuilt := (&Error{Code: CodeInternal, Message: "daemon exploded"}).Err()
	if cerrdefs.IsNotFound(rebuilt) || cerrdefs.IsConflict(rebuilt) {
		t.Fatalf("internal error must not gain a typed class: %v", rebuilt)
	}
	if rebuilt.Error() != "agent: daemon exploded" {
		t.Fatalf("message = %q", rebuilt.Error())
	}
}

func TestFrameRoundTripKeepsChunkBytes(t *testing.T) {
	f := Frame{Type: FrameStream, Chunk: &StreamChunk{ID: 7, Data: []byte{0x00, 0xFF, 0x42}}}
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var back Frame
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Type != FrameStream || back.Chunk == nil || back.Chunk.ID != 7 {
		t.Fatalf("frame = %+v", back)
	}
	if !errors.Is(nil, nil) || string(back.Chunk.Data) != string([]byte{0x00, 0xFF, 0x42}) {
		t.Fatalf("chunk bytes = %v", back.Chunk.Data)
	}
}
