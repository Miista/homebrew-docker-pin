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
	f := writeCompose(t, "services:\n  web:\n    image: nginx:latest@sha256:cba06b5736faf67e54b07b561eae94395e774c517a7d910a54369e1263ccfbd4\n")
	d := Docker{GetDigest: func(string) (string, error) {
		return "sha256:11507a0e2f5e69d5dfa40a62a1bd7b6ee57e6bcd85c67c9b8431b36fff21c437", nil
	}}

	out, err := Compute(f, "web", "nginx:latest", d)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Changed {
		t.Fatal("expected a change")
	}
	if out.NewRaw != "nginx:latest@sha256:11507a0e2f5e69d5dfa40a62a1bd7b6ee57e6bcd85c67c9b8431b36fff21c437" {
		t.Errorf("NewRaw = %q", out.NewRaw)
	}
	if out.Tag != "latest" {
		t.Errorf("Tag = %q, want latest — the tag is the tag to follow", out.Tag)
	}
}

// An explicit version IS a tag change: it is the one operation that moves the
// tag, because moving it is what was asked for.
func TestCompute_ExplicitVersionBecomesTheTag(t *testing.T) {
	f := writeCompose(t, "services:\n  web:\n    image: nginx:latest@sha256:cba06b5736faf67e54b07b561eae94395e774c517a7d910a54369e1263ccfbd4\n")
	d := Docker{GetDigest: func(string) (string, error) {
		return "sha256:11507a0e2f5e69d5dfa40a62a1bd7b6ee57e6bcd85c67c9b8431b36fff21c437", nil
	}}

	out, err := Compute(f, "web", "nginx:1.26.3", d)
	if err != nil {
		t.Fatal(err)
	}
	if out.NewRaw != "nginx:1.26.3@sha256:11507a0e2f5e69d5dfa40a62a1bd7b6ee57e6bcd85c67c9b8431b36fff21c437" || out.Tag != "1.26.3" {
		t.Errorf("got %q / %q", out.NewRaw, out.Tag)
	}
}

func TestCompute_SameDigestIsNoChange(t *testing.T) {
	f := writeCompose(t, "services:\n  web:\n    image: nginx:latest@sha256:0967115f2813a3541eaef77de9d9d5773f1c0c04314b0bbfe4ff3b3b1c55b5d5\n")
	d := Docker{GetDigest: func(string) (string, error) {
		return "sha256:0967115f2813a3541eaef77de9d9d5773f1c0c04314b0bbfe4ff3b3b1c55b5d5", nil
	}}

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
	d := Docker{GetDigest: func(string) (string, error) {
		return "sha256:11507a0e2f5e69d5dfa40a62a1bd7b6ee57e6bcd85c67c9b8431b36fff21c437", nil
	}}

	out, err := Compute(f, "web", "nginx:latest", d)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(f, "web", out); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, f); !strings.Contains(got, "nginx:latest@sha256:11507a0e2f5e69d5dfa40a62a1bd7b6ee57e6bcd85c67c9b8431b36fff21c437") {
		t.Errorf("file not updated:\n%s", got)
	}
}

func TestCompute_UnknownService(t *testing.T) {
	f := writeCompose(t, "services:\n  web:\n    image: nginx:latest\n")
	d := Docker{GetDigest: func(string) (string, error) {
		return "sha256:2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881", nil
	}}
	if _, err := Compute(f, "nope", "nginx:latest", d); err == nil {
		t.Error("an unknown service must be an error, not an empty outcome")
	}
}

func TestCompute_MissingComposeFile(t *testing.T) {
	d := Docker{GetDigest: func(string) (string, error) {
		return "sha256:2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881", nil
	}}
	if _, err := Compute(filepath.Join(t.TempDir(), "nope.yml"), "web", "nginx:latest", d); err == nil {
		t.Error("a missing compose file must be an error")
	}
}

// The image is expected to be present locally by now -- Compute runs after the
// pull. A failure here means something is wrong, and must not be mistaken for
// "no change".
func TestCompute_DigestLookupFailure(t *testing.T) {
	f := writeCompose(t, "services:\n  web:\n    image: nginx:latest@sha256:cba06b5736faf67e54b07b561eae94395e774c517a7d910a54369e1263ccfbd4\n")
	d := Docker{GetDigest: func(string) (string, error) { return "", errors.New("no such image") }}

	out, err := Compute(f, "web", "nginx:latest", d)
	if err == nil {
		t.Fatal("expected an error")
	}
	if out.Changed {
		t.Error("a failed lookup must not report a change")
	}
}

func TestApply_UnknownService(t *testing.T) {
	f := writeCompose(t, "services:\n  web:\n    image: nginx:latest\n")
	err := Apply(f, "nope", Outcome{NewRaw: "nginx:latest@sha256:2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"})
	if err == nil {
		t.Error("applying to an unknown service must be an error")
	}
}

// An unpinned service has no digest to compare against, so anything the
// registry offers is a change.
func TestCompute_UnpinnedServiceIsAlwaysAChange(t *testing.T) {
	f := writeCompose(t, "services:\n  web:\n    image: nginx:1.26.0\n")
	d := Docker{GetDigest: func(string) (string, error) {
		return "sha256:11507a0e2f5e69d5dfa40a62a1bd7b6ee57e6bcd85c67c9b8431b36fff21c437", nil
	}}

	out, err := Compute(f, "web", "nginx:1.26.0", d)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Changed || out.NewRaw != "nginx:1.26.0@sha256:11507a0e2f5e69d5dfa40a62a1bd7b6ee57e6bcd85c67c9b8431b36fff21c437" {
		t.Errorf("got %+v", out)
	}
}

// A registry with a port in the host must not have it mistaken for a tag.
func TestCompute_RegistryWithPort(t *testing.T) {
	f := writeCompose(t, "services:\n  web:\n    image: registry.example.com:5000/app:1.0.0@sha256:cba06b5736faf67e54b07b561eae94395e774c517a7d910a54369e1263ccfbd4\n")
	d := Docker{GetDigest: func(string) (string, error) {
		return "sha256:11507a0e2f5e69d5dfa40a62a1bd7b6ee57e6bcd85c67c9b8431b36fff21c437", nil
	}}

	out, err := Compute(f, "web", "registry.example.com:5000/app:1.0.0", d)
	if err != nil {
		t.Fatal(err)
	}
	if out.Tag != "1.0.0" {
		t.Errorf("Tag = %q, want 1.0.0 -- the port is part of the host", out.Tag)
	}
	if out.NewRaw != "registry.example.com:5000/app:1.0.0@sha256:11507a0e2f5e69d5dfa40a62a1bd7b6ee57e6bcd85c67c9b8431b36fff21c437" {
		t.Errorf("NewRaw = %q", out.NewRaw)
	}
}
