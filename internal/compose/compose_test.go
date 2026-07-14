package compose

import (
	"context"
	"strings"
	"testing"
	"time"
)

const stackUUID = "0b6f7f3a-1111-2222-3333-444455556666"

func load(t *testing.T, content string, mutate ...func(*Input)) *Result {
	t.Helper()
	in := Input{Content: content, StackUUID: stackUUID, Variables: map[string]string{}}
	for _, m := range mutate {
		m(&in)
	}
	res, err := Load(context.Background(), in)
	if err != nil {
		t.Fatalf("Load: internal error: %v", err)
	}
	return res
}

func hasFinding(res *Result, code string, severity Severity) bool {
	for _, f := range res.Findings {
		if f.Code == code && f.Severity == severity {
			return true
		}
	}
	return false
}

func mustPlan(t *testing.T, res *Result) *Plan {
	t.Helper()
	if res.HasErrors() {
		t.Fatalf("unexpected errors: %v", res.Findings)
	}
	if res.Plan == nil {
		t.Fatalf("no plan produced")
	}
	return res.Plan
}

func servicePlan(t *testing.T, plan *Plan, name string) ServicePlan {
	t.Helper()
	for _, sp := range plan.Services {
		if sp.Name == name {
			return sp
		}
	}
	t.Fatalf("service %q not in plan", name)
	return ServicePlan{}
}

const twoServices = `
services:
  web:
    image: ghcr.io/acme/web:1.2
    expose: ["3000"]
    depends_on:
      db:
        condition: service_healthy
    volumes:
      - data:/srv/data
  db:
    image: postgres:16
    healthcheck:
      test: ["CMD-SHELL", "pg_isready"]
      interval: 10s
      retries: 5
    volumes:
      - data:/var/lib/postgresql/data
volumes:
  data:
`

func TestPlanTwoServices(t *testing.T) {
	res := load(t, twoServices)
	plan := mustPlan(t, res)

	// Topological order: the dependency starts first (§2.6).
	if plan.Services[0].Name != "db" || plan.Services[1].Name != "web" {
		t.Fatalf("wrong start order: %v", []string{plan.Services[0].Name, plan.Services[1].Name})
	}

	web := servicePlan(t, plan, "web")
	if web.ContainerName != stackUUID+"-web" || web.CandidateName != stackUUID+"-web-next" {
		t.Fatalf("wrong container names: %+v", web)
	}
	if len(web.Aliases) != 2 || web.Aliases[0] != "web" || web.Aliases[1] != stackUUID+"-web" {
		t.Fatalf("wrong aliases: %v", web.Aliases)
	}
	if web.DefaultRoutePort != 3000 {
		t.Fatalf("expose must drive the default route port, got %d", web.DefaultRoutePort)
	}
	if web.Restart != "unless-stopped" {
		t.Fatalf("restart default not injected: %q", web.Restart)
	}
	if len(web.DependsOn) != 1 || web.DependsOn[0].Condition != "service_healthy" {
		t.Fatalf("depends_on not planned: %+v", web.DependsOn)
	}

	// The shared named volume is rewritten coherently for both services (§2.4).
	if plan.Volumes["data"] != stackUUID+"_data" {
		t.Fatalf("volume not prefixed: %v", plan.Volumes)
	}
	db := servicePlan(t, plan, "db")
	if db.Mounts[0].Source != stackUUID+"_data" || web.Mounts[0].Source != stackUUID+"_data" {
		t.Fatalf("volume references not rewritten: db=%v web=%v", db.Mounts, web.Mounts)
	}

	// Health flags with compose defaults filled (§7.1).
	if db.Health == nil || db.Health.Interval != 10*time.Second || db.Health.Retries != 5 || db.Health.Timeout != 30*time.Second {
		t.Fatalf("health flags wrong: %+v", db.Health)
	}

	// Database detection by image basename (§10).
	if !db.IsDatabase || db.DatabaseEngine != "postgresql" {
		t.Fatalf("postgres image not detected: %+v", db)
	}
	if web.IsDatabase {
		t.Fatalf("web wrongly classified as database")
	}
	if plan.Canonical == "" {
		t.Fatalf("canonical form missing")
	}
}

