package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/deepteams/akerdock/internal/tunnel"
)

// pipeStream is one HTTP attach stream as both peers see it: a request body to
// read and a response body to write, each a one-way pipe.
type pipeStream struct {
	reader *io.PipeReader
	writer *io.PipeWriter
}

func (s pipeStream) Read(p []byte) (int, error)  { return s.reader.Read(p) }
func (s pipeStream) Write(p []byte) (int, error) { return s.writer.Write(p) }

func (s pipeStream) Close() error {
	_ = s.writer.Close()
	return s.reader.Close()
}

// httpPair builds the two wires of an HTTP attach: the conn under test on one
// side, and on the other the peer's control wire and data stream.
type httpPair struct {
	conn *HTTPConn
	peer *tunnel.LineControl
	// The peer's half of the data stream, split so a test can end it cleanly
	// (a closed request) or violently (a reset stream).
	dataIn  *io.PipeWriter
	dataOut *io.PipeReader
}

func newHTTPPair(t *testing.T) *httpPair {
	t.Helper()
	controlReader, peerControlWriter := io.Pipe()
	peerControlReader, controlWriter := io.Pipe()
	local := tunnel.NewLineControl(controlReader, controlWriter, nil, func() error {
		_ = controlWriter.Close()
		return controlReader.Close()
	})
	peer := tunnel.NewLineControl(peerControlReader, peerControlWriter, nil, func() error {
		_ = peerControlWriter.Close()
		return peerControlReader.Close()
	})

	dataReader, dataIn := io.Pipe()
	dataOut, dataWriter := io.Pipe()
	pair := &httpPair{
		conn:    NewHTTPConn(local, pipeStream{reader: dataReader, writer: dataWriter}),
		peer:    peer,
		dataIn:  dataIn,
		dataOut: dataOut,
	}
	t.Cleanup(func() {
		_ = pair.conn.Close()
		_ = peer.Close()
		_ = dataIn.Close()
		_ = dataOut.Close()
	})
	return pair
}

func (p *httpPair) read(t *testing.T) (MessageType, []byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return p.conn.Read(ctx)
}

// send hands the conn one control frame. The pipe is unbuffered, so this
// returns once the conn's reader took it — which is the ordering the
// assertions below rely on.
func (p *httpPair) send(t *testing.T, frame tunnel.HTTPControlFrame) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.peer.Send(ctx, frame); err != nil {
		t.Fatalf("send %s: %v", frame.Type, err)
	}
}

// The merge: two wires in, one flow of typed messages out. A control frame is
// a text message, the data stream is binary, and neither is re-framed.
func TestHTTPConnMergesControlAndDataIntoTypedMessages(t *testing.T) {
	pair := newHTTPPair(t)

	pair.send(t, tunnel.HTTPControlFrame{Type: "resize", Cols: 132, Rows: 43})
	typ, data, err := pair.read(t)
	if err != nil || typ != MessageText {
		t.Fatalf("read = %d, %q, %v — a control frame must arrive as a text message", typ, data, err)
	}
	var resize controlMessage
	if err := json.Unmarshal(data, &resize); err != nil {
		t.Fatal(err)
	}
	if resize.Type != "resize" || resize.Cols != 132 || resize.Rows != 43 {
		t.Fatalf("resize = %+v — the geometry must survive the control wire", resize)
	}

	go func() { _, _ = pair.dataIn.Write([]byte("ls -l\n")) }()
	typ, data, err = pair.read(t)
	if err != nil || typ != MessageBinary || string(data) != "ls -l\n" {
		t.Fatalf("read = %d, %q, %v — the data stream must arrive as binary", typ, data, err)
	}
}

// Routing, the other way: a text message is one control frame, a binary
// message is bytes on the data stream. Nothing carries a length-and-type
// prefix (ADR-064 §3).
func TestHTTPConnRoutesWritesOnTheMessageType(t *testing.T) {
	pair := newHTTPPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	payload, err := json.Marshal(endMessage{Type: "end", Reason: EndIdleTimeout})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = pair.conn.Write(ctx, MessageText, payload) }()
	frame, err := pair.peer.Receive()
	if err != nil {
		t.Fatal(err)
	}
	if frame.Type != "end" || frame.Reason != string(EndIdleTimeout) {
		t.Fatalf("frame = %+v — the end reason travels on the control wire", frame)
	}

	go func() { _ = pair.conn.Write(ctx, MessageBinary, []byte("motd\r\n")) }()
	got := make([]byte, 6)
	if _, err := io.ReadFull(pair.dataOut, got); err != nil || string(got) != "motd\r\n" {
		t.Fatalf("data stream carried %q, %v", got, err)
	}

	// A text message that is not JSON is a programming error, not a frame to
	// invent: it must be refused rather than silently sent as bytes.
	if err := pair.conn.Write(ctx, MessageText, []byte("not json")); err == nil {
		t.Fatal("a non-JSON control message must be refused")
	}
}

