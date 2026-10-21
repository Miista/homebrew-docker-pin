package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/settings"
	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/watch"
)

// act runs one of the queue's actions from a terminal, as
// `docker exec <duva> duva <action> ...`, through this host's own queue API
// with its own token. The same path the page takes, so the same checks
// apply, and the history records the action as the CLI's.
func act(action string, args []string) error {
	usage := map[string]string{
		"discard": "duva discard <entry>",
		"block":   "duva block <entry> [reason]",
		"unblock": "duva unblock <image> <digest>",
		"check":   "duva check [<service>] [--since 7d]",
	}
	if action == "check" {
		path, form, err := checkRequest(args)
		if err != nil {
			return fmt.Errorf("%v\nusage: %s", err, usage["check"])
		}
		return postLocal(path, form)
	}
	var path string
	form := url.Values{}
	switch {
	case action == "discard" && len(args) == 1:
		path = "/v1/" + action + "/" + url.PathEscape(args[0])
	case action == "block" && len(args) >= 1:
		path = "/v1/block/" + url.PathEscape(args[0])
		form.Set("reason", strings.Join(args[1:], " "))
	case action == "unblock" && len(args) == 2:
		path = "/v1/unblock"
		form.Set("image", args[0])
		form.Set("digest", args[1])
	default:
		return fmt.Errorf("usage: %s", usage[action])
	}
	return postLocal(path, form)
}

// checkRequest reads `duva check [<service>] [--since D]` into the queue API
// call it makes: one service, or every one, at least D back.
//
// Flags first, then the leftovers, and a leftover that looks like a flag is
// refused: sniffing for --since among the arguments would let a mistyped
// `--snice 30d` run an ordinary check and say nothing.
func checkRequest(args []string) (string, url.Values, error) {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	since := fs.String("since", "", "how far back to look: 7d, 72h; default since the last check")
	// The service may come before the flag, as people type it.
	var service string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		service, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return "", nil, err
	}
	rest := fs.Args()
	for _, a := range rest {
		if strings.HasPrefix(a, "-") {
			return "", nil, fmt.Errorf("unknown flag %s", a)
		}
	}
	if service == "" && len(rest) > 0 {
		service, rest = rest[0], rest[1:]
	}
	if len(rest) > 0 {
		return "", nil, fmt.Errorf("one service at most, got %s", strings.Join(append([]string{service}, rest...), " "))
	}
	back, err := watch.ParseLookBack(*since)
	if err != nil {
		return "", nil, fmt.Errorf("--since: %v", err)
	}
	form := url.Values{}
	if back > 0 {
		form.Set("since", watch.FormatLookBack(back))
	}
	if service == "" {
		return "/v1/check-all", form, nil
	}
	return "/v1/check/" + url.PathEscape(service), form, nil
}

// postLocal sends an action to this host's queue API, as the CLI.
func postLocal(path string, form url.Values) error {
	token := settings.Value(settings.APIToken)
	if token == "" {
		return fmt.Errorf("%s is not set: run this inside duva's container", settings.EnvVar(settings.APIToken))
	}
	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:8080"+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Duva-Actor", "cli")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("reaching duva: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("duva answered %s", resp.Status)
	}
	var out struct{ Message string }
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Message != "" {
		return fmt.Errorf("%s", out.Message)
	}
	return nil
}
