package diun

import (
	"strings"
	"testing"
)

// The payload captured from diun 4.33 against a real container. Kept verbatim
// rather than reduced to the fields used: if diun changes the shape, the test
// that notices should be reading what diun actually sends.
const realPayload = `{
  "diun_version": "v4.33.0",
  "hostname": "b110ea828afd",
  "status": "update",
  "provider": "docker",
  "image": "docker.io/fnsys/dockhand:latest",
  "hub_link": "https://dockhand.pro",
  "mime_type": "application/vnd.oci.image.index.v1+json",
  "digest": "sha256:4e0c30e703f1435cd1aacf4948794526dee7c0b4b81b8a992196f1cac53e11d3",
  "created": "2026-09-02T14:38:12.544781399Z",
  "platform": "linux/amd64",
  "metadata": {
    "ctn_command": "/sbin/tini -- /usr/local/bin/docker-entrypoint.sh",
    "ctn_createdat": "2026-09-11 12:22:45 +0000 UTC",
    "ctn_id": "9dca70d6637aaf3db85e6a5a393332adb521ca5327fe67d34de3a93567ab2050",
    "ctn_names": "dockhand",
    "ctn_size": "0B",
    "ctn_state": "running",
    "ctn_status": "Up 2 hours (healthy)"
  }
}`

func TestParseRealPayload(t *testing.T) {
	p, err := Parse([]byte(realPayload))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Status != "update" || p.Provider != "docker" {
		t.Errorf("status/provider = %q/%q", p.Status, p.Provider)
	}
	if p.Image != "docker.io/fnsys/dockhand:latest" {
		t.Errorf("image = %q", p.Image)
	}
	if p.Metadata.Names != "dockhand" {
		t.Errorf("container name = %q, want dockhand", p.Metadata.Names)
	}
	if p.Metadata.State != "running" {
		t.Errorf("state = %q", p.Metadata.State)
	}
}

func TestTranslateRealPayload(t *testing.T) {
	p, _ := Parse([]byte(realPayload))
	n, why, ok := Translate(p)
	if !ok {
		t.Fatalf("a real update was not translated: %s", why)
	}
	if n.Container != "dockhand" {
		t.Errorf("container = %q", n.Container)
	}
	if n.Image != "docker.io/fnsys/dockhand:latest" {
		t.Errorf("image = %q", n.Image)
	}
	if !strings.HasPrefix(n.Digest, "sha256:") {
		t.Errorf("digest = %q", n.Digest)
	}
}

// A first sighting is diun recording a baseline, not reporting a change.
// Acting on one would treat every newly-watched image as an update to itself.
func TestFirstSightingIsNotASignal(t *testing.T) {
	p := Payload{Status: StatusNew, Image: "app:1.0", Digest: "sha256:abc"}
	p.Metadata.Names = "app"
	_, why, ok := Translate(p)
	if ok {
		t.Fatal("a first sighting was treated as a change")
	}
	if !strings.Contains(why, "first sighting") {
		t.Errorf("reason = %q", why)
	}
}

func TestUnrecognisedStatusIsNotASignal(t *testing.T) {
	p := Payload{Status: "something-else", Image: "app:1.0", Digest: "sha256:abc"}
	p.Metadata.Names = "app"
	_, why, ok := Translate(p)
	if ok {
		t.Fatal("an unknown status was treated as a change")
	}
	if !strings.Contains(why, "something-else") {
		t.Errorf("the reason should quote what it saw, got %q", why)
	}
}

// The container name is the only thing that maps a notification to a service.
// A file-provider notification has none, and describes an image nothing on
// this host necessarily runs.
func TestNoContainerNameIsNotASignal(t *testing.T) {
	p := Payload{Status: StatusUpdate, Provider: "file", Image: "app:1.0", Digest: "sha256:abc"}
	_, why, ok := Translate(p)
	if ok {
		t.Fatal("a notification with no container was translated")
	}
	if !strings.Contains(why, "container") {
		t.Errorf("reason = %q", why)
	}
}

func TestMissingImageOrDigestIsNotASignal(t *testing.T) {
	for _, p := range []Payload{
		{Status: StatusUpdate, Digest: "sha256:abc"},
		{Status: StatusUpdate, Image: "app:1.0"},
	} {
		p.Metadata.Names = "app"
		if _, _, ok := Translate(p); ok {
			t.Errorf("translated an incomplete payload: %+v", p)
		}
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	if _, err := Parse([]byte("not json")); err == nil {
		t.Fatal("want an error for a body that is not JSON")
	}
}

// Unknown fields are ignored rather than rejected: diun adds to its payload
// between versions, and a translator that refused anything it did not
// recognise would break on an upgrade that changed nothing it uses.
func TestUnknownFieldsAreIgnored(t *testing.T) {
	p, err := Parse([]byte(`{"status":"update","image":"app:1.0","digest":"sha256:abc",
		"something_new":"whatever","metadata":{"ctn_names":"app","ctn_future":"x"}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, why, ok := Translate(p); !ok {
		t.Errorf("a payload with extra fields was rejected: %s", why)
	}
}
