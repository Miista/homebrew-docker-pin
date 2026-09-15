package main

import "testing"

// Where the compose project is mounted is configurable, because it has to be
// able to match the host's own path.
//
// Only this stage. A relative bind in the compose file resolves against this
// directory, and the daemon then has to find that path on the host -- so with
// the two differing there is no single answer, and with them equal both are
// the same string. The watcher and the queue only read the file, so neither
// needs this.
func TestTheComposeDirectoryComesFromTheEnvironment(t *testing.T) {
	t.Setenv("DUVA_UPDATE_COMPOSE_DIR", "/home/guldmund/docker")
	if got := composeDirFromEnv(); got != "/home/guldmund/docker" {
		t.Errorf("composeDir = %q, want the configured one", got)
	}
}

// Unset is /compose, which is what every existing deployment mounts.
func TestAnUnsetComposeDirectoryIsCompose(t *testing.T) {
	t.Setenv("DUVA_UPDATE_COMPOSE_DIR", "")
	if got := composeDirFromEnv(); got != "/compose" {
		t.Errorf("composeDir = %q, want /compose", got)
	}
}
