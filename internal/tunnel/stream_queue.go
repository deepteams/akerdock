package tunnel

import (
	"io"
	"net"
	"sync"
)

const (
	// streamFramePayload is the wire chunk fixed by ADR-032. Rejecting larger
	// frames prevents one peer from bypassing the bounded queue with one giant
	// allocation.
	streamFramePayload = 32 * 1024
	// Sixteen chunks bound one stalled stream to 512 KiB while leaving enough
	// room for a normal bandwidth-delay product on a development connection.
	streamQueueChunks = 16
)

var streamChunkPool = sync.Pool{New: func() any {
	b := make([]byte, streamFramePayload)
	return &b
}}

type streamChunk struct {
	buf *[]byte
	n   int
}

func acquireStreamChunk(src []byte) streamChunk {
	b := streamChunkPool.Get().(*[]byte)
	copy((*b)[:len(src)], src)
	return streamChunk{buf: b, n: len(src)}
}

func (c streamChunk) release() {
	if c.buf != nil {
		streamChunkPool.Put(c.buf)
	}
}

// streamQueue decouples the one socket decoder from a stream consumer. A
// stalled target can fill only its own bounded queue; it can never stop the
// decoder from processing sibling data or session control frames.
type streamQueue struct {
	chunks chan streamChunk
	done   chan struct{}
	once   sync.Once
}

func newStreamQueue() *streamQueue {
	return &streamQueue{
		chunks: make(chan streamChunk, streamQueueChunks),
		done:   make(chan struct{}),
	}
}

// enqueue copies data because Conn.Read implementations may reuse their
// receive buffer after the next read. It is deliberately non-blocking: false
// means this stream exceeded its bounded receive window and must be reset.
func (q *streamQueue) enqueue(p []byte) bool {
	if len(p) == 0 {
		return true
	}
	if len(p) > streamFramePayload {
		return false
	}
	chunk := acquireStreamChunk(p)
	select {
	case <-q.done:
		chunk.release()
		return false
	case q.chunks <- chunk:
		return true
	default:
		chunk.release()
		return false
	}
}

func (q *streamQueue) close() {
	q.once.Do(func() { close(q.done) })
}

// pump serializes this stream's writes away from the shared socket decoder.
// It releases every pooled chunk, including chunks left behind at shutdown.
func (q *streamQueue) pump(dst net.Conn) error {
	defer func() {
		for {
			select {
			case chunk := <-q.chunks:
				chunk.release()
			default:
				return
			}
		}
	}()
	for {
		select {
		case <-q.done:
			return net.ErrClosed
		case chunk := <-q.chunks:
			err := writeAll(dst, (*chunk.buf)[:chunk.n])
			chunk.release()
			if err != nil {
				return err
			}
		}
	}
}

func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		p = p[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}
