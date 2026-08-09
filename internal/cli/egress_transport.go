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
	"sync/atomic"
	"time"

	tun "github.com/deepteams/akerdock/internal/tunnel"
)

// ADR-067 §6's progress vocabulary, as this client reads it. Both families
// carry the same two values on their own wires — the frame is added to each
// access path's vocabulary and never pooled (ADR-064 §1) — and an older server
// sends neither, which is why nothing here is required for a session to work.
const (
	wakeControlFrame   = "waking"
	wakeFrameColdStart = "cold_start"
	wakeFrameReady     = "ready"

	// mintStateWaking is the same news on the mint response, one step earlier
	// (ADR-067 §6). Any other value — including the empty string an older
	// manager leaves — is `ready`, so no client has to tell "the target was up"
	// apart from "this server predates the field".
	mintStateWaking = "waking"

	// wakeColdStartNotice is what the developer reads. It names scale-to-zero,
	// because they did nothing to stop the target and have no reason to suspect
	// it, and it names the ceiling, because the only thing worse than a
	// 75-second wait is an unbounded-looking one. Deliberately the same sentence
	// the server puts in its cold-start frame: the two channels report one
	// event, and a developer who somehow saw both must not read two.
	wakeColdStartNotice = "the target is asleep (scale-to-zero) — starting it, this can take up to 75 s"
)

// egressSession is one attached port-forward over an HTTP transport.
type egressSession struct {
	control *tun.LineControl
	cancel  context.CancelFunc
	attach  *url.URL
	key     string
	uuid    string
	pool    *httpLanePool
	// kind is the rung the ladder actually settled on. It is carried rather
	// than assumed because every error a data stream produces is labelled with
	// it, and a label that lies about the transport sends the reader looking
	// at the wrong protocol.
	kind transportKind
	// waking is set while the target is cold-starting (ADR-067 §6) — from the
	// mint's own state, and then kept by the control frame. It changes one thing
	// on this side, how long the first connection may wait to be served, and it
	// is cleared as soon as the server says the target is up, so a session that
	// woke something does not keep an 85-second budget for the rest of its life.
	waking atomic.Bool
	// announced records that the cold start was already reported to the
	// developer — from the mint, before the listener was opened. The frame that
	// follows is the SAME event on a second channel, not a second event, and
	// printing it again would read as two wakes.
	announced atomic.Bool
}

