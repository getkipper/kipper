package migrate

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/getkipper/kipper/controller/pkg/dexsecrets"
	"github.com/getkipper/kipper/controller/pkg/usergrants"
)

const backupKey = "KIPPER_GITHUB_CLIENT_SECRET_ABCDEFGHIJKL"

const backupDexConfig = `connectors:
  - id: github
    type: github
    config:
      clientID: gh-client
      clientSecret: $` + backupKey + `
  - id: google
    type: google
    config:
      clientID: g-client
      clientSecret: ${KIPPER_GOOGLE_CLIENT_SECRET_MNOPQRSTUVWX}
  - id: corp
    type: oidc
    config:
      clientID: corp-client
      clientSecret: $CORP_SECRET
staticPasswords:
  - email: prod-user-a@example.com
    hash: real-hash-a
    username: usera
`

var backupSecrets = map[string][]byte{
	backupKey: []byte("gh-value"),
	"KIPPER_GOOGLE_CLIENT_SECRET_MNOPQRSTUVWX": []byte("google-value"),
}

func connectorIDs(t *testing.T, merged []byte) []string {
	t.Helper()
	var cfg map[string]any
	require.NoError(t, yaml.Unmarshal(merged, &cfg))
	var ids []string
	conns, _ := cfg["connectors"].([]any)
	for _, c := range conns {
		m, _ := c.(map[string]any)
		id, _ := m["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

// secretRef returns the key connector id's clientSecret refers to in a config.
func secretRef(t *testing.T, configYAML []byte, id string) string {
	t.Helper()
	var cfg map[string]any
	require.NoError(t, yaml.Unmarshal(configYAML, &cfg))
	conns, _ := cfg["connectors"].([]any)
	for _, c := range conns {
		m, _ := c.(map[string]any)
		if m["id"] == id {
			ref, _ := m["config"].(map[string]any)["clientSecret"].(string)
			return referencedVariable(ref)
		}
	}
	t.Fatalf("no connector %s", id)
	return ""
}

func TestMergeDexBackup_BringsConnectorsWithTheSecretsTheyReference(t *testing.T) {
	merged, report, values, err := MergeDexBackup([]byte(liveDexConfig), []byte(backupDexConfig), backupSecrets)
	require.NoError(t, err)

	assert.Equal(t, []string{"github", "google"}, report.ConnectorsAdded)
	assert.Equal(t, []string{"corp"}, report.ConnectorsSkipped, "the backup holds no value for $CORP_SECRET")
	assert.Equal(t, []string{"prod-user-a@example.com"}, report.StaticPasswordsAdded)
	githubKey, googleKey := secretRef(t, merged, "github"), secretRef(t, merged, "google")
	assert.Equal(t, map[string]string{githubKey: "gh-value", googleKey: "google-value"}, values)
	assert.NotEqual(t, backupKey, githubKey, "an imported connector gets a key of its own")
	assert.Regexp(t, `^KIPPER_GITHUB_CLIENT_SECRET_`, githubKey)
	assert.Equal(t, []string{"github", "google"}, connectorIDs(t, merged))
	assert.NotContains(t, string(merged), "gh-value", "secret values stay out of the config")
}

func TestMergeDexBackup_KeepsTheLiveConnectorAndStoresNoSecretForIt(t *testing.T) {
	live := `connectors:
  - id: github
    type: github
    config:
      clientID: live-client
      clientSecret: $KIPPER_GITHUB_CLIENT_SECRET_LIVELIVELIVE
`
	merged, report, values, err := MergeDexBackup([]byte(live), []byte(backupDexConfig), backupSecrets)
	require.NoError(t, err)

	assert.Equal(t, []string{"google"}, report.ConnectorsAdded)
	assert.Len(t, values, 1, "only google's secret is stored")
	assert.Contains(t, string(merged), "live-client")
	assert.NotContains(t, string(merged), "gh-client")
}

func TestMergeDexConfig_StillLeavesOutEveryReferencedSecret(t *testing.T) {
	_, report, err := MergeDexConfig([]byte(liveDexConfig), []byte(backupDexConfig))
	require.NoError(t, err)

	assert.Empty(t, report.ConnectorsAdded)
	assert.Equal(t, []string{"github", "google", "corp"}, report.ConnectorsSkipped)
}

func dexDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "dex", Namespace: DexNamespace},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "dex"}},
		}}},
	}
}

