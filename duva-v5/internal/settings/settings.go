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
	UI         = "ui"          // serve the page too; default false
	APIToken   = "api_token"   // required: what a peer, or the CLI, presents to the API
	Peers      = "peers"       // the other hosts, "host=url,host=url"

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

// PeerToken is the setting holding a peer's API token: peer.<host>.token,
// DUVA_SETTING_PEER_<HOST>_TOKEN. Per peer, never in the Peers list, so the
// list is not a secret and can sit in a compose file in git.
//
// A host name may hold "-" or ".", which cannot appear in a variable's name,
// so anything but a letter, digit or "_" is written as "_".
func PeerToken(host string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(host) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return "peer." + b.String() + ".token"
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
	"DUVA_UI":                    UI,
	"DUVA_QUEUE_TOKEN":           APIToken,
	"DUVA_QUEUES":                Peers,
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

// oldPeerToken is v4's per-host token variable, DUVA_QUEUE_TOKEN_<HOST>.
const oldPeerToken = "DUVA_QUEUE_TOKEN_"

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
		} else if host, ok := strings.CutPrefix(name, oldPeerToken); ok && host != "" {
			found = append(found, name+" is now "+EnvVar(PeerToken(host)))
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
