package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
)

// The subject the updater commits under.
//
// A template rather than a fixed format, because a commit convention is the
// repository's, not the updater's. This repository requires
// `<box>/<service>: <subject>` and enforces it with a commit-msg hook; a repo
// that prefixes with a ticket, or writes conventional commits, should not have
// to accept anyone else's shape.
//
// There is deliberately no Host field and no DUVA_HOST. The updater needs the
// host for nothing else -- it pulls, writes a pin, recreates and commits, none
// of which care which box they are on -- and the template is already mounted
// per host, so the host belongs *in* it:
//
//	optiplex/{{.Container}}: update to {{.NewVersion}}
//
// An env var would be a second place to say the same thing, and two places to
// get it wrong.
//
// DUVA_COMMIT_TEMPLATE rather than a mounted file: it is one line, it is not
// a secret, and it does not change without a restart, so it belongs beside
// DUVA_UPDATE_TOKEN and DUVA_GIT_PUSH rather than being a mount to arrange.

// defaultCommitTemplate is used when nothing is mounted -- which is the
// common case, and should need no configuration.
//
// No host prefix: a default cannot know the box, and guessing one would write
// a wrong name into history. A repository whose hook demands one mounts a
// template that supplies it.
//
// It reads a version change as one and a digest move as the other, because a
// digest move leaves the tag alone: "bazarr: latest -> latest" would say
// nothing happened.
const defaultCommitTemplate = `{{.Container}}: ` +
	`{{if eq .OldVersion .NewVersion}}{{.NewVersion}} moved to {{.NewDigest}}` +
	`{{else}}{{.OldVersion}} -> {{.NewVersion}}{{end}}`

// commitFields is what a template may refer to.
//
// Container is the compose service name, not the Docker container's: it is
// what belongs in a commit subject, and "optiplex-paperless-db-1" carries a
// project prefix and a replica suffix that mean nothing there. It is called
// Container because that is what people call it.
type commitFields struct {
	Container  string // the compose service
	Image      string // the image, without tag or digest
	OldVersion string // the tag before
	NewVersion string // the tag after
	OldDigest  string // the digest before
	NewDigest  string // the digest after
}

// commitSubject renders the message a change is committed under.
//
// A template that does not parse, or names a field that does not exist, is a
// hard error: the updater refuses to commit rather than writing "{{.Servce}}: 1.0 ->
// 2.0" into the history of every host it runs on. Executing against a struct
// reports an unknown field on its own; missingkey is set for the same
// guarantee should the fields ever become a map.
func commitSubject(tmpl string, f commitFields) (string, error) {
	// Empty means the default rather than an empty subject: a caller that
	// does not care about the convention should get this repository's, not a
	// commit with no message.
	if strings.TrimSpace(tmpl) == "" {
		tmpl = defaultCommitTemplate
	}

	t, err := template.New("commit").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("the commit template does not parse: %w", err)
	}

	var b strings.Builder
	if err := t.Execute(&b, f); err != nil {
		return "", fmt.Errorf("the commit template refers to something that does not exist: %w", err)
	}

	// A subject is one line. A template with a newline in it would otherwise
	// produce a commit whose body arrived by accident.
	subject := strings.TrimSpace(b.String())
	if i := strings.IndexByte(subject, '\n'); i != -1 {
		subject = strings.TrimSpace(subject[:i])
	}
	if subject == "" {
		return "", fmt.Errorf("the commit template produced an empty subject")
	}
	return subject, nil
}

// loadCommitTemplate reads DUVA_COMMIT_TEMPLATE, or returns the default.
//
// An environment variable rather than a mounted file, which is what v3 had.
// It is one line, it is not a secret, and it does not change without a
// restart -- so a file would be a mount to arrange and a second mechanism
// beside DUVA_UPDATE_TOKEN and DUVA_GIT_PUSH, for no benefit. It also reads where
// an operator looks for it: next to the rest of the service's configuration.
//
// Unset is the normal case rather than a problem: most stacks want the
// default and configure nothing. Set-but-empty is an error, because someone
// meant to say something and said nothing.
func loadCommitTemplate() (string, error) {
	raw, ok := os.LookupEnv("DUVA_COMMIT_TEMPLATE")
	if !ok {
		return defaultCommitTemplate, nil
	}
	tmpl := strings.TrimSpace(raw)
	if tmpl == "" {
		return "", fmt.Errorf("DUVA_COMMIT_TEMPLATE is set but empty")
	}
	return tmpl, nil
}

// checkMessageAccepted renders a representative subject and asks the
// repository's own commit-msg hook whether it would take it.
//
// Asked at readiness, before any work, because the alternative is what
// happened to bazarr: pulled, repinned, recreated, and only then rejected --
// leaving the container running a new image the repository does not record.
// A rollback at that point would mean tearing down a healthy service over a
// commit message, which is worse than the divergence it fixes. Not starting
// is strictly better than unwinding.
//
// The message is representative rather than real: readiness is asked with no
// service in mind. That is enough to catch the failure that matters -- a
// template whose *shape* the hook refuses, which is every apply -- and not
// enough to catch one that refuses a particular service name, which is not a
// thing hooks do.
//
// No hook, or a hook that cannot be run, is ready. A repository without one
// accepts anything, and refusing to work because a hook is missing would be
// inventing a requirement.
func checkMessageAccepted(dir, tmpl string) error {
	subject, err := commitSubject(tmpl, commitFields{
		Container:  "example",
		Image:      "example.com/example",
		OldVersion: "1.0.0",
		NewVersion: "1.0.1",
		OldDigest:  "sha256:0000000000000000",
		NewDigest:  "sha256:1111111111111111",
	})
	if err != nil {
		return err
	}

	hookPath, err := exec.Command("git", "-C", dir, "-c", "safe.directory="+dir,
		"rev-parse", "--git-path", "hooks/commit-msg").Output()
	if err != nil {
		return nil // not a git repository to ask; the preflight has that
	}
	hook := strings.TrimSpace(string(hookPath))
	if !filepath.IsAbs(hook) {
		hook = filepath.Join(dir, hook)
	}
	info, err := os.Stat(hook)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return nil // no hook, or not executable: nothing would reject this
	}

	f, err := os.CreateTemp("", "duva-commit-msg-*")
	if err != nil {
		return nil // cannot ask; do not invent a refusal
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(subject + "\n"); err != nil {
		f.Close()
		return nil
	}
	f.Close()

	cmd := exec.Command(hook, f.Name())
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("the repository's commit-msg hook would reject %q: %s",
			subject, strings.TrimSpace(string(out)))
	}
	return nil
}
