//go:build dockerintegration

package hostops

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/image"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime"
)

// TestBuildImageAgainstLocalDaemon exercises the whole ADR-055 phase-2 rail
// against the real daemon: the hijacked gRPC session, the dockerfile.v0
// solve with build-args and secrets, the moby exporter, and the plain-text
// progress stream. Run with `make test-docker`.
func TestBuildImageAgainstLocalDaemon(t *testing.T) {
	rt, err := dockerruntime.NewLocal("", "")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	l := &Local{Root: root, RT: rt}
	dockerfile := `FROM busybox
ARG WHO
RUN --mount=type=secret,id=TOKEN test -s /run/secrets/TOKEN
RUN echo "hello ${WHO}" > /greeting
CMD ["cat", "/greeting"]
`
	if err := os.WriteFile(root+"/Dockerfile", []byte(dockerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	tag := "akerdock-test/hostops-build:itest"
	rc, err := l.BuildImage(context.Background(), agentwire.ImageBuildParams{
		ContextDir: root, Dockerfile: "Dockerfile", Tags: []string{tag},
		BuildArgs: map[string]string{"WHO": "akerdock"},
		Secrets:   map[string][]byte{"TOKEN": []byte("hush")},
		Labels:    map[string]string{"akerdock.managed": "true"},
		NoCache:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, readErr := io.ReadAll(rc)
	_ = rc.Close()
	if readErr != nil {
		t.Fatalf("build failed: %v\n%s", readErr, out)
	}
	if !strings.Contains(string(out), "DONE") {
		t.Fatalf("progress stream looks empty:\n%s", out)
	}
	resp, err := rt.ImageInspect(context.Background(), tag)
	if err != nil {
		t.Fatalf("built image absent: %v", err)
	}
	if resp.Config == nil || resp.Config.Labels["akerdock.managed"] != "true" {
		t.Fatalf("labels missing: %+v", resp.Config)
	}
	t.Cleanup(func() {
		_, _ = rt.ImageRemove(context.Background(), tag, image.RemoveOptions{Force: true})
	})
}
