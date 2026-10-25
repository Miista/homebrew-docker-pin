package watcher

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/Miista/homebrew-docker-pin/compose"
)

// tagRules reads which tags a service considers candidates.
//
// They are duva's own labels, `duva.include_tags` and `duva.exclude_tags`. The
// same two rules were once read from diun's names, `diun.include_tags` and
// `diun.exclude_tags`, because diun was the watcher and the services already
// carried them; those are no longer read, and a service that still has only
// the old name is refused by name (see renamed) rather than watched without
// the constraint it was given.
//
// Policy -- what may be applied unattended -- is a different question, lives
// on `duva.auto`, and is the gate's to read.
func tagRules(composeFile, service string) (include, exclude *regexp.Regexp, err error) {
	labels, err := compose.Labels(composeFile, service)
	if err != nil {
		return nil, nil, fmt.Errorf("reading its labels: %w", err)
	}
	return rulesFrom(labels)
}

// errIgnored is a service labelled duva.ignore: true. Not a fault: duva was
// told not to follow it.
var errIgnored = errors.New("duva.ignore is true")

// rulesFrom compiles the tag rules from a set of labels, wherever they were
// read from.
func rulesFrom(labels map[string]string) (include, exclude *regexp.Regexp, err error) {
	// duva.ignore first: a service duva does not follow has no rules worth
	// compiling. Strict, as the queue reads it: a value that is neither true
	// nor false is an error, not a service quietly watched or not.
	switch labels["duva.ignore"] {
	case "", "false":
	case "true":
		return nil, nil, errIgnored
	default:
		return nil, nil, fmt.Errorf("duva.ignore: %q is not true or false", labels["duva.ignore"])
	}

	if include, err = tagRule(labels, "include_tags"); err != nil {
		return nil, nil, err
	}
	if exclude, err = tagRule(labels, "exclude_tags"); err != nil {
		return nil, nil, err
	}
	return include, exclude, nil
}

// tagRule reads one tag rule from duva's label.
//
// An empty value is no value. A pattern that does not compile is an error, not
// something to ignore: ignoring it would widen what qualifies rather than
// narrow it, which is the dangerous direction -- the operator wrote a
// constraint and would get none.
//
// The same holds for the old name. diun's label with no duva one beside it is
// a constraint somebody wrote that nothing would read, so it is an error that
// says what to rename, not a service quietly watched without it. A duva label
// beside a leftover diun one is fine: the duva one is the rule, and the other
// is only clutter.
func tagRule(labels map[string]string, name string) (*regexp.Regexp, error) {
	duvaKey, diunKey := "duva."+name, "diun."+name

	raw := labels[duvaKey]
	if raw == "" {
		if old := labels[diunKey]; old != "" {
			return nil, fmt.Errorf("%s is no longer read; rename it %s (it says %q)", diunKey, duvaKey, old)
		}
		return nil, nil
	}

	re, err := regexp.Compile(raw)
	if err != nil {
		return nil, fmt.Errorf("%s %q does not compile: %w", duvaKey, raw, err)
	}
	return re, nil
}
