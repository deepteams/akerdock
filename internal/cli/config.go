// Package cli implements the local `akerdock` client subcommands (ADR-031,
// ADR-032, ADR-033, spec docs/specs/cli.md). It talks only to the manager
// over HTTPS and never opens a listening network port.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config and credentials live under ~/.akerdock (dir 0700). They are split so
// the config can be inspected or shared without leaking tokens (spec §4).
const (
	configDirName   = ".akerdock"
	configFileName  = "config.yaml"
	credsFileName   = "credentials.yaml"
	dirMode         = 0o700
	fileMode        = 0o600
	envContext      = "AKERDOCK_CONTEXT"
	defaultCtxLabel = "default"
)

// Context is one instance + active team the client talks to.
type Context struct {
	URL      string `yaml:"url"`                 // base URL, e.g. https://manager.ad.kedric.fr
	Fqdn     string `yaml:"fqdn,omitempty"`      // convenience copy of the host
	TeamUUID string `yaml:"team_uuid,omitempty"` // active team; empty = server default
}

// Config is the non-secret part of the client state.
type Config struct {
	CurrentContext string             `yaml:"current_context"`
	Contexts       map[string]Context `yaml:"contexts"`
}

// credentials maps a context name to its bearer token (kept 0600, separate file).
type credentials struct {
	Tokens map[string]string `yaml:"tokens"`
}

func configDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate the home directory: %w", err)
	}
	return filepath.Join(home, configDirName), nil
}

// LoadConfig reads ~/.akerdock/config.yaml, returning an empty config when it
// does not exist yet.
func LoadConfig() (*Config, error) {
	dir, err := configDir()
	if err != nil {
		return nil, err
	}
	cfg := &Config{Contexts: map[string]Context{}}
	data, err := os.ReadFile(filepath.Join(dir, configFileName))
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", configFileName, err)
	}
	if cfg.Contexts == nil {
		cfg.Contexts = map[string]Context{}
	}
	return cfg, nil
}

// Save writes config and credentials back with strict permissions.
func (c *Config) Save() error {
	dir, err := configDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, configFileName), data, fileMode)
}

func loadCredentials() (*credentials, error) {
	dir, err := configDir()
	if err != nil {
		return nil, err
	}
	creds := &credentials{Tokens: map[string]string{}}
	data, err := os.ReadFile(filepath.Join(dir, credsFileName))
	if os.IsNotExist(err) {
		return creds, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(data, creds); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", credsFileName, err)
	}
	if creds.Tokens == nil {
		creds.Tokens = map[string]string{}
	}
	return creds, nil
}

func (cr *credentials) save() error {
	dir, err := configDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	data, err := yaml.Marshal(cr)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, credsFileName), data, fileMode)
}

// setToken stores (or clears) the token of a context in the 0600 file.
func setToken(context, token string) error {
	creds, err := loadCredentials()
	if err != nil {
		return err
	}
	if token == "" {
		delete(creds.Tokens, context)
	} else {
		creds.Tokens[context] = token
	}
	return creds.save()
}

// resolveContextName picks the context: --context flag, then $AKERDOCK_CONTEXT,
// then the config's current_context.
func (c *Config) resolveContextName(flag string) string {
	if flag != "" {
		return flag
	}
	if env := strings.TrimSpace(os.Getenv(envContext)); env != "" {
		return env
	}
	return c.CurrentContext
}
