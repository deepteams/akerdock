// ADR-065 and ADR-066 on the terminal attach path, which land together and are
// tested together because they are the same failure taken from two ends: the
// attach answers on local state alone and resolves the shell behind the
// response head (ADR-066), and the token it spent stays re-claimable by the
// same attacher for the rest of its TTL (ADR-065). The reported symptom was one
// message — "invalid, expired or already used terminal token" — for a client
// whose own first attempt burnt the token while the control plane was still
// dialling.
//
// Every top-level identifier is prefixed termclaim (concurrent-agent rule).
package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/terminal"
	"github.com/deepteams/akerdock/internal/tunnel"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// termclaimWriter is a ResponseWriter that says WHEN the response head was
// committed, and is safe to read while the handler is still writing to it. Both
// properties are the point: httptest.ResponseRecorder answers neither, and the
// ordering between the head and the remote half is the whole of ADR-066.
type termclaimWriter struct {
	headers http.Header

	mu   sync.Mutex
	body bytes.Buffer
	code int

	flushed   chan struct{}
	flushOnce sync.Once
}

func termclaimNewWriter() *termclaimWriter {
	return &termclaimWriter{headers: http.Header{}, flushed: make(chan struct{})}
}

func (w *termclaimWriter) Header() http.Header { return w.headers }

func (w *termclaimWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.body.Write(p)
}

func (w *termclaimWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.code == 0 {
		w.code = code
	}
}

// Flush is what http.ResponseController reaches for, and closing the channel
// here is the observation the ordering assertions are built on.
func (w *termclaimWriter) Flush() { w.flushOnce.Do(func() { close(w.flushed) }) }

// EnableFullDuplex is what a real net/http server offers on HTTP/1.1, which is
// how Traefik reaches the control plane — and without it every attach here
// would be refused at the door instead of exercising anything.
func (w *termclaimWriter) EnableFullDuplex() error { return nil }

func (w *termclaimWriter) status() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.code
}

// wire is everything the client has been sent so far — for a session request,
// the control frames.
func (w *termclaimWriter) wire() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String()
}

// termclaimDB records the statements the attach issues. The durable half of
// both decisions — which key the claim bound, which generation guarded the
// close, whether the row was finalized at all — is observable nowhere else.
type termclaimDB struct {
	*netcovDB

	mu    sync.Mutex
	calls []termclaimCall
}

type termclaimCall struct {
	sql  string
	args []any
}

func (db *termclaimDB) record(sql string, args []any) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.calls = append(db.calls, termclaimCall{sql: sql, args: args})
}

func (db *termclaimDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	db.record(sql, args)
	return db.netcovDB.Exec(ctx, sql, args...)
}

func (db *termclaimDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	db.record(sql, args)
	return db.netcovDB.QueryRow(ctx, sql, args...)
}

func (db *termclaimDB) calledWith(match string) (termclaimCall, bool) {
	db.mu.Lock()
	defer db.mu.Unlock()
	for _, call := range db.calls {
		if strings.Contains(call.sql, match) {
			return call, true
		}
	}
	return termclaimCall{}, false
}

func (db *termclaimDB) count(match string) int {
	db.mu.Lock()
	defer db.mu.Unlock()
	n := 0
	for _, call := range db.calls {
		if strings.Contains(call.sql, match) {
			n++
		}
	}
	return n
}

func termclaimAPI(t *testing.T) (*API, *termclaimDB) {
	t.Helper()
	a, base := netcovAPI(t)
	db := &termclaimDB{netcovDB: base}
	a.Store = store.New(db)
	a.Audit = &audit.Recorder{Store: a.Store, Logger: a.Logger}
	return a, db
}

// termclaimRequest is one half of an HTTP attach, driven directly against the
// handler: the session request or its one data stream.
type termclaimRequest struct {
	writer *termclaimWriter
	// body is the client's end of the request stream: keystrokes on a data
	// stream, control frames on a session request. Exposed because proving a
	// bridge is actually pumping needs a byte to go in and come back — no other
	// observable distinguishes a live shell from one about to start.
	body   *io.PipeWriter
	cancel context.CancelFunc
	done   chan struct{}
}

