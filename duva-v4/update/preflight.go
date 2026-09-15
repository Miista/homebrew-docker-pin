package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

// The updater's startup preconditions.
//
// These are checked before serving rather than watched afterwards, because
// none of them can change while the process runs: a mount is a mount, a
// socket is a socket, and a uid does not move. A healthcheck would be asking
// a question whose answer was settled at exec time.
//
// Refusing to start is right here, where it would be wrong for the watcher.
// A watcher whose webhook is down still detects and logs; an updater that
// cannot write, cannot reach the daemon, or has no git can do nothing at all.
// Starting anyway means discovering it halfway through an apply -- after a
// container has been replaced and before the pin recording it was written,
// which is the one state this design works to avoid.

// dockerSocket is where the daemon is reached. Fixed: it is the contract with
// whoever runs this, the same way /compose is.
const dockerSocket = "/var/run/docker.sock"

// preflight checks everything the updater needs before it agrees to serve.
//
// All checks run before any error is returned, so a misconfigured deployment
// is fixed in one pass rather than one restart per problem.
func preflight(dir string) error {
	var problems []string
	for _, check := range []func(string) error{
		notRoot,
		writable,
		daemonReachable,
		gitPresent,
		gitIdentity,
	} {
		if err := check(dir); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("this actor cannot do the work it exists for:\n\n  - %s\n\n%s",
		strings.Join(problems, "\n\n  - "),
		"See duva-v4/README.md: the actor runs as the user that owns the "+
			"repository, and that user is in the docker group.")
}

// notRoot refuses to run as uid 0.
//
// Not hardening for its own sake. The repository is bind-mounted from the
// host, so every file this touches as root becomes root's on the host too --
// and the operator who owns that repository then needs sudo to edit their own
// compose file. Git also refuses a repository it does not own, and the commit
// identity in .git/config is the right one precisely because the running user
// is the one who owns it.
//
// The argument takes a dir it does not use, so every check has one signature
// and preflight can hold them in a slice.
func notRoot(string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	return fmt.Errorf("this is running as root (uid 0), which would leave root-owned "+
		"files in the repository mounted at %s and lose the commit identity its "+
		".git/config carries\n    Set `user: \"<uid>:<gid>\"` to the owner of that "+
		"repository, and `group_add` to the docker group's GID on this host "+
		"(`stat -c '%%g' %s`)", composeDir, dockerSocket)
}

// daemonReachable dials the docker socket.
//
// Dialled rather than stat'ed: the socket file being present says nothing
// about whether this process may open it, and group membership is exactly the
// thing most likely to be missing.
func daemonReachable(string) error {
	info, err := os.Stat(dockerSocket)
	if err != nil {
		return fmt.Errorf("the docker socket is not at %s: %w\n    "+
			"Mount it: `- %s:%s`", dockerSocket, err, dockerSocket, dockerSocket)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s is not a socket", dockerSocket)
	}
	conn, err := net.DialTimeout("unix", dockerSocket, 5*time.Second)
	if err != nil {
		return fmt.Errorf("the docker socket at %s cannot be opened: %w\n    "+
			"It is usually root:docker mode 0660, so this user must be in the "+
			"docker group: `group_add: [\"<gid>\"]`, with the GID from "+
			"`stat -c '%%g' %s` on this host", dockerSocket, err, dockerSocket)
	}
	return conn.Close()
}

// gitPresent checks git is on PATH and will run.
//
// Both, because a binary that is present but not executable by this user
// fails identically to one that is missing, and the fix is different.
func gitPresent(string) error {
	path, err := exec.LookPath("git")
	if err != nil {
		return fmt.Errorf("git is not on PATH: %w\n    "+
			"Applying commits the rewritten pin, so there has to be a git to "+
			"shell out to", err)
	}
	if err := exec.Command(path, "--version").Run(); err != nil {
		return fmt.Errorf("git is at %s but will not run: %w", path, err)
	}
	return nil
}

// gitIdentity checks the repository will accept a commit from this user.
//
// The last step of an apply is the commit, and it is the one that runs after
// a container has already been replaced. A missing user.email fails it there,
// leaving the host running an image the compose file does not record -- the
// exact state this checks exist to prevent.
//
// Asked of the repository rather than of a config file, because the answer
// can come from .git/config, a global, or the environment, and only git knows
// which apply here.
func gitIdentity(dir string) error {
	out, err := exec.Command("git", "-C", dir, "-c", "safe.directory="+dir,
		"rev-parse", "--is-inside-work-tree").Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return fmt.Errorf("%s is not a git repository\n    "+
			"Applying commits the rewritten pin, so the compose files have to "+
			"be in one", dir)
	}
	for _, key := range []string{"user.email", "user.name"} {
		v, err := exec.Command("git", "-C", dir, "-c", "safe.directory="+dir,
			"config", "--get", key).Output()
		if err != nil || strings.TrimSpace(string(v)) == "" {
			return fmt.Errorf("the repository at %s has no %s, so the commit at "+
				"the end of an apply would fail -- after a container had already "+
				"been replaced\n    Set it in that repository: "+
				"`git config %s <value>`", dir, key, key)
		}
	}
	return nil
}
