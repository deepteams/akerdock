package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/deepteams/akerdock/internal/tunnel"
)

// HTTPConn presents an HTTP attach pair — the session request's control wire
// and the one data stream carrying the PTY's bytes — as the Conn the bridge
// reads and writes (ADR-064 §3). It is the exact counterpart of the WebSocket
// adapter: the transport changes, the bridge does not.
//
// An HTTP stream has no frames to be typed by, so the message type IS the
// wire it travels on: a text message is one control frame on the session
// request, a binary message is bytes on the data stream. Reading merges the
// two sources back into one ordered-per-source flow of typed messages —
// the same merge MultiLaneConn performs for WebSocket lanes.
//
// The adapter is direction-agnostic: the control plane bridges a PTY with it,
// and the CLI pumps a local TTY through the very same code.
type HTTPConn struct {
	control *tunnel.LineControl
	data    io.ReadWriteCloser

	ctx      context.Context
	cancel   context.CancelFunc
	incoming chan httpMessage

	closeOnce sync.Once
	closeErr  error
}

// httpMessage is one merged message: what Read hands the bridge.
type httpMessage struct {
	typ  MessageType
	data []byte
	err  error
}

// httpControlPayload is the JSON both halves of the terminal protocol already
// speak — {"type":"resize","cols":N,"rows":N} in, {"type":"end","reason":…}
// out. The adapter translates it field for field into a control frame and
// back, so neither the bridge nor the CLI learns a second vocabulary.
type httpControlPayload struct {
	Type   string `json:"type"`
	Cols   int    `json:"cols,omitempty"`
	Rows   int    `json:"rows,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Msg carries the operator sentence of an end frame (ADR-066 §3). The
	// control wire has always had the field; dropping it here is what made the
	// terminal print a bare reason where the port-forward prints a sentence.
	Msg string `json:"msg,omitempty"`
}

// Liveness frames. They are the transport's business, not the session's: a
// ping is answered here and never wakes the bridge, which has its own
// heartbeat and its own idle bound.
const (
	httpControlPing = "ping"
	httpControlPong = "pong"
)

// NewHTTPConn merges an already-open control wire and data stream. Both are
// owned by the returned Conn from here on: Close tears down the pair.
func NewHTTPConn(control *tunnel.LineControl, data io.ReadWriteCloser) *HTTPConn {
	ctx, cancel := context.WithCancel(context.Background())
	conn := &HTTPConn{
		control: control,
		data:    data,
		ctx:     ctx,
		cancel:  cancel,
		// Two producers, each of which may have one message in flight while
		// the bridge is busy with the other.
		incoming: make(chan httpMessage, 2),
	}
	go conn.readControl()
	go conn.readData()
	return conn
}

// readControl turns control frames into text messages.
func (c *HTTPConn) readControl() {
	for {
		frame, err := c.control.Receive()
		if err != nil {
			c.deliver(httpMessage{err: endOfWire(err)})
			return
		}
		switch frame.Type {
		case httpControlPing:
			// Answered without a round trip through the session: the peer only
			// wants to know the wire still carries bytes.
			if sendErr := c.control.Send(c.ctx, tunnel.HTTPControlFrame{Type: httpControlPong}); sendErr != nil {
				c.deliver(httpMessage{err: endOfWire(sendErr)})
				return
			}
			continue
		case httpControlPong:
			continue
		}
		payload, err := json.Marshal(httpControlPayload{
			Type: frame.Type, Cols: frame.Cols, Rows: frame.Rows, Reason: frame.Reason, Msg: frame.Msg,
		})
		if err != nil {
			continue
		}
		if !c.deliver(httpMessage{typ: MessageText, data: payload}) {
			return
		}
	}
}

// readData turns PTY bytes into binary messages.
func (c *HTTPConn) readData() {
	buf := make([]byte, 32<<10)
	for {
		n, err := c.data.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if !c.deliver(httpMessage{typ: MessageBinary, data: chunk}) {
				return
			}
		}
		if err != nil {
			c.deliver(httpMessage{err: endOfWire(err)})
			return
		}
	}
}

// deliver hands one message to Read, or gives up once the conn is closed.
func (c *HTTPConn) deliver(msg httpMessage) bool {
	select {
	case c.incoming <- msg:
		return true
	case <-c.ctx.Done():
		return false
	}
}

// endOfWire classifies a half that stopped. A clean end of stream is the peer
// closing its request — a wanted close, which the bridge must not report as a
// vanished peer.
func endOfWire(err error) error {
	if errors.Is(err, io.EOF) {
		return ErrClientClosed
	}
	return err
}

// Read returns the next message from either half.
func (c *HTTPConn) Read(ctx context.Context) (MessageType, []byte, error) {
	select {
	case msg := <-c.incoming:
		return msg.typ, msg.data, msg.err
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case <-c.ctx.Done():
		return 0, nil, ErrClientClosed
	}
}

// Write routes on the type: a control message travels on the session request,
// bytes on the data stream. Nothing is re-framed — that is the whole point of
// carrying the two on two wires (ADR-064 §3).
func (c *HTTPConn) Write(ctx context.Context, typ MessageType, data []byte) error {
	if typ != MessageText {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := c.data.Write(data)
		return err
	}
	var payload httpControlPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("terminal: control message is not JSON: %w", err)
	}
	return c.control.Send(ctx, tunnel.HTTPControlFrame{
		Type: payload.Type, Cols: payload.Cols, Rows: payload.Rows, Reason: payload.Reason, Msg: payload.Msg,
	})
}

// Ping is the §24.4 heartbeat, on the control wire: the data stream carries
// keystrokes and output only, and a silent shell must still reveal a peer that
// vanished.
func (c *HTTPConn) Ping(ctx context.Context) error {
	return c.control.Send(ctx, tunnel.HTTPControlFrame{Type: httpControlPing})
}

// Close tears both halves down once, and wakes any pending Read.
func (c *HTTPConn) Close() error {
	c.closeOnce.Do(func() {
		c.cancel()
		if err := c.data.Close(); err != nil {
			c.closeErr = err
		}
		if err := c.control.Close(); c.closeErr == nil && err != nil {
			c.closeErr = err
		}
	})
	return c.closeErr
}
