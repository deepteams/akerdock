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
	// kind is the rung the ladder actually settled on. It is carried rather
	// than assumed because every error a data stream produces is labelled with
	// it, and a label that lies about the transport sends the reader looking
	// at the wrong protocol.
	kind transportKind
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

	stream, err := openAttachStream(ctx, pool, 0, sessionURL.String(), headers, kind, egressAttachTimeout)
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
		key: key, uuid: sessionUUID, pool: pool, kind: kind,
	}, nil
}

// openStream carries one accepted local connection. It is spread over the pool:
// nothing about a forwarded connection is pinned to the session's lane.
func (s *egressSession) openStream(ctx context.Context) (net.Conn, error) {
	headers := http.Header{}
	headers.Set("Content-Type", tun.EgressHTTP.StreamContentType)
	headers.Set(tun.EgressHTTP.SessionHeader, s.uuid)
	headers.Set(tun.EgressHTTP.AttachKeyHeader, s.key)

	stream, err := openAttachStream(ctx, s.pool, -1, s.attach.String(), headers, s.kind, egressDataOpenTimeout)
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

// tokenSpentError says the attach request DID reach the server carrying the
// mint token, and still came back a failure. The token is single-use and the
// server claims it before it resolves anything (ADR-032's attach handler), so
// it is burnt whatever the outcome — a lower rung, or the WebSocket underneath,
// would only present a spent token and collect a 401 that reads as the
// developer's fault. The only move left is a FRESH mint, which is
// runPortForward's business and is bounded there to exactly one.
type tokenSpentError struct {
	// reason is what the developer is told. It is set at classification time
	// rather than derived here, because the server's own 401 sentence describes
	// the symptom and not the cause.
	reason string
	// cause is kept for the reader of a wrapped error: it is the only evidence
	// of what the attempt actually observed.
	cause error
}

func (e *tokenSpentError) Error() string { return "cannot open tunnel: " + e.reason }
func (e *tokenSpentError) Unwrap() error { return e.cause }

// spentToken classifies a failed attach whose request went out. A 401 is the
// server telling us the token was already claimed — by a previous attempt that
// reached it and then gave up waiting — and repeating its sentence verbatim
// sends the developer looking for an expiry that never happened.
func spentToken(err error) *tokenSpentError {
	var rejection *attachRejection
	if errors.As(err, &rejection) && rejection.code == http.StatusUnauthorized {
		return &tokenSpentError{
			reason: "the attach token was already spent — a previous transport attempt reached the server " +
				"but timed out before it answered",
			cause: err,
		}
	}
	return &tokenSpentError{reason: err.Error(), cause: err}
}

// attachVerdict is what a failed SESSION attach means for the ladder. It exists
// because the answer is never "the transport failed, step down": the attach
// request carried the single-use token, and the server claims it before it
// inspects the container or dials SSH. Whether the token survived is therefore
// the first thing to establish, and only then whether the rung is at fault.
type attachVerdict int

const (
	// attachDescend: nothing was spent, the next rung is worth the same token.
	attachDescend attachVerdict = iota
	// attachRetireRung: the rung is genuinely out AND the token is spent — the
	// verdict came after the claim, so the retry must start below this rung.
	attachRetireRung
	// attachFinal: the server's answer would not change for a second session.
	attachFinal
	// attachSpent: the token is burnt and nothing but a fresh mint follows.
	attachSpent
)

// classifyAttachFailure reads a failed session attach. Descending the ladder on
// a burnt token wastes the two lower rungs and ends on a 401 whose sentence —
// "invalid, expired or already used tunnel token" — blames the developer for
// the ladder's own impatience, which is the bug this classification removes.
func classifyAttachFailure(err error) attachVerdict {
	var rejection *attachRejection
	if !errors.As(err, &rejection) {
		// A transport error or an open that timed out: the request went out, so
		// the server may well have claimed the token and then spent longer than
		// we waited resolving the target. Treat it as spent — the one reading
		// that is wrong costs a mint, the other costs the whole command.
		return attachSpent
	}
	switch rejection.code {
	case http.StatusUpgradeRequired:
		// The only answer that PREDATES the claim: the peer does not speak this
		// protocol at all, and refuses on the header alone.
		return attachDescend
	case http.StatusHTTPVersionNotSupported:
		// Full duplex is unavailable on this rung — a real transport verdict,
		// but one the server reaches only after claiming the token.
		return attachRetireRung
	case http.StatusUnauthorized:
		return attachSpent
	default:
		// A policy or target verdict: an unreachable server, a destroyed
		// preview, a session cap. A second mint would meet the same answer.
		return attachFinal
	}
}

// forwardOverHTTP runs one port-forward over the best HTTP transport available,
// serving the local listener until the session ends. It returns
// errNoHTTPTransport when neither rung could be negotiated, leaving the
// WebSocket path to take over.
//
// state is passed in rather than built here so that a rung's verdict survives
// the caller's re-mint: a peer that answered "not over this protocol" is no
// less refused for the token being fresh, and re-learning it would spend the
// one retry on the same rung.
func (c *Client) forwardOverHTTP(
	ctx context.Context,
	state *transportState,
	attachPath, token string,
	authorizedUntil *time.Time,
	localPort, remotePort int,
) error {
	attach, err := egressAttachURL(c.base, attachPath)
	if err != nil {
		return errNoHTTPTransport
	}
	// The token is validated once, up front, so that from here on EVERY failure
	// of openEgressSession is one where the attach request did go out. That is
	// the fact the classification below rests on, and deriving it from the
	// error would be guesswork.
	if _, err := withToken(attach, token); err != nil {
		return err
	}
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
			// The token has now been presented. Which of the four things that
			// means decides everything from here; the ladder itself only ever
			// keeps going on the first of them.
			switch classifyAttachFailure(err) {
			case attachDescend:
				continue
			case attachFinal:
				var rejection *attachRejection
				_ = errors.As(err, &rejection)
				return fmt.Errorf("cannot open tunnel: %s", rejection.message)
			case attachRetireRung:
				// Retired here so the fresh mint lands on the next rung down
				// rather than re-learning this verdict at the cost of the one
				// retry it gets.
				state.disable(kind)
			}
			return spentToken(err)
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
	case "target_stopped":
		return "tunnel closed: the target container stopped (it may have been put to sleep by scale-to-zero) — " +
			"run the command again to reopen it"
	case "", "user_close":
		return ""
	default:
		return "tunnel closed (" + reason + ")"
	}
}
