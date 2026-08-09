package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// logsServer fakes every endpoint `akerdock logs` touches. onPreviewLogs, when
// set, is invoked at each preview snapshot poll (used to stop a -f loop).
func logsServer(t *testing.T, onPreviewLogs func(poll int)) *httptest.Server {
	t.Helper()
	previewPolls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/applications":
			_, _ = w.Write([]byte(`{"data":[{"uuid":"app-1","name":"varuna"}]}`))
		case "/api/v1/applications/app-1/logs":
			if got := r.URL.Query().Get("component"); got != "" && got != "web" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(`{"data":[
				{"sequence":1,"timestamp":"t1","channel":"stdout","message":"hello"},
				{"sequence":2,"timestamp":"t2","channel":"system","message":"container restarted"}]}`))
		case "/api/v1/applications/app-1/logs/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: log\n")
			_, _ = fmt.Fprint(w, "data: {\"message\":\"streamed line\",\"channel\":\"stdout\"}\n\n")
			_, _ = fmt.Fprint(w, "data: {not json}\n\n")
		case "/api/v1/applications/app-1/previews":
			_, _ = w.Write([]byte(`{"data":[{"uuid":"pv-1","pr_id":42,"status":"active"}]}`))
		case "/api/v1/applications/app-1/previews/pv-1/logs":
			previewPolls++
			if onPreviewLogs != nil {
				onPreviewLogs(previewPolls)
			}
			_, _ = w.Write([]byte(`{"data":[{"sequence":1,"channel":"stdout","message":"preview line"}]}`))
		case "/api/v1/applications/app-1/deployments":
			_, _ = w.Write([]byte(`{"data":[
				{"uuid":"dep-9","pr_id":null},
				{"uuid":"dep-42","pr_id":42}]}`))
		case "/api/v1/deployments/dep-9/logs", "/api/v1/deployments/dep-42/logs", "/api/v1/deployments/dep-x/logs":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "data: {\"message\":\"build log for %s\"}\n\n", strings.Split(r.URL.Path, "/")[4])
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprintf(w, `{"code":"boom","message":"unexpected path %s"}`, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLogsSnapshot(t *testing.T) {
	srv := logsServer(t, nil)
	setupContext(t, srv.URL)
	out, errOut := captureOutput(t, func() {
		if err := runCmd(logsCmd(kindApp), "varuna", "-c", "web"); err != nil {
			t.Errorf("logs: %v", err)
		}
	})
	if !strings.Contains(out, "hello") {
		t.Fatalf("stdout = %q", out)
	}
	// System lines go to stderr with a marker, so piping stdout stays clean.
	if !strings.Contains(errOut, "· container restarted") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestLogsSnapshotJSON(t *testing.T) {
	srv := logsServer(t, nil)
	setupContext(t, srv.URL)
	flags.output = "json"
	out, _ := captureOutput(t, func() {
		if err := runCmd(logsCmd(kindApp), "varuna"); err != nil {
			t.Errorf("logs: %v", err)
		}
	})
	if !strings.Contains(out, `"message": "hello"`) {
		t.Fatalf("json output = %q", out)
	}
}

func TestLogsFollowStream(t *testing.T) {
	srv := logsServer(t, nil)
	setupContext(t, srv.URL)
	out, _ := captureOutput(t, func() {
		if err := runCmd(logsCmd(kindApp), "varuna", "-f"); err != nil {
			t.Errorf("logs -f: %v", err)
		}
	})
	if !strings.Contains(out, "streamed line") {
		t.Fatalf("stdout = %q", out)
	}
}

func TestLogsDeployment(t *testing.T) {
	srv := logsServer(t, nil)
	setupContext(t, srv.URL)

	t.Run("latest", func(t *testing.T) {
		out, _ := captureOutput(t, func() {
			if err := runCmd(logsCmd(kindApp), "varuna", "--deployment"); err != nil {
				t.Errorf("logs --deployment: %v", err)
			}
		})
		if !strings.Contains(out, "build log for dep-9") {
			t.Fatalf("stdout = %q", out)
		}
	})

	t.Run("explicit uuid", func(t *testing.T) {
		out, _ := captureOutput(t, func() {
			// NoOptDefVal flags take their value with `=`.
			if err := runCmd(logsCmd(kindApp), "varuna", "--deployment=dep-x"); err != nil {
				t.Errorf("logs --deployment=dep-x: %v", err)
			}
		})
		if !strings.Contains(out, "build log for dep-x") {
			t.Fatalf("stdout = %q", out)
		}
	})

	t.Run("preview latest", func(t *testing.T) {
		out, _ := captureOutput(t, func() {
			if err := runCmd(logsCmd(kindApp), "varuna", "--pr", "42", "--deployment"); err != nil {
				t.Errorf("logs --pr --deployment: %v", err)
			}
		})
		// The deployment picked must be the preview's, not the newest overall.
		if !strings.Contains(out, "build log for dep-42") {
			t.Fatalf("stdout = %q", out)
		}
	})
}

func TestLogsPreviewSnapshot(t *testing.T) {
	srv := logsServer(t, nil)
	setupContext(t, srv.URL)
	out, _ := captureOutput(t, func() {
		if err := runCmd(logsCmd(kindApp), "varuna", "--pr", "42"); err != nil {
			t.Errorf("logs --pr: %v", err)
		}
	})
	if !strings.Contains(out, "preview line") {
		t.Fatalf("stdout = %q", out)
	}
}

func TestLogsPreviewFollowStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := logsServer(t, func(poll int) {
		if poll == 1 {
			// Give the client time to print the page and reach its poll wait,
			// then interrupt — the loop must exit cleanly, not error out.
			go func() {
				time.Sleep(100 * time.Millisecond)
				cancel()
			}()
		}
	})
	setupContext(t, srv.URL)
	out, _ := captureOutput(t, func() {
		if err := runCmdCtx(ctx, logsCmd(kindApp), "varuna", "--pr", "42", "-f"); err != nil {
			t.Errorf("logs --pr -f: %v", err)
		}
	})
	if !strings.Contains(out, "preview line") {
		t.Fatalf("stdout = %q", out)
	}
}

func TestLogsErrors(t *testing.T) {
	srv := logsServer(t, nil)

	t.Run("without a client", func(t *testing.T) {
		setupHome(t)
		if err := runCmd(logsCmd(kindApp), "varuna"); err == nil {
			t.Fatal("expected a client error")
		}
	})

	// The type/name form is gone (ADR-070 §5) and must be refused by naming the
	// spelling that replaced it — never resolved as a literal name.
	t.Run("the old REF form is refused by name", func(t *testing.T) {
		setupContext(t, srv.URL)
		err := runCmd(logsCmd(kindApp), "db/pg")
		if err == nil || !strings.Contains(err.Error(), "akerdock db <verb> pg") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("bad ref", func(t *testing.T) {
		setupContext(t, srv.URL)
		if err := runCmd(logsCmd(kindApp), "nope"); err == nil {
			t.Fatal("expected a ref error")
		}
	})

	t.Run("unknown app", func(t *testing.T) {
		setupContext(t, srv.URL)
		if err := runCmd(logsCmd(kindApp), "ghost"); err == nil {
			t.Fatal("expected a resolve error")
		}
	})

	t.Run("unknown preview", func(t *testing.T) {
		setupContext(t, srv.URL)
		if err := runCmd(logsCmd(kindApp), "varuna", "--pr", "99"); err == nil || !strings.Contains(err.Error(), "no preview") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestLatestPreviewDeploymentNotFound(t *testing.T) {
	srv := logsServer(t, nil)
	setupContext(t, srv.URL)
	c, err := newClient("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.latestPreviewDeployment(context.Background(), "app-1", 7); err == nil || !strings.Contains(err.Error(), "no deployment yet") {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.latestPreviewDeployment(context.Background(), "ghost", 42); err == nil {
		t.Fatal("expected the API error to surface")
	}
}

func TestStreamSSEBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"unauthorized","message":"token expired"}`))
	}))
	defer srv.Close()
	c := &Client{base: srv.URL, token: "tok", http: srv.Client()}
	err := c.streamSSE(context.Background(), "/whatever", url.Values{"a": {"b"}}, func(logLine) {})
	if err == nil || !strings.Contains(err.Error(), "token expired") {
		t.Fatalf("err = %v", err)
	}
}

func TestStreamSSERequestErrors(t *testing.T) {
	c := &Client{base: "http://127.0.0.1:1", token: "tok"}
	if err := c.streamSSE(context.Background(), "/x", nil, func(logLine) {}); err == nil {
		t.Fatal("expected a connection error")
	}
}

func TestStreamDeploymentLogsNoDeployment(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	c := &Client{base: srv.URL, token: "tok", http: srv.Client()}
	err := c.streamDeploymentLogs(context.Background(), "app-1", "")
	if err == nil || !strings.Contains(err.Error(), "no deployment yet") {
		t.Fatalf("err = %v", err)
	}
}

func TestStreamDeploymentLogsListError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := &Client{base: srv.URL, token: "tok", http: srv.Client()}
	if err := c.streamDeploymentLogs(context.Background(), "app-1", "latest"); err == nil {
		t.Fatal("expected the list error to surface")
	}
}