// openEgressSession claims the mint token on lane 0 and holds that request open
// for the session's whole life: it is what proves the laptop is still there
// while no local client is connected, which is most of a port-forward's life.
func openEgressSession(
	ctx context.Context,
	pool *httpLanePool,
	attach *url.URL,
	minted mintedTunnel,
	key string,
	kind transportKind,
) (*egressSession, error) {
	sessionURL, err := withToken(attach, minted.token)
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
		return nil, &rejectedAttachError{
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
	session := &egressSession{
		control: control, cancel: stream.cancel, attach: attach,
		key: key, uuid: sessionUUID, pool: pool, kind: kind,
	}
	// The mint already told us, and already told the developer: carry both facts
	// in rather than waiting for the frame that repeats them, so the very first
	// connection gets the wider budget even if it beats the frame to the wire.
	session.waking.Store(minted.waking)
	session.announced.Store(minted.waking)
	return session, nil
}

// openStream carries one accepted local connection. It is spread over the pool:
// nothing about a forwarded connection is pinned to the session's lane.
func (s *egressSession) openStream(ctx context.Context) (net.Conn, error) {
	headers := http.Header{}
	headers.Set("Content-Type", tun.EgressHTTP.StreamContentType)
	headers.Set(tun.EgressHTTP.SessionHeader, s.uuid)
	headers.Set(tun.EgressHTTP.AttachKeyHeader, s.key)

	stream, err := openAttachStream(ctx, s.pool, -1, s.attach.String(), headers, s.kind, s.openTimeout())
	if err != nil {
		return nil, err
	}
	return tun.NewDuplexConn(stream.resp.Body, stream.writer, nil, stream.cancel), nil
}

// openTimeout is how long this stream may wait for its response head. A target
// the server told us is still cold-starting gets ADR-067's ceiling instead of
// the ordinary dial budget, because that is precisely the wait the server
// announced it was going to make us do.
func (s *egressSession) openTimeout() time.Duration {
	if s.waking.Load() {
		return egressWakeOpenTimeout
	}
	return egressDataOpenTimeout
}

// noteWaking renders ADR-067's progress frame and adjusts the one budget it
// affects.
//
// The cold-start notice is printed AT MOST ONCE per session, whichever channel
// carried it first: the mint's `state` and this frame are the same event seen
// twice, and both exist because either can be missing — an older manager sends
// no state, and a client that never read one still learns from the wire. Every
// other code is printed as it arrives, including one this build does not know:
// the sentence is the server's, and it is the whole point of the frame.
func (s *egressSession) noteWaking(code, msg string) {
	s.waking.Store(code != wakeFrameReady)
	if code == wakeFrameColdStart && s.announced.Swap(true) {
		return
	}
	if msg != "" {
		fmt.Fprintln(os.Stderr, msg)
	}
}

// run answers the server's liveness checks until the session ends, and reports
// the terminal reason the CLI decides re-dial versus exit on.
func (s *egressSession) run(ctx context.Context) (sessionEnd, error) {
	for {
		frame, err := s.control.Receive()
		if err != nil {
			// A read that fails once our own context is done failed BECAUSE of
			// it: Ctrl-C, or the caller giving up, tears the control stream down
			// under us. That is the session ending the way it was asked to, so
			// it ends on the reason that prints nothing, not on the read error.
			if ctx.Err() != nil {
				//nolint:nilerr // the read failed because we cancelled it, not the reverse.
				return sessionEnd{reason: "user_close"}, nil
			}
			return sessionEnd{}, err
		}
		switch frame.Type {
		case "ping":
			if err := s.control.Send(ctx, tun.HTTPControlFrame{Type: "pong"}); err != nil {
				return sessionEnd{}, err
			}
		case wakeControlFrame:
			// ADR-067 §6. A minute of apparent silence on a tunnel reads as the
			// bug this whole decision fixes, so the news is printed the moment it
			// arrives rather than folded into the close message that may never
			// come.
			s.noteWaking(frame.Code, frame.Msg)
		case "session_close":
			return sessionEnd{reason: frame.Reason, message: frame.Msg}, nil
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
//
// key is the caller's per-mint attach key (ADR-065 §3), not one per rung: it is
// what tells the server that a lower rung is the same attacher retrying rather
// than someone replaying the token, and generating it here would make every
// rung a stranger to the one above it.
func (c *Client) forwardOverHTTP(
	ctx context.Context,
	minted mintedTunnel,
	key string,
	localPort, remotePort int,
) error {
	attach, err := egressAttachURL(c.base, minted.attachPath)
	if err != nil {
		return errNoHTTPTransport
	}
	// A mint with no token is refused once, up front, rather than three times
	// over as an attach failure per rung.
	if _, err := withToken(attach, minted.token); err != nil {
		return err
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
		session, err := openEgressSession(ctx, pool, attach, minted, key, kind)
		if err != nil {
			_ = pool.Close()
			// A refused attach is the server's verdict on this session — an
			// expired token, a container that stopped, a replay from another
			// attacher — and a lower rung would only collect it again. Anything
			// else is the ladder's own business and steps down, which since
			// ADR-065 costs nothing: the next rung presents the SAME token with
			// the SAME attach key, so it re-takes the session an abandoned attach
			// may have claimed instead of finding it burnt. That is the exact
			// shape the terminal ladder has always had, and the reason this path
			// no longer needs a verdict enum of its own.
			var rejection *rejectedAttachError
			if errors.As(err, &rejection) && !rejection.transportRefused() {
				return fmt.Errorf("cannot open tunnel: %s", rejection.message)
			}
			continue
		}
		fmt.Fprintf(os.Stderr, "tunnel transport: %s\n", kind.label())
		err = serveForwardSession(ctx, session, minted.authorizedUntil, localPort, remotePort)
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
	// Derived BEFORE the listener exists, because the listener is what watches
	// it: listenAndAnnounce closes the socket when the context it was handed is
	// done, and the session ending is the whole of what has to reach it. Given
	// the caller's context instead, the accept below would keep blocking after
	// the server closed the tunnel — the command would hold a local port open,
	// print no reason, and only stop on Ctrl-C. The WebSocket rung derives it in
	// this order for the same reason.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	ln, err := listenAndAnnounce(ctx, localPort, remotePort, authorizedUntil)
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close() }()

	ended := make(chan sessionEnd, 1)
	go func() {
		end, runErr := session.run(ctx)
		if runErr != nil {
			end = sessionEnd{}
		}
		ended <- end
		cancel()
	}()

	var streams sync.WaitGroup
	defer streams.Wait()
	for {
		local, acceptErr := ln.Accept()
		if acceptErr != nil {
			select {
			case end := <-ended:
				if message := forwardCloseMessage(end.reason, end.message); message != "" {
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
//
// message is the sentence the server sent beside the reason, and where it
// exists it wins: the reasons that carry one are the ones whose cause lives on
// another machine, so the server knows which machine and this process does not.
func forwardCloseMessage(reason, message string) string {
	switch reason {
	case "target_unreachable":
		// ADR-066: the target was never reached at all — the agent was not
		// connected, the container was not running, the SSH handshake failed.
		// The listener is already open by then, so this is how the developer
		// learns their psql will not connect, and the server's own sentence
		// names which of the three it was.
		if message != "" {
			return "tunnel closed: " + message
		}
		return "tunnel closed: the target could not be reached — check the server, its agent and the target container"
	case "wake_failed":
		// ADR-067: the resource was asleep and the wake did not finish. Naming
		// scale-to-zero matters more here than anywhere else, because the
		// developer did nothing to stop it and has no reason to suspect it.
		if message != "" {
			return "tunnel closed: " + message
		}
		return "tunnel closed: the target was asleep (scale-to-zero) and could not be woken — " +
			"run the command again to retry"
	case "idle_timeout":
		return "tunnel closed after its idle timeout — run the command again to reopen it"
	case "max_duration":
		return "tunnel closed: it reached its session limit — run the command again to reopen it"
	case "grant_expired":
		return "tunnel closed: the access grant expired — request access again"
	case "revoked":
		return "tunnel closed: an administrator revoked it"
	case "target_stopped":
		return "tunnel closed: the target container stopped (it may have been put to sleep by scale-to-zero) — " +
			"run the command again to reopen it"
	case "", "user_close":
		return ""
	default:
		return "tunnel closed (" + reason + ")"
	}
}
