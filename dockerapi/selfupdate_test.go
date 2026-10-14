package dockerapi

import (
	"encoding/json"
	"testing"
)

// What the updater's own container looks like on optiplex, trimmed to the
// fields the helper's spec depends on.
const updaterInspect = `{
	"Image": "sha256:eb53a7c3c4227543bb9ed4cb3a125586dd11c3019dbb144e12ae4fa75f360984",
	"Config": {
		"Hostname": "ffebe044a58d",
		"User": "1000:1000",
		"Image": "ghcr.io/miista/duva-update:v7.9@sha256:eb53a7c3c4227543bb9ed4cb3a125586dd11c3019dbb144e12ae4fa75f360984",
		"Cmd": null,
		"Env": ["DUVA_REPO=/home/guldmund/docker", "DUVA_UPDATE_TOKEN=t"],
		"Labels": {
			"com.docker.compose.project": "optiplex",
			"com.docker.compose.service": "duva-update",
			"com.docker.compose.container-number": "1",
			"com.docker.compose.config-hash": "abc",
			"duva.auto": "none"
		}
	},
	"HostConfig": {
		"Binds": ["/var/run/docker.sock:/var/run/docker.sock:rw", "/home/guldmund/docker:/home/guldmund/docker:rw"],
		"GroupAdd": ["989"],
		"RestartPolicy": {"Name": "unless-stopped"},
		"PortBindings": {"8080/tcp": [{"HostPort": "8080"}]}
	},
	"NetworkSettings": {"Networks": {"optiplex_default": {
		"Aliases": ["duva-update", "duva-update"],
		"DNSNames": ["duva-update", "ffebe044a58d"],
		"IPAddress": "172.18.0.16",
		"NetworkID": "68f9189c"
	}}}
}`

func helperFromUpdater(t *testing.T) map[string]any {
	t.Helper()
	var spec containerSpec
	if err := json.Unmarshal([]byte(updaterInspect), &spec); err != nil {
		t.Fatal(err)
	}
	return helperSpec(spec, "sha256:eb53a7c3c4227543bb9ed4cb3a125586dd11c3019dbb144e12ae4fa75f360984",
		[]string{"self-update", "abc", "{}"})
}

// A helper that still carried the service label would be a container
// findContainer could return for duva-update -- and the helper would stop
// itself, which is the failure it exists to avoid.
func TestAHelperIsNotTheServiceItWasCopiedFrom(t *testing.T) {
	labels := helperFromUpdater(t)["Labels"].(map[string]any)

	for _, gone := range []string{
		"com.docker.compose.service",
		"com.docker.compose.container-number",
		"com.docker.compose.config-hash",
	} {
		if _, ok := labels[gone]; ok {
			t.Errorf("the helper still carries %s", gone)
		}
	}
	// The project stays: it is how the helper knows where to recreate.
	if labels["com.docker.compose.project"] != "optiplex" {
		t.Errorf("project label = %v, want optiplex", labels["com.docker.compose.project"])
	}
	if labels[SelfUpdateLabel] != "true" {
		t.Errorf("the helper is not marked as one: %v", labels)
	}
	if labels["duva.auto"] != "none" {
		t.Error("a label that is not compose's was dropped")
	}
}

// The queue addresses the updater as duva-update. A helper answering to that
// name too would take requests meant for the updater.
func TestAHelperDoesNotAnswerToTheUpdatersName(t *testing.T) {
	cfg := helperFromUpdater(t)
	endpoints := cfg["NetworkingConfig"].(map[string]any)["EndpointsConfig"].(map[string]any)

	ep, ok := endpoints["optiplex_default"].(map[string]any)
	if !ok {
		t.Fatalf("the helper is not on the updater's network: %v", endpoints)
	}
	if len(ep) != 0 {
		t.Errorf("the helper carries the updater's endpoint settings: %v", ep)
	}
	if _, ok := cfg["Hostname"]; ok {
		t.Error("the helper took the updater's hostname")
	}
}

// Everything the job needs comes across; what would make it run twice or
// collide does not.
func TestAHelperIsAOneShotCopy(t *testing.T) {
	cfg := helperFromUpdater(t)
	host := cfg["HostConfig"].(map[string]any)

	if cfg["Image"] != "sha256:eb53a7c3c4227543bb9ed4cb3a125586dd11c3019dbb144e12ae4fa75f360984" {
		t.Errorf("image = %v, want the updater's image id", cfg["Image"])
	}
	if cmd, _ := cfg["Cmd"].([]string); len(cmd) == 0 || cmd[0] != "self-update" {
		t.Errorf("cmd = %v", cfg["Cmd"])
	}
	if cfg["User"] != "1000:1000" {
		t.Errorf("user = %v, want the updater's", cfg["User"])
	}
	if binds := toStringsAny(host["Binds"]); len(binds) != 2 {
		t.Errorf("binds = %v, want the socket and the repository", host["Binds"])
	}
	if rp := host["RestartPolicy"].(map[string]any); rp["Name"] != "no" {
		t.Errorf("restart policy = %v, want no", rp)
	}
	if host["AutoRemove"] != false {
		t.Error("a failed helper would take its log with it")
	}
	if _, ok := host["PortBindings"]; ok {
		t.Error("the helper would collide with the updater's published port")
	}
}

func toStringsAny(v any) []string {
	switch b := v.(type) {
	case []string:
		return b
	default:
		return toStrings(v)
	}
}
