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

	writeMu sync.Mutex
	once    sync.Once
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

func (c *duplexConn) Close() error {
	var err error
	c.once.Do(func() {
		c.close()
		if closeErr := c.writer.Close(); closeErr != nil {
			err = closeErr
		}
		if closeErr := c.reader.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	})
	return err
}

func (c *duplexConn) LocalAddr() net.Addr              { return tunnelAddr("http-local") }
func (c *duplexConn) RemoteAddr() net.Addr             { return tunnelAddr("http-remote") }
func (c *duplexConn) SetDeadline(time.Time) error      { return nil }
func (c *duplexConn) SetReadDeadline(time.Time) error  { return nil }
func (c *duplexConn) SetWriteDeadline(time.Time) error { return nil }

type tunnelAddr string

func (a tunnelAddr) Network() string { return "akerdock-ingress" }
func (a tunnelAddr) String() string  { return string(a) }
