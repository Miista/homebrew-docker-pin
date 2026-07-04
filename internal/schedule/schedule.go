// Package schedule reads pin.yaml (the declarative schedule config living
// next to the compose file) and generates the systemd service/timer unit
// pair that runs `docker pin schedule run`. Unit generation is pure so it
// can be tested on any OS; only installing them requires systemd.
package schedule

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Miista/homebrew-docker-pin/internal/croncal"
)

// Config is the parsed pin.yaml.
type Config struct {
	// Schedule is a 5-field cron expression. Required.
	Schedule string `yaml:"schedule"`
	// Services to upgrade; empty means every service in the compose file.
	Services []string `yaml:"services"`
	// OnChange is run via `sh -c` in the compose dir after upgrades
	// changed the compose file. Optional.
	OnChange string `yaml:"on_change"`
}

// FindFile returns the pin.yaml (or pin.yml) sitting next to composeFile.
func FindFile(composeFile string) (string, error) {
	dir := filepath.Dir(composeFile)
	for _, name := range []string{"pin.yaml", "pin.yml"} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no pin.yaml found next to %s", composeFile)
}

// Load parses and minimally validates a pin.yaml.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if strings.TrimSpace(cfg.Schedule) == "" {
		return nil, fmt.Errorf("%s: 'schedule' is required", path)
	}
	return &cfg, nil
}

var slugCleanRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slug derives a unit-name-safe identifier from the compose directory:
// its basename plus a short hash of the full path to avoid collisions.
func Slug(composeDir string) string {
	base := slugCleanRe.ReplaceAllString(strings.ToLower(filepath.Base(composeDir)), "-")
	base = strings.Trim(base, "-")
	if base == "" {
		base = "compose"
	}
	sum := sha256.Sum256([]byte(composeDir))
	return fmt.Sprintf("%s-%x", base, sum[:4])
}

// UnitNames returns the service and timer unit file names for composeDir.
func UnitNames(composeDir string) (service, timer string) {
	slug := Slug(composeDir)
	return "docker-pin-" + slug + ".service", "docker-pin-" + slug + ".timer"
}

// Units renders the systemd service and timer unit contents.
// execPath is the absolute path of the docker-pin binary and user the
// account the service runs as.
func Units(cfg *Config, composeDir, execPath, user string) (service, timer string, err error) {
	onCal, err := croncal.Translate(cfg.Schedule)
	if err != nil {
		return "", "", err
	}
	service = fmt.Sprintf(`[Unit]
Description=docker pin scheduled upgrade for %s
Wants=docker.service
After=docker.service network-online.target

[Service]
Type=oneshot
User=%s
WorkingDirectory=%s
ExecStart=%s schedule run
`, composeDir, user, composeDir, execPath)
	timer = fmt.Sprintf(`[Unit]
Description=docker pin schedule timer for %s

[Timer]
OnCalendar=%s
Persistent=true

[Install]
WantedBy=timers.target
`, composeDir, onCal)
	return service, timer, nil
}
