package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"

	tun "github.com/deepteams/akerdock/internal/tunnel"
)

// sleepCtx waits for d or ctx cancellation; reports false if cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ingressSubprotocol is the attach subprotocol (ADR-060 §3), distinct from the
// egress tunnel's so a token can never be redeemed on the wrong path.
const (
	ingressSubprotocol = "akerdock-ingress-v1"
	ingressMaxBackoff  = 30 * time.Second
)

func ingressCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ingress ENDPOINT LOCAL_PORT",
		Short: "Relay a declared public URL to a local port on your machine",
		Long: "Relay an ingress endpoint's stable public URL to a port on your machine " +
			"(ADR-060). Visitors reach the URL; their requests are relayed to " +
			"127.0.0.1:LOCAL_PORT. The tunnel reconnects by itself if the network " +
			"drops, and exits when access ends.",
		Example: "  akerdock ingress dev-kedric 3000        # serve localhost:3000 at the endpoint's URL",
		Args:    usageArgs(2, "ingress ENDPOINT LOCAL_PORT", "ingress dev-kedric 3000"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(flags.context)
			if err != nil {
				return err
			}
			endpoint := args[0]
			localPort, err := parseLocalPort(args[1])
			if err != nil {
				return err
			}
			return c.runIngress(cmd.Context(), endpoint, localPort)
		},
	}
	return cmd
}

func parseLocalPort(s string) (int, error) {
	var port int
	if _, err := fmt.Sscanf(s, "%d", &port); err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("invalid local port %q — pass a number in 1–65535", s)
	}
	return port, nil
}

// ingressMint is the subset of the mint response the CLI needs.
type ingressMint struct {
	Uuid           string    `json:"uuid"`
	Url            string    `json:"url"`
	AttachUrl      string    `json:"attach_url"`
	Token          string    `json:"token"`
	TokenExpiresAt time.Time `json:"token_expires_at"`
}

// runIngress mints, attaches, and relays visitor connections to the local
// port — reconnecting on a transport drop, exiting on a policy close.
func (c *Client) runIngress(ctx context.Context, endpoint string, localPort int) error {
	res, err := c.resolve(ctx, ref{kind: "ingress-endpoints", name: endpoint})
	if err != nil {
		return err
	}
	mintPath := "/ingress-endpoints/" + res.Uuid + "/tunnels"

	// Backoff between transport reconnects; reset after a session that ran for
	// a while, so a stable tunnel that blips once does not inherit a long delay.
	backoff := time.Second
	announced := false
	transportState := newIngressTransportState()

	for {
		var sess ingressMint
		if err := c.do(ctx, http.MethodPost, mintPath, nil, struct{}{}, &sess); err != nil {
			// A 409 (occupied, agent down) is worth one retry — the previous
			// session may still be tearing down — but not a tight loop.
			var apiErr *apiError
			if errors.As(err, &apiErr) && apiErr.Code == "occupied" {
				return fmt.Errorf("cannot attach: %s", apiErr.Message)
			}
			if ctx.Err() != nil {
				//nolint:nilerr // Ctrl-C during the mint is a clean exit, not the mint's error.
				return nil
			}
			fmt.Fprintf(os.Stderr, "cannot attach: %v — retrying in %s\n", err, backoff)
			if !sleepCtx(ctx, backoff) {
				return nil
			}
			backoff = nextBackoff(backoff)
			continue
		}

		if !announced {
			fmt.Fprintf(os.Stderr, "relaying %s -> 127.0.0.1:%d (Ctrl-C to stop)\n", sess.Url, localPort)
			announced = true
		}
		start := time.Now()
		reason, _, err := c.attachIngress(ctx, sess, localPort, transportState)
		if ctx.Err() != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			cleanupErr := c.closeIngressMint(cleanupCtx, sess.Uuid)
			cancel()
			if cleanupErr != nil {
				fmt.Fprintf(os.Stderr, "warning: cannot close ingress session: %v\n", cleanupErr)
			}
			return nil
		}
		// A policy close is terminal — the CLI never re-dials through a control
		// (ADR-060 §6). A transport drop reconnects.
		if msg := ingressCloseMessage(reason); msg != "" {
			fmt.Fprintln(os.Stderr, msg)
		}
		if isPolicyClose(reason) {
			return nil
		}
		// One session is one mint and one WebSocket. Finalize the failed mint
		// before asking for another one, otherwise its durable occupancy row
		// makes the reconnect reject itself as "already in use".
		if cleanupErr := c.closeIngressMint(ctx, sess.Uuid); cleanupErr != nil {
			fmt.Fprintf(os.Stderr, "warning: cannot close dropped ingress session: %v\n", cleanupErr)
		}
		if err != nil && reason == "" {
			fmt.Fprintf(os.Stderr, "tunnel dropped: %v — reconnecting\n", err)
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second // a healthy session resets the backoff
		}
		if !sleepCtx(ctx, backoff) {
			return nil
		}
		backoff = nextBackoff(backoff)
	}
}