func TestIgnoredKeysAreWarnings(t *testing.T) {
	res := load(t, `
version: "3.8"
name: my-project
made_up_key: true
services:
  app:
    image: nginx
    container_name: my-app
    links: [db]
    made_up: 1
`)
	mustPlan(t, res)
	for _, expected := range []string{CodeVersionIgnored, CodeContainerNameIgnored} {
		if !hasFinding(res, expected, Warning) {
			t.Fatalf("missing warning %s: %v", expected, res.Findings)
		}
	}
	// name, made_up_key, links and made_up all funnel into compose_key_ignored.
	count := 0
	for _, f := range res.Findings {
		if f.Code == CodeKeyIgnored {
			count++
		}
	}
	if count != 4 {
		t.Fatalf("expected 4 ignored keys, got %d: %v", count, res.Findings)
	}
}

func TestIncludeIsRejected(t *testing.T) {
	res := load(t, `
include:
  - other.yaml
services:
  app:
    image: nginx
`)
	if !hasFinding(res, CodeIncludeRejected, Error) {
		t.Fatalf("include must be rejected: %v", res.Findings)
	}
	if res.Plan != nil {
		t.Fatalf("no plan on error")
	}
}

func TestRejectedKeys(t *testing.T) {
	cases := []struct {
		yaml string
		code string
	}{
		{"deploy:\n      replicas: 3", CodeSwarmKeyRejected},
		{"network_mode: host", CodeNetworkModeHostRejected},
		{"network_mode: service:db", CodeNetworkModeRejected},
		{"pid: host", CodeHostNamespaceRejected},
		{"isolation: hyperv", CodePlatformUnsupported},
		{"external_links: [legacy]", CodeSwarmKeyRejected},
	}
	for _, tc := range cases {
		res := load(t, "services:\n  app:\n    image: nginx\n    "+strings.ReplaceAll(tc.yaml, "\n", "\n"))
		if !hasFinding(res, tc.code, Error) {
			t.Fatalf("%q must produce %s: %v", tc.yaml, tc.code, res.Findings)
		}
	}
}

func TestPrivilegedPolicy(t *testing.T) {
	content := `
services:
  agent:
    image: acme/agent
    privileged: true
    cap_add: [SYS_ADMIN, CHOWN]
`
	res := load(t, content)
	if !hasFinding(res, CodePrivilegedDenied, Error) {
		t.Fatalf("privileged must be denied by default: %v", res.Findings)
	}

	res = load(t, content, func(in *Input) {
		in.Policy = Policy{AllowPrivileged: true, ExtraCapAdd: []string{"SYS_ADMIN"}}
	})
	if res.HasErrors() {
		t.Fatalf("server policy must allow it: %v", res.Findings)
	}
}

func TestDefaultCapAllowlist(t *testing.T) {
	res := load(t, `
services:
  app:
    image: nginx
    cap_add: [NET_BIND_SERVICE]
    cap_drop: [ALL]
`)
	if res.HasErrors() {
		t.Fatalf("allowlisted capability refused: %v", res.Findings)
	}
}

func TestBindMountPolicy(t *testing.T) {
	content := `
services:
  app:
    image: nginx
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
`
	res := load(t, content)
	if !hasFinding(res, CodeBindMountDenied, Error) {
		t.Fatalf("docker.sock bind must be denied: %v", res.Findings)
	}

	res = load(t, content, func(in *Input) {
		in.Policy = Policy{AllowedBindRoots: []string{"/var/run"}}
	})
	if res.HasErrors() {
		t.Fatalf("allowed root must permit the bind: %v", res.Findings)
	}

	res = load(t, `
services:
  app:
    image: nginx
    volumes:
      - ../../etc:/host-etc
`)
	if !hasFinding(res, CodePathTraversal, Error) {
		t.Fatalf("relative escape must be refused: %v", res.Findings)
	}
}

func TestDependencyRules(t *testing.T) {
	res := load(t, `
services:
  a:
    image: nginx
    depends_on: [b]
  b:
    image: nginx
    depends_on: [a]
`)
	if !hasFinding(res, CodeDependencyCycle, Error) {
		t.Fatalf("cycle must be refused: %v", res.Findings)
	}

	res = load(t, `
services:
  web:
    image: nginx
    depends_on:
      db:
        condition: service_healthy
  db:
    image: postgres
`)
	if !hasFinding(res, CodeDependencyNeedsHealthcheck, Error) {
		t.Fatalf("service_healthy without healthcheck must be refused: %v", res.Findings)
	}
}

func TestConflictingLimits(t *testing.T) {
	res := load(t, `
services:
  app:
    image: nginx
    mem_limit: 256m
    deploy:
      resources:
        limits:
          memory: 512m
`)
	if !hasFinding(res, CodeConflictingLimits, Error) {
		t.Fatalf("contradictory limits must be refused: %v", res.Findings)
	}
}

