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

	// Verify that accessControl is nested under http alongside auth.
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

	var root map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(raw), &root))
	for key := range root {
		assert.Contains(t, []string{"storage", "http", "log"}, key,
			"unexpected root configuration key %q", key)
	}

	all, ok := cfg.HTTP.AccessControl.Repositories["**"]
	require.True(t, ok, "accessControl must cover every repository")
	assert.Empty(t, all.DefaultPolicy, "authenticated users without a policy must have no access")
	assert.NotContains(t, raw, "anonymousPolicy", "anonymous access must not exist")

	byUser := map[string][]string{}
	for _, p := range all.Policies {
		for _, u := range p.Users {
			byUser[u] = p.Actions
		}
	}
	assert.Equal(t, []string{"read", "create", "update", "delete"}, byUser["kipper-push"])
	assert.Equal(t, []string{"read"}, byUser["kipper-pull"],
		"the node credential must allow image pulls only")
}

func TestZotDeploymentMountsAuthAndTLS(t *testing.T) {
	rendered := renderZotRuntimeManifest("zot-config-abcdef1234")

	assert.Contains(t, rendered, "mountPath: /etc/zot-auth")
	assert.Contains(t, rendered, "mountPath: /etc/zot-tls")
	assert.Contains(t, rendered, "secretName: zot-htpasswd")
	assert.Contains(t, rendered, "secretName: zot-tls")
	// Keep probe credentials out of the pod specification.
	assert.Contains(t, rendered, "tcpSocket")
	assert.NotContains(t, rendered, "httpGet")
}

func TestRenderZotRuntimeManifest(t *testing.T) {
	rendered := renderZotRuntimeManifest("zot-config-abcdef1234")

	// The Deployment must reference the immutable ConfigMap created with it.
	assert.Equal(t, 2, strings.Count(rendered, "zot-config-abcdef1234"),
		"the config name must appear as the ConfigMap name and the volume reference")
	assert.Contains(t, rendered, "immutable: true")
	assert.NotContains(t, rendered, "name: zot-config\n", "the manifest must use the supplied ConfigMap name")

	// Check that YAML indentation preserves the embedded JSON.
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

	assert.Contains(t, rendered, "- zot.kipper-system.svc.cluster.local")
	assert.Contains(t, rendered, "- zot.kipper-system.svc")
	assert.Contains(t, rendered, "- localhost")
	assert.Contains(t, rendered, "- 10.43.0.17")
	assert.Contains(t, rendered, "- 127.0.0.1")
	assert.Contains(t, rendered, "secretName: zot-tls")
}

// Include configuration errors from pod logs in rollout diagnostics.
func TestFormatZotDiagnosis(t *testing.T) {
	logs := `{"level":"error","error":"decoding failed due to the following error(s):\n\n'' has invalid keys: accesscontrol","message":"failed to unmarshal new config"}
Error: decoding failed`
	got := formatZotDiagnosis(logs)
	assert.Contains(t, got, "invalid keys: accesscontrol", "the pod's own reason must reach the operator")

	assert.Empty(t, formatZotDiagnosis(""), "no logs means nothing to add")
	assert.Empty(t, formatZotDiagnosis("\n  \n"), "blank logs mean nothing to add")

	long := formatZotDiagnosis(strings.Repeat("x", 500))
	assert.Less(t, len(long), 360, "long log lines must be truncated")
	assert.Contains(t, long, "…")
}

func TestZotStorageRequest(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		want    string
		wantErr bool
	}{
		{name: "fresh install, no claim yet", out: "", want: "10Gi"},
		{name: "noise but no claim", out: "Last login: Wed\n", want: "10Gi"},
		{name: "claim at the default", out: "zot-size[10Gi|10Gi]", want: "10Gi"},
		{name: "claim expanded by the operator", out: "zot-size[20Gi|20Gi]", want: "20Gi"},
		{name: "capacity grown past the request", out: "zot-size[10Gi|25Gi]", want: "25Gi"},
		{name: "request raised, resize still pending", out: "zot-size[30Gi|10Gi]", want: "30Gi"},
		{name: "smaller legacy claim is raised to the default", out: "zot-size[5Gi|5Gi]", want: "10Gi"},
		{name: "unbound claim reports no capacity", out: "zot-size[20Gi|]", want: "20Gi"},
		{name: "diagnostics on earlier lines", out: "Warning: deprecated\nzot-size[20Gi|20Gi]", want: "20Gi"},
		{name: "startup output without a final newline", out: "Startup message: zot-size[20Gi|20Gi]", want: "20Gi"},
		{name: "diagnostics straight after the payload", out: "zot-size[20Gi|20Gi]Connection closed\r\n", want: "20Gi"},
		{name: "unparseable values", out: "zot-size[lots|more]", wantErr: true},
		{name: "payload without both fields", out: "zot-size[20Gi]", wantErr: true},
		{name: "empty payload", out: "zot-size[|]", wantErr: true},
		{name: "no request", out: "zot-size[|20Gi]", wantErr: true},
		{name: "unterminated payload", out: "zot-size[20Gi|20Gi", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := zotStorageRequest(tt.out)
			if tt.wantErr {
				if err == nil {
					t.Errorf("zotStorageRequest(%q) = %q, want an error", tt.out, got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("zotStorageRequest(%q) = %q, %v; want %q", tt.out, got, err, tt.want)
			}
		})
	}
}

func TestZotStorageQuery_TreatsAMissingClaimAsEmpty(t *testing.T) {
	// A missing claim permits a fresh install; other lookup errors must stop it.
	if !strings.Contains(zotStorageQuery, "--ignore-not-found") {
		t.Error("query must use --ignore-not-found")
	}
	if !strings.Contains(zotStorageQuery, `{"zot-size["}`) || !strings.Contains(zotStorageQuery, `{"]"}`) {
		t.Error("query must wrap its output in size markers")
	}
}

func TestRenderZotBaseManifest_UsesTheGivenSize(t *testing.T) {
	m := renderZotBaseManifest("25Gi")
	if !strings.Contains(m, "storage: 25Gi\n") || strings.Contains(m, "storage: 10Gi") {
		t.Errorf("manifest does not request 25Gi:\n%s", m)
	}
}
