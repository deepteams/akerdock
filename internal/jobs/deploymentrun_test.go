package jobs

import (
	"testing"

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

func TestInspectField(t *testing.T) {
	tests := []struct {
		name string
		out  string
		key  string
		want string
	}{
		{
			name: "key present",
			out:  "old=running\nnext=exited\n",
			key:  "old=",
			want: "running",
		},
		{
			name: "second key present",
			out:  "old=running\nnext=exited\n",
			key:  "next=",
			want: "exited",
		},
		{
			name: "line whitespace is trimmed before matching",
			out:  "  old=healthy  \n",
			key:  "old=",
			want: "healthy",
		},
		{
			name: "value whitespace is trimmed",
			out:  "old=   restarting   \n",
			key:  "old=",
			want: "restarting",
		},
		{
			name: "key absent",
			out:  "old=running\n",
			key:  "next=",
			want: "absent",
		},
		{
			name: "empty output",
			out:  "",
			key:  "old=",
			want: "absent",
		},
		{
			name: "empty value",
			out:  "old=\n",
			key:  "old=",
			want: "",
		},
		{
			name: "first matching line wins",
			out:  "state=first\nstate=second\n",
			key:  "state=",
			want: "first",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := inspectField(tt.out, tt.key); got != tt.want {
				t.Errorf("inspectField(%q, %q) = %q, want %q", tt.out, tt.key, got, tt.want)
			}
		})
	}
}

// TestBuildInputsFlags checks the ARG/secret split (INV-003): a plain variable
// becomes --build-arg, a secret NEVER does — it is mounted via --secret. The
// key names below are fixtures; no values are involved at all, which is the
// point of the flag shape.
func TestBuildInputsFlags(t *testing.T) {
	tests := []struct {
		name string
		in   buildInputs
		want string
	}{
		{
			name: "empty inputs render no flags",
			in:   buildInputs{},
			want: "",
		},
		{
			name: "args only",
			in:   buildInputs{args: []string{"NODE_ENV", "BASE_URL"}},
			want: " --build-arg NODE_ENV --build-arg BASE_URL",
		},
		{
			name: "secrets only",
			in:   buildInputs{secrets: []string{"NPM_TOKEN"}},
			want: " --secret id=NPM_TOKEN,env=NPM_TOKEN",
		},
		{
			name: "mixed: args first, then secrets",
			in: buildInputs{
				args:    []string{"NODE_ENV"},
				secrets: []string{"NPM_TOKEN", "FAKE_API_KEY"},
			},
			want: " --build-arg NODE_ENV --secret id=NPM_TOKEN,env=NPM_TOKEN --secret id=FAKE_API_KEY,env=FAKE_API_KEY",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.Flags(); got != tt.want {
				t.Errorf("Flags() = %q, want %q", got, tt.want)
			}
		})
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
