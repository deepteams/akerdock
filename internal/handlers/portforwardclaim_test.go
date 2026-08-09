// The egress attach after ADR-066 and ADR-065: it answers before it dials, and
// the token it spends belongs to the session it opens rather than to the request
// that tried.
//
// The two decisions are tested together because they are the same failure taken
// from two ends. ADR-066 removes the reason a first attempt was ever abandoned —
// a response head held open for an unpooled SSH handshake — and ADR-065 makes
// the abandonment survivable when it happens anyway. What a developer reported
// was one sentence, `invalid, expired or already used tunnel token`, false in all
// three of its terms.
//
// Every top-level identifier is prefixed pfclaim (concurrent-agent rule).
package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/tunnel"
)

// ---------------------------------------------------------------------------
// A store fake that remembers arguments, not only statements
// ---------------------------------------------------------------------------

// pfclaimDB records the SQL AND the arguments of every statement. The beat's
// fake only needs to know which statement ran; here the arguments are the
// decision: which attacher the claim stamped, which generation the close was
// guarded by, whether the heartbeat carried one at all.
type pfclaimDB struct {
	*netcovDB
	mu    sync.Mutex
	calls []pfclaimCall
}

type pfclaimCall struct {
	sql  string
	args []any
}

func (db *pfclaimDB) record(sql string, args []any) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.calls = append(db.calls, pfclaimCall{sql: sql, args: args})
}

// seen returns every call to the named sqlc query, in order.
func (db *pfclaimDB) seen(name string) []pfclaimCall {
	db.mu.Lock()
	defer db.mu.Unlock()
	var out []pfclaimCall
	for _, call := range db.calls {
		if strings.Contains(call.sql, "-- name: "+name+" ") {
			out = append(out, call)
		}
	}
	return out
}

// only asserts that exactly one call to name happened, and returns it.
func (db *pfclaimDB) only(t *testing.T, name string) pfclaimCall {
	t.Helper()
	calls := db.seen(name)
	if len(calls) != 1 {
		t.Fatalf("%s ran %d times, want exactly 1", name, len(calls))
	}
	return calls[0]
}

func (db *pfclaimDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	db.record(sql, args)
	return db.netcovDB.Exec(ctx, sql, args...)
}

func (db *pfclaimDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	db.record(sql, args)
	return db.netcovDB.Query(ctx, sql, args...)
}

func (db *pfclaimDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	db.record(sql, args)
	return db.netcovDB.QueryRow(ctx, sql, args...)
}

var _ store.DBTX = (*pfclaimDB)(nil)

func pfclaimAPI(t *testing.T) (*API, *pfclaimDB) {
	t.Helper()
	a, inner := netcovAPI(t)
	db := &pfclaimDB{netcovDB: inner}
	a.Store = store.New(db)
	return a, db
}

// pfclaimRow is a claimed session against server 1 and resource 9 — the shape
// netcovAgent scripts a channel for — with a token that is still redeemable and
// no ADR-045 grant, so the bridge gets its default bounds rather than a
// millisecond of budget.
func pfclaimRow() store.PortForwardSession {
	server, resource := int64(1), int64(9)
	return store.PortForwardSession{
		ID: 42, TeamID: 1, ServerID: &server, ResourceID: &resource,
		TargetName: "shop", TargetPort: 3000, AttachSeq: 1,
		TokenExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(portForwardTokenTTL), Valid: true},
	}
}

func pfclaimClaims(db *pfclaimDB, row store.PortForwardSession) {
	_ = row.Uuid.Scan(fixtureUUID)
	db.rule(netcovRule{match: "-- name: ClaimPortForwardSession ", set: netcovRowOf(row)})
}

// ---------------------------------------------------------------------------
// Driving one session request
// ---------------------------------------------------------------------------

// pfclaimWriter is a recorder that says WHEN the head left, and can be made to
// fail the flush the way a client that walked away does.
type pfclaimWriter struct {
	*httptest.ResponseRecorder
	flushErr error
	// atFlush is sampled exactly once, the instant the head is flushed — which
	// is the only moment at which "nothing remote has been attempted yet" can be
	// observed rather than inferred.
	atFlush  func()
	flushed  chan struct{}
	flushOne sync.Once
}

func pfclaimNewWriter() *pfclaimWriter {
	return &pfclaimWriter{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{})}
}

