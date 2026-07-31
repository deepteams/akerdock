package dockerruntime

import (
	"fmt"

	"github.com/docker/docker/client"
)

// DefaultSocket is the local Docker Engine API socket (ADR-004: standalone
// Docker is the only runtime).
const DefaultSocket = "/var/run/docker.sock"

// Local is the Runtime served by the official SDK client over a local unix
// socket — the implementation the agent (and the waker) runs against
// /var/run/docker.sock. Remote execution reaches this same implementation
// through the typed command channel (ADR-052).
type Local struct {
	*client.Client
}

var _ Runtime = (*Local)(nil)

// NewLocal builds a Runtime over the unix socket (DefaultSocket when empty).
// apiVersion optionally pins the Engine API version (e.g. "1.45"); empty — the
// default — negotiates with the daemon, so a client newer than the daemon
// still works. Pinning a version newer than the daemon supports makes every
// call fail ("client version too new"), so we do not pin by default.
func NewLocal(socket, apiVersion string) (*Local, error) {
	if socket == "" {
		socket = DefaultSocket
	}
	opts := []client.Opt{client.WithHost("unix://" + socket)}
	if apiVersion != "" {
		opts = append(opts, client.WithVersion(apiVersion))
	} else {
		opts = append(opts, client.WithAPIVersionNegotiation())
	}
	// No http.Client timeout: streams (events, follow logs) outlive any fixed
	// budget; every call is bounded by its caller's ctx instead.
	cli, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("dockerruntime: %w", err)
	}
	return &Local{Client: cli}, nil
}
