// Package hostops is the ADR-054 host-operations adapter: the typed file
// primitives the control plane executes on a server's /var/lib/akerdock tree
// through the agent channel — the same rail, vocabulary discipline and
// mandatory-agent stance as the Docker runtime (ADR-051/052). The agent side
// is pure Go on the bind-mounted tree: the helper image is distroless, so
// there is no shell, and nothing outside the enumerated vocabulary runs.
package hostops

import (
	"context"

	"github.com/deepteams/akerdock/internal/agentwire"
)

// Root is the only tree host-ops may touch. Anything outside it — agent
// provisioning, first-contact validation, break-glass — is SSH's remit.
const Root = "/var/lib/akerdock"

// Ops is the host-file vocabulary. Signatures speak the wire types directly:
// they ARE this vocabulary's SDK, exactly as the Docker SDK types are the
// runtime adapter's.
type Ops interface {
	WriteFile(ctx context.Context, p agentwire.FileWriteParams) error
	ReadFile(ctx context.Context, p agentwire.FileReadParams) (agentwire.FileReadResult, error)
	Remove(ctx context.Context, p agentwire.FileRemoveParams) error
	Stat(ctx context.Context, path string) (agentwire.FileStatResult, error)
	Chown(ctx context.Context, p agentwire.FileChownParams) error
	CopyFile(ctx context.Context, p agentwire.FileCopyParams) error
	EnsureDir(ctx context.Context, p agentwire.DirEnsureParams) error
}

// Source resolves the Ops executing on a given server — the seam job code
// depends on, served by the same registries as dockerruntime.Source (the
// api's live channels, or the worker's relay). An unreachable agent answers
// an IsUnavailable error.
type Source interface {
	HostOps(ctx context.Context, serverID int64) (Ops, error)
}
