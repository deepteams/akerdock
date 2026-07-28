package cli

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

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
			"  akerdock port-forward app/varuna 15432:5432 -c postgres --pr 8   # a PR preview\n" +
			"  akerdock port-forward endpoint/prod-replica 15432                # a declared external endpoint",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(flags.context)
			if err != nil {
				return err
			}
			// The ports argument is always last; a leading REF is optional when a
			// default application is configured (.akerdock, spec §4).
			var refArgs []string
			if len(args) == 2 {
				refArgs = args[:1]
			}
			r, err := refFromArgs(refArgs)
			if err != nil {
				return err
			}
			localPort, remotePort, err := parsePorts(args[len(args)-1])
			if err != nil {
				return err
			}
			component = defaultComponent(component)
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
			// An external endpoint (ADR-045) froze its own host and port at
			// declaration: the mint takes no body, and the remote port in the
			// argument is only there to name the local one.
			if r.kind == "endpoints" {
				return c.runPortForward(cmd.Context(),
					"/external-endpoints/"+res.Uuid+"/port-forwards", "", localPort, remotePort)
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
		WebsocketPath   string     `json:"websocket_path"`
		Token           string     `json:"token"`
		AuthorizedUntil *time.Time `json:"authorized_until"`
	}
	// An external endpoint froze its host and port at declaration, so its mint
	// takes no body at all (ADR-045 §2) — stricter than the ADR-032 mints,
	// which still name a port.
	var body any
	if !strings.HasPrefix(mintPath, "/external-endpoints/") {
		body = map[string]int{"port": remotePort}
	}
	if err := c.do(ctx, http.MethodPost, mintPath, q, body, &sess); err != nil {
		var apiErr *apiError
		if !errors.As(err, &apiErr) || apiErr.Code != "access_request_required" {
			return err
		}
		// No live grant: send the developer to the page that issues one, wait
		// for it, and replay the mint. Same choreography as `akerdock login`
		// (ADR-031) — the point is that they never have to go looking.
		if err := waitForAccessGrant(ctx, apiErr); err != nil {
			return err
		}
		if err := c.do(ctx, http.MethodPost, mintPath, q, body, &sess); err != nil {
			return err
		}
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
	// Announced at open, not only when it ends: the developer plans a long
	// transfer around this instant, and a deadline that arrives unannounced
	// reads as a bug in the platform (ADR-045 §5).
	fmt.Fprintf(os.Stderr, "forwarding 127.0.0.1:%d -> remote :%d%s (Ctrl-C to stop)\n",
		localPort, remotePort, authorizedSuffix(sess.AuthorizedUntil))
	if sess.AuthorizedUntil != nil {
		go warnBeforeExpiry(ctx, *sess.AuthorizedUntil)
	}

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

// readLoop dispatches server frames to their streams. On close it surfaces the
// server's end reason: the bridge already sends it in the close frame, and
// dropping it here is what made tunnels appear to die for no reason.
func (t *tunnel) readLoop(ctx context.Context, cancel context.CancelFunc) {
	defer cancel()
	for {
		typ, data, err := t.conn.Read(ctx)
		if err != nil {
			// The reason travels in the close frame's Reason field, not in the
			// status code: the bridge closes with StatusNormalClosure and the
			// end reason as text.
			var ce websocket.CloseError
			if errors.As(err, &ce) {
				if msg := closeMessage(ce.Reason); msg != "" {
					fmt.Fprintln(os.Stderr, msg)
				}
			}
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

// grantPollInterval is how often the CLI re-checks whether the access grant
// has been issued. The human is filling a form and touching a security key —
// polling faster would only spend requests on their typing speed.
const grantPollInterval = 2 * time.Second

// waitForAccessGrant opens the dashboard page that issues an access grant and
// blocks until the developer has done it. It never decides anything itself:
// the grant is created by a browser session behind a fresh second factor, and
// this only spares the developer from hunting for the URL.
func waitForAccessGrant(ctx context.Context, apiErr *apiError) error {
	if apiErr.RequestURL == "" {
		// The instance has no FQDN configured, so there is no page to point
		// at. Say what is missing rather than spin forever.
		return fmt.Errorf("%s — request access from the dashboard, then run this again", apiErr.Message)
	}
	fmt.Fprintf(os.Stderr, "this endpoint needs an access grant\nopening %s\n", apiErr.RequestURL)
	_ = openBrowser(apiErr.RequestURL)
	fmt.Fprintf(os.Stderr, "waiting for the grant (Ctrl-C to give up)...\n")

	ticker := time.NewTicker(grantPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			// The mint itself is the poll: replaying it is exactly the check
			// we need, and it avoids a second endpoint that could disagree
			// with the first about what "has access" means.
			return nil
		}
	}
}

// authorizedSuffix renders the deadline the way someone plans around it:
// absolute so a long transfer can be scheduled, relative so it registers at a
// glance. Neither alone is enough — the absolute time needs mental arithmetic,
// the relative one is forgotten a minute later.
func authorizedSuffix(until *time.Time) string {
	if until == nil {
		return ""
	}
	left := time.Until(*until).Round(time.Minute)
	if left <= 0 {
		return " — authorization expired"
	}
	return fmt.Sprintf(" — authorized until %s (%s)", until.Local().Format("15:04"), humanDuration(left))
}

func humanDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02d", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

// warnBeforeExpiry gives notice before the deadline lands. This is the moment
// the developer discovers their transfer will not fit, so it is also the
// moment to tell them renewal exists.
func warnBeforeExpiry(ctx context.Context, until time.Time) {
	for _, lead := range []time.Duration{15 * time.Minute, 2 * time.Minute} {
		wait := time.Until(until.Add(-lead))
		if wait <= 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
			fmt.Fprintf(os.Stderr,
				"\nheads up: this tunnel's authorization ends in %s (at %s) — request access again to extend it\n",
				humanDuration(lead), until.Local().Format("15:04"))
		}
	}
}

// closeMessage turns the server's end reason into something actionable. A
// tunnel that dies in silence reads as a bug in AkerDock, and the developer's
// next move is to look for a way around the platform rather than back into it.
func closeMessage(reason string) string {
	switch reason {
	case "idle_timeout":
		return "tunnel closed after 30 minutes with no traffic — rerun the command to reopen it"
	case "grant_expired":
		return "tunnel closed: your access grant expired — request access again to reopen it"
	case "revoked":
		return "tunnel closed: an administrator revoked the access grant"
	case "max_duration":
		return "tunnel closed: maximum session duration reached — rerun the command to reopen it"
	case "disconnect":
		return "tunnel closed: the connection to the manager dropped"
	case "user_close", "":
		return ""
	default:
		return "tunnel closed (" + reason + ")"
	}
}