// Liveness is the transport's business: a ping goes out on the control wire,
// and an incoming one is answered there without ever waking the bridge.
func TestHTTPConnAnswersLivenessOnTheControlWire(t *testing.T) {
	pair := newHTTPPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() { _ = pair.conn.Ping(ctx) }()
	frame, err := pair.peer.Receive()
	if err != nil || frame.Type != httpControlPing {
		t.Fatalf("ping = %+v, %v", frame, err)
	}

	pair.send(t, tunnel.HTTPControlFrame{Type: httpControlPing})
	answer, err := pair.peer.Receive()
	if err != nil || answer.Type != httpControlPong {
		t.Fatalf("answer = %+v, %v — a ping must be answered on the wire it arrived on", answer, err)
	}
	// A pong is equally invisible, and the resize behind it still lands: the
	// bridge sees the session's messages only.
	pair.send(t, tunnel.HTTPControlFrame{Type: httpControlPong})
	pair.send(t, tunnel.HTTPControlFrame{Type: "resize", Cols: 100, Rows: 40})
	typ, data, err := pair.read(t)
	if err != nil || typ != MessageText {
		t.Fatalf("read = %d, %q, %v", typ, data, err)
	}
	var msg controlMessage
	if err := json.Unmarshal(data, &msg); err != nil || msg.Type != "resize" {
		t.Fatalf("liveness leaked into the session: %q", data)
	}
}

// A half that ends cleanly is the peer closing its request — a wanted close,
// which the bridge must read as user_close and not as a vanished peer.
func TestHTTPConnClassifiesTheEndOfEachHalf(t *testing.T) {
	t.Run("control closed", func(t *testing.T) {
		pair := newHTTPPair(t)
		_ = pair.peer.Close()
		if _, _, err := pair.read(t); !errors.Is(err, ErrClientClosed) {
			t.Fatalf("err = %v, want ErrClientClosed", err)
		}
	})

	t.Run("data closed", func(t *testing.T) {
		pair := newHTTPPair(t)
		_ = pair.dataIn.Close()
		if _, _, err := pair.read(t); !errors.Is(err, ErrClientClosed) {
			t.Fatalf("err = %v, want ErrClientClosed", err)
		}
	})

	t.Run("data reset", func(t *testing.T) {
		pair := newHTTPPair(t)
		// A stream that dies mid-flight is a disconnect, not a close: the
		// distinction is the end reason the session row records.
		_ = pair.dataIn.CloseWithError(errors.New("stream reset"))
		_, _, err := pair.read(t)
		if err == nil || errors.Is(err, ErrClientClosed) {
			t.Fatalf("err = %v, want a transport error", err)
		}
	})
}

// Close tears both halves down and wakes a pending Read: the bridge's
// guaranteed teardown must not depend on the peer cooperating.
func TestHTTPConnCloseWakesAPendingRead(t *testing.T) {
	pair := newHTTPPair(t)
	done := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		_, _, err := pair.conn.Read(context.Background())
		done <- err
	}()
	// Nothing observable says "this goroutine is parked in Read", so the
	// scheduler gets a moment rather than a polled condition that cannot
	// exist.
	<-started
	time.Sleep(20 * time.Millisecond)
	if err := pair.conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrClientClosed) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close left a Read pending")
	}
	// Closing twice is what a handler and a defer do between them.
	if err := pair.conn.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// The whole point of the adapter: the bridge runs unchanged over the HTTP
