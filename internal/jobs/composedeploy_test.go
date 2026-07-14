package jobs

import (
	"context"
	"strings"
	"testing"

	"github.com/deepteams/akerdock/internal/compose"
)

const composeTestUUID = "11112222-3333-4444-5555-666677778888"

func loadPlan(t *testing.T, content string) *compose.Plan {
	t.Helper()
	res, err := compose.Load(context.Background(), compose.Input{
		Content: content, StackUUID: composeTestUUID,
		Variables: map[string]string{},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.HasErrors() || res.Plan == nil {
		t.Fatalf("unexpected findings: %v", res.Findings)
	}
	return res.Plan
}

func TestComposeCreateCommand(t *testing.T) {
	plan := loadPlan(t, `
services:
  web:
    image: ghcr.io/acme/web:1.2
    command: ["serve", "--port", "3000"]
    entrypoint: ["/bin/app"]
    user: "1000:1000"
    labels:
      app.custom: "yes"
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
`)
	r := &deploymentRun{}
	sp := plan.Services[0]
	cmd := r.composeCreateCommand(plan, sp, "/data/akerdock/applications/"+composeTestUUID, "--label akerdock.managed=true", "/env/web.sh", []string{"DB_URL"}, "ghcr.io/acme/web:1.2",
		composeCreateOpts{Name: sp.ContainerName, Aliases: sp.Aliases, ReplaceOld: true})

	for _, want := range []string{
		"--name " + composeTestUUID + "-web",
		"--restart unless-stopped",
		"--network " + composeTestUUID,
		"--network-alias web",
		"--network-alias " + composeTestUUID + "-web",
		"--label akerdock.component=web",
		"--label 'app.custom=yes'",
		"-v '" + composeTestUUID + "_data:/srv/data'",
		"-p '8080:80'",
		"--memory 536870912",
		"--cpus 0.5",
		"--health-cmd 'curl -fsS http://localhost:3000/health'",
		"--health-interval 10s",
		"--user '1000:1000'",
		"--entrypoint '/bin/app'",
		"-e DB_URL",
		". /env/web.sh;",
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("create command missing %q:\n%s", want, cmd)
		}
	}
	// The command args come after the image reference, individually quoted.
	if !strings.Contains(cmd, "ghcr.io/acme/web:1.2 'serve' '--port' '3000'") {
		t.Fatalf("command args not appended after the image:\n%s", cmd)
	}
	// Values never travel inline: only -e KEY references (INV-003/012).
	if strings.Contains(cmd, "-e DB_URL=") {
		t.Fatalf("environment value leaked into argv:\n%s", cmd)
	}
}

func TestComposeCreateCommandOneShot(t *testing.T) {
	plan := loadPlan(t, `
services:
  migrate:
    image: acme/app:1.0
    restart: "no"
    x-akerdock:
      exclude_from_hc: true
`)
	r := &deploymentRun{}
	cmd := r.composeCreateCommand(plan, plan.Services[0], "/data", "--label x=y", "/env/m.sh", nil, "acme/app:1.0",
		composeCreateOpts{Name: plan.Services[0].ContainerName, Aliases: plan.Services[0].Aliases})
	if !strings.Contains(cmd, "--restart no") {
		t.Fatalf("one-shot restart policy lost:\n%s", cmd)
	}
}

func TestComposeCreateCommandQuotesHostileValues(t *testing.T) {
	plan := loadPlan(t, `
services:
  app:
    image: nginx
    labels:
      note: "a'; rm -rf / #"
`)
	r := &deploymentRun{}
	cmd := r.composeCreateCommand(plan, plan.Services[0], "/data", "", "/env/a.sh", nil, "nginx",
		composeCreateOpts{Name: plan.Services[0].ContainerName, Aliases: plan.Services[0].Aliases})
	// The hostile label value must be inside a shell-quoted literal.
	if !strings.Contains(cmd, `--label 'note=a'\''; rm -rf / #'`) {
		t.Fatalf("hostile label not quoted:\n%s", cmd)
	}
}

func TestZeroDowntimeEligibility(t *testing.T) {
	plan := loadPlan(t, `
services:
  web:
    image: nginx
    healthcheck:
      test: ["CMD-SHELL", "curl -f localhost"]
  ported:
    image: nginx
    ports: ["8080:80"]
    healthcheck:
      test: ["CMD-SHELL", "curl -f localhost"]
  optout:
    image: nginx
    healthcheck:
      test: ["CMD-SHELL", "curl -f localhost"]
    x-akerdock:
      zero_downtime: false
  nohealth:
    image: nginx
`)
	byName := map[string]compose.ServicePlan{}
	for _, sp := range plan.Services {
		byName[sp.Name] = sp
	}

	if ok, _ := zeroDowntimeEligibility(byName["web"], false, false); !ok {
		t.Fatalf("healthy web service must be eligible")
	}
	if ok, reason := zeroDowntimeEligibility(byName["web"], true, false); ok || !strings.Contains(reason, "raw") {
		t.Fatalf("raw mode must be ineligible: %v %q", ok, reason)
	}
	if ok, reason := zeroDowntimeEligibility(byName["ported"], false, false); ok || !strings.Contains(reason, "port") {
		t.Fatalf("host ports must be ineligible: %v %q", ok, reason)
	}
	if ok, reason := zeroDowntimeEligibility(byName["optout"], false, false); ok || !strings.Contains(reason, "zero_downtime") {
		t.Fatalf("opt-out must be ineligible: %v %q", ok, reason)
	}
	if ok, reason := zeroDowntimeEligibility(byName["nohealth"], false, false); ok || !strings.Contains(reason, "healthcheck") {
		t.Fatalf("no healthcheck must be ineligible: %v %q", ok, reason)
	}
	// An image HEALTHCHECK resolves the health requirement (§7.1 priority).
	if ok, _ := zeroDowntimeEligibility(byName["nohealth"], false, true); !ok {
		t.Fatalf("image healthcheck must make the service eligible")
	}
}

func TestComposeConfigHash(t *testing.T) {
	a := composeConfigHash("docker create --name x nginx", "export A='1'")
	b := composeConfigHash("docker create --name x nginx", "export A='1'")
	if a != b || len(a) != 12 {
		t.Fatalf("hash must be deterministic and short: %q vs %q", a, b)
	}
	if composeConfigHash("docker create --name x nginx", "export A='2'") == a {
		t.Fatalf("an environment change must change the hash")
	}
	if composeConfigHash("docker create --name x nginx:1.29", "export A='1'") == a {
		t.Fatalf("an image change must change the hash")
	}
}

func TestZeroDowntimeCandidateCommandKeepsShortAliasOff(t *testing.T) {
	plan := loadPlan(t, `
services:
  web:
    image: nginx
    healthcheck:
      test: ["CMD-SHELL", "curl -f localhost"]
`)
	r := &deploymentRun{}
	sp := plan.Services[0]
	cmd := r.composeCreateCommand(plan, sp, "/data", "", "/env/web.sh", nil, "nginx",
		composeCreateOpts{Name: sp.CandidateName, Aliases: []string{sp.CandidateName}})
	// §8.3: the candidate must NOT carry the short service alias — the other
	// services keep resolving it to the old container until promotion.
	if strings.Contains(cmd, "--network-alias web ") || strings.Contains(cmd, "--network-alias web\n") {
		t.Fatalf("candidate must not carry the short alias:\n%s", cmd)
	}
	if !strings.Contains(cmd, "--name "+sp.CandidateName) || !strings.Contains(cmd, "--network-alias "+sp.CandidateName) {
		t.Fatalf("candidate naming wrong:\n%s", cmd)
	}
	if strings.Contains(cmd, "docker stop") || strings.Contains(cmd, "docker rm") {
		t.Fatalf("candidate creation must not touch the old container:\n%s", cmd)
	}
}
