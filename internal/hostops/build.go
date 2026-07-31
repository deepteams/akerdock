// The ADR-055 phase-2 build primitive, agent side: a BuildKit build driven
// against the LOCAL daemon's embedded builder — the exact rail `docker build`
// uses — with the context read from the mounted tree. The image lands in the
// daemon's store (moby exporter); only plain-text progress leaves the server.
package hostops

import (
	"context"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/secrets/secretsprovider"
	"github.com/moby/buildkit/util/progress/progressui"
	"github.com/tonistiigi/fsutil"
	"golang.org/x/sync/errgroup"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime"
)

// BuildImage solves the dockerfile.v0 frontend over a gRPC session hijacked
// on the local Docker API — dockerd's embedded BuildKit, so caching, secrets
// and Dockerfile semantics are exactly `docker build`'s. Secrets arrive in
// the typed params and are attached as session secrets, mounted for single
// RUNs and never written to a layer (INV-003, §5.2).
func (l *Local) BuildImage(ctx context.Context, p agentwire.ImageBuildParams) (io.ReadCloser, error) {
	contextDir, err := l.resolve(p.ContextDir)
	if err != nil {
		return nil, err
	}
	if p.Dockerfile == "" || filepath.IsAbs(p.Dockerfile) ||
		strings.HasPrefix(filepath.Clean(p.Dockerfile), "..") {
		return nil, fmt.Errorf("dockerfile %q must be a relative path inside the context: %w",
			p.Dockerfile, cerrdefs.ErrInvalidArgument)
	}
	if len(p.Tags) == 0 {
		return nil, fmt.Errorf("a build needs at least one tag: %w", cerrdefs.ErrInvalidArgument)
	}
	rt, err := l.execRuntime()
	if err != nil {
		return nil, err
	}
	hijacker, ok := rt.(dockerruntime.Hijacker)
	if !ok {
		return nil, fmt.Errorf("this runtime cannot carry a BuildKit session: %w", cerrdefs.ErrUnavailable)
	}

	bk, err := bkclient.New(ctx, "",
		bkclient.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return hijacker.DialHijack(ctx, "/grpc", "h2c", nil)
		}),
		bkclient.WithSessionDialer(func(ctx context.Context, proto string, meta map[string][]string) (net.Conn, error) {
			return hijacker.DialHijack(ctx, "/session", proto, meta)
		}))
	if err != nil {
		return nil, fmt.Errorf("dialing the daemon's builder: %w", err)
	}

	localFS, err := fsutil.NewFS(contextDir)
	if err != nil {
		return nil, err
	}
	attrs := map[string]string{"filename": p.Dockerfile}
	for k, v := range p.BuildArgs {
		attrs["build-arg:"+k] = v
	}
	for k, v := range p.Labels {
		attrs["label:"+k] = v
	}
	if p.Target != "" {
		attrs["target"] = p.Target
	}
	if p.NoCache {
		attrs["no-cache"] = ""
	}
	var attachables []session.Attachable
	if len(p.Secrets) > 0 {
		attachables = append(attachables, secretsprovider.FromMap(p.Secrets))
	}
	opt := bkclient.SolveOpt{
		Frontend:      "dockerfile.v0",
		FrontendAttrs: attrs,
		LocalMounts:   map[string]fsutil.FS{"context": localFS, "dockerfile": localFS},
		Exports: []bkclient.ExportEntry{{
			Type:  "moby",
			Attrs: map[string]string{"name": strings.Join(p.Tags, ",")},
		}},
		Session: attachables,
	}

	pr, pw := io.Pipe()
	statusCh := make(chan *bkclient.SolveStatus)
	// The solve and its progress consumer run to completion in the
	// background; the pipe carries the plain-text progress and, on failure,
	// the solve's error as the stream's terminal error.
	go func() {
		defer func() { _ = bk.Close() }()
		eg, egCtx := errgroup.WithContext(ctx)
		eg.Go(func() error {
			_, err := bk.Solve(egCtx, nil, opt, statusCh)
			return err
		})
		eg.Go(func() error {
			display, err := progressui.NewDisplay(&lineWriter{w: pw}, progressui.PlainMode)
			if err != nil {
				// The status channel must drain or the solve deadlocks.
				for status := range statusCh {
					_ = status
				}
				return err
			}
			_, err = display.UpdateFrom(egCtx, statusCh)
			return err
		})
		pw.CloseWithError(eg.Wait())
	}()
	return pr, nil
}

// lineWriter forwards writes; it exists so the progress display never
// receives the pipe directly (a CloseWithError on our side must win).
type lineWriter struct {
	w io.Writer
}

func (l *lineWriter) Write(p []byte) (int, error) { return l.w.Write(p) }
