// The ingress access path's use of the shared HTTP attach transport
// (httptransport.go): its URLs, its control choreography and the bridge that
// dials the developer's app. Everything protocol-agnostic lives one file over.

package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"

	tun "github.com/deepteams/akerdock/internal/tunnel"
)

// ingressHTTPURL is the probe URL: HTTP scheme, no token — a capability probe
// must never spend a single-use secret.
func ingressHTTPURL(sess ingressMint) (*url.URL, error) {
	return attachURL(sess.AttachUrl)
}

// ingressHTTPControlURL carries the authoritative token from the mint
// response, which replaces any stale query value without exposing it in logs.
func ingressHTTPControlURL(sess ingressMint) (*url.URL, error) {
	attach, err := ingressHTTPURL(sess)
	if err != nil {
		return nil, err
	}
	return withToken(attach, sess.Token)
}

func probeIngressHTTP(ctx context.Context, pool *httpLanePool, attach *url.URL, kind transportKind) error {
	return probeAttach(ctx, pool, attach, kind, tun.IngressHTTP)
}

type ingressHTTPControlSession struct {
	control *tun.LineControl
	cancel  context.CancelFunc
}

// openIngressHTTPControl claims the session on lane 0 and keeps that request
// open for the whole tunnel: it is the one stream the agent writes to when it
// needs the laptop to open a connection.
func openIngressHTTPControl(
	ctx context.Context,
	pool *httpLanePool,
	sess ingressMint,
	key string,
	kind transportKind,
) (*ingressHTTPControlSession, error) {
	controlURL, err := ingressHTTPControlURL(sess)
	if err != nil {
		return nil, err
	}
	headers := http.Header{}
	headers.Set("Content-Type", tun.IngressHTTP.ControlContentType)
	headers.Set(tun.IngressHTTP.ProtocolHeader, tun.IngressHTTP.Name)
	headers.Set(tun.IngressHTTP.AttachKeyHeader, key)
	headers.Set(tun.IngressHTTP.TransportHeader, string(kind))

	stream, err := openAttachStream(ctx, pool, 0, controlURL.String(), headers, kind, transportAttachTimeout)
	if err != nil {
		return nil, err
	}
	if stream.resp.Header.Get(tun.IngressHTTP.ProtocolHeader) != tun.IngressHTTP.Name {
		defer func() { _ = stream.resp.Body.Close() }()
		message, _ := io.ReadAll(io.LimitReader(stream.resp.Body, 4*1024))
		stream.cancel()
		_ = stream.writer.Close()
		return nil, &attachRejection{
			kind: kind, status: stream.resp.Status, code: stream.resp.StatusCode,
			message: "the peer did not echo " + tun.IngressHTTP.Name + ": " + string(message),
		}
	}
	control := tun.NewLineControl(stream.resp.Body, stream.writer, nil, func() error {
		stream.cancel()
		_ = stream.writer.Close()
		return stream.resp.Body.Close()
	})
	return &ingressHTTPControlSession{control: control, cancel: stream.cancel}, nil
}

// openIngressHTTPData opens one data stream, spread over the pool: it is the
// visitor's connection, and nothing about it is pinned to the control lane.
func openIngressHTTPData(
	ctx context.Context,
	pool *httpLanePool,
	attach *url.URL,
	sessionUUID, key string,
	id uint32,
	kind transportKind,
) (net.Conn, error) {
	headers := http.Header{}
	headers.Set("Content-Type", tun.IngressHTTP.StreamContentType)
	headers.Set(tun.IngressHTTP.SessionHeader, sessionUUID)
	headers.Set(tun.IngressHTTP.StreamHeader, strconv.FormatUint(uint64(id), 10))
	headers.Set(tun.IngressHTTP.AttachKeyHeader, key)

	stream, err := openAttachStream(ctx, pool, -1, attach.String(), headers, kind, ingressDataOpenTimeout)
	if err != nil {
		return nil, fmt.Errorf("stream %d: %w", id, err)
	}
	return tun.NewDuplexConn(stream.resp.Body, stream.writer, nil, stream.cancel), nil
}

// runIngressHTTPBridge answers the agent's opens until the session ends: one
// local dial and one data stream per visitor connection.
func runIngressHTTPBridge(
	ctx context.Context,
	control *tun.LineControl,
	pool *httpLanePool,
	attach *url.URL,
	sessionUUID, key string,
	localPort int,
	kind transportKind,
) (string, error) {
	workCtx, cancel := context.WithCancel(ctx)
	var streams sync.WaitGroup
	defer func() {
		cancel()
		streams.Wait()
	}()
	for {
		frame, err := control.Receive()
		if err != nil {
			if ctx.Err() != nil {
				return "user_close", nil
			}
			return "", err
		}
		switch frame.Type {
		case "open":
			streams.Add(1)
			go func(id uint32) {
				defer streams.Done()
				local, dialErr := dialIngressLocal(workCtx, localPort)
				if dialErr != nil {
					_ = control.Send(workCtx, tun.HTTPControlFrame{Type: "open_err", ID: id, Code: "dial_failed", Msg: dialErr.Error()})
					return
				}
				remote, openErr := openIngressHTTPData(workCtx, pool, attach, sessionUUID, key, id, kind)
				if openErr != nil {
					_ = local.Close()
					_ = control.Send(workCtx, tun.HTTPControlFrame{Type: "open_err", ID: id, Code: "stream_failed", Msg: openErr.Error()})
					return
				}
				bridgeIngressConns(local, remote)
			}(frame.ID)
		case "ping":
			if err := control.Send(workCtx, tun.HTTPControlFrame{Type: "pong"}); err != nil {
				return "", err
			}
		case "session_close":
			return frame.Reason, nil
		}
	}
}

// dialIngressLocal reaches the developer's app on the loopback. It resolves
// "localhost" rather than dialing 127.0.0.1 outright: a dev server bound to
// ::1 only (the default of more than one framework) is on the loopback too,
// and a tunnel that answers "connection refused" on it is a tunnel the
// developer cannot debug. Go's dual-stack dial tries both families.
func dialIngressLocal(ctx context.Context, port int) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", net.JoinHostPort("localhost", strconv.Itoa(port)))
}

func bridgeIngressConns(a, b net.Conn) {
	done := make(chan struct{}, 2)
	copyOne := func(dst, src net.Conn) {
		buf := ingressCopyBufferPool.Get().(*[]byte)
		_, _ = io.CopyBuffer(dst, src, *buf)
		ingressCopyBufferPool.Put(buf)
		done <- struct{}{}
	}
	go copyOne(a, b)
	go copyOne(b, a)
	<-done
	_ = a.Close()
	_ = b.Close()
	<-done
}

var ingressCopyBufferPool = sync.Pool{New: func() any {
	buf := make([]byte, 64*1024)
	return &buf
}}
