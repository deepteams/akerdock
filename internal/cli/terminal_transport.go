// The terminal access path's use of the shared HTTP attach transport
// (httptransport.go): `akerdock shell` (ADR-024) on ADR-064's ladder.
//
// The terminal is the ladder with exactly one data stream: the session request
// carries the control wire — resize, liveness, the end reason — and one data
// stream carries the PTY's bytes. terminal.HTTPConn merges the pair back into
// the typed messages the pump below reads, which is why the WebSocket rung and
// the HTTP rungs share that pump instead of each having their own.

package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"

	"github.com/deepteams/akerdock/internal/terminal"
	tun "github.com/deepteams/akerdock/internal/tunnel"
)

// terminalSession is one attached terminal over an HTTP transport: the control
// request, the data request, and the Conn merging them.
type terminalSession struct {
	conn   *terminal.HTTPConn
	cancel func()
}

func (s *terminalSession) close() {
	_ = s.conn.Close()
	s.cancel()
}

// probeTerminal asks the control plane, without spending the mint token,
// whether it carries this access path over this transport.
func probeTerminal(ctx context.Context, pool *httpLanePool, attach *url.URL, kind transportKind) error {
	return probeAttach(ctx, pool, attach, kind, tun.TerminalHTTP)
}

// terminalAttachURL turns the mint's attach path into the probe URL.
func terminalAttachURL(base, path string) (*url.URL, error) {
	if path == "" {
		return nil, fmt.Errorf("the mint response has no attach path")
	}
	return attachURL(base + path)
}

// openTerminalSession claims the mint token on the session request, then opens
// the one data stream that request authorized. The geometry travels in the
// query string, like the WebSocket rung: the PTY is sized before the first
// byte crosses, not resized after the shell has already drawn its prompt.
func openTerminalSession(
	ctx context.Context,
	pool *httpLanePool,
	attach *url.URL,
	token, key string,
	kind transportKind,
	cols, rows int,
) (*terminalSession, error) {
	sessionURL, err := withToken(attach, token)
	if err != nil {
		return nil, err
	}
	query := sessionURL.Query()
	query.Set("cols", strconv.Itoa(cols))
	query.Set("rows", strconv.Itoa(rows))
	sessionURL.RawQuery = query.Encode()

	headers := http.Header{}
	headers.Set("Content-Type", tun.TerminalHTTP.ControlContentType)
	headers.Set(tun.TerminalHTTP.ProtocolHeader, tun.TerminalHTTP.Name)
	headers.Set(tun.TerminalHTTP.AttachKeyHeader, key)
	headers.Set(tun.TerminalHTTP.TransportHeader, string(kind))

	control, err := openAttachStream(ctx, pool, 0, sessionURL.String(), headers, kind, transportAttachTimeout)
	if err != nil {
		return nil, err
	}
	closeControl := func() {
		control.cancel()
		_ = control.writer.Close()
		_ = control.resp.Body.Close()
	}
	if control.resp.Header.Get(tun.TerminalHTTP.ProtocolHeader) != tun.TerminalHTTP.Name {
		closeControl()
		return nil, &attachRejection{
			kind: kind, status: control.resp.Status, code: control.resp.StatusCode,
			message: "the server did not echo " + tun.TerminalHTTP.Name,
		}
	}
	sessionUUID := control.resp.Header.Get(tun.TerminalHTTP.SessionHeader)

	dataHeaders := http.Header{}
	dataHeaders.Set("Content-Type", tun.TerminalHTTP.StreamContentType)
	dataHeaders.Set(tun.TerminalHTTP.SessionHeader, sessionUUID)
	dataHeaders.Set(tun.TerminalHTTP.AttachKeyHeader, key)
	data, err := openAttachStream(ctx, pool, -1, attach.String(), dataHeaders, kind, transportDataOpenTimeout)
	if err != nil {
		closeControl()
		return nil, err
	}

	wire := tun.NewLineControl(control.resp.Body, control.writer, nil, func() error {
		control.cancel()
		_ = control.writer.Close()
		return control.resp.Body.Close()
	})
	stream := tun.NewDuplexConn(data.resp.Body, data.writer, nil, data.cancel)
	return &terminalSession{
		conn:   terminal.NewHTTPConn(wire, stream),
		cancel: func() { data.cancel(); control.cancel() },
	}, nil
}

// shellOverHTTP runs one terminal over the best HTTP transport available. It
// returns errNoHTTPTransport when no rung could be negotiated, leaving the
// WebSocket path — the bottom rung, not a failure — to take over.
func (c *Client) shellOverHTTP(ctx context.Context, attachPath, token string) error {
	attach, err := terminalAttachURL(c.base, attachPath)
	if err != nil {
		return errNoHTTPTransport
	}
	cols, rows := terminalSize()
	state := newTransportState()
	preference := transportPreference()
	for _, kind := range preference[:len(preference)-1] {
		if !state.usable(kind) {
			continue
		}
		pool := newPool(kind)
		if err := probeTerminal(ctx, pool, attach, kind); err != nil {
			state.noteProbeFailure(kind)
			_ = pool.Close()
			continue
		}
		key, err := tun.NewIngressAttachKey()
		if err != nil {
			_ = pool.Close()
			return err
		}
		session, err := openTerminalSession(ctx, pool, attach, token, key, kind, cols, rows)
		if err != nil {
			_ = pool.Close()
			// A refused attach is the server's verdict on this session — an
			// expired token, a container that stopped — and re-dialing it over
			// the WebSocket would only spend a second one. A transport failure
			// is the ladder's business, and steps down.
			var rejection *attachRejection
			if errors.As(err, &rejection) && !rejection.transportRefused() {
				return fmt.Errorf("cannot open terminal: %s", rejection.message)
			}
			continue
		}
		fmt.Fprintf(os.Stderr, "terminal transport: %s\r\n", kind.label())
		err = runTerminalPumps(ctx, session.conn)
		session.close()
		_ = pool.Close()
		return err
	}
	return errNoHTTPTransport
}