func TestImportDexBackup_StoresSecretsBeforeTheConfigUsesThem(t *testing.T) {
	client := fake.NewClientset(DexConfigMap(liveDexConfig), dexDeployment())
	var order []string
	client.PrependReactor("*", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetVerb() == "create" || a.GetVerb() == "update" {
			order = append(order, a.GetVerb()+" "+a.GetResource().Resource)
		}
		return false, nil, nil
	})

	report, err := ImportDexBackup(context.Background(), client, []byte(backupDexConfig), backupSecrets)
	require.NoError(t, err)

	assert.Equal(t, []string{"github", "google"}, report.ConnectorsAdded)
	assert.Equal(t, []string{"create secrets", "update deployments", "update configmaps"}, order)
	secret, err := client.CoreV1().Secrets(dexsecrets.Namespace).Get(context.Background(), dexsecrets.SecretName, metav1.GetOptions{})
	require.NoError(t, err)
	cm, err := client.CoreV1().ConfigMaps(DexNamespace).Get(context.Background(), DexConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "gh-value", string(secret.Data[secretRef(t, []byte(cm.Data["config.yaml"]), "github")]))
}

func TestImportDexBackup_WithoutSecretsToStoreLeavesTheSecretAndDexAlone(t *testing.T) {
	client := fake.NewClientset(DexConfigMap(liveDexConfig))

	report, err := ImportDexBackup(context.Background(), client, []byte(snapshotDexConfig), nil)
	require.NoError(t, err)

	assert.Equal(t, []string{"github"}, report.ConnectorsAdded)
	_, err = client.CoreV1().Secrets(dexsecrets.Namespace).Get(context.Background(), dexsecrets.SecretName, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
}

func TestImportDexBackup_LeavesTheConfigWhenStoringFails(t *testing.T) {
	client := fake.NewClientset(DexConfigMap(liveDexConfig), dexDeployment())
	client.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})

	_, err := ImportDexBackup(context.Background(), client, []byte(backupDexConfig), backupSecrets)
	require.Error(t, err)

	cm, err := client.CoreV1().ConfigMaps(DexNamespace).Get(context.Background(), DexConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, liveDexConfig, cm.Data["config.yaml"], "a config must never refer to a secret that was not stored")
}

func TestImportDexBackup_DoesNotOverwriteAConfigThatChangedMeanwhile(t *testing.T) {
	client := fake.NewClientset(DexConfigMap(liveDexConfig), dexDeployment())
	client.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, DexConfigMapName, errors.New("modified"))
	})

	_, err := ImportDexBackup(context.Background(), client, []byte(backupDexConfig), backupSecrets)

	require.Error(t, err)
	assert.True(t, apierrors.IsConflict(err))
}

func TestImportUserRoles_AddsTheBackupsRoles(t *testing.T) {
	live := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: usergrants.ConfigMapName, Namespace: usergrants.Namespace},
		Data:       map[string]string{usergrants.UsersKey: `{"admin@test.com":"admin"}`},
	}
	client := fake.NewClientset(live)

	added, err := ImportUserRoles(context.Background(), client, map[string]string{usergrants.UsersKey: `{"dev@test.com":"deployer"}`})
	require.NoError(t, err)

	assert.Equal(t, []string{"dev@test.com"}, added)
	cm, err := client.CoreV1().ConfigMaps(usergrants.Namespace).Get(context.Background(), usergrants.ConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.JSONEq(t, `{"admin@test.com":"admin","dev@test.com":"deployer"}`, cm.Data[usergrants.UsersKey])
}

func TestImportUserRoles_WithNothingNewDoesNotWrite(t *testing.T) {
	live := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: usergrants.ConfigMapName, Namespace: usergrants.Namespace},
		Data:       map[string]string{usergrants.UsersKey: `{"admin@test.com":"admin"}`},
	}
	client := fake.NewClientset(live)
	client.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("unexpected write")
	})

	added, err := ImportUserRoles(context.Background(), client, map[string]string{usergrants.UsersKey: `{"admin@test.com":"viewer"}`})

	require.NoError(t, err)
	assert.Empty(t, added)
}

