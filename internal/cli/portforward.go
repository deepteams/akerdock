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
		Use:   "port-forward [REF] [[LOCAL:]REMOTE]",
		Short: "Tunnel a local port to a container port, or to a declared external endpoint",
		Example: "  akerdock port-forward db/pg 15432:5432\n" +
			"  akerdock port-forward app/varuna 15432:5432 -c postgres\n" +
			"  akerdock port-forward app/varuna 15432:5432 -c postgres --pr 8   # a PR preview\n" +
			"  akerdock port-forward endpoint/prod-replica                      # a declared external endpoint\n" +
			"  akerdock port-forward endpoint/prod-replica 15432                # …on a chosen local port",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(flags.context)
			if err != nil {
				return err
			}
			refArg, portsArg := splitForwardArgs(args)
			var refArgs []string
			if refArg != "" {
				refArgs = []string{refArg}
			}
			r, err := refFromArgs(refArgs)
			if err != nil {
				return err
			}
			// An external endpoint declared its own host and port (ADR-045), so
			// there is no remote port to name: the ports argument becomes
			// optional, and without it the OS picks the local port.
			var localPort, remotePort int
			switch {
			case portsArg != "":
				if localPort, remotePort, err = parsePorts(portsArg); err != nil {
					return err
				}
			case r.kind == "endpoints":
				localPort, remotePort = 0, 0
			default:
				return fmt.Errorf("no port given — pass the container port to forward (e.g. %s 15432:5432)", refArg)
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

// splitForwardArgs tells the optional REF from the optional ports, because both
// are positional and either may be omitted:
//
//	port-forward db/pg 15432:5432    → ref + ports
//	port-forward 15432:5432          → ports only (default app from .akerdock)
//	port-forward endpoint/replica    → ref only (an endpoint names no port)
//
// A REF always contains a slash and a ports argument never does, so one
// argument is never ambiguous — reading it by POSITION alone is what made
// `port-forward endpoint/x` complain about a missing default application.
func splitForwardArgs(args []string) (refArg, portsArg string) {
	if len(args) == 2 {
		return args[0], args[1]
	}
	if len(args) == 1 && strings.Contains(args[0], "/") {
		return args[0], ""
	}
	if len(args) == 1 {
		return "", args[0]
	}
	return "", ""
}

// handshakeReason extracts the API error message from a refused WebSocket
// upgrade. The tunnel redeem answers a normal JSON error before switching
// protocols (a revoked grant, an unreachable server, a session cap), and that
// sentence is the only actionable part of the failure.
func handshakeReason(resp *http.Response) string {
	if resp == nil || resp.Body == nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&body); err != nil {
		return ""
	}
	return body.Message
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
	mint := func() error { return c.do(ctx, http.MethodPost, mintPath, q, body, &sess) }
	if err := mint(); err != nil {
		var apiErr *apiError
		if !errors.As(err, &apiErr) || apiErr.Code != "access_request_required" {
			return err
		}
		// No live grant: send the developer to the page that issues one, then
		// poll the mint until it goes through. Same choreography as
		// `akerdock login` (ADR-031) — the point is that they never have to go
		// looking, nor to run the command a second time.
		if err := waitForAccessGrant(ctx, apiErr, mint); err != nil {
			return err
		}
	}

	// The ladder's memory outlives the re-mint below, so a rung already known to
	// refuse this path is not tried a second time on the fresh token.
	state := newTransportState()

	// The mint token is single-use and the server claims it at the very top of
	// the attach handler, BEFORE it inspects the container and dials SSH. An
	// attach that reaches the server and then gives up waiting therefore leaves
	// a burnt token behind, and every later attempt — any rung, WebSocket
	// included — answers 401. One fresh session and one retry is the whole
	// remedy, and it is capped at one on purpose: each mint writes an audit row
	// and takes one of the team's ten concurrent slots (ADR-032), so a target
	// that is genuinely broken must not be able to spend them in a loop.
	//
	// TRANSITIONAL (ADR-065): a retry is what a single-use token forces on a
	// client that cannot tell "the server never saw this" from "the server saw
	// it and is still working". Once a token re-presented by the same attach key
	// within its TTL is accepted, the attempt stops burning anything and this
	// loop has nothing left to remedy.
	for attempt := 0; ; attempt++ {
		err := c.forwardSession(ctx, state, sess.WebsocketPath, sess.Token, sess.AuthorizedUntil, localPort, remotePort)
		var spent *tokenSpentError
		if !errors.As(err, &spent) || attempt > 0 {
			return err
		}
		if err := mint(); err != nil {
			return err
		}
	}
}

