package dockerapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// An updater cannot recreate its own container. Recreate stops the old
// container before creating the replacement, and when the old container is the
// one doing the recreating, the stop is the last thing it does: the replacement
// is never created and the host is left without an updater. Both hosts lost
// theirs that way on 2026-10-14.
//
// So the updater hands the work to a helper: a one-shot container built from
// its own, running its own image, which recreates the updater the ordinary way
// because to the helper the updater is just another container.

// SelfUpdateLabel marks a helper, so one left behind by a failure can be found.
const SelfUpdateLabel = "duva.self-update"

// Self returns the id of the container this process runs in.
func Self() (string, error) {
	return containerID()
}

// ServiceContainer returns the id of the running container for a service in
// duva's own compose project.
func ServiceContainer(service string) (string, error) {
	return findContainer(service)
}

// StartHelper starts a container that is this one with a different name and
// command, and returns its id.
//
// Built from this container rather than from configuration, because what the
// helper needs is exactly what this container already has: the user that owns
// the repository, the docker group, the socket, the repository at its host
// path, the token, the commit identity and the networks the queue reaches it
// on. Restating any of that somewhere else would be a second copy that could
// drift from the first.
//
// It runs the image this container runs -- by id, so it is the exact image and
// not whatever a tag points at now -- because that image's recreate is the one
// that has already worked on this host.
func StartHelper(name string, cmd []string) (string, error) {
	self, err := Self()
	if err != nil {
		return "", err
	}
	raw, err := dockerDo(http.MethodGet, "/containers/"+self+"/json", nil)
	if err != nil {
		return "", fmt.Errorf("inspecting this container: %w", err)
	}
	var spec containerSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return "", fmt.Errorf("reading this container's configuration: %w", err)
	}
	var image struct {
		Image string `json:"Image"`
	}
	if err := json.Unmarshal(raw, &image); err != nil || image.Image == "" {
		return "", fmt.Errorf("reading which image this container runs: %v", err)
	}

	created, err := dockerDo(http.MethodPost,
		"/containers/create?name="+url.QueryEscape(name), helperSpec(spec, image.Image, cmd))
	if err != nil {
		return "", fmt.Errorf("creating %s: %w", name, err)
	}
	var c struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(created, &c); err != nil || c.ID == "" {
		return "", fmt.Errorf("creating %s: %v", name, err)
	}
	if _, err := dockerDo(http.MethodPost, "/containers/"+c.ID+"/start", nil); err != nil {
		// Removed rather than left: it never ran, so it has no log worth
		// keeping, and a created-but-not-started container is clutter that
		// looks like it means something.
		dockerDo(http.MethodDelete, "/containers/"+c.ID+"?force=true", nil)
		return "", fmt.Errorf("starting %s: %w", name, err)
	}
	return c.ID, nil
}

// helperSpec is the create payload for a helper: the container it is built
// from, with the image pinned to an id and the command replaced -- and with
// everything removed that would let it be mistaken for that container.
func helperSpec(spec containerSpec, imageID string, cmd []string) map[string]any {
	cfg := createSpec(spec, imageID)
	cfg["Cmd"] = cmd
	// The hostname is the original's id; the helper has its own.
	delete(cfg, "Hostname")

	// Compose labels name the service. Left on, the helper IS duva-update as
	// far as findContainer is concerned, and could be the container it finds
	// and stops -- itself, which is the failure this exists to avoid. And
	// compose would count it as a second replica of the service.
	//
	// The project label stays: it is how the helper knows which project to
	// recreate in, the same way the updater does.
	labels := map[string]any{}
	if old, ok := cfg["Labels"].(map[string]any); ok {
		for k, v := range old {
			if strings.HasPrefix(k, "com.docker.compose.") && k != "com.docker.compose.project" {
				continue
			}
			labels[k] = v
		}
	}
	labels[SelfUpdateLabel] = "true"
	cfg["Labels"] = labels

	if host, ok := cfg["HostConfig"].(map[string]any); ok {
		// One shot. Restarted, it would run its one job again.
		host["RestartPolicy"] = map[string]any{"Name": "no"}
		// Kept after it exits: a failed helper's log is the only record of
		// why the updater did not come back. It removes itself on success.
		host["AutoRemove"] = false
		// Nothing outside reaches the updater, and a published port would
		// collide with the container it was copied from.
		delete(host, "PortBindings")
		host["PublishAllPorts"] = false
	}

	// The same networks, but none of the original's identity on them. Its
	// endpoints carry the alias "duva-update", and a helper answering to that
	// name would receive the queue's requests meant for the updater. Reached
	// by its own name instead, which docker resolves on any network it joins.
	endpoints := map[string]any{}
	for name := range spec.NetworkSettings.Networks {
		endpoints[name] = map[string]any{}
	}
	cfg["NetworkingConfig"] = map[string]any{"EndpointsConfig": endpoints}
	return cfg
}

// Wait blocks until a container is no longer running and returns its exit code.
//
// Without the client timeout every other call has: a helper lives as long as a
// pull takes, which on a slow link is longer than any timeout worth setting.
func Wait(id string) (int, error) {
	req, err := http.NewRequest(http.MethodPost,
		"http://docker"+apiVersion+"/containers/"+id+"/wait?condition=not-running", nil)
	if err != nil {
		return 0, err
	}
	client := &http.Client{Transport: dockerHTTP.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("waiting for %s: %w", id, err)
	}
	defer resp.Body.Close()
	var out struct {
		StatusCode int `json:"StatusCode"`
		Error      *struct {
			Message string `json:"Message"`
		} `json:"Error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("waiting for %s: %s: %w", id, resp.Status, err)
	}
	if resp.StatusCode >= 400 {
		return 0, fmt.Errorf("waiting for %s: %s", id, resp.Status)
	}
	if out.Error != nil && out.Error.Message != "" {
		return out.StatusCode, fmt.Errorf("waiting for %s: %s", id, out.Error.Message)
	}
	return out.StatusCode, nil
}

// RemoveSelf removes the container this process runs in, which ends the
// process. For a helper that finished its job: nothing about a success needs
// keeping.
func RemoveSelf() error {
	self, err := Self()
	if err != nil {
		return err
	}
	_, err = dockerDo(http.MethodDelete, "/containers/"+self+"?force=true", nil)
	return err
}
