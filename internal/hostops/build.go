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
	"sync"
	"time"

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
	lw := &lineWriter{w: pw}
	stopFlush := make(chan struct{})
	// The solve and its progress consumer run to completion in the
	// background; the pipe carries the plain-text progress and, on failure,
	// the solve's error as the stream's terminal error. The recover is the
	// agent's life insurance: a panic anywhere in the solve stack must fail
	// THIS build, never kill the process every command in flight rides on.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				pw.CloseWithError(fmt.Errorf("build panicked: %v", r))
			}
		}()
		defer func() { _ = bk.Close() }()
		eg, egCtx := errgroup.WithContext(ctx)
		eg.Go(func() error {
			_, err := bk.Solve(egCtx, nil, opt, statusCh)
			return err
		})
		eg.Go(func() error {
			display, err := progressui.NewDisplay(lw, progressui.PlainMode)
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
		err := eg.Wait()
		close(stopFlush)
		lw.Flush() // whatever the last tick did not carry
		pw.CloseWithError(err)
	}()
	// The coalescing buffer only bounds SIZE; this bounds LATENCY, so a quiet
	// build step still shows up in the console within a tick.
	go func() {
		tick := time.NewTicker(flushInterval)
		defer tick.Stop()
		for {
			select {
			case <-stopFlush:
				return
			case <-tick.C:
				lw.Flush()
			}
		}
	}()
	return pr, nil
}

// flushInterval bounds how long a coalesced progress line waits before it
// reaches the pipe.
const flushInterval = 100 * time.Millisecond

// lineWriter coalesces the progress display's many small writes — plain mode
// writes per status update, so a chatty build emits thousands of them — into
// pipe writes of at most ChunkSize. Every pipe write becomes one wire frame
// against a per-stream buffer of StreamBuffer chunks: one frame per line
// overruns it on a verbose build and the stream dies as a slow consumer.
// Writing through it (rather than handing the pipe to the display) also keeps
// a CloseWithError on our side authoritative.
type lineWriter struct {
	mu  sync.Mutex
	w   io.Writer
	buf []byte
}

func (l *lineWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	if len(l.buf) < agentwire.ChunkSize {
		return len(p), nil
	}
	if err := l.flushLocked(); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Flush writes out what has accumulated; it is safe to call concurrently with
// Write and after the display is done.
func (l *lineWriter) Flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.flushLocked()
}

func (l *lineWriter) flushLocked() error {
	if len(l.buf) == 0 {
		return nil
	}
	// The pipe write blocks until the pump has taken everything; releasing
	// the buffer first would let a concurrent Write interleave into it.
	_, err := l.w.Write(l.buf)
	l.buf = l.buf[:0]
	return err
}
