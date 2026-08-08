package agent

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"

	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
)

// freeLoopbackAddr reserves an ephemeral loopback port and frees it for the
// server under test — the usual small race, acceptable in a unit test.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// waitServing polls the address until the waker answers, whatever the status.
func waitServing(t *testing.T, addr string) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waker on %s never started serving", addr)
}

// serveGet performs one request against the waker with an explicit Host.
func serveGet(t *testing.T, addr, host string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestServeUnconfiguredAnswers503 runs the real Serve loop against a corrupt
// routing file: the load failure is tolerated (the control plane will deposit
// a good one), every request answers 503, and cancellation shuts the server
// down cleanly.
func TestServeUnconfiguredAnswers503(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, RoutesFile), []byte(`{corrupt`), 0o644); err != nil {
		t.Fatal(err)
	}
	addr := freeLoopbackAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, dir, addr, &fake.Runtime{}, Enrollment{}, nil) }()

	waitServing(t, addr)
	resp := serveGet(t, addr, "anything.example.com")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured waker = %d, want 503", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v after shutdown, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve never returned after cancellation")
	}
}

// TestServeRoutesWakerAndIngress runs Serve with a deposited routing table
// and an enrolled agent: a scale-to-zero host forwards to its (running)
// backend, an ingress host serves the offline page, an unknown host is 404 —
// the three arms of the front handler.
func TestServeRoutesWakerAndIngress(t *testing.T) {
	// The application backend the waker forwards to.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer backend.Close()
	bu, _ := url.Parse(backend.URL)

	// The control plane the agent pushes to: no WS endpoint (the dial fails and
	// the POST fallback carries the hello), observations always accepted.
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agent/v1/observations" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		http.NotFound(w, r)
	}))
	defer cp.Close()

	dir := t.TempDir()
	cfg := Config{
		Routes:    []Route{{Host: "app.local", ResourceUUID: "res-1", Container: bu.Hostname(), Port: mustPort(t, bu)}},
		Resources: []Resource{{UUID: "res-1", Containers: []string{"c1"}}},
		Ingress:   []IngressRoute{{Host: "dev.local", EndpointUUID: "ep1"}},
	}
	data, err := MarshalConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, RoutesFile), data, 0o644); err != nil {
		t.Fatal(err)
	}

	rt := &fake.Runtime{}
	rt.ContainerInspectFn = func(context.Context, string) (container.InspectResponse, error) {
		return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{
			State: &container.State{Running: true},
		}}, nil
	}
	rt.EventsFn = func(ctx context.Context, _ events.ListOptions) (<-chan events.Message, <-chan error) {
		errs := make(chan error, 1)
		go func() {
			<-ctx.Done()
			errs <- ctx.Err()
		}()
		return make(chan events.Message), errs
	}

	addr := freeLoopbackAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, dir, addr, rt, Enrollment{InstanceURL: cp.URL, Token: "akda_test"}, nil)
	}()
	waitServing(t, addr)

	// Scale-to-zero host, containers already running: forwarded to the app.
	resp := serveGet(t, addr, "app.local")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("app.local = %d, want the backend's 418", resp.StatusCode)
	}
	// Ingress host with no laptop attached: the offline page.
	resp = serveGet(t, addr, "dev.local")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("dev.local = %d, want the 503 offline page", resp.StatusCode)
	}
	// Unknown host: refused by the waker.
	resp = serveGet(t, addr, "nope.local")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("nope.local = %d, want 404", resp.StatusCode)
	}
	// The waker recorded the app.local request as activity.
	if _, err := os.Stat(ActivityPath(dir, "res-1")); err != nil {
		t.Fatalf("activity file after a forwarded request: %v", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v after shutdown, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve never returned after cancellation")
	}
}

// TestServeSurfacesABindFailure pins the one error Serve reports: an address
// it cannot listen on.
func TestServeSurfacesABindFailure(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()

	err = Serve(context.Background(), t.TempDir(), l.Addr().String(), &fake.Runtime{}, Enrollment{}, nil)
	if err == nil {
		t.Fatal("Serve on an occupied address must fail")
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		t.Fatalf("bind failure = %v (%T), want a net error", err, err)
	}
}
