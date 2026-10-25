package watcher

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func projectWith(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

// duva's own names are the ones read.
func TestReadsDuvasLabels(t *testing.T) {
	file := projectWith(t, `
services:
  app:
    image: example.com/app:1.0.0@sha256:abc
    container_name: my-app
    labels:
      duva.include_tags: '^\d+\.\d+\.\d+$'
      duva.exclude_tags: '(alpha|beta|rc)'
`)
	include, exclude, err := tagRules(file, "app")
	if err != nil {
		t.Fatalf("tagRules: %v", err)
	}
	if include == nil || !include.MatchString("1.2.3") || include.MatchString("nightly") {
		t.Errorf("include did not compile to the right pattern: %v", include)
	}
	if exclude == nil || !exclude.MatchString("1.2.3-rc1") {
		t.Errorf("exclude did not compile to the right pattern: %v", exclude)
	}
}

// The refusing case, and the one that matters: a service with only diun's
// name. Reading nothing from it would watch the service without the
// constraint somebody wrote -- the dangerous direction -- so it is an error
// that says what to rename.
func TestDiunsNamesAreRefusedNotIgnored(t *testing.T) {
	for _, name := range []string{"include_tags", "exclude_tags"} {
		t.Run(name, func(t *testing.T) {
			file := projectWith(t, `
services:
  app:
    image: example.com/app:1.0.0@sha256:abc
    container_name: my-app
    labels:
      diun.`+name+`: '^1\.'
`)
			include, exclude, err := tagRules(file, "app")
			if err == nil {
				t.Fatal("want an error for a rule under the old name, got none")
			}
			for _, want := range []string{"diun." + name, "duva." + name} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error should name %s, got %v", want, err)
				}
			}
			if include != nil || exclude != nil {
				t.Errorf("a refused service came back with rules: %v/%v", include, exclude)
			}
		})
	}
}

// A rename half done on one rule: include moved, exclude not. The one that has
// not moved is the one that is refused, so it cannot be forgotten.
func TestAHalfMovedPairIsRefusedForTheRuleThatHasNotMoved(t *testing.T) {
	_, _, err := rulesFrom(map[string]string{
		"duva.include_tags": `^1\.`,
		"diun.exclude_tags": `-rc`,
	})
	if err == nil || !strings.Contains(err.Error(), "diun.exclude_tags") {
		t.Errorf("want an error naming diun.exclude_tags, got %v", err)
	}
}

// Both names: the duva one is the rule and the diun one is clutter, whatever it
// says. Nothing to refuse -- the constraint was written and is being read.
func TestADuvaLabelWinsOverALeftoverDiunOne(t *testing.T) {
	include, _, err := rulesFrom(map[string]string{
		"duva.include_tags": `^1\.`,
		"diun.include_tags": `^2\.`,
	})
	if err != nil {
		t.Fatalf("a duva label beside a leftover diun one was refused: %v", err)
	}
	if include == nil || !include.MatchString("1.2.3") || include.MatchString("2.0.0") {
		t.Errorf("the duva label did not win: %v", include)
	}
}

// An empty value is no value. Two empty labels constrain nothing; an empty
// duva label beside a real diun one is still the old name on its own.
func TestAnEmptyLabelIsNoLabel(t *testing.T) {
	include, exclude, err := rulesFrom(map[string]string{"duva.include_tags": "", "diun.include_tags": ""})
	if err != nil || include != nil || exclude != nil {
		t.Errorf("two empty labels gave %v/%v, %v; want no constraint", include, exclude, err)
	}
	if _, _, err := rulesFrom(map[string]string{"duva.include_tags": "", "diun.include_tags": `^1\.`}); err == nil {
		t.Error("an empty duva label hid a real diun one instead of refusing it")
	}
}

