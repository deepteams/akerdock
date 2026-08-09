package jobs

// build.env is the one host file a build still needs: nixpacks drives the CLI
// on the machine and sources it (ADR-055 phase 2 note). It holds the build-time
// variables in plaintext, secrets included, so it must not outlive the build —
// and a build that FAILS is the case that matters, since that is when a
// deployment stops early and leaves the file where it fell.

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/deepteams/akerdock/internal/store"
)

// deployrunCommandLog records every command the scripted SSH server executes,
// so a test can assert what the deployment did to the host rather than what it
// meant to do.
type deployrunCommandLog struct {
	mu   sync.Mutex
	cmds []string
}

func (l *deployrunCommandLog) record(command string) {
	l.mu.Lock()
	l.cmds = append(l.cmds, command)
	l.mu.Unlock()
}

// matching returns the recorded commands containing all the given fragments.
func (l *deployrunCommandLog) matching(fragments ...string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, c := range l.cmds {
		all := true
		for _, f := range fragments {
			if !strings.Contains(c, f) {
				all = false
			}
		}
		if all {
			out = append(out, c)
		}
	}
	return out
}

func TestDeployrunNixpacksBuildEnvIsRemoved(t *testing.T) {
	cases := []struct {
		name string
		// failBuild makes the nixpacks build exit non-zero, standing in for
		// every way a build can end early.
		failBuild bool
	}{
		{name: "successful build"},
		{name: "failed build", failBuild: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deployrunFastTimers(t)
			log := &deployrunCommandLog{}
			env := deployrunSetup(t, func(command string) (string, uint32) {
				log.record(command)
				if tc.failBuild && strings.Contains(command, nixpacksBin+" build ") {
					return "nixpacks: build failed", 1
				}
				return jobCommandOutput(command), 0
			})
			deployrunProxyOutputs(env.rt, `{}`)
			app := deployrunApp(t)
			app.BuildConfig.BuildPack = store.BuildPackNixpacks
			app.Application.GitRepositoryUrl = ptr("https://example.test/acme/app.git")

			r := deployrunNewRun(env, deployrunDeployment(t), app)
			err := r.execute(context.Background())
			r.close()
			if tc.failBuild == (err == nil) {
				t.Fatalf("failBuild=%v but error = %v", tc.failBuild, err)
			}

			// The build did use the file — otherwise the removal below would
			// prove nothing about the path under test.
			if len(log.matching("env/build.env", "cat >")) == 0 {
				t.Fatal("the nixpacks path must write build.env")
			}
			if got := log.matching("rm -f", "env/build.env"); len(got) == 0 {
				t.Fatalf("build.env must be removed from the host; commands touching it: %v",
					log.matching("env/build.env"))
			}
		})
	}
}
