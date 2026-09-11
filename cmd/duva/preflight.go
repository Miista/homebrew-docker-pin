package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// checkStateWritable fails if duva cannot write where it keeps its state.
//
// duva discovers this today at the worst possible moment: after a check has
// run, against every service, over the network -- and then loses the result
// with "could not save what was found, so it may be reported again". The
// work is done and thrown away, once per run, forever, and the line that
// says so is one warning among the findings it is about to forget.
//
// It is not a hypothetical. A bind mount whose host path does not exist yet
// is created by the daemon owned by root, while duva runs as the host user
// so that git will touch the repository at all -- so a first deployment
// lands in exactly this state, and looks like it is working: the checks
// succeed, the queue renders, and only the next restart reveals that
// nothing was ever remembered.
//
// Checked by writing rather than by inspecting permission bits: the
// question is "can this process write here", and ownership, mode, ACLs and
// a read-only mount are four different ways for the answer to be no.
func checkStateWritable(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("duva keeps its state in %s, which it cannot create: %w", dir, err)
	}

	probe, err := os.CreateTemp(dir, ".duva-preflight-*")
	if err != nil {
		return fmt.Errorf(
			"duva keeps its state in %s and cannot write there: %w\n"+
				"  A bind mount to a path that did not exist is created owned by root, "+
				"while duva runs as the user that owns the repository.\n"+
				"  chown it to the user in the container's `user:` and restart.", dir, err)
	}
	name := probe.Name()
	probe.Close()
	return os.Remove(name)
}

// checkComposeReadable fails if the compose project is not where duva was
// told it is.
//
// Same reasoning: a mistyped DUVA_COMPOSE_SUBDIR, or a repository mounted
// one directory too deep, currently surfaces as every service being
// unwatched -- which reads as "nothing to do" rather than "duva is looking
// in the wrong place".
func checkComposeReadable(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("duva looks for the compose project in %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("duva looks for the compose project in %s, which is not a directory", dir)
	}
	return nil
}

// preflight checks everything duva needs before it does anything, so a
// misconfiguration stops it here rather than being discovered halfway
// through the first run -- or, worse, not discovered at all.
//
// Agents and local duvas only: a hub keeps no state and reads no compose
// file, and requiring either of it would defeat the point of it holding
// neither.
func preflight(cfg envConfig) error {
	if cfg.Mode == ModeHub {
		return nil
	}
	dir, err := projectDir()
	if err != nil {
		return err
	}
	if err := checkComposeReadable(dir); err != nil {
		return err
	}
	return checkStateWritable(stateFile)
}
