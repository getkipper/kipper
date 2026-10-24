package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/getkipper/kipper/controller/pkg/capacity"
	"github.com/getkipper/kipper/controller/pkg/healthcheck"
	"github.com/getkipper/kipper/controller/pkg/internalpath"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

// redirectFromHostPattern is the shape a route.redirectFrom entry must have:
// at least two lowercase DNS labels. The reconciler applies the same rule;
// failing at parse time surfaces the problem before anything is applied.
var redirectFromHostPattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

var routePathPattern = regexp.MustCompile(internalpath.SchemaPattern)

// ValidateRouteHost requires a lowercase DNS name with at least two labels
// and at most 253 characters.
func ValidateRouteHost(host string) error {
	if len(host) > 253 || !redirectFromHostPattern.MatchString(host) {
		return fmt.Errorf("%q is not a valid hostname (lowercase DNS name like shop.example.com, without a scheme or path)", host)
	}
	return nil
}

// ValidateRoutePath accepts absolute paths with ASCII letters, digits and . _ ~ -.
// Segments must be nonempty and cannot be "." or ".."; one trailing slash is allowed.
func ValidateRoutePath(path string) error {
	if path == "/" {
		return nil
	}
	trimmed := strings.TrimSuffix(path, "/")
	if !strings.HasPrefix(path, "/") || !routePathPattern.MatchString(trimmed) {
		return fmt.Errorf("%q is not a valid route path (use / or slash-separated segments of ASCII letters, digits and . _ ~ -; empty, . and .. segments are invalid; one trailing slash is allowed)", path)
	}
	return nil
}

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

// Validate checks required fields and validates app health, capacity and route settings.
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
		if app.Health != nil {
			if app.Health.Type == healthcheck.Auto {
				return fmt.Errorf("app %q: health: leave the health block out to let Kipper decide", name)
			}
			if err := app.Health.check().Validate(app.Port); err != nil {
				return fmt.Errorf("app %q: health: %w", name, err)
			}
		}
		if err := validateLinks(app.Links); err != nil {
			return fmt.Errorf("app %q: %w", name, err)
		}
		if err := capacity.Validate(app.Autoscale.policy(), app.Replicas); err != nil {
			return fmt.Errorf("app %q: autoscale: %w", name, err)
		}
		if app.Route != nil {
			if err := ValidateRedirectFromHosts(app.Route.RedirectFrom); err != nil {
				return fmt.Errorf("app %q: %w", name, err)
			}
			if err := internalpath.Validate(app.Route.InternalPaths); err != nil {
				return fmt.Errorf("app %q: route.internalPaths: %w", name, err)
			}
			if err := internalpath.Validate(app.Route.PublicPaths); err != nil {
				return fmt.Errorf("app %q: route.publicPaths: %w", name, err)
			}
		}
	}

	for name, svc := range m.Services {
		if svc.Type == "" {
			return fmt.Errorf("service %q: type is required", name)
		}
		if svc.TLS != "" && svc.Type != "postgres" {
			return fmt.Errorf("service %q: tls is supported for postgres services only", name)
		}
		if svc.ConsistentBackups && svc.Type != "postgres" {
			return fmt.Errorf("service %q: consistentBackups is supported for postgres services only", name)
		}
		if svc.TLS != "" && svc.TLS != "required" && svc.TLS != "on" && svc.TLS != "off" {
			return fmt.Errorf("service %q: tls must be required, on or off, not %q", name, svc.TLS)
		}
		if !validCopies(svc.Copies) {
			return fmt.Errorf("service %q: copies must be 1 or 2, not %d", name, *svc.Copies)
		}
	}

	for name, vol := range m.Volumes {
		if vol.Size == "" {
			return fmt.Errorf("volume %q: size is required", name)
		}
		if !validCopies(vol.Copies) {
			return fmt.Errorf("volume %q: copies must be 1 or 2, not %d", name, *vol.Copies)
		}
	}

	for name, job := range m.Jobs {
		if job.Image == "" {
			return fmt.Errorf("job %q: image is required", name)
		}
	}

	return nil
}

// maxLinks matches the App CRD's limit on spec.links.
const maxLinks = 64

var linkTargetName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// validCopies accepts an omitted copies value, 1 or 2.
func validCopies(copies *int32) bool {
	return copies == nil || (*copies >= 1 && *copies <= platform.MaxCopies)
}

func validateLinks(links []LinkSpec) error {
	if len(links) > maxLinks {
		return fmt.Errorf("links: at most %d are allowed, got %d", maxLinks, len(links))
	}
	for i, l := range links {
		if !linkTargetName.MatchString(l.App) {
			return fmt.Errorf("links[%d]: app %q is not a valid app name", i, l.App)
		}
		if l.Environment != "" && l.Project == "" {
			return fmt.Errorf("links[%d]: project is required when environment is set", i)
		}
	}
	return nil
}
