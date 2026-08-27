package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempCompose(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	f := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func readCompose(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// --- pinInFile ---

func TestPinInFile_AlreadyPinned(t *testing.T) {
	f := writeTempCompose(t, `services:
  web:
    image: nginx:1.25@sha256:existing
`)
	called := false
	d := dockerFuncs{
		getDigest: func(ref string) (string, error) { called = true; return "", errors.New("should not be called") },
		pull:      func(ref string) error { called = true; return errors.New("should not be called") },
	}
	if _, err := pinInFile(f, "web", d, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Error("docker should not be called when image is already pinned")
	}
	if !strings.Contains(readCompose(t, f), "nginx:1.25@sha256:existing") {
		t.Error("compose file should be unchanged")
	}
}

func TestPinInFile_LocalImage(t *testing.T) {
	f := writeTempCompose(t, `services:
  web:
    image: nginx:1.25
`)
	pulled := false
	d := dockerFuncs{
		getDigest: func(ref string) (string, error) { return "sha256:localhash", nil },
		pull:      func(ref string) error { pulled = true; return nil },
	}
	if _, err := pinInFile(f, "web", d, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pulled {
		t.Error("should not pull when image is available locally")
	}
	if !strings.Contains(readCompose(t, f), "nginx:1.25@sha256:localhash") {
		t.Errorf("expected pinned image in compose, got:\n%s", readCompose(t, f))
	}
}

// The reason runningDigest exists: pin is normally run after a stack has been
// up a while, and in that window something else can re-pull the moving tag.
// The local image for `latest` then points somewhere newer than the container
// is actually running, and pinning that would record a digest that never ran.
func TestPinInFile_PrefersRunningContainer(t *testing.T) {
	f := writeTempCompose(t, `services:
  web:
    image: nginx:latest
`)
	d := dockerFuncs{
		runningDigest: func(dir, service, baseImage string) (string, error) {
			if service != "web" || baseImage != "nginx" {
				t.Errorf("unexpected lookup: service=%q baseImage=%q", service, baseImage)
			}
			return "sha256:whatisrunning", nil
		},
		getDigest: func(ref string) (string, error) { return "sha256:newerlocal", nil },
		pull:      func(ref string) error { return errors.New("should not be called") },
	}
	if _, err := pinInFile(f, "web", d, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := readCompose(t, f)
	if !strings.Contains(got, "nginx:latest@sha256:whatisrunning") {
		t.Errorf("expected the running container's digest to win, got:\n%s", got)
	}
}

// No container for the service (never brought up, or it belongs to another
// host) falls back to the local image.
func TestPinInFile_FallsBackToLocalWhenNotRunning(t *testing.T) {
	f := writeTempCompose(t, `services:
  web:
    image: nginx:latest
`)
	d := dockerFuncs{
		runningDigest: func(dir, service, baseImage string) (string, error) { return "", nil },
		getDigest:     func(ref string) (string, error) { return "sha256:localhash", nil },
		pull:          func(ref string) error { return errors.New("should not be called") },
	}
	if _, err := pinInFile(f, "web", d, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(readCompose(t, f), "nginx:latest@sha256:localhash") {
		t.Errorf("expected fallback to the local image, got:\n%s", readCompose(t, f))
	}
}

// A container running an image with no usable repo digest (locally built, or
// pruned after the tag moved) must not block pinning -- fall through.
func TestPinInFile_RunningLookupFailureIsFatal(t *testing.T) {
	f := writeTempCompose(t, `services:
  web:
    image: nginx:latest
`)
	d := dockerFuncs{
		runningDigest: func(dir, service, baseImage string) (string, error) {
			return "", errors.New("docker daemon unreachable")
		},
		getDigest: func(ref string) (string, error) { return "sha256:localhash", nil },
		pull:      func(ref string) error { return errors.New("should not be called") },
	}
	if _, err := pinInFile(f, "web", d, false); err == nil {
		t.Fatal("expected a docker failure to surface rather than silently pinning something else")
	}
	if strings.Contains(readCompose(t, f), "@sha256:") {
		t.Error("compose file should be untouched when the running lookup errors")
	}
}

// The tag is the tag to FOLLOW, so pinning must write it back verbatim.
// Resolving `latest` to whatever version tag happens to carry the same digest
// would freeze the service on that version line: the concrete tag never moves,
// so it would silently stop receiving updates.
func TestPinInFile_KeepsMovingTag(t *testing.T) {
	f := writeTempCompose(t, `services:
  web:
    image: nginx:latest
`)
	d := dockerFuncs{
		getDigest: func(ref string) (string, error) { return "sha256:localhash", nil },
		pull:      func(ref string) error { return errors.New("should not be called") },
	}
	out, err := pinInFile(f, "web", d, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Tag != "latest" {
		t.Errorf("tag should stay latest, got %q", out.Tag)
	}
	got := readCompose(t, f)
	if !strings.Contains(got, "nginx:latest@sha256:localhash") {
		t.Errorf("expected latest to be kept as the followed tag, got:\n%s", got)
	}
}

func TestPinInFile_DryRun(t *testing.T) {
	f := writeTempCompose(t, `services:
  web:
    image: nginx:1.25
`)
	d := dockerFuncs{
		getDigest: func(ref string) (string, error) { return "sha256:localhash", nil },
		pull:      func(ref string) error { return errors.New("should not be called") },
	}
	if _, err := pinInFile(f, "web", d, true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(readCompose(t, f), "nginx:1.25\n") || strings.Contains(readCompose(t, f), "@sha256:") {
		t.Errorf("dry run should not modify the compose file, got:\n%s", readCompose(t, f))
	}
}

func TestPinInFile_PullsWhenNotLocal(t *testing.T) {
	f := writeTempCompose(t, `services:
  web:
    image: nginx:1.25
`)
	callCount := 0
	d := dockerFuncs{
		getDigest: func(ref string) (string, error) {
			callCount++
			if callCount == 1 {
				return "", errors.New("not found locally")
			}
			return "sha256:pulledhash", nil
		},
		pull: func(ref string) error { return nil },
	}
	if _, err := pinInFile(f, "web", d, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(readCompose(t, f), "sha256:pulledhash") {
		t.Errorf("expected pinned image after pull, got:\n%s", readCompose(t, f))
	}
}

func TestPinInFile_PullFails(t *testing.T) {
	f := writeTempCompose(t, `services:
  web:
    image: nginx:1.25
`)
	d := dockerFuncs{
		getDigest: func(ref string) (string, error) { return "", errors.New("not found") },
		pull:      func(ref string) error { return errors.New("pull failed: network error") },
	}
	_, err := pinInFile(f, "web", d, false)
	if err == nil {
		t.Fatal("expected error when pull fails")
	}
	if !strings.Contains(err.Error(), "pull failed") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPinInFile_UnknownService(t *testing.T) {
	f := writeTempCompose(t, `services:
  web:
    image: nginx:1.25
`)
	d := dockerFuncs{
		getDigest: func(ref string) (string, error) { return "sha256:abc", nil },
		pull:      func(ref string) error { return nil },
	}
	if _, err := pinInFile(f, "nonexistent", d, false); err == nil {
		t.Error("expected error for unknown service")
	}
}

// --- upgradeInFile ---

func TestUpgradeInFile_AlreadyUpToDate(t *testing.T) {
	f := writeTempCompose(t, `services:
  db:
    image: postgres:16.3@sha256:currenthash
`)
	d := dockerFuncs{
		getDigest: func(ref string) (string, error) { return "sha256:currenthash", nil },
		pull:      func(ref string) error { return nil },
	}
	if _, err := upgradeInFile(f, "db", "16.3", d, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(readCompose(t, f), "sha256:currenthash") {
		t.Errorf("compose should be unchanged when already up to date")
	}
}

func TestUpgradeInFile_NewDigest(t *testing.T) {
	f := writeTempCompose(t, `services:
  db:
    image: postgres:16.3@sha256:oldhash
`)
	d := dockerFuncs{
		getDigest: func(ref string) (string, error) { return "sha256:newhash", nil },
		pull:      func(ref string) error { return nil },
	}
	if _, err := upgradeInFile(f, "db", "16.3", d, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := readCompose(t, f)
	if !strings.Contains(got, "sha256:newhash") {
		t.Errorf("expected new digest in compose, got:\n%s", got)
	}
	if strings.Contains(got, "sha256:oldhash") {
		t.Errorf("old digest should be gone, got:\n%s", got)
	}
}

func TestUpgradeInFile_PullFails(t *testing.T) {
	f := writeTempCompose(t, `services:
  db:
    image: postgres:16
`)
	d := dockerFuncs{
		getDigest: func(ref string) (string, error) { return "", errors.New("not found") },
		pull:      func(ref string) error { return errors.New("registry unreachable") },
	}
	_, err := upgradeInFile(f, "db", "16", d, false)
	if err == nil {
		t.Fatal("expected error when pull fails")
	}
}

// --- digestOf ---

func TestDigestOf(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"nginx:1.25@sha256:abc123", "sha256:abc123"},
		{"nginx:1.25", ""},
		{"nginx", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := digestOf(tt.input)
			if got != tt.want {
				t.Errorf("digestOf(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- pinAll / upgradeAll via temp dir ---

func TestPinAll(t *testing.T) {
	f := writeTempCompose(t, `services:
  web:
    image: nginx:1.25
  db:
    image: postgres:16
`)
	calls := map[string]int{}
	d := dockerFuncs{
		getDigest: func(ref string) (string, error) {
			calls[ref]++
			return fmt.Sprintf("sha256:digest-%s", ref), nil
		},
		pull: func(ref string) error { return nil },
	}

	// pinAll uses os.Getwd + FindFile, so exercise pinInFile directly per service
	for _, svc := range []string{"web", "db"} {
		if _, err := pinInFile(f, svc, d, false); err != nil {
			t.Fatalf("pinInFile(%s): %v", svc, err)
		}
	}

	got := readCompose(t, f)
	if !strings.Contains(got, "@sha256:") {
		t.Errorf("expected both services pinned, got:\n%s", got)
	}
	if strings.Count(got, "@sha256:") != 2 {
		t.Errorf("expected 2 pinned services, got:\n%s", got)
	}
}

// --- listInFile ---

const listCompose = `services:
  db:
    image: postgres:16.3@sha256:a3dc6bd4a4a5a26a2e04ecb548d94a52b972e01e6b18ad4e37d51703cd0eeecd
  plex:
    image: plexinc/pms-docker:latest
  web:
    image: nginx:1.25@sha256:b2e814d28359e77bd0aa5fed1939620075e4ffa0eb20423cc557b375bd5c14ad
`

func TestListInFile_Table(t *testing.T) {
	f := writeTempCompose(t, listCompose)
	var buf strings.Builder
	unpinned, err := listInFile(f, false, false, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if unpinned != 1 {
		t.Errorf("unpinned = %d, want 1", unpinned)
	}
	out := buf.String()
	for _, want := range []string{"SERVICE", "db", "plex", "web", "a3dc6bd4a4a5", "✓", "✗"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// Sorted: db before plex before web.
	if strings.Index(out, "db") > strings.Index(out, "plex") || strings.Index(out, "plex") > strings.Index(out, "web") {
		t.Errorf("services not sorted:\n%s", out)
	}
}

func TestListInFile_MissingQuiet(t *testing.T) {
	f := writeTempCompose(t, listCompose)
	var buf strings.Builder
	unpinned, err := listInFile(f, true, true, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if unpinned != 1 {
		t.Errorf("unpinned = %d, want 1", unpinned)
	}
	if got := buf.String(); got != "plex\n" {
		t.Errorf("quiet --missing output = %q, want %q", got, "plex\n")
	}
}

func TestListInFile_AllPinnedQuiet(t *testing.T) {
	f := writeTempCompose(t, `services:
  db:
    image: postgres:16.3@sha256:a3dc6bd4a4a5a26a2e04ecb548d94a52b972e01e6b18ad4e37d51703cd0eeecd
`)
	var buf strings.Builder
	unpinned, err := listInFile(f, true, true, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if unpinned != 0 {
		t.Errorf("unpinned = %d, want 0", unpinned)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no output, got %q", buf.String())
	}
}