func (w *pfclaimWriter) Unwrap() http.ResponseWriter { return w.ResponseRecorder }

func (w *pfclaimWriter) FlushError() error {
	w.flushOne.Do(func() {
		if w.atFlush != nil {
			w.atFlush()
		}
		close(w.flushed)
	})
	if w.flushErr != nil {
		return w.flushErr
	}
	w.Flush()
	return nil
}

// pfclaimSession is one running session request plus the handles a test needs:
// its control wire (the request body), and when it finished.
type pfclaimSession struct {
	writer *pfclaimWriter
	body   *io.PipeWriter
	cancel context.CancelFunc
	done   chan struct{}
}

// end closes the control wire the way a CLI leaving does, and waits for the
// handler to return.
func (s *pfclaimSession) end(t *testing.T) {
	t.Helper()
	_ = s.body.Close()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the session request never returned")
	}
}

func (s *pfclaimSession) awaitHead(t *testing.T) {
	t.Helper()
	select {
	case <-s.flushedOrDone():
	case <-time.After(5 * time.Second):
		t.Fatal("the session request never produced a response head")
	}
}

// flushedOrDone unblocks on the head OR on a handler that returned without one,
// so a refusal fails the assertion instead of the whole test timing out.
func (s *pfclaimSession) flushedOrDone() <-chan struct{} {
	out := make(chan struct{})
	go func() {
		select {
		case <-s.writer.flushed:
		case <-s.done:
		}
		close(out)
	}()
	return out
}

// pfclaimOpen runs tunnelAttachSession on a request that speaks the egress wire
// over HTTP/2, so full duplex is native and the handler reaches its head.
func pfclaimOpen(t *testing.T, a *API, key string, writer *pfclaimWriter) *pfclaimSession {
	t.Helper()
	reader, body := io.Pipe()
	request := httptest.NewRequest(http.MethodPost, tunnelAttachPath+"?token=akdp_unit", reader)
	request.ProtoMajor, request.ProtoMinor = 2, 0
	request.Header.Set("Content-Type", tunnel.EgressHTTP.ControlContentType)
	request.Header.Set(tunnel.EgressHTTP.ProtocolHeader, tunnel.EgressHTTP.Name)
	request.Header.Set(tunnel.EgressHTTP.AttachKeyHeader, key)
	ctx, cancel := context.WithCancel(context.Background())
	request = request.WithContext(ctx)

	session := &pfclaimSession{writer: writer, body: body, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(session.done)
		a.tunnelAttachSession(writer, request)
	}()
	t.Cleanup(func() {
		cancel()
		_ = body.Close()
		<-session.done
	})
	return session
}

// pfclaimStream opens one data stream against a live session.
func pfclaimStream(ctx context.Context, a *API, key, sessionUUID string) (*pfclaimWriter, <-chan struct{}) {
	writer := pfclaimNewWriter()
	reader, _ := io.Pipe()
	request := httptest.NewRequest(http.MethodPost, tunnelAttachPath, reader).WithContext(ctx)
	request.ProtoMajor, request.ProtoMinor = 2, 0
	request.Header.Set("Content-Type", tunnel.EgressHTTP.StreamContentType)
	request.Header.Set(tunnel.EgressHTTP.AttachKeyHeader, key)
	request.Header.Set(tunnel.EgressHTTP.SessionHeader, sessionUUID)
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.tunnelAttachStream(writer, request)
	}()
	return writer, done
}

// pfclaimBlockingAgent scripts ContainerInspect to hold until release is closed,
// then answer with a routable container IP. It is the seam for "the remote half
// is slow": a real agent RPC on the real code path, stopped where the SSH
// handshake would otherwise be.
func pfclaimBlockingAgent(t *testing.T, a *API, release <-chan struct{}) *pfclaimCounter {
	t.Helper()
	counter := &pfclaimCounter{}
	netcovAgent(t, a, func(cmd agentwire.Command) (*agentwire.Result, bool) {
		counter.hit()
		<-release
		if cmd.Method != agentwire.MethodContainerInspect {
			return &agentwire.Result{Err: &agentwire.Error{Code: agentwire.CodeInvalid, Message: "unexpected"}}, false
		}
		return &agentwire.Result{Body: []byte(
			`{"NetworkSettings":{"Networks":{"bridge":{"IPAddress":"127.0.0.1"}}}}`)}, false
	})
	return counter
}

