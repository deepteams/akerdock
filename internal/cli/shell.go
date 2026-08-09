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
			}
			if err := c.do(cmd.Context(), http.MethodPost,
				"/applications/"+res.Uuid+"/terminal-sessions", q, struct{}{}, &sess); err != nil {
				return err
			}
			return c.attachTerminal(cmd.Context(), sess.WebsocketPath, sess.Token)
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
func (c *Client) attachTerminal(ctx context.Context, attachPath, token string) error {
	switch err := c.shellOverHTTP(ctx, attachPath, token); {
	case err == nil:
		return nil
	case !errors.Is(err, errNoHTTPTransport):
		return err
	}

	cols, rows := terminalSize()
	wsURL := toWS(c.base) + attachPath + "?" + url.Values{
		"token": {token}, "cols": {strconv.Itoa(cols)}, "rows": {strconv.Itoa(rows)},
	}.Encode()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("cannot open terminal: %w", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	conn.SetReadLimit(-1)
	return runTerminalPumps(ctx, wsTerminalConn{conn})
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
func runTerminalPumps(ctx context.Context, conn terminal.Conn) error {
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
			if reason, ok := terminalEndReason(data); ok {
				reportTerminalEnd(reason)
				return nil
			}
			continue
		}
		_, _ = os.Stdout.Write(data)
	}
}

// terminalEndReason reads the `end` control message, ignoring anything else.
func terminalEndReason(data []byte) (string, bool) {
	var end struct {
		Type, Reason string
	}
	if json.Unmarshal(data, &end) != nil || end.Type != "end" {
		return "", false
	}
	return end.Reason, true
}

// drainTerminalEnd gives an end message already in flight a bounded chance to
// land after the read loop was ended by the other half closing.
func drainTerminalEnd(conn terminal.Conn) string {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return ""
		}
		if typ != terminal.MessageText {
			continue
		}
		if reason, ok := terminalEndReason(data); ok {
			return reason
		}
	}
}

// reportTerminalEnd tells the developer why the session stopped — unless they
// stopped it themselves, which needs no explanation.
func reportTerminalEnd(reason string) {
	if reason == "" || reason == "user_close" {
		return
	}
	fmt.Fprintf(os.Stderr, "\r\nsession ended: %s\r\n", reason)
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
