//go:build integration

package integration

import (
	"strings"
	"testing"
)

// The subject duva commits under is the repository's to decide. A template
// mounted at /etc/duva/commit-template replaces duva's default.
//
// The unit tests cover the rendering; what they cannot show is that the file
// is found where duva looks for it, and that what comes out is what git
// actually records.
func TestCommitTemplateIsUsed(t *testing.T) {
	s := Up(t, "duva-commit/custom-template")

	s.PushUpdates()
	s.Pin("app")
	s.Start()

	if got := s.Image("app"); !strings.Contains(got, ":1.0.1@sha256:") {
		t.Fatalf("the update was not applied, so there is no commit to check:\n  got %s", got)
	}

	// The fixture's template is:
	//   chore(deps): bump {{.Container}} from {{.OldVersion}} to {{.NewVersion}}
	want := "chore(deps): bump app from 1.0.0 to 1.0.1"
	if got := s.Log(); !strings.Contains(got, want) {
		t.Errorf("git recorded a different subject:\n  got  %s\n  want %s", got, want)
	}
	// And not duva's default, which would name the host and use an arrow.
	if got := s.Log(); strings.Contains(got, " -> ") {
		t.Errorf("the mounted template was ignored, duva used its default:\n  %s", got)
	}
}
