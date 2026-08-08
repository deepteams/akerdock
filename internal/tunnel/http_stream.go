package tunnel

import (
	"io"
	"net"
	"sync"
	"time"
)

// duplexConn adapts a full-duplex HTTP request body/response body pair to the
// net.Conn shape expected by http.Transport and the ingress bridge.
type duplexConn struct {
	reader io.ReadCloser
	writer io.WriteCloser
	flush  func() error
	close  func()

	writeMu  sync.Mutex
	once     sync.Once
	closeErr error
}

// NewDuplexConn builds a stream connection. flush may be nil on the client;
// closeFn is used to cancel the owning HTTP request or wake its handler.
func NewDuplexConn(reader io.ReadCloser, writer io.WriteCloser, flush func() error, closeFn func()) net.Conn {
	if flush == nil {
		flush = func() error { return nil }
	}
	if closeFn == nil {
		closeFn = func() {}
	}
	return &duplexConn{reader: reader, writer: writer, flush: flush, close: closeFn}
}

func (c *duplexConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func (c *duplexConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	n, err := c.writer.Write(p)
	if err == nil {
		err = c.flush()
	}
	return n, err
}

// Close tears both halves down once and memoizes the verdict: net/http and the
// bridge both close what they were handed, and the second caller must not be
// told the stream ended cleanly when it did not.
func (c *duplexConn) Close() error {
	c.once.Do(func() {
		c.close()
		if closeErr := c.writer.Close(); closeErr != nil {
			c.closeErr = closeErr
		}
		if closeErr := c.reader.Close(); c.closeErr == nil && closeErr != nil {
			c.closeErr = closeErr
		}
	})
	return c.closeErr
}

func (c *duplexConn) LocalAddr() net.Addr  { return tunnelAddr("http-local") }
func (c *duplexConn) RemoteAddr() net.Addr { return tunnelAddr("http-remote") }

// Deadlines are accepted and ignored: the two halves are HTTP bodies, whose
// lifetime belongs to the owning request's context, not to a socket timer.
// The callers are written accordingly — the relay's Transport runs with
// ResponseHeaderTimeout and ExpectContinueTimeout at zero — and any future
// timeout configured on a Transport dialing this conn would be a silent no-op
// rather than an error. Returning one instead would break net/http, which
// treats a deadline failure as a fatal connection error.
func (c *duplexConn) SetDeadline(time.Time) error      { return nil }
func (c *duplexConn) SetReadDeadline(time.Time) error  { return nil }
func (c *duplexConn) SetWriteDeadline(time.Time) error { return nil }

type tunnelAddr string

func (a tunnelAddr) Network() string { return "akerdock-ingress" }
func (a tunnelAddr) String() string  { return string(a) }
