package jobs

// INV-010 — the preview boundary of the deployment variable set.
//
// A preview MUST see its dedicated variable set only; the production set never
// reaches it, at build time as at runtime. The build side is the one that bites:
// on a fork preview the PR author owns the Dockerfile, so anything handed to the
// build is readable — an `ARG` lands in the image metadata, a
// `RUN --mount=type=secret` reads the secret, and the nixpacks build.env sits on
// the host. That is how the defect this file pins worked: the runtime renderer
// branched on the preview, the build renderer did not. Three renderers select a
// variable set — build, runtime and the compose interpolation map — and each is
// exercised here, because one guard held while another was missing is precisely
// the failure that happened.
//
// The test asserts on WHAT COMES OUT of the renderers, never on which query was
// called. A test that pinned the query would pass on a renderer that read the
// preview set AND the production set, and would break on any rename of the
// query — it would pin the implementation and prove nothing. So the fake DB
// below is a WORLD, not a spy: both sets exist, under the same keys with
// different values, and the assertion is that a preview deployment carries no
// production value on any surface that a Dockerfile can read.

import (
	"context"
	"strings"
	"testing"

	"github.com/deepteams/akerdock/internal/store"
	"github.com/jackc/pgx/v5"
)

// The two sets share their keys — an override of DATABASE_URL is the realistic
// shape — so nothing can be told apart by key. Only the value distinguishes
// them, which is exactly what leaks.
var (
	deployEnvProdValues = []string{
		"prod-api-base-marker", "prod-database-url-marker", "prod-session-key-marker",
	}
	deployEnvPreviewValues = []string{
		"preview-api-base-value", "preview-database-url-value", "preview-session-key-value",
	}
)

// deployEnvWorld gives the resource both variable sets: the production one
// behind the deploy query, the dedicated preview one behind the preview query.
// Column indices follow store.EnvironmentVariable: 3 key, 4 value_enc,
// 5 is_secret, 6 is_build_time, 7 is_literal.
func deployEnvWorld(t *testing.T, env *deployrunEnv) {
	t.Helper()
	enc := func(v string) []byte {
		return deployrunEncrypt(t, env, "environment_variables", "value_enc", v)
	}
	rows := func(values []string) []map[int]any {
		return []map[int]any{
			// build-time, plain: becomes a --build-arg, visible in docker history.
			{3: "API_BASE", 4: enc(values[0]), 5: false, 6: true, 7: false},
			// build-time, secret: becomes a BuildKit session secret.
			{3: "DATABASE_URL", 4: enc(values[1]), 5: true, 6: true, 7: false},
			// runtime only.
			{3: "SESSION_KEY", 4: enc(values[2]), 5: true, 6: false, 7: false},
		}
	}
	env.db.rowsFor = func(sql string) (pgx.Rows, error, bool) {
		switch {
		case strings.Contains(sql, "-- name: ListEnvVarsForDeploy "):
			return &deployrunRows{rows: rows(deployEnvProdValues), blob: env.inner.blob}, nil, true
		case strings.Contains(sql, "-- name: ListPreviewEnvVars "):
			return &deployrunRows{rows: rows(deployEnvPreviewValues), blob: env.inner.blob}, nil, true
		}
		return nil, nil, false
	}
}

// deployEnvSurfaces renders every deployment input that carries variable
// VALUES, across the three renderers (build, runtime, compose). A future
// renderer, or a new surface on an existing one, belongs here — so that a new
// path out of the process is a test edit rather than a silent leak.
func deployEnvSurfaces(t *testing.T, r *deploymentRun) map[string][]string {
	t.Helper()
	script, inputs, err := r.renderBuildEnv(context.Background())
	if err != nil {
		t.Fatalf("renderBuildEnv: %v", err)
	}
	runtime, err := r.renderRuntimeEnv(context.Background())
	if err != nil {
		t.Fatalf("renderRuntimeEnv: %v", err)
	}
	// The compose interpolation map (compose-spec §3.2): its values are
	// substituted INTO the compose file deposited on the host, so it carries
	// variable values just as literally as a build arg does.
	stack, err := r.plainEnvVars(context.Background())
	if err != nil {
		t.Fatalf("plainEnvVars: %v", err)
	}
	surfaces := map[string][]string{
		"build.env (nixpacks, written to the host)": {script},
		"build args (baked into image metadata)":    {},
		"BuildKit secrets (mounted into RUN)":       {},
		"runtime environment":                       runtime,
		"compose interpolation map":                 {},
	}
	for k, v := range stack {
		surfaces["compose interpolation map"] = append(surfaces["compose interpolation map"], k+"="+v)
	}
	for k, v := range inputs.argValues {
		surfaces["build args (baked into image metadata)"] =
			append(surfaces["build args (baked into image metadata)"], k+"="+v)
	}
	for k, v := range inputs.secretValues {
		surfaces["BuildKit secrets (mounted into RUN)"] =
			append(surfaces["BuildKit secrets (mounted into RUN)"], k+"="+string(v))
	}
	return surfaces
}

func TestDeployEnvPreviewIsolation(t *testing.T) {
	approver := int64(3)
	preview := func(fork bool, approved bool) *store.Preview {
		p := &store.Preview{
			ID: 7, Uuid: mustUUID(t, deployrunPreviewUUID), PrID: 42, IsFork: fork,
		}
		if approved {
			p.ForkApprovedBy = &approver
		}
		return p
	}

	cases := []struct {
		name string
		// preview is the deployment being rendered; nil is production.
		preview *store.Preview
		// want are the values this deployment legitimately carries, forbidden
		// the values belonging to the other set.
		want, forbidden []string
	}{
		{
			name:    "production deployment reads the production set",
			preview: nil,
			want:    deployEnvProdValues, forbidden: deployEnvPreviewValues,
		},
		{
			name:    "base-repo preview reads its dedicated set only",
			preview: preview(false, false),
			want:    deployEnvPreviewValues, forbidden: deployEnvProdValues,
		},
		{
			name:    "fork preview reads its dedicated set only",
			preview: preview(true, false),
			want:    deployEnvPreviewValues, forbidden: deployEnvProdValues,
		},
		{
			// Approval lets a fork be BUILT; it does not hand it the
			// production secrets. This is the exploitable path: an approved
			// fork runs a PR-authored Dockerfile.
			name:    "approved fork preview still reads its dedicated set only",
			preview: preview(true, true),
			want:    deployEnvPreviewValues, forbidden: deployEnvProdValues,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := deployrunSetup(t, nil)
			deployEnvWorld(t, env)
			r := deployrunNewRun(env, deployrunDeployment(t), deployrunApp(t))
			r.preview = tc.preview

			surfaces := deployEnvSurfaces(t, r)
			for surface, entries := range surfaces {
				joined := strings.Join(entries, "\n")
				for _, leaked := range tc.forbidden {
					if strings.Contains(joined, leaked) {
						t.Errorf("INV-010: %q reached %s\n%s", leaked, surface, joined)
					}
				}
			}

			// The negative assertion alone would be satisfied by a renderer
			// that produced nothing at all, so pin that the right set DID come
			// through — on the union, since each value belongs to one surface.
			all := ""
			for _, entries := range surfaces {
				all += strings.Join(entries, "\n") + "\n"
			}
			for _, want := range tc.want {
				if !strings.Contains(all, want) {
					t.Errorf("expected value %q to be rendered, surfaces:\n%s", want, all)
				}
			}
		})
	}
}
