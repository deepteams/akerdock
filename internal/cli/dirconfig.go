package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// dirConfigName is the per-repository config file (spec §4): base CLI defaults
// for a working directory, so commands need no repeated --context/--team/target
// flags (e.g. `akerdock logs` from a repo already knows its instance and app).
const dirConfigName = ".akerdock"

// Environment overrides, one per resolvable setting (spec §4). AKERDOCK_CONTEXT
// is defined in config.go and reused here.
const (
	envTeam        = "AKERDOCK_TEAM"
	envProject     = "AKERDOCK_PROJECT"
	envApplication = "AKERDOCK_APPLICATION"
	envEnvironment = "AKERDOCK_ENVIRONMENT"
	envComponent   = "AKERDOCK_COMPONENT"
)

// DirConfig is a .akerdock file: non-secret CLI defaults for a directory tree.
// It never holds tokens — those stay in ~/.akerdock/credentials.yaml — so it is
// safe to commit. Every field is optional; an empty one falls through to the
// global config (spec §4).
type DirConfig struct {
	Context     string `yaml:"context,omitempty"`     // a global context name (instance + token)
	Team        string `yaml:"team,omitempty"`        // team uuid or name, overrides the context's
	Project     string `yaml:"project,omitempty"`     // default project
	Application string `yaml:"application,omitempty"` // default application (target)
	Environment string `yaml:"environment,omitempty"` // default environment
	Component   string `yaml:"component,omitempty"`   // default component (e.g. a compose service)
}

// loadDirConfig finds the nearest .akerdock walking up from the working
// directory to the filesystem root — like git's .git — and parses it. A
// DIRECTORY named .akerdock (the global ~/.akerdock) is skipped: only a file
// counts. Returns an empty config when none is found.
func loadDirConfig() (*DirConfig, error) {
	dir, err := os.Getwd()
	if err != nil {
		return &DirConfig{}, nil //nolint:nilerr // no working dir → no directory config, not a failure.
	}
	for {
		path := filepath.Join(dir, dirConfigName)
		if info, statErr := os.Stat(path); statErr == nil && !info.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			dc := &DirConfig{}
			if err := yaml.Unmarshal(data, dc); err != nil {
				return nil, fmt.Errorf("invalid %s at %s: %w", dirConfigName, path, err)
			}
			return dc, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return &DirConfig{}, nil // reached the filesystem root
		}
		dir = parent
	}
}

// Settings is the fully-resolved CLI configuration for one invocation, layering
// (highest priority first) flags, AKERDOCK_* env, the nearest .akerdock, then
// the global ~/.akerdock config (spec §4).
type Settings struct {
	ContextName string
	Team        string
	Project     string
	Application string
	Environment string
	Component   string
}

// resolveSettings merges the four layers. The context's own team_uuid is the
// last-resort team, so a plain login still targets its default team.
func resolveSettings(f globalFlags, cfg *Config, dir *DirConfig) *Settings {
	ctxName := firstNonEmpty(f.context, os.Getenv(envContext), dir.Context, cfg.CurrentContext)
	team := firstNonEmpty(f.team, os.Getenv(envTeam), dir.Team)
	if team == "" {
		if ctx, ok := cfg.Contexts[ctxName]; ok {
			team = ctx.TeamUUID
		}
	}
	return &Settings{
		ContextName: ctxName,
		Team:        team,
		Project:     firstNonEmpty(f.project, os.Getenv(envProject), dir.Project),
		Application: firstNonEmpty(f.application, os.Getenv(envApplication), dir.Application),
		Environment: firstNonEmpty(f.environment, os.Getenv(envEnvironment), dir.Environment),
		Component:   firstNonEmpty(f.component, os.Getenv(envComponent), dir.Component),
	}
}

// settings loads both config layers and resolves them against the current
// flags. Exposed so any command can read the effective defaults (target app,
// component…) without re-implementing the precedence.
func settings() (*Settings, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return nil, err
	}
	dir, err := loadDirConfig()
	if err != nil {
		return nil, err
	}
	return resolveSettings(flags, cfg, dir), nil
}
