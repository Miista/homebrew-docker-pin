// Package settings is duva's settings, read through configy -- the same way
// every app here reads them.
//
// A setting is read from DUVA_SETTING_<KEY> (upper-cased, "." as "_") when that
// is set, and otherwise from the store's settings table. The environment
// always wins, and a value that does not parse is an error, never a quiet
// fallback: a typo in a switch must not mean "off".
//
// Not everything is a setting. What duva needs before its store exists, or
// what would lock people out if edited from behind the sign-in, stays a plain
// environment variable: DUVA_REPO, DUVA_COMPOSE_DIR, DUVA_HOST, DUVA_UI, the
// DUVA_UI_OIDC_* authy reads, and the queue tokens and list.
package settings

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/miista/configy"

	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/store"
)

// Prefix is configy's: a setting a.b is DUVA_SETTING_A_B.
const Prefix = "DUVA"

// The settings, and what each is when unset.
const (
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

var (
	mu sync.RWMutex
	st *store.Store
)

// Use reads stored settings from s. Until it is called -- in tests, and in
// the self-update helper -- only the environment is read.
func Use(s *store.Store) {
	mu.Lock()
	defer mu.Unlock()
	st = s
}

// lookup is configy's view of the store, read on every call.
func lookup(key string) (string, bool) {
	mu.RLock()
	s := st
	mu.RUnlock()
	if s == nil {
		return "", false
	}
	var value string
	if err := s.DB().QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value); err != nil {
		return "", false
	}
	return value, true
}

func config() *configy.Config { return configy.New(Prefix, lookup) }

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

// IsSet reports whether key is set anywhere, without parsing it.
func IsSet(key string) bool { return config().Source(key) != configy.SourceUnset }

// Renamed is each v4 and early-v5 variable that is now a setting, and the
// name it has now.
var Renamed = map[string]string{
	"DUVA_LOG_LEVEL":          LogLevel,
	"DUVA_SINCE":              Since,
	"DUVA_SCHEDULE":           Schedule,
	"DUVA_RECONCILE_INTERVAL": ReconcileInterval,
	"DUVA_UPDATE_TIMEOUT":     UpdateTimeout,
	"DUVA_GIT_PUSH":           GitPush,
	"DUVA_COMMIT_TEMPLATE":    CommitTemplate,
	"DUVA_NTFY_ENDPOINT":      NtfyEndpoint,
	"DUVA_NTFY_TOPIC":         NtfyTopic,
	"NTFY_TOKEN":              NtfyToken,
	"DUVA_NTFY_CLICK":         NtfyClick,
	"DUVA_READ_ONLY":          ReadOnly,
}

// CheckRenamed refuses a variable that was renamed. Read under its old name
// it would be silently ignored, and duva would run on a default nobody chose:
// a carried-over configuration that pushed commits would quietly stop.
func CheckRenamed(getenv func(string) string) error {
	var found []string
	for old, key := range Renamed {
		if getenv(old) != "" {
			found = append(found, old+" is now "+EnvVar(key))
		}
	}
	if len(found) == 0 {
		return nil
	}
	sort.Strings(found)
	return fmt.Errorf("settings have been renamed: %s", strings.Join(found, "; "))
}