type pfclaimCounter struct {
	mu sync.Mutex
	n  int
}

func (c *pfclaimCounter) hit() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
}

func (c *pfclaimCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// pfclaimSSHConns counts the TCP connections the loopback SSH server accepted.
// One per session, however many streams ride it, is the whole point of the SSH
// client outliving them.
func pfclaimSSHConns(s *netcovSSHServer) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// pfclaimFrames decodes the control frames the session wrote.
func pfclaimFrames(t *testing.T, writer *pfclaimWriter) []tunnel.HTTPControlFrame {
	t.Helper()
	var out []tunnel.HTTPControlFrame
	for _, line := range strings.Split(writer.Body.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var frame tunnel.HTTPControlFrame
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("control frame %q: %v", line, err)
		}
		out = append(out, frame)
	}
	return out
}

// pfclaimEndReason reads the end_reason bound to the one EndPortForwardSession
// call, and the generation it was guarded by.
func pfclaimEndReason(t *testing.T, db *pfclaimDB) (store.TerminalEndReason, *int64) {
	t.Helper()
	call := db.only(t, "EndPortForwardSession")
	if len(call.args) != 3 {
		t.Fatalf("EndPortForwardSession args = %v", call.args)
	}
	reason, ok := call.args[1].(*store.TerminalEndReason)
	if !ok || reason == nil {
		t.Fatalf("end_reason argument = %#v", call.args[1])
	}
	seq, _ := call.args[2].(*int64)
	return *reason, seq
}

// ---------------------------------------------------------------------------
// ADR-066 — the attach answers before it dials
// ---------------------------------------------------------------------------

// The defect itself: the head used to wait for an agent RPC and an unpooled SSH
// handshake bounded only by ssh_timeout_seconds — 30 s by default — while the
// client's open budget was 5 s. Whatever that budget is, it was a bet on someone
// else's network. The head must now be produced from local state alone, so a
// remote half that never answers at all costs it nothing.
func TestPfclaimSessionAnswersBeforeAnyRemoteLegIsAttempted(t *testing.T) {
	a, db := pfclaimAPI(t)
	pfclaimClaims(db, pfclaimRow())
	release := make(chan struct{})
	defer close(release)
	agent := pfclaimBlockingAgent(t, a, release)

	writer := pfclaimNewWriter()
	var startedAtHead int
	writer.atFlush = func() { startedAtHead = agent.count() }
	key, _ := freshAttachKey(t)
	session := pfclaimOpen(t, a, key, writer)

	session.awaitHead(t)
	if startedAtHead != 0 {
		t.Fatalf("the remote half had already been attempted (%d calls) when the head was flushed",
			startedAtHead)
	}
	if writer.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a resolver blocked for the whole test must not hold the head",
			writer.Code)
	}
	if got := writer.Header().Get(tunnel.EgressHTTP.SessionHeader); got == "" {
		t.Fatal("the head must carry the session the CLI binds its data streams to")
	}
}

// The one ordering requirement the change brings, and the terminal is its
// precedent: the CLI opens its data stream the moment it reads the head, so a
// session not yet in the register would answer it `unknown tunnel session`.
// Answering earlier is exactly what would widen that window.
func TestPfclaimAttachIsRegisteredBeforeTheHeadIsFlushed(t *testing.T) {
	a, db := pfclaimAPI(t)
	pfclaimClaims(db, pfclaimRow())
	release := make(chan struct{})
	defer close(release)
	pfclaimBlockingAgent(t, a, release)

	writer := pfclaimNewWriter()
	key, hash := freshAttachKey(t)
	var registeredAtHead bool
	writer.atFlush = func() { registeredAtHead = a.egressLookup(fixtureUUID, hash) != nil }
	session := pfclaimOpen(t, a, key, writer)

	session.awaitHead(t)
	if !registeredAtHead {
		t.Fatal("a data stream presented the instant the head is read would have been refused 401")
	}
}

