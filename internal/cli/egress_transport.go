// The egress access path's use of the shared HTTP attach transport
// (httptransport.go): port-forward and bastion (ADR-032/045) on ADR-064's
// ladder.
//
// Mirror of ingress, and simpler for it: here the CLI opens a stream whenever a
// local client connects, so the session request carries no opens — only the
// session's liveness and, at the end, why it stopped.

package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	tun "github.com/deepteams/akerdock/internal/tunnel"
)

// egressSession is one attached port-forward over an HTTP transport.
type egressSession struct {
	control *tun.LineControl
	cancel  context.CancelFunc
	attach  *url.URL
	key     string
	uuid    string
	pool    *httpLanePool
}

// openEgressSession claims the mint token on lane 0 and holds that request open
// for the session's whole life: it is what proves the laptop is still there
// while no local client is connected, which is most of a port-forward's life.
func openEgressSession(
	ctx context.Context,
	pool *httpLanePool,
	attach *url.URL,
	token, key string,
	kind transportKind,
) (*egressSession, error) {
	sessionURL, err := withToken(attach, token)
	if err != nil {
		return nil, err
	}
	headers := http.Header{}
	headers.Set("Content-Type", tun.EgressHTTP.ControlContentType)
	headers.Set(tun.EgressHTTP.ProtocolHeader, tun.EgressHTTP.Name)
	headers.Set(tun.EgressHTTP.AttachKeyHeader, key)
	headers.Set(tun.EgressHTTP.TransportHeader, string(kind))

	stream, err := openAttachStream(ctx, pool, 0, sessionURL.String(), headers, kind, transportAttachTimeout)
	if err != nil {
		return nil, err
	}
	if stream.resp.Header.Get(tun.EgressHTTP.ProtocolHeader) != tun.EgressHTTP.Name {
		stream.cancel()
		_ = stream.writer.Close()
		_ = stream.resp.Body.Close()
		return nil, &attachRejection{
			kind: kind, status: stream.resp.Status, code: stream.resp.StatusCode,
			message: "the server did not echo " + tun.EgressHTTP.Name,
		}
	}
	// The session UUID the server binds data streams to travels in the same
	// header it will expect back on each of them.
	sessionUUID := stream.resp.Header.Get(tun.EgressHTTP.SessionHeader)
	control := tun.NewLineControl(stream.resp.Body, stream.writer, nil, func() error {
		stream.cancel()
		_ = stream.writer.Close()
		return stream.resp.Body.Close()
	})
	return &egressSession{
		control: control, cancel: stream.cancel, attach: attach,
		key: key, uuid: sessionUUID, pool: pool,
	}, nil
}

// openStream carries one accepted local connection. It is spread over the pool:
// nothing about a forwarded connection is pinned to the session's lane.
func (s *egressSession) openStream(ctx context.Context) (net.Conn, error) {
	headers := http.Header{}
	headers.Set("Content-Type", tun.EgressHTTP.StreamContentType)
	headers.Set(tun.EgressHTTP.SessionHeader, s.uuid)
	headers.Set(tun.EgressHTTP.AttachKeyHeader, s.key)

	stream, err := openAttachStream(ctx, s.pool, -1, s.attach.String(), headers, transportH2, transportDataOpenTimeout)
	if err != nil {
		return nil, err
	}
	return tun.NewDuplexConn(stream.resp.Body, stream.writer, nil, stream.cancel), nil
}

// run answers the server's liveness checks until the session ends, and reports
// the terminal reason the CLI decides re-dial versus exit on.
func (s *egressSession) run(ctx context.Context) (string, error) {
	for {
		frame, err := s.control.Receive()
		if err != nil {
			if ctx.Err() != nil {
				return "user_close", nil
			}
			return "", err
		}
		switch frame.Type {
		case "ping":
			if err := s.control.Send(ctx, tun.HTTPControlFrame{Type: "pong"}); err != nil {
				return "", err
			}
		case "session_close":
			return frame.Reason, nil
		}
	}
}

func (s *egressSession) close() {
	_ = s.control.Close()
	s.cancel()
}

// probeEgress asks the control plane, without spending the mint token, whether
// it carries this access path over this transport.
func probeEgress(ctx context.Context, pool *httpLanePool, attach *url.URL, kind transportKind) error {
	return probeAttach(ctx, pool, attach, kind, tun.EgressHTTP)
}