// No labels is no constraint, not an error: a service that never needed one
// should still be watched.
func TestNoLabelsMeansNoConstraint(t *testing.T) {
	file := projectWith(t, `
services:
  app:
    image: example.com/app:1.0.0@sha256:abc
    container_name: my-app
`)
	include, exclude, err := tagRules(file, "app")
	if err != nil {
		t.Fatalf("tagRules: %v", err)
	}
	if include != nil || exclude != nil {
		t.Errorf("got %v/%v, want no constraint", include, exclude)
	}
}

// diun's enable labels were never rules, and still are not: they say nothing
// to duva, which only understands duva.ignore.
func TestDiunsEnableLabelsMeanNothing(t *testing.T) {
	include, exclude, err := rulesFrom(map[string]string{"diun.enable": "false", "diun.enabled": "false"})
	if err != nil || include != nil || exclude != nil {
		t.Errorf("got %v/%v, %v; want no constraint and no error", include, exclude, err)
	}
}

// A pattern that does not compile is an error. Ignoring it would widen what
// qualifies rather than narrow it -- the operator wrote a constraint and
// would get none.
func TestAnUncompilablePatternIsAnError(t *testing.T) {
	file := projectWith(t, `
services:
  app:
    image: example.com/app:1.0.0@sha256:abc
    container_name: my-app
    labels:
      duva.include_tags: '^[unclosed'
`)
	_, _, err := tagRules(file, "app")
	if err == nil {
		t.Fatal("want an error for a pattern that does not compile")
	}
	if !strings.Contains(err.Error(), "duva.include_tags") {
		t.Errorf("the error should name the label, got %v", err)
	}
}

func TestAnUncompilableExcludeIsAlsoAnError(t *testing.T) {
	file := projectWith(t, `
services:
  app:
    image: example.com/app:1.0.0@sha256:abc
    container_name: my-app
    labels:
      duva.exclude_tags: '(unclosed'
`)
	_, _, err := tagRules(file, "app")
	if err == nil || !strings.Contains(err.Error(), "duva.exclude_tags") {
		t.Fatalf("want an error naming duva.exclude_tags, got %v", err)
	}
}

// duva.ignore still comes first: a service duva does not follow has no rules
// worth compiling, or complaining about -- not even one under the old name.
func TestIgnoreBeatsEvenARuleThatWouldBeRefused(t *testing.T) {
	for name, labels := range map[string]map[string]string{
		"uncompilable": {"duva.ignore": "true", "duva.include_tags": `^[unclosed`},
		"old name":     {"duva.ignore": "true", "diun.include_tags": `^1\.`},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := rulesFrom(labels); !errors.Is(err, errIgnored) {
				t.Errorf("got %v, want errIgnored", err)
			}
		})
	}
}

// In duva v5 the rules come from the container, which is what runs.
func TestContainerRulesAreTheContainers(t *testing.T) {
	labels := func(container string) (map[string]string, bool, error) {
		if container != "my-app" {
			return nil, false, nil
		}
		return map[string]string{"duva.include_tags": `^1\.`}, true, nil
	}
	include, exclude, err := containerRules(labels, "my-app")
	if err != nil {
		t.Fatal(err)
	}
	if include == nil || !include.MatchString("1.2.3") || exclude != nil {
		t.Errorf("include/exclude = %v/%v", include, exclude)
	}
	if _, _, err := containerRules(labels, "gone"); err == nil {
		t.Error("a service with no container was watched")
	}
}

// A container still running with the old name, which is what every one is
// until it is recreated after the rename: refused by name, so the log says
// which to recreate.
func TestAContainerStillOnTheOldNameIsRefused(t *testing.T) {
	labels := func(string) (map[string]string, bool, error) {
		return map[string]string{"diun.include_tags": `^1\.`}, true, nil
	}
	_, _, err := containerRules(labels, "my-app")
	if err == nil || !strings.Contains(err.Error(), "duva.include_tags") {
		t.Errorf("want an error saying to rename it duva.include_tags, got %v", err)
	}
}
