package queue

import (
	"errors"
	"fmt"

	"github.com/Miista/homebrew-docker-pin/compose"
)

// Lookup finds what a compose project declares for the service behind a
// container name.
//
// This is the whole of the queue's impurity: everything else in the package
// takes a Service and gives an answer. Kept in one place so the boundary
// between "reads the world" and "decides about it" is a function call rather
// than a habit.
type Lookup struct {
	// Root is the compose file to read from. include: is resolved from it.
	Root string
	// Labels reads policy from the container itself, by container_name, and
	// reports whether there is one. Nil reads it from the compose file.
	//
	// duva v5 sets it: the running container is the truth about what governs
	// a service, and a label edited in the file takes effect when the
	// container is recreated, not before.
	Labels func(container string) (map[string]string, bool, error)
}

// Service returns what the project declares for a container name.
//
// found is false when no service claims that container, which the caller must
// treat as something to report rather than something to drop: it means either
// the watcher is watching what the compose file does not describe, or a
// service lost its container_name and went invisible.
func (l Lookup) Service(container string) (Service, bool, error) {
	index, err := compose.ContainerIndex(l.Root)
	if err != nil {
		return Service{}, false, err
	}
	ref, ok := index[container]
	if !ok {
		return Service{}, false, nil
	}
	return l.declared(container, ref)
}

// ByService is Service for a compose service name, which is what a queued
// entry records.
//
// Through the project as it is now rather than the file an entry was queued
// from: a service moved into a renamed include: is still found.
func (l Lookup) ByService(name string) (Service, bool, error) {
	index, err := compose.ContainerIndex(l.Root)
	if err != nil {
		return Service{}, false, err
	}
	for container, ref := range index {
		if ref.Service == name {
			return l.declared(container, ref)
		}
	}
	return Service{}, false, nil
}

// declared reads one service's image and policy.
func (l Lookup) declared(container string, ref compose.ServiceRef) (Service, bool, error) {
	raw, err := compose.RawImage(ref.File, ref.Service)
	if err != nil {
		return Service{}, false, fmt.Errorf("reading %s's image: %w", ref.Service, err)
	}
	// Read as written, so what is decided about is what the file says rather
	// than what compose would substitute. A reference with an unexpanded
	// variable cannot be reasoned about at all.
	if compose.HasUnexpandedVariable(raw) {
		return Service{}, false, fmt.Errorf("%s's image has an unexpanded variable: %s", ref.Service, raw)
	}

	base, tag, err := compose.ParseImage(ref.File, ref.Service)
	if err != nil {
		return Service{}, false, fmt.Errorf("parsing %s's image: %w", ref.Service, err)
	}

	labels, err := l.labels(container, ref)
	if err != nil {
		return Service{}, false, err
	}
	auto, err := ParseAuto(labels["duva.auto"])
	if err != nil {
		// A policy that cannot be read is not a policy to guess at. Erring
		// here means the service is reported rather than silently treated as
		// the default -- and the default is the permissive direction for a
		// typo only if you assume the typo was in the safe direction.
		return Service{}, false, fmt.Errorf("%s: %w", ref.Service, err)
	}
	// Per service, with no host default: a soak is a judgement about one
	// image's upstream, the same as duva.auto, and lives beside it.
	// duva.defer. The earlier names, duva.soak and duva.delay, are refused
	// rather than ignored: ignored, a service carrying one would lose its
	// deferral silently.
	for _, old := range []string{"duva.soak", "duva.delay"} {
		if _, set := labels[old]; set {
			return Service{}, false, fmt.Errorf("%s: %s is now duva.defer", ref.Service, old)
		}
	}
	soak, err := ParseSoak(labels["duva.defer"])
	if err != nil {
		return Service{}, false, fmt.Errorf("%s: %w", ref.Service, err)
	}
	ignored, err := ParseIgnore(labels["duva.ignore"])
	if err != nil {
		return Service{}, false, fmt.Errorf("%s: %w", ref.Service, err)
	}

	return Service{
		Name:      ref.Service,
		Container: container,
		File:      ref.File,
		Image:     base,
		Tag:       tag,
		Digest:    compose.DigestOf(raw),
		Auto:      auto,
		Soak:      soak,
		Source:    SourceOf(labels["org.opencontainers.image.source"]),
		Ignored:   ignored,
	}, true, nil
}

// ErrNoContainer is a service declared in the compose file with no container
// on this host. Not a fault: nothing runs, so nothing can be updated, and the
// service is asked about again once its container is back.
var ErrNoContainer = errors.New("no container")

// labels is a service's policy, from wherever this Lookup reads it.
func (l Lookup) labels(container string, ref compose.ServiceRef) (map[string]string, error) {
	if l.Labels == nil {
		labels, err := compose.Labels(ref.File, ref.Service)
		if err != nil {
			return nil, fmt.Errorf("reading %s's labels: %w", ref.Service, err)
		}
		return labels, nil
	}
	labels, found, err := l.Labels(container)
	if err != nil {
		return nil, fmt.Errorf("reading %s's labels from its container: %w", ref.Service, err)
	}
	if !found {
		// Not the file's labels instead: those describe a container that
		// does not exist, and acting on them would be acting on a policy
		// nothing is running under.
		return nil, fmt.Errorf("%w: %s has no container named %s, so there is no policy to read", ErrNoContainer, ref.Service, container)
	}
	return labels, nil
}
