package settings

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miista/configy"

	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/store"
)

func withStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "duva.db"))
	if err != nil {
		t.Fatal(err)
	}
	Use(s)
	t.Cleanup(func() { Use(nil); s.Close() })
	return s
}

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
	if got := EnvVar(NtfyEndpoint); got != "DUVA_SETTING_NTFY_ENDPOINT" {
		t.Errorf("EnvVar = %s", got)
	}
}

// The store is used when the environment says nothing, and the environment
// wins when it does.
func TestTheEnvironmentWinsOverTheStore(t *testing.T) {
	s := withStore(t)
	s.DB().Exec(`INSERT INTO settings (key, value) VALUES ('schedule', '0 4 * * *')`)
	if got, _ := String(Schedule, "0 3 * * *"); got != "0 4 * * *" {
		t.Errorf("from the store = %q", got)
	}
	t.Setenv("DUVA_SETTING_SCHEDULE", "0 5 * * *")
	if got, _ := String(Schedule, "0 3 * * *"); got != "0 5 * * *" {
		t.Errorf("with the env set = %q, want the env", got)
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
	env := map[string]string{"DUVA_GIT_PUSH": "true"}
	err := CheckRenamed(func(k string) string { return env[k] })
	if err == nil || !strings.Contains(err.Error(), "DUVA_SETTING_GIT_PUSH") {
		t.Errorf("err = %v, want one naming DUVA_SETTING_GIT_PUSH", err)
	}
	if err := CheckRenamed(func(string) string { return "" }); err != nil {
		t.Errorf("nothing old set = %v", err)
	}
}