// A stream that arrives while the resolution is in flight WAITS. It is not
// refused, and the wait is not unbounded: the await and the dial share the one
// EgressDialTimeout context the stream already built, so a stream that waits
// cannot hang longer than a stream that dials.
func TestPfclaimStreamArrivingDuringResolutionWaitsAndIsThenServed(t *testing.T) {
	a, db := pfclaimAPI(t)
	sshServer := netcovNewSSHServer(t, false)
	netcovProvisionSSH(t, a, db.netcovDB, sshServer)
	pfclaimClaims(db, pfclaimRow())
	release := make(chan struct{})
	pfclaimBlockingAgent(t, a, release)

	key, hash := freshAttachKey(t)
	session := pfclaimOpen(t, a, key, pfclaimNewWriter())
	session.awaitHead(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stream, streamDone := pfclaimStream(ctx, a, key, fixtureUUID)
	select {
	case <-stream.flushed:
		t.Fatal("the stream answered before the target was even resolved")
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	select {
	case <-stream.flushed:
	case <-streamDone:
	case <-time.After(tunnel.EgressDialTimeout + 5*time.Second):
		t.Fatal("the stream never got served once the resolution completed")
	}
	if stream.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a stream that waited must be served, not refused", stream.Code)
	}

	// The SSH client outlives every data stream: a second one is served without
	// a second handshake, which is what "ownership did not move, only the moment
	// of acquisition did" has to mean in practice.
	second, secondDone := pfclaimStream(ctx, a, key, fixtureUUID)
	select {
	case <-second.flushed:
	case <-secondDone:
	case <-time.After(tunnel.EgressDialTimeout + 5*time.Second):
		t.Fatal("the second stream was never served")
	}
	if second.Code != http.StatusOK {
		t.Fatalf("second stream status = %d, want 200", second.Code)
	}
	if got := pfclaimSSHConns(sshServer); got != 1 {
		t.Fatalf("SSH connections = %d, want exactly 1: the client is dialed once and outlives every stream", got)
	}

	attach := a.egressLookup(fixtureUUID, hash)
	if attach == nil {
		t.Fatal("the live session left the register while it was still running")
	}
	session.end(t)
	<-streamDone
	<-secondDone
	// And closed exactly once, when the session ends — not by whichever stream
	// happened to finish last.
	if _, err := attach.dial(ctx); err == nil {
		t.Fatal("the SSH client outlived the session that owned it")
	}
	if got := pfclaimSSHConns(sshServer); got != 1 {
		t.Fatalf("SSH connections = %d after teardown, want no re-dial", got)
	}
}

// A resolution that never completes must read as a dial that never completed —
// the same 502 target_unreachable, no new status and no new error code, which is
// the whole reason early streams wait rather than being told to come back.
func TestPfclaimStreamThatOutwaitsTheResolutionAnswersLikeAFailedDial(t *testing.T) {
	a, db := pfclaimAPI(t)
	pfclaimClaims(db, pfclaimRow())
	release := make(chan struct{})
	defer close(release)
	pfclaimBlockingAgent(t, a, release)

	key, _ := freshAttachKey(t)
	session := pfclaimOpen(t, a, key, pfclaimNewWriter())
	session.awaitHead(t)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	stream, streamDone := pfclaimStream(ctx, a, key, fixtureUUID)
	select {
	case <-streamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream hung instead of spending its own budget")
	}
	if stream.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 — the same answer a dial that never completes gives", stream.Code)
	}
	if body := stream.Body.String(); !strings.Contains(body, "target_unreachable") {
		t.Fatalf("body = %s, want the existing vocabulary rather than a new code", body)
	}
}

// A failure that belongs to no particular stream is reported on the session,
// before it closes: the reason the row persists and the sentence the developer
// reads must come from the same place. `disconnect` would send them to inspect
// their own network for an agent that is not connected.
func TestPfclaimFailedResolutionEndsTheSessionWithTargetUnreachable(t *testing.T) {
	a, db := pfclaimAPI(t)
	pfclaimClaims(db, pfclaimRow())
	// No agent registry at all: containerIP fails as unavailable, which is the
	// commonest of the five refusals ADR-066 §4 moves behind the head.
	key, _ := freshAttachKey(t)
	writer := pfclaimNewWriter()
	session := pfclaimOpen(t, a, key, writer)
	session.awaitHead(t)

	select {
	case <-session.done:
	case <-time.After(5 * time.Second):
		t.Fatal("an unreachable target left the session open and the listener forwarding to nothing")
	}

	var closed *tunnel.HTTPControlFrame
	for _, frame := range pfclaimFrames(t, writer) {
		if frame.Type == "session_close" {
			f := frame
			closed = &f
		}
	}
	if closed == nil {
		t.Fatalf("no session_close frame in %q", writer.Body.String())
	}
	if closed.Reason != string(endReasonTargetUnreachable) {
		t.Fatalf("reason = %q, want target_unreachable", closed.Reason)
	}
	if !strings.Contains(closed.Msg, "agent is not connected") {
		t.Fatalf("msg = %q — the reason alone names no target", closed.Msg)
	}

	reason, seq := pfclaimEndReason(t, db)
	if reason != store.TerminalEndReason(endReasonTargetUnreachable) {
		t.Fatalf("persisted end_reason = %q, want target_unreachable and never disconnect or revoked", reason)
	}
	if seq == nil || *seq != 1 {
		t.Fatalf("close generation = %v, want the attach's own", seq)
	}
}

