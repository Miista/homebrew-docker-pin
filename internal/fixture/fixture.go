// Package fixture builds randomised-but-valid test data: compose projects and
// the registry answers that go with them.
//
// The point is that a test says only what it is actually asserting about.
// "a service with an update that policy would apply automatically" is the
// interesting part; the service name, registry, image and version numbers are
// not, so they are random. A hardcoded 1.2.0 -> 1.3.0 risks passing because
// those particular strings happen to line up.
//
// Randomness is seeded per test run and the seed is logged, so a failure can
// be reproduced by pinning it.
package fixture

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// New returns a fixture builder with its own source of randomness. The seed is
// logged so a failing run can be reproduced with NewSeeded.
func New(t *testing.T) *Fixture {
	t.Helper()
	seed := time.Now().UnixNano()
	t.Logf("fixture seed: %d (reproduce with fixture.NewSeeded(t, %d))", seed, seed)
	return NewSeeded(t, seed)
}

// NewSeeded returns a builder with a fixed seed, for reproducing a failure.
func NewSeeded(t *testing.T, seed int64) *Fixture {
	t.Helper()
	return &Fixture{t: t, rnd: rand.New(rand.NewSource(seed))}
}

type Fixture struct {
	t   *testing.T
	rnd *rand.Rand
}

// Service describes one service in a generated project, and the registry
// answers that should be given for it.
type Service struct {
	Name  string
	Image string
	// Tag and Digest are what the compose file pins.
	Tag, Digest string
	// Pinned false writes the image with no digest, so duva skips it.
	Pinned bool
	// Built writes a build: key, so duva skips it.
	Built bool
	// Labels are the duva.* rules, already stringified.
	Labels map[string]string

	// AvailableTags is what the registry reports for this image. Empty means
	// only the current tag exists.
	AvailableTags []string
	// AvailableDigest is what a moving tag currently points at. Empty means
	// unchanged from Digest.
	AvailableDigest string
	// Published is when candidate tags were published, for the soak.
	Published time.Time
}

// Project writes a compose file containing the given services and returns its
// path. The directory is a t.TempDir, so it is real: duva reads it with the
// real compose parser rather than a mock of one.
func (f *Fixture) Project(services ...Service) string {
	f.t.Helper()
	dir := f.t.TempDir()
	return f.ProjectIn(dir, services...)
}

// ProjectIn is Project with a caller-chosen directory, for tests that need the
// path to be stable or shared.
func (f *Fixture) ProjectIn(dir string, services ...Service) string {
	f.t.Helper()
	var b strings.Builder
	b.WriteString("services:\n")
	for _, s := range services {
		fmt.Fprintf(&b, "  %s:\n", s.Name)
		if s.Built {
			b.WriteString("    build: ./" + s.Name + "\n")
		}
		image := s.Image + ":" + s.Tag
		if s.Pinned {
			image += "@" + s.Digest
		}
		fmt.Fprintf(&b, "    image: %s\n", image)
		if len(s.Labels) > 0 {
			b.WriteString("    labels:\n")
			for _, k := range sortedKeys(s.Labels) {
				fmt.Fprintf(&b, "      %s: '%s'\n", k, s.Labels[k])
			}
		}
	}

	path := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return path
}

// --- randomised building blocks ----------------------------------------

var registries = []string{"docker.io", "ghcr.io/acme", "registry.example.com:5000/team", "quay.io/org"}

// ServiceName returns a random compose-legal service name.
func (f *Fixture) ServiceName() string {
	words := []string{"api", "web", "cache", "queue", "worker", "store", "edge", "index"}
	return fmt.Sprintf("%s-%d", words[f.rnd.Intn(len(words))], f.rnd.Intn(1000))
}

// Image returns a random image reference without a tag.
func (f *Fixture) Image() string {
	return fmt.Sprintf("%s/%s", registries[f.rnd.Intn(len(registries))], f.ServiceName())
}

// Digest returns a random but well-formed sha256 digest.
func (f *Fixture) Digest() string {
	const hex = "0123456789abcdef"
	b := make([]byte, 64)
	for i := range b {
		b[i] = hex[f.rnd.Intn(len(hex))]
	}
	return "sha256:" + string(b)
}

// Version returns a random semver-shaped version, deliberately avoiding 0 in
// every position so that a decrement is always possible.
func (f *Fixture) Version() (major, minor, patch int) {
	return 1 + f.rnd.Intn(20), 1 + f.rnd.Intn(20), 1 + f.rnd.Intn(20)
}

// VersionString formats a version.
func VersionString(major, minor, patch int) string {
	return fmt.Sprintf("%d.%d.%d", major, minor, patch)
}

// --- service presets ----------------------------------------------------

// PinnedService returns a pinned service on a random semver tag with no
// update available and no rules.
func (f *Fixture) PinnedService() Service {
	ma, mi, pa := f.Version()
	tag := VersionString(ma, mi, pa)
	return Service{
		Name:          f.ServiceName(),
		Image:         f.Image(),
		Tag:           tag,
		Digest:        f.Digest(),
		Pinned:        true,
		AvailableTags: []string{tag},
		Published:     time.Now().Add(-90 * 24 * time.Hour),
	}
}

