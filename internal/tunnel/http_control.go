package tunnel

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// HTTP ingress v2 protocol identifiers and headers (ADR-061). The attach key
// is ephemeral and must never be logged or persisted.
const (
	IngressHTTPProtocol       = "akerdock-ingress-http-v2"
	IngressCapabilitiesHeader = "Akerdock-Ingress-Transports"
	IngressProtocolHeader     = "Akerdock-Ingress-Protocol"
	IngressAttachKeyHeader    = "Akerdock-Ingress-Key"
	IngressSessionHeader      = "Akerdock-Ingress-Session"
	IngressStreamHeader       = "Akerdock-Ingress-Stream"
	IngressTransportHeader    = "Akerdock-Ingress-Transport"
	IngressLaneHeader         = "Akerdock-Ingress-Lane"
	IngressWebSocketV2        = "akerdock-ingress-v2"

	IngressControlContentType = "application/vnd.akerdock.ingress-control.v2+json"
	IngressStreamContentType  = "application/vnd.akerdock.ingress-stream.v2"
)

// HTTPAttachProtocol names one access path's HTTP v2 attach wire. ADR-027's
// standing rule is a distinct name per access path, and it is load-bearing: an
// attach token minted for one path must never be redeemable on another, and a
// control request offered to the wrong endpoint must be refused on its content
// type alone. Sharing the transport machinery across paths therefore means
// PARAMETERISING these identifiers — never pooling them.
type HTTPAttachProtocol struct {
	Name               string
	CapabilitiesHeader string
	ProtocolHeader     string
	AttachKeyHeader    string
	SessionHeader      string
	StreamHeader       string
	TransportHeader    string
	ControlContentType string
	StreamContentType  string
}

// IngressHTTP is the ingress access path's wire (ADR-060/061): a laptop
// attaching to the agent that serves its declared public URL.
var IngressHTTP = HTTPAttachProtocol{
	Name:               IngressHTTPProtocol,
	CapabilitiesHeader: IngressCapabilitiesHeader,
	ProtocolHeader:     IngressProtocolHeader,
	AttachKeyHeader:    IngressAttachKeyHeader,
	SessionHeader:      IngressSessionHeader,
	StreamHeader:       IngressStreamHeader,
	TransportHeader:    IngressTransportHeader,
	ControlContentType: IngressControlContentType,
	StreamContentType:  IngressStreamContentType,
}

// HTTPControlFrame is one newline-delimited control message on an HTTP v2
// ingress session. Data never uses this framing; every data flow has its own
// HTTP stream.
type HTTPControlFrame struct {
	Type   string `json:"t"`
	ID     uint32 `json:"id,omitempty"`
	Code   string `json:"code,omitempty"`
	Msg    string `json:"msg,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// LineControl is a concurrent-safe writer and single-reader for the HTTP v2
// control stream. Flush is required because the request stays open for the
// whole tunnel session.
type LineControl struct {
	reader *bufio.Reader
	writer io.Writer
	flush  func() error
	close  func() error

	writeMu   sync.Mutex
	closeOnce sync.Once
	closeErr  error
}

const maxControlFrame = 16 * 1024

// NewIngressAttachKey returns the ephemeral 256-bit key that binds HTTP data
// requests to the one control request that consumed the mint token.
func NewIngressAttachKey() (string, error) {
	raw := make([]byte, sha256.Size)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", fmt.Errorf("generate ingress attach key: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// NewLineControl wraps the two halves of a full-duplex HTTP request. receive
// must be called by one goroutine; Send is safe from many goroutines.
func NewLineControl(reader io.Reader, writer io.Writer, flush func() error, closeFn func() error) *LineControl {
	if flush == nil {
		flush = func() error { return nil }
	}
	if closeFn == nil {
		closeFn = func() error { return nil }
	}
	return &LineControl{
		reader: bufio.NewReaderSize(reader, maxControlFrame+1),
		writer: writer,
		flush:  flush,
		close:  closeFn,
	}
}

// Send writes and flushes exactly one control frame.
func (c *LineControl) Send(ctx context.Context, frame HTTPControlFrame) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("encode ingress control: %w", err)
	}
	data = append(data, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := writeAll(c.writer, data); err != nil {
		return fmt.Errorf("write ingress control: %w", err)
	}
	if err := c.flush(); err != nil {
		return fmt.Errorf("flush ingress control: %w", err)
	}
	return nil
}

// Receive reads one bounded newline-delimited frame.
func (c *LineControl) Receive() (HTTPControlFrame, error) {
	line, err := c.reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) || len(line) > maxControlFrame {
		return HTTPControlFrame{}, errors.New("ingress control frame exceeds 16 KiB")
	}
	if err != nil {
		return HTTPControlFrame{}, err
	}
	var frame HTTPControlFrame
	if err := json.Unmarshal(line, &frame); err != nil {
		return HTTPControlFrame{}, fmt.Errorf("decode ingress control: %w", err)
	}
	if frame.Type == "" {
		return HTTPControlFrame{}, errors.New("ingress control frame has no type")
	}
	return frame, nil
}

// Close tears down both halves once.
func (c *LineControl) Close() error {
	c.closeOnce.Do(func() { c.closeErr = c.close() })
	return c.closeErr
}