func TestLimitsNormalization(t *testing.T) {
	res := load(t, `
services:
  app:
    image: nginx
    deploy:
      resources:
        limits:
          memory: 512m
          cpus: "0.5"
          pids: 100
        reservations:
          memory: 128m
`)
	plan := mustPlan(t, res)
	limits := servicePlan(t, plan, "app").Limits
	if limits.Memory != 512<<20 || limits.MemoryReservation != 128<<20 || limits.CPUs != 0.5 || limits.Pids != 100 {
		t.Fatalf("limits not normalized: %+v", limits)
	}
}

func TestReservedLabels(t *testing.T) {
	res := load(t, `
services:
  app:
    image: nginx
    labels:
      akerdock.managed: "false"
`)
	if !hasFinding(res, CodeReservedLabel, Error) {
		t.Fatalf("akerdock.* label must be refused: %v", res.Findings)
	}
}

func TestServiceExtensions(t *testing.T) {
	res := load(t, `
services:
  migrate:
    image: acme/app
    restart: "no"
    x-akerdock:
      exclude_from_hc: true
  app:
    image: acme/app
    x-akerdock:
      zero_downtime: false
`)
	plan := mustPlan(t, res)
	migrate := servicePlan(t, plan, "migrate")
	if !migrate.OneShot || !migrate.ExcludeFromHC {
		t.Fatalf("one-shot extension not applied: %+v", migrate)
	}
	if hasFinding(res, CodeOneshotWithoutExclude, Warning) {
		t.Fatalf("excluded one-shot must not warn: %v", res.Findings)
	}
	if !servicePlan(t, plan, "app").ZeroDowntimeOptOut {
		t.Fatalf("zero_downtime: false not applied")
	}
}

func TestOneShotWithoutExcludeWarns(t *testing.T) {
	res := load(t, `
services:
  migrate:
    image: acme/app
    restart: "no"
`)
	mustPlan(t, res)
	if !hasFinding(res, CodeOneshotWithoutExclude, Warning) {
		t.Fatalf("one-shot without exclude must warn: %v", res.Findings)
	}
}

func TestStorageExtensionConflict(t *testing.T) {
	res := load(t, `
services:
  app:
    image: nginx
    volumes:
      - type: bind
        source: ./config
        target: /etc/app
        x-akerdock:
          is_directory: true
          content: |
            [config]
`)
	if !hasFinding(res, CodeStorageExtensionConflict, Error) {
		t.Fatalf("content + is_directory must conflict: %v", res.Findings)
	}
}

func TestInterpolation(t *testing.T) {
	content := `
services:
  app:
    image: acme/app:${TAG:-latest}
    environment:
      DB_URL: ${DATABASE_URL}
`
	res := load(t, content, func(in *Input) {
		in.Variables = map[string]string{"DATABASE_URL": "postgres://db:5432/app"}
	})
	plan := mustPlan(t, res)
	app := servicePlan(t, plan, "app")
	if app.Image != "acme/app:latest" {
		t.Fatalf("default interpolation failed: %q", app.Image)
	}
	if hasFinding(res, CodeVariableUndefined, Warning) {
		t.Fatalf("defined variable must not warn: %v", res.Findings)
	}

	// Undefined without default: warning, empty value (§3.1).
	res = load(t, content)
	if !hasFinding(res, CodeVariableUndefined, Warning) {
		t.Fatalf("undefined variable must warn: %v", res.Findings)
	}
}

func TestRequiredVariableMissing(t *testing.T) {
	res := load(t, `
services:
  app:
    image: acme/app
    environment:
      SECRET: ${APP_SECRET:?APP_SECRET is required}
`)
	if !hasFinding(res, CodeRequiredVariableMissing, Error) {
		t.Fatalf("required variable miss must block: %v", res.Findings)
	}
}

func TestExternalObjectsPolicy(t *testing.T) {
	content := `
services:
  app:
    image: nginx
    networks: [shared]
networks:
  shared:
    external: true
`
	res := load(t, content)
	if !hasFinding(res, CodeExternalObjectRejected, Error) {
		t.Fatalf("external network must be refused by default: %v", res.Findings)
	}
	res = load(t, content, func(in *Input) { in.Policy = Policy{AllowExternalObjects: true} })
	if res.HasErrors() {
		t.Fatalf("policy must allow external objects: %v", res.Findings)
	}
}

