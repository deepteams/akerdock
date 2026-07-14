package sshexec

import (
	"fmt"
	"io"

	"golang.org/x/crypto/ssh"
)

// PTY is an interactive remote session bound to a pseudo-terminal (§24.4):
// Write feeds keystrokes, Read streams the terminal output (stderr is merged
// into the same stream — that is what a tty does), Resize propagates window
// changes. Read returns io.EOF when the remote command exits.
type PTY struct {
	session *ssh.Session
	stdin   io.WriteCloser
	out     *io.PipeReader
}

// StartPTY opens a session with a pseudo-terminal and starts command in it —
// or the user's login shell when command is empty. cols/rows are the initial
// window size; use Resize for later changes.
func (c *Client) StartPTY(command string, cols, rows int) (*PTY, error) {
	session, err := c.conn.NewSession()
	if err != nil {
		return nil, fmt.Errorf("sshexec: session: %w", err)
	}
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("sshexec: request pty: %w", err)
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("sshexec: stdin: %w", err)
	}
	// One pipe for both streams: io.Pipe serialises concurrent writers, and a
	// terminal has a single output anyway.
	pr, pw := io.Pipe()
	session.Stdout = pw
	session.Stderr = pw

	if command == "" {
		err = session.Shell()
	} else {
		err = session.Start(command)
	}
	if err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("sshexec: start: %w", err)
	}

	// Unblock readers when the remote command exits: Wait flushes the output
	// copies first, so nothing printed before exit is lost.
	go func() {
		_ = session.Wait()
		_ = pw.Close()
	}()

	return &PTY{session: session, stdin: stdin, out: pr}, nil
}

// Read streams terminal output; io.EOF means the remote command exited.
func (p *PTY) Read(b []byte) (int, error) { return p.out.Read(b) }

// Write feeds keystrokes to the terminal.
func (p *PTY) Write(b []byte) (int, error) { return p.stdin.Write(b) }

// Resize propagates a window change.
func (p *PTY) Resize(cols, rows int) error { return p.session.WindowChange(rows, cols) }

// Close tears the session down. This is the guaranteed kill of §24.4: closing
// the SSH channel terminates the remote pty, and with it the command.
func (p *PTY) Close() error {
	_ = p.out.Close()
	return p.session.Close()
}