// The same failure on the stream channel: a 502 carrying the RESOLVER's
// sentence, not a dial's, because there was never a dial to quote.
func TestPfclaimStreamCarriesTheResolversSentence(t *testing.T) {
	a := &API{}
	resolution := &egressResolution{done: make(chan struct{})}
	resolution.settle(nil, "", "the target container is not running")
	key, hash := freshAttachKey(t)
	a.egressRegister("session-1", &egressAttach{key: hash, dial: resolution.dial})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, done := pfclaimStream(ctx, a, key, "session-1")
	<-done
	if stream.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", stream.Code)
	}
	body := stream.Body.String()
	if !strings.Contains(body, "the target container is not running") {
		t.Fatalf("body = %s — the 502 must carry the resolver's own words", body)
	}
	if !strings.Contains(body, "target_unreachable") {
		t.Fatalf("body = %s — typed so the CLI can tell it apart", body)
	}
}

// The local half keeps its 409 and its prose: these are facts the control plane
// holds locally and can state in microseconds, and their refusals are the
// actionable ones. A refusal on the merits also finalizes the row — a re-claim
// would only reproduce the same verdict three more times.
func TestPfclaimLocalHalfStillRefusesBeforeTheHead(t *testing.T) {
	cases := map[string]struct {
		row     func() store.PortForwardSession
		arrange func(db *pfclaimDB)
		want    string
	}{
		"server gone": {
			row: func() store.PortForwardSession {
				row := pfclaimRow()
				row.ServerID = nil
				return row
			},
			want: "no longer exists",
		},
		"server row deleted": {
			row:     pfclaimRow,
			arrange: func(db *pfclaimDB) { db.rule(netcovRule{match: "-- name: GetServerByID ", noRows: true}) },
			want:    "server no longer exists",
		},
		"resource deleted": {
			row:     pfclaimRow,
			arrange: func(db *pfclaimDB) { db.rule(netcovRule{match: "-- name: GetResourceByID ", noRows: true}) },
			want:    "resource no longer exists",
		},
		"destroyed preview": {
			row: func() store.PortForwardSession {
				row := pfclaimRow()
				preview := int64(11)
				row.PreviewID = &preview
				return row
			},
			arrange: func(db *pfclaimDB) {
				db.rule(netcovRule{match: "-- name: GetPreviewByID ", typed: []any{store.PreviewStatusDestroyed}})
			},
			want: "destroyed",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			a, db := pfclaimAPI(t)
			pfclaimClaims(db, tc.row())
			if tc.arrange != nil {
				tc.arrange(db)
			}
			key, _ := freshAttachKey(t)
			writer := pfclaimNewWriter()
			session := pfclaimOpen(t, a, key, writer)
			<-session.done

			if writer.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409 before the head", writer.Code)
			}
			if !strings.Contains(writer.Body.String(), tc.want) {
				t.Fatalf("body = %s, want %q", writer.Body.String(), tc.want)
			}
			select {
			case <-writer.flushed:
				t.Fatal("a 409 must be answered before any head is committed")
			default:
			}
			if reason, _ := pfclaimEndReason(t, db); reason != store.TerminalEndReason(endReasonTargetUnreachable) {
				t.Fatalf("end_reason = %q, want the token finalized as target_unreachable", reason)
			}
		})
	}
}