// egressAttachURL turns the mint's attach path into the probe URL.
func egressAttachURL(base, path string) (*url.URL, error) {
	if path == "" {
		return nil, fmt.Errorf("the mint response has no attach path")
	}
	return attachURL(base + path)
}

// errNoHTTPTransport says the ladder found no HTTP rung this path can carry —
// the caller falls through to WebSocket, which is the bottom rung and not a
// failure.
var errNoHTTPTransport = errors.New("no HTTP transport available")

// forwardOverHTTP runs one port-forward over the best HTTP transport available,
// serving the local listener until the session ends. It returns
// errNoHTTPTransport when neither rung could be negotiated, leaving the
// WebSocket path to take over.
func (c *Client) forwardOverHTTP(
	ctx context.Context,
	attachPath, token string,
	authorizedUntil *time.Time,
	localPort, remotePort int,
) error {
	attach, err := egressAttachURL(c.base, attachPath)
	if err != nil {
		return errNoHTTPTransport
	}
	state := newTransportState()
	preference := transportPreference()
	for _, kind := range preference[:len(preference)-1] {
		if !state.usable(kind) {
			continue
		}
		pool := newPool(kind)
		if err := probeEgress(ctx, pool, attach, kind); err != nil {
			state.noteProbeFailure(kind)
			_ = pool.Close()
			continue
		}
		key, err := tun.NewIngressAttachKey()
		if err != nil {
			_ = pool.Close()
			return err
		}
		session, err := openEgressSession(ctx, pool, attach, token, key, kind)
		if err != nil {
			_ = pool.Close()
			// A refused attach is the server's verdict on this session — an
			// expired token, a target that moved — and re-dialing it over the
			// WebSocket would only spend a second one. A transport failure is
			// the ladder's business, and steps down.
			var rejection *attachRejection
			if errors.As(err, &rejection) && !rejection.transportRefused() {
				return fmt.Errorf("cannot open tunnel: %s", rejection.message)
			}
			continue
		}
		fmt.Fprintf(os.Stderr, "tunnel transport: %s\n", kind.label())
		err = serveForwardSession(ctx, session, authorizedUntil, localPort, remotePort)
		session.close()
		_ = pool.Close()
		return err
	}
	return errNoHTTPTransport
}

// serveForwardSession accepts local connections and gives each one its own
// stream, until the session ends or the developer stops it.
func serveForwardSession(
	ctx context.Context,
	session *egressSession,
	authorizedUntil *time.Time,
	localPort, remotePort int,
) error {
	ln, err := listenAndAnnounce(ctx, localPort, remotePort, authorizedUntil)
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close() }()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ended := make(chan string, 1)
	go func() {
		reason, runErr := session.run(ctx)
		if runErr != nil {
			reason = ""
		}
		ended <- reason
		cancel()
	}()

	var streams sync.WaitGroup
	defer streams.Wait()
	for {
		local, acceptErr := ln.Accept()
		if acceptErr != nil {
			select {
			case reason := <-ended:
				if message := forwardCloseMessage(reason); message != "" {
					fmt.Fprintln(os.Stderr, message)
				}
				return nil
			default:
				if ctx.Err() != nil {
					return nil
				}
				return acceptErr
			}
		}
		streams.Add(1)
		go func() {
			defer streams.Done()
			remote, openErr := session.openStream(ctx)
			if openErr != nil {
				fmt.Fprintf(os.Stderr, "connection refused by the tunnel: %v\n", openErr)
				_ = local.Close()
				return
			}
			bridgeIngressConns(local, remote)
		}()
	}
}

// forwardCloseMessage phrases the server's end reason as something the
// developer can act on. A tunnel that dies without a word reads as a platform
// bug (ADR-045 §5).
func forwardCloseMessage(reason string) string {
	switch reason {
	case "idle_timeout":
		return "tunnel closed after its idle timeout — run the command again to reopen it"
	case "max_duration":
		return "tunnel closed: it reached its session limit — run the command again to reopen it"
	case "grant_expired":
		return "tunnel closed: the access grant expired — request access again"
	case "revoked":
		return "tunnel closed: an administrator revoked it"
	case "", "user_close":
		return ""
	default:
		return "tunnel closed (" + reason + ")"
	}
}