// attachIngress dials the attach URL and relays until the socket closes,
// returning the server's end reason (empty on a bare transport error). The
// laptop is the Bridge side: it receives "open" per visitor connection and
// dials 127.0.0.1:port (internal/tunnel, roles mirrored from the agent).
func (c *Client) attachIngress(
	ctx context.Context,
	sess ingressMint,
	localPort int,
	state *ingressTransportState,
) (string, ingressTransportKind, error) {
	httpAttach, httpErr := ingressHTTPURL(sess)
	if httpErr == nil {
		preference := ingressTransportPreference()
		for _, kind := range preference[:2] {
			if state.disabled[kind] {
				continue
			}
			var pool *ingressHTTPLanePool
			if kind == ingressTransportH3 {
				pool = newIngressH3Pool()
			} else {
				pool = newIngressH2Pool()
			}
			if err := probeIngressHTTP(ctx, pool, httpAttach, kind); err != nil {
				_ = pool.Close()
				continue
			}
			key, err := tun.NewIngressAttachKey()
			if err != nil {
				_ = pool.Close()
				return "", kind, err
			}
			control, err := openIngressHTTPControl(ctx, pool, sess, key, kind)
			if err != nil {
				state.disabled[kind] = true
				_ = pool.Close()
				return "", kind, err
			}
			if state.announced != kind {
				fmt.Fprintf(os.Stderr, "tunnel transport: %s\n", kind.label())
				state.announced = kind
			}
			started := time.Now()
			reason, runErr := runIngressHTTPBridge(ctx, control.control, pool, httpAttach, sess.Uuid, key, localPort)
			if runErr != nil && time.Since(started) < ingressTransportAttachTimeout {
				state.disabled[kind] = true
			}
			_ = control.control.Close()
			control.cancel()
			_ = pool.Close()
			return reason, kind, runErr
		}
	}
	reason, err := c.attachIngressWebSocket(ctx, sess, localPort)
	if state.announced != ingressTransportWS && err == nil {
		fmt.Fprintf(os.Stderr, "tunnel transport: %s\n", ingressTransportWS.label())
		state.announced = ingressTransportWS
	}
	return reason, ingressTransportWS, err
}

