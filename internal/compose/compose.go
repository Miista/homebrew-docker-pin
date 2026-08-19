package compose

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Locate finds the compose file for the current working directory.
func Locate() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return FindFile(wd)
}

var composeNames = []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"}

// composeFileIn returns the compose file directly inside dir, if any,
// preferring names earlier in composeNames.
func composeFileIn(dir string) (string, bool) {
	for _, name := range composeNames {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// FindFile traverses up from dir looking for the nearest compose file, then
// keeps climbing through the contiguous run of ancestor directories that
// also have one, returning the topmost of that run. This finds a project's
// root compose file even when invoked from a nested directory that has its
// own (e.g. one pulled in via a parent's `include:`).
func FindFile(dir string) (string, error) {
	current := dir
	var nearest string
	for {
		if f, ok := composeFileIn(current); ok {
			nearest = f
			break
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no compose file found in %s or any parent directory", dir)
		}
		current = parent
	}

	topmost := nearest
	for {
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		f, ok := composeFileIn(parent)
		if !ok {
			break
		}
		topmost = f
		current = parent
	}
	return topmost, nil
}

type composeFile struct {
	Services map[string]struct {
		Image  string      `yaml:"image"`
		Labels labelsField `yaml:"labels"`
	} `yaml:"services"`
	Include []includeEntry `yaml:"include"`
}

// labelsField accepts both forms Compose allows for a service's `labels:`:
// a mapping (`labels: {KEY: value}`) or a list of "KEY=value" strings
// (`labels: ["KEY=value"]`).
type labelsField map[string]string

func (l *labelsField) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.MappingNode {
		var m map[string]string
		if err := value.Decode(&m); err != nil {
			return err
		}
		*l = m
		return nil
	}
	var list []string
	if err := value.Decode(&list); err != nil {
		return err
	}
	m := make(map[string]string, len(list))
	for _, entry := range list {
		k, v, _ := strings.Cut(entry, "=")
		m[k] = v
	}
	*l = m
	return nil
}

// includeEntry accepts both forms an `include:` list element can take: a
// bare path string, or a mapping with a `path` key (itself a string or a
// list of strings, for a base+override pair within one logical include).
type includeEntry struct {
	Paths []string
}

func (e *includeEntry) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		e.Paths = []string{value.Value}
		return nil
	}
	var m struct {
		Path yaml.Node `yaml:"path"`
	}
	if err := value.Decode(&m); err != nil {
		return err
	}
	if m.Path.Kind == yaml.ScalarNode {
		e.Paths = []string{m.Path.Value}
		return nil
	}
	return m.Path.Decode(&e.Paths)
}

// ResolveService locates the project's root compose file, then finds the
// file that directly declares serviceName's services: entry — recursing
// into the root's include: tree (root's own services: take precedence;
// among includes, earlier entries win, matching Compose's own merge order)
// only if the service isn't declared directly.
func ResolveService(serviceName string) (string, error) {
	root, err := Locate()
	if err != nil {
		return "", err
	}
	return ResolveServiceIn(root, serviceName)
}

// ResolveServiceIn is ResolveService against an already-known root file,
// for callers that resolve many services against one root they've already
// located (e.g. listing every service) rather than the working directory.
func ResolveServiceIn(root, serviceName string) (string, error) {
	found, err := resolveServiceIn(root, serviceName)
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("service %q not found in %s or any included file", serviceName, root)
	}
	return found, nil
}

func resolveServiceIn(file, serviceName string) (string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	var cf composeFile
	if err := yaml.Unmarshal(data, &cf); err != nil {
		return "", fmt.Errorf("parsing %s: %w", file, err)
	}
	if _, ok := cf.Services[serviceName]; ok {
		return file, nil
	}
	dir := filepath.Dir(file)
	for _, entry := range cf.Include {
		for _, p := range entry.Paths {
			included := p
			if !filepath.IsAbs(included) {
				included = filepath.Join(dir, included)
			}
			found, err := resolveServiceIn(included, serviceName)
			if err != nil {
				return "", err
			}
			if found != "" {
				return found, nil
			}
		}
	}
	return "", nil
}

// ListServices returns the names of every service reachable from file,
// recursing into its include: tree. A name defined in more than one place
// (root shadowing an include, or an earlier include shadowing a later one)
// is only reported once.
func ListServices(file string) ([]string, error) {
	seen := map[string]bool{}
	if err := collectServices(file, seen); err != nil {
		return nil, err
	}
	services := make([]string, 0, len(seen))
	for name := range seen {
		services = append(services, name)
	}
	return services, nil
}

func collectServices(file string, seen map[string]bool) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var cf composeFile
	if err := yaml.Unmarshal(data, &cf); err != nil {
		return fmt.Errorf("parsing %s: %w", file, err)
	}
	for name := range cf.Services {
		seen[name] = true
	}
	dir := filepath.Dir(file)
	for _, entry := range cf.Include {
		for _, p := range entry.Paths {
			included := p
			if !filepath.IsAbs(included) {
				included = filepath.Join(dir, included)
			}
			if err := collectServices(included, seen); err != nil {
				return err
			}
		}
	}
	return nil
}

