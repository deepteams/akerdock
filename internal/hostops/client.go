package hostops

import (
	"context"
	"encoding/json"
	"io"

	"github.com/deepteams/akerdock/internal/agentwire"
)

// Sender carries typed commands to a server's agent — the unary calls and
// the one streamed build — satisfied by the same live and relayed
// connections the Docker runtime rides.
type Sender interface {
	Command(ctx context.Context, method string, params any) (json.RawMessage, error)
	Stream(ctx context.Context, method string, params any) (io.ReadCloser, error)
}

// NewClient returns the Ops that executes every call as a typed command on
// the server's agent channel. The agent is mandatory (ADR-051/054): a dead
// channel surfaces as an IsUnavailable error, and the caller's remedy is
// repairing the agent, never a shell fallback.
func NewClient(s Sender) Ops {
	return &client{s: s}
}

type client struct {
	s Sender
}

var _ Ops = (*client)(nil)

// call sends one command and unmarshals its result body into T.
func call[T any](ctx context.Context, s Sender, method string, params any) (T, error) {
	var out T
	raw, err := s.Command(ctx, method, params)
	if err != nil || len(raw) == 0 {
		return out, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	return out, nil
}

func (c *client) WriteFile(ctx context.Context, p agentwire.FileWriteParams) error {
	_, err := c.s.Command(ctx, agentwire.MethodFileWrite, p)
	return err
}

func (c *client) ReadFile(ctx context.Context, p agentwire.FileReadParams) (agentwire.FileReadResult, error) {
	return call[agentwire.FileReadResult](ctx, c.s, agentwire.MethodFileRead, p)
}

func (c *client) Remove(ctx context.Context, p agentwire.FileRemoveParams) error {
	_, err := c.s.Command(ctx, agentwire.MethodFileRemove, p)
	return err
}

func (c *client) Stat(ctx context.Context, path string) (agentwire.FileStatResult, error) {
	return call[agentwire.FileStatResult](ctx, c.s, agentwire.MethodFileStat, agentwire.FileStatParams{Path: path})
}

func (c *client) Chown(ctx context.Context, p agentwire.FileChownParams) error {
	_, err := c.s.Command(ctx, agentwire.MethodFileChown, p)
	return err
}

func (c *client) CopyFile(ctx context.Context, p agentwire.FileCopyParams) error {
	_, err := c.s.Command(ctx, agentwire.MethodFileCopy, p)
	return err
}

func (c *client) EnsureDir(ctx context.Context, p agentwire.DirEnsureParams) error {
	_, err := c.s.Command(ctx, agentwire.MethodDirEnsure, p)
	return err
}

func (c *client) ExecToFile(ctx context.Context, p agentwire.ExecToFileParams) (agentwire.ExecToFileResult, error) {
	return call[agentwire.ExecToFileResult](ctx, c.s, agentwire.MethodExecToFile, p)
}

func (c *client) FileToExec(ctx context.Context, p agentwire.FileToExecParams) (agentwire.FileToExecResult, error) {
	return call[agentwire.FileToExecResult](ctx, c.s, agentwire.MethodFileToExec, p)
}

func (c *client) FileToURL(ctx context.Context, p agentwire.FileToURLParams) error {
	_, err := c.s.Command(ctx, agentwire.MethodFileToURL, p)
	return err
}

func (c *client) URLToFile(ctx context.Context, p agentwire.URLToFileParams) error {
	_, err := c.s.Command(ctx, agentwire.MethodURLToFile, p)
	return err
}

func (c *client) HashFile(ctx context.Context, path string) (agentwire.FileHashResult, error) {
	return call[agentwire.FileHashResult](ctx, c.s, agentwire.MethodFileHash, agentwire.FileHashParams{Path: path})
}

func (c *client) BuildImage(ctx context.Context, p agentwire.ImageBuildParams) (io.ReadCloser, error) {
	return c.s.Stream(ctx, agentwire.MethodImageBuild, p)
}
