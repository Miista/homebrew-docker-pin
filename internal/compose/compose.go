package compose

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// FindFile traverses up from dir looking for a compose file.
func FindFile(dir string) (string, error) {
	names := []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"}
	current := dir
	for {
		for _, name := range names {
			p := filepath.Join(current, name)
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no compose file found in %s or any parent directory", dir)
		}
		current = parent
	}
}

type composeFile struct {
	Services map[string]struct {
		Image string `yaml:"image"`
	} `yaml:"services"`
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
	serviceRe := regexp.MustCompile(`^(\s*)` + regexp.QuoteMeta(serviceName) + `\s*:\s*$`)
	imageRe := regexp.MustCompile(`^(\s*image:\s*)(.+)$`)

	inService := false
	serviceIndent := -1
	updated := false

	for i, line := range lines {
		if !inService {
			if m := serviceRe.FindStringSubmatch(line); m != nil {
				inService = true
				serviceIndent = len(m[1])
			}
			continue
		}

		trimmed := strings.TrimLeft(line, " \t")
		if trimmed != "" {
			indent := len(line) - len(trimmed)
			if indent <= serviceIndent {
				break
			}
		}

		if m := imageRe.FindStringSubmatch(line); m != nil {
			lines[i] = m[1] + pinnedImage
			updated = true
			break
		}
	}

	if !updated {
		return fmt.Errorf("image field not found for service %q in %s", serviceName, file)
	}
	return os.WriteFile(file, []byte(strings.Join(lines, "\n")), 0o644)
}
