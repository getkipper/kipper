package installer

import (
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestRenderTrustedIPs(t *testing.T) {
	tests := []struct {
		name    string
		in      []string
		want    string
		wantErr bool
	}{
		{"empty list", nil, "[]", false},
		{"blank entries only", []string{"", "  "}, "[]", false},
		{"bare IPv4 normalised to host prefix", []string{"203.0.113.7"}, `["203.0.113.7/32"]`, false},
		{"cidr kept", []string{"10.0.0.0/8"}, `["10.0.0.0/8"]`, false},
		{"ipv6 host prefix", []string{"2001:db8::1"}, `["2001:db8::1/128"]`, false},
		{"duplicates collapse", []string{"203.0.113.7", "203.0.113.7/32"}, `["203.0.113.7/32"]`, false},
		{"hostname rejected", []string{"lb.example.com"}, "", true},
		// The rendered value lands inside a kubectl heredoc; anything that
		// is not a bare IP or CIDR must be rejected, never interpolated.
		{"injection rejected", []string{"1.2.3.4\"]\nKIPEOF\nrm -rf /"}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := renderTrustedIPs(tt.in)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestTraefikManifestTransportPosture(t *testing.T) {
	trusted, err := renderTrustedIPs([]string{"203.0.113.7"})
	require.NoError(t, err)
	manifest := fmt.Sprintf(traefikManifestTemplate, trusted, trusted)

	// Cleartext HTTP must redirect before any route (and any API key) is
	// processed, and forwarded headers are trusted only from the list.
	assert.Contains(t, manifest, "redirections:")
	assert.Contains(t, manifest, "to: websecure")
	assert.Contains(t, manifest, "scheme: https")
	assert.Equal(t, 2, strings.Count(manifest, `trustedIPs: ["203.0.113.7/32"]`),
		"both entrypoints carry the same trust list")
	assert.NotContains(t, manifest, "forwardedHeaders.insecure",
		"insecure forwarded headers must never come back")
}

// traefikValues parses the HelmChart's spec.valuesContent out of the rendered
// manifest into a navigable map, so tests assert exact value paths rather than
// substring presence (a value at the wrong path is silently ignored by Helm).
func traefikValues(t *testing.T) map[string]any {
	t.Helper()
	manifest := fmt.Sprintf(traefikManifestTemplate, "[]", "[]")

	dec := yaml.NewDecoder(strings.NewReader(manifest))
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if err != nil {
			break
		}
		if doc["kind"] != "HelmChart" {
			continue
		}
		spec, _ := doc["spec"].(map[string]any)
		content, ok := spec["valuesContent"].(string)
		require.True(t, ok, "HelmChart spec.valuesContent must be a string")

		var values map[string]any
		require.NoError(t, yaml.Unmarshal([]byte(content), &values))
		return values
	}
	t.Fatal("no HelmChart document found in the Traefik manifest")
	return nil
}

// dig walks a nested map by keys, failing the test if any segment is missing.
func dig(t *testing.T, m map[string]any, path ...string) any {
	t.Helper()
	var cur any = m
	for _, key := range path {
		asMap, ok := cur.(map[string]any)
		require.Truef(t, ok, "path %v: %q is not a map", path, key)
		cur, ok = asMap[key]
		require.Truef(t, ok, "path %v: missing key %q", path, key)
	}
	return cur
}

func TestTraefikManifestPinsPatchedVersion(t *testing.T) {
	values := traefikValues(t)

	// version-gated chart logic must see the real running version, which it
	// cannot derive from a digest, and the pulled image must be the patched
	// v3.7.8 by digest (the chart's own appVersion v3.7.6 still has the
	// ReplacePathRegex path-traversal auth-bypass CVE).
	assert.Equal(t, "v3.7.8", dig(t, values, "versionOverride"))
	assert.Equal(t, "v3.7.8", dig(t, values, "image", "tag"))
	assert.Equal(t, "sha256:4299bbed850421258fc5448c2e0e6ad350981d4d335a68de11b92448aedbefe5",
		dig(t, values, "image", "digest"))

	// The chart version pin itself (asserted on the HelmChart spec).
	manifest := fmt.Sprintf(traefikManifestTemplate, "[]", "[]")
	assert.Contains(t, manifest, "version: 41.0.2")
}

func TestTraefikRedirectSitsAtChart41Path(t *testing.T) {
	values := traefikValues(t)

	// The HTTP→HTTPS redirect must sit at ports.web.http.redirections.entryPoint
	// for chart 41; the pre-41 path (ports.web.redirections) is silently
	// ignored, which would carry API keys over cleartext HTTP. Assert the exact
	// path and the redirect target, so a misplacement fails the test.
	entryPoint := dig(t, values, "ports", "web", "http", "redirections", "entryPoint")
	ep, ok := entryPoint.(map[string]any)
	require.True(t, ok, "redirections.entryPoint must be a map")
	assert.Equal(t, "websecure", ep["to"])
	assert.Equal(t, "https", ep["scheme"])
	assert.Equal(t, true, ep["permanent"])

	// The pre-41 path must not carry a redirect that Helm would ignore.
	web := dig(t, values, "ports", "web").(map[string]any)
	_, staleRedirect := web["redirections"]
	assert.False(t, staleRedirect, "redirections must not sit directly under ports.web (the ignored pre-41 path)")
}

func TestResolveTrustedProxies(t *testing.T) {
	orig := lookupIP
	defer func() { lookupIP = orig }()

	lookupIP = func(host string) ([]net.IP, error) {
		assert.Equal(t, "shop.kipper.run", host)
		return []net.IP{net.ParseIP("198.51.100.9")}, nil
	}
	got := ResolveTrustedProxies("shop.kipper.run", []string{"10.0.0.0/8"})
	assert.Equal(t, []string{"10.0.0.0/8", "198.51.100.9"}, got)

	// Custom domains resolve nothing: only the operator's extras apply.
	lookupIP = func(string) ([]net.IP, error) {
		t.Fatal("custom domains must not trigger gateway resolution")
		return nil, nil
	}
	got = ResolveTrustedProxies("api.example.com", []string{"10.0.0.0/8"})
	assert.Equal(t, []string{"10.0.0.0/8"}, got)

	// Resolution failure warns and continues with the extras.
	lookupIP = func(string) ([]net.IP, error) { return nil, fmt.Errorf("dns down") }
	got = ResolveTrustedProxies("shop.kipper.run", []string{"10.0.0.0/8"})
	assert.Equal(t, []string{"10.0.0.0/8"}, got)
}

// externalTrafficPolicy Local and the placement of the single Traefik replica
// are one decision, not two. Under Local, kube-proxy serves the NodePort only on
// a node that has a Traefik pod, and k3s's service LB DNATs :443 to its own
// node's NodePort — verified on a live cluster: the svclb DaemonSet runs
// everywhere with DEST_IPS=status.hostIPs and DEST_PORT=<nodePort>. So Traefik
// on a worker means the server node answers nothing on :443, which is both the
// address the cluster publishes and the address the API server resolves its OIDC
// issuer to through the loopback pin. Keeping one without the other is the trap.
func TestIngressRunsWhereThePublishedAddressPoints(t *testing.T) {
	values := traefikValues(t)

	policy := dig(t, values, "service", "spec", "externalTrafficPolicy")
	selector, ok := dig(t, values, "nodeSelector").(map[string]any)
	if !ok {
		t.Fatalf("nodeSelector must be a map, got %T", dig(t, values, "nodeSelector"))
	}

	if policy == "Local" && selector["node-role.kubernetes.io/control-plane"] != "true" {
		t.Error("externalTrafficPolicy Local without pinning Traefik to the server node: " +
			"a replica scheduled onto a worker leaves the published address dropping traffic, " +
			"and the loopback pin with it")
	}
	if policy != "Local" && selector != nil {
		t.Error("the nodeSelector exists to make Local coherent; if the policy changed, revisit whether " +
			"pinning ingress to one node is still wanted")
	}

	// A server node carrying the usual k3s taints must still accept it, or the
	// selector would leave the pod Pending and the cluster serving nothing.
	tolerations, ok := dig(t, values, "tolerations").([]any)
	if !ok || len(tolerations) == 0 {
		t.Fatal("pinning to the control plane needs tolerations, or a tainted server leaves Traefik Pending")
	}
	var keys []string
	for _, entry := range tolerations {
		if m, isMap := entry.(map[string]any); isMap {
			keys = append(keys, fmt.Sprint(m["key"]))
		}
	}
	for _, want := range []string{"node-role.kubernetes.io/control-plane", "CriticalAddonsOnly"} {
		if !slices.Contains(keys, want) {
			t.Errorf("missing toleration for %s; a server with that taint would not run ingress", want)
		}
	}
}
