package watch

import (
	"fmt"
	"github.com/Miista/homebrew-docker-pin/internal/version"
	"strings"
)

// Auto is how large a change duva may apply without being asked, read from a
// service's duva.auto label.
type Auto string

const (
	// AutoNone: never act; every update waits for a human. The default.
	AutoNone Auto = "none"
	// AutoPatch: apply patches unattended.
	AutoPatch Auto = "patch"
	// AutoMinor: apply patches and minors.
	AutoMinor Auto = "minor"
	// AutoMajor: apply anything, including majors and changes that could not
	// be classified.
	AutoMajor Auto = "major"
)

// ParseAuto reads a duva.auto label value. An empty value means the label is
// absent, which is AutoNone.
//
// The default is deliberately "act on nothing". duva has spent its life as a
// notify-only tool; a build that can write must not start writing on hosts
// where nobody has said it may. Opting in is one label per service.
//
// An unrecognised value is an error rather than a fallback: a typo'd
// `duva.auto: pathc` silently meaning "none" is the kind of thing discovered
// months later, when an update that should have applied itself never did.
func ParseAuto(value string) (Auto, error) {
	switch a := Auto(strings.TrimSpace(strings.ToLower(value))); a {
	case "":
		return AutoNone, nil
	case AutoNone, AutoPatch, AutoMinor, AutoMajor:
		return a, nil
	default:
		return AutoNone, fmt.Errorf("duva.auto: unknown value %q (want patch, minor, major or none)", value)
	}
}

// rank orders the ladder so a threshold can be compared against a bump.
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

// bumpRank is the size of an actual change on the same ladder. Unknown ranks
// with major: if duva cannot tell how big a change is, it is not a change it
// should make on its own.
func bumpRank(k version.Kind) int {
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

// Decision is what should happen to an available update.
type Decision string

const (
	// DecideAuto: duva may apply this itself.
	DecideAuto Decision = "auto"
	// DecideApprove: it waits for a human.
	DecideApprove Decision = "approve"
)

// Decide works out whether an available finding may be applied unattended,
// and says why in one clause suitable for a notification or a table cell.
//
// A moving-tag service has no version pair, so there is no bump to threshold
// on. Choosing `latest` is itself the decision to ride the edge — adding an
// approval step on top would be pretending to a control already declined — so
// a digest move applies unless the service opted out with duva.auto: none.
func Decide(f Finding, auto Auto) (Decision, string) {
	if f.Kind == KindDigest {
		if auto == AutoNone {
			return DecideApprove, "digest moved; duva.auto is none"
		}
		return DecideAuto, "digest moved on a followed tag"
	}

	if auto == AutoNone {
		return DecideApprove, string(f.Bump) + "; duva.auto is none"
	}
	if bumpRank(f.Bump) <= auto.rank() {
		return DecideAuto, fmt.Sprintf("%s, within duva.auto: %s", f.Bump, auto)
	}
	if f.Bump == version.KindUnknown {
		return DecideApprove, fmt.Sprintf("version change could not be classified; needs duva.auto: %s", AutoMajor)
	}
	return DecideApprove, fmt.Sprintf("%s exceeds duva.auto: %s", f.Bump, auto)
}
