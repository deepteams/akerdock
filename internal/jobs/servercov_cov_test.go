package jobs

// Shared steering infrastructure for the servercov coverage suite: a DBTX
// wrapper that lets one test override individual query results on top of the
// jobFlowDB defaults, and a scripted loopback SSH server whose per-command
// outputs (stdout, stderr, exit code) are chosen by the test.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/ssh"

	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/store"
	"log/slog"
)

// servercovName extracts the sqlc query name from a SQL text.
func servercovName(sql string) string {
	_, rest, ok := strings.Cut(sql, "-- name: ")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(rest, " ")
	return name
}

// servercovDB steers individual queries by name while everything else keeps
// the jobFlowDB default behavior (fixture fills, UPDATE 1 tags).
type servercovDB struct {
	inner *jobFlowDB

	mu       sync.Mutex
	execErr  map[string]error
	rowErr   map[string]error
	rowAfter map[string]func([]any)
	queryFor map[string][][]func([]any) // per name: successive Query batches
	queryErr map[string]error
}

func (d *servercovDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if err := d.execErr[servercovName(sql)]; err != nil {
		return pgconn.CommandTag{}, err
	}
	return d.inner.Exec(ctx, sql, args...)
}

func (d *servercovDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	name := servercovName(sql)
	if err := d.queryErr[name]; err != nil {
		return nil, err
	}
	d.mu.Lock()
	batches, ok := d.queryFor[name]
	if ok {
		var fills []func([]any)
		if len(batches) > 0 {
			fills = batches[0]
			d.queryFor[name] = batches[1:]
		}
		d.mu.Unlock()
		return &servercovRows{fills: fills}, nil
	}
	d.mu.Unlock()
	return d.inner.Query(ctx, sql, args...)
}

func (d *servercovDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	return servercovRow{d: d, sql: sql}
}

type servercovRow struct {
	d   *servercovDB
	sql string
}

func (r servercovRow) Scan(dest ...any) error {
	name := servercovName(r.sql)
	if err := r.d.rowErr[name]; err != nil {
		return err
	}
	if err := r.d.inner.QueryRow(context.Background(), r.sql).Scan(dest...); err != nil {
		return err
	}
	if fn := r.d.rowAfter[name]; fn != nil {
		fn(dest)
	}
	return nil
}

// servercovRows plays scripted result rows: each fill fills one row's dests.
type servercovRows struct {
	fills   []func([]any)
	idx     int
	current bool
}

func (r *servercovRows) Close()                                     {}
func (*servercovRows) Err() error                                   { return nil }
func (*servercovRows) CommandTag() pgconn.CommandTag                { return pgconn.NewCommandTag("SELECT 1") }
func (*servercovRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (*servercovRows) Values() ([]any, error)                       { return nil, nil }
func (*servercovRows) RawValues() [][]byte                          { return nil }
func (*servercovRows) Conn() *pgx.Conn                              { return nil }

func (r *servercovRows) Next() bool {
	if r.idx >= len(r.fills) {
		r.current = false
		return false
	}
	r.idx++
	r.current = true
	return true
}

func (r *servercovRows) Scan(dest ...any) error {
	if !r.current {
		return pgx.ErrNoRows
	}
	r.fills[r.idx-1](dest)
	return nil
}

var _ store.DBTX = (*servercovDB)(nil)
var _ pgx.Rows = (*servercovRows)(nil)

// servercovFill builds a row-fill: every dest gets the jobFlow default,
// except the listed indices which are overridden.
func servercovFill(overrides map[int]func(any)) func([]any) {
	return func(dest []any) {
		for i, d := range dest {
			if o, ok := overrides[i]; ok {
				o(d)
				continue
			}
			_ = fillJobDestination(d, nil, false)
		}
	}
}

func servercovBytes(b []byte) func(any) {
	return func(d any) { *(d.(*[]byte)) = append([]byte(nil), b...) }
}

func servercovPtr[T any](v T) func(any) {
	return func(d any) { *(d.(**T)) = &v }
}

func servercovNilPtr[T any]() func(any) {
	return func(d any) { *(d.(**T)) = nil }
}

func servercovVal[T any](v T) func(any) {
	return func(d any) { *(d.(*T)) = v }
}

// servercovOverride is servercovFill for QueryRow post-processing: only the
// listed indices are touched, everything else keeps the inner fill.
func servercovOverride(overrides map[int]func(any)) func([]any) {
	return func(dest []any) {
		for i, o := range overrides {
			if i < len(dest) {
				o(dest[i])
			}
		}
	}
}

// servercovDeps is jobFlowDependencies wrapped in a steering servercovDB.
func servercovDeps(t *testing.T) (*store.Queries, *envelope.Keyring, *audit.Recorder, *slog.Logger, *servercovDB) {
	t.Helper()
	_, keyring, _, logger, inner := jobFlowDependencies(t)
	db := &servercovDB{
		inner:    inner,
		execErr:  map[string]error{},
		rowErr:   map[string]error{},
		rowAfter: map[string]func([]any){},
		queryFor: map[string][][]func([]any){},
		queryErr: map[string]error{},
	}
	q := store.New(db)
	rec := &audit.Recorder{Store: q, Logger: logger}
	return q, keyring, rec, logger, db
}

// servercovSSHResponse is one scripted answer of the SSH server.
type servercovSSHResponse struct {
	stdout string
	stderr string
	exit   uint32
}

// servercovSSHServer is a scripted loopback SSH server: the test decides,
// per command, what stdout/stderr/exit code come back. Commands without a
// script fall back to jobCommandOutput with exit 0.
type servercovSSHServer struct {
	listener net.Listener
	config   *ssh.ServerConfig
	script   func(cmd string) (servercovSSHResponse, bool)
	mu       sync.Mutex
	conns    []net.Conn
}

func servercovNewSSHServer(t *testing.T, script func(cmd string) (servercovSSHResponse, bool)) *servercovSSHServer {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return nil, nil
		},
	}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &servercovSSHServer{listener: listener, config: config, script: script}
	go server.accept()
	t.Cleanup(server.close)
	return server
}

