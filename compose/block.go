package compose

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ServiceBlock is everything a service declares except its image, decoded.
//
// Decoded rather than read as text, so that what counts as a change is a
// change of meaning: a comment, a blank line, key order or a different way of
// quoting the same value is not one. yaml.v3 expands aliases and `<<:` merges
// while decoding, so an edit to an anchor a service borrows from shows up in
// every service that uses it.
//
// The image is left out because it is the one thing an update is about to
// change; whether it differs is a separate question.
func ServiceBlock(file, serviceName string) (map[string]any, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	return serviceBlockOf(data, file, serviceName)
}

func serviceBlockOf(data []byte, name, serviceName string) (map[string]any, error) {
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", name, err)
	}
	svc, ok := doc.Services[serviceName]
	if !ok {
		return nil, fmt.Errorf("service %q not found in %s", serviceName, name)
	}
	block := make(map[string]any, len(svc))
	for k, v := range svc {
		if k != "image" {
			block[k] = v
		}
	}
	return block, nil
}

// ServiceBlockOf is ServiceBlock for content already in hand, such as a file as
// it was committed. name is only for error messages.
func ServiceBlockOf(data []byte, name, serviceName string) (map[string]any, error) {
	return serviceBlockOf(data, name, serviceName)
}

// ServiceFiles is the other files a service reads at deploy time: its
// `env_file` entries and the file of an `extends`, resolved against the
// compose file's directory. An edit to one changes what the service runs with
// without touching the compose file.
//
// Entries that are not plain paths are skipped. `env_file` may be a string, a
// list, or a list of {path, required} mappings; `extends` a mapping with
// `file`, or only `service` (the same file, which is the compose file itself).
func ServiceFiles(file, serviceName string) ([]string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Services map[string]struct {
			EnvFile yaml.Node `yaml:"env_file"`
			Extends yaml.Node `yaml:"extends"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", file, err)
	}
	svc, ok := doc.Services[serviceName]
	if !ok {
		return nil, fmt.Errorf("service %q not found in %s", serviceName, file)
	}

	var paths []string
	switch svc.EnvFile.Kind {
	case yaml.ScalarNode:
		paths = append(paths, svc.EnvFile.Value)
	case yaml.SequenceNode:
		for _, n := range svc.EnvFile.Content {
			switch n.Kind {
			case yaml.ScalarNode:
				paths = append(paths, n.Value)
			case yaml.MappingNode:
				var e struct {
					Path string `yaml:"path"`
				}
				if err := n.Decode(&e); err == nil && e.Path != "" {
					paths = append(paths, e.Path)
				}
			}
		}
	}
	if svc.Extends.Kind == yaml.MappingNode {
		var e struct {
			File string `yaml:"file"`
		}
		if err := svc.Extends.Decode(&e); err == nil && e.File != "" {
			paths = append(paths, e.File)
		}
	}

	dir := filepath.Dir(file)
	for i, p := range paths {
		if !filepath.IsAbs(p) {
			paths[i] = filepath.Join(dir, p)
		}
	}
	return paths, nil
}
