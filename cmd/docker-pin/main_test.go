package main

import (
	"errors"
	"fmt"
	"io"
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
    image: nginx:1.25@sha256:afafb16ac47b9b3da9825787ce354896fce376ae6cb1b5a157fa682a122011a1
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
	if !strings.Contains(readCompose(t, f), "nginx:1.25@sha256:afafb16ac47b9b3da9825787ce354896fce376ae6cb1b5a157fa682a122011a1") {
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
		getDigest: func(ref string) (string, error) {
			return "sha256:e96dbed44ed3109d74359ee54137587a2d0a23154bd4c1c7a6497b0ecd415639", nil
		},
		pull: func(ref string) error { pulled = true; return nil },
	}
	if _, err := pinInFile(f, "web", d, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pulled {
		t.Error("should not pull when image is available locally")
	}
	if !strings.Contains(readCompose(t, f), "nginx:1.25@sha256:e96dbed44ed3109d74359ee54137587a2d0a23154bd4c1c7a6497b0ecd415639") {
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
			return "sha256:96d852451a0966be19077d46ce8d1c0db49e9901affa94565fb3e5888d389697", nil
		},
		getDigest: func(ref string) (string, error) {
			return "sha256:aeaf3be9c170ab8c4e58cf31e7e8565efa41a97793fd8f9edf5ee529d7597c83", nil
		},
		pull: func(ref string) error { return errors.New("should not be called") },
	}
	if _, err := pinInFile(f, "web", d, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := readCompose(t, f)
	if !strings.Contains(got, "nginx:latest@sha256:96d852451a0966be19077d46ce8d1c0db49e9901affa94565fb3e5888d389697") {
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
		getDigest: func(ref string) (string, error) {
			return "sha256:e96dbed44ed3109d74359ee54137587a2d0a23154bd4c1c7a6497b0ecd415639", nil
		},
		pull: func(ref string) error { return errors.New("should not be called") },
	}
	if _, err := pinInFile(f, "web", d, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(readCompose(t, f), "nginx:latest@sha256:e96dbed44ed3109d74359ee54137587a2d0a23154bd4c1c7a6497b0ecd415639") {
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
		getDigest: func(ref string) (string, error) {
			return "sha256:e96dbed44ed3109d74359ee54137587a2d0a23154bd4c1c7a6497b0ecd415639", nil
		},
		pull: func(ref string) error { return errors.New("should not be called") },
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
		getDigest: func(ref string) (string, error) {
			return "sha256:e96dbed44ed3109d74359ee54137587a2d0a23154bd4c1c7a6497b0ecd415639", nil
		},
		pull: func(ref string) error { return errors.New("should not be called") },
	}
	out, err := pinInFile(f, "web", d, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Tag != "latest" {
		t.Errorf("tag should stay latest, got %q", out.Tag)
	}
	got := readCompose(t, f)
	if !strings.Contains(got, "nginx:latest@sha256:e96dbed44ed3109d74359ee54137587a2d0a23154bd4c1c7a6497b0ecd415639") {
		t.Errorf("expected latest to be kept as the followed tag, got:\n%s", got)
	}
}

func TestPinInFile_DryRun(t *testing.T) {
	f := writeTempCompose(t, `services:
  web:
    image: nginx:1.25
`)
	d := dockerFuncs{
		getDigest: func(ref string) (string, error) {
			return "sha256:e96dbed44ed3109d74359ee54137587a2d0a23154bd4c1c7a6497b0ecd415639", nil
		},
		pull: func(ref string) error { return errors.New("should not be called") },
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
			return "sha256:4dc03020840f61c091dbd9c853a96890a941f7b205ab28f4e30fd0834494dca0", nil
		},
		pull: func(ref string) error { return nil },
	}
	if _, err := pinInFile(f, "web", d, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(readCompose(t, f), "sha256:4dc03020840f61c091dbd9c853a96890a941f7b205ab28f4e30fd0834494dca0") {
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
		getDigest: func(ref string) (string, error) {
			return "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", nil
		},
		pull: func(ref string) error { return nil },
	}
	if _, err := pinInFile(f, "nonexistent", d, false); err == nil {
		t.Error("expected error for unknown service")
	}
}

// --- upgradeInFile ---

func TestUpgradeInFile_AlreadyUpToDate(t *testing.T) {
	f := writeTempCompose(t, `services:
  db:
    image: postgres:16.3@sha256:e1c671f5f6ed0588a361de450ab5ced6b3cd6bea538fa48d6b283f41b07d03ca
`)
	d := dockerFuncs{
		getDigest: func(ref string) (string, error) {
			return "sha256:e1c671f5f6ed0588a361de450ab5ced6b3cd6bea538fa48d6b283f41b07d03ca", nil
		},
		pull: func(ref string) error { return nil },
	}
	if _, err := upgradeInFile(f, "db", "16.3", d, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(readCompose(t, f), "sha256:e1c671f5f6ed0588a361de450ab5ced6b3cd6bea538fa48d6b283f41b07d03ca") {
		t.Errorf("compose should be unchanged when already up to date")
	}
}

func TestUpgradeInFile_NewDigest(t *testing.T) {
	f := writeTempCompose(t, `services:
  db:
    image: postgres:16.3@sha256:b82ce3889092915fcb00a46d8015882d931424bd70eaf8003868f9a5c305c8ff
`)
	d := dockerFuncs{
		getDigest: func(ref string) (string, error) {
			return "sha256:0a3a3c2576f6dcda223b04cd80017d78924ed32f86d189c61b1680f059e818b8", nil
		},
		pull: func(ref string) error { return nil },
	}
	if _, err := upgradeInFile(f, "db", "16.3", d, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := readCompose(t, f)
	if !strings.Contains(got, "sha256:0a3a3c2576f6dcda223b04cd80017d78924ed32f86d189c61b1680f059e818b8") {
		t.Errorf("expected new digest in compose, got:\n%s", got)
	}
	if strings.Contains(got, "sha256:b82ce3889092915fcb00a46d8015882d931424bd70eaf8003868f9a5c305c8ff") {
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
		{"nginx:1.25@sha256:6ca13d52ca70c883e0f0bb101e425a89e8624de51db2d2392593af6a84118090", "sha256:6ca13d52ca70c883e0f0bb101e425a89e8624de51db2d2392593af6a84118090"},
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
			return fmt.Sprintf("sha256:496b1462ae4cfaf3934f9331f2b36f382a984bcc61d5b3265c13c097aba268eb%s", ref), nil
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

// chdir switches to dir for the duration of the test (t.Chdir needs go1.24,
// and this module targets 1.22).
func chdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(prev) })
}

// captureStdout runs fn with os.Stdout redirected and returns what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		var b strings.Builder
		io.Copy(&b, r)
		done <- b.String()
	}()
	fn()
	w.Close()
	os.Stdout = orig
	return <-done
}

// summaryOrder returns the service column of a "Summary:" table, in the order
// printed.
func summaryOrder(t *testing.T, out string) []string {
	t.Helper()
	var got []string
	seenHeader := false
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if f[0] == "SERVICE" {
			seenHeader = true
			continue
		}
		if seenHeader {
			got = append(got, f[0])
		}
	}
	return got
}

// The dry-run summary is a report a human reads and diffs between runs, so it
// is sorted rather than left in compose-file order (pin) or goroutine
// completion order (upgrade --all, which was nondeterministic).
func TestPinAll_SummaryIsSorted(t *testing.T) {
	dir := t.TempDir()
	// deliberately not alphabetical in the file
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(`services:
  web:
    image: nginx:1.25
  alpha:
    image: alpine:3.20
  mango:
    image: redis:7
`), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)

	d := dockerFuncs{
		getDigest: func(ref string) (string, error) { return "sha256:" + ref, nil },
		pull:      func(ref string) error { return nil },
	}
	out := captureStdout(t, func() {
		if err := pinAll(d, true); err != nil {
			t.Errorf("pinAll: %v", err)
		}
	})

	got := summaryOrder(t, out)
	want := []string{"alpha", "mango", "web"}
	if len(got) != len(want) {
		t.Fatalf("expected %d rows, got %d:\n%s", len(want), len(got), out)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("summary not sorted: got %v, want %v", got, want)
		}
	}
}