func (s *servercovSSHServer) accept() {
	for {
		raw, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, raw)
		s.mu.Unlock()
		go s.serve(raw)
	}
}

func (s *servercovSSHServer) serve(raw net.Conn) {
	connection, channels, requests, err := ssh.NewServerConn(raw, s.config)
	if err != nil {
		_ = raw.Close()
		return
	}
	go ssh.DiscardRequests(requests)
	for incoming := range channels {
		go s.handleChannel(incoming)
	}
	_ = connection.Close()
}

func (s *servercovSSHServer) handleChannel(incoming ssh.NewChannel) {
	if incoming.ChannelType() != "session" {
		_ = incoming.Reject(ssh.UnknownChannelType, "session only")
		return
	}
	channel, requests, err := incoming.Accept()
	if err != nil {
		return
	}
	for request := range requests {
		if request.Type != "exec" {
			_ = request.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		_ = ssh.Unmarshal(request.Payload, &payload)
		_ = request.Reply(true, nil)
		_, _ = io.Copy(io.Discard, channel)
		resp := servercovSSHResponse{stdout: jobCommandOutput(payload.Command)}
		if s.script != nil {
			if scripted, ok := s.script(payload.Command); ok {
				resp = scripted
			}
		}
		if resp.stdout != "" {
			_, _ = channel.Write([]byte(resp.stdout))
		}
		if resp.stderr != "" {
			_, _ = channel.Stderr().Write([]byte(resp.stderr))
		}
		_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{resp.exit}))
		_ = channel.Close()
		return
	}
}

func (s *servercovSSHServer) address(t *testing.T) (string, int) {
	t.Helper()
	host, rawPort, err := net.SplitHostPort(s.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}

func (s *servercovSSHServer) close() {
	_ = s.listener.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, connection := range s.conns {
		_ = connection.Close()
	}
}

// servercovScript builds a substring-matching script from ordered rules:
// the first rule whose substring is contained in the command answers it.
type servercovRule struct {
	contains string
	resp     servercovSSHResponse
}

func servercovScript(rules ...servercovRule) func(string) (servercovSSHResponse, bool) {
	return func(cmd string) (servercovSSHResponse, bool) {
		for _, r := range rules {
			if strings.Contains(cmd, r.contains) {
				return r.resp, true
			}
		}
		return servercovSSHResponse{}, false
	}
}

// servercovClosedPort returns a loopback port with nothing listening on it.
func servercovClosedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// servercovEncrypt is keyring.Encrypt bound to the fixture row uuid.
func servercovEncrypt(t *testing.T, keyring *envelope.Keyring, table, column, plaintext string) []byte {
	t.Helper()
	blob, err := keyring.Encrypt(table, column, jobFixtureUUID, []byte(plaintext))
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

// servercovTimeout is a per-test safety net used by polling helpers.
const servercovTimeout = 30 * time.Second

// servercovFixtureKeyring re-derives the deterministic keyring used by
// jobFlowDependencies (static all-zero master key, version 1): ciphertexts
// produced with it decrypt under any other parse of the same material.
func servercovFixtureKeyring(t *testing.T) *envelope.Keyring {
	t.Helper()
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	keyring, err := envelope.Parse([]byte("1:" + key + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return keyring
}
