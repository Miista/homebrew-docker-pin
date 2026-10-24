// Package settings is duva's settings, read through configy -- the same way
// every app here reads them.
//
// Every setting is DUVA_SETTING_<KEY> (upper-cased, "." as "_"), read from the
// environment and nowhere else. A value that does not parse is an error, never
// a quiet fallback: a typo in a switch must not mean "off".
//
// Environment only, with no stored fallback, because nothing edits settings
// from the page: a store behind them would be a second place a value could
// come from that no one can see. It also means a secret never lands in the
// database, and that nothing has to wait for the database to open.
package settings

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/miista/configy"
)

// Prefix is configy's: a setting a.b is DUVA_SETTING_A_B.
const Prefix = "DUVA"

// The settings, and what each is when unset.
const (
	Repo       = "repo"        // required: the repository, at its path on the host
	ComposeDir = "compose_dir" // where in Repo the compose file is; default its root
	Host       = "host"        // this host's name; default the hostname
	Mode       = "mode"        // hub or agent; default agent
	APIToken   = "api_token"   // required: what the hub, or the CLI, presents to the API
	Agents     = "agents"      // a hub's agents, "host=url,host=url"

	OIDCIssuer       = "ui.oidc.issuer"
	OIDCClientID     = "ui.oidc.client_id"
	OIDCClientSecret = "ui.oidc.client_secret" // secret
	OIDCBaseURL      = "ui.oidc.base_url"

	LogLevel          = "log_level"          // default info
	Since             = "since"              // override the next check's cutoff, once
	Schedule          = "schedule"           // 5-field cron; default DefaultSchedule
	ReconcileInterval = "reconcile_interval" // default 10m
	UpdateTimeout     = "update_timeout"     // default 30m
	GitPush           = "git.push"           // default false
	CommitTemplate    = "commit_template"    // default the updater's
	NtfyEndpoint      = "ntfy.endpoint"
	NtfyTopic         = "ntfy.topic"
	NtfyToken         = "ntfy.token" // secret
	NtfyClick         = "ntfy.click"
	ReadOnly          = "read_only" // default false
)

// The modes. A hub serves the page, over its own queue and its agents'; an
// agent serves only its queue's API, for the hub to reach. Agents never reach
// the hub, or each other.
const (
	ModeHub   = "hub"
	ModeAgent = "agent"
)

// HubOnly is the settings an agent refuses: set on one, they would be read
// by nothing, and a host meant to be the hub would run without its page.
var HubOnly = []string{Agents, OIDCIssuer, OIDCClientID, OIDCClientSecret, OIDCBaseURL}

// ModeOf is this host's mode, checked: an unknown mode is an error, and so
// is a hub-only setting on an agent.
func ModeOf() (string, error) {
	mode := Value(Mode)
	switch mode {
	case "":
		mode = ModeAgent
	case ModeHub, ModeAgent:
	default:
		return "", fmt.Errorf("%s: unknown mode %q (want hub or agent)", EnvVar(Mode), mode)
	}
	if mode == ModeAgent {
		var set []string
		for _, key := range HubOnly {
			if Value(key) != "" {
				set = append(set, EnvVar(key))
			}
		}
		if len(set) > 0 {
			return "", fmt.Errorf("this host is an agent (%s), and %s are a hub's: set %s=hub, or remove them",
				EnvVar(Mode), strings.Join(set, ", "), EnvVar(Mode))
		}
	}
	return mode, nil
}

// AgentToken is the setting holding an agent's API token: agent.<host>.token,
// DUVA_SETTING_AGENT_<HOST>_TOKEN. Per agent, never in the Agents list, so
// the list is not a secret and can sit in a compose file in git.
//
// A host name may hold "-" or ".", which cannot appear in a variable's name,
// so anything but a letter, digit or "_" is written as "_".
func AgentToken(host string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(host) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return "agent." + b.String() + ".token"
}

// none is configy's lookup: there is no store, so nothing is ever found.
func none(string) (string, bool) { return "", false }

func config() *configy.Config { return configy.New(Prefix, none) }

// EnvVar is the variable that sets key, for messages.
func EnvVar(key string) string { return config().EnvVar(key) }

// String is a string setting, def when unset.
func String(key, def string) (string, error) {
	s, err := config().Resolve[string](key)
	if err != nil {
		return "", err
	}
	if s.Source == configy.SourceUnset {
		return def, nil
	}
	return s.Value, nil
}

// Value is a string setting, empty when unset. A string always parses, so it
// cannot fail.
func Value(key string) string {
	v, _ := String(key, "")
	return v
}