// The promise's late-arrival release is the only thing standing between a 30 s
// handshake and one leaked SSH client per abandoned session, so ADR-066 asks for
// a test rather than trusting the shape of the code.
func TestPfclaimResolutionReleasesWhatArrivesAfterTeardown(t *testing.T) {
	sshServer := netcovNewSSHServer(t, false)
	a, db := netcovAPI(t)
	netcovProvisionSSH(t, a, db, sshServer)
	server, err := a.Store.GetServerByID(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	client, msg := a.dialSessionServer(context.Background(), server)
	if msg != "" {
		t.Fatalf("dial = %q", msg)
	}

	resolution := &egressResolution{done: make(chan struct{})}
	// The session tore down while the handshake was still in flight.
	resolution.release()
	resolution.settle(client, "127.0.0.1:3000", "")

	if _, err := client.DialTCP("127.0.0.1:3000"); err == nil {
		t.Fatal("a resolution that completes after teardown left its SSH client open")
	}
	// And releasing twice closes nothing twice: the client is closed exactly
	// once, when the session ends.
	resolution.release()
}

// A session that ends mid-handshake cancels the handshake: the resolution runs
// under the session request's context, and nothing about it outlives the request
// that owns it.
func TestPfclaimResolutionFollowsTheSessionsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	resolution := &egressResolution{done: make(chan struct{})}
	observed := make(chan error, 1)
	resolution.start(ctx, func(ctx context.Context) (*sshexec.Client, string, string) {
		<-ctx.Done()
		observed <- ctx.Err()
		return nil, "", "cancelled"
	})
	cancel()
	select {
	case err := <-observed:
		if err == nil {
			t.Fatal("the resolver was not cancelled with its session")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled session left its handshake running")
	}
}

// ---------------------------------------------------------------------------
// ADR-065 — the token is spent by the session, not by the request
// ---------------------------------------------------------------------------

// The claim must be exactly one statement, and the idempotence must live in its
// WHERE: a read-then-write would race two rungs of the same ladder against each
// other and produce two attaches that both believe they own the session — the
// one failure mode strict single-use never had.
func TestPfclaimClaimIsOneStatementBoundToTheAttacher(t *testing.T) {
	a, db := pfclaimAPI(t)
	pfclaimClaims(db, pfclaimRow())
	key, hash := freshAttachKey(t)
	session := pfclaimOpen(t, a, key, pfclaimNewWriter())
	session.awaitHead(t)

	call := db.only(t, "ClaimPortForwardSession")
	if !strings.HasPrefix(strings.TrimSpace(strings.SplitN(call.sql, "\n", 2)[1]), "UPDATE port_forward_sessions") {
		t.Fatalf("the claim is no longer one UPDATE: %s", call.sql)
	}
	for _, clause := range []string{
		// The re-claim window, and the whole of it.
		"token_expires_at > now()",
		// The replay: a different attacher matches zero rows.
		"(attach_key_hash IS NULL OR attach_key_hash = $2)",
		// A revocation between two rungs beats the re-claim, twice over.
		"ended_at IS NULL",
		"(authorized_until IS NULL OR authorized_until > now())",
		// A retry must not buy itself duration by restarting the ceiling.
		"coalesce(claimed_at, now())",
		"CASE WHEN attach_seq = 0 THEN now() ELSE started_at END",
	} {
		if !strings.Contains(call.sql, clause) {
			t.Errorf("the claim no longer carries %q:\n%s", clause, call.sql)
		}
	}
	stored, ok := call.args[1].([]byte)
	if !ok || !bytes.Equal(stored, hash[:]) {
		t.Fatalf("the claim stamped %x, want the hash of the presented attach key", stored)
	}
}

// The compatibility shim that must not become a replay hole: an attach with no
// key at all — an N-1 CLI mid rolling upgrade, the dashboard's browser terminal
// — stores server-generated random bytes. A NULL, or a fixed sentinel, would
// make that token freely re-claimable for 60 s by whoever holds it.
func TestPfclaimKeylessAttachStaysStrictlySingleUse(t *testing.T) {
	first, err := attachClaimKey("")
	if err != nil {
		t.Fatal(err)
	}
	second, err := attachClaimKey("")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("two keyless attaches stored the same value — one token would re-claim the other")
	}
	if first == ([sha256.Size]byte{}) {
		t.Fatal("a keyless attach stored a sentinel that anything could match")
	}
	// And no presentable key hashes to it: the stored value is not the hash of
	// any 256-bit key an attacker could send.
	presented, hash := freshAttachKey(t)
	decoded, err := attachClaimKey(presented)
	if err != nil || decoded != hash {
		t.Fatalf("a presented key must still be stored as its own hash: %x, %v", decoded, err)
	}
	if decoded == first {
		t.Fatal("a presented key matched a keyless attach's stored value")
	}
}