// pair, resizes the PTY from a control frame, and reports the end on the wire
// the session request holds open.
func TestBridgeRunsOverTheHTTPPair(t *testing.T) {
	pair := newHTTPPair(t)
	pty := newFakePTY()
	reasons := make(chan EndReason, 1)
	go func() {
		reasons <- Bridge(context.Background(), pair.conn, pty, Options{
			IdleTimeout: generous, MaxDuration: generous, Heartbeat: generous,
		})
	}()

	pair.send(t, tunnel.HTTPControlFrame{Type: "resize", Cols: 120, Rows: 40})
	go func() { _, _ = pair.dataIn.Write([]byte("whoami\n")) }()
	waitFor(3*time.Second, func() bool { return pty.writtenBytes() == "whoami\n" })
	if got := pty.writtenBytes(); got != "whoami\n" {
		t.Fatalf("the pty received %q — keystrokes travel on the data stream", got)
	}
	waitFor(3*time.Second, func() bool {
		pty.mu.Lock()
		defer pty.mu.Unlock()
		return len(pty.resizes) == 1
	})
	pty.mu.Lock()
	resizes := append([][2]int(nil), pty.resizes...)
	pty.mu.Unlock()
	if len(resizes) != 1 || resizes[0] != [2]int{120, 40} {
		t.Fatalf("resizes = %v — the control wire must carry the geometry", resizes)
	}

	pty.out <- []byte("$ ")
	got := make([]byte, 2)
	if _, err := io.ReadFull(pair.dataOut, got); err != nil || string(got) != "$ " {
		t.Fatalf("the data stream carried %q, %v", got, err)
	}

	_ = pty.Close() // the shell exits
	// The end frame is read first, and deliberately: the bridge writes it
	// before returning, so a test waiting on the reason first would hold the
	// wire it is waiting for.
	frame, err := pair.peer.Receive()
	if err != nil || frame.Type != "end" || frame.Reason != string(EndUserClose) {
		t.Fatalf("end frame = %+v, %v", frame, err)
	}
	select {
	case reason := <-reasons:
		if reason != EndUserClose {
			t.Fatalf("reason = %q", reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the bridge never ended")
	}
}

// The control frame has always had a message field; the terminal's mapping
// dropped it in both directions (ADR-066 §3). Both halves are asserted here
// because the adapter is direction-agnostic — the control plane writes the end
// frame, and the CLI reads it through this very code.
func TestHTTPConnCarriesTheEndMessageBothWays(t *testing.T) {
	pair := newHTTPPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	payload, err := json.Marshal(endMessage{
		Type: "end", Reason: EndTargetUnreachable, Msg: "the server is not reachable over SSH right now",
	})
	if err != nil {
		t.Fatal(err)
	}
	// The control pipe is unbuffered: the write only completes once the peer
	// takes the frame, so the two must overlap.
	written := make(chan error, 1)
	go func() { written <- pair.conn.Write(ctx, MessageText, payload) }()
	frame, err := pair.peer.Receive()
	if err != nil {
		t.Fatal(err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if frame.Type != "end" || frame.Reason != string(EndTargetUnreachable) {
		t.Fatalf("frame = %+v", frame)
	}
	if frame.Msg != "the server is not reachable over SSH right now" {
		t.Fatalf("frame.Msg = %q — the sentence must reach the wire", frame.Msg)
	}

	pair.send(t, tunnel.HTTPControlFrame{
		Type: "end", Reason: string(EndTargetUnreachable), Msg: "the container does not exist on the server",
	})
	typ, data, err := pair.read(t)
	if err != nil || typ != MessageText {
		t.Fatalf("read = %d, %q, %v", typ, data, err)
	}
	var got endMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Msg != "the container does not exist on the server" {
		t.Fatalf("msg = %q — the sentence must survive the merge", got.Msg)
	}

	// The key is pinned, not merely round-tripped through our own structs. The
	// CLI reads {"type":"end","reason":…,"msg":…} by name, and a tag that says
	// anything else fails silently: the frame still parses, the sentence is
	// simply gone and the developer reads a bare reason.
	if !strings.Contains(string(data), `"msg":"the container does not exist on the server"`) {
		t.Fatalf("merged payload = %s — the sentence must travel under the key `msg`", data)
	}
	onTheWire, err := json.Marshal(tunnel.HTTPControlFrame{Type: "end", Msg: "sentence"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(onTheWire), `"msg":"sentence"`) {
		t.Fatalf("control frame = %s — the terminal and the egress wire must name the field alike", onTheWire)
	}
}
