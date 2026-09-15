package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/decide"
	ociversion "github.com/Miista/homebrew-docker-pin/oci/version"
)

func TestTheMessageIsTheServiceAndTheChange(t *testing.T) {
	title, body := message("optiplex", decide.Entry{
		Service: "whoami", From: "v1.10.4", To: "v1.12.0", Kind: ociversion.KindMinor,
	}, queued, "")
	if title != "whoami on optiplex" {
		t.Errorf("title = %q, want the service and the host", title)
	}
	if body != "v1.10.4 → v1.12.0 (minor) — waiting for approval" {
		t.Errorf("body = %q, want the version jump and the kind", body)
	}
}

// A digest move has no version pair, which is why Kind is empty for one. There
// is no "x to y" to write, so the message says what moved instead.
func TestADigestMoveSaysWhatMoved(t *testing.T) {
	_, body := message("optiplex", decide.Entry{
		Service: "wiki", Tag: "latest",
		Digest: "sha256:55d780f84cb69d6b13ae4783b660e3fda733fd25cc7f2a4f203798949a500227",
	}, queued, "")
	if body != "latest moved (sha256:55d780f84cb6) — waiting for approval" {
		t.Errorf("body = %q, want the tag and a short digest", body)
	}
}

// A decider with no host still says something useful.
func TestAMissingHostLeavesTheTitleAsTheService(t *testing.T) {
	title, _ := message("", decide.Entry{Service: "whoami", From: "1.0", To: "1.1", Kind: ociversion.KindMinor}, queued, "")
	if title != "whoami" {
		t.Errorf("title = %q, want just the service", title)
	}
}

// No endpoint is the default and means nobody is told: this host had no
// notifier before and must not gain one by upgrading.
func TestNoEndpointMeansNoNotifier(t *testing.T) {
	t.Setenv("DECIDER_NOTIF_NTFY_ENDPOINT", "")
	if n := newNtfy(zerolog.Nop()); n != nil {
		t.Error("a decider with no endpoint configured built a notifier")
	}
}

// An endpoint with no topic is a half-configuration. Publishing to a guessed
// topic would be worse than refusing and saying so.
func TestAnEndpointWithoutATopicNotifiesNobody(t *testing.T) {
	t.Setenv("DECIDER_NOTIF_NTFY_ENDPOINT", "https://notify.example.com")
	t.Setenv("DECIDER_NOTIF_NTFY_TOPIC", "")
	if n := newNtfy(zerolog.Nop()); n != nil {
		t.Error("a decider with no topic built a notifier and would publish somewhere unintended")
	}
}

// What actually goes over the wire: the topic in the path, the title and the
// click as headers, the change as the body.
func TestAnAnnouncementCarriesTheTopicTitleAndButton(t *testing.T) {
	var gotPath, gotTitle, gotAuth, gotClick, gotActions, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotTitle = r.URL.Path, r.Header.Get("Title")
		gotAuth, gotClick = r.Header.Get("Authorization"), r.Header.Get("Click")
		gotActions = r.Header.Get("Actions")
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		gotBody = string(buf)
	}))
	defer srv.Close()

	os.Setenv("DECIDER_NOTIF_NTFY_ENDPOINT", srv.URL)
	t.Setenv("DECIDER_NOTIF_NTFY_TOPIC", "duva")
	t.Setenv("NTFY_TOKEN", "tk_secret")
	t.Setenv("DECIDER_NOTIF_NTFY_CLICK", "http://192.0.2.10:8097/")
	defer os.Unsetenv("DECIDER_NOTIF_NTFY_ENDPOINT")

	n := newNtfy(zerolog.Nop())
	if n == nil {
		t.Fatal("no notifier was built from a complete configuration")
	}
	n.announce("optiplex", decide.Entry{
		Service: "whoami", From: "v1.10.4", To: "v1.12.0", Kind: ociversion.KindMinor,
	}, queued, "")

	if gotPath != "/duva" {
		t.Errorf("path = %q, want the topic", gotPath)
	}
	if gotTitle != "whoami on optiplex" {
		t.Errorf("Title = %q", gotTitle)
	}
	if gotAuth != "Bearer tk_secret" {
		t.Errorf("Authorization = %q, want a bearer token", gotAuth)
	}
	// A button, not Click: Click makes the whole notification a link, so
	// tapping it merely to read it opens the UI.
	if gotClick != "" {
		t.Errorf("Click = %q, want none -- the whole notification must not be a link", gotClick)
	}
	if gotActions != `view, Open duva, "http://192.0.2.10:8097/", clear=true` {
		t.Errorf("Actions = %q", gotActions)
	}
	if gotBody != "v1.10.4 → v1.12.0 (minor) — waiting for approval" {
		t.Errorf("body = %q", gotBody)
	}
}

