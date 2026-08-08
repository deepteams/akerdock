package tunnel

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

type benchmarkFrame struct {
	typ  MessageType
	data []byte
}

type benchmarkTunnelConn struct {
	in  <-chan benchmarkFrame
	out chan<- benchmarkFrame
}

func (c benchmarkTunnelConn) Read(ctx context.Context) (MessageType, []byte, error) {
	select {
	case frame := <-c.in:
		return frame.typ, frame.data, nil
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
}

func (c benchmarkTunnelConn) Write(ctx context.Context, typ MessageType, data []byte) error {
	frame := benchmarkFrame{typ: typ, data: append([]byte(nil), data...)}
	select {
	case c.out <- frame:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (benchmarkTunnelConn) Ping(context.Context) error { return nil }

func benchmarkTunnelPair() (benchmarkTunnelConn, benchmarkTunnelConn) {
	// A small socket-like buffer preserves write backpressure. An enormous
	// in-memory channel would unrealistically inject more than the negotiated
	// per-stream receive window in one scheduler slice.
	leftToRight := make(chan benchmarkFrame, 8)
	rightToLeft := make(chan benchmarkFrame, 8)
	return benchmarkTunnelConn{in: rightToLeft, out: leftToRight},
		benchmarkTunnelConn{in: leftToRight, out: rightToLeft}
}

func startBenchmarkTunnel(b *testing.B, maxStreams int) *Origin {
	b.Helper()
	originConn, bridgeConn := benchmarkTunnelPair()
	ctx, cancel := context.WithCancel(context.Background())
	opts := Options{
		IdleTimeout: time.Hour, MaxDuration: time.Hour,
		MaxStreams: maxStreams, MaxPendingStreams: 512,
	}
	origin := NewOriginWithOptions(originConn, opts)
	go origin.Run(ctx, opts)
	go Bridge(ctx, bridgeConn, func(context.Context) (net.Conn, error) {
		bridgeEnd, echoEnd := net.Pipe()
		go func() {
			_, _ = io.Copy(echoEnd, echoEnd)
			_ = echoEnd.Close()
		}()
		return bridgeEnd, nil
	}, opts)
	b.Cleanup(cancel)
	return origin
}

func BenchmarkTunnelBulkStream(b *testing.B) {
	origin := startBenchmarkTunnel(b, 32)
	payload := make([]byte, 256*1024)
	received := make([]byte, len(payload))
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		stream, err := origin.OpenStream(context.Background())
		if err != nil {
			b.Fatal(err)
		}
		readDone := make(chan error, 1)
		go func() {
			_, readErr := io.ReadFull(stream, received)
			readDone <- readErr
		}()
		if _, err := stream.Write(payload); err != nil {
			b.Fatal(err)
		}
		if err := <-readDone; err != nil {
			b.Fatal(err)
		}
		_ = stream.Close()
	}
}

func BenchmarkTunnelConcurrentSmallStreams(b *testing.B) {
	origin := startBenchmarkTunnel(b, 64)
	payload := make([]byte, 4*1024)
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		received := make([]byte, len(payload))
		for pb.Next() {
			stream, err := origin.OpenStream(context.Background())
			if err != nil {
				b.Error(err)
				return
			}
			if _, err := stream.Write(payload); err != nil {
				b.Error(err)
				return
			}
			if _, err := io.ReadFull(stream, received); err != nil {
				b.Error(err)
				return
			}
			_ = stream.Close()
		}
	})
}
