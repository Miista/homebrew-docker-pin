package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Miista/homebrew-docker-pin/internal/compose"
)

// Recreating a container means replacing it with one identical in every way
// except the image. duva does that through the docker API rather than by
// running `docker compose up`, because it watches from inside a container: it
// sees the compose file at /compose while the daemon it drives sees the stack
// at its own path on the host, and every relative path in that file would have
// to be resolved correctly in both places at once.
//
// It cannot be. Compose resolves a service's relative paths against the
// project directory, but the two kinds of relative path are consumed by
// different processes:
//
//   - a bind (`./data:/var/lib/postgresql/data`) is handed to the daemon, so
//     it has to come out as a HOST path;
//   - an include (`common/registry.yml`) is read by compose itself, so it has
//     to come out as a path inside duva.
//
// One --project-directory cannot satisfy both: set it to the host path and
// includes fail to open; leave it and every bind is silently rebound to a
// directory that does not exist on the host, which loses data quietly.
//
// The API sidesteps the question. A running container's configuration is
// already resolved -- its binds are absolute, its includes were consumed when
// it was created, its project is recorded on it as a label. Swapping the image
// needs no path interpreted at all. This is what Watchtower and WUD do, and
// for an image-only change to a stack that is already up it is sufficient:
// depends_on and healthcheck ordering govern starting a stack from nothing,
// not replacing one member of a running one.

const dockerSocket = "/var/run/docker.sock"

// dockerHTTP talks to the daemon over its unix socket. The CLI has no verb for
// "recreate this container with a different image": doing it by hand needs the
// container's own creation spec, which only the API returns.
var dockerHTTP = &http.Client{
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", dockerSocket)
		},
	},
	Timeout: 2 * time.Minute,
}

// apiVersion is pinned rather than negotiated: every call here is from the
// long-stable part of the API, and a version the daemon predates would fail
// loudly at the first request rather than subtly later.
const apiVersion = "v1.44"

func dockerDo(method, path string, body any) ([]byte, error) {
	var buf *bytes.Buffer
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		buf = bytes.NewBuffer(raw)
	} else {
		buf = bytes.NewBuffer(nil)
	}

	req, err := http.NewRequest(method, "http://docker"+apiVersion+path, buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := dockerHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("talking to the docker daemon: %w", err)
	}
	defer resp.Body.Close()

	out := new(bytes.Buffer)
	if _, err := out.ReadFrom(resp.Body); err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("docker api %s %s: %s: %s",
			method, path, resp.Status, strings.TrimSpace(out.String()))
	}
	return out.Bytes(), nil
}

// containerSpec is a container's inspect output, kept as a raw map so that
// everything the daemon recorded survives into the replacement. Naming the
// fields would silently drop whatever this struct did not know about.
type containerSpec struct {
	Config          map[string]any `json:"Config"`
	HostConfig      map[string]any `json:"HostConfig"`
	NetworkSettings struct {
		Networks map[string]any `json:"Networks"`
	} `json:"NetworkSettings"`
	Mounts []struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
}

// findContainer locates the running container for a service in duva's own
// compose project.
//
// The project comes from duva's own labels, which is why duva has to run as a
// service in the stack it watches: it is what says which of several stacks on
// a host this service belongs to.
func findContainer(service string) (string, error) {
	name, err := project(realSelf)
	if err != nil {
		return "", err
	}
	filters := url.QueryEscape(fmt.Sprintf(
		`{"label":["com.docker.compose.project=%s","com.docker.compose.service=%s"]}`,
		name, service))

	raw, err := dockerDo(http.MethodGet, "/containers/json?filters="+filters, nil)
	if err != nil {
		return "", err
	}
	var found []struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(raw, &found); err != nil {
		return "", fmt.Errorf("listing containers for %s: %w", service, err)
	}
	if len(found) == 0 {
		return "", fmt.Errorf("no running container for %s in compose project %s", service, name)
	}
	return found[0].ID, nil
}