// forwardSession runs one attempt on one minted token: ADR-064's ladder first —
// HTTP/3, then HTTP/2, each carrying one forwarded connection per independent
// stream — then the WebSocket underneath, reached whenever no rung could be
// negotiated or the server predates the ladder.
func (c *Client) forwardSession(
	ctx context.Context,
	state *transportState,
	attachPath, token string,
	authorizedUntil *time.Time,
	localPort, remotePort int,
) error {
	switch err := c.forwardOverHTTP(ctx, state, attachPath, token, authorizedUntil, localPort, remotePort); {
	case err == nil:
		return nil
	case !errors.Is(err, errNoHTTPTransport):
		return err
	}

	wsURL := toWS(c.base) + attachPath + "?" + url.Values{"token": {token}}.Encode()
	conn, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{Subprotocols: []string{"akerdock-tunnel-v1"}})
	if err != nil {
		// The bottom rung burns the token exactly like the others, so a 401 here
		// is classified rather than reported: it means an earlier attempt got to
		// the server first, and only a fresh mint can follow it.
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return spentToken(&attachRejection{
				kind: transportWS, status: resp.Status, code: resp.StatusCode,
				message: handshakeReason(resp),
			})
		}
		// The refusal carries a reason the operator can act on — "the server is
		// not reachable over SSH right now" beats "expected handshake response
		// status code 101 but got 409", which describes only our disappointment.
		if reason := handshakeReason(resp); reason != "" {
			return fmt.Errorf("cannot open tunnel: %s", reason)
		}
		return fmt.Errorf("cannot open tunnel: %w", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	conn.SetReadLimit(-1)

	tun := &tunnel{conn: conn, streams: map[uint32]net.Conn{}}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go tun.readLoop(ctx, cancel)

	ln, err := listenAndAnnounce(ctx, localPort, remotePort, authorizedUntil)
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close() }()
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

// listenAndAnnounce opens the local listener both transports serve from, and
// tells the developer where it is. Port 0 lets the OS pick a free one — the
// case of an endpoint forward with no ports argument — and the chosen port is
// read back, because a port nobody told you about is a tunnel you cannot use.
//
// The deadline is announced at open, not only when it ends: the developer plans
// a long transfer around this instant, and a deadline that arrives unannounced
// reads as a bug in the platform (ADR-045 §5).
func listenAndAnnounce(ctx context.Context, localPort, remotePort int, authorizedUntil *time.Time) (net.Listener, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
	if err != nil {
		return nil, fmt.Errorf("cannot listen on 127.0.0.1:%d: %w", localPort, err)
	}
	if addr, ok := ln.Addr().(*net.TCPAddr); ok {
		localPort = addr.Port
	}
	target := fmt.Sprintf("remote :%d", remotePort)
	if remotePort == 0 {
		target = "the endpoint's declared target" // frozen server-side (ADR-045 §2)
	}
	fmt.Fprintf(os.Stderr, "forwarding 127.0.0.1:%d -> %s%s (Ctrl-C to stop)\n",
		localPort, target, authorizedSuffix(authorizedUntil))
	if authorizedUntil != nil {
		go warnBeforeExpiry(ctx, *authorizedUntil)
	}
	go func() { <-ctx.Done(); _ = ln.Close() }()
	return ln, nil
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

// grantWaitTimeout bounds that wait. Long enough to read the page, type a
// reason and reach for a second factor without being rushed; short enough that
// a command left running in a forgotten terminal eventually says why nothing
// happened.
const grantWaitTimeout = 10 * time.Minute

// waitForAccessGrant opens the dashboard page that issues an access grant and
// blocks until the developer has done it. It never decides anything itself:
// the grant is created by a browser session behind a fresh second factor, and
// this only spares the developer from hunting for the URL.
func waitForAccessGrant(ctx context.Context, apiErr *apiError, mint func() error) error {
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
	deadline := time.After(grantWaitTimeout)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("no access grant after %s — request one from %s, then run this again",
				grantWaitTimeout, apiErr.RequestURL)
		case <-ticker.C:
			// The mint itself is the poll: replaying it is exactly the check
			// we need, and it avoids a second endpoint that could disagree
			// with the first about what "has access" means.
			//
			// It keeps polling on `access_request_required` and ONLY on that:
			// filling in a reason, picking a duration and passing a second
			// factor takes a minute or two, and a single retry — what this
			// used to do — was over before the form had loaded. Any other
			// error is final, so a revoked token or an unreachable server
			// stops here rather than spinning.
			err := mint()
			if err == nil {
				return nil
			}
			var again *apiError
			if !errors.As(err, &again) || again.Code != "access_request_required" {
				return err
			}
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
	case "target_stopped":
		return "tunnel closed: the target container stopped (it may have been put to sleep by scale-to-zero) — " +
			"rerun the command to reopen it"
	case "user_close", "":
		return ""
	default:
		return "tunnel closed (" + reason + ")"
	}
}
