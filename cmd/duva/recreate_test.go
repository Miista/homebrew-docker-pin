package main

import (
	"strings"
	"testing"
)

// createSpec is where a recreate can quietly lose things: it builds the
// replacement container from the old one's configuration, and anything it
// fails to carry over is simply gone. The daemon is not involved, so these are
// ordinary unit tests.

func spec() containerSpec {
	return containerSpec{
		Config: map[string]any{
			"Image": "app:1.0.0",
			"Env":   []any{"TZ=Europe/Copenhagen"},
			"Labels": map[string]any{
				"com.docker.compose.project": "stack",
				"com.docker.compose.service": "db",
			},
		},
		HostConfig: map[string]any{
			"Binds":         []any{"/srv/stack/conf:/etc/app:ro"},
			"RestartPolicy": map[string]any{"Name": "unless-stopped"},
		},
	}
}

func TestCreateSpec_SwapsOnlyTheImage(t *testing.T) {
	got := createSpec(spec(), "app:1.0.1")

	if got["Image"] != "app:1.0.1" {
		t.Errorf("image not swapped: %v", got["Image"])
	}
	// Everything else has to survive, or the replacement is a different
	// container wearing the same name.
	if _, ok := got["Env"]; !ok {
		t.Error("environment dropped")
	}
	host := got["HostConfig"].(map[string]any)
	if _, ok := host["RestartPolicy"]; !ok {
		t.Error("restart policy dropped")
	}
	labels := got["Labels"].(map[string]any)
	if labels["com.docker.compose.project"] != "stack" {
		t.Error("compose labels dropped -- compose would stop managing this container")
	}
}

// Anonymous volumes appear only in Mounts. A replacement built from Config
// alone gets a fresh empty one, so the service comes back up healthy and
// empty -- the quietest way to lose a database.
func TestCreateSpec_CarriesAnonymousVolumes(t *testing.T) {
	s := spec()
	s.Config["Volumes"] = map[string]any{"/var/lib/postgresql/data": map[string]any{}}
	s.Mounts = append(s.Mounts, struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	}{Type: "volume", Name: "d82bf304c9bc", Destination: "/var/lib/postgresql/data", RW: true})

	got := createSpec(s, "app:1.0.1")

	binds := got["HostConfig"].(map[string]any)["Binds"].([]string)
	var found bool
	for _, b := range binds {
		if strings.HasPrefix(b, "d82bf304c9bc:/var/lib/postgresql/data") {
			found = true
		}
	}
	if !found {
		t.Errorf("the anonymous volume was not carried over: %v", binds)
	}

	// The image's VOLUME line also lists the path in Config.Volumes. Left
	// there alongside the bind, the daemon rejects the create outright:
	// "Duplicate mount point".
	if vols := got["Volumes"].(map[string]any); len(vols) != 0 {
		t.Errorf("Config.Volumes should no longer claim a bound path: %v", vols)
	}
}

// A named volume is already in Binds. Carrying it again would be the same
// duplicate the daemon rejects.
func TestCreateSpec_DoesNotDoubleBindAVolumeAlreadyInBinds(t *testing.T) {
	s := spec()
	s.HostConfig["Binds"] = []any{"pgdata:/var/lib/postgresql/data:rw"}
	s.Mounts = append(s.Mounts, struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	}{Type: "volume", Name: "pgdata", Destination: "/var/lib/postgresql/data", RW: true})

	got := createSpec(s, "app:1.0.1")

	binds := got["HostConfig"].(map[string]any)["Binds"].([]string)
	if len(binds) != 1 {
		t.Errorf("expected the one existing bind, got %v", binds)
	}
}

// A read-only mount has to come back read-only.
func TestCreateSpec_PreservesMountMode(t *testing.T) {
	s := spec()
	s.Mounts = append(s.Mounts, struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	}{Type: "volume", Name: "refdata", Destination: "/ref", RW: false})

	got := createSpec(s, "app:1.0.1")

	binds := got["HostConfig"].(map[string]any)["Binds"].([]string)
	var found bool
	for _, b := range binds {
		if b == "refdata:/ref:ro" {
			found = true
		}
	}
	if !found {
		t.Errorf("a read-only volume came back writable: %v", binds)
	}
}

// Bind mounts are already absolute host paths in Mounts and need no carrying:
// they are in Binds. Treating them like volumes would bind the host path to
// itself under a volume name.
func TestCreateSpec_IgnoresBindMounts(t *testing.T) {
	s := spec()
	s.Mounts = append(s.Mounts, struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	}{Type: "bind", Name: "", Destination: "/etc/app", RW: true})

	got := createSpec(s, "app:1.0.1")

	binds := got["HostConfig"].(map[string]any)["Binds"].([]string)
	if len(binds) != 1 || !strings.HasPrefix(binds[0], "/srv/stack/conf:") {
		t.Errorf("bind mounts should be left alone: %v", binds)
	}
}

// Network endpoints carry each network's aliases; without them compose's
// service-to-service DNS stops resolving this container by name.
func TestCreateSpec_KeepsNetworkEndpoints(t *testing.T) {
	s := spec()
	s.NetworkSettings.Networks = map[string]any{
		"stack_default": map[string]any{"Aliases": []any{"db"}},
	}

	got := createSpec(s, "app:1.0.1")

	nets := got["NetworkingConfig"].(map[string]any)["EndpointsConfig"].(map[string]any)
	if _, ok := nets["stack_default"]; !ok {
		t.Errorf("network endpoints dropped: %v", nets)
	}
}
