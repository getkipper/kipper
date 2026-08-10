package blueprint

import (
	"embed"
	"fmt"
	"sort"
	"strings"
)

//go:embed builtin/*.yaml
var builtinFS embed.FS

// List returns all available blueprints sorted by name.
func List() ([]Metadata, error) {
	entries, err := builtinFS.ReadDir("builtin")
	if err != nil {
		return nil, fmt.Errorf("reading builtin blueprints: %w", err)
	}

	var blueprints []Metadata
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}

		data, readErr := builtinFS.ReadFile("builtin/" + entry.Name())
		if readErr != nil {
			continue
		}

		bp, parseErr := Parse(data)
		if parseErr != nil {
			continue
		}

		blueprints = append(blueprints, bp.Metadata)
	}

	sort.Slice(blueprints, func(i, j int) bool {
		return blueprints[i].Name < blueprints[j].Name
	})

	return blueprints, nil
}

// Get loads a blueprint by name from the built-in registry.
func Get(name string) (*Blueprint, error) {
	entries, err := builtinFS.ReadDir("builtin")
	if err != nil {
		return nil, fmt.Errorf("reading builtin blueprints: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		data, readErr := builtinFS.ReadFile("builtin/" + entry.Name())
		if readErr != nil {
			continue
		}

		bp, parseErr := Parse(data)
		if parseErr != nil {
			continue
		}

		if bp.Metadata.Name == name {
			return bp, nil
		}
	}

	return nil, fmt.Errorf("blueprint %q not found", name)
}
