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
	})
	if title != "whoami on optiplex" {
		t.Errorf("title = %q, want the service and the host", title)
	}
	if body != "v1.10.4 → v1.12.0 (minor)" {
		t.Errorf("body = %q, want the version jump and the kind", body)
	}
}

// A digest move has no version pair, which is why Kind is empty for one. There
// is no "x to y" to write, so the message says what moved instead.
func TestADigestMoveSaysWhatMoved(t *testing.T) {
	_, body := message("optiplex", decide.Entry{
		Service: "wiki", Tag: "latest",
		Digest: "sha256:55d780f84cb69d6b13ae4783b660e3fda733fd25cc7f2a4f203798949a500227",
	})
	if body != "latest moved (sha256:55d780f84cb6)" {
		t.Errorf("body = %q, want the tag and a short digest", body)
	}
}

// A decider with no host still says something useful.
func TestAMissingHostLeavesTheTitleAsTheService(t *testing.T) {
	title, _ := message("", decide.Entry{Service: "whoami", From: "1.0", To: "1.1", Kind: ociversion.KindMinor})
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
func TestAnAnnouncementCarriesTheTopicTitleAndClick(t *testing.T) {
	var gotPath, gotTitle, gotAuth, gotClick, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotTitle = r.URL.Path, r.Header.Get("Title")
		gotAuth, gotClick = r.Header.Get("Authorization"), r.Header.Get("Click")
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		gotBody = string(buf)
	}))
	defer srv.Close()

	os.Setenv("DECIDER_NOTIF_NTFY_ENDPOINT", srv.URL)
	t.Setenv("DECIDER_NOTIF_NTFY_TOPIC", "duva")
	t.Setenv("DECIDER_NOTIF_NTFY_TOKEN", "tk_secret")
	t.Setenv("DECIDER_NOTIF_NTFY_CLICK", "http://192.0.2.10:8097/")
	defer os.Unsetenv("DECIDER_NOTIF_NTFY_ENDPOINT")

	n := newNtfy(zerolog.Nop())
	if n == nil {
		t.Fatal("no notifier was built from a complete configuration")
	}
	n.announce("optiplex", decide.Entry{
		Service: "whoami", From: "v1.10.4", To: "v1.12.0", Kind: ociversion.KindMinor,
	})

	if gotPath != "/duva" {
		t.Errorf("path = %q, want the topic", gotPath)
	}
	if gotTitle != "whoami on optiplex" {
		t.Errorf("Title = %q", gotTitle)
	}
	if gotAuth != "Bearer tk_secret" {
		t.Errorf("Authorization = %q, want a bearer token", gotAuth)
	}
	if gotClick != "http://192.0.2.10:8097/" {
		t.Errorf("Click = %q", gotClick)
	}
	if gotBody != "v1.10.4 → v1.12.0 (minor)" {
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
	n.announce("optiplex", decide.Entry{Service: "whoami", From: "1.0", To: "1.1", Kind: ociversion.KindMinor})
	// Reaching here without a panic is the assertion.
}
