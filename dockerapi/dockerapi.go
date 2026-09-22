package dockerapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/distribution/reference"

	"github.com/Miista/homebrew-docker-pin/compose"
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
	Config     map[string]any `json:"Config"`
	HostConfig map[string]any `json:"HostConfig"`
	// State is read for one field: whether this container was running. A
	// recreate must put back what it took away, and starting a container
	// somebody stopped is not a no-op -- it is the service coming back.
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
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

// ContainerName is what a service's container is called, or the service
// name when there is no container to ask -- a service that is not running
// still has to be named in a log line.
func ContainerName(service string) string {
	id, err := findContainer(service)
	if err != nil {
		return service
	}
	raw, err := dockerDo(http.MethodGet, "/containers/"+id+"/json", nil)
	if err != nil {
		return service
	}
	var named struct {
		Name string `json:"Name"`
	}
	if err := json.Unmarshal(raw, &named); err != nil || named.Name == "" {
		return service
	}
	return strings.TrimPrefix(named.Name, "/")
}

// preRecreateDump is where a container's inspect output is saved just before
// Recreate stops and removes it.
//
// /tmp rather than the bind-mounted repository: the repository is a git working
// tree, and a diagnostic file left there would need cleaning up or committing,
// neither of which is this function's business. A container recycle loses it,
// which is an acceptable cost for a file nobody usually reads -- `docker cp`
// while the updater container is still up is what a person reaches for after
// a create fails, and the error message names this exact path.
func preRecreateDump(service string) string {
	return "/tmp/duva-recreate-" + service + ".json"
}

// Recreate replaces a service's container with one running the image
// its compose file now pins.
//
// The compose file is not read: what the container should be is already
// recorded on the container itself, and the only thing changing is the image.
// The name is reused, so the replacement is indistinguishable from the
// original to anything that refers to it -- including compose, which keeps
// managing it because its labels come across untouched.
func Recreate(composeFile, service string) error {
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

	// Written before the container is touched, not after a failure: once
	// stop+remove runs, the old container's exact Mounts/Config are gone for
	// good, and a create that then fails (e.g. "Duplicate mount point") has
	// nothing left to diagnose it against. A prior incident (flaresolverr,
	// 2026-09-22) hit exactly this -- a create failed with a duplicate /config
	// mount that a fresh container built from the same two images would not
	// reproduce, and with the original gone there was no way to tell what
	// state actually caused it. Best-effort: a diagnostic that could itself
	// fail and block a real update would be worse than not having it.
	_ = os.WriteFile(preRecreateDump(service), raw, 0o644)

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
		dump := preRecreateDump(service)
		return fmt.Errorf("creating the replacement for %s: %w"+
			" (its configuration just before this was saved to %s inside this container --"+
			" `docker cp <this container>:%s .` retrieves it before it is gone)",
			service, err, dump, dump)
	}
	var newc struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(created, &newc); err != nil || newc.ID == "" {
		return fmt.Errorf("creating the replacement for %s: %w", service, err)
	}

	// Started only if the old one was running.
	//
	// A stopped container is a decision: `docker stop` on a service with
	// restart: unless-stopped is how an operator takes something out of
	// service, and the compose file still declaring it is what makes it easy
	// to put back. Starting it here would override that decision as a side
	// effect of an image update -- a service returning on its own, which is
	// the kind of surprise this tool exists to avoid.
	//
	// The update still happens: the pin is rewritten and the container is
	// recreated on the new image, so it comes back on the new version
	// whenever someone starts it.
	if !spec.State.Running {
		return nil
	}
	if _, err := dockerDo(http.MethodPost, "/containers/"+newc.ID+"/start", nil); err != nil {
		return fmt.Errorf("starting the replacement for %s: %w", service, err)
	}
	return nil
}

// Pull fetches an image through the daemon.
//
// The daemon does the pulling: duva names the image and, for a private
// registry, passes credentials. It does not fetch layers itself -- the image
// store is the daemon's.
//
// The response must be read to the end. It is a stream of progress events,
// and abandoning it aborts the pull: the request returns promptly and the
// image is not there, which looks like a registry problem rather than a
// mistake here. Watchtower and WUD both drain it for the same reason.
func Pull(ref string) error {
	name, tag := splitRef(ref)
	path := "/images/create?fromImage=" + url.QueryEscape(name) + "&tag=" + url.QueryEscape(tag)

	req, err := http.NewRequest(http.MethodPost, "http://docker"+apiVersion+path, nil)
	if err != nil {
		return err
	}
	if auth := registryAuth(name); auth != "" {
		req.Header.Set("X-Registry-Auth", auth)
	}

	resp, err := dockerHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("pulling %s: %w", ref, err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("pulling %s: %s: %s", ref, resp.Status, strings.TrimSpace(string(body)))
	}
	if readErr != nil {
		return fmt.Errorf("pulling %s: %w", ref, readErr)
	}
	// A pull can fail mid-stream with a 200 already sent, so the error is in
	// the body rather than the status.
	if i := strings.Index(string(body), `"error"`); i != -1 {
		return fmt.Errorf("pulling %s: %s", ref, strings.TrimSpace(string(body)[i:]))
	}
	return nil
}

