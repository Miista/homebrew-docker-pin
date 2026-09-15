package queue

import (
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

	labels, err := compose.Labels(ref.File, ref.Service)
	if err != nil {
		return Service{}, false, fmt.Errorf("reading %s's labels: %w", ref.Service, err)
	}
	auto, err := ParseAuto(labels["duva.auto"])
	if err != nil {
		// A policy that cannot be read is not a policy to guess at. Erring
		// here means the service is reported rather than silently treated as
		// the default -- and the default is the permissive direction for a
		// typo only if you assume the typo was in the safe direction.
		return Service{}, false, fmt.Errorf("%s: %w", ref.Service, err)
	}

	return Service{
		Name:   ref.Service,
		File:   ref.File,
		Image:  base,
		Tag:    tag,
		Digest: compose.DigestOf(raw),
		Auto:   auto,
	}, true, nil
}
