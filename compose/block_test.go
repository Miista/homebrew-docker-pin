package compose

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeCompose(t *testing.T, content string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "docker-compose.yml")
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

func block(t *testing.T, content, service string) map[string]any {
	t.Helper()
	b, err := ServiceBlock(writeCompose(t, content), service)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const radarrBase = `
services:
  radarr:
    image: example.com/radarr:1.0@sha256:aaa
    environment:
      TZ: Europe/Copenhagen
    volumes:
      - ./radarr:/config
  other:
    image: example.com/other:1.0
`

func TestServiceBlock_LeavesTheImageOut(t *testing.T) {
	b := block(t, radarrBase, "radarr")
	if _, ok := b["image"]; ok {
		t.Errorf("the image is what an update changes, and must not be part of the comparison: %v", b)
	}
	if _, ok := b["environment"]; !ok {
		t.Errorf("the rest of the service should be there: %v", b)
	}
}

// What counts as an edit is a change of meaning.
func TestServiceBlock_IgnoresWhatDoesNotChangeMeaning(t *testing.T) {
	want := block(t, radarrBase, "radarr")
	for name, content := range map[string]string{
		"a different image": strings.Replace(radarrBase, "1.0@sha256:aaa", "2.0@sha256:bbb", 1),
		"a comment":         strings.Replace(radarrBase, "    volumes:", "    # for later\n    volumes:", 1),
		"blank lines":       strings.Replace(radarrBase, "    volumes:", "\n\n    volumes:", 1),
		"key order": `
services:
  radarr:
    volumes:
      - ./radarr:/config
    environment:
      TZ: Europe/Copenhagen
    image: example.com/radarr:1.0@sha256:aaa
  other:
    image: example.com/other:1.0
`,
		"quoting": strings.Replace(radarrBase, "TZ: Europe/Copenhagen", `TZ: "Europe/Copenhagen"`, 1),
		"another service": strings.Replace(radarrBase, "other:1.0", "other:2.0", 1) +
			"  third:\n    image: example.com/third\n",
	} {
		t.Run(name, func(t *testing.T) {
			if got := block(t, content, "radarr"); !reflect.DeepEqual(got, want) {
				t.Errorf("not an edit to radarr, but the blocks differ:\n  want %v\n  got  %v", want, got)
			}
		})
	}
}

func TestServiceBlock_SeesAnEditToTheService(t *testing.T) {
	want := block(t, radarrBase, "radarr")
	for name, content := range map[string]string{
		"an env value":   strings.Replace(radarrBase, "Europe/Copenhagen", "UTC", 1),
		"a new key":      strings.Replace(radarrBase, "    volumes:", "    restart: always\n    volumes:", 1),
		"a removed key":  strings.Replace(radarrBase, "    volumes:\n      - ./radarr:/config\n", "", 1),
		"a list element": strings.Replace(radarrBase, "- ./radarr:/config", "- ./radarr:/config\n      - ./more:/more", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if got := block(t, content, "radarr"); reflect.DeepEqual(got, want) {
				t.Errorf("an edit to radarr went unseen")
			}
		})
	}
}

// An anchor is shared, so editing it edits every service that borrows from it.
// Read as text this is an edit to some other block.
func TestServiceBlock_SeesAnEditToASharedAnchor(t *testing.T) {
	const withAnchor = `
x-common: &common
  environment:
    TZ: Europe/Copenhagen
services:
  radarr:
    <<: *common
    image: example.com/radarr:1.0
  other:
    <<: *common
    image: example.com/other:1.0
`
	before := block(t, withAnchor, "radarr")
	if env, _ := before["environment"].(map[string]any); env["TZ"] != "Europe/Copenhagen" {
		t.Fatalf("the merge key was not expanded, so an anchor edit would go unseen: %v", before)
	}
	after := block(t, strings.Replace(withAnchor, "Europe/Copenhagen", "UTC", 1), "radarr")
	if reflect.DeepEqual(before, after) {
		t.Error("an edit to an anchor radarr borrows from went unseen")
	}
	// And the other direction: an anchor radarr does not use is not its edit.
	const unrelated = `
x-other: &other
  environment:
    TZ: x
services:
  radarr:
    image: example.com/radarr:1.0
  b:
    <<: *other
    image: example.com/b:1.0
`
	if !reflect.DeepEqual(block(t, unrelated, "radarr"), block(t, strings.Replace(unrelated, "TZ: x", "TZ: y", 1), "radarr")) {
		t.Error("an anchor radarr does not use changed radarr's block")
	}
}

func TestServiceBlock_UnknownServiceAndBadYAML(t *testing.T) {
	if _, err := ServiceBlock(writeCompose(t, radarrBase), "nope"); err == nil {
		t.Error("a service that is not there should be an error")
	}
	if _, err := ServiceBlock(writeCompose(t, "services: [unclosed"), "radarr"); err == nil {
		t.Error("unparseable yaml should be an error")
	}
	if _, err := ServiceBlock(filepath.Join(t.TempDir(), "missing.yml"), "radarr"); err == nil {
		t.Error("a missing file should be an error")
	}
}

func TestServiceBlockOf_ReadsContentAsGiven(t *testing.T) {
	b, err := ServiceBlockOf([]byte(radarrBase), "HEAD:x.yml", "radarr")
	if err != nil || b["environment"] == nil {
		t.Errorf("got %v, %v", b, err)
	}
	if _, err := ServiceBlockOf([]byte(radarrBase), "HEAD:x.yml", "nope"); err == nil || !strings.Contains(err.Error(), "HEAD:x.yml") {
		t.Errorf("the error should name what was read: %v", err)
	}
}

func TestServiceFiles(t *testing.T) {
	for name, tc := range map[string]struct {
		yaml string
		want []string
	}{
		"none": {"services:\n  radarr:\n    image: x\n", nil},
		"a string": {"services:\n  radarr:\n    image: x\n    env_file: radarr.env\n",
			[]string{"radarr.env"}},
		"a list": {"services:\n  radarr:\n    image: x\n    env_file:\n      - a.env\n      - ./b.env\n",
			[]string{"a.env", "b.env"}},
		"the long form": {"services:\n  radarr:\n    image: x\n    env_file:\n      - path: a.env\n        required: false\n",
			[]string{"a.env"}},
		"extends a file": {"services:\n  radarr:\n    extends:\n      file: base.yml\n      service: radarr\n",
			[]string{"base.yml"}},
		"extends in the same file": {"services:\n  radarr:\n    extends:\n      service: base\n",
			nil},
		"both": {"services:\n  radarr:\n    env_file: a.env\n    extends:\n      file: base.yml\n      service: r\n",
			[]string{"a.env", "base.yml"}},
	} {
		t.Run(name, func(t *testing.T) {
			file := writeCompose(t, tc.yaml)
			got, err := ServiceFiles(file, "radarr")
			if err != nil {
				t.Fatal(err)
			}
			var want []string
			for _, w := range tc.want {
				want = append(want, filepath.Join(filepath.Dir(file), w))
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("want %v, got %v", want, got)
			}
		})
	}

	t.Run("an absolute path is kept", func(t *testing.T) {
		file := writeCompose(t, "services:\n  radarr:\n    image: x\n    env_file: /etc/radarr.env\n")
		got, _ := ServiceFiles(file, "radarr")
		if !reflect.DeepEqual(got, []string{"/etc/radarr.env"}) {
			t.Errorf("got %v", got)
		}
	})

	t.Run("an unknown service is an error", func(t *testing.T) {
		if _, err := ServiceFiles(writeCompose(t, radarrBase), "nope"); err == nil {
			t.Error("want an error")
		}
	})
}
