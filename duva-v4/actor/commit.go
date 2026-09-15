package main

import (
	"fmt"
	"os"
	"strings"
	"text/template"
)

// The subject the actor commits under.
//
// A template rather than a fixed format, because a commit convention is the
// repository's, not the actor's. This repository requires
// `<box>/<service>: <subject>` and enforces it with a commit-msg hook; a repo
// that prefixes with a ticket, or writes conventional commits, should not have
// to accept anyone else's shape.
//
// There is deliberately no Host field and no ACTOR_HOST. The actor needs the
// host for nothing else -- it pulls, writes a pin, recreates and commits, none
// of which care which box they are on -- and the template is already mounted
// per host, so the host belongs *in* it:
//
//	optiplex/{{.Container}}: update to {{.NewVersion}}
//
// An env var would be a second place to say the same thing, and two places to
// get it wrong.
//
// ACTOR_COMMIT_TEMPLATE rather than a mounted file: it is one line, it is not
// a secret, and it does not change without a restart, so it belongs beside
// ACTOR_TOKEN and ACTOR_GIT_PUSH rather than being a mount to arrange.

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
// hard error: the actor refuses to commit rather than writing "{{.Servce}}: 1.0 ->
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

// loadCommitTemplate reads ACTOR_COMMIT_TEMPLATE, or returns the default.
//
// An environment variable rather than a mounted file, which is what v3 had.
// It is one line, it is not a secret, and it does not change without a
// restart -- so a file would be a mount to arrange and a second mechanism
// beside ACTOR_TOKEN and ACTOR_GIT_PUSH, for no benefit. It also reads where
// an operator looks for it: next to the rest of the service's configuration.
//
// Unset is the normal case rather than a problem: most stacks want the
// default and configure nothing. Set-but-empty is an error, because someone
// meant to say something and said nothing.
func loadCommitTemplate() (string, error) {
	raw, ok := os.LookupEnv("ACTOR_COMMIT_TEMPLATE")
	if !ok {
		return defaultCommitTemplate, nil
	}
	tmpl := strings.TrimSpace(raw)
	if tmpl == "" {
		return "", fmt.Errorf("ACTOR_COMMIT_TEMPLATE is set but empty")
	}
	return tmpl, nil
}
