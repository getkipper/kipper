package installer

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

func TestRenderZotHtpasswd(t *testing.T) {
	content, err := renderZotHtpasswd("push-pw", "pull-pw")
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(content), "\n")
	require.Len(t, lines, 2)

	// Each line must be user:bcrypt-hash and the hash must verify against
	// the password it was derived from — zot accepts only bcrypt.
	push := strings.SplitN(lines[0], ":", 2)
	require.Len(t, push, 2)
	assert.Equal(t, "kipper-push", push[0])
	assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(push[1]), []byte("push-pw")))
	assert.Error(t, bcrypt.CompareHashAndPassword([]byte(push[1]), []byte("wrong")))

	pull := strings.SplitN(lines[1], ":", 2)
	require.Len(t, pull, 2)
	assert.Equal(t, "kipper-pull", pull[0])
	assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(pull[1]), []byte("pull-pw")))
}

func TestRenderZotRegistriesConfig(t *testing.T) {
	got := renderZotRegistriesConfig("10.43.0.17", "deadbeef")

	assert.Equal(t, `mirrors:
  "zot.kipper-system.svc.cluster.local:5000":
    endpoint:
      - "https://10.43.0.17:5000"
configs:
  "10.43.0.17:5000":
    auth:
      username: "kipper-pull"
      password: "deadbeef"
    tls:
      ca_file: /etc/rancher/k3s/zot-ca.crt
`, got)
}