// A successful re-claim means the previous attach lost. It must be cut, not left
// running beside the new one: two live attaches on one session is exactly what a
// mint does not authorize, and it would mean two SSH clients against the target.
func TestPfclaimReclaimSupersedesTheIncumbentAttach(t *testing.T) {
	t.Run("a re-claim cuts the attach it displaced", func(t *testing.T) {
		a, _ := pfclaimAPI(t)
		var logs bytes.Buffer
		a.Logger = slog.New(slog.NewTextHandler(&logs, nil))
		row := pfclaimRow()
		row.AttachSeq = 2
		incumbent := a.Tunnels.register(row.ID)
		defer a.Tunnels.unregister(row.ID, incumbent)

		a.supersedePortForwardAttach(row)
		select {
		case reason := <-incumbent:
			if reason != endReasonSuperseded {
				t.Fatalf("reason = %q, want superseded — a silent overwrite is not a cut", reason)
			}
		default:
			t.Fatal("the displaced attach was left running beside its successor")
		}
		// Operational noise belongs in a log line, never the attach key or the
		// token (§23.2).
		if line := logs.String(); !strings.Contains(line, "superseded") {
			t.Fatalf("the supersession went unrecorded: %q", line)
		}
	})

	t.Run("a first claim cuts nothing", func(t *testing.T) {
		a, _ := pfclaimAPI(t)
		row := pfclaimRow()
		incumbent := a.Tunnels.register(row.ID)
		defer a.Tunnels.unregister(row.ID, incumbent)

		a.supersedePortForwardAttach(row)
		select {
		case reason := <-incumbent:
			t.Fatalf("a first claim cut a live session with %q", reason)
		default:
		}
	})

	// The register is keyed on the session row, so the loser leaving must not
	// evict the winner — otherwise a revocation could no longer reach the tunnel
	// that is actually live, and shutdown would stop waiting for it.
	t.Run("the loser leaving does not evict the winner", func(t *testing.T) {
		var p TunnelPresence
		loser := p.register(1)
		winner := p.register(1)
		p.unregister(1, loser)
		if !p.Cut(1, tunnel.EndUserClose) {
			t.Fatal("the winner's entry was deleted by the attach it superseded")
		}
		select {
		case <-winner:
		default:
			t.Fatal("the cut did not reach the live attach")
		}
	})
}

// A superseded attach never finalizes the session: the row stays open for the
// winner, and no close audit is emitted for the loser.
func TestPfclaimSupersededAttachNeverFinalizesTheSession(t *testing.T) {
	t.Run("superseded is never persisted", func(t *testing.T) {
		a, db := pfclaimAPI(t)
		a.endPortForwardAttach(pfclaimRow(), endReasonSuperseded)
		if calls := db.seen("EndPortForwardSession"); len(calls) != 0 {
			t.Fatalf("a superseded socket closed the session its successor is using (%d calls)", len(calls))
		}
	})

	t.Run("an attach closes under its own generation", func(t *testing.T) {
		a, db := pfclaimAPI(t)
		row := pfclaimRow()
		row.AttachSeq = 3
		a.endPortForwardAttach(row, tunnel.EndIdleTimeout)
		_, seq := pfclaimEndReason(t, db)
		if seq == nil || *seq != 3 {
			t.Fatalf("close generation = %v, want the attach's own", seq)
		}
	})

	// Revocation, the operator's close and the sweep pass no generation: their
	// verdict is about the session, not about whichever socket holds it.
	t.Run("an operator close is unconditional", func(t *testing.T) {
		a, db := pfclaimAPI(t)
		a.endPortForwardSession(pfclaimRow(), tunnelEndReasonRevoked)
		if _, seq := pfclaimEndReason(t, db); seq != nil {
			t.Fatalf("close generation = %v, want NULL for a verdict about the session", *seq)
		}
	})
}