// Duration is a duration setting ("10m", "7d"), def when unset.
func Duration(key string, def time.Duration) (time.Duration, error) {
	s, err := config().Resolve[time.Duration](key)
	if err != nil {
		return 0, err
	}
	if s.Source == configy.SourceUnset {
		return def, nil
	}
	return s.Value, nil
}

// Bool is a boolean setting, def when unset.
func Bool(key string, def bool) (bool, error) {
	s, err := config().Resolve[bool](key)
	if err != nil {
		return false, err
	}
	if s.Source == configy.SourceUnset {
		return def, nil
	}
	return s.Value, nil
}

// IsSet reports whether key is set, without parsing it.
func IsSet(key string) bool { return config().Source(key) != configy.SourceUnset }

// HostName is this host's name: the host setting, else the machine's
// hostname.
func HostName() string {
	if h := Value(Host); h != "" {
		return h
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown-host"
}

// Renamed is each v4 and early-v5 variable that is now a setting, and the
// name it has now.
var Renamed = map[string]string{
	"DUVA_REPO":                  Repo,
	"DUVA_COMPOSE_DIR":           ComposeDir,
	"DUVA_HOST":                  Host,
	"DUVA_UI":                    Mode,
	"DUVA_QUEUE_TOKEN":           APIToken,
	"DUVA_QUEUES":                Agents,
	"DUVA_SETTING_UI":            Mode,
	"DUVA_SETTING_PEERS":         Agents,
	"DUVA_UI_OIDC_ISSUER":        OIDCIssuer,
	"DUVA_UI_OIDC_CLIENT_ID":     OIDCClientID,
	"DUVA_UI_OIDC_CLIENT_SECRET": OIDCClientSecret,
	"DUVA_UI_OIDC_BASE_URL":      OIDCBaseURL,
	"DUVA_LOG_LEVEL":             LogLevel,
	"DUVA_SINCE":                 Since,
	"DUVA_SCHEDULE":              Schedule,
	"DUVA_RECONCILE_INTERVAL":    ReconcileInterval,
	"DUVA_UPDATE_TIMEOUT":        UpdateTimeout,
	"DUVA_GIT_PUSH":              GitPush,
	"DUVA_COMMIT_TEMPLATE":       CommitTemplate,
	"DUVA_NTFY_ENDPOINT":         NtfyEndpoint,
	"DUVA_NTFY_TOPIC":            NtfyTopic,
	"NTFY_TOKEN":                 NtfyToken,
	"DUVA_NTFY_CLICK":            NtfyClick,
	"DUVA_READ_ONLY":             ReadOnly,
}

// oldAgentTokens are the per-host token variables before agent.<host>.token:
// v4's DUVA_QUEUE_TOKEN_<HOST>, and early v5's DUVA_SETTING_PEER_<HOST>_TOKEN.
var oldAgentTokens = []struct{ prefix, suffix string }{
	{"DUVA_QUEUE_TOKEN_", ""},
	{"DUVA_SETTING_PEER_", "_TOKEN"},
}

// Gone is each variable that weakened the sign-in gate, which v5 does not
// have under any name: sign-in is OIDC, always.
var Gone = []string{"DUVA_UI_AUTH_DISABLED", "DUVA_UI_LOCAL_ENABLED", "DUVA_UI_AUTH_PUBLIC_PATHS"}

// CheckRenamed refuses a variable that was renamed or removed, given the
// environment as os.Environ has it. Read under its old name it would be
// silently ignored, and duva would run on a default nobody chose: a
// carried-over configuration that pushed commits would quietly stop, and one
// that turned the sign-in off would be read as having one.
func CheckRenamed(environ []string) error {
	var found []string
	for _, kv := range environ {
		name, value, _ := strings.Cut(kv, "=")
		if value == "" {
			continue
		}
		if key, ok := Renamed[name]; ok {
			found = append(found, name+" is now "+EnvVar(key))
		} else {
			for _, old := range oldAgentTokens {
				host, ok := strings.CutPrefix(name, old.prefix)
				if host, ok2 := strings.CutSuffix(host, old.suffix); ok && ok2 && host != "" {
					found = append(found, name+" is now "+EnvVar(AgentToken(host)))
				}
			}
		}
		for _, gone := range Gone {
			if name == gone {
				found = append(found, name+" is gone: sign-in is OIDC, always")
			}
		}
	}
	if len(found) == 0 {
		return nil
	}
	sort.Strings(found)
	return fmt.Errorf("settings have been renamed: %s", strings.Join(found, "; "))
}