// splitRef separates an image reference into the name and the tag or digest.
//
// A digest is kept whole -- `fromImage=name&tag=sha256:...` is how the API
// asks for one -- because duva pulls by digest when following a moving tag it
// has already resolved.
func splitRef(ref string) (name, tag string) {
	if i := strings.Index(ref, "@"); i != -1 {
		return ref[:i], ref[i+1:]
	}
	// A colon in the host part is a port, not a tag separator.
	if i := strings.LastIndex(ref, ":"); i != -1 && !strings.Contains(ref[i:], "/") {
		return ref[:i], ref[i+1:]
	}
	return ref, "latest"
}

// registryAuth is the credential for a registry, base64 JSON as the API wants
// it, or "" for an anonymous pull.
//
// From the environment rather than a config file: duva's image carries no
// docker configuration, and a mounted config.json would mean parsing
// credential helpers to be useful. Public images -- which is what a stack
// following upstream tags mostly pulls -- need nothing.
func registryAuth(image string) string {
	user, pass := os.Getenv("DUVA_REGISTRY_USER"), os.Getenv("DUVA_REGISTRY_PASSWORD")
	if user == "" || pass == "" {
		return ""
	}
	cred, err := json.Marshal(map[string]string{
		"username":      user,
		"password":      pass,
		"serveraddress": registryOf(image),
	})
	if err != nil {
		return ""
	}
	return base64.URLEncoding.EncodeToString(cred)
}

// registryOf is the host an image comes from, and so which registry a
// credential is sent to. Getting it wrong sends a private registry's password
// to Docker Hub.
//
// Deciding which first path segment is a hostname has more cases than it
// looks: a dot or a colon marks a domain or a host:port, "localhost" is a
// reserved host with neither, and a segment that is not all lowercase cannot
// be a namespace so must be a host. This was hand-written once and had two of
// those four wrong, so it defers to docker's own parser rather than keeping a
// copy of rules that only drift.
func registryOf(image string) string {
	// The legacy index URL, not "docker.io": this goes in X-Registry-Auth,
	// where the daemon still identifies Hub by its v1 address.
	const hub = "https://index.docker.io/v1/"

	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		// Not a reference duva can reason about. Anonymous is the safe
		// answer: no credential is sent anywhere.
		return hub
	}
	if domain := reference.Domain(named); domain != "docker.io" {
		return domain
	}
	return hub
}

// Digest is the repo digest of an image the daemon already has: what it
// can be pulled by on another host.
func Digest(ref string) (string, error) {
	raw, err := dockerDo(http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil)
	if err != nil {
		return "", fmt.Errorf("inspecting %s: %w", ref, err)
	}
	var image struct {
		RepoDigests []string `json:"RepoDigests"`
	}
	if err := json.Unmarshal(raw, &image); err != nil {
		return "", fmt.Errorf("reading the digest of %s: %w", ref, err)
	}
	for _, rd := range image.RepoDigests {
		if i := strings.Index(rd, "@"); i != -1 {
			return rd[i+1:], nil
		}
	}
	return "", fmt.Errorf("no repo digest for %s", ref)
}

// containerLabel reads one label from a container.
func containerLabel(id, label string) (string, error) {
	raw, err := dockerDo(http.MethodGet, "/containers/"+id+"/json", nil)
	if err != nil {
		return "", fmt.Errorf("inspecting %s: %w", id, err)
	}
	var c struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", fmt.Errorf("reading %s from %s: %w", label, id, err)
	}
	return c.Config.Labels[label], nil
}

// Exists reports whether a compose service has a container to replace.
//
// For a caller that wants to know *before* doing anything irreversible.
// Recreate finds the container itself, but by then a pull has happened and a
// pin has been written -- and a service with no container leaves the compose
// file claiming an image that nothing runs.
//
// Read-only: a lookup by the same labels Recreate uses, so the two cannot
// disagree about what "this service" means.
func Exists(service string) error {
	_, err := findContainer(service)
	return err
}

// Running reports whether a service's container exists and is running.
//
// A container that does not exist is not running, and not an error: a service
// declared but never started is an ordinary state, not a fault.
func Running(service string) (bool, error) {
	name, err := project(realSelf)
	if err != nil {
		return false, err
	}
	// all=true, unlike findContainer: a stopped container is exactly what this
	// is asking about, and the default listing omits it.
	filters := url.QueryEscape(fmt.Sprintf(
		`{"label":["com.docker.compose.project=%s","com.docker.compose.service=%s"]}`,
		name, service))
	raw, err := dockerDo(http.MethodGet, "/containers/json?all=true&filters="+filters, nil)
	if err != nil {
		return false, err
	}
	var found []struct {
		State string `json:"State"`
	}
	if err := json.Unmarshal(raw, &found); err != nil {
		return false, fmt.Errorf("listing containers for %s: %w", service, err)
	}
	if len(found) == 0 {
		return false, nil
	}
	return found[0].State == "running", nil
}
