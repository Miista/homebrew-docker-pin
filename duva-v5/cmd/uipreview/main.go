// The page over a fake queue, to look at in a browser, and what
// hack/ui-mount loads to check that every page and tab mounts. It listens on
// UIPREVIEW_ADDR, else 127.0.0.1:8099. It has no sign-in server, so the page's
// request for auth/client.js is a 404 here.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/jobs"
	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/queue"
	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/ui"
	"github.com/Miista/homebrew-docker-pin/oci/version"
)

type source struct {
	mu      sync.Mutex
	rows    []ui.Hosted
	soaking []ui.Hosted
	blocks  []ui.HostedBlock
	// taken is the row each block took out, by digest, to put back.
	taken map[string]ui.Hosted
	// checked is when each host last looked.
	checked map[string]time.Time
	// runs is each host's latest asked check.
	runs map[string]*queue.CheckRun
}

// startRun pretends a check was asked of host: numbered, running, and ended
// a few seconds on with an outcome chosen by what was asked about.
func (s *source) startRun(host, service string) {
	s.mu.Lock()
	if s.runs == nil {
		s.runs = map[string]*queue.CheckRun{}
	}
	seq := 1
	if prev := s.runs[host]; prev != nil {
		seq = prev.Seq + 1
	}
	s.runs[host] = &queue.CheckRun{Seq: seq, Service: service}
	s.mu.Unlock()
	go func() {
		time.Sleep(4 * time.Second)
		s.mu.Lock()
		defer s.mu.Unlock()
		r := s.runs[host]
		r.Done, r.Checked = true, 9
		switch {
		case service == "grafana":
			r.Findings = []queue.CheckFinding{{Service: "grafana", Label: "11.3.1", Fate: queue.FateApplying}}
		case service == "caddy":
			r.Findings = []queue.CheckFinding{{Service: "caddy", Label: "2.9.0", Fate: queue.FateWaiting}}
		case service == "pihole":
			r.Checked = 3
			r.Failed = []queue.CheckFailure{{Service: "pihole", Error: "registry answered 429"}}
		case service == "":
			r.Findings = []queue.CheckFinding{
				{Service: "grafana", Label: "11.3.1", Fate: queue.FateApplying},
				{Service: "caddy", Label: "2.9.0", Fate: queue.FateWaiting},
				{Service: "radarr", Label: "5.1", Fate: queue.FateHeld, Until: time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)},
			}
		}
	}()
}

func (s *source) LastChecks() []ui.HostCheck {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Services with nothing waiting too, and one name on both hosts.
	watched := map[string][]string{
		"optiplex": {"authelia", "caddy", "gluetun", "grafana", "homeassistant", "paperless", "postgres", "profilarr", "radarr"},
		"pi":       {"caddy", "pihole", "radarr", "sonarr"},
	}
	out := []ui.HostCheck{}
	for _, h := range []string{"optiplex", "pi"} {
		out = append(out, ui.HostCheck{Host: h, At: s.checked[h].UTC().Format(time.RFC3339), Services: watched[h],
			Jobs:    previewJobs(s.checked[h]),
			Run:     copyRun(s.runs[h]),
			Ignored: []queue.IgnoredService{{Service: h + "-db", Image: "postgres", Tag: "17.10-alpine"}}})
	}
	return out
}

func (s *source) Blocks() []ui.HostedBlock {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ui.HostedBlock{}, s.blocks...)
}

func (s *source) Soaking() []ui.Hosted {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ui.Hosted(nil), s.soaking...)
}

// Upcoming is the first scheduled row again, as one its policy does not
// cover: held for a person rather than applied.
func (s *source) Upcoming() []ui.Hosted {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.soaking) == 0 {
		return nil
	}
	up := s.soaking[0]
	up.ID += 1000
	up.Service = "autobrr"
	up.Auto = queue.AutoNone
	return []ui.Hosted{up}
}

func (s *source) Pending() []ui.Hosted {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ui.Hosted(nil), s.rows...)
}
func (s *source) Unreachable() []ui.Problem { return nil }

