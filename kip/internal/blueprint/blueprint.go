package blueprint

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"

	"github.com/getkipper/kipper/kip/internal/manifest"
)

// templateFuncs provides helper functions available in blueprint templates.
var templateFuncs = template.FuncMap{
	"generateSecret": func(length int) string {
		b := make([]byte, length)
		_, _ = rand.Read(b)
		return hex.EncodeToString(b)
	},
}

// Parse splits a blueprint file into metadata and template.
// The file contains two YAML documents separated by "---".
func Parse(data []byte) (*Blueprint, error) {
	parts := splitDocuments(string(data))
	if len(parts) < 2 {
		return nil, fmt.Errorf("blueprint must contain two YAML documents (metadata and template) separated by ---")
	}

	var meta Metadata
	if err := yaml.Unmarshal([]byte(parts[0]), &meta); err != nil {
		return nil, fmt.Errorf("parsing blueprint metadata: %w", err)
	}

	if meta.Name == "" {
		return nil, fmt.Errorf("blueprint metadata must have a name")
	}

	return &Blueprint{
		Metadata: meta,
		Template: parts[1],
	}, nil
}

// Render applies the parameter values to the template and returns a parsed Manifest.
func (b *Blueprint) Render(params map[string]string) (*manifest.Manifest, error) {
	// Apply defaults for missing parameters
	values := make(map[string]string)
	for _, p := range b.Metadata.Parameters {
		values[p.Name] = p.Default
	}
	for k, v := range params {
		values[k] = v
	}

	// Validate required parameters
	for _, p := range b.Metadata.Parameters {
		if p.Required {
			if v, ok := values[p.Name]; !ok || v == "" {
				return nil, fmt.Errorf("required parameter %q is missing", p.Name)
			}
		}
	}

	// Render the Go template
	tmpl, err := template.New("blueprint").Funcs(templateFuncs).Option("missingkey=error").Parse(b.Template)
	if err != nil {
		return nil, fmt.Errorf("parsing template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, values); err != nil {
		return nil, fmt.Errorf("rendering template: %w", err)
	}

	// Parse the rendered YAML as a manifest
	var m manifest.Manifest
	if err := yaml.Unmarshal(buf.Bytes(), &m); err != nil {
		return nil, fmt.Errorf("parsing rendered manifest: %w", err)
	}

	if err := manifest.Validate(&m); err != nil {
		return nil, fmt.Errorf("validating rendered manifest: %w", err)
	}

	return &m, nil
}

// RenderYAML renders the template to raw YAML bytes (for kip init --blueprint).
func (b *Blueprint) RenderYAML(params map[string]string) ([]byte, error) {
	values := make(map[string]string)
	for _, p := range b.Metadata.Parameters {
		values[p.Name] = p.Default
	}
	for k, v := range params {
		values[k] = v
	}

	for _, p := range b.Metadata.Parameters {
		if p.Required {
			if v, ok := values[p.Name]; !ok || v == "" {
				return nil, fmt.Errorf("required parameter %q is missing", p.Name)
			}
		}
	}

	tmpl, err := template.New("blueprint").Funcs(templateFuncs).Option("missingkey=error").Parse(b.Template)
	if err != nil {
		return nil, fmt.Errorf("parsing template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, values); err != nil {
		return nil, fmt.Errorf("rendering template: %w", err)
	}

	return buf.Bytes(), nil
}

func splitDocuments(content string) []string {
	var parts []string
	var current strings.Builder

	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == "---" {
			if current.Len() > 0 {
				parts = append(parts, current.String())
				current.Reset()
			}
			continue
		}
		current.WriteString(line)
		current.WriteString("\n")
	}

	if current.Len() > 0 {
		parts = append(parts, current.String())
	}

	return parts
}
