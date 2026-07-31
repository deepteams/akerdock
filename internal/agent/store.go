package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultDir is where the control plane deposits the routing table and the
// waker writes activity files (§8.1). Both sit under the server's AkerDock root.
const DefaultDir = "/var/lib/akerdock/waker"

// RoutesFile is the routing table filename inside the waker directory.
const RoutesFile = "routes.json"

// FileActivity records last-activity timestamps as one file per resource,
// written atomically (write-temp + rename) so the control plane's SSH read
// never sees a half-written value.
type FileActivity struct {
	Dir string
}

// ActivityPath is the per-resource activity file the control plane reads over
// SSH. The value is decimal Unix seconds — trivial to `cat` and parse remotely.
func ActivityPath(dir, uuid string) string {
	return filepath.Join(dir, uuid+".activity")
}

// Record writes the resource's last-activity timestamp atomically.
func (f FileActivity) Record(uuid string, at time.Time) error {
	if err := os.MkdirAll(f.Dir, 0o755); err != nil {
		return err
	}
	path := ActivityPath(f.Dir, uuid)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(at.Unix(), 10)), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ParseActivity parses the content of an activity file (decimal Unix seconds)
// into a time. The control plane uses it after reading the file over SSH.
func ParseActivity(content string) (time.Time, error) {
	sec, err := strconv.ParseInt(strings.TrimSpace(content), 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("waker: invalid activity value %q: %w", content, err)
	}
	return time.Unix(sec, 0), nil
}

// LoadConfig reads the routing table the control plane deposited.
func LoadConfig(dir string) (Config, error) {
	var cfg Config
	raw, err := os.ReadFile(filepath.Join(dir, RoutesFile))
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("waker: invalid routes file: %w", err)
	}
	return cfg, nil
}

// MarshalConfig renders a routing table for the control plane to deposit.
func MarshalConfig(cfg Config) ([]byte, error) {
	return json.MarshalIndent(cfg, "", "  ")
}
