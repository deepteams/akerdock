package main

import (
	"strings"
	"testing"

	"github.com/deepteams/akerdock/internal/store"
)

func TestPickProxyServer(t *testing.T) {
	instanceHost := store.Server{ID: 1, Name: "hub", IsLocalhost: true}
	other := store.Server{ID: 2, Name: "prod-eu"}
	servers := []store.Server{other, instanceHost}

	// The default target is the one whose proxy can lock the operator out.
	got, err := pickProxyServer(servers, "")
	if err != nil || got.ID != instanceHost.ID {
		t.Fatalf("default = %+v, %v", got, err)
	}
	if got, err := pickProxyServer(servers, "PROD-EU"); err != nil || got.ID != other.ID {
		t.Fatalf("by name = %+v, %v", got, err)
	}

	// An unknown name must list what does exist: the operator running this is
	// locked out of the dashboard and has no other way to look the names up.
	_, err = pickProxyServer(servers, "nope")
	if err == nil || !strings.Contains(err.Error(), "prod-eu") {
		t.Fatalf("unknown server error = %v", err)
	}
	_, err = pickProxyServer([]store.Server{other}, "")
	if err == nil || !strings.Contains(err.Error(), "--server") {
		t.Fatalf("no instance host error = %v", err)
	}
}
