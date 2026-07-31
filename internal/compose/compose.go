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
		Image string `yaml:"image"`
	} `yaml:"services"`
}

// ListServices returns all service names in the compose file.
func ListServices(file string) ([]string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var cf composeFile
	if err := yaml.Unmarshal(data, &cf); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", file, err)
	}
	services := make([]string, 0, len(cf.Services))
	for name := range cf.Services {
		services = append(services, name)
	}
	return services, nil
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
