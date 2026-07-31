package hostops

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	cerrdefs "github.com/containerd/errdefs"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime"
)

// defaultReadCap bounds a FileRead when the caller sets none: these files are
// manifests and stores, and an unbounded read would let one command occupy
// the channel's memory.
const defaultReadCap = 4 << 20

// Local executes the vocabulary on the local filesystem — the agent side.
// Every path is resolved against Root before anything is touched; the guard
// is authoritative here, whatever the control plane sent. RT is the LOCAL
// Docker runtime the pipe primitives exec through; nil refuses them.
type Local struct {
	Root string
	RT   dockerruntime.Runtime
}

// DetectLocal returns the Local rooted at Root when the tree is present, nil
// when it is not — a helper created before the ADR-054 mount (spec < 7) has
// no host tree, and nil makes the executor answer that plainly instead of
// ENOENT-ing on every call. rt powers the pipe primitives.
func DetectLocal(rt dockerruntime.Runtime) *Local {
	if st, err := os.Stat(Root); err != nil || !st.IsDir() {
		return nil
	}
	return &Local{Root: Root, RT: rt}
}

var _ Ops = (*Local)(nil)

// resolve validates that path is absolute, clean and inside the root.
func (l *Local) resolve(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", fmt.Errorf("path %q must be absolute and clean: %w", path, cerrdefs.ErrInvalidArgument)
	}
	root := l.Root
	if path != root && !strings.HasPrefix(path, root+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes %s: %w", path, root, cerrdefs.ErrInvalidArgument)
	}
	return path, nil
}

func (l *Local) WriteFile(_ context.Context, p agentwire.FileWriteParams) error {
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
	target := path
	if p.Atomic {
		target = path + ".tmp"
	}
	if err := os.WriteFile(target, p.Content, os.FileMode(p.Mode)); err != nil {
		return err
	}
	// WriteFile's mode only applies to a NEW file; an existing one keeps its
	// bits. Chmod makes the requested mode true either way, umask included.
	if err := os.Chmod(target, os.FileMode(p.Mode)); err != nil {
		return err
	}
	if p.Atomic {
		return os.Rename(target, path)
	}
	return nil
}

func (l *Local) ReadFile(_ context.Context, p agentwire.FileReadParams) (agentwire.FileReadResult, error) {
	path, err := l.resolve(p.Path)
	if err != nil {
		return agentwire.FileReadResult{}, err
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return agentwire.FileReadResult{}, nil
	}
	if err != nil {
		return agentwire.FileReadResult{}, err
	}
	defer func() { _ = f.Close() }()
	limit := p.MaxBytes
	if limit <= 0 {
		limit = defaultReadCap
	}
	content, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return agentwire.FileReadResult{}, err
	}
	res := agentwire.FileReadResult{Content: content, Found: true}
	// One probe byte decides truncation without stating the whole file.
	if n, _ := f.Read(make([]byte, 1)); n > 0 {
		res.Truncated = true
	}
	return res, nil
}

func (l *Local) Remove(_ context.Context, p agentwire.FileRemoveParams) error {
	path, err := l.resolve(p.Path)
	if err != nil {
		return err
	}
	if path == l.Root {
		return fmt.Errorf("refusing to remove the root itself: %w", cerrdefs.ErrInvalidArgument)
	}
	if p.Recursive {
		return os.RemoveAll(path)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (l *Local) Stat(_ context.Context, path string) (agentwire.FileStatResult, error) {
	resolved, err := l.resolve(path)
	if err != nil {
		return agentwire.FileStatResult{}, err
	}
	st, err := os.Stat(resolved)
	if os.IsNotExist(err) {
		return agentwire.FileStatResult{}, nil
	}
	if err != nil {
		return agentwire.FileStatResult{}, err
	}
	return agentwire.FileStatResult{Found: true, IsDir: st.IsDir(), Size: st.Size()}, nil
}

func (l *Local) Chown(_ context.Context, p agentwire.FileChownParams) error {
	path, err := l.resolve(p.Path)
	if err != nil {
		return err
	}
	return os.Chown(path, p.UID, p.GID)
}

func (l *Local) CopyFile(_ context.Context, p agentwire.FileCopyParams) error {
	src, err := l.resolve(p.Src)
	if err != nil {
		return err
	}
	dst, err := l.resolve(p.Dst)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func (l *Local) EnsureDir(_ context.Context, p agentwire.DirEnsureParams) error {
	path, err := l.resolve(p.Path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(path, os.FileMode(p.Mode)); err != nil {
		return err
	}
	return os.Chmod(path, os.FileMode(p.Mode))
}
