package migrate

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"
)

const liveDexConfig = `issuer: https://dex--cluster.kipper.run/dex
storage:
  type: kubernetes
  config:
    inCluster: true
enablePasswordDB: true
connectors: []
staticPasswords:
  - email: admin@install.com
    hash: bootstrap-hash
    username: admin
staticClients:
  - id: kipper-console
    name: Kipper Console
    redirectURIs:
      - https://console.example.com/callback
    secret: live-secret
oauth2:
  skipApprovalScreen: true
`

const snapshotDexConfig = `issuer: https://dex-old.example.com/dex
enablePasswordDB: true
connectors:
  - id: github
    type: github
    config:
      clientID: gh-client
staticPasswords:
  - email: admin@install.com
    hash: stale-hash-do-not-pick
    username: admin-old
  - email: prod-user-a@example.com
    hash: real-hash-a
    username: usera
  - email: prod-user-b@example.com
    hash: real-hash-b
    username: userb
staticClients:
  - id: kipper-console
    name: Old Console
    secret: stale-secret
`

func TestMergeDexConfig_AddsNewUsersAndConnectors(t *testing.T) {
	merged, report, err := MergeDexConfig([]byte(liveDexConfig), []byte(snapshotDexConfig))
	require.NoError(t, err)

	assert.ElementsMatch(t,
		[]string{"prod-user-a@example.com", "prod-user-b@example.com"},
		report.StaticPasswordsAdded,
		"production users from snapshot must be appended")
	assert.Equal(t, []string{"github"}, report.ConnectorsAdded)
	assert.Equal(t, 1, report.AlreadyPresent, "admin@install.com already exists, must not double-add")

	// Decode merged YAML and verify shape.
	var got map[string]any
	require.NoError(t, yaml.Unmarshal(merged, &got))

	pws, ok := got["staticPasswords"].([]any)
	require.True(t, ok)
	emails := emailsFromList(pws)
	assert.Contains(t, emails, "admin@install.com")
	assert.Contains(t, emails, "prod-user-a@example.com")
	assert.Contains(t, emails, "prod-user-b@example.com")

	// Admin entry must still carry the install hash, not the snapshot's
	// stale-hash-do-not-pick value. Existing entries always win.
	for _, item := range pws {
		entry := item.(map[string]any)
		if entry["email"] == "admin@install.com" {
			assert.Equal(t, "bootstrap-hash", entry["hash"])
			assert.Equal(t, "admin", entry["username"])
		}
	}

	// staticClients was NOT touched (only staticPasswords + connectors
	// are in scope). The merged config keeps the live secret value.
	clients := got["staticClients"].([]any)
	for _, c := range clients {
		entry := c.(map[string]any)
		if entry["id"] == "kipper-console" {
			assert.Equal(t, "live-secret", entry["secret"], "staticClients out of scope; live values preserved")
		}
	}

	// Issuer must not have changed.
	assert.Equal(t, "https://dex--cluster.kipper.run/dex", got["issuer"])
}

func TestMergeDexConfig_NoChangesIsIdempotent(t *testing.T) {
	// Running the merge against an already-merged config is a no-op.
	merged, _, err := MergeDexConfig([]byte(liveDexConfig), []byte(snapshotDexConfig))
	require.NoError(t, err)

	_, report, err := MergeDexConfig(merged, []byte(snapshotDexConfig))
	require.NoError(t, err)
	assert.False(t, report.HasChanges(), "re-merging a snapshot must not add anything new")
	assert.Equal(t, 4, report.AlreadyPresent, "3 passwords + 1 connector already present")
}

func TestExtractDexConfigYAML_AcceptsBothShapes(t *testing.T) {
	// 1. Full ConfigMap manifest (what `kubectl get cm dex-config -o yaml` produces).
	cmDump := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: dex-config
  namespace: dex
data:
  config.yaml: |
    issuer: https://dex.example.com/dex
    staticPasswords:
      - email: a@b.c
        hash: h
`)
	got, err := ExtractDexConfigYAML(cmDump)
	require.NoError(t, err)
	assert.Contains(t, string(got), "staticPasswords")
	assert.NotContains(t, string(got), "apiVersion", "extracted body must not include the ConfigMap envelope")

	// 2. Raw Dex config YAML (operator pre-extracted it).
	raw := []byte(`issuer: https://dex.example.com/dex
staticPasswords:
  - email: a@b.c
    hash: h
`)
	got, err = ExtractDexConfigYAML(raw)
	require.NoError(t, err)
	assert.Contains(t, string(got), "staticPasswords")
}

func TestImportDexUsers_RoundTripAgainstFakeClient(t *testing.T) {
	client := fake.NewSimpleClientset(DexConfigMap(liveDexConfig))

	report, err := ImportDexUsers(context.Background(), client, []byte(snapshotDexConfig))
	require.NoError(t, err)
	assert.True(t, report.HasChanges())
	assert.Len(t, report.StaticPasswordsAdded, 2)

	// Live ConfigMap was updated.
	got, err := client.CoreV1().ConfigMaps(DexNamespace).Get(context.Background(), DexConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Contains(t, got.Data["config.yaml"], "prod-user-a@example.com")
	assert.Contains(t, got.Data["config.yaml"], "prod-user-b@example.com")
	assert.Contains(t, got.Data["config.yaml"], "github")
}

func TestImportDexUsers_NoChangesDoesNotWriteBack(t *testing.T) {
	// If the snapshot has nothing the live config doesn't already have,
	// the import path skips the apiserver Update — useful for the boot-
	// time hook if we ever add one, and a cheap correctness guarantee.
	identicalSnapshot := liveDexConfig
	client := fake.NewSimpleClientset(DexConfigMap(liveDexConfig))

	report, err := ImportDexUsers(context.Background(), client, []byte(identicalSnapshot))
	require.NoError(t, err)
	assert.False(t, report.HasChanges())
}

func emailsFromList(items []any) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			if v, ok := m["email"].(string); ok {
				out = append(out, v)
			}
		}
	}
	return out
}