// Blocked has one host that cannot apply right now, as optiplex did with an
// uncommitted compose file, so the standing toast can be seen.
func (s *source) Blocked() []ui.Blocked {
	return []ui.Blocked{{Host: "optiplex", Kind: "actor-not-ready",
		Reason: "the repository has uncommitted changes, and applying commits — commit or stash them and this clears on its own"}}
}

func (s *source) drop(match func(ui.Hosted) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, r := range s.rows {
		if match(r) {
			s.rows = append(s.rows[:i], s.rows[i+1:]...)
			return
		}
	}
	for i, r := range s.soaking {
		if match(r) {
			s.soaking = append(s.soaking[:i], s.soaking[i+1:]...)
			return
		}
	}
}

// Stream plays a pretend apply, then takes the row away as a real one would.
func (s *source) Stream(key string, w http.ResponseWriter, r *http.Request) error {
	host, service, _ := ui.SplitKey(key)
	f, _ := w.(http.Flusher)
	for _, line := range []string{"pulling new image", "pinning image", "recreating", "committing"} {
		fmt.Fprintln(w, line)
		if f != nil {
			f.Flush()
		}
		time.Sleep(700 * time.Millisecond)
	}
	if service == "postgres" {
		fmt.Fprintln(w, "status: failed: recreating postgres: the container exited with code 1 -- the compose file still records 17.2 and the repository is now dirty")
		return nil
	}
	fmt.Fprintln(w, "status: completed")
	id, _ := approved.Load(key)
	s.drop(func(h ui.Hosted) bool { return h.Host == host && h.Service == service && h.ID == id })
	return nil
}

func copyRun(r *queue.CheckRun) *queue.CheckRun {
	if r == nil {
		return nil
	}
	c := *r
	return &c
}

type actions struct{ src *source }

func (a *actions) Discard(host string, id int64, actor string) error {
	log.Printf("DISCARD %s entry %d by %s", host, id, actor)
	a.src.drop(func(h ui.Hosted) bool { return h.Host == host && h.ID == id })
	return nil
}
func (a *actions) CheckAll(actor string, back time.Duration) error {
	log.Printf("CHECK ALL back %s by %s", back, actor)
	if err := asked("check", "*", "every service", actor); err != nil {
		return fmt.Errorf("a check was asked for a moment ago; try again in a minute")
	}
	a.src.mu.Lock()
	defer a.src.mu.Unlock()
	for h := range a.src.checked {
		a.src.checked[h] = time.Now()
	}
	go func() {
		for _, h := range []string{"optiplex", "pi"} {
			a.src.startRun(h, "")
		}
	}()
	return nil
}

func (a *actions) Unblock(host, image, digest, actor string) error {
	log.Printf("UNBLOCK %s %s@%s by %s", host, image, digest, actor)
	a.src.mu.Lock()
	defer a.src.mu.Unlock()
	for i, b := range a.src.blocks {
		if b.Host == host && b.Image == image && b.Digest == digest {
			a.src.blocks = append(a.src.blocks[:i], a.src.blocks[i+1:]...)
			if row, ok := a.src.taken[digest]; ok {
				row.ReleaseAt = "" // back waiting for a person
				a.src.rows = append(a.src.rows, row)
				delete(a.src.taken, digest)
			}
			return nil
		}
	}
	return fmt.Errorf("%s@%s is not blocked", image, digest)
}

func (a *actions) Block(host string, id int64, reason, actor string) error {
	log.Printf("BLOCK %s entry %d (%s) by %s", host, id, reason, actor)
	a.src.mu.Lock()
	for _, r := range append(append([]ui.Hosted{}, a.src.rows...), a.src.soaking...) {
		if r.Host == host && r.ID == id {
			digest := fmt.Sprintf("sha256:%064d", id)
			a.src.blocks = append([]ui.HostedBlock{{Host: host, Block: queue.Block{Image: r.Image,
				Digest: digest, Release: r.To, By: actor,
				At: time.Now().UTC().Format(time.RFC3339)}}}, a.src.blocks...)
			a.src.taken[digest] = r
		}
	}
	a.src.mu.Unlock()
	a.src.drop(func(h ui.Hosted) bool { return h.Host == host && h.ID == id })
	return nil
}

