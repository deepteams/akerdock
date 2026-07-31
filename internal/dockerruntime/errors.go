package dockerruntime

import (
	cerrdefs "github.com/containerd/errdefs"
)

// The Engine API answers with typed errors; callers branch on these
// predicates instead of matching CLI stderr text (the old sshexec-era
// discipline). Wrapping errdefs here keeps call sites free of a direct
// dependency on the SDK's error plumbing.

// IsNotFound reports the daemon's "no such object" answer — the API
// equivalent of the CLI's "No such container/volume/network". Removal and
// inspection paths treat it as "nothing to do", the way `|| true` did.
func IsNotFound(err error) bool { return cerrdefs.IsNotFound(err) }

// IsConflict reports a state conflict — name already taken, container not
// stopped before removal, network still in use.
func IsConflict(err error) bool { return cerrdefs.IsConflict(err) }

// IsNotModified reports the daemon's 304 — start of an already-running or
// stop of an already-stopped container. The SDK swallows most of these as
// success; the predicate exists for the paths where it surfaces.
func IsNotModified(err error) bool { return cerrdefs.IsNotModified(err) }

// IsUnavailable reports the mandatory-agent failure mode (ADR-051): the
// server's command channel is not there. The remedy is the agent's
// reconciliation, never a fallback.
func IsUnavailable(err error) bool { return cerrdefs.IsUnavailable(err) }
