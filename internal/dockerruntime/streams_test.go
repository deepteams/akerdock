package dockerruntime

import (
	"bytes"
	"strings"
	"testing"

	"github.com/docker/docker/pkg/stdcopy"
)

func TestDemuxMultiplexedMergesStdoutAndStderrInOrder(t *testing.T) {
	var buf bytes.Buffer
	stdout := stdcopy.NewStdWriter(&buf, stdcopy.Stdout)
	stderr := stdcopy.NewStdWriter(&buf, stdcopy.Stderr)
	if _, err := stdout.Write([]byte("building\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := stderr.Write([]byte("warning: cache miss\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := stdout.Write([]byte("done\n")); err != nil {
		t.Fatal(err)
	}

	var got []string
	if err := Demux(&buf, false, func(chunk string) { got = append(got, chunk) }); err != nil {
		t.Fatalf("Demux: %v", err)
	}
	joined := strings.Join(got, "")
	want := "building\nwarning: cache miss\ndone\n"
	if joined != want {
		t.Fatalf("merged stream = %q, want %q", joined, want)
	}
}

func TestDemuxTTYPassesRawBytesThrough(t *testing.T) {
	var got strings.Builder
	r := strings.NewReader("raw tty output")
	if err := Demux(r, true, func(chunk string) { got.WriteString(chunk) }); err != nil {
		t.Fatalf("Demux: %v", err)
	}
	if got.String() != "raw tty output" {
		t.Fatalf("tty stream = %q", got.String())
	}
}
