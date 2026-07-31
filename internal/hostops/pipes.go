// The ADR-054 pipe primitives, agent side: container exec ↔ host file with
// compression, host file ↔ presigned URL. Pure Go — the helper image has no
// shell, no gzip, no curl — and strictly local: the payload moves between the
// container, the mounted tree and the bucket without ever crossing the
// control plane.
package hostops

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime"
)

// tailLimit bounds the diagnostic tails (stderr, HTTP error bodies) a pipe
// verdict carries back — the payload stays on the server, only the reason
// travels.
const tailLimit = 8 << 10

// pipeClient is the transfer client: no global timeout (a multi-gigabyte
// upload outlives any fixed budget — the command's ctx bounds it).
var pipeClient = &http.Client{}

// tailBuffer keeps the last tailLimit bytes written to it.
type tailBuffer struct {
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > tailLimit {
		t.buf = t.buf[len(t.buf)-tailLimit:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }

func (l *Local) execRuntime() (dockerruntime.Runtime, error) {
	if l.RT == nil {
		return nil, fmt.Errorf("this helper has no local runtime for pipe execs: %w", cerrdefs.ErrUnavailable)
	}
	return l.RT, nil
}

// ExecToFile runs the exec and streams its stdout — gzipped on request —
// into the target file, hashing exactly what lands on disk.
func (l *Local) ExecToFile(ctx context.Context, p agentwire.ExecToFileParams) (agentwire.ExecToFileResult, error) {
	var out agentwire.ExecToFileResult
	path, err := l.resolve(p.Path)
	if err != nil {
		return out, err
	}
	rt, err := l.execRuntime()
	if err != nil {
		return out, err
	}
	if p.MakeDirs {
		dirMode := os.FileMode(p.DirMode)
		if dirMode == 0 {
			dirMode = 0o700
		}
		if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
			return out, err
		}
	}
	mode := os.FileMode(p.Mode)
	if mode == 0 {
		mode = 0o600
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return out, err
	}
	defer func() { _ = f.Close() }()
	if err := f.Chmod(mode); err != nil {
		return out, err
	}

	created, err := rt.ContainerExecCreate(ctx, p.Container, container.ExecOptions{
		Cmd: p.Cmd, AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		return out, err
	}
	att, err := rt.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return out, err
	}
	defer att.Close()

	hash := sha256.New()
	counted := &countingWriter{w: io.MultiWriter(f, hash)}
	var payload io.Writer = counted
	var gz *gzip.Writer
	if p.Gzip {
		gz = gzip.NewWriter(counted)
		payload = gz
	}
	stderr := &tailBuffer{}
	if _, err := stdcopy.StdCopy(payload, stderr, att.Reader); err != nil {
		return out, fmt.Errorf("exec stream: %w", err)
	}
	if gz != nil {
		if err := gz.Close(); err != nil {
			return out, err
		}
	}
	if err := f.Close(); err != nil {
		return out, err
	}
	inspect, err := rt.ContainerExecInspect(ctx, created.ID)
	if err != nil {
		return out, err
	}
	return agentwire.ExecToFileResult{
		ExitCode: inspect.ExitCode, Stderr: stderr.String(),
		SizeBytes: counted.n, SHA256: hex.EncodeToString(hash.Sum(nil)),
	}, nil
}

// FileToExec streams the file — gunzipped on request — into the exec's stdin
// and reports its exit code with the merged output tail.
func (l *Local) FileToExec(ctx context.Context, p agentwire.FileToExecParams) (agentwire.FileToExecResult, error) {
	var out agentwire.FileToExecResult
	path, err := l.resolve(p.Path)
	if err != nil {
		return out, err
	}
	rt, err := l.execRuntime()
	if err != nil {
		return out, err
	}
	f, err := os.Open(path)
	if err != nil {
		return out, err
	}
	defer func() { _ = f.Close() }()
	var payload io.Reader = f
	if p.Gunzip {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return out, fmt.Errorf("the dump is not gzip data: %w", err)
		}
		defer func() { _ = gz.Close() }()
		payload = gz
	}

	created, err := rt.ContainerExecCreate(ctx, p.Container, container.ExecOptions{
		Cmd: p.Cmd, AttachStdin: true, AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		return out, err
	}
	att, err := rt.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return out, err
	}
	defer att.Close()

	// The output must drain WHILE stdin feeds: a psql that blocks writing its
	// notices against a full pipe would deadlock the copy.
	tail := &tailBuffer{}
	done := make(chan error, 1)
	go func() {
		_, err := stdcopy.StdCopy(tail, tail, att.Reader)
		done <- err
	}()
	if _, err := io.Copy(att.Conn, payload); err != nil {
		return out, fmt.Errorf("feeding the exec: %w", err)
	}
	_ = att.CloseWrite()
	if err := <-done; err != nil {
		return out, fmt.Errorf("exec stream: %w", err)
	}
	inspect, err := rt.ContainerExecInspect(ctx, created.ID)
	if err != nil {
		return out, err
	}
	return agentwire.FileToExecResult{ExitCode: inspect.ExitCode, Output: tail.String()}, nil
}

// FileToURL uploads the file with a plain PUT — the presigned URL arrived in
// the command body over the encrypted channel, never argv (INV-003). A
// non-2xx answer fails with the body's tail: an S3 error names its cause and
// carries no credential.
func (l *Local) FileToURL(ctx context.Context, p agentwire.FileToURLParams) error {
	path, err := l.resolve(p.Path)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, p.URL, f)
	if err != nil {
		return err
	}
	req.ContentLength = st.Size()
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	resp, err := pipeClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, tailLimit))
		return fmt.Errorf("upload answered %s: %s", resp.Status, string(body))
	}
	return nil
}

// URLToFile downloads the URL into the file. The content is verified by the
// caller's checksum comparison, exactly as a local dump would be.
func (l *Local) URLToFile(ctx context.Context, p agentwire.URLToFileParams) error {
	path, err := l.resolve(p.Path)
	if err != nil {
		return err
	}
	if p.MakeDirs {
		dirMode := os.FileMode(p.DirMode)
		if dirMode == 0 {
			dirMode = 0o700
		}
		if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return err
	}
	resp, err := pipeClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, tailLimit))
		return fmt.Errorf("download answered %s: %s", resp.Status, string(body))
	}
	mode := os.FileMode(p.Mode)
	if mode == 0 {
		mode = 0o600
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		_ = os.Remove(path) // never leave a truncated download looking whole
		return err
	}
	return f.Close()
}

// HashFile digests the file as it sits on disk.
func (l *Local) HashFile(_ context.Context, path string) (agentwire.FileHashResult, error) {
	resolved, err := l.resolve(path)
	if err != nil {
		return agentwire.FileHashResult{}, err
	}
	f, err := os.Open(resolved)
	if err != nil {
		return agentwire.FileHashResult{}, err
	}
	defer func() { _ = f.Close() }()
	hash := sha256.New()
	n, err := io.Copy(hash, f)
	if err != nil {
		return agentwire.FileHashResult{}, err
	}
	return agentwire.FileHashResult{SHA256: hex.EncodeToString(hash.Sum(nil)), SizeBytes: n}, nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