// Supersession across replicas has no new mechanism: the heartbeat carries the
// generation, and the bridge already ends itself when the beat updates zero rows
// — "another replica finalized this" and "another attach superseded me" are the
// same sentence.
func TestPfclaimHeartbeatCarriesTheAttachGeneration(t *testing.T) {
	a, db := pfclaimAPI(t)
	row := pfclaimRow()
	row.AttachSeq = 7
	cancel := a.Tunnels.register(row.ID)
	defer a.Tunnels.unregister(row.ID, cancel)
	if ended := a.portForwardHeartbeat(row)(context.Background()); ended != "" {
		t.Fatalf("a healthy beat ended the session as %q", ended)
	}
	call := db.only(t, "HeartbeatPortForwardSession")
	if len(call.args) != 2 || call.args[1] != int64(7) {
		t.Fatalf("heartbeat args = %v, want the attach generation", call.args)
	}
}

// Idempotence in the claim is worthless if the abandoned attach kills the row on
// its way out — which is exactly what the reported failure did. A client that
// went away before anything was served leaves the row claimed, un-ended and
// re-claimable for the remainder of its TTL. Nobody is told anything, because
// nobody is listening.
func TestPfclaimAbandonedAttachLeavesTheRowReclaimable(t *testing.T) {
	t.Run("the head never reached the client", func(t *testing.T) {
		a, db := pfclaimAPI(t)
		pfclaimClaims(db, pfclaimRow())
		writer := pfclaimNewWriter()
		writer.flushErr = io.ErrClosedPipe
		key, _ := freshAttachKey(t)
		session := pfclaimOpen(t, a, key, writer)
		<-session.done

		if calls := db.seen("EndPortForwardSession"); len(calls) != 0 {
			t.Fatalf("the abandoned attach finalized the row (%d calls) — the next rung gains nothing", len(calls))
		}
	})

	t.Run("the client vanished after the head", func(t *testing.T) {
		a, db := pfclaimAPI(t)
		pfclaimClaims(db, pfclaimRow())
		// Still resolving: this is the window ADR-066 opens on purpose — a
		// session that is open and has dialled nothing — and precisely the state
		// that must survive the CLI stepping down a rung.
		release := make(chan struct{})
		defer close(release)
		pfclaimBlockingAgent(t, a, release)
		key, _ := freshAttachKey(t)
		session := pfclaimOpen(t, a, key, pfclaimNewWriter())
		session.awaitHead(t)
		session.cancel()
		select {
		case <-session.done:
		case <-time.After(5 * time.Second):
			t.Fatal("the session request never returned")
		}
		if calls := db.seen("EndPortForwardSession"); len(calls) != 0 {
			t.Fatalf("a cancelled request finalized the row (%d calls)", len(calls))
		}
	})

	// Past the TTL nothing can re-claim it, so leaving it open buys nothing and
	// the row is finalized as the disconnect it was.
	t.Run("an expired token has nothing left to rescue", func(t *testing.T) {
		a, db := pfclaimAPI(t)
		row := pfclaimRow()
		row.TokenExpiresAt = pgtype.Timestamptz{Time: time.Now().Add(-time.Second), Valid: true}
		pfclaimClaims(db, row)
		writer := pfclaimNewWriter()
		writer.flushErr = io.ErrClosedPipe
		key, _ := freshAttachKey(t)
		session := pfclaimOpen(t, a, key, writer)
		<-session.done

		if reason, _ := pfclaimEndReason(t, db); reason != store.TerminalEndReason(tunnel.EndDisconnect) {
			t.Fatalf("end_reason = %q, want disconnect", reason)
		}
	})
}

// A claim that matches zero rows is the replay this whole rule exists to stop —
// a different attacher, an expired token, an ended session — and it must still
// answer 401 without touching the row.
func TestPfclaimRefusedClaimTouchesNothing(t *testing.T) {
	a, db := pfclaimAPI(t)
	db.rule(netcovRule{match: "-- name: ClaimPortForwardSession ", noRows: true})
	key, _ := freshAttachKey(t)
	writer := pfclaimNewWriter()
	session := pfclaimOpen(t, a, key, writer)
	<-session.done

	if writer.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", writer.Code)
	}
	if calls := db.seen("EndPortForwardSession"); len(calls) != 0 {
		t.Fatalf("a refused claim finalized somebody's row (%d calls)", len(calls))
	}
}
