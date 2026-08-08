package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFileActivityRecordRoundTrip pins the activity file contract (§8.1): one
// file per resource, decimal Unix seconds, readable back through
// ParseActivity — exactly what the control plane's SSH read does.
func TestFileActivityRecordRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "waker") // Record must create it
	act := FileActivity{Dir: dir}
	at := time.Unix(1_700_000_000, 500) // sub-second precision is dropped

	if err := act.Record("res-1", at); err != nil {
		t.Fatalf("Record: %v", err)
	}
	raw, err := os.ReadFile(ActivityPath(dir, "res-1"))
	if err != nil {
		t.Fatalf("activity file: %v", err)
	}
	if string(raw) != "1700000000" {
		t.Fatalf("activity content = %q, want decimal Unix seconds", raw)
	}
	got, err := ParseActivity(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(time.Unix(1_700_000_000, 0)) {
		t.Fatalf("round trip = %v", got)
	}
	// No temp file left behind: the write is rename-atomic.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

// TestFileActivityRecordFailsOnUnwritableDir surfaces the mkdir error instead
// of silently losing the timestamp.
func TestFileActivityRecordFailsOnUnwritableDir(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	act := FileActivity{Dir: filepath.Join(file, "waker")} // parent is a file
	if err := act.Record("res-1", time.Now()); err == nil {
		t.Fatal("Record must fail when the directory cannot be created")
	}
}

// TestConfigMarshalLoadRoundTrip pins the deposited routing table: the control
// plane renders it with MarshalConfig, the waker reads it with LoadConfig.
func TestConfigMarshalLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Routes:    []Route{{Host: "app.example.com", ResourceUUID: "res-1", Container: "c1", Port: 3000}},
		Resources: []Resource{{UUID: "res-1", Containers: []string{"c1"}}},
		Ingress:   []IngressRoute{{Host: "dev.example.com", EndpointUUID: "ep1"}},
	}
	data, err := MarshalConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, RoutesFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(got.Routes) != 1 || got.Routes[0].Host != "app.example.com" ||
		len(got.Resources) != 1 || got.Resources[0].UUID != "res-1" ||
		len(got.Ingress) != 1 || got.Ingress[0].EndpointUUID != "ep1" {
		t.Fatalf("round trip = %+v", got)
	}
}

// TestLoadConfigErrors distinguishes the two failure modes: a missing file
// (the control plane has not deposited yet — os.IsNotExist for the caller)
// and a corrupt one (a real error worth logging).
func TestLoadConfigErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadConfig(dir); !os.IsNotExist(err) {
		t.Fatalf("missing file: err = %v, want os.IsNotExist", err)
	}
	if err := os.WriteFile(filepath.Join(dir, RoutesFile), []byte(`{corrupt`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(dir); err == nil || !strings.Contains(err.Error(), "invalid routes file") {
		t.Fatalf("corrupt file: err = %v, want the parse error surfaced", err)
	}
}

// TestFileActivityRecordFailsOnReadOnlyDir surfaces the temp-write error.
func TestFileActivityRecordFailsOnReadOnlyDir(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	act := FileActivity{Dir: dir}
	if err := act.Record("res-1", time.Now()); err == nil {
		t.Fatal("Record must fail when the directory is not writable")
	}
}
