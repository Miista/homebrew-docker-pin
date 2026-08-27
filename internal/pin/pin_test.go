package pin

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCompose(t *testing.T, content string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "docker-compose.yml")
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCompute_PinsPulledDigestUnderPulledTag(t *testing.T) {
	f := writeCompose(t, "services:\n  web:\n    image: nginx:latest@sha256:old\n")
	d := Docker{GetDigest: func(string) (string, error) { return "sha256:new", nil }}

	out, err := Compute(f, "web", "nginx:latest", d)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Changed {
		t.Fatal("expected a change")
	}
	if out.NewRaw != "nginx:latest@sha256:new" {
		t.Errorf("NewRaw = %q", out.NewRaw)
	}
	if out.Tag != "latest" {
		t.Errorf("Tag = %q, want latest — the tag is the tag to follow", out.Tag)
	}
}

// An explicit version IS a tag change: it is the one operation that moves the
// tag, because moving it is what was asked for.
func TestCompute_ExplicitVersionBecomesTheTag(t *testing.T) {
	f := writeCompose(t, "services:\n  web:\n    image: nginx:latest@sha256:old\n")
	d := Docker{GetDigest: func(string) (string, error) { return "sha256:new", nil }}

	out, err := Compute(f, "web", "nginx:1.26.3", d)
	if err != nil {
		t.Fatal(err)
	}
	if out.NewRaw != "nginx:1.26.3@sha256:new" || out.Tag != "1.26.3" {
		t.Errorf("got %q / %q", out.NewRaw, out.Tag)
	}
}

func TestCompute_SameDigestIsNoChange(t *testing.T) {
	f := writeCompose(t, "services:\n  web:\n    image: nginx:latest@sha256:same\n")
	d := Docker{GetDigest: func(string) (string, error) { return "sha256:same", nil }}

	out, err := Compute(f, "web", "nginx:latest", d)
	if err != nil {
		t.Fatal(err)
	}
	if out.Changed {
		t.Error("same digest must not count as a change")
	}
	if out.NewRaw != out.OldRaw {
		t.Errorf("NewRaw %q should equal OldRaw %q", out.NewRaw, out.OldRaw)
	}
}

// A locally built image's digest is local to one daemon, so pinning it yields
// a reference no other host can pull.
func TestCompute_SkipsLocallyBuilt(t *testing.T) {
	f := writeCompose(t, "services:\n  app:\n    build: ./app\n    image: myapp:local\n")
	d := Docker{GetDigest: func(string) (string, error) {
		return "", errors.New("docker must not be consulted for a built service")
	}}

	out, err := Compute(f, "app", "myapp:local", d)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Built || out.Changed {
		t.Errorf("expected a skipped built service, got %+v", out)
	}
}

func TestApply_WritesComputedLine(t *testing.T) {
	f := writeCompose(t, "services:\n  web:\n    image: nginx:latest\n")
	d := Docker{GetDigest: func(string) (string, error) { return "sha256:new", nil }}

	out, err := Compute(f, "web", "nginx:latest", d)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(f, "web", out); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, f); !strings.Contains(got, "nginx:latest@sha256:new") {
		t.Errorf("file not updated:\n%s", got)
	}
}

func TestDigestOfTagOfShortDigest(t *testing.T) {
	const full = "sha256:41b194461e4bae16f9b25d68b0976ed4735b89ca625c89aad88e1c1c3b7e8860"
	for _, tc := range []struct{ in, digest, tag string }{
		{"nginx:latest@" + full, full, "latest"},
		{"nginx:1.26.3", "", "1.26.3"},
		{"registry.example.com:5000/app:v2", "", "v2"},
		{"nginx", "", ""},
	} {
		if got := DigestOf(tc.in); got != tc.digest {
			t.Errorf("DigestOf(%q) = %q, want %q", tc.in, got, tc.digest)
		}
		if got := TagOf(tc.in); got != tc.tag {
			t.Errorf("TagOf(%q) = %q, want %q", tc.in, got, tc.tag)
		}
	}
	if got := ShortDigest(full); got != "sha256:41b194461e4b" {
		t.Errorf("ShortDigest = %q", got)
	}
	if got := ShortDigest("sha256:short"); got != "sha256:short" {
		t.Errorf("ShortDigest should pass through short input, got %q", got)
	}
}
