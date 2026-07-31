// Package fake is the typed test double for hostops.Ops: a call journal plus
// overridable behaviors, mirroring dockerruntime/fake. Zero-valued, every
// call succeeds (reads answer "absent"); tests assert on the journal and
// script the few calls whose answer matters.
package fake

import (
	"context"
	"io"
	"strings"
	"sync"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/hostops"
)

// Call is one journal entry: the method name and its params struct.
type Call struct {
	Method string
	Params any
}

// Ops records every call and delegates to the matching *Fn when set.
type Ops struct {
	mu    sync.Mutex
	calls []Call

	WriteFileFn  func(ctx context.Context, p agentwire.FileWriteParams) error
	ReadFileFn   func(ctx context.Context, p agentwire.FileReadParams) (agentwire.FileReadResult, error)
	RemoveFn     func(ctx context.Context, p agentwire.FileRemoveParams) error
	StatFn       func(ctx context.Context, path string) (agentwire.FileStatResult, error)
	ChownFn      func(ctx context.Context, p agentwire.FileChownParams) error
	CopyFileFn   func(ctx context.Context, p agentwire.FileCopyParams) error
	EnsureDirFn  func(ctx context.Context, p agentwire.DirEnsureParams) error
	ExecToFileFn func(ctx context.Context, p agentwire.ExecToFileParams) (agentwire.ExecToFileResult, error)
	FileToExecFn func(ctx context.Context, p agentwire.FileToExecParams) (agentwire.FileToExecResult, error)
	FileToURLFn  func(ctx context.Context, p agentwire.FileToURLParams) error
	URLToFileFn  func(ctx context.Context, p agentwire.URLToFileParams) error
	HashFileFn   func(ctx context.Context, path string) (agentwire.FileHashResult, error)
	BuildImageFn func(ctx context.Context, p agentwire.ImageBuildParams) (io.ReadCloser, error)
}

var _ hostops.Ops = (*Ops)(nil)

func (f *Ops) record(method string, params any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{Method: method, Params: params})
}

// Calls returns a copy of the journal.
func (f *Ops) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// CallsTo returns the params of every call to method.
func (f *Ops) CallsTo(method string) []any {
	var out []any
	for _, c := range f.Calls() {
		if c.Method == method {
			out = append(out, c.Params)
		}
	}
	return out
}

func (f *Ops) WriteFile(ctx context.Context, p agentwire.FileWriteParams) error {
	f.record(agentwire.MethodFileWrite, p)
	if f.WriteFileFn != nil {
		return f.WriteFileFn(ctx, p)
	}
	return nil
}

func (f *Ops) ReadFile(ctx context.Context, p agentwire.FileReadParams) (agentwire.FileReadResult, error) {
	f.record(agentwire.MethodFileRead, p)
	if f.ReadFileFn != nil {
		return f.ReadFileFn(ctx, p)
	}
	return agentwire.FileReadResult{}, nil
}

func (f *Ops) Remove(ctx context.Context, p agentwire.FileRemoveParams) error {
	f.record(agentwire.MethodFileRemove, p)
	if f.RemoveFn != nil {
		return f.RemoveFn(ctx, p)
	}
	return nil
}

func (f *Ops) Stat(ctx context.Context, path string) (agentwire.FileStatResult, error) {
	f.record(agentwire.MethodFileStat, agentwire.FileStatParams{Path: path})
	if f.StatFn != nil {
		return f.StatFn(ctx, path)
	}
	return agentwire.FileStatResult{}, nil
}

func (f *Ops) Chown(ctx context.Context, p agentwire.FileChownParams) error {
	f.record(agentwire.MethodFileChown, p)
	if f.ChownFn != nil {
		return f.ChownFn(ctx, p)
	}
	return nil
}

func (f *Ops) CopyFile(ctx context.Context, p agentwire.FileCopyParams) error {
	f.record(agentwire.MethodFileCopy, p)
	if f.CopyFileFn != nil {
		return f.CopyFileFn(ctx, p)
	}
	return nil
}

func (f *Ops) EnsureDir(ctx context.Context, p agentwire.DirEnsureParams) error {
	f.record(agentwire.MethodDirEnsure, p)
	if f.EnsureDirFn != nil {
		return f.EnsureDirFn(ctx, p)
	}
	return nil
}

func (f *Ops) ExecToFile(ctx context.Context, p agentwire.ExecToFileParams) (agentwire.ExecToFileResult, error) {
	f.record(agentwire.MethodExecToFile, p)
	if f.ExecToFileFn != nil {
		return f.ExecToFileFn(ctx, p)
	}
	return agentwire.ExecToFileResult{}, nil
}

func (f *Ops) FileToExec(ctx context.Context, p agentwire.FileToExecParams) (agentwire.FileToExecResult, error) {
	f.record(agentwire.MethodFileToExec, p)
	if f.FileToExecFn != nil {
		return f.FileToExecFn(ctx, p)
	}
	return agentwire.FileToExecResult{}, nil
}

func (f *Ops) FileToURL(ctx context.Context, p agentwire.FileToURLParams) error {
	f.record(agentwire.MethodFileToURL, p)
	if f.FileToURLFn != nil {
		return f.FileToURLFn(ctx, p)
	}
	return nil
}

func (f *Ops) URLToFile(ctx context.Context, p agentwire.URLToFileParams) error {
	f.record(agentwire.MethodURLToFile, p)
	if f.URLToFileFn != nil {
		return f.URLToFileFn(ctx, p)
	}
	return nil
}

func (f *Ops) HashFile(ctx context.Context, path string) (agentwire.FileHashResult, error) {
	f.record(agentwire.MethodFileHash, agentwire.FileHashParams{Path: path})
	if f.HashFileFn != nil {
		return f.HashFileFn(ctx, path)
	}
	return agentwire.FileHashResult{}, nil
}

func (f *Ops) BuildImage(ctx context.Context, p agentwire.ImageBuildParams) (io.ReadCloser, error) {
	f.record(agentwire.MethodImageBuild, p)
	if f.BuildImageFn != nil {
		return f.BuildImageFn(ctx, p)
	}
	return io.NopCloser(strings.NewReader("")), nil
}