// A notifier that cannot reach its server must not take the decision down with
// it: the entry is queued either way, and the detector's request is answered.
func TestAnUnreachableNotifierIsNotFatal(t *testing.T) {
	t.Setenv("DECIDER_NOTIF_NTFY_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("DECIDER_NOTIF_NTFY_TOPIC", "duva")
	n := newNtfy(zerolog.Nop())
	if n == nil {
		t.Fatal("no notifier was built")
	}
	n.announce("optiplex", decide.Entry{Service: "whoami", From: "1.0", To: "1.1", Kind: ociversion.KindMinor}, queued, "")
	// Reaching here without a panic is the assertion.
}

// The Actions header separates fields on commas, and a query string is
// entitled to contain one. An unquoted URL would truncate the action there.
func TestAViewActionQuotesTheURL(t *testing.T) {
	got := viewAction("Open duva", "https://duva.example.com/?a=1,b=2")
	want := `view, Open duva, "https://duva.example.com/?a=1,b=2", clear=true`
	if got != want {
		t.Errorf("viewAction = %q, want %q", got, want)
	}
}

// A failed auto-apply reads as one, and carries the actor's reason.
//
// This is the case worth interrupting someone: policy changed a host and did
// not finish, so the container may be down and the repository dirty.
func TestAFailedAutoApplySaysSoAndCarriesTheReason(t *testing.T) {
	title, body := message("optiplex", decide.Entry{
		Service: "sonarr", From: "4.0.15", To: "4.0.16", Kind: ociversion.KindPatch,
	}, failedApply, "recreating sonarr: exit status 1")

	if title != "sonarr on optiplex" {
		t.Errorf("title = %q", title)
	}
	want := "4.0.15 → 4.0.16 (patch) — auto-apply failed\nrecreating sonarr: exit status 1"
	if body != want {
		t.Errorf("body = %q, want %q", body, want)
	}
}

// A failure with no reason still says it failed rather than rendering a
// dangling blank line.
func TestAFailureWithoutAReasonIsStillReadable(t *testing.T) {
	_, body := message("optiplex", decide.Entry{
		Service: "sonarr", From: "4.0.15", To: "4.0.16", Kind: ociversion.KindPatch,
	}, failedApply, "")
	if body != "4.0.15 → 4.0.16 (patch) — auto-apply failed" {
		t.Errorf("body = %q", body)
	}
}

// The two are tellable apart at a glance, and only the failure interrupts.
func TestAFailureIsTaggedAndLoudWhileWaitingIsNot(t *testing.T) {
	var gotTags, gotPriority string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTags, gotPriority = r.Header.Get("Tags"), r.Header.Get("Priority")
	}))
	defer srv.Close()

	os.Setenv("DECIDER_NOTIF_NTFY_ENDPOINT", srv.URL)
	defer os.Unsetenv("DECIDER_NOTIF_NTFY_ENDPOINT")
	t.Setenv("DECIDER_NOTIF_NTFY_TOPIC", "duva")
	n := newNtfy(zerolog.Nop())

	e := decide.Entry{Service: "sonarr", From: "4.0.15", To: "4.0.16", Kind: ociversion.KindPatch}

	n.announce("optiplex", e, failedApply, "boom")
	if gotTags != "rotating_light" || gotPriority != "high" {
		t.Errorf("failure: Tags = %q Priority = %q, want rotating_light and high", gotTags, gotPriority)
	}

	n.announce("optiplex", e, queued, "")
	if gotTags != "package" {
		t.Errorf("queued: Tags = %q, want package", gotTags)
	}
	// Waiting for approval is not urgent -- it is waiting.
	if gotPriority != "" {
		t.Errorf("queued: Priority = %q, want none", gotPriority)
	}
}
