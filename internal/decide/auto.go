package decide

import (
	"fmt"

	"github.com/Miista/homebrew-docker-pin/internal/version"
)

// Auto is how large a change a service will take without being asked.
//
// A copy of the ladder duva has today rather than an import of it: this
// package is meant to be the whole of the gate, and a decider that reached
// into duva's internals for its policy type would not be separable from duva.
// The values are the same because the meaning is the same, and because a
// service's existing duva.auto label must keep meaning what it meant.
type Auto string

const (
	// AutoNone: never act; every change waits for a human. The default, and
	// what every service on both hosts is set to today.
	AutoNone Auto = "none"
	// AutoPatch: take patches.
	AutoPatch Auto = "patch"
	// AutoMinor: take patches and minors.
	AutoMinor Auto = "minor"
	// AutoMajor: take anything, including what could not be classified.
	AutoMajor Auto = "major"
)

// ParseAuto reads a duva.auto label value. An empty value means the label is
// absent, which is AutoNone.
//
// An unrecognised value is an error rather than a fallback. `duva.auto: pathc`
// silently meaning "none" is the kind of thing discovered months later, when
// something has not been happening and nobody knows why — and the opposite
// mistake, a typo that silently widened what may be applied unattended, is
// worse still.
func ParseAuto(value string) (Auto, error) {
	switch Auto(value) {
	case "":
		return AutoNone, nil
	case AutoNone, AutoPatch, AutoMinor, AutoMajor:
		return Auto(value), nil
	default:
		return AutoNone, fmt.Errorf("duva.auto: unknown value %q (want patch, minor, major or none)", value)
	}
}

// rank orders the ladder. Higher takes more.
func (a Auto) rank() int {
	switch a {
	case AutoPatch:
		return 1
	case AutoMinor:
		return 2
	case AutoMajor:
		return 3
	default:
		return 0
	}
}

// rank is the size of an actual change, on the same ladder.
//
// Unknown ranks with major: if the size of a change cannot be measured, it is
// not one to make unattended. That is deliberately the conservative reading —
// an unclassifiable change is more likely to be a flavour switch or a
// prerelease than a quiet patch.
func rank(k version.Kind) int {
	switch k {
	case version.KindPatch:
		return 1
	case version.KindMinor:
		return 2
	case version.KindMajor, version.KindUnknown:
		return 3
	default:
		return 0
	}
}