func termclaimOpen(t *testing.T, a *API, contentType, target string, headers map[string]string) *termclaimRequest {
	t.Helper()
	bodyReader, bodyWriter := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, target, bodyReader).WithContext(ctx)
	request.Header.Set("Content-Type", contentType)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	open := &termclaimRequest{
		writer: termclaimNewWriter(), body: bodyWriter, cancel: cancel, done: make(chan struct{}),
	}
	go func() {
		defer close(open.done)
		a.TerminalAttach(open.writer, request)
	}()
	t.Cleanup(func() {
		cancel()
		_ = bodyWriter.Close()
		_ = bodyReader.Close()
	})
	return open
}

func termclaimOpenSession(t *testing.T, a *API, key, query string) *termclaimRequest {
	t.Helper()
	return termclaimOpen(t, a, tunnel.TerminalHTTP.ControlContentType, terminalAttachPath+query, map[string]string{
		tunnel.TerminalHTTP.ProtocolHeader:  tunnel.TerminalHTTP.Name,
		tunnel.TerminalHTTP.AttachKeyHeader: key,
	})
}

func termclaimOpenStream(t *testing.T, a *API, key, sessionUUID string) *termclaimRequest {
	t.Helper()
	return termclaimOpen(t, a, tunnel.TerminalHTTP.StreamContentType, terminalAttachPath, map[string]string{
		tunnel.TerminalHTTP.SessionHeader:   sessionUUID,
		tunnel.TerminalHTTP.AttachKeyHeader: key,
	})
}

func (o *termclaimRequest) awaitHead(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case <-o.writer.flushed:
	case <-o.done:
		t.Fatalf("the request ended with %d instead of committing a head: %s", o.writer.status(), o.writer.wire())
	case <-time.After(within):
		t.Fatal("no response head — the attach is waiting on something it must not wait on")
	}
}

func (o *termclaimRequest) awaitDone(t *testing.T) {
	t.Helper()
	select {
	case <-o.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the request never returned")
	}
}

func (o *termclaimRequest) headWasCommitted() bool {
	select {
	case <-o.writer.flushed:
		return true
	default:
		return false
	}
}

// termclaimEndReason reads the enum an EndTerminalSession call persisted.
func termclaimEndReason(t *testing.T, call termclaimCall) store.TerminalEndReason {
	t.Helper()
	reason, ok := call.args[1].(*store.TerminalEndReason)
	if !ok || reason == nil {
		t.Fatalf("the close carried no end reason: %#v", call.args)
	}
	return *reason
}

// ---------------------------------------------------------------------------
// ADR-066 — the attach answers before it dials
// ---------------------------------------------------------------------------

// The defect, from both ends at once. The response head must be produced from
// local state alone, and the reachability failure that used to be a 409 at
// redeem must reach the developer on the session that is already open —
// carrying the operator's sentence, and persisting a reason that is true.
//
// The container shell is deliberately the subject: its remote half is two typed
// commands on the agent channel and it opens no SSH connection at all, so a
// harness with no SSH server anywhere still exercises the whole path.
func TestTermclaimSessionAnswersBeforeItDialsAndReportsAnUnreachableTarget(t *testing.T) {
	a, db := termclaimAPI(t)
	netcovTerminalClaim(db.netcovDB, store.TerminalSession{
		ID: 7, TeamID: 1, AttachSeq: 1,
		TargetKind: store.TerminalTargetContainer, ServerID: ptr(int64(1)), ResourceID: ptr(int64(1)),
	})

	// The resolver refuses to answer until the head has been observed. An
	// attach that still resolved first would deadlock against this and the head
	// assertion below fires long before the agent gives up.
	release := make(chan struct{})
	netcovAgent(t, a, func(cmd agentwire.Command) (*agentwire.Result, bool) {
		if cmd.Method != agentwire.MethodContainerExecCreate {
			return &agentwire.Result{Err: &agentwire.Error{Code: agentwire.CodeInvalid, Message: "unexpected"}}, false
		}
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		return &agentwire.Result{Err: &agentwire.Error{Code: agentwire.CodeNotFound, Message: "no such container"}}, false
	})

	key, keyHash := freshAttachKey(t)
	session := termclaimOpenSession(t, a, key, "?token=tk&cols=100&rows=30")
	session.awaitHead(t, 2*time.Second)
	if got := session.writer.status(); got != http.StatusOK {
		t.Fatalf("status = %d, want %d — the head is produced from local state", got, http.StatusOK)
	}
	close(release)

	// The claim is bound to the attacher, not to the request: that hash is what
	// lets the next rung present the same token and be recognised rather than
	// accused of a replay.
	claim, ok := db.calledWith("-- name: ClaimTerminalSession ")
	if !ok {
		t.Fatal("the session never claimed its token")
	}
	stored, isBytes := claim.args[1].([]byte)
	if !isBytes || !bytes.Equal(stored, keyHash[:]) {
		t.Fatalf("claim bound %x, want the attach key hash %x", stored, keyHash[:])
	}

	stream := termclaimOpenStream(t, a, key, session.writer.Header().Get(tunnel.TerminalHTTP.SessionHeader))
	stream.awaitHead(t, 2*time.Second)
	session.awaitDone(t)

	wire := session.writer.wire()
	if !strings.Contains(wire, `"reason":"target_unreachable"`) {
		t.Fatalf("session wire = %s — the failure must be reported on the session", wire)
	}
	if !strings.Contains(wire, "the container does not exist on the server") {
		t.Fatalf("session wire = %s — the reason alone is not a report", wire)
	}

	end, ok := db.calledWith("-- name: EndTerminalSession ")
	if !ok {
		t.Fatal("a refusal on the merits must finalize the row")
	}
	switch got := termclaimEndReason(t, end); got {
	case store.TerminalEndReasonTargetUnreachable:
	case store.TerminalEndReasonRevoked:
		t.Fatal("the row records a revocation nobody performed")
	default:
		t.Fatalf("end reason = %q, want %q", got, store.TerminalEndReasonTargetUnreachable)
	}

	// The stream joined, so the row carries the trace the sweep needs — written
	// once, because a terminal has exactly one data stream.
	if got := db.count("-- name: MarkTerminalSessionStreamed "); got != 1 {
		t.Fatalf("streamed_at stamped %d times, want exactly 1", got)
	}
}