func (c *Client) attachIngressWebSocket(ctx context.Context, sess ingressMint, localPort int) (string, error) {
	attachURL, err := ingressAttachURL(sess)
	if err != nil {
		return "", err
	}
	key, err := tun.NewIngressAttachKey()
	if err != nil {
		return "", err
	}
	primaryHeaders := make(http.Header)
	primaryHeaders.Set(tun.IngressAttachKeyHeader, key)
	conn, resp, err := websocket.Dial(ctx, attachURL, &websocket.DialOptions{
		Subprotocols: []string{tun.IngressWebSocketV2, ingressSubprotocol},
		HTTPHeader:   primaryHeaders,
	})
	if err != nil {
		if reason := handshakeReason(resp); reason != "" {
			return "", fmt.Errorf("%s", reason)
		}
		return "", err
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	conn.SetReadLimit(-1)
	closeReason := &ingressCloseReason{}
	primary := ingressClientConn{c: conn, reason: closeReason}
	var bridgeConn tun.Conn = primary
	if conn.Subprotocol() == tun.IngressWebSocketV2 {
		lanes := tun.NewMultiLaneConn(primary, func() error {
			return conn.Close(websocket.StatusNormalClosure, "")
		}, 4)
		defer func() { _ = lanes.Close() }()
		bridgeConn = lanes

		laneURL, parseErr := url.Parse(attachURL)
		if parseErr != nil {
			return "", parseErr
		}
		query := laneURL.Query()
		query.Del("token")
		laneURL.RawQuery = query.Encode()
		var joins sync.WaitGroup
		for lane := 1; lane < 4; lane++ {
			joins.Add(1)
			go func() {
				defer joins.Done()
				laneConn, joinErr := dialIngressWebSocketLane(ctx, laneURL.String(), sess.Uuid, key, lane, closeReason)
				if joinErr != nil {
					return
				}
				laneConn.c.SetReadLimit(-1)
				if addErr := lanes.AddLane(lane, laneConn, func() error {
					return laneConn.c.Close(websocket.StatusNormalClosure, "")
				}); addErr != nil {
					_ = laneConn.c.Close(websocket.StatusPolicyViolation, addErr.Error())
				}
			}()
		}
		joins.Wait()
	}

	dial := func(dialCtx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(dialCtx, "tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
	}
	reason := tun.Bridge(ctx, bridgeConn, dial, tun.Options{
		// The agent enforces the real idle/ceiling bounds; the client's own
		// timers only need not fire first.
		IdleTimeout: 24 * time.Hour,
		MaxDuration: 24 * time.Hour,
	})
	_ = reason
	return closeReason.get(), nil
}

func dialIngressWebSocketLane(
	ctx context.Context,
	attachURL, sessionUUID, key string,
	lane int,
	reason *ingressCloseReason,
) (ingressClientConn, error) {
	headers := make(http.Header)
	headers.Set(tun.IngressSessionHeader, sessionUUID)
	headers.Set(tun.IngressAttachKeyHeader, key)
	headers.Set(tun.IngressLaneHeader, strconv.Itoa(lane))
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		laneCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		conn, resp, err := websocket.Dial(laneCtx, attachURL, &websocket.DialOptions{
			Subprotocols: []string{tun.IngressWebSocketV2},
			HTTPHeader:   headers,
		})
		cancel()
		if err == nil {
			return ingressClientConn{c: conn, reason: reason}, nil
		}
		lastErr = err
		if resp == nil || resp.StatusCode != http.StatusUnauthorized || !sleepCtx(ctx, 25*time.Millisecond) {
			break
		}
	}
	return ingressClientConn{}, lastErr
}

// ingressAttachURL keeps the CLI compatible with mint responses produced
// before attach_url carried its token as required by the API contract. The
// separately returned token remains the authority and replaces any stale
// query value without exposing it in logs.
func ingressAttachURL(sess ingressMint) (string, error) {
	attachURL, err := url.Parse(sess.AttachUrl)
	if err != nil {
		return "", fmt.Errorf("invalid ingress attach URL: %w", err)
	}
	query := attachURL.Query()
	if sess.Token != "" {
		query.Set("token", sess.Token)
	}
	if query.Get("token") == "" {
		return "", fmt.Errorf("ingress mint response has no attach token")
	}
	attachURL.RawQuery = query.Encode()
	return attachURL.String(), nil
}

// closeIngressMint releases the durable occupancy row before a reconnect.
// The endpoint is idempotent, so an agent observation that won the race is
// harmless.
func (c *Client) closeIngressMint(ctx context.Context, uuid string) error {
	if uuid == "" {
		return nil
	}
	return c.do(ctx, http.MethodDelete, "/ingress-tunnel-sessions/"+url.PathEscape(uuid), nil, nil, nil)
}

// nextBackoff doubles up to a cap.
func nextBackoff(cur time.Duration) time.Duration {
	next := cur * 2
	if next > ingressMaxBackoff {
		return ingressMaxBackoff
	}
	return next
}

// isPolicyClose reports whether the reason is a deliberate teardown the CLI
// must not reconnect through: idle timeout, ceiling, revocation.
func isPolicyClose(reason string) bool {
	switch reason {
	case "idle_timeout", "max_duration", "revoked", "user_close":
		return true
	default:
		return false
	}
}

// ingressCloseMessage phrases the end reason as an instruction. A tunnel that
// dies without a word reads as a platform bug (ADR-045 §5's discipline).
func ingressCloseMessage(reason string) string {
	switch reason {
	case "idle_timeout":
		return "tunnel closed after 30 minutes with no traffic — run the command again to reopen it"
	case "max_duration":
		return "tunnel closed: reached the 12-hour session limit — run the command again to reopen it"
	case "revoked":
		return "tunnel closed: an administrator closed it or removed the endpoint"
	case "user_close", "":
		return ""
	default:
		return "tunnel closed (" + reason + ")"
	}
}

// ingressClientConn adapts coder/websocket to tunnel.Conn and captures the
// close-frame reason on the way out, so the caller can decide reconnect vs
// exit on it.
type ingressClientConn struct {
	c      *websocket.Conn
	reason *ingressCloseReason
}

type ingressCloseReason struct {
	mu    sync.Mutex
	value string
}

func (r *ingressCloseReason) set(value string) {
	r.mu.Lock()
	if value != "" || r.value == "" {
		r.value = value
	}
	r.mu.Unlock()
}

func (r *ingressCloseReason) get() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.value
}

func (t ingressClientConn) Read(ctx context.Context) (tun.MessageType, []byte, error) {
	typ, data, err := t.c.Read(ctx)
	if err != nil {
		var ce websocket.CloseError
		if errors.As(err, &ce) {
			t.reason.set(ce.Reason)
			switch ce.Code {
			case websocket.StatusNormalClosure, websocket.StatusGoingAway:
				return 0, nil, tun.ErrClientClosed
			}
		}
		return 0, nil, err
	}
	if typ == websocket.MessageText {
		return tun.MessageText, data, nil
	}
	return tun.MessageBinary, data, nil
}

func (t ingressClientConn) Write(ctx context.Context, typ tun.MessageType, data []byte) error {
	kind := websocket.MessageBinary
	if typ == tun.MessageText {
		kind = websocket.MessageText
	}
	return t.c.Write(ctx, kind, data)
}

func (t ingressClientConn) Ping(ctx context.Context) error { return t.c.Ping(ctx) }
