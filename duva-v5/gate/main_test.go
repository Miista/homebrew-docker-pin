package gate

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --- config ------------------------------------------------------------------

func TestConfigDefaults(t *testing.T) {
	t.Setenv("DUVA_HOST", "")
	t.Setenv("DUVA_UPDATE_TIMEOUT", "")
	c := loadConfig()
	if c.ApplyTimeout != defaultApplyTimeout {
		t.Errorf("timeout = %v, want the default", c.ApplyTimeout)
	}
	// Unset must still name the host as something, or a UI over several
	// queues cannot tell them apart.
	if c.Host == "" {
		t.Error("host is empty; it should fall back to the OS hostname")
	}
}

func TestConfigReadsTheEnvironment(t *testing.T) {
	t.Setenv("DUVA_HOST", "pi")
	t.Setenv("DUVA_UPDATE_TIMEOUT", "5m")

	c := loadConfig()
	if c.Host != "pi" {
		t.Errorf("host = %q", c.Host)
	}
	if c.ApplyTimeout != 5*time.Minute {
		t.Errorf("timeout = %v, want 5m", c.ApplyTimeout)
	}
}

// --- the project path --------------------------------------------------------

func TestProjectFileFindsTheComposeFile(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, "docker-compose.yml")
	os.WriteFile(want, []byte("services: {}\n"), 0o644)

	old := composeDir
	composeDir = dir
	t.Cleanup(func() { composeDir = old })

	got, err := projectFile()
	if err != nil {
		t.Fatalf("projectFile: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// What is mounted is the project, so there is no subdirectory to honour and
// none to escape from. This stage reads compose files and touches no git, so
// it has no reason to see the repository around them -- only the updater
// mounts that, because committing needs it.
func TestTheProjectIsWhatIsMounted(t *testing.T) {
	dir := t.TempDir()
	// A repository-shaped tree: the project in a subdirectory, which this
	// stage must NOT go looking through.
	sub := filepath.Join(dir, "pi")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(sub, "docker-compose.yml"), []byte("services: {}\n"), 0o644)

	old := composeDir
	composeDir = dir
	t.Cleanup(func() { composeDir = old })

	if _, err := projectFile(); err == nil {
		t.Error("a project one directory down was found, so the mount is not the contract")
	}
}

// --- the token ----------------------------------------------------------------

// An injected token is used as given.
func TestAnInjectedTokenIsUsed(t *testing.T) {
	t.Setenv("DUVA_QUEUE_TOKEN", "s3cret")
	if got, err := tokenFromEnv(); err != nil || got != "s3cret" {
		t.Errorf("token = %q, want what was injected", got)
	}
}

// No token is a refusal to start, not a queue left open and not one with a
// token minted and printed to the log.
func TestNoTokenIsARefusal(t *testing.T) {
	t.Setenv("DUVA_QUEUE_TOKEN", "")
	if _, err := tokenFromEnv(); err == nil {
		t.Fatal("a queue with no token was allowed to start")
	}
}