// upgrade --all resolves and pulls concurrently, so before sorting the
// summary came out in goroutine-completion order -- a different order every
// run, for a report meant to be diffed.
func TestUpgradeAll_SummaryIsSorted(t *testing.T) {
	dir := t.TempDir()
	// `latest` keeps MovingPullTag off the network; not alphabetical in file.
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(`services:
  web:
    image: nginx:latest@sha256:872737ae8121bc1b984efcbd5f7931d3822326351a30518a018c4c502b95c533
  alpha:
    image: alpine:latest@sha256:893a3cfd3912a125b2419774d2752a3bb8cc175927cd5b421336e4f028607452
  mango:
    image: redis:latest@sha256:3638b1871d9c86acdec8bd57988c81dc8629c007c0e981b399e1c275fe85c55f
`), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)

	d := dockerFuncs{
		getDigest: func(ref string) (string, error) {
			return "sha256:14d73d256d2db4dc98fa448b9bf1a55791e23cb0342d89f0f865dae49d15db44" + ref, nil
		},
		pull: func(ref string) error { return nil },
	}
	out := captureStdout(t, func() {
		if err := upgradeAll(d, true, 4); err != nil {
			t.Errorf("upgradeAll: %v", err)
		}
	})

	got := summaryOrder(t, out)
	want := []string{"alpha", "mango", "web"}
	if len(got) != len(want) {
		t.Fatalf("expected %d rows, got %d:\n%s", len(want), len(got), out)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("summary not sorted: got %v, want %v", got, want)
		}
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