// createSpec builds the payload for the replacement container: everything the
// old one had, with the image swapped.
func createSpec(spec containerSpec, image string) map[string]any {
	cfg := map[string]any{}
	for k, v := range spec.Config {
		cfg[k] = v
	}
	cfg["Image"] = image

	host := map[string]any{}
	for k, v := range spec.HostConfig {
		host[k] = v
	}

	// Anonymous volumes exist ONLY in the container's Mounts, never in
	// HostConfig.Binds and usually not in the compose file either -- they come
	// from a VOLUME line in the image, which postgres, mysql and mongo all
	// have. Destroying the container destroys the only reference to that
	// randomly-named volume, so a replacement built from Config alone gets a
	// fresh empty one: the service comes back up healthy and empty while the
	// real data sits in an unreferenced volume awaiting the next prune.
	binds := toStrings(host["Binds"])
	bound := map[string]bool{}
	for _, b := range binds {
		if parts := strings.Split(b, ":"); len(parts) > 1 {
			bound[parts[1]] = true
		}
	}
	for _, m := range spec.Mounts {
		if m.Type != "volume" || m.Name == "" || bound[m.Destination] {
			continue
		}
		mode := "rw"
		if !m.RW {
			mode = "ro"
		}
		binds = append(binds, m.Name+":"+m.Destination+":"+mode)
		// The image's VOLUME declaration also lists this path in Config.Volumes.
		// Left there alongside the bind the daemon rejects the whole create as
		// a duplicate mount point, so the explicit bind supersedes it.
		if vols, ok := cfg["Volumes"].(map[string]any); ok {
			delete(vols, m.Destination)
		}
	}
	if len(binds) > 0 {
		host["Binds"] = binds
	}
	cfg["HostConfig"] = host

	// Endpoints carry each network's aliases, without which compose's
	// service-to-service DNS stops resolving this container by name.
	cfg["NetworkingConfig"] = map[string]any{
		"EndpointsConfig": spec.NetworkSettings.Networks,
	}
	return cfg
}

func toStrings(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// recreateContainer replaces a service's container with one running the image
// its compose file now pins.
//
// The compose file is not read: what the container should be is already
// recorded on the container itself, and the only thing changing is the image.
// The name is reused, so the replacement is indistinguishable from the
// original to anything that refers to it -- including compose, which keeps
// managing it because its labels come across untouched.
func recreateContainer(composeFile, service string) error {
	id, err := findContainer(service)
	if err != nil {
		return err
	}

	raw, err := dockerDo(http.MethodGet, "/containers/"+id+"/json", nil)
	if err != nil {
		return fmt.Errorf("inspecting %s: %w", service, err)
	}
	var spec containerSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return fmt.Errorf("reading the configuration of %s: %w", service, err)
	}

	// The one thing that does not come from the container: the image the
	// compose file now pins, which is what this whole exercise is for.
	image, err := compose.RawImage(composeFile, service)
	if err != nil {
		return fmt.Errorf("reading the pinned image for %s: %w", service, err)
	}

	var named struct {
		Name string `json:"Name"`
	}
	if err := json.Unmarshal(raw, &named); err != nil {
		return fmt.Errorf("reading the name of %s: %w", service, err)
	}
	name := strings.TrimPrefix(named.Name, "/")

	// Stopped and removed before the replacement is created, because the name
	// has to be free. A failure here leaves the old container in place, which
	// the transaction reports and the next run retries.
	if _, err := dockerDo(http.MethodPost, "/containers/"+id+"/stop?t=10", nil); err != nil {
		return fmt.Errorf("stopping %s: %w", service, err)
	}
	if _, err := dockerDo(http.MethodDelete, "/containers/"+id, nil); err != nil {
		return fmt.Errorf("removing %s: %w", service, err)
	}

	created, err := dockerDo(http.MethodPost,
		"/containers/create?name="+url.QueryEscape(name), createSpec(spec, image))
	if err != nil {
		return fmt.Errorf("creating the replacement for %s: %w", service, err)
	}
	var newc struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(created, &newc); err != nil || newc.ID == "" {
		return fmt.Errorf("creating the replacement for %s: %w", service, err)
	}

	if _, err := dockerDo(http.MethodPost, "/containers/"+newc.ID+"/start", nil); err != nil {
		return fmt.Errorf("starting the replacement for %s: %w", service, err)
	}
	return nil
}
