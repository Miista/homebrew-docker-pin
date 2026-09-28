package queue

import (
	"fmt"

	"github.com/Miista/homebrew-docker-pin/oci/version"
)

// Auto is how large a change a service will take without being asked.
//
// A copy of the ladder duva has today rather than an import of it: this
// package is meant to be the whole of the gate, and a queue that reached
// into duva's internals for its policy type would not be separable from duva.
// The values are the same because the meaning is the same, and because a
// service's existing duva.auto label must keep meaning what it meant.
type Auto string

const (
	// AutoNone: never act; every change waits for a human. The default, and
	// what every service on both hosts is set to today.
	AutoNone Auto = "none"
	// AutoDigest: take digest moves on the tag already followed, and nothing
	// else. Not a rung below AutoPatch but a different mode: a digest move has
	// no magnitude to threshold on, so this answers "which stream do you
	// follow" where the rest answer "how far along it will you move".
	//
	// A service on `latest` that wants what `latest` points at, without also
	// consenting to 1.4.2 -> 1.4.3, has no other way to say so: every rung of
	// the ladder implies digest moves, so asking for them used to mean asking
	// for a version policy you did not want.
	AutoDigest Auto = "digest"
	// AutoPatch: take digest moves and patches.
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
	case AutoNone, AutoDigest, AutoPatch, AutoMinor, AutoMajor:
		return Auto(value), nil
	default:
		return AutoNone, fmt.Errorf("duva.auto: unknown value %q (want digest, patch, minor, major or none)", value)
	}
}

// TakesDigestMoves reports whether a digest move on an already-followed tag
// may be applied unattended.
//
// Every value but none does. AutoDigest is the one that does only this, which
// is why this is a predicate rather than a rung on rank(): ordering digest
// against patch would claim a magnitude it does not have.
func (a Auto) TakesDigestMoves() bool {
	return a != AutoNone
}

// rank orders the version ladder. Higher takes a larger version step.
//
// AutoDigest ranks 0, alongside AutoNone: it consents to no version step at
// all. That is not a statement that it is the weakest policy -- it is off the
// ladder entirely, and rank() is only ever asked about version changes.
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
