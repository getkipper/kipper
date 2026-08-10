package manifest

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// withheld are the spec paths whose values are deliberately not printed.
//
// Together with printable this has to account for every path Convert emits. A
// path in neither is the thing this file exists to catch: the allowlist was
// written from memory once and named resources.cpu and autoscale.min, which
// exist nowhere, while the real cpuRequest and minReplicas went unprinted at the
// one moment the number is what the operator needs.
var withheld = map[string]struct{}{
	// The operator's own text, any of which can carry a credential.
	"env": {}, "command": {}, "args": {},
	"git.url": {}, "git.buildArgs": {},
	"source.code": {}, "source.dependencies": {},
	"route.path": {},

	// Structures rather than values: a diff names them and their leaves are
	// reached through the same rules.
	"secretRefs": {}, "serviceBindings": {}, "volumes": {}, "mounts": {},
	"triggers": {}, "route.redirects": {}, "route.cspAllowlist": {},
	"cspAllowlist": {}, "route.basicAuth": {}, "labels": {}, "config": {},
}

// isWithheld reports whether a path, or a block it sits inside, is withheld.
// display hides a whole block by its prefix, so env.API_TOKEN is covered by env.
func isWithheld(path string) bool {
	for p := path; p != ""; {
		if _, ok := withheld[p]; ok {
			return true
		}
		cut := strings.LastIndex(p, ".")
		if cut < 0 {
			return false
		}
		p = p[:cut]
	}
	return false
}

// leafPaths walks a converted spec the way diffInto does, so what it collects is
// what display is asked about.
func leafPaths(out map[string]struct{}, prefix string, spec map[string]interface{}) {
	for k, v := range spec {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		if nested, isMap := v.(map[string]interface{}); isMap && len(nested) > 0 {
			leafPaths(out, path, nested)
			continue
		}
		out[path] = struct{}{}
	}
}

// A field Convert learns to emit is withheld until somebody classifies it, which
// is the direction to be wrong in — but it must be classified, because a value
// silently hidden is as wrong as one silently printed.
func TestRedaction_ClassifiesEveryPathConvertEmits(t *testing.T) {
	m := &Manifest{
		Project: "acme", Environment: "test",
		Apps: map[string]AppSpec{"api": {
			Image: "nginx:1.27", Port: 8080, Replicas: 2,
			Env: map[string]string{"LOG_LEVEL": "debug"}, SecretRefs: []string{"api-secrets"},
			Route: &RouteSpec{
				Host: "api.example.com", Path: "/v1", Group: "public",
				RedirectFrom: []string{"www.example.com"}, NoSecurityHeaders: true,
				NoInstanceHeader: true, RateLimit: 100, CSPAllowlist: []string{"cdn.example.com"},
				Redirects: []RedirectSpec{{Source: "/old", Target: "/new", Permanent: true}},
				BasicAuth: true, RequireAPIKey: true,
			},
			Resources: &ResourceSpec{
				Profile: "standard", CPURequest: "100m", CPULimit: "500m",
				MemoryRequest: "128Mi", MemoryLimit: "512Mi",
			},
			Autoscale: &AutoscaleSpec{Enabled: true, MinReplicas: 1, MaxReplicas: 5, CPUTarget: 70, MemoryTarget: 80},
			Git: &GitSpec{
				URL: "https://github.com/acme/api.git", Branch: "main",
				CredentialsSecret: "api-git", DockerfilePath: "Dockerfile", Context: ".",
				BuildArgs:      map[string]string{"NPM_TOKEN": "x"},
				BuildResources: &BuildResources{CPU: "2", Memory: "4Gi"},
			},
			ServiceBindings: []BindingSpec{{Name: "db", Prefix: "DB_", Database: "api"}},
			Volumes:         []VolumeMountSpec{{Name: "uploads", MountPath: "/data"}},
		}},
		Services: map[string]SvcSpec{"db": {
			Type: "postgres", Version: "16", Storage: "5Gi",
			Resources: &ResourceSpec{CPURequest: "100m", MemoryRequest: "256Mi"},
		}},
		Volumes: map[string]VolSpec{"uploads": {Size: "10Gi", Mounts: []MountSpec{{App: "api", MountPath: "/data"}}}},
		Jobs: map[string]JobSpec{"cleanup": {
			Image: "busybox", Schedule: "0 3 * * *",
			Command: []string{"sh", "-c", "true"}, Env: map[string]string{"DRY_RUN": "1"},
		}},
		Functions: map[string]FuncSpec{"resize": {
			Image: "resizer:1", Port: 8080, Runtime: "node20",
			Source: &FuncSourceSpec{
				Code: "export default () => {}", Handler: "index.handler",
				Dependencies: map[string]string{"sharp": "^0.33"},
			},
			Env:               map[string]string{"MODE": "fast"},
			Resources:         &ResourceSpec{CPULimit: "1"},
			ServiceBindings:   []BindingSpec{{Name: "db"}},
			Volumes:           []VolumeMountSpec{{Name: "uploads", MountPath: "/data"}},
			Triggers:          []TriggerSpec{{Type: "cron", Schedule: "* * * * *"}},
			NoSecurityHeaders: true,
			CSPAllowlist:      []string{"cdn.example.com"},
		}},
	}

	emitted := map[string]struct{}{}
	for _, res := range Convert(m, "acme-test") {
		spec, _ := res.Object.Object["spec"].(map[string]interface{})
		leafPaths(emitted, "", spec)
	}

	var unclassified []string
	for path := range emitted {
		if _, shown := printable[path]; shown {
			continue
		}
		if isWithheld(path) {
			continue
		}
		unclassified = append(unclassified, path)
	}
	sort.Strings(unclassified)
	assert.Empty(t, unclassified,
		"Convert emits these and nothing says whether their values may be printed; add each to printable or withheld in specdiff.go")

	// And the reverse: an allowlist entry that names nothing is a value the
	// operator was promised and does not get.
	var namesNothing []string
	for path := range printable {
		if _, ok := emitted[path]; !ok {
			namesNothing = append(namesNothing, path)
		}
	}
	sort.Strings(namesNothing)
	assert.Empty(t, namesNothing, "printable names paths Convert never emits")
}

// The paths an operator most needs to read in a clear warning still read.
func TestRedaction_TheNumbersAnOperatorNeedsAreShown(t *testing.T) {
	for _, path := range []string{
		"image", "replicas", "port", "storage", "size", "schedule",
		"resources.cpuRequest", "resources.cpuLimit",
		"resources.memoryRequest", "resources.memoryLimit",
		"autoscale.minReplicas", "autoscale.maxReplicas",
		"git.buildResources.cpu", "git.buildResources.memory",
		"source.handler", "route.host",
	} {
		_, ok := printable[path]
		assert.True(t, ok, path)
	}
}

// Convert must not need a leaf under a path that is withheld whole, or the
// value would be reached through a name nothing classified.
func TestRedaction_WithheldPathsHideWhatIsUnderThem(t *testing.T) {
	spec := map[string]interface{}{
		"env":    map[string]interface{}{"API_TOKEN": "x"},
		"source": map[string]interface{}{"dependencies": map[string]interface{}{"lib": "https://tok@npm.example/lib"}},
	}
	for _, c := range DiffSpec(spec, map[string]interface{}{}, nil, nil) {
		assert.Equal(t, hidden, c.Live, c.Path)
	}
}
