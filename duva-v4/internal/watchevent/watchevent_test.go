package watchevent

import (
	"strings"
	"testing"
)

// The payload the watcher actually sends, verbatim rather than reduced to
// the fields used: if the watcher changes shape, the test that notices
// should be reading what it really sends.
const realEvent = `{
  "watcher": "watch/v1",
  "host": "optiplex",
  "service": "paperless",
  "container": "paperless",
  "image": "ghcr.io/paperless-ngx/paperless-ngx",
  "tag": "3.1.3",
  "published": "2026-09-04T02:58:02Z",
  "observed_at": "2026-09-11T17:14:36Z"
}`

func TestParseAndTranslate(t *testing.T) {
	e, err := Parse([]byte(realEvent))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	n, why, ok := Translate(e)
	if !ok {
		t.Fatalf("a real event was not translated: %s", why)
	}
	if n.Container != "paperless" {
		t.Errorf("container = %q", n.Container)
	}
	// The gate wants a full reference and reads the tag from it.
	if n.Image != "ghcr.io/paperless-ngx/paperless-ngx:3.1.3" {
		t.Errorf("image = %q, want the reference with its tag", n.Image)
	}
	// No digest: resolving one would be a registry request per finding to
	// tell the gate something the updater learns when it pulls.
	if n.Digest != "" {
		t.Errorf("digest = %q, want none", n.Digest)
	}
}

// The container name is the only thing that maps an event to a service.
func TestNoContainerIsUnusable(t *testing.T) {
	_, why, ok := Translate(Event{Image: "app", Tag: "1.0"})
	if ok {
		t.Fatal("an event with no container was translated")
	}
	if !strings.Contains(why, "container") {
		t.Errorf("reason = %q", why)
	}
}

func TestMissingImageOrTagIsUnusable(t *testing.T) {
	for _, e := range []Event{
		{Container: "app", Tag: "1.0"},
		{Container: "app", Image: "app"},
	} {
		if _, _, ok := Translate(e); ok {
			t.Errorf("translated an incomplete event: %+v", e)
		}
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	if _, err := Parse([]byte("not json")); err == nil {
		t.Fatal("want an error for a body that is not JSON")
	}
}

// Unknown fields are ignored rather than rejected: the watcher may add to
// its payload, and a gate that refused anything it did not recognise would
// break on a change that affected nothing it uses.
func TestUnknownFieldsAreIgnored(t *testing.T) {
	e, err := Parse([]byte(`{"container":"app","image":"app","tag":"1.0","something_new":"x"}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, why, ok := Translate(e); !ok {
		t.Errorf("an event with extra fields was rejected: %s", why)
	}
}
