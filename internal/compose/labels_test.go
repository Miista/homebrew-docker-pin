package compose

import (
	"path/filepath"
	"testing"
)

// Compose allows labels in two shapes, and both appear in the wild. duva reads
// all of its per-service rules through here, so a shape it cannot parse means
// silently unconfigured services rather than an error.

func labelFixture(t *testing.T, body string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "docker-compose.yml")
	writeFile(t, f, body)
	return f
}

func TestLabels_MappingForm(t *testing.T) {
	f := labelFixture(t, `services:
  app:
    image: x/y:1.0.0
    labels:
      duva.include: '^\d+$'
      com.example.other: value
`)
	got, err := Labels(f, "app")
	if err != nil {
		t.Fatal(err)
	}
	if got["duva.include"] != `^\d+$` {
		t.Errorf("duva.include = %q", got["duva.include"])
	}
	if got["com.example.other"] != "value" {
		t.Errorf("other label = %q", got["com.example.other"])
	}
}

func TestLabels_ListForm(t *testing.T) {
	f := labelFixture(t, `services:
  app:
    image: x/y:1.0.0
    labels:
      - duva.include=^\d+$
      - com.example.other=value
`)
	got, err := Labels(f, "app")
	if err != nil {
		t.Fatal(err)
	}
	if got["duva.include"] != `^\d+$` {
		t.Errorf("duva.include = %q", got["duva.include"])
	}
	if got["com.example.other"] != "value" {
		t.Errorf("other label = %q", got["com.example.other"])
	}
}

// A value containing = must keep everything after the first one: regexes and
// URLs both contain them.
func TestLabels_ListFormValueWithEquals(t *testing.T) {
	f := labelFixture(t, `services:
  app:
    image: x/y:1.0.0
    labels:
      - custom.url=https://example.com/?a=1&b=2
`)
	got, err := Labels(f, "app")
	if err != nil {
		t.Fatal(err)
	}
	if got["custom.url"] != "https://example.com/?a=1&b=2" {
		t.Errorf("value truncated at the second '=': %q", got["custom.url"])
	}
}

// A bare entry with no '=' is a label with an empty value, per compose.
func TestLabels_ListFormBareKey(t *testing.T) {
	f := labelFixture(t, `services:
  app:
    image: x/y:1.0.0
    labels:
      - standalone
`)
	got, err := Labels(f, "app")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["standalone"]; !ok {
		t.Errorf("bare key missing from %v", got)
	}
}

func TestLabels_NoLabels(t *testing.T) {
	f := labelFixture(t, "services:\n  app:\n    image: x/y:1.0.0\n")
	got, err := Labels(f, "app")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected no labels, got %v", got)
	}
}

func TestLabels_UnknownService(t *testing.T) {
	f := labelFixture(t, "services:\n  app:\n    image: x/y:1.0.0\n")
	if _, err := Labels(f, "nope"); err == nil {
		t.Error("an unknown service must be an error")
	}
}

func TestLabels_MissingFile(t *testing.T) {
	if _, err := Labels(filepath.Join(t.TempDir(), "nope.yml"), "app"); err == nil {
		t.Error("a missing file must be an error")
	}
}

func TestLabels_MalformedYAML(t *testing.T) {
	f := labelFixture(t, "services:\n  app:\n   image: [unclosed\n")
	if _, err := Labels(f, "app"); err == nil {
		t.Error("malformed YAML must be an error")
	}
}

// Numeric and boolean label values are common (ports, feature flags) and must
// survive as strings rather than failing the whole parse.
func TestLabels_NonStringValues(t *testing.T) {
	f := labelFixture(t, `services:
  app:
    image: x/y:1.0.0
    labels:
      some.port: 8080
      some.flag: true
`)
	got, err := Labels(f, "app")
	if err != nil {
		t.Fatalf("non-string label values must not fail the parse: %v", err)
	}
	if got["some.port"] != "8080" || got["some.flag"] != "true" {
		t.Errorf("got %v", got)
	}
}
