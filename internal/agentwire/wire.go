// Package agentwire defines the wire protocol of the agent channel: the
// ADR-041 observation frames (v1) and the ADR-052 typed command frames (v2).
// Both sides — the agent (internal/waker) and the control plane
// (internal/handlers, internal/dockerruntime) — share these types, so the
// vocabulary is defined exactly once and every command on the wire is one of
// the enumerated methods below, never an opaque byte stream to the daemon.
package agentwire

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	cerrdefs "github.com/containerd/errdefs"
)

// Channel subprotocols. The agent offers both; the control plane picks v2
// when it speaks commands, and an older side falls back to v1 (observations
// only) — the rail upgrades without a flag day.
const (
	SubprotocolV1 = "akerdock-agent-v1"
	SubprotocolV2 = "akerdock-agent-v2"
	// SubprotocolRelay is the worker→api bridge (ADR-052 §8): a process that
	// does not terminate agent WebSockets sends its typed commands here, and
	// the api forwards them onto the target server's live channel.
	SubprotocolRelay = "akerdock-relay-v1"
)

// Frame types.
const (
	// FrameObservations carries an acked observation batch (agent → CP).
	FrameObservations = "observations"
	// FrameAck acknowledges an observation batch by sequence (CP → agent).
	FrameAck = "ack"
	// FrameCommand carries one typed command (CP → agent, v2).
	FrameCommand = "cmd"
	// FrameResult answers a command by id (agent → CP, v2).
	FrameResult = "res"
	// FrameStream carries one chunk of a command's output stream — logs
	// follow, pull/push progress, daemon events (agent → CP, v2).
	FrameStream = "stream"
	// FrameCancel aborts a command in flight and closes its stream
	// (CP → agent, v2). Identified by the command id.
	FrameCancel = "cancel"
)

// Observation is one pushed fact (ADR-040). Types: "container_state" (a
// managed container changed state), "stz_woken" (a wake started containers),
// "heartbeat" (the agent is alive).
type Observation struct {
	Type         string    `json:"type"`
	At           time.Time `json:"at"`
	Container    string    `json:"container,omitempty"`
	State        string    `json:"state,omitempty"`
	ResourceUUID string    `json:"resource_uuid,omitempty"`
}

// Frame is one message on the channel, both directions. Exactly one of the
// role-specific fields is set, per Type.
type Frame struct {
	Type string `json:"type"`

	// Observation batching (v1 semantics, unchanged in v2).
	Seq          int64         `json:"seq,omitempty"`
	Observations []Observation `json:"observations,omitempty"`
	Denied       bool          `json:"denied,omitempty"`

	// Command traffic (v2).
	Cmd    *Command     `json:"cmd,omitempty"`
	Res    *Result      `json:"res,omitempty"`
	Chunk  *StreamChunk `json:"chunk,omitempty"`
	Cancel int64        `json:"cancel,omitempty"`
}

// Command is one typed method call. Method is a name from the enumerated
// vocabulary (method.go); Params is the JSON of that method's params struct —
// the Docker SDK types, which are the Engine API's own wire types.
type Command struct {
	ID     int64           `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Result answers the command with the same ID: a body (the method's return
// value as JSON) or an error, never both. A streaming method's Result only
// acknowledges the open; its output arrives as StreamChunks and its end as a
// chunk with EOF or Err set.
type Result struct {
	ID   int64           `json:"id"`
	Body json.RawMessage `json:"body,omitempty"`
	Err  *Error          `json:"error,omitempty"`
}

// StreamChunk is one piece of a command's output stream. Data is raw bytes
// (base64 on the wire); EOF marks a clean end, Err a broken one.
type StreamChunk struct {
	ID   int64  `json:"id"`
	Data []byte `json:"data,omitempty"`
	EOF  bool   `json:"eof,omitempty"`
	Err  *Error `json:"error,omitempty"`
}

// Error codes: the daemon's typed answers (errdefs), flattened for the wire
// so the control plane can re-wrap them and keep IsNotFound/IsConflict
// working across the channel.
const (
	CodeNotFound      = "not_found"
	CodeConflict      = "conflict"
	CodeNotModified   = "not_modified"
	CodeInvalid       = "invalid"
	CodeUnavailable   = "unavailable"
	CodeCanceled      = "canceled"
	CodeUnimplemented = "unimplemented"
	CodeInternal      = "internal"
)

// Error is a command failure on the wire.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WireError flattens a daemon error into its wire form.
func WireError(err error) *Error {
	code := CodeInternal
	switch {
	case cerrdefs.IsNotFound(err):
		code = CodeNotFound
	case cerrdefs.IsConflict(err):
		code = CodeConflict
	case cerrdefs.IsNotModified(err):
		code = CodeNotModified
	case cerrdefs.IsInvalidArgument(err):
		code = CodeInvalid
	case cerrdefs.IsUnavailable(err):
		code = CodeUnavailable
	case cerrdefs.IsCanceled(err):
		code = CodeCanceled
	case cerrdefs.IsNotImplemented(err):
		code = CodeUnimplemented
	}
	return &Error{Code: code, Message: err.Error()}
}

// Err rebuilds a typed error from its wire form: the message is preserved and
// the matching errdefs sentinel is wrapped in, so dockerruntime's predicates
// answer the same on both sides of the channel.
func (e *Error) Err() error {
	var sentinel error
	switch e.Code {
	case CodeNotFound:
		sentinel = cerrdefs.ErrNotFound
	case CodeConflict:
		sentinel = cerrdefs.ErrConflict
	case CodeNotModified:
		sentinel = cerrdefs.ErrNotModified
	case CodeInvalid:
		sentinel = cerrdefs.ErrInvalidArgument
	case CodeUnavailable:
		sentinel = cerrdefs.ErrUnavailable
	case CodeCanceled:
		sentinel = context.Canceled // what cerrdefs.IsCanceled matches
	case CodeUnimplemented:
		sentinel = cerrdefs.ErrNotImplemented
	default:
		return fmt.Errorf("agent: %s", e.Message)
	}
	return fmt.Errorf("agent: %s: %w", e.Message, sentinel)
}
