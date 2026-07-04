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
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Miista/homebrew-docker-pin/internal/croncal"
)

// Config is the parsed pin.yaml.
type Config struct {
	// Schedule is a 5-field cron expression. Required.
	Schedule string `yaml:"schedule"`
	// Services to upgrade; empty means every service in the compose file.
	Services []Service `yaml:"services"`
	// OnChange is run via `sh -c` in the compose dir after upgrades
	// changed the compose file. Optional.
	OnChange string `yaml:"on_change"`
	// Notify configures where `schedule run` reports applied upgrades
	// and failures. Optional.
	Notify *Notify `yaml:"notify"`
	// Hostname identifies this box in notification titles (several hosts
	// can share one ntfy topic). Optional; defaults to the OS hostname.
	Hostname string `yaml:"hostname"`
}

// Service is one entry under `services:` — either a bare name or a mapping
// with a tag constraint:
//
//	services:
//	  - cloudflared              # unconstrained (variant-safe moving tag)
//	  - name: paperless-db
//	    tags: '^17\.\d+-alpine$' # only tags matching this regex
type Service struct {
	Name string `yaml:"name"`
	// Tags restricts upgrades to registry tags matching this regex. The
	// newest matching tag (numeric version order) newer than the current
	// one is chosen; when nothing matches, the service is left untouched
	// rather than falling back to a moving tag.
	Tags string `yaml:"tags"`
	// Exclude drops tags matching this regex from the candidates (e.g.
	// '(alpha|beta|rc)'). Requires Tags.
	Exclude string `yaml:"exclude"`
	// Delay only upgrades to a tag that has been published for at least
	// this long ("48h", "7d", "2w"), skipping fresher candidates in favor
	// of the newest sufficiently aged one. Requires Tags.
	Delay string `yaml:"delay"`
}

// UnmarshalYAML accepts both `- name` and `- {name: ..., tags: ...}` forms.
func (s *Service) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		s.Name = value.Value
		return nil
	}
	type plain Service
	var p plain
	if err := value.Decode(&p); err != nil {
		return err
	}
	*s = Service(p)
	return nil
}

// ServiceNames returns just the names of the configured services.
func (c *Config) ServiceNames() []string {
	names := make([]string, len(c.Services))
	for i, s := range c.Services {
		names[i] = s.Name
	}
	return names
}

// Notify is the `notify:` block of pin.yaml.
type Notify struct {
	Ntfy *Ntfy `yaml:"ntfy"`
}

// Ntfy describes an ntfy.sh-compatible notification target. The access token
// is never stored in pin.yaml: it is read from the environment variable named
// by token_env (default NTFY_TOKEN), optionally sourced from a KEY=VALUE file
// named by token_file first.
type Ntfy struct {
	URL      string `yaml:"url"`
	Topic    string `yaml:"topic"`
	TokenEnv string `yaml:"token_env"`
	// TokenFile is a KEY=VALUE file (e.g. an env_file) to read the token
	// from; the key looked up is TokenEnv. Optional — without it the
	// process environment is used.
	TokenFile string `yaml:"token_file"`
}

// Token resolves the ntfy access token. An empty return means unauthenticated.
func (n *Ntfy) Token() (string, error) {
	key := n.TokenEnv
	if key == "" {
		key = "NTFY_TOKEN"
	}
	if n.TokenFile != "" {
		data, err := os.ReadFile(n.TokenFile)
		if err != nil {
			return "", fmt.Errorf("notify token_file: %w", err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, key+"=") {
				return strings.TrimPrefix(line, key+"="), nil
			}
		}
		return "", fmt.Errorf("notify token_file %s has no %s= line", n.TokenFile, key)
	}
	return os.Getenv(key), nil
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
	for _, s := range cfg.Services {
		if strings.TrimSpace(s.Name) == "" {
			return nil, fmt.Errorf("%s: service entry with empty name", path)
		}
		if s.Tags != "" {
			if _, err := regexp.Compile(s.Tags); err != nil {
				return nil, fmt.Errorf("%s: service %s: invalid tags regex: %v", path, s.Name, err)
			}
		}
		if s.Exclude != "" {
			if s.Tags == "" {
				return nil, fmt.Errorf("%s: service %s: 'exclude' requires 'tags'", path, s.Name)
			}
			if _, err := regexp.Compile(s.Exclude); err != nil {
				return nil, fmt.Errorf("%s: service %s: invalid exclude regex: %v", path, s.Name, err)
			}
		}
		if s.Delay != "" {
			if s.Tags == "" {
				return nil, fmt.Errorf("%s: service %s: 'delay' requires 'tags'", path, s.Name)
			}
			if _, err := ParseDelay(s.Delay); err != nil {
				return nil, fmt.Errorf("%s: service %s: %v", path, s.Name, err)
			}
		}
	}
	if cfg.Notify != nil && cfg.Notify.Ntfy != nil {
		n := cfg.Notify.Ntfy
		if strings.TrimSpace(n.URL) == "" || strings.TrimSpace(n.Topic) == "" {
			return nil, fmt.Errorf("%s: notify.ntfy requires both 'url' and 'topic'", path)
		}
	}
	return &cfg, nil
}

var delayRe = regexp.MustCompile(`^(\d+)(h|d|w)$`)

// ParseDelay parses a release-age delay: "48h", "7d" or "2w".
func ParseDelay(s string) (time.Duration, error) {
	m := delayRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("invalid delay %q (use e.g. \"48h\", \"7d\", \"2w\")", s)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, err
	}
	unit := time.Hour
	switch m[2] {
	case "d":
		unit = 24 * time.Hour
	case "w":
		unit = 7 * 24 * time.Hour
	}
	return time.Duration(n) * unit, nil
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
