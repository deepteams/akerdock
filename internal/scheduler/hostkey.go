package scheduler

import "github.com/deepteams/akerdock/internal/store"

// hostKeyOf returns the pinned host key of a server (§20.1); empty means the
// server has never been validated, which the drift reconciler treats as
// trust-on-first-use rather than refusing to converge a proxy.
func hostKeyOf(server store.Server) string {
	if server.HostKeyFingerprint == nil {
		return ""
	}
	return *server.HostKeyFingerprint
}
