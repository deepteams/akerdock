package dockerruntime

import (
	"strings"
	"testing"
)

func TestNewLocalDefaultsToTheStandardSocket(t *testing.T) {
	rt, err := NewLocal("", "")
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	defer func() { _ = rt.Close() }()
	if got := rt.DaemonHost(); got != "unix://"+DefaultSocket {
		t.Fatalf("DaemonHost = %q, want unix://%s", got, DefaultSocket)
	}
}

func TestNewLocalPinsTheRequestedAPIVersion(t *testing.T) {
	rt, err := NewLocal("/tmp/akerdock-test.sock", "1.45")
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	defer func() { _ = rt.Close() }()
	if got := rt.ClientVersion(); got != "1.45" {
		t.Fatalf("ClientVersion = %q, want pinned 1.45", got)
	}
	if !strings.HasPrefix(rt.DaemonHost(), "unix:///tmp/") {
		t.Fatalf("DaemonHost = %q, want the explicit socket", rt.DaemonHost())
	}
}
