package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// redirectFromHostPattern is the shape a route.redirectFrom entry must have:
// at least two lowercase DNS labels. The reconciler applies the same rule;
// failing at parse time surfaces the problem before anything is applied.
var redirectFromHostPattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// maxRedirectFromHosts matches the App CRD's cap on route.redirectFrom.
const maxRedirectFromHosts = 10

// ValidateRedirectFromHosts applies the rules a route's redirect domains have
// to satisfy: at most ten, each a lowercase DNS name of at least two labels,
// none of them a kipper.run hostname.
//
// Exported because `kip app deploy --redirect-from` asks the same question, and
// a second spelling of it would drift. The CRD and the reconciler check this
// too; failing here means a malformed host is refused before anything is
// written rather than after.
func ValidateRedirectFromHosts(hosts []string) error {
	if len(hosts) > maxRedirectFromHosts {
		return fmt.Errorf("route.redirectFrom supports at most %d hostnames", maxRedirectFromHosts)
	}
	for _, h := range hosts {
		if len(h) > 253 || !redirectFromHostPattern.MatchString(h) {
			return fmt.Errorf("route.redirectFrom entry %q is not a valid hostname (lowercase DNS name like www.example.com)", h)
		}
		if strings.HasSuffix(h, ".kipper.run") || h == "kipper.run" {
			return fmt.Errorf("route.redirectFrom entry %q: kipper.run hostnames cannot serve redirects", h)
		}
	}
	return nil
}

// ParseFile reads and validates a single kipper.yaml file.
func ParseFile(path string) (*Manifest, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is from CLI flag, not user input
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	if err := Validate(&m); err != nil {
		return nil, fmt.Errorf("validating %s: %w", path, err)
	}

	return &m, nil
}

// ParseDir reads all .yaml/.yml files in a directory and returns a manifest per file.
func ParseDir(dir string) ([]*Manifest, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading directory %s: %w", dir, err)
	}

	var manifests []*Manifest
	for _, entry := range entries {
		if entry.IsDir() {
			// Recurse into subdirectories
			sub, subErr := ParseDir(filepath.Join(dir, entry.Name()))
			if subErr != nil {
				return nil, subErr
			}
			manifests = append(manifests, sub...)
			continue
		}

		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}

		m, parseErr := ParseFile(filepath.Join(dir, name))
		if parseErr != nil {
			return nil, parseErr
		}
		manifests = append(manifests, m)
	}

	return manifests, nil
}

// Parse reads a file or directory and returns all manifests found.
func Parse(path string) ([]*Manifest, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	if info.IsDir() {
		return ParseDir(path)
	}

	m, err := ParseFile(path)
	if err != nil {
		return nil, err
	}
	return []*Manifest{m}, nil
}

// Validate checks that a manifest has all required fields.
func Validate(m *Manifest) error {
	if m.Project == "" {
		return fmt.Errorf("project is required")
	}

	for name, app := range m.Apps {
		if app.Port == 0 {
			return fmt.Errorf("app %q: port is required", name)
		}
		if app.Image == "" && app.Git == nil {
			return fmt.Errorf("app %q: either image or git is required", name)
		}
		if app.Image != "" && app.Git != nil {
			return fmt.Errorf("app %q: image and git are mutually exclusive", name)
		}
		if app.Git != nil && app.Git.URL == "" {
			return fmt.Errorf("app %q: git.url is required", name)
		}
		if app.Route != nil {
			if err := ValidateRedirectFromHosts(app.Route.RedirectFrom); err != nil {
				return fmt.Errorf("app %q: %w", name, err)
			}
		}
	}

	for name, svc := range m.Services {
		if svc.Type == "" {
			return fmt.Errorf("service %q: type is required", name)
		}
	}

	for name, vol := range m.Volumes {
		if vol.Size == "" {
			return fmt.Errorf("volume %q: size is required", name)
		}
	}

	for name, job := range m.Jobs {
		if job.Image == "" {
			return fmt.Errorf("job %q: image is required", name)
		}
	}

	return nil
}
