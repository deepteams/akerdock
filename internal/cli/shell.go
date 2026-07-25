package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func shellCmd() *cobra.Command {
	var component string
	cmd := &cobra.Command{
		Use:   "shell REF",
		Short: "Open an interactive shell in a container",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient(flags.context)
			if err != nil {
				return err
			}
			r, err := parseRef(args[0])
			if err != nil {
				return err
			}
			if r.kind != "apps" {
				return fmt.Errorf("shell currently supports app/… references")
			}
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

// attachTerminal bridges the local TTY to the terminal WebSocket: raw mode,
// binary frames both ways, resize on SIGWINCH, `end` control message.
func (c *Client) attachTerminal(ctx context.Context, wsPath, token string) error {
	fd := int(os.Stdin.Fd())
	cols, rows := 80, 24
	if term.IsTerminal(fd) {
		if w, h, err := term.GetSize(fd); err == nil {
			cols, rows = w, h
		}
	}

	wsURL := toWS(c.base) + wsPath + "?" + url.Values{
		"token": {token}, "cols": {strconv.Itoa(cols)}, "rows": {strconv.Itoa(rows)},
	}.Encode()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("cannot open terminal: %w", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	conn.SetReadLimit(-1)

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
					_ = conn.Write(ctx, websocket.MessageText, msg)
				}
			}
		}
	}()

	// Keystrokes → binary frames.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				if werr := conn.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
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

	// Server frames → stdout; a text frame is the `end` control message.
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return nil
		}
		if typ == websocket.MessageText {
			var end struct {
				Type, Reason string
			}
			if json.Unmarshal(data, &end) == nil && end.Type == "end" {
				if end.Reason != "" && end.Reason != "user_close" {
					fmt.Fprintf(os.Stderr, "\r\nsession ended: %s\r\n", end.Reason)
				}
				return nil
			}
			continue
		}
		_, _ = os.Stdout.Write(data)
	}
}

// toWS converts an http(s) base URL to its ws(s) form.
func toWS(base string) string {
	if strings.HasPrefix(base, "https://") {
		return "wss://" + strings.TrimPrefix(base, "https://")
	}
	return "ws://" + strings.TrimPrefix(base, "http://")
}
