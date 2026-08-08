package tunnel

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// pipeConn is an in-memory Conn pair for wiring an Origin to a Bridge without a
// real WebSocket: whatever one side writes, the other reads, framed by type.
type pipeConn struct {
	in     <-chan frame
	out    chan<- frame
	closed chan struct{}
	once   sync.Once
}

func newPipe() (*pipeConn, *pipeConn) {
	a2b := make(chan frame, 64)
	b2a := make(chan frame, 64)
	closed := make(chan struct{})
	a := &pipeConn{in: b2a, out: a2b, closed: closed}
	b := &pipeConn{in: a2b, out: b2a, closed: closed}
	return a, b
}

func (p *pipeConn) Read(ctx context.Context) (MessageType, []byte, error) {
	select {
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case <-p.closed:
		return 0, nil, ErrClientClosed
	case f, ok := <-p.in:
		if !ok {
			return 0, nil, ErrClientClosed
		}
		return f.typ, f.data, nil
	}
}

func (p *pipeConn) Write(ctx context.Context, typ MessageType, data []byte) error {
	cp := make([]byte, len(data))
	copy(cp, data)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.closed:
		return io.ErrClosedPipe
	case p.out <- frame{typ, cp}:
		return nil
	}
}

func (p *pipeConn) Ping(context.Context) error { return nil }

// TestOriginBridgeRoundTrip wires an Origin (agent side) to a Bridge (laptop
// side) through the pipe, then opens a stream and checks bytes flow both ways
// through a real local echo listener — the exact shape of the ingress relay.
func TestOriginBridgeRoundTrip(t *testing.T) {
	// A local echo server stands in for the developer's app.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer func() { _ = conn.Close() }()
				buf := make([]byte, 1024)
				for {
					n, err := conn.Read(buf)
					if n > 0 {
						// echo, uppercased so we know it round-tripped
						out := make([]byte, n)
						for i := 0; i < n; i++ {
							b := buf[i]
							if b >= 'a' && b <= 'z' {
								b -= 32
							}
							out[i] = b
						}
						_, _ = conn.Write(out)
					}
					if err != nil {
						return
					}
				}
			}(c)
		}
	}()

	originConn, bridgeConn := newPipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	origin := NewOrigin(originConn)
	go origin.Run(ctx, Options{})

	// The Bridge (laptop) dials the echo server for every opened stream.
	dial := func(context.Context) (net.Conn, error) {
		return net.Dial("tcp", ln.Addr().String())
	}
	go Bridge(ctx, bridgeConn, dial, Options{})

	stream, err := origin.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	if _, err := stream.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, 5)
	_ = stream.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(stream, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "HELLO" {
		t.Fatalf("round trip got %q, want HELLO", got)
	}
}

// TestOriginOpenStreamAfterClose ensures OpenStream fails once the session ends
// rather than hanging — a re-dial must get a clean error, not a stall.
func TestOriginOpenStreamAfterClose(t *testing.T) {
	originConn, _ := newPipe()
	origin := NewOrigin(originConn)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { origin.Run(ctx, Options{}); close(done) }()
	cancel()
	<-done
	if _, err := origin.OpenStream(context.Background()); err != ErrOriginClosed {
		t.Fatalf("OpenStream after close: got %v, want ErrOriginClosed", err)
	}
}
