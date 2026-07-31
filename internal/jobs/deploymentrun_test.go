package jobs

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/deepteams/akerdock/internal/agentwire"
	hostfake "github.com/deepteams/akerdock/internal/hostops/fake"
	"github.com/deepteams/akerdock/internal/store"
)

// A deployment that reuses an existing artifact clones nothing, builds
// nothing, records no artifact and prunes nothing — whether it looks
// backwards (rollback) or at the image already running (skip_build,
// ADR-048). Getting this wrong on skip_build would have it rebuild from a
// branch that moved, which is the opposite of applying a configuration.
func TestReusesArtifact(t *testing.T) {
	tests := map[string]struct {
		deployment store.Deployment
		want       bool
	}{
		"plain deployment": {store.Deployment{}, false},
		"rollback":         {store.Deployment{IsRollback: true}, true},
		"skip build":       {store.Deployment{SkipBuild: true}, true},
		"forced rebuild":   {store.Deployment{ForceRebuild: true}, false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := &deploymentRun{d: tc.deployment}
			if got := r.reusesArtifact(); got != tc.want {
				t.Fatalf("reusesArtifact() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPreviewLifecyclePayloadIncludesSlackContext(t *testing.T) {
	fqdn := "varuna-pr8.ad.example.com"
	branch := "feat/analytical-engine"
	author := "Ada Lovelace"
	payload := previewLifecyclePayload("varuna", store.Preview{
		Uuid:         mustUUID(t, "22222222-2222-4222-8222-222222222222"),
		PrID:         8,
		Fqdn:         &fqdn,
		SourceBranch: &branch,
	}, store.Deployment{CommitAuthor: &author})

	for key, want := range map[string]any{
		"name":          "varuna",
		"pr_id":         int32(8),
		"fqdn":          fqdn,
		"branch":        branch,
		"commit_author": author,
	} {
		if got := payload[key]; got != want {
			t.Errorf("payload[%q] = %#v, want %#v", key, got, want)
		}
	}
}

// TestAgentBuildPumpsProgressAndSurfacesFailure pins the ADR-055 seam: the
// typed build's plain-text progress reaches the step log line by line, and
// the stream's terminal error IS the build failure, cause included.
func TestAgentBuildPumpsProgressAndSurfacesFailure(t *testing.T) {
	ops := &hostfake.Ops{BuildImageFn: func(context.Context, agentwire.ImageBuildParams) (io.ReadCloser, error) {
		pr, pw := io.Pipe()
		go func() {
			_, _ = io.WriteString(pw, "#1 [internal] load build definition\n#2 DONE\n")
			pw.CloseWithError(errors.New("process \"/bin/sh -c make\" did not complete successfully: exit code 2"))
		}()
		return pr, nil
	}}
	r := &deploymentRun{hops: ops}
	var lines []string
	err := r.agentBuild(context.Background(), func(s string) { lines = append(lines, s) }, agentwire.ImageBuildParams{
		ContextDir: "/var/lib/akerdock/applications/app/source", Dockerfile: "Dockerfile", Tags: []string{"akerdock/app:abc"},
	})
	if err == nil || !strings.Contains(err.Error(), "did not complete successfully") {
		t.Fatalf("agentBuild = %v, want the solve failure surfaced", err)
	}
	if len(lines) != 2 || lines[0] != "#1 [internal] load build definition" {
		t.Fatalf("progress lines = %v", lines)
	}
	// The typed params reached the channel untouched.
	calls := ops.CallsTo(agentwire.MethodImageBuild)
	if len(calls) != 1 || calls[0].(agentwire.ImageBuildParams).Dockerfile != "Dockerfile" {
		t.Fatalf("build calls = %v", calls)
	}
}

// TestDockerVolumeName pins the deterministic naming scheme (INV-011): the
// resource UUID prefixes the declared name, so two applications declaring
// `data` never collide. Existing volumes are addressed by this exact string —
// changing it would orphan every one of them.
func TestDockerVolumeName(t *testing.T) {
	tests := []struct {
		name         string
		resourceUUID string
		volume       string
		want         string
	}{
		{
			name:         "uuid prefix joined with underscore",
			resourceUUID: "0195a0b0-1c2d-7e3f-8a4b-5c6d7e8f9a0b",
			volume:       "data",
			want:         "0195a0b0-1c2d-7e3f-8a4b-5c6d7e8f9a0b_data",
		},
		{
			name:         "name containing an underscore is preserved",
			resourceUUID: "0195a0b0-1c2d-7e3f-8a4b-5c6d7e8f9a0b",
			volume:       "pg_data",
			want:         "0195a0b0-1c2d-7e3f-8a4b-5c6d7e8f9a0b_pg_data",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DockerVolumeName(tt.resourceUUID, tt.volume); got != tt.want {
				t.Errorf("DockerVolumeName(%q, %q) = %q, want %q",
					tt.resourceUUID, tt.volume, got, tt.want)
			}
		})
	}
}