var lastAsked sync.Map

// previewJobs is a host's jobs as the page would see them: a check this
// morning, a tick a few minutes ago, and one that went wrong to look at.
func previewJobs(checked time.Time) []jobs.Status {
	tick := time.Now().Add(-4 * time.Minute)
	run := func(job string, at time.Time, took time.Duration, summary string, failed bool, out ...string) *jobs.Run {
		return &jobs.Run{Job: job, Started: at, Took: took, Summary: summary, Output: out, Failed: failed}
	}
	return []jobs.Status{
		{Job: jobs.Check, Next: checked.Add(24 * time.Hour), Last: run(jobs.Check, checked, 41*time.Second,
			"42 service(s), 3 new tag(s), 1 could not be checked", true,
			"looking for tags published since each service's last check (2026-10-21 03:00)",
			"checking 42 service(s) in /home/guldmund/docker/optiplex/docker-compose.yml",
			"grafana: latest now points at sha256:dbbc38ea2110",
			"! private-app: could not list tags: 401 Unauthorized",
			"check complete: 3 new tag(s) across 42 service(s), 1 could not be checked")},
		{Job: jobs.Sync, Next: tick.Add(10 * time.Minute), Last: run(jobs.Sync, tick, 180*time.Millisecond,
			"44 services; 0 added, 1 removed", false,
			"thelounge: gone from the compose file and its container gone; the service is disabled, its history kept")},
		{Job: jobs.Reconcile, Next: tick.Add(10 * time.Minute), Last: run(jobs.Reconcile, tick, 40*time.Millisecond,
			"1 dropped", false,
			"loki: sha256:d70e46596 -> sha256:1107dd527 no longer applies (already matches the compose file), dropped from the queue")},
		{Job: jobs.Release, Next: tick.Add(10 * time.Minute), Last: run(jobs.Release, tick, 2*time.Millisecond, "0 released", false)},
	}
}

func (a *actions) RunJob(job, actor string) error {
	log.Printf("RUN %s on every host by %s", job, actor)
	return asked("run", "*", job, actor)
}

func (a *actions) Check(host, service, actor string, back time.Duration) error {
	if err := asked("check", host, service, actor); err != nil {
		return err
	}
	a.src.startRun(host, service)
	return nil
}
func asked(what, host, service, actor string) error {
	if t, ok := lastAsked.Load(host + "/" + service); ok && time.Since(t.(time.Time)) < time.Minute {
		return fmt.Errorf("%s was asked for a moment ago; try again in a minute", service)
	}
	lastAsked.Store(host+"/"+service, time.Now())
	log.Printf("%s %s/%s by %s", strings.ToUpper(what), host, service, actor)
	return nil
}

// approved is the entry each service's last approval named, so the pretend
// apply removes that entry and not another of the same service's.
var approved sync.Map

type approver struct{}

func (approver) Approve(key string, entry int64, actor string) error {
	log.Printf("APPROVE %s entry %d by %s", key, entry, actor)
	approved.Store(key, entry)
	return nil
}

