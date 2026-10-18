package installer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestServiceCAManifest(t *testing.T) {
	docs := map[string]map[string]any{}
	for _, doc := range strings.Split(serviceCAManifest, "\n---\n") {
		var obj map[string]any
		require.NoError(t, yaml.Unmarshal([]byte(doc), &obj))
		meta := obj["metadata"].(map[string]any)
		docs[obj["kind"].(string)+"/"+meta["name"].(string)] = obj
	}

	ca := docs["Certificate/"+ServiceCAName]
	require.NotNil(t, ca)
	spec := ca["spec"].(map[string]any)
	assert.Equal(t, true, spec["isCA"])
	assert.Equal(t, "cert-manager", ca["metadata"].(map[string]any)["namespace"], "a ClusterIssuer reads its CA Secret from cert-manager's namespace")
	assert.Equal(t, "Never", spec["privateKey"].(map[string]any)["rotationPolicy"],
		"a renewed root keeps its key, so valid copies of the old CA certificate can still verify signatures")

	template := spec["secretTemplate"].(map[string]any)["labels"].(map[string]any)
	assert.Equal(t, "true", template["velero.io/exclude-from-backup"], "the root key that signs every service certificate stays out of backups")

	issuer := docs["ClusterIssuer/"+ServiceCAName]
	require.NotNil(t, issuer)
	assert.Equal(t, ServiceCAName, issuer["spec"].(map[string]any)["ca"].(map[string]any)["secretName"])
}

func TestInstallServiceCAAppliesTheChain(t *testing.T) {
	host := &routedRunner{}
	require.NoError(t, InstallServiceCA(host))
	assert.Contains(t, strings.Join(host.commands, "\n"), "kind: ClusterIssuer")
}
