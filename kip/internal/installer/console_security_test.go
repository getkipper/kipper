package installer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// consoleAPIDeployment renders the console manifest and returns the
// console-api Deployment as a navigable map.
func consoleAPIDeployment(t *testing.T) map[string]any {
	t.Helper()
	manifest := renderConsoleManifest(
		"dex--acme.kipper.run", "console--acme.kipper.run", "console-api--acme.kipper.run",
		"acme.kipper.run", "acme.kipper.run", "203.0.113.10")

	dec := yaml.NewDecoder(strings.NewReader(manifest))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			break
		}
		if doc["kind"] != "Deployment" {
			continue
		}
		meta, _ := doc["metadata"].(map[string]any)
		if meta["name"] == "console-api" {
			return doc
		}
	}
	t.Fatal("console-api Deployment not found in the rendered manifest")
	return nil
}

// dig is defined in traefik_test.go (same package).

// The console-api pod holds a cluster-powerful ServiceAccount, so its pod
// must run locked down. A regression that drops any of these silently widens
// what a console-api compromise can do on the node.
func TestConsoleAPIPodIsHardened(t *testing.T) {
	dep := consoleAPIDeployment(t)
	podSpec := dig(t, dep, "spec", "template", "spec").(map[string]any)

	podSec := dig(t, dep, "spec", "template", "spec", "securityContext").(map[string]any)
	assert.Equal(t, true, podSec["runAsNonRoot"], "pod must run as non-root")
	assert.EqualValues(t, 65532, podSec["runAsUser"], "pod must run as the nonroot UID")
	assert.EqualValues(t, 65532, podSec["runAsGroup"], "pod must run as the nonroot GID")
	assert.EqualValues(t, 65532, podSec["fsGroup"], "fsGroup must match the run GID so projected volumes are readable")
	seccomp := dig(t, dep, "spec", "template", "spec", "securityContext", "seccompProfile").(map[string]any)
	assert.Equal(t, "RuntimeDefault", seccomp["type"], "pod must use the default seccomp profile")

	containers, ok := podSpec["containers"].([]any)
	require.True(t, ok, "containers must be a list")
	require.Len(t, containers, 1)
	container := containers[0].(map[string]any)
	cSec := dig(t, container, "securityContext").(map[string]any)
	assert.Equal(t, false, cSec["allowPrivilegeEscalation"], "privilege escalation must be disabled")
	assert.Equal(t, true, cSec["readOnlyRootFilesystem"], "the root filesystem must be read-only")

	dropped := dig(t, container, "securityContext", "capabilities", "drop")
	assert.Contains(t, dropped, "ALL", "all Linux capabilities must be dropped")

	// A read-only rootfs needs a writable /tmp. The mount and the volume must
	// share the name "tmp" and the volume must be an emptyDir, so the mount is
	// actually backed by ephemeral storage rather than dangling.
	mounts, ok := container["volumeMounts"].([]any)
	require.True(t, ok, "the container needs a /tmp volume mount")
	var tmpMount bool
	for _, m := range mounts {
		if mm, ok := m.(map[string]any); ok && mm["mountPath"] == "/tmp" && mm["name"] == "tmp" {
			tmpMount = true
		}
	}
	assert.True(t, tmpMount, "a writable /tmp must be mounted from the volume named tmp")

	volumes, ok := podSpec["volumes"].([]any)
	require.True(t, ok, "the pod needs a volumes list")
	var tmpVol bool
	for _, v := range volumes {
		if vv, ok := v.(map[string]any); ok && vv["name"] == "tmp" {
			_, isEmptyDir := vv["emptyDir"]
			tmpVol = isEmptyDir
		}
	}
	assert.True(t, tmpVol, "the tmp volume must be an emptyDir")
}

// The console, console-api, and Dex hosts must carry HSTS and the
// clickjacking/MIME/referrer headers, via a Traefik Middleware referenced from
// each platform ingress. A regression that drops these leaves the login and
// admin surfaces without HSTS or frame protection.
func TestPlatformHostsGetSecurityHeaders(t *testing.T) {
	cases := []struct {
		manifest  string
		namespace string
		ingresses []string
	}{
		{
			manifest:  renderConsoleManifest("dex--acme.kipper.run", "console--acme.kipper.run", "console-api--acme.kipper.run", "acme.kipper.run", "acme.kipper.run", "203.0.113.10"),
			namespace: "kipper-system",
			ingresses: []string{"console", "console-api"},
		},
		{
			manifest:  renderDexManifest("dex--acme.kipper.run", "console--acme.kipper.run", "acme.kipper.run", "fakehash"),
			namespace: "dex",
			ingresses: []string{"dex"},
		},
	}

	for _, tc := range cases {
		// The Middleware exists in the platform namespace with HSTS + the frame
		// header, and deliberately no CSP (console/Dex own theirs).
		mw := findManifestDoc(t, tc.manifest, "Middleware", "security-headers", tc.namespace)
		spec := dig(t, mw, "spec", "headers").(map[string]any)
		assert.EqualValues(t, 31536000, spec["stsSeconds"], "%s: HSTS max-age must be set", tc.namespace)
		assert.Equal(t, true, spec["stsIncludeSubdomains"], "%s: HSTS must include subdomains", tc.namespace)
		assert.Equal(t, true, spec["stsPreload"], "%s: HSTS preload must be set", tc.namespace)
		assert.Equal(t, "SAMEORIGIN", spec["customFrameOptionsValue"], "%s: frame options must be set", tc.namespace)
		assert.Equal(t, true, spec["contentTypeNosniff"], "%s: nosniff must be set", tc.namespace)
		assert.Equal(t, true, spec["browserXssFilter"], "%s: xss filter must be set", tc.namespace)
		assert.Equal(t, "strict-origin-when-cross-origin", spec["referrerPolicy"], "%s: referrer policy must be set", tc.namespace)
		_, hasCSP := spec["contentSecurityPolicy"]
		assert.False(t, hasCSP, "%s: platform middleware must not override the app CSP", tc.namespace)
		// Server / X-Powered-By must be stripped (empty custom value = deletion).
		crh := dig(t, mw, "spec", "headers", "customResponseHeaders").(map[string]any)
		assert.Equal(t, "", crh["Server"], "%s: Server header must be stripped", tc.namespace)
		assert.Equal(t, "", crh["X-Powered-By"], "%s: X-Powered-By header must be stripped", tc.namespace)

		// Every platform ingress references the middleware.
		want := tc.namespace + "-security-headers@kubernetescrd"
		for _, name := range tc.ingresses {
			ing := findManifestDoc(t, tc.manifest, "Ingress", name, tc.namespace)
			ann := dig(t, ing, "metadata", "annotations").(map[string]any)
			assert.Equal(t, want, ann["traefik.ingress.kubernetes.io/router.middlewares"],
				"ingress %s must reference the security-headers middleware", name)
		}
	}
}

// findManifestDoc returns the YAML document in a multi-doc manifest matching a
// kind, metadata.name, and namespace.
func findManifestDoc(t *testing.T, manifest, kind, name, namespace string) map[string]any {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(manifest))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			break
		}
		if doc["kind"] != kind {
			continue
		}
		meta, _ := doc["metadata"].(map[string]any)
		if meta["name"] == name && meta["namespace"] == namespace {
			return doc
		}
	}
	t.Fatalf("%s %s/%s not found in manifest", kind, namespace, name)
	return nil
}