// WithUpdate returns a copy of s with a newer tag available, of the given
// size, and an include rule that admits it.
func (f *Fixture) WithUpdate(s Service, kind Bump) Service {
	ma, mi, pa := parseVersion(f.t, s.Tag)
	var next string
	switch kind {
	case BumpPatch:
		next = VersionString(ma, mi, pa+1+f.rnd.Intn(3))
	case BumpMinor:
		next = VersionString(ma, mi+1+f.rnd.Intn(3), 0)
	case BumpMajor:
		next = VersionString(ma+1+f.rnd.Intn(3), 0, 0)
	}
	s.AvailableTags = append([]string{s.Tag}, next)
	s.Labels = merge(s.Labels, map[string]string{"duva.include": `^\d+\.\d+\.\d+$`})
	return s
}

// WithAuto sets the service's duva.auto threshold.
func (f *Fixture) WithAuto(s Service, auto string) Service {
	s.Labels = merge(s.Labels, map[string]string{"duva.auto": auto})
	return s
}

// MovingTagService returns a service following a moving tag, with no version
// constraint, whose tag currently points at a different digest.
func (f *Fixture) MovingTagService(moved bool) Service {
	s := Service{
		Name:      f.ServiceName(),
		Image:     f.Image(),
		Tag:       []string{"latest", "dev", "stable"}[f.rnd.Intn(3)],
		Digest:    f.Digest(),
		Pinned:    true,
		Published: time.Now().Add(-90 * 24 * time.Hour),
	}
	s.AvailableDigest = s.Digest
	if moved {
		s.AvailableDigest = f.Digest()
	}
	return s
}

// UnpinnedService returns a service with no digest, which duva must skip.
func (f *Fixture) UnpinnedService() Service {
	s := f.PinnedService()
	s.Pinned = false
	return s
}

// BuiltService returns a locally built service, which duva must skip.
func (f *Fixture) BuiltService() Service {
	s := f.PinnedService()
	s.Built = true
	return s
}

// Bump is how large an available update is.
type Bump string

const (
	BumpPatch Bump = "patch"
	BumpMinor Bump = "minor"
	BumpMajor Bump = "major"
)

// --- registry answers ---------------------------------------------------

// Registry returns registry answers for the given services: tag lists,
// moving-tag digests and publish dates, keyed by image.
//
// This is the one boundary the tests fake, because it is the one that leaves
// the machine. Everything else -- compose parsing, state, policy -- runs for
// real, so no assumption about them is baked into a mock.
func (f *Fixture) Registry(services ...Service) RegistryAnswers {
	tags := map[string][]string{}
	digests := map[string]string{}
	published := map[string]time.Time{}
	for _, s := range services {
		tags[s.Image] = s.AvailableTags
		if s.AvailableDigest != "" {
			digests[s.Image] = s.AvailableDigest
		}
		when := s.Published
		if when.IsZero() {
			when = time.Now().Add(-90 * 24 * time.Hour)
		}
		published[s.Image] = when
	}
	return RegistryAnswers{Tags: tags, Digests: digests, Published: published}
}

// RegistryAnswers is a canned registry, addressable by image.
type RegistryAnswers struct {
	Tags      map[string][]string
	Digests   map[string]string
	Published map[string]time.Time
	// Fail, when set, makes every lookup for that image return an error.
	Fail map[string]error
}

// ListMatchingTags implements the tag-listing half of a registry.
func (r RegistryAnswers) ListMatchingTags(image string, include, exclude *regexp.Regexp, current string) ([]string, error) {
	if err := r.Fail[image]; err != nil {
		return nil, err
	}
	var out []string
	for _, t := range r.Tags[image] {
		if include != nil && !include.MatchString(t) {
			continue
		}
		if exclude != nil && exclude.MatchString(t) {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// RemoteDigest implements the moving-tag half.
func (r RegistryAnswers) RemoteDigest(image, tag string) (string, error) {
	if err := r.Fail[image]; err != nil {
		return "", err
	}
	if d, ok := r.Digests[image]; ok {
		return d, nil
	}
	return "", fmt.Errorf("no digest for %s:%s", image, tag)
}

// TagCreated implements publish dates, for the soak.
func (r RegistryAnswers) TagCreated(image, tag string) (time.Time, error) {
	if err := r.Fail[image]; err != nil {
		return time.Time{}, err
	}
	if when, ok := r.Published[image]; ok {
		return when, nil
	}
	return time.Now().Add(-90 * 24 * time.Hour), nil
}

// --- helpers ------------------------------------------------------------

func merge(into, from map[string]string) map[string]string {
	if into == nil {
		into = map[string]string{}
	}
	for k, v := range from {
		into[k] = v
	}
	return into
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}

func parseVersion(t *testing.T, tag string) (major, minor, patch int) {
	t.Helper()
	if _, err := fmt.Sscanf(tag, "%d.%d.%d", &major, &minor, &patch); err != nil {
		t.Fatalf("fixture: %q is not a semver tag: %v", tag, err)
	}
	return major, minor, patch
}
