package updater

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/Miista/homebrew-docker-pin/compose"
)

// Applying to a file somebody is editing.
//
// A compose file holds many services, so "this file has an uncommitted edit"
// is usually about some other one. What matters is whether the edit is in the
// way of *this* service: if everything it declares, bar the image, is the same
// in HEAD and in the working tree, the pin can be committed on its own and the
// edit left exactly where it was. An edit to the service itself is somebody
// mid-change to what is about to be recreated, and is waited out.
//
// The working tree already has the pin when this runs (writePin wrote it, the
// recreate read it). Only the commit needs care, and it is built from HEAD
// rather than from the working tree: HEAD's file with the one image line
// replaced, committed through a temporary index so the real one, and whatever a
// person has staged, is never touched. A commit-msg hook still runs, because
// this is `git commit` and not plumbing around it.

// serviceUntouched reports whether the uncommitted edits to file leave the
// service alone -- its image line and everything else it declares, and the
// files it reads (env_file, extends) -- so the pin can be committed apart from
// them. When it cannot say yes, why is a clause for the reason shown to the
// person.
func serviceUntouched(dir, file, service string) (ok bool, why string, err error) {
	rel, err := repoPath(dir, file)
	if err != nil {
		return false, "", err
	}
	if rel == "" {
		return false, "is not tracked yet", nil
	}

	// Staged changes to this file would be reversed in the index by the commit
	// below. Rare, and never worth guessing about.
	staged := gitCmd(dir, "diff", "--cached", "--quiet", "--", file)
	if err := staged.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return false, "has staged changes", nil
		}
		return false, "", fmt.Errorf("checking for staged changes: %w", err)
	}

	head, err := headContent(dir, rel)
	if err != nil {
		return false, "", err
	}
	headFile, cleanup, err := scratchCopy(head)
	if err != nil {
		return false, "", err
	}
	defer cleanup()

	headRaw, err := compose.RawImage(headFile, service)
	if err != nil {
		return false, "is not in the committed file", nil
	}
	workRaw, err := compose.RawImage(file, service)
	if err != nil {
		return false, "", fmt.Errorf("reading %s: %w", service, err)
	}
	if headRaw != workRaw {
		return false, "has an edit to its image line", nil
	}

	headBlock, err := compose.ServiceBlock(headFile, service)
	if err != nil {
		return false, "", err
	}
	workBlock, err := compose.ServiceBlock(file, service)
	if err != nil {
		return false, "", err
	}
	if !reflect.DeepEqual(headBlock, workBlock) {
		return false, "has an edit", nil
	}

	// What the service reads at deploy time. An edit there is an edit to the
	// service that the compose file does not show.
	files, err := compose.ServiceFiles(file, service)
	if err != nil {
		return false, "", err
	}
	for _, f := range files {
		out, err := gitCmd(dir, "status", "--porcelain", "--", f).Output()
		if err != nil {
			return false, "", fmt.Errorf("checking %s: %w", f, err)
		}
		if strings.TrimSpace(string(out)) != "" {
			return false, "reads " + filepath.Base(f) + ", which has uncommitted changes", nil
		}
	}
	return true, "", nil
}

// commitPinOnly commits the service's new image line as the only change to
// file, built from HEAD. The working tree is left as it is: the pin is already
// in it, and so is whatever else was being edited.
func commitPinOnly(dir, file, service, newRaw, message string) error {
	rel, err := repoPath(dir, file)
	if err != nil {
		return err
	}
	if rel == "" {
		return fmt.Errorf("%s is not tracked", file)
	}
	head, err := headContent(dir, rel)
	if err != nil {
		return err
	}
	headFile, cleanup, err := scratchCopy(head)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := compose.PinImage(headFile, service, newRaw); err != nil {
		return fmt.Errorf("pinning %s in the committed file: %w", service, err)
	}
	pinned, err := os.ReadFile(headFile)
	if err != nil {
		return err
	}

	blob := gitCmd(dir, "hash-object", "-w", "--stdin")
	blob.Stdin = strings.NewReader(string(pinned))
	sha, err := blob.Output()
	if err != nil {
		return fmt.Errorf("hash-object: %w", err)
	}

	// rel is from the repository root, and git is run from dir, which is
	// usually a subdirectory: --full-tree, and :(top) below, say so.
	tree, err := gitCmd(dir, "ls-tree", "--full-tree", "HEAD", "--", rel).Output()
	if err != nil {
		return fmt.Errorf("ls-tree: %w", err)
	}
	mode := strings.Fields(string(tree))
	if len(mode) < 1 {
		return fmt.Errorf("%s is not in HEAD", rel)
	}

	idxDir, err := os.MkdirTemp("", "duva-index-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(idxDir)
	index := "GIT_INDEX_FILE=" + filepath.Join(idxDir, "index")
	withIndex := func(args ...string) error {
		cmd := gitCmd(dir, args...)
		cmd.Env = append(os.Environ(), index)
		return runGit(cmd)
	}
	if err := withIndex("read-tree", "HEAD"); err != nil {
		return err
	}
	if err := withIndex("update-index", "--cacheinfo",
		fmt.Sprintf("%s,%s,%s", mode[0], strings.TrimSpace(string(sha)), rel)); err != nil {
		return err
	}
	if err := withIndex("commit", "-m", message); err != nil {
		return err
	}

	// The real index still holds the old line for this path, which would now
	// read as a staged change reversing the pin. Nothing of a person's was
	// staged here (serviceUntouched refused that), so this only catches the
	// index up with HEAD.
	return runGit(gitCmd(dir, "reset", "-q", "--", ":(top)"+rel))
}

// repoPath is file's path from the repository root, or "" when git does not
// track it.
func repoPath(dir, file string) (string, error) {
	out, err := gitCmd(dir, "ls-files", "--full-name", "--", file).Output()
	if err != nil {
		return "", fmt.Errorf("ls-files: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func headContent(dir, rel string) ([]byte, error) {
	out, err := gitCmd(dir, "show", "HEAD:"+rel).Output()
	if err != nil {
		return nil, fmt.Errorf("reading %s as committed: %w", rel, err)
	}
	return out, nil
}

// scratchCopy writes content where the compose helpers, which take a path, can
// read and rewrite it.
func scratchCopy(content []byte) (path string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "duva-head-*")
	if err != nil {
		return "", nil, err
	}
	path = filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		os.RemoveAll(dir)
		return "", nil, err
	}
	return path, func() { os.RemoveAll(dir) }, nil
}
