// actor applies what a decider decided.
//
// It is the reference implementation of the contract in internal/actor: pull
// the image, write the digest into the compose file, recreate the container,
// commit, push. Another actor answering the same two calls -- one that opens
// a pull request, one that hands the work to Ansible -- would be a valid
// replacement, and the decider could not tell the difference.
//
// Everything it holds, it holds because applying needs it:
//
//	/compose                  the repository, read-write: the pin is rewritten here
//	/var/run/docker.sock      to pull and to replace a container
//
// Configuration:
//
//	ACTOR_TOKEN     the bearer token the decider must present
//	ACTOR_GIT_PUSH  publish commits (default off)
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
	// The timezone database, embedded in the binary. Go reads TZ on its
	// own, but resolves a name like Europe/Copenhagen against the host's
	// /usr/share/zoneinfo -- which a scratch image does not carry, so TZ
	// would be silently ignored and every timestamp would stay UTC:
	// every line it streams is timestamped by the log, and an operator watching an apply wants their own clock.
	_ "time/tzdata"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/actor"
)

var version = "dev"

// addr is where the actor listens. Fixed rather than configurable: it is
// reached by container name on the host's own network, and nothing outside
// addresses it.
const addr = ":8080"

// composeDir is the contract with whoever runs this.
var composeDir = "/compose"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Println("actor", version)
			return
		default:
			fmt.Fprintln(os.Stderr, "Usage: actor [version]")
			os.Exit(1)
		}
	}
	log := newLogger(os.Getenv("ACTOR_LOG_LEVEL"))
	if err := run(log); err != nil {
		log.Error().Msgf("%v", err)
		os.Exit(1)
	}
}

func run(log zerolog.Logger) error {
	// Before anything else: an actor that cannot write where the pins live
	// would take work, do most of it, and fail at the step that matters. The
	// same reasoning as duva's preflight -- discovering it later means
	// discovering it after a container was already replaced.
	if err := preflight(composeDir); err != nil {
		return err
	}

	push := boolEnv("ACTOR_GIT_PUSH", false)
	token := os.Getenv("ACTOR_TOKEN")

	// Loaded once, at startup, and a bad one refuses to start. A template
	// that does not parse would otherwise be discovered by an apply -- after
	// a container had already been replaced -- and the failure would be a
	// commit nobody made rather than a container nobody expected.
	tmpl, err := loadCommitTemplate()
	if err != nil {
		return fmt.Errorf("the commit template: %w", err)
	}
	commitTemplate = tmpl
	if _, err := commitSubject(tmpl, commitFields{Container: "probe", NewVersion: "1"}); err != nil {
		return fmt.Errorf("the commit template: %w", err)
	}

	srv := &http.Server{
		Addr: addr,
		Handler: (&Server{
			Token:   token,
			Version: version,
			Log:     log,
			Ready:   readiness,
			Apply: func(req actor.Request, step func(string, ...any)) (actor.Status, string) {
				return transaction(req, step, realDocker, realGit, push, tmpl)
			},
		}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info().Msgf("ready to apply, serving on %s", addr)
		if token == "" {
			// Worth a line rather than silence: an open actor is a root
			// shell on this host for anything that can reach the port.
			log.Warn().Msg("no ACTOR_TOKEN: this actor will accept work from any caller that can reach it")
		}
		if !push {
			log.Info().Msg("ACTOR_GIT_PUSH is off: commits stay local")
		}
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error().Msgf("stopped serving: %v", err)
		}
	}()

	<-ctx.Done()
	log.Info().Msg("shutting down")
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

// writable reports whether this process can write in dir.
//
// By writing rather than by reading permission bits: the question is "can
// this process write here", and ownership, mode, ACLs and a read-only mount
// are four different ways for the answer to be no.
func writable(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("the repository is mounted at %s, which cannot be read: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	probe, err := os.CreateTemp(dir, ".actor-preflight-*")
	if err != nil {
		return fmt.Errorf(
			"the repository at %s is not writable: %w\n"+
				"  Applying rewrites the pin in the compose file, so a read-only mount "+
				"or a directory owned by another user leaves this able to pull and "+
				"recreate but not to record what it did.", dir, err)
	}
	name := probe.Name()
	probe.Close()
	return os.Remove(name)
}

func boolEnv(key string, fallback bool) bool {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		// A typo silently meaning "no" is the kind of thing discovered
		// months later, when something has not been happening.
		fmt.Fprintf(os.Stderr, "Error: %s=%q is not a boolean\n", key, raw)
		os.Exit(1)
	}
	return v
}

// readiness reports whether this actor would take work right now.
//
// It asks the same realGit.IsClean the transaction asks, so the answer a UI
// greys a button on and the answer that refuses the work cannot disagree.
// Anything else would be a second implementation of the precondition, which
// is the way a button ends up enabled for work that is then refused.
//
// A busy repository reads as ready: someone is mid-commit, it will be quiet
// in a moment, and greying every button for that would be noise. The
// transaction retries it by design.
func readiness() actor.Readiness {
	clean, err := realGit.IsClean(composeDir)
	switch {
	case errors.Is(err, errRepoBusy):
		return actor.Readiness{Ready: true}
	case err != nil:
		return actor.Readiness{
			Ready:  false,
			Reason: fmt.Sprintf("the repository at %s cannot be read: %v", composeDir, err),
		}
	case !clean:
		return actor.Readiness{
			Ready: false,
			Reason: "the repository has uncommitted changes, and applying commits — " +
				"commit or stash them and this clears on its own",
		}
	}

	// Would the commit this is going to write be accepted? Asked here rather
	// than discovered after a container has been replaced: leaving a running
	// container the repository does not record is the divergence this tool
	// exists to prevent, and a rollback at that point would mean tearing down
	// a healthy service over a commit message.
	if err := checkMessageAccepted(composeDir, commitTemplate); err != nil {
		return actor.Readiness{Ready: false, Reason: err.Error()}
	}
	return actor.Readiness{Ready: true}
}

// commitTemplate is what readiness checks against and what an apply commits
// under. Set once at startup so the two cannot differ: a readiness check that
// validated a different template from the one used would be worse than none.
var commitTemplate string