func main() {
	// Release notes for some, as images that name their source; none for
	// the rest.
	sources := map[string]string{
		"paperless": "https://github.com/paperless-ngx/paperless-ngx",
		"authelia":  "https://github.com/authelia/authelia",
	}
	e := func(id int64, svc, img, from, to string, kind version.Kind, auto queue.Auto, why string) queue.Entry {
		return queue.Entry{ID: id, Service: svc, Image: img, From: from, To: to, Tag: to, Kind: kind,
			Why: why, Auto: auto, FirstSeen: time.Now().Add(-30 * time.Hour).UTC().Format(time.RFC3339),
			Source: sources[svc]}
	}
	move := queue.Entry{ID: 12, Service: "homeassistant", Image: "ghcr.io/home-assistant/home-assistant",
		From: "sha256:4f1c0a9e", To: "sha256:9b2d77c1", Tag: "stable", Digest: "sha256:9b2d77c1",
		Why: "digest moved; duva.auto is none", Auto: queue.AutoNone,
		Source: "https://github.com/home-assistant/core"}
	src := &source{rows: []ui.Hosted{
		{Entry: e(7, "paperless", "ghcr.io/paperless-ngx/paperless-ngx", "2.13.0", "2.14.1", version.KindMinor, queue.AutoPatch, "minor exceeds duva.auto: patch"), Host: "optiplex", CanApply: true},
		{Entry: e(8, "postgres", "postgres", "16.4", "17.2", version.KindMajor, queue.AutoMinor, "major exceeds duva.auto: minor"), Host: "optiplex", CanApply: true},
		{Entry: move, Host: "optiplex", CanApply: true},
		{Entry: e(3, "radarr", "lscr.io/linuxserver/radarr", "5.14.0", "5.15.0", version.KindMinor, queue.AutoPatch, "minor exceeds duva.auto: patch"), Host: "pi", CanApply: true},
		{Entry: e(4, "pihole", "pihole/pihole", "2024.07.0", "2025.03.0", version.KindUnknown, queue.AutoMinor, "version change could not be classified; needs duva.auto: major"), Host: "pi", CanApply: true},
	}}
	soak := func(id int64, svc, img, from, to string, in time.Duration, host string) ui.Hosted {
		at := time.Now().Add(in)
		en := e(id, svc, img, from, to, version.KindPatch, queue.AutoPatch,
			"patch, within duva.auto: patch; delayed until "+at.Format("2006-01-02 15:04 MST"))
		en.ReleaseAt = at.UTC().Format(time.RFC3339)
		return ui.Hosted{Entry: en, Host: host, CanApply: true}
	}
	src.soaking = []ui.Hosted{
		soak(20, "authelia", "authelia/authelia", "4.39.24", "4.39.25", 9*time.Hour, "optiplex"),
		soak(21, "paperless", "ghcr.io/paperless-ngx/paperless-ngx", "2.13.0", "2.13.1", 31*time.Hour, "optiplex"),
		soak(22, "authelia", "authelia/authelia", "4.39.24", "4.39.27", 52*time.Hour, "optiplex"),
		soak(23, "sonarr", "lscr.io/linuxserver/sonarr", "4.0.10", "4.0.11", 70*time.Hour, "pi"),
	}
	src.blocks = []ui.HostedBlock{
		{Host: "optiplex", Block: queue.Block{Image: "linuxserver/sabnzbd", Digest: "sha256:7f3a9c0e1b2d4f6a8c0e2b4d6f8a0c2e4b6d8f0a2c4e6b8d0f2a4c6e8b0d2f4a", Release: "4.4.0", By: "soren", At: time.Now().Add(-9 * 24 * time.Hour).UTC().Format(time.RFC3339)}},
		{Host: "pi", Block: queue.Block{Image: "pihole/pihole", Digest: "sha256:1c2e3a4b5d6f7081928374655647382910abcdef0123456789abcdef01234567", Release: "2025.02.1", By: "soren", At: time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339)}},
	}
	src.checked = map[string]time.Time{
		"optiplex": time.Now().Add(-5*time.Hour - 12*time.Minute),
		"pi":       time.Now().Add(-5*time.Hour - 9*time.Minute),
	}
	src.taken = map[string]ui.Hosted{
		src.blocks[0].Digest: {Entry: e(30, "sabnzbd", "linuxserver/sabnzbd", "4.3.3", "4.4.0", version.KindMinor, queue.AutoPatch, "minor exceeds duva.auto: patch"), Host: "optiplex", CanApply: true},
		src.blocks[1].Digest: {Entry: e(31, "pihole", "pihole/pihole", "2024.07.0", "2025.02.1", version.KindUnknown, queue.AutoMinor, "version change could not be classified; needs duva.auto: major"), Host: "pi", CanApply: true},
	}
	s := &ui.Server{Source: src, Approver: approver{}, Actions: &actions{src: src},
		Actor: func(*http.Request) string { return "soren" }, Version: "preview (dummy data)"}
	addr := os.Getenv("UIPREVIEW_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8099"
	}
	log.Println("serving on http://" + addr)
	log.Fatal(http.ListenAndServe(addr, s.Handler()))
}
