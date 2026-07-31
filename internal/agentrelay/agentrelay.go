// Package agentrelay reaches a server's agent channel from a process that
// does not terminate it — worker, scheduler (ADR-052 §8). It dials the api's
// /agent/v1/relay endpoint authenticated with the TARGET SERVER's own agent
// token (which any control-plane process can read from the store, exactly
// like agent provisioning does) and speaks the same typed frames; the api
// bridges them onto the live channel.
package agentrelay

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime"
	"github.com/deepteams/akerdock/internal/hostops"
)

// Source implements dockerruntime.Source over relay connections, one per
// target server, dialed on demand and cached while healthy. Both resolvers
// are funcs so this package depends on neither the store nor the config
// wiring.
type Source struct {
	// BaseURL resolves the api base URL ("http(s)://host[:port]") a relay
	// connection dials.
	BaseURL func(ctx context.Context) (string, error)
	// Token resolves the target server's agent token plaintext.
	Token  func(ctx context.Context, serverID int64) (string, error)
	Logger *slog.Logger

	mu    sync.Mutex
	conns map[int64]*relayConn
}

var _ dockerruntime.Source = (*Source)(nil)

// Runtime returns the Docker runtime executing on the given server, bridged
// through the api.
func (s *Source) Runtime(ctx context.Context, serverID int64) (dockerruntime.Runtime, error) {
	c, err := s.conn(ctx, serverID)
	if err != nil {
		return nil, err
	}
	return dockerruntime.NewAgentRuntime(c), nil
}

// HostOps returns the ADR-054 file primitives executing on the given server,
// bridged through the api.
func (s *Source) HostOps(ctx context.Context, serverID int64) (hostops.Ops, error) {
	c, err := s.conn(ctx, serverID)
	if err != nil {
		return nil, err
	}
	return hostops.NewClient(c), nil
}

var _ hostops.Source = (*Source)(nil)

// Close tears down every cached relay connection (process shutdown).
func (s *Source) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, c := range s.conns {
		c.close()
		delete(s.conns, id)
	}
}

func (s *Source) conn(ctx context.Context, serverID int64) (*relayConn, error) {
	s.mu.Lock()
	if c, ok := s.conns[serverID]; ok {
		select {
		case <-c.Done(): // died since; dial fresh below
			delete(s.conns, serverID)
		default:
			s.mu.Unlock()
			return c, nil
		}
	}
	s.mu.Unlock()

	c, err := s.dial(ctx, serverID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.conns[serverID]; ok { // lost a dial race; keep one
		select {
		case <-existing.Done():
			existing.close()
		default:
			c.close()
			return existing, nil
		}
	}
	if s.conns == nil {
		s.conns = map[int64]*relayConn{}
	}
	s.conns[serverID] = c
	return c, nil
}

func (s *Source) dial(ctx context.Context, serverID int64) (*relayConn, error) {
	base, err := s.BaseURL(ctx)
	if err != nil {
		return nil, agentwire.Unavailable("relay url unresolved")
	}
	token, err := s.Token(ctx, serverID)
	if err != nil {
		return nil, agentwire.Unavailable("agent token unavailable")
	}
	url := strings.Replace(base, "http", "ws", 1) + "/agent/v1/relay"
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(dialCtx, url, &websocket.DialOptions{
		Subprotocols: []string{agentwire.SubprotocolRelay},
		HTTPHeader:   http.Header{"Authorization": {"Bearer " + token}},
	})
	if err != nil {
		return nil, agentwire.Unavailable("relay dial failed")
	}
	ws.SetReadLimit(1 << 20)
	// The connection outlives the dialing request: it serves every later
	// call to this server until it breaks, when the next Runtime() redials.
	connCtx, connCancel := context.WithCancel(context.Background())
	c := &relayConn{Conn: agentwire.NewConn(connCtx, ws), ws: ws, cancel: connCancel}
	go c.readLoop(connCtx)
	if s.Logger != nil {
		s.Logger.Info("agent relay dialed", "server_id", serverID, "url", url)
	}
	return c, nil
}

// relayConn is one live bridge to a server's channel; agentwire.Conn carries
// the command routing, this adds the client-side read loop and teardown.
type relayConn struct {
	*agentwire.Conn
	ws     *websocket.Conn
	cancel context.CancelFunc
}

// Attach narrows agentwire's concrete stream to the CommandSender interface.
func (c *relayConn) Attach(ctx context.Context, method string, params any) (dockerruntime.AttachStream, error) {
	return c.Conn.Attach(ctx, method, params)
}

func (c *relayConn) close() {
	c.cancel()
	_ = c.ws.Close(websocket.StatusNormalClosure, "")
}

func (c *relayConn) readLoop(ctx context.Context) {
	defer c.cancel() // a broken read ends the connection's lifetime
	for {
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			return
		}
		var f agentwire.Frame
		if json.Unmarshal(data, &f) != nil {
			continue
		}
		switch f.Type {
		case agentwire.FrameResult:
			c.DeliverResult(f.Res)
		case agentwire.FrameStream:
			c.DeliverChunk(f.Chunk)
		}
	}
}
