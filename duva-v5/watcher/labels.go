package watcher

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/Miista/homebrew-docker-pin/compose"
)

// tagRules reads which tags a service considers candidates.
//
// It reads **diun's** labels, not duva's, and that is deliberate. Every
// service on both hosts already carries `diun.include_tags` /
// `diun.exclude_tags` -- around forty-five of them -- because diun has been
// the watcher here for a long time. Reading duva's namespace instead would
// mean a migration that changes nothing about what the constraints say, and
// would leave every service unconstrained until it was done.
//
// It is also the right namespace on the merits: these are detection rules,
// and this is the watcher. Policy -- what may be applied unattended -- is a
// different question, lives on `duva.auto`, and is the gate's to read.
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

	if raw := labels["diun.include_tags"]; raw != "" {
		include, err = regexp.Compile(raw)
		if err != nil {
			// A pattern that does not compile is an error, not something to
			// ignore. Ignoring it would widen what qualifies rather than
			// narrow it, which is the dangerous direction: the operator
			// wrote a constraint and would get none.
			return nil, nil, fmt.Errorf("diun.include_tags %q does not compile: %w", raw, err)
		}
	}
	if raw := labels["diun.exclude_tags"]; raw != "" {
		exclude, err = regexp.Compile(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("diun.exclude_tags %q does not compile: %w", raw, err)
		}
	}
	return include, exclude, nil
}