func TestMergeDexBackup_PointsAddedConnectorsAtThisClustersCallback(t *testing.T) {
	backup := `issuer: https://dex.old.example.com/dex
connectors:
  - id: github
    type: github
    config:
      clientID: gh-client
      clientSecret: $` + backupKey + `
      redirectURI: https://dex.old.example.com/dex/callback
  - id: corp
    type: oidc
    config:
      clientID: corp-client
      clientSecret: plain
      redirectURI: https://sso.example.com/custom
`
	live := "issuer: https://dex.new.example.com/dex\nconnectors: []\n"

	merged, report, _, err := MergeDexBackup([]byte(live), []byte(backup), backupSecrets)
	require.NoError(t, err)

	assert.Contains(t, string(merged), "redirectURI: https://dex.new.example.com/dex/callback")
	assert.NotContains(t, string(merged), "dex.old.example.com")
	assert.Contains(t, string(merged), "https://sso.example.com/custom", "a callback the console did not derive stays")
	assert.Equal(t, "https://dex.new.example.com/dex/callback", report.ConnectorCallback)
}

func TestMergeDexBackup_OnTheSameIssuerReportsNoNewCallback(t *testing.T) {
	backup := "issuer: https://dex.example.com/dex\nconnectors:\n  - id: corp\n    type: oidc\n    config:\n      clientSecret: plain\n      redirectURI: https://dex.example.com/dex/callback\n"

	_, report, _, err := MergeDexBackup([]byte("issuer: https://dex.example.com/dex\n"), []byte(backup), nil)
	require.NoError(t, err)

	assert.Equal(t, []string{"corp"}, report.ConnectorsAdded)
	assert.Empty(t, report.ConnectorCallback)
}

func TestImportDexBackup_LeavesTheValueOfALiveKeyAlone(t *testing.T) {
	live := `connectors:
  - id: corp
    type: oidc
    config:
      clientSecret: $SHARED_SECRET
`
	backup := `connectors:
  - id: partner
    type: oidc
    config:
      clientSecret: $SHARED_SECRET
`
	liveSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: dexsecrets.SecretName, Namespace: dexsecrets.Namespace},
		Data:       map[string][]byte{"SHARED_SECRET": []byte("current")},
	}
	client := fake.NewClientset(DexConfigMap(live), dexDeployment(), liveSecret)

	report, err := ImportDexBackup(context.Background(), client, []byte(backup), map[string][]byte{"SHARED_SECRET": []byte("old")})
	require.NoError(t, err)

	assert.Equal(t, []string{"partner"}, report.ConnectorsAdded)
	secret, err := client.CoreV1().Secrets(dexsecrets.Namespace).Get(context.Background(), dexsecrets.SecretName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "current", string(secret.Data["SHARED_SECRET"]), "the live connector keeps its credential")
	cm, err := client.CoreV1().ConfigMaps(DexNamespace).Get(context.Background(), DexConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	var cfg map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(cm.Data["config.yaml"]), &cfg))
	partner := cfg["connectors"].([]any)[1].(map[string]any)["config"].(map[string]any)
	ref, _ := partner["clientSecret"].(string)
	require.NotEqual(t, "$SHARED_SECRET", ref)
	assert.Equal(t, "old", string(secret.Data[ref[1:]]), "the imported connector gets its own key with the backup's value")
}

func TestMergeDexBackup_SurvivesAConnectorListedTwice(t *testing.T) {
	backup := `connectors:
  - id: github
    type: github
  - id: github
    type: github
    config:
      clientSecret: $` + backupKey + `
`
	_, report, values, err := MergeDexBackup([]byte("connectors: []\n"), []byte(backup), backupSecrets)

	require.NoError(t, err)
	assert.Equal(t, []string{"github"}, report.ConnectorsAdded)
	assert.Empty(t, values, "the connector that was added refers to no secret")
}