// The local half keeps its 409 and its prose (ADR-066 §1): these are indexed
// reads whose refusals are actionable, and none of them is a bet on another
// machine. The row is finalized — a re-claim would only reproduce the verdict.
func TestTermclaimLocalRefusalsStayInFrontOfTheHead(t *testing.T) {
	for name, tc := range map[string]struct {
		row   store.TerminalSession
		steer func(db *termclaimDB)
		want  string
	}{
		"server gone": {
			row:  store.TerminalSession{ID: 7, TeamID: 1, AttachSeq: 1, TargetKind: store.TerminalTargetServer},
			want: "the target server no longer exists",
		},
		"resource gone": {
			row: store.TerminalSession{
				ID: 7, TeamID: 1, AttachSeq: 1,
				TargetKind: store.TerminalTargetContainer, ServerID: ptr(int64(1)), ResourceID: ptr(int64(1)),
			},
			steer: func(db *termclaimDB) {
				db.rule(netcovRule{match: "-- name: GetResourceByID ", err: errors.New("gone")})
			},
			want: "the target resource no longer exists",
		},
	} {
		t.Run(name, func(t *testing.T) {
			a, db := termclaimAPI(t)
			netcovTerminalClaim(db.netcovDB, tc.row)
			if tc.steer != nil {
				tc.steer(db)
			}
			key, _ := freshAttachKey(t)
			session := termclaimOpenSession(t, a, key, "?token=tk")
			session.awaitDone(t)

			if session.headWasCommitted() {
				t.Fatal("a local refusal committed a response head — it must answer 409 instead")
			}
			if got := session.writer.status(); got != http.StatusConflict {
				t.Fatalf("status = %d, want %d", got, http.StatusConflict)
			}
			if !strings.Contains(session.writer.wire(), tc.want) {
				t.Fatalf("body = %s, want %q", session.writer.wire(), tc.want)
			}
			end, ok := db.calledWith("-- name: EndTerminalSession ")
			if !ok {
				t.Fatal("a refusal on the merits must finalize the row")
			}
			if got := termclaimEndReason(t, end); got != store.TerminalEndReasonTargetUnreachable {
				t.Fatalf("end reason = %q, want %q", got, store.TerminalEndReasonTargetUnreachable)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ADR-066 §2 — the promise's lifetime
// ---------------------------------------------------------------------------

// termclaimPTY counts its own teardown, which is the only way to see the leak
// this promise exists to prevent.
type termclaimPTY struct{ closes atomic.Int32 }

func (p *termclaimPTY) Read([]byte) (int, error)    { return 0, io.EOF }
func (p *termclaimPTY) Write(b []byte) (int, error) { return len(b), nil }
func (p *termclaimPTY) Close() error                { p.closes.Add(1); return nil }
func (p *termclaimPTY) Resize(_ int, _ int) error   { return nil }

// The accepted risk of ADR-066, guarded by a test rather than by the shape of
// the code: a session abandoned during a 30 s handshake must not leak an SSH
// client — or, on a container shell, a live shell and an exec instance on
// someone's container. Whoever arrives last releases.
func TestTermclaimPromiseReleasesAResolutionThatArrivesLate(t *testing.T) {
	resolved := make(chan struct{})
	pty := &termclaimPTY{}
	var cleanups atomic.Int32

	promise := startTerminalPTY(context.Background(), func(context.Context) (terminal.PTY, func(), string) {
		<-resolved
		return pty, func() { cleanups.Add(1) }, ""
	})
	promise.release() // the session tore down while the handshake was in flight
	close(resolved)

	select {
	case <-promise.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the resolution never settled")
	}
	if pty.closes.Load() != 1 || cleanups.Load() != 1 {
		t.Fatalf("pty closes = %d, cleanups = %d — a late resolution owns what it produced",
			pty.closes.Load(), cleanups.Load())
	}
}

// Taking the PTY takes responsibility for closing it: the bridge does that at
// teardown. The promise keeps the transport cleanup, so the SSH client the
// shell rides on is released whichever way the session ends — and the PTY is
// never closed twice.
func TestTermclaimPromiseHandsThePTYToItsAwaiterAndKeepsTheTransport(t *testing.T) {
	pty := &termclaimPTY{}
	var cleanups atomic.Int32
	promise := startTerminalPTY(context.Background(), func(context.Context) (terminal.PTY, func(), string) {
		return pty, func() { cleanups.Add(1) }, ""
	})

	got, errMsg, ok := promise.await(context.Background())
	if !ok || errMsg != "" || got != terminal.PTY(pty) {
		t.Fatalf("await = %v, %q, %v", got, errMsg, ok)
	}
	promise.release()
	if pty.closes.Load() != 0 {
		t.Fatal("the promise closed a PTY it had handed away — the bridge owns it from here")
	}
	if cleanups.Load() != 1 {
		t.Fatalf("cleanups = %d, want 1 — the transport is released whatever happens", cleanups.Load())
	}
}

// A session that ends before its target answers reports nothing and finalizes
// nothing: that is an abandonment (ADR-065 §6), not a refusal on the merits.
func TestTermclaimPromiseAwaitGivesUpWithItsSession(t *testing.T) {
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	promise := startTerminalPTY(context.Background(), func(context.Context) (terminal.PTY, func(), string) {
		<-blocked
		return nil, nil, "too late"
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pty, errMsg, ok := promise.await(ctx)
	if ok || pty != nil || errMsg != "" {
		t.Fatalf("await = %v, %q, %v — a session that ended first has nobody to tell", pty, errMsg, ok)
	}
}

// The resolution runs under the session's context, so a session that ends
// mid-dial cancels the dial rather than leaving it to finish into nothing.
func TestTermclaimPromiseCancelsTheDialWithItsSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancelled := make(chan error, 1)
	startTerminalPTY(ctx, func(ctx context.Context) (terminal.PTY, func(), string) {
		<-ctx.Done()
		cancelled <- ctx.Err()
		return nil, nil, "the server is not reachable over SSH right now"
	})

	cancel()
	select {
	case err := <-cancelled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("dial saw %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the dial outlived the session that owned it")
	}
}

// ---------------------------------------------------------------------------
// ADR-065 — the claim is idempotent, and the loser of a re-claim is cut
// ---------------------------------------------------------------------------

// A client that gave up on a rung and came back on the next one must find its
// session, not a finalized row. Nothing was served, so nothing is reported and
// nothing is closed: the row stays claimed and re-claimable for the rest of its
// TTL, which is the whole of what makes the ladder's step-down survivable.
func TestTermclaimAbandonedAttachLeavesTheRowReclaimable(t *testing.T) {
	a, db := termclaimAPI(t)
	netcovTerminalClaim(db.netcovDB, store.TerminalSession{
		ID: 7, TeamID: 1, AttachSeq: 1,
		TargetKind: store.TerminalTargetServer, ServerID: ptr(int64(1)),
	})

	key, _ := freshAttachKey(t)
	session := termclaimOpenSession(t, a, key, "?token=tk")
	session.awaitHead(t, 2*time.Second)
	// The client dies between the two requests — the awaitTerminalStream path,
	// reached here by its own request ending rather than by waiting out the
	// fifteen-second bound.
	session.cancel()
	session.awaitDone(t)

	if _, ok := db.calledWith("-- name: EndTerminalSession "); ok {
		t.Fatal("an abandoned attach finalized its row — every remaining rung would then be refused")
	}
	if _, ok := db.calledWith("-- name: MarkTerminalSessionStreamed "); ok {
		t.Fatal("streamed_at was stamped for a stream that never joined — the sweep would then never reclaim the slot")
	}
}

// A successful re-claim means the previous attach lost, and a loser must be cut
// rather than left running beside its successor: two live attaches on one
// session is two PTYs on someone's container.
func TestTermclaimRegisterCutsTheAttachItDisplaces(t *testing.T) {
	api := &API{}
	_, keyHash := freshAttachKey(t)

	cut := make(chan struct{})
	first := newTerminalAttach(keyHash, func() { close(cut) })
	api.terminalRegister("session-1", first)

	second := newTerminalAttach(keyHash, nil)
	api.terminalRegister("session-1", second)

	select {
	case <-first.done:
	default:
		t.Fatal("the displaced attach still parks its data-stream handler on done")
	}
	select {
	case <-cut:
	default:
		t.Fatal("the displaced session was never cancelled — its PTY outlives the re-claim")
	}
	if got := api.terminalLookup("session-1", keyHash); got != second {
		t.Fatal("the winner must own the session")
	}
	// The loser's own teardown must not evict the winner on its way out.
	api.terminalRelease("session-1", first)
	if got := api.terminalLookup("session-1", keyHash); got != second {
		t.Fatal("a superseded attach deleted its successor's entry")
	}
}

// The WebSocket rung wins re-claims too (ADR-065 §7) and must cut what it
// displaced — and since ADR-067 §2 it takes the displaced attach's PLACE rather
// than merely emptying it: a rung that registers nothing is a shell no cut can
// reach, neither this one nor the beat's when the container is gone.
func TestTermclaimWebSocketRungRegistersWhatItSupersedes(t *testing.T) {
	api := &API{}
	_, keyHash := freshAttachKey(t)
	cut := make(chan struct{})
	attach := newTerminalAttach(keyHash, func() { close(cut) })
	api.terminalRegister("session-1", attach)

	ws := newTerminalWebSocketAttach(keyHash, nil)
	api.terminalRegister("session-1", ws)
	select {
	case <-cut:
	default:
		t.Fatal("the HTTP attach survived a WebSocket re-claim")
	}
	if got := api.terminalLookup("session-1", keyHash); got != ws {
		t.Fatal("the WebSocket rung left the session unreachable by any cut")
	}
	// It holds a session, not a data stream: the WebSocket carries its own
	// bytes, so a data request presenting the key gets the ordinary refusal
	// instead of parking a connection nobody will read.
	if ws.claimed.CompareAndSwap(false, true) {
		t.Fatal("a data stream could still join a WebSocket-attached session")
	}
	// Idempotent: cutting a session nobody holds is not an error.
	api.terminalRelease("session-1", ws)
	if api.terminalCut("session-1", terminalEndReasonSuperseded) {
		t.Fatal("a session nobody holds reported a live attach")
	}
}

// The generation guard, from both sides: the attach that still holds the
// session finalizes it and its close is audited; the one a re-claim displaced
// matches zero rows and audits nothing, because no session closed.
func TestTermclaimCloseIsGuardedByTheAttachGeneration(t *testing.T) {
	row := store.TerminalSession{ID: 7, TeamID: 1, AttachSeq: 3}
	_ = row.Uuid.Scan(fixtureUUID)

	winner, winnerDB := termclaimAPI(t)
	winner.endTerminalSession(row, terminal.EndUserClose)
	end, ok := winnerDB.calledWith("-- name: EndTerminalSession ")
	if !ok {
		t.Fatal("the close never reached the row")
	}
	seq, isSeq := end.args[2].(*int64)
	if !isSeq || seq == nil || *seq != 3 {
		t.Fatalf("close guarded by %#v, want the attach generation 3", end.args[2])
	}
	if _, ok := winnerDB.calledWith("-- name: InsertAuditEvent "); !ok {
		t.Fatal("a session that really closed must be audited")
	}

	loser, loserDB := termclaimAPI(t)
	loserDB.rule(netcovRule{match: "-- name: EndTerminalSession ", tag: "UPDATE 0"})
	loser.endTerminalSession(row, terminal.EndRevoked)
	if _, ok := loserDB.calledWith("-- name: InsertAuditEvent "); ok {
		t.Fatal("a superseded attach audited a close it did not perform")
	}
}

// The attacher identity the claim binds to (ADR-065 §3 and §7). A presented key
// is stored as its hash; an attach that presents none — an N-1 CLI mid-rollout,
// or the dashboard's browser terminal — stores something no presentable key can
// ever match, which is what keeps it strictly single-use instead of freely
// re-claimable for sixty seconds by whoever holds the token.
func TestTermclaimAttachKeyBindsTheClaimToItsAttacher(t *testing.T) {
	key, keyHash := freshAttachKey(t)
	presented := httptest.NewRequest(http.MethodGet, terminalLegacyWebsocketPath+"?token=x", nil)
	presented.Header.Set(tunnel.TerminalHTTP.AttachKeyHeader, key)
	got, err := terminalClaimKey(presented)
	if err != nil || got != keyHash {
		t.Fatalf("claim key = %x, %v — a presented key is stored as its hash", got, err)
	}

	bare := httptest.NewRequest(http.MethodGet, terminalLegacyWebsocketPath+"?token=x", nil)
	first, err := terminalClaimKey(bare)
	if err != nil {
		t.Fatal(err)
	}
	second, err := terminalClaimKey(bare)
	if err != nil {
		t.Fatal(err)
	}
	// The width is the type's since ADR-067 §2 — the register compares keys in
	// constant time on a fixed size — so what is left to assert is the value.
	if first == second {
		t.Fatal("two keyless attaches stored the same value — the second would re-claim the first's session")
	}
	if first == keyHash || first == ([sha256.Size]byte{}) {
		t.Fatal("the keyless value must match nothing presentable, and never be a zero sentinel")
	}

	malformed := httptest.NewRequest(http.MethodGet, terminalLegacyWebsocketPath+"?token=x", nil)
	malformed.Header.Set(tunnel.TerminalHTTP.AttachKeyHeader, "not-a-key")
	if _, err := terminalClaimKey(malformed); err == nil {
		t.Fatal("a malformed key must be refused, not quietly replaced by a random one")
	}
}

// The bottom rung participates in the claim and keeps its choreography: it
// still resolves before websocket.Accept and still answers 409 with the
// operator sentence (ADR-066 §5 as a decision, not an omission), and it carries
// the same per-mint attach key every other rung presents (ADR-065 §7).
func TestTermclaimWebSocketRungCarriesTheKeyAndStillRefusesBeforeTheUpgrade(t *testing.T) {
	a, db := termclaimAPI(t)
	netcovTerminalClaim(db.netcovDB, store.TerminalSession{
		ID: 7, TeamID: 1, AttachSeq: 1, TargetKind: store.TerminalTargetServer,
	})

	key, keyHash := freshAttachKey(t)
	request := httptest.NewRequest(http.MethodGet, terminalLegacyWebsocketPath+"?token=x", nil)
	request.Header.Set(tunnel.TerminalHTTP.AttachKeyHeader, key)
	recorder := httptest.NewRecorder()
	a.TerminalWebSocket(recorder, request)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d — the WebSocket rung answers its refusals inline", recorder.Code, http.StatusConflict)
	}
	if !strings.Contains(recorder.Body.String(), "the target server no longer exists") {
		t.Fatalf("body = %s", recorder.Body.String())
	}
	claim, ok := db.calledWith("-- name: ClaimTerminalSession ")
	if !ok {
		t.Fatal("the WebSocket rung never claimed")
	}
	stored, isBytes := claim.args[1].([]byte)
	if !isBytes || !bytes.Equal(stored, keyHash[:]) {
		t.Fatalf("claim bound %x, want %x — the bottom rung must be recognised as the same attacher", stored, keyHash[:])
	}
	end, ok := db.calledWith("-- name: EndTerminalSession ")
	if !ok {
		t.Fatal("a refusal on the merits must finalize the row")
	}
	if got := termclaimEndReason(t, end); got != store.TerminalEndReasonTargetUnreachable {
		t.Fatalf("end reason = %q, want %q", got, store.TerminalEndReasonTargetUnreachable)
	}
}
