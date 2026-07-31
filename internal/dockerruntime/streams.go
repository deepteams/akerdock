package dockerruntime

import (
	"io"
	"sync"

	"github.com/docker/docker/pkg/stdcopy"
)

// Demux copies a Docker log/attach stream to onOutput until EOF. The Engine
// multiplexes stdout and stderr with an 8-byte frame header unless the
// container has a TTY; tty says which framing r carries. Chunks are forwarded
// in arrival order, merged into one stream — the contract sshexec.RunStream
// gives job code today, so migrated call sites keep their onOutput callbacks
// unchanged.
func Demux(r io.Reader, tty bool, onOutput func(string)) error {
	w := &callbackWriter{fn: onOutput}
	var err error
	if tty {
		_, err = io.Copy(w, r)
	} else {
		_, err = stdcopy.StdCopy(w, w, r)
	}
	return err
}

// callbackWriter serializes writes into the callback: stdcopy alternates one
// writer for stdout and one for stderr, and both are this same value.
type callbackWriter struct {
	mu sync.Mutex
	fn func(string)
}

func (w *callbackWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(p) > 0 {
		w.fn(string(p))
	}
	return len(p), nil
}