func TestRawModeSkipsRenaming(t *testing.T) {
	res := load(t, twoServices, func(in *Input) { in.Raw = true })
	plan := mustPlan(t, res)
	if plan.Volumes["data"] != "data" {
		t.Fatalf("raw mode must not prefix volumes: %v", plan.Volumes)
	}
	db := servicePlan(t, plan, "db")
	if db.Restart != "" {
		t.Fatalf("raw mode must not inject a restart policy: %q", db.Restart)
	}
}

func TestHostPortsMakeServiceIneligible(t *testing.T) {
	res := load(t, `
services:
  app:
    image: nginx
    ports:
      - "8080:80"
`)
	plan := mustPlan(t, res)
	if !servicePlan(t, plan, "app").HasHostPorts {
		t.Fatalf("host port mapping not detected")
	}
}

func TestDetectDatabase(t *testing.T) {
	cases := map[string]string{
		"postgres:16":                          "postgresql",
		"bitnami/postgresql:15":                "postgresql",
		"supabase/postgres:15.1":               "postgresql",
		"pgvector/pgvector:pg16":               "postgresql",
		"mysql@sha256:abcd":                    "mysql",
		"mariadb:11":                           "mariadb",
		"mongodb/mongodb-community-server:7.0": "mongodb",
		"nginx:alpine":                         "",
		"":                                     "",
	}
	for image, engine := range cases {
		isDB, got := detectDatabase(image)
		if got != engine || isDB != (engine != "") {
			t.Fatalf("detectDatabase(%q) = %v/%q, want %q", image, isDB, got, engine)
		}
	}
}

func TestInvalidServiceName(t *testing.T) {
	res := load(t, `
services:
  MyApp:
    image: nginx
`)
	if !hasFinding(res, CodeInvalidServiceName, Error) {
		t.Fatalf("invalid service name must be refused: %v", res.Findings)
	}
}

// An extension key written at the SERVICE level (rather than under
// x-akerdock) is not ours: it is dropped like any unknown key, with a
// warning — the operator sees that it had no effect.
func TestServiceLevelExtensionKeyIsIgnored(t *testing.T) {
	res := load(t, `
services:
  migrate:
    image: acme/app
    restart: "no"
    exclude_from_hc: true
`)
	mustPlan(t, res)
	if !hasFinding(res, CodeKeyIgnored, Warning) {
		t.Fatalf("an unknown service key must warn: %v", res.Findings)
	}
	// It did NOT act as x-akerdock.exclude_from_hc: the one-shot still warns.
	if !hasFinding(res, CodeOneshotWithoutExclude, Warning) {
		t.Fatalf("a service-level key must not be honored as an extension: %v", res.Findings)
	}
	if servicePlan(t, res.Plan, "migrate").ExcludeFromHC {
		t.Fatalf("a service-level key must not exclude the service from health")
	}
}

func TestExternalVolumesMountedVerbatim(t *testing.T) {
	// Adoption (§20.7): an external volume — the form the adoption rewrite
	// produces — is mounted under its REAL docker name and never created or
	// prefixed. Prefixing it would silently remount an empty volume. The
	// policy gate stays: without AllowExternalObjects this file is refused.
	allow := func(in *Input) { in.Policy.AllowExternalObjects = true }
	res := load(t, `
services:
  db:
    image: postgres:16
    volumes:
      - pinned:/var/lib/postgresql/data
      - plain:/plain
volumes:
  pinned:
    external: true
    name: legacy_pgdata
  plain:
`, allow)
	plan := mustPlan(t, res)
	if _, created := plan.Volumes["pinned"]; created {
		t.Fatalf("an external volume must not be scheduled for creation: %v", plan.Volumes)
	}
	if got := plan.ExternalVolumes["pinned"]; got != "legacy_pgdata" {
		t.Fatalf("external volume docker name = %q, want legacy_pgdata", got)
	}
	if got := plan.Volumes["plain"]; got != stackUUID+"_plain" {
		t.Fatalf("a plain volume keeps the uuid prefix, got %q", got)
	}
	db := servicePlan(t, plan, "db")
	var pinned, plain string
	for _, m := range db.Mounts {
		switch m.Target {
		case "/var/lib/postgresql/data":
			pinned = m.Source
		case "/plain":
			plain = m.Source
		}
	}
	if pinned != "legacy_pgdata" {
		t.Fatalf("external mount source = %q, want legacy_pgdata", pinned)
	}
	if plain != stackUUID+"_plain" {
		t.Fatalf("plain mount source = %q", plain)
	}
}
