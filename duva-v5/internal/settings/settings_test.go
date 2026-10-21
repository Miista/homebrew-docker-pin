package settings

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/miista/configy"
)

func TestUnsetIsTheDefault(t *testing.T) {
	if d, err := Duration(ReconcileInterval, 10*time.Minute); err != nil || d != 10*time.Minute {
		t.Errorf("= %v %v, want the default", d, err)
	}
}

func TestTheEnvironmentIsReadUnderConfigysName(t *testing.T) {
	t.Setenv("DUVA_SETTING_GIT_PUSH", "true")
	if on, err := Bool(GitPush, false); err != nil || !on {
		t.Errorf("= %v %v, want true from DUVA_SETTING_GIT_PUSH", on, err)
	}
	for key, want := range map[string]string{
		NtfyEndpoint:     "DUVA_SETTING_NTFY_ENDPOINT",
		Repo:             "DUVA_SETTING_REPO",
		APIToken:         "DUVA_SETTING_API_TOKEN",
		OIDCClientSecret: "DUVA_SETTING_UI_OIDC_CLIENT_SECRET",
	} {
		if got := EnvVar(key); got != want {
			t.Errorf("EnvVar(%s) = %s, want %s", key, got, want)
		}
	}
}

// A peer's token is its own variable, and a host name that cannot be part of
// one is written so it can.
func TestAPeersTokenIsNamedForTheHost(t *testing.T) {
	for host, want := range map[string]string{
		"pi":               "DUVA_SETTING_PEER_PI_TOKEN",
		"raspberry.local":  "DUVA_SETTING_PEER_RASPBERRY_LOCAL_TOKEN",
		"family-dashboard": "DUVA_SETTING_PEER_FAMILY_DASHBOARD_TOKEN",
	} {
		if got := EnvVar(PeerToken(host)); got != want {
			t.Errorf("%s: %s, want %s", host, got, want)
		}
	}
	t.Setenv("DUVA_SETTING_PEER_PI_TOKEN", "just-the-pi")
	if got := Value(PeerToken("pi")); got != "just-the-pi" {
		t.Errorf("= %q", got)
	}
}

// A typo is an error, not a quiet "off".
func TestAnInvalidValueIsAnError(t *testing.T) {
	t.Setenv("DUVA_SETTING_READ_ONLY", "ture")
	if _, err := Bool(ReadOnly, false); !errors.Is(err, configy.ErrInvalid) {
		t.Errorf("err = %v, want configy.ErrInvalid", err)
	}
}

// An old name is refused, naming the new one, rather than silently ignored.
func TestARenamedVariableIsRefused(t *testing.T) {
	for old, want := range map[string]string{
		"DUVA_GIT_PUSH":         "DUVA_SETTING_GIT_PUSH",
		"DUVA_REPO":             "DUVA_SETTING_REPO",
		"DUVA_QUEUES":           "DUVA_SETTING_PEERS",
		"DUVA_QUEUE_TOKEN":      "DUVA_SETTING_API_TOKEN",
		"DUVA_QUEUE_TOKEN_PI":   "DUVA_SETTING_PEER_PI_TOKEN",
		"DUVA_UI_OIDC_ISSUER":   "DUVA_SETTING_UI_OIDC_ISSUER",
		"DUVA_UI_AUTH_DISABLED": "OIDC, always",
		"DUVA_UI_LOCAL_ENABLED": "OIDC, always",
	} {
		err := CheckRenamed([]string{old + "=x"})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want one naming %s", old, err, want)
		}
	}
	if err := CheckRenamed([]string{"DUVA_SETTING_REPO=/srv", "DUVA_GIT_PUSH=", "PATH=/bin"}); err != nil {
		t.Errorf("nothing old set = %v", err)
	}
}
