package jobs

// Byte-exact goldens for the FROZEN v1 compose rendering (ADR-053).
//
// WHAT BREAKING THESE MEANS, AND WHY YOU MUST NOT JUST UPDATE THE CONSTANTS.
//
// The v1 fingerprint is stored in an immutable label on every compose container
// created before the v2 rollout. It is how a deployment decides "this container
// already matches its configuration, leave it running". The renderer is not
// merely stable by habit: its exact bytes are the identity of containers that
// are running right now. Reorder one flag, add one space, and every one of
// those containers gets a new hash, matches nothing, and is RECREATED — a
// platform-wide restart of workloads nobody asked to touch.
//
// So these constants are not "the current output". They are the output that
// already exists in the field. If a change makes this test fail:
//
//   - the honest fix is almost always to revert the change to the frozen
//     renderer — it is frozen, that is the whole point;
//   - if the rendering genuinely must change, it does so as a NEW hash version
//     alongside the old one (that is what v2 is), never by editing v1 in place;
//   - updating the constant to match the new output silently converts a caught
//     regression into a mass recreation. That is the failure this file exists
//     to prevent.
//
// The property tests next door (determinism, sensitivity to env and image)
// cannot catch any of this: they recompute what they expect from the renderer
// itself, so the renderer always agrees with them.

import (
	"strings"
	"testing"
)

// composeFrozenYAML is deliberately rich — ports, named volume, resource
// limits, healthcheck, user, entrypoint, command, several labels — so that the
// golden covers the flag ORDER of the whole command, not just its presence.
const composeFrozenYAML = `
services:
  web:
    image: ghcr.io/acme/web:1.2
    command: ["serve", "--port", "3000"]
    entrypoint: ["/bin/app"]
    user: "1000:1000"
    environment:
      DB_URL: "postgres://x"
      APP_MODE: "prod"
    labels:
      app.custom: "yes"
      zz.last: "1"
    ports:
      - "8080:80"
    volumes:
      - data:/srv/data
    deploy:
      resources:
        limits:
          memory: 512m
          cpus: "0.5"
    healthcheck:
      test: ["CMD-SHELL", "curl -fsS http://localhost:3000/health"]
      interval: 10s
volumes:
  data:
`

const composeFrozenAppDir = "/var/lib/akerdock/applications/" + composeTestUUID

// The exact command the frozen renderer produces for composeFrozenYAML.
const composeFrozenCommand = ". /var/lib/akerdock/applications/11112222-3333-4444-5555-666677778888/env/runtime.sh; " +
	". /env/web.sh; " +
	"docker stop -t 30 11112222-3333-4444-5555-666677778888-web >/dev/null 2>&1; " +
	"docker rm -f 11112222-3333-4444-5555-666677778888-web >/dev/null 2>&1; " +
	"docker create --name 11112222-3333-4444-5555-666677778888-web " +
	"--restart unless-stopped " +
	"--network 11112222-3333-4444-5555-666677778888 " +
	"--network-alias web " +
	"--network-alias 11112222-3333-4444-5555-666677778888-web " +
	"--label akerdock.managed=true " +
	"--label akerdock.component=web " +
	"--label 'app.custom=yes' " +
	"--label 'zz.last=1' " +
	"-v '11112222-3333-4444-5555-666677778888_data:/srv/data' " +
	"-p '8080:80' " +
	"--memory 536870912 " +
	"--cpus 0.5 " +
	"--health-cmd 'curl -fsS http://localhost:3000/health' " +
	"--health-interval 10s " +
	"--health-timeout 30s " +
	"--health-retries 3 " +
	"--health-start-period 0s " +
	"--user '1000:1000' " +
	"--entrypoint '/bin/app' " +
	"-e DB_URL " +
	"ghcr.io/acme/web:1.2 'serve' '--port' '3000' >/dev/null"

// The v1 environment file content, the hash's second input.
const composeFrozenEnvFile = "export APP_MODE='prod'\nexport DB_URL='postgres://x'\n"

// The fingerprints themselves — the values living in container labels today.
const (
	composeFrozenHashV1 = "a1612cb8c7b4"
	composeFrozenHashV2 = "2:61530378d47c"
)

