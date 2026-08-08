package dockerruntime

import (
	"strings"
	"testing"
)

func TestNewLocalRejectsAnOversizedSocketPath(t *testing.T) {
	// A unix socket path longer than sockaddr_un's limit (~104 bytes) is the
	// one client-construction failure reachable without a daemon.
	if _, err := NewLocal("/tmp/"+strings.Repeat("a", 200)+".sock", ""); err == nil {
		t.Fatal("NewLocal accepted a socket path no OS can bind")
	} else if !strings.Contains(err.Error(), "dockerruntime:") {
		t.Fatalf("error = %v, want the package's wrap prefix", err)
	}
}

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