// RawImage returns the image string exactly as written in the compose file.
func RawImage(file, serviceName string) (string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	var cf composeFile
	if err := yaml.Unmarshal(data, &cf); err != nil {
		return "", fmt.Errorf("parsing %s: %w", file, err)
	}
	svc, ok := cf.Services[serviceName]
	if !ok {
		return "", fmt.Errorf("service %q not found in %s", serviceName, file)
	}
	return svc.Image, nil
}

// Labels returns the labels of the given service (nil if it has none).
func Labels(file, serviceName string) (map[string]string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var cf composeFile
	if err := yaml.Unmarshal(data, &cf); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", file, err)
	}
	svc, ok := cf.Services[serviceName]
	if !ok {
		return nil, fmt.Errorf("service %q not found in %s", serviceName, file)
	}
	return svc.Labels, nil
}

// ParseImage returns the base image name and tag for the given service.
// Strips any existing digest from the current value.
func ParseImage(file, serviceName string) (base, tag string, err error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return "", "", err
	}
	var cf composeFile
	if err := yaml.Unmarshal(data, &cf); err != nil {
		return "", "", fmt.Errorf("parsing %s: %w", file, err)
	}
	svc, ok := cf.Services[serviceName]
	if !ok {
		return "", "", fmt.Errorf("service %q not found in %s", serviceName, file)
	}
	if svc.Image == "" {
		return "", "", fmt.Errorf("service %q has no image field", serviceName)
	}
	return splitImage(svc.Image)
}

// splitImage strips any digest, then splits base and tag.
func splitImage(image string) (base, tag string, err error) {
	if i := strings.Index(image, "@sha256:"); i != -1 {
		image = image[:i]
	}
	// Tag lives after the last colon, but only if it's in the final path segment.
	lastSlash := strings.LastIndex(image, "/")
	lastColon := strings.LastIndex(image, ":")
	if lastColon > lastSlash {
		return image[:lastColon], image[lastColon+1:], nil
	}
	return image, "latest", nil
}

// PinImage rewrites the image line for serviceName in file to pinnedImage.
// Preserves all surrounding formatting and comments.
func PinImage(file, serviceName, pinnedImage string) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}

	lines := strings.Split(string(data), "\n")
	idx, prefix, err := findImageLine(lines, serviceName)
	if err != nil {
		return fmt.Errorf("%w in %s", err, file)
	}
	// Keep any inline comment after the value (image refs cannot contain '#').
	comment := ""
	if m := regexp.MustCompile(`\s+#.*$`).FindString(lines[idx]); m != "" {
		comment = m
	}
	lines[idx] = prefix + pinnedImage + comment
	return os.WriteFile(file, []byte(strings.Join(lines, "\n")), 0o644)
}

// findImageLine locates the `image:` line of a service, anchored on the
// top-level `services:` section so that same-named keys elsewhere (e.g. a
// depends_on entry, a network name) can never be mistaken for the service.
// The service key must sit at the services section's direct-child indent, and
// the image key at the service's direct-child indent — so an `image:` label
// nested deeper (e.g. under labels:) is never rewritten. Returns the line
// index and the line's prefix up to the value.
func findImageLine(lines []string, serviceName string) (idx int, prefix string, err error) {
	servicesRe := regexp.MustCompile(`^services\s*:\s*(#.*)?$`)
	serviceRe := regexp.MustCompile(`^(\s*)(["']?)` + regexp.QuoteMeta(serviceName) + `(["']?)\s*:\s*(#.*)?$`)
	imageRe := regexp.MustCompile(`^(\s*image\s*:\s*)(.+)$`)

	const (
		beforeServices = iota
		inServices
		inService
	)
	state := beforeServices
	serviceKeyIndent := -1 // indent of the services section's direct children
	serviceIndent := -1    // indent of the matched service key
	childIndent := -1      // indent of the matched service's direct children

	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue // blanks and comments are neutral: neither keys nor boundaries
		}
		indent := len(line) - len(trimmed)

		switch state {
		case beforeServices:
			if servicesRe.MatchString(line) {
				state = inServices
			}
		case inServices:
			if indent == 0 {
				return 0, "", fmt.Errorf("service %q not found under services:", serviceName)
			}
			if serviceKeyIndent == -1 {
				serviceKeyIndent = indent // first key under services: sets the level
			}
			if indent != serviceKeyIndent {
				continue // inside some other service's block
			}
			if m := serviceRe.FindStringSubmatch(line); m != nil && m[2] == m[3] {
				state = inService
				serviceIndent = indent
			}
		case inService:
			if indent <= serviceIndent {
				return 0, "", fmt.Errorf("image field not found for service %q", serviceName)
			}
			if childIndent == -1 {
				childIndent = indent // first key inside the service sets the level
			}
			if indent != childIndent {
				continue // nested content (labels, depends_on children, ...)
			}
			if m := imageRe.FindStringSubmatch(line); m != nil {
				return i, m[1], nil
			}
		}
	}
	switch state {
	case beforeServices:
		return 0, "", fmt.Errorf("no top-level services: section found")
	case inServices:
		return 0, "", fmt.Errorf("service %q not found under services:", serviceName)
	default:
		return 0, "", fmt.Errorf("image field not found for service %q", serviceName)
	}
}
