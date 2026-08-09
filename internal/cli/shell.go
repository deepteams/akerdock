package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/deepteams/akerdock/internal/terminal"
	tun "github.com/deepteams/akerdock/internal/tunnel"
)

func shellCmd() *cobra.Command {
	var component string
	cmd := &cobra.Command{
		Use:     "shell [REF]",
		Short:   "Open an interactive shell in a container",
		Example: "  akerdock shell app/varuna\n  akerdock shell -c postgres   # default app from .akerdock",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(flags.context)
			if err != nil {
				return err
			}
			r, err := refFromArgs(args)
			if err != nil {
				return err
			}
			if r.kind != "apps" {
				return fmt.Errorf("shell currently supports app/… references")
			}
			component = defaultComponent(component)
			res, err := c.resolve(cmd.Context(), r)
			if err != nil {
				return err
			}

			q := url.Values{}
			if component != "" {
				q.Set("component", component)
			}
			var sess struct {
				WebsocketPath string `json:"websocket_path"`
				Token         string `json:"token"`
				// ADR-067 §6: absent is `ready`, which is both "the container was
				// up" and "this manager predates the field" — one nil, one
				// meaning, no branch that has to tell them apart.
				State string `json:"state"`
			}
			if err := c.do(cmd.Context(), http.MethodPost,
				"/applications/"+res.Uuid+"/terminal-sessions", q, struct{}{}, &sess); err != nil {
				return err
			}
			return c.attachTerminal(cmd.Context(), sess.WebsocketPath, sess.Token,
				sess.State == mintStateWaking)
		},
	}
	cmd.Flags().StringVarP(&component, "component", "c", "", "compose service to open the shell in")
	return cmd
}

// attachTerminal bridges the local TTY to the remote PTY. ADR-064's ladder
// first — HTTP/3, then HTTP/2, where the resize travels on the session
// request and the keystrokes on their own stream — then the WebSocket, which
// stays the bottom rung and answers whenever a rung above fails or the server
// predates the ladder.
func (c *Client) attachTerminal(ctx context.Context, attachPath, token string, waking bool) error {
	// Before the terminal window appears, and once (ADR-067 §6): a blank window
	// held for up to 75 s reads as a hung client, and the control frame that
	// says the same thing only lands once the session is already open.
	if waking {
		fmt.Fprintf(os.Stderr, "%s\r\n", wakeColdStartNotice)
	}
	// One attach key per mint, generated before the climb rather than inside it
	// (ADR-065 §3). The key is who is attaching: minted per rung, every step down
	// would present the token as a different attacher and be refused as a replay,
	// which is the failure ADR-065 exists to remove.
	key, err := tun.NewIngressAttachKey()
	if err != nil {
		return err
	}
	switch err := c.shellOverHTTP(ctx, attachPath, token, key, waking); {
	case err == nil:
		return nil
	case !errors.Is(err, errNoHTTPTransport):
		return err
	}

	cols, rows := terminalSize()
	wsURL := toWS(c.base) + attachPath + "?" + url.Values{
		"token": {token}, "cols": {strconv.Itoa(cols)}, "rows": {strconv.Itoa(rows)},
	}.Encode()

	// The key travels in the terminal path's own header, not in the query string
	// (ADR-065 §7): the token stays where ADR-024 put it, and a claim credential
	// does not belong in an intermediary's access log.
	wsHeaders := http.Header{}
	wsHeaders.Set(tun.TerminalHTTP.AttachKeyHeader, key)
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: wsHeaders})
	if err != nil {
		return fmt.Errorf("cannot open terminal: %w", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	conn.SetReadLimit(-1)
	return runTerminalPumps(ctx, wsTerminalConn{conn}, waking)
}

// terminalSize reads the local window, falling back to what a terminal that
// will not say is assumed to be.
func terminalSize() (cols, rows int) {
	cols, rows = 80, 24
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		if w, h, err := term.GetSize(fd); err == nil {
			cols, rows = w, h
		}
	}
	return cols, rows
}

// runTerminalPumps drives the session: raw mode, keystrokes as binary
// messages, SIGWINCH as a resize control message, output to stdout, and the
// server's `end` message as the reason the session stopped. It reads the
// transport through terminal.Conn alone, so the WebSocket rung and the HTTP
// rungs run this exact code.
func runTerminalPumps(ctx context.Context, conn terminal.Conn, announced bool) error {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		state, err := term.MakeRaw(fd)
		if err == nil {
			defer func() { _ = term.Restore(fd, state) }()
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Window-size changes → resize control message.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-winch:
				if w, h, err := term.GetSize(fd); err == nil {
					msg, _ := json.Marshal(map[string]any{"type": "resize", "cols": w, "rows": h})
					_ = conn.Write(ctx, terminal.MessageText, msg)
				}
			}
		}
	}()

	// Keystrokes → binary messages.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				if werr := conn.Write(ctx, terminal.MessageBinary, buf[:n]); werr != nil {
					cancel()
					return
				}
			}
			if err != nil {
				cancel()
				return
			}
		}
	}()

	// Server messages → stdout; a text message is the `end` control message.
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			// The server sends its end message and then lets the transport
			// close; over two wires those two events race, so a dead stream is
			// not necessarily the end of the story.
			reportTerminalEnd(drainTerminalEnd(conn))
			return nil
		}
		if typ == terminal.MessageText {
			if end, ok := terminalEndReason(data); ok {
				reportTerminalEnd(end)
				return nil
			}
			if note, ok := terminalWakingNote(data); ok {
				// At most once per session, whichever channel carried it first:
				// the mint's `state` and this frame are the same event seen
				// twice, and both exist because either can be missing. The
				// terminal is in raw mode by now, so the line needs its own
				// carriage return — the same shape reportTerminalEnd uses.
				if !announced {
					announced = true
					fmt.Fprintf(os.Stderr, "\r\n%s\r\n", note)
				}
			}
			continue
		}
		_, _ = os.Stdout.Write(data)
	}
}