// The frozen preview-seeding rendering, a pure hash input like the rest.
const composeFrozenSeedScript = "if docker volume inspect src >/dev/null 2>&1; then " +
	"docker run --rm --user 0 --entrypoint /bin/sh " +
	"-v src:/akerdock-seed-from:ro -v dst:/akerdock-volume img:1 " +
	`-c '[ -n "$(ls -A /akerdock-volume)" ] || cp -a /akerdock-seed-from/. /akerdock-volume/'` +
	"; fi"

func TestComposeFrozenV1RenderingIsByteExact(t *testing.T) {
	plan := loadPlan(t, composeFrozenYAML)
	r := &deploymentRun{}
	sp := plan.Services[0]

	got := r.composeCreateCommand(plan, sp, composeFrozenAppDir, "--label akerdock.managed=true",
		"/env/web.sh", []string{"DB_URL"}, "ghcr.io/acme/web:1.2",
		composeCreateOpts{Name: sp.ContainerName, Aliases: sp.Aliases, ReplaceOld: true})
	if got != composeFrozenCommand {
		t.Errorf("the frozen v1 renderer changed — every pre-v2 container would be recreated.\n got: %s\nwant: %s\nfirst difference at byte %d",
			got, composeFrozenCommand, composeFirstDifference(got, composeFrozenCommand))
	}

	_, _, envContent := composeServiceEnv(sp)
	if envContent != composeFrozenEnvFile {
		t.Errorf("the frozen v1 environment rendering changed.\n got: %q\nwant: %q", envContent, composeFrozenEnvFile)
	}
}

// The hash is pinned as a VALUE, not recomputed from the renderer: a hash
// derived from the code under test agrees with any change that code makes.
func TestComposeFrozenHashesArePinnedValues(t *testing.T) {
	plan := loadPlan(t, composeFrozenYAML)
	r := &deploymentRun{}
	sp := plan.Services[0]

	v1 := composeConfigHash(
		r.composeCreateCommand(plan, sp, composeFrozenAppDir, "--label akerdock.managed=true",
			"/env/web.sh", []string{"DB_URL"}, "ghcr.io/acme/web:1.2",
			composeCreateOpts{Name: sp.ContainerName, Aliases: sp.Aliases, ReplaceOld: true}),
		composeFrozenEnvFile)
	if v1 != composeFrozenHashV1 {
		t.Errorf("v1 fingerprint = %q, want %q — pre-v2 containers would all be recreated", v1, composeFrozenHashV1)
	}

	v2 := composeConfigHashV2(buildComposeCreateSpec(plan, sp, composeFrozenAppDir, nil,
		[]string{"DB_URL=postgres://x"}, "ghcr.io/acme/web:1.2",
		composeCreateOpts{Name: sp.ContainerName, Aliases: sp.Aliases}))
	if v2 != composeFrozenHashV2 {
		t.Errorf("v2 fingerprint = %q, want %q — every compose container would be recreated", v2, composeFrozenHashV2)
	}
}

func TestComposeFrozenSeedScriptIsByteExact(t *testing.T) {
	got := previewSeedScript("img:1", [][2]string{{"src", "dst"}})
	if got != composeFrozenSeedScript {
		t.Errorf("the frozen seeding rendering changed.\n got: %s\nwant: %s", got, composeFrozenSeedScript)
	}
}

// composeFirstDifference points at the byte where two renderings diverge, so a
// failure reads as "this flag moved" instead of two walls of text.
func composeFirstDifference(got, want string) int {
	n := min(len(got), len(want))
	for i := range n {
		if got[i] != want[i] {
			return i
		}
	}
	if len(got) != len(want) {
		return n
	}
	return -1
}

// Guard on the guard: the goldens must actually be reachable by the renderer.
// A YAML the loader silently drops would make every assertion above compare
// two empty strings.
func TestComposeFrozenFixtureIsNotEmpty(t *testing.T) {
	plan := loadPlan(t, composeFrozenYAML)
	if len(plan.Services) != 1 {
		t.Fatalf("fixture services = %d, want exactly the one the goldens describe", len(plan.Services))
	}
	if !strings.Contains(composeFrozenCommand, "docker create") {
		t.Fatal("the golden command no longer looks like a create command")
	}
}