func TestZotConfigEnforcesAuthAndTLS(t *testing.T) {
	raw := zotConfigJSON

	// The nesting is load-bearing, not cosmetic: accessControl is a peer of auth
	// under http. zot rejects the whole config when it sits at the root, and the
	// Recreate strategy turns that rejection into a registry outage.
	var cfg struct {
		HTTP struct {
			TLS struct {
				Cert string `json:"cert"`
				Key  string `json:"key"`
			} `json:"tls"`
			Auth struct {
				Htpasswd struct {
					Path string `json:"path"`
				} `json:"htpasswd"`
			} `json:"auth"`
			AccessControl struct {
				Repositories map[string]struct {
					Policies []struct {
						Users   []string `json:"users"`
						Actions []string `json:"actions"`
					} `json:"policies"`
					DefaultPolicy []string `json:"defaultPolicy"`
				} `json:"repositories"`
			} `json:"accessControl"`
		} `json:"http"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &cfg), "config.json must be valid JSON")

	assert.Equal(t, "/etc/zot-tls/tls.crt", cfg.HTTP.TLS.Cert)
	assert.Equal(t, "/etc/zot-tls/tls.key", cfg.HTTP.TLS.Key)
	assert.Equal(t, "/etc/zot-auth/htpasswd", cfg.HTTP.Auth.Htpasswd.Path)

	// zot validates strictly and names the offending key, so anything the schema
	// does not know at the root fails the config outright.
	var root map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(raw), &root))
	for key := range root {
		assert.Contains(t, []string{"storage", "http", "log"}, key,
			"zot rejects unknown root keys — %q must be nested where its schema puts it", key)
	}

	all, ok := cfg.HTTP.AccessControl.Repositories["**"]
	require.True(t, ok, "accessControl must cover every repository")
	assert.Empty(t, all.DefaultPolicy, "authenticated users without a policy must get nothing")
	assert.NotContains(t, raw, "anonymousPolicy", "anonymous access must not exist")

	byUser := map[string][]string{}
	for _, p := range all.Policies {
		for _, u := range p.Users {
			byUser[u] = p.Actions
		}
	}
	assert.Equal(t, []string{"read", "create", "update", "delete"}, byUser["kipper-push"])
	assert.Equal(t, []string{"read"}, byUser["kipper-pull"],
		"the node credential must stay read-only: it lives on every node, and push access from a node is supply-chain compromise")
}

func TestZotDeploymentMountsAuthAndTLS(t *testing.T) {
	rendered := renderZotRuntimeManifest("zot-config-abcdef1234")

	// The deployment must mount the secrets where config.json expects them,
	// and must not probe /v2/ over HTTP: it answers 401 without credentials,
	// which the kubelet counts as failure.
	assert.Contains(t, rendered, "mountPath: /etc/zot-auth")
	assert.Contains(t, rendered, "mountPath: /etc/zot-tls")
	assert.Contains(t, rendered, "secretName: zot-htpasswd")
	assert.Contains(t, rendered, "secretName: zot-tls")
	assert.Contains(t, rendered, "tcpSocket")
	assert.NotContains(t, rendered, "httpGet")
}

func TestRenderZotRuntimeManifest(t *testing.T) {
	rendered := renderZotRuntimeManifest("zot-config-abcdef1234")

	// The ConfigMap is content-named and immutable, and the Deployment must
	// reference exactly that object — the cutover's fail-closed property
	// rests on the running pod never sharing a config object with the new
	// one.
	assert.Equal(t, 2, strings.Count(rendered, "zot-config-abcdef1234"),
		"the config name must appear as the ConfigMap name and the volume reference")
	assert.Contains(t, rendered, "immutable: true")
	assert.NotContains(t, rendered, "name: zot-config\n", "no fixed-name config object may remain")

	// The embedded JSON must survive YAML block-scalar indentation intact.
	const marker = "config.json: |\n"
	start := strings.Index(rendered, marker)
	require.NotEqual(t, -1, start)
	var buf strings.Builder
	for _, line := range strings.Split(rendered[start+len(marker):], "\n") {
		if line != "" && !strings.HasPrefix(line, "    ") {
			break
		}
		buf.WriteString(strings.TrimPrefix(line, "    "))
		buf.WriteString("\n")
	}
	var fromManifest, fromConst any
	require.NoError(t, json.Unmarshal([]byte(buf.String()), &fromManifest))
	require.NoError(t, json.Unmarshal([]byte(zotConfigJSON), &fromConst))
	assert.Equal(t, fromConst, fromManifest)
}

func TestZotConfigMapName(t *testing.T) {
	name := zotConfigMapName()
	assert.Regexp(t, `^zot-config-[0-9a-f]{10}$`, name)
	assert.Equal(t, name, zotConfigMapName(), "the name must be stable for identical config")
}

func TestZotCertTemplateCoversAllAccessPaths(t *testing.T) {
	rendered := strings.Replace(zotCertManifestTemplate, "%s", "10.43.0.17", 1)

	// containerd verifies the mirror endpoint by ClusterIP, builds and
	// console-api verify by Service DNS, kip tunnel verifies by localhost.
	assert.Contains(t, rendered, "- zot.kipper-system.svc.cluster.local")
	assert.Contains(t, rendered, "- zot.kipper-system.svc")
	assert.Contains(t, rendered, "- localhost")
	assert.Contains(t, rendered, "- 10.43.0.17")
	assert.Contains(t, rendered, "- 127.0.0.1")
	assert.Contains(t, rendered, "secretName: zot-tls")
}

// A rejected config is the likeliest reason the registry will not start, and zot
// names the offending key in its log. Carrying that first line into the error is
// the difference between "timed out" and knowing what to fix.
func TestFormatZotDiagnosis(t *testing.T) {
	logs := `{"level":"error","error":"decoding failed due to the following error(s):\n\n'' has invalid keys: accesscontrol","message":"failed to unmarshal new config"}
Error: decoding failed`
	got := formatZotDiagnosis(logs)
	assert.Contains(t, got, "invalid keys: accesscontrol", "the pod's own reason must reach the operator")

	assert.Empty(t, formatZotDiagnosis(""), "no logs means nothing to add")
	assert.Empty(t, formatZotDiagnosis("\n  \n"), "blank logs means nothing to add")

	long := formatZotDiagnosis(strings.Repeat("x", 500))
	assert.Less(t, len(long), 360, "a runaway log line must be truncated, not pasted whole")
	assert.Contains(t, long, "…")
}
