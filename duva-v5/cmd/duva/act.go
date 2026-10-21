package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/settings"
)

// act runs one of the queue's actions from a terminal, as
// `docker exec <duva> duva <action> ...`, through this host's own queue API
// with its own token. The same path the page takes, so the same checks
// apply, and the history records the action as the CLI's.
//
// Until the page has buttons for discard, block and rescan, this is how
// they are done.
func act(action string, args []string) error {
	usage := map[string]string{
		"discard": "duva discard <entry>",
		"block":   "duva block <entry> [reason]",
		"unblock": "duva unblock <image> <digest>",
		"check":   "duva check <service>",
		"rescan":  "duva rescan <service>",
	}
	var path string
	form := url.Values{}
	switch {
	case action == "discard" && len(args) == 1,
		action == "check" && len(args) == 1,
		action == "rescan" && len(args) == 1:
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