// terminalEndReason reads the `end` control message, ignoring anything else.
// The message beside the reason is what the server knows and this process does
// not — which machine refused, and how (ADR-066 §3).
func terminalEndReason(data []byte) (sessionEnd, bool) {
	var end struct {
		Type   string `json:"type"`
		Reason string `json:"reason"`
		Msg    string `json:"msg"`
	}
	if json.Unmarshal(data, &end) != nil || end.Type != "end" {
		return sessionEnd{}, false
	}
	return sessionEnd{reason: end.Reason, message: end.Msg}, true
}

// terminalWakingNote reads ADR-067 §6's progress message, which arrives on the
// same text wire as the end frame and before any PTY byte does. Without it a
// shell into a sleeping preview shows a blank window for up to 75 s, which
// reads as a hung client rather than as the cold start the platform is
// deliberately paying — and no phrasing invented here could say which container
// it is waiting on, which is why the sentence is the server's.
//
// Anything that is neither an `end` nor a `waking` frame is still ignored: a
// client must drop the control-frame types it does not know, or a server that
// gains one breaks every client older than it.
func terminalWakingNote(data []byte) (string, bool) {
	var frame struct {
		Type string `json:"type"`
		Msg  string `json:"msg"`
	}
	if json.Unmarshal(data, &frame) != nil || frame.Type != wakeControlFrame || frame.Msg == "" {
		return "", false
	}
	return frame.Msg, true
}

// drainTerminalEnd gives an end message already in flight a bounded chance to
// land after the read loop was ended by the other half closing.
func drainTerminalEnd(conn terminal.Conn) sessionEnd {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return sessionEnd{}
		}
		if typ != terminal.MessageText {
			continue
		}
		if end, ok := terminalEndReason(data); ok {
			return end
		}
	}
}

// reportTerminalEnd tells the developer why the session stopped — unless they
// stopped it themselves, which needs no explanation.
func reportTerminalEnd(end sessionEnd) {
	if message := terminalEndMessage(end.reason, end.message); message != "" {
		fmt.Fprintf(os.Stderr, "\r\n%s\r\n", message)
	}
}

// terminalEndMessage phrases why the shell stopped. The terminal has no data
// stream to answer a 502 on — its one stream carries the PTY — so this line is
// the ONLY report channel for a failure that has nothing to do with a
// keystroke, and printing a bare enum value there sends the developer to search
// the source for it.
func terminalEndMessage(reason, message string) string {
	switch reason {
	case "", "user_close":
		// Ctrl-D or Ctrl-C: the developer already knows.
		return ""
	case "target_unreachable":
		// ADR-066: the attach answered before it dialled, so the shell that
		// never appeared is explained here rather than by a 409 at open. The
		// server's sentence names which leg failed — an agent that is not
		// connected, a container that does not exist, an SSH handshake that did
		// not complete — and no phrasing here can reconstruct that.
		if message != "" {
			return "session ended: " + message
		}
		return "session ended: the target could not be reached — check the server, its agent and the container"
	case "target_stopped":
		// ADR-067 §2: the container went away under the shell — a redeploy, an
		// operator's stop, a scale-to-zero sleep. Named rather than left to the
		// default arm because "target_stopped" reads as a diagnosis the
		// developer is supposed to already understand, and the one thing they
		// need to know is that rerunning is the remedy.
		if message != "" {
			return "session ended: " + message
		}
		return "session ended: the target container stopped (a redeploy, a manual stop, or scale-to-zero) — run the command again"
	case "wake_failed":
		// ADR-067: the resource was asleep and the wake did not finish.
		if message != "" {
			return "session ended: " + message
		}
		return "session ended: the target was asleep (scale-to-zero) and could not be woken — run the command again"
	default:
		// Everything else is a reason the developer can read as-is; a sentence
		// sent beside it is still better than the value, so it wins when present.
		if message != "" {
			return "session ended: " + message
		}
		return "session ended: " + reason
	}
}

// wsTerminalConn adapts coder/websocket to the bridge's Conn — the client-side
// twin of the server's adapter, so the pump above cannot tell the rungs apart.
type wsTerminalConn struct{ c *websocket.Conn }

func (w wsTerminalConn) Read(ctx context.Context) (terminal.MessageType, []byte, error) {
	typ, data, err := w.c.Read(ctx)
	if err != nil {
		switch websocket.CloseStatus(err) {
		case websocket.StatusNormalClosure, websocket.StatusGoingAway:
			return 0, nil, terminal.ErrClientClosed
		}
		return 0, nil, err
	}
	if typ == websocket.MessageText {
		return terminal.MessageText, data, nil
	}
	return terminal.MessageBinary, data, nil
}

func (w wsTerminalConn) Write(ctx context.Context, typ terminal.MessageType, data []byte) error {
	kind := websocket.MessageBinary
	if typ == terminal.MessageText {
		kind = websocket.MessageText
	}
	return w.c.Write(ctx, kind, data)
}

func (w wsTerminalConn) Ping(ctx context.Context) error { return w.c.Ping(ctx) }

// toWS converts an http(s) base URL to its ws(s) form.
func toWS(base string) string {
	if strings.HasPrefix(base, "https://") {
		return "wss://" + strings.TrimPrefix(base, "https://")
	}
	return "ws://" + strings.TrimPrefix(base, "http://")
}