// A locally built image must never be pinned: its repo digest exists only on
// this daemon, so `myapp:local@sha256:...` is unpullable anywhere else --
// precisely the reproducibility failure pinning exists to prevent.
func TestPinInFile_SkipsLocallyBuilt(t *testing.T) {
	f := writeTempCompose(t, `services:
  app:
    build: ./app
    image: myapp:local
`)
	d := dockerFuncs{
		runningDigest: func(dir, service, baseImage string) (string, error) {
			return "", errors.New("should not be consulted for a built service")
		},
		getDigest: func(ref string) (string, error) { return "", errors.New("should not be called") },
		pull:      func(ref string) error { return errors.New("should not be called") },
	}
	out, err := pinInFile(f, "app", d, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.Built {
		t.Error("outcome should be marked Built")
	}
	if strings.Contains(readCompose(t, f), "@sha256:") {
		t.Errorf("a built service must not be pinned, got:\n%s", readCompose(t, f))
	}
}

func TestUpgradeInFile_SkipsLocallyBuilt(t *testing.T) {
	f := writeTempCompose(t, `services:
  app:
    build: ./app
    image: myapp:local
`)
	d := dockerFuncs{
		getDigest: func(ref string) (string, error) { return "", errors.New("should not be called") },
		pull:      func(ref string) error { return errors.New("should not be called") },
	}
	out, err := upgradeInFile(f, "app", "", d, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.Built || out.Changed {
		t.Errorf("expected a skipped built service, got %+v", out)
	}
	if strings.Contains(readCompose(t, f), "@sha256:") {
		t.Errorf("a built service must not be pinned, got:\n%s", readCompose(t, f))
	}
}

// `image: nginx:${TAG}` is compose's to expand, from .env or the environment.
// docker pin reads the file as written, so it sees the variable itself.
//
// Unpinnable either way -- the daemon rejects "nginx:${TAG}" as an invalid
// reference -- but it failed several steps later, with docker's wording and no
// mention of which service or why. Caught before the pull instead.
func TestUnexpandedVariableIsRejectedBeforePulling(t *testing.T) {
	f := writeTempCompose(t, "services:\n  web:\n    image: nginx:${TAG}\n")

	pulled := false
	d := dockerFuncs{
		pull: func(ref string) error { pulled = true; return nil },
		getDigest: func(ref string) (string, error) {
			return "sha256:2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881", nil
		},
	}

	_, err := pinInFile(f, "web", d, false)
	if err == nil {
		t.Fatal("an unexpanded variable should be an error, not something to pull")
	}
	if !strings.Contains(err.Error(), "web") {
		t.Errorf("the error should name the service, got: %v", err)
	}
	if pulled {
		t.Error("nothing should be pulled for a reference that cannot resolve")
	}
	if got := readCompose(t, f); !strings.Contains(got, "nginx:${TAG}") {
		t.Errorf("the file should be left alone, got:\n%s", got)
	}
}

// The upgrade path resolves the reference before pulling it or asking a
// registry which tags exist. Neither works on the literal "${TAG}".
func TestResolvePullRefRejectsAnUnexpandedVariable(t *testing.T) {
	f := writeTempCompose(t, "services:\n  web:\n    image: nginx:${TAG}@sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad\n")

	_, _, err := resolvePullRef(f, "web", "", false, true)
	if err == nil {
		t.Fatal("a variable in the image should stop the upgrade")
	}
	if !strings.Contains(err.Error(), "web") {
		t.Errorf("the error should name the service, got: %v", err)
	}
}

// The same path on a literal reference still resolves -- otherwise the guard
// above would be indistinguishable from refusing everything.
func TestResolvePullRefResolvesALiteralReference(t *testing.T) {
	f := writeTempCompose(t, "services:\n  web:\n    image: nginx:1.25@sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad\n")

	ref, _, err := resolvePullRef(f, "web", "1.26", false, true)
	if err != nil {
		t.Fatalf("a literal reference should resolve: %v", err)
	}
	if ref != "nginx:1.26" {
		t.Errorf("pullRef = %q, want nginx:1.26", ref)
	}
}

// pinInFile's own guard, from the other direction: an ordinary unpinned
// service is still pinned normally.
func TestPinInFileStillPinsALiteralReference(t *testing.T) {
	f := writeTempCompose(t, "services:\n  web:\n    image: nginx:1.25\n")
	d := dockerFuncs{
		getDigest: func(string) (string, error) {
			return "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", nil
		},
		pull: func(string) error { return nil },
	}

	if _, err := pinInFile(f, "web", d, false); err != nil {
		t.Fatalf("an ordinary service should pin: %v", err)
	}
	if got := readCompose(t, f); !strings.Contains(got, "nginx:1.25@sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad") {
		t.Errorf("the service was not pinned, got:\n%s", got)
	}
}

// Carrying a digest is not the same as carrying a usable one. `nginx:1.25@sha256:abc`
// used to report "already pinned" and exit 0 -- the command whose job is to
// guarantee a pin saying the work was done, for a reference the daemon rejects.
func TestPinInFileRejectsAMalformedDigest(t *testing.T) {
	f := writeTempCompose(t, "services:\n  web:\n    image: nginx:1.25@sha256:abc\n")

	touched := false
	d := dockerFuncs{
		getDigest: func(string) (string, error) { touched = true; return "", nil },
		pull:      func(string) error { touched = true; return nil },
	}

	_, err := pinInFile(f, "web", d, false)
	if err == nil {
		t.Fatal("a malformed digest should be an error, not an already-pinned report")
	}
	// The remedies matter more than the wording: both work on a bad digest.
	for _, want := range []string{"unpin", "upgrade"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should point at `docker %s`, got: %v", want, err)
		}
	}
	if touched {
		t.Error("nothing should be pulled for a reference that cannot resolve")
	}
	// Left alone: overwriting would discard something a person wrote.
	if got := readCompose(t, f); !strings.Contains(got, "sha256:abc") {
		t.Errorf("the file should not be rewritten, got:\n%s", got)
	}
}

// The opposite: a well-formed digest still reports already-pinned and succeeds.
// A check that rejected every digest would pass the test above.
func TestPinInFileAcceptsAValidDigest(t *testing.T) {
	const good = "sha256:41b194461e4bae16f9b25d68b0976ed4735b89ca625c89aad88e1c1c3b7e8860"
	f := writeTempCompose(t, "services:\n  web:\n    image: nginx:1.25@"+good+"\n")

	out, err := pinInFile(f, "web", dockerFuncs{}, false)
	if err != nil {
		t.Fatalf("a valid digest should be accepted: %v", err)
	}
	if !out.AlreadyPinned {
		t.Error("a validly pinned service should report as already pinned")
	}
}
