package cli

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"
)

// mint path per resource kind (ADR-032).
var forwardPath = map[string]string{
	"apps": "/applications", "databases": "/databases", "services": "/services",
}

func portForwardCmd() *cobra.Command {
	var component string
	var pr int
	cmd := &cobra.Command{
		Use:   "port-forward REF [LOCAL:]REMOTE",
		Short: "Tunnel a local port to a container port through the manager",
		Example: "  akerdock port-forward db/pg 15432:5432\n" +
			"  akerdock port-forward app/varuna 15432:5432 -c postgres\n" +
			"  akerdock port-forward app/varuna 15432:5432 -c postgres --pr 8   # a PR preview",
		Args: usageArgs(2, "port-forward <type/name> [LOCAL:]REMOTE", "port-forward db/pg 15432:5432"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient(flags.context)
			if err != nil {
				return err
			}
			r, err := parseRef(args[0])
			if err != nil {
				return err
			}
			localPort, remotePort, err := parsePorts(args[1])
			if err != nil {
				return err
			}
			res, err := c.resolve(cmd.Context(), r)
			if err != nil {
				return err
			}
			// A PR preview: the tunnel targets the preview instance's container.
			if pr > 0 {
				if r.kind != "apps" {
					return fmt.Errorf("--pr only applies to an app/… reference")
				}
				preview, err := c.resolvePreview(cmd.Context(), res.Uuid, pr)
				if err != nil {
					return err
				}
				mint := "/applications/" + res.Uuid + "/previews/" + preview.Uuid + "/port-forwards"
				return c.runPortForward(cmd.Context(), mint, component, localPort, remotePort)
			}
			basePath, ok := forwardPath[r.kind]
			if !ok {
				return fmt.Errorf("port-forward does not support %q", r.kind)
			}
			return c.runPortForward(cmd.Context(), basePath+"/"+res.Uuid+"/port-forwards", component, localPort, remotePort)
		},
	}
	cmd.Flags().StringVarP(&component, "component", "c", "", "compose service to target")
	cmd.Flags().IntVar(&pr, "pr", 0, "target the preview of this PR number instead of production")
	return cmd
}

// parsePorts parses "LOCAL:REMOTE" or "REMOTE" (local defaults to remote).
func parsePorts(s string) (local, remote int, err error) {
	if l, r, ok := strings.Cut(s, ":"); ok {
		local, err = strconv.Atoi(l)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid local port %q", l)
		}
		remote, err = strconv.Atoi(r)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid remote port %q", r)
		}
		return local, remote, nil
	}
	remote, err = strconv.Atoi(s)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid port %q", s)
	}
	return remote, remote, nil
}

// runPortForward mints a session, opens the tunnel WebSocket, and serves a
// local loopback listener multiplexing every accepted TCP connection over it
// (akerdock-tunnel-v1, ADR-032).
func (c *Client) runPortForward(ctx context.Context, mintPath, component string, localPort, remotePort int) error {
	q := url.Values{}
	if component != "" {
		q.Set("component", component)
	}
	var sess struct {
		WebsocketPath string `json:"websocket_path"`
		Token         string `json:"token"`
	}
	if err := c.do(ctx, http.MethodPost, mintPath, q, map[string]int{"port": remotePort}, &sess); err != nil {
		return err
	}

	wsURL := toWS(c.base) + sess.WebsocketPath + "?" + url.Values{"token": {sess.Token}}.Encode()
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{Subprotocols: []string{"akerdock-tunnel-v1"}})
	if err != nil {
		return fmt.Errorf("cannot open tunnel: %w", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	conn.SetReadLimit(-1)

	tun := &tunnel{conn: conn, streams: map[uint32]net.Conn{}}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go tun.readLoop(ctx, cancel)

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
	if err != nil {
		return fmt.Errorf("cannot listen on 127.0.0.1:%d: %w", localPort, err)
	}
	defer func() { _ = ln.Close() }()
	fmt.Fprintf(os.Stderr, "forwarding 127.0.0.1:%d -> remote :%d (Ctrl-C to stop)\n", localPort, remotePort)

	go func() { <-ctx.Done(); _ = ln.Close() }()
	var nextID uint32
	for {
		local, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return err
			}
		}
		nextID++
		tun.open(ctx, nextID, local)
	}
}

// tunnel multiplexes TCP streams over one WebSocket (akerdock-tunnel-v1).
type tunnel struct {
	conn    *websocket.Conn
	mu      sync.Mutex
	streams map[uint32]net.Conn
	writeMu sync.Mutex
}

type tunnelCtrl struct {
	T    string `json:"t"`
	ID   uint32 `json:"id"`
	Code string `json:"code,omitempty"`
	Msg  string `json:"msg,omitempty"`
}

func (t *tunnel) ctrl(ctx context.Context, m tunnelCtrl) error {
	data, _ := json.Marshal(m)
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return t.conn.Write(ctx, websocket.MessageText, data)
}

func (t *tunnel) sendData(ctx context.Context, id uint32, p []byte) error {
	frame := make([]byte, 4+len(p))
	binary.BigEndian.PutUint32(frame, id)
	copy(frame[4:], p)
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return t.conn.Write(ctx, websocket.MessageBinary, frame)
}

// open registers a new stream and asks the server to dial the target.
func (t *tunnel) open(ctx context.Context, id uint32, local net.Conn) {
	t.mu.Lock()
	t.streams[id] = local
	t.mu.Unlock()
	if err := t.ctrl(ctx, tunnelCtrl{T: "open", ID: id}); err != nil {
		_ = local.Close()
		return
	}
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := local.Read(buf)
			if n > 0 {
				if werr := t.sendData(ctx, id, buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		_ = t.ctrl(ctx, tunnelCtrl{T: "eof", ID: id})
	}()
}

// readLoop dispatches server frames to their streams.
func (t *tunnel) readLoop(ctx context.Context, cancel context.CancelFunc) {
	defer cancel()
	for {
		typ, data, err := t.conn.Read(ctx)
		if err != nil {
			return
		}
		switch typ {
		case websocket.MessageBinary:
			if len(data) < 4 {
				continue
			}
			id := binary.BigEndian.Uint32(data[:4])
			if local := t.get(id); local != nil {
				_, _ = local.Write(data[4:])
			}
		case websocket.MessageText:
			var m tunnelCtrl
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			switch m.T {
			case "open_err":
				fmt.Fprintf(os.Stderr, "stream %d: %s %s\n", m.ID, m.Code, m.Msg)
				t.closeStream(m.ID)
			case "eof", "close":
				t.closeStream(m.ID)
			}
		}
	}
}

func (t *tunnel) get(id uint32) net.Conn {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.streams[id]
}

func (t *tunnel) closeStream(id uint32) {
	t.mu.Lock()
	local := t.streams[id]
	delete(t.streams, id)
	t.mu.Unlock()
	if local != nil {
		_ = local.Close()
	}
}

var _ = io.EOF
