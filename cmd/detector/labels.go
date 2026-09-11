package main

import (
	"fmt"
	"regexp"

	"github.com/Miista/homebrew-docker-pin/internal/compose"
)

// tagRules reads which tags a service considers candidates.
//
// It reads **diun's** labels, not duva's, and that is deliberate. Every
// service on both hosts already carries `diun.include_tags` /
// `diun.exclude_tags` -- around forty-five of them -- because diun has been
// the detector here for a long time. Reading duva's namespace instead would
// mean a migration that changes nothing about what the constraints say, and
// would leave every service unconstrained until it was done.
//
// It is also the right namespace on the merits: these are detection rules,
// and this is the detector. Policy -- what may be applied unattended -- is a
// different question, lives on `duva.auto`, and is the gate's to read.
func tagRules(composeFile, service string) (include, exclude *regexp.Regexp, err error) {
	labels, err := compose.Labels(composeFile, service)
	if err != nil {
		return nil, nil, fmt.Errorf("reading its labels: %w", err)
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
