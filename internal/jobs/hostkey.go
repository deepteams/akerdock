package jobs

import "github.com/deepteams/akerdock/internal/store"

// PinnedHostKey returns the host key a server is expected to present (§20.1).
//
// An empty string means "nothing pinned yet" — trust-on-first-use. That is the
// case exactly once, during the first validation of a server; every operational
// job afterwards runs against a pinned key, so a server that suddenly presents
// a different one fails loudly instead of being handed our deploy key.
// PinnedHostKey is the pinned SSH host key of a server, or "" on first
// contact (§20.1). Exported because the API dials servers too (proxy logs).
func PinnedHostKey(server store.Server) string { return pinnedHostKey(server) }

func pinnedHostKey(server store.Server) string {
	if server.HostKeyFingerprint == nil {
		return ""
	}
	return *server.HostKeyFingerprint
}
