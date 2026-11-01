package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/getkipper/kipper/controller/pkg/dexsecrets"
	"github.com/getkipper/kipper/controller/pkg/migrate"
	"github.com/getkipper/kipper/controller/pkg/usergrants"
)

const importedKey = "KIPPER_GITHUB_CLIENT_SECRET_ABCDEFGHIJKL"

const liveAccounts = `issuer: https://dex.example.com/dex
staticPasswords:
  - email: admin@example.com
    hash: new-cluster-hash
connectors: []
`

const backedUpAccounts = `issuer: https://dex.example.com/dex
staticPasswords:
  - email: admin@example.com
    hash: old-cluster-hash
  - email: dev@example.com
    hash: dev-hash
connectors:
  - id: github
    type: github
    config:
      clientID: gh-client
      clientSecret: $` + importedKey + `
`

func accountsBackup(name string, excluded ...interface{}) *unstructured.Unstructured {
	b := backupObject(name, "Completed", "2026-11-01T03:00:00Z", nil, excluded)
	b.SetNamespace("velero")
	b.SetAPIVersion("velero.io/v1")
	b.SetKind("Backup")
	return &b
}

func liveUsers(users string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: usergrants.ConfigMapName, Namespace: usergrants.Namespace},
		Data:       map[string]string{usergrants.UsersKey: users},
	}
}

func liveDex() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "dex", Namespace: migrate.DexNamespace},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "dex"}},
		}}},
	}
}

// accountImportCluster returns fakes of a fresh cluster holding backups. Each
// submitted Restore finishes with phase and, when Completed, copies what
// backedUp holds for its namespace into the drill namespace, as Velero would.
func accountImportCluster(phase string, backedUp map[string][]runtime.Object, backups ...*unstructured.Unstructured) (*kubefake.Clientset, *dynamicfake.FakeDynamicClient, *[]*unstructured.Unstructured) {
	clientset := kubefake.NewClientset(migrate.DexConfigMap(liveAccounts), liveDex(), liveUsers(`{"admin@example.com":"admin"}`))
	objects := []runtime.Object{}
	for _, b := range backups {
		objects = append(objects, b)
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		restoreGVR: "RestoreList",
		backupGVR:  "BackupList",
	}, objects...)
	var submitted []*unstructured.Unstructured
	dyn.PrependReactor("create", "restores", func(action k8stesting.Action) (bool, runtime.Object, error) {
		restore := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
		submitted = append(submitted, restore.DeepCopy())
		_ = unstructured.SetNestedField(restore.Object, phase, "status", "phase")
		if phase == "Completed" {
			namespaces, _, _ := unstructured.NestedStringSlice(restore.Object, "spec", "includedNamespaces")
			for _, obj := range backedUp[namespaces[0]] {
				copied := obj.DeepCopyObject()
				copied.(metav1.Object).SetNamespace(drillNamespace)
				if err := clientset.Tracker().Add(copied); err != nil {
					return true, nil, err
				}
			}
		}
		return false, nil, nil
	})
	return clientset, dyn, &submitted
}

func backedUpCluster() map[string][]runtime.Object {
	return map[string][]runtime.Object{
		migrate.DexNamespace: {
			migrate.DexConfigMap(backedUpAccounts),
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: dexsecrets.SecretName, Namespace: migrate.DexNamespace},
				Data:       map[string][]byte{importedKey: []byte("gh-value")},
			},
		},
		usergrants.Namespace: {
			liveUsers(`{"admin@example.com":"viewer","dev@example.com":"deployer"}`),
		},
	}
}

func TestAccountImportBringsBackAccountsRolesAndConnectors(t *testing.T) {
	clientset, dyn, submitted := accountImportCluster("Completed", backedUpCluster(), accountsBackup("daily-1"))
	ctx := context.Background()

	result, err := importAccountsFromBackup(ctx, clientset, dyn, "daily-1")
	require.NoError(t, err)

	assert.Equal(t, []string{"dev@example.com"}, result.dex.StaticPasswordsAdded)
	assert.Equal(t, []string{"github"}, result.dex.ConnectorsAdded)
	assert.Equal(t, []string{"dev@example.com"}, result.roles)

	cm, err := clientset.CoreV1().ConfigMaps(migrate.DexNamespace).Get(ctx, migrate.DexConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Contains(t, cm.Data["config.yaml"], "new-cluster-hash", "the new cluster's admin keeps its password")
	assert.Contains(t, cm.Data["config.yaml"], "dev-hash")
	secret, err := clientset.CoreV1().Secrets(dexsecrets.Namespace).Get(ctx, dexsecrets.SecretName, metav1.GetOptions{})
	require.NoError(t, err)
	var cfg struct {
		Connectors []struct {
			Config struct {
				ClientSecret string `json:"clientSecret"`
			} `json:"config"`
		} `json:"connectors"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(cm.Data["config.yaml"]), &cfg))
	require.Len(t, cfg.Connectors, 1)
	assert.Equal(t, "gh-value", string(secret.Data[strings.TrimPrefix(cfg.Connectors[0].Config.ClientSecret, "$")]))
	users, err := clientset.CoreV1().ConfigMaps(usergrants.Namespace).Get(ctx, usergrants.ConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.JSONEq(t, `{"admin@example.com":"admin","dev@example.com":"deployer"}`, users.Data[usergrants.UsersKey])

	_, err = clientset.CoreV1().Namespaces().Get(ctx, drillNamespace, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "the copies leave with the drill namespace")
	require.Len(t, *submitted, 2)
}

func TestAccountImportRestoresOnlyWhatItReads(t *testing.T) {
	clientset, dyn, submitted := accountImportCluster("Completed", backedUpCluster(), accountsBackup("daily-1"))

	_, err := importAccountsFromBackup(context.Background(), clientset, dyn, "daily-1")
	require.NoError(t, err)

	byNamespace := map[string]map[string]interface{}{}
	for _, r := range *submitted {
		spec, _, _ := unstructured.NestedMap(r.Object, "spec")
		namespaces, _, _ := unstructured.NestedStringSlice(spec, "includedNamespaces")
		require.Len(t, namespaces, 1)
		byNamespace[namespaces[0]] = spec
		assert.Equal(t, "daily-1", spec["backupName"])
		assert.Equal(t, map[string]interface{}{namespaces[0]: drillNamespace}, spec["namespaceMapping"])
		assert.Contains(t, r.GetName(), "drill-", "kip backup drill --clean finds a leftover restore by this prefix")
	}
	assert.Equal(t, []interface{}{"configmaps", "secrets"}, byNamespace[migrate.DexNamespace]["includedResources"])
	assert.Equal(t, []interface{}{"configmaps"}, byNamespace[usergrants.Namespace]["includedResources"],
		"the old cluster's system Secrets stay in the backup")
}

func TestAccountImportNamesABackupThatHoldsTheAccounts(t *testing.T) {
	clientset, dyn, submitted := accountImportCluster("Completed", backedUpCluster(),
		accountsBackup("apps-only", "dex", "kipper-system"), accountsBackup("daily-1"))

	_, err := importAccountsFromBackup(context.Background(), clientset, dyn, "apps-only")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "daily-1")
	assert.Empty(t, *submitted)
}

func TestAccountImportRefusesWhileTheDrillNamespaceIsInUse(t *testing.T) {
	clientset, dyn, submitted := accountImportCluster("Completed", backedUpCluster(), accountsBackup("daily-1"))
	_, err := clientset.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: drillNamespace}}, metav1.CreateOptions{})
	require.NoError(t, err)

	_, err = importAccountsFromBackup(context.Background(), clientset, dyn, "daily-1")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--clean")
	assert.Empty(t, *submitted)
	_, err = clientset.CoreV1().Namespaces().Get(context.Background(), drillNamespace, metav1.GetOptions{})
	assert.NoError(t, err, "someone else's drill stays")
}

func TestAccountImportChangesNothingWhenARestoreFails(t *testing.T) {
	clientset, dyn, _ := accountImportCluster("PartiallyFailed", backedUpCluster(), accountsBackup("daily-1"))
	ctx := context.Background()

	_, err := importAccountsFromBackup(ctx, clientset, dyn, "daily-1")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "PartiallyFailed")
	cm, err := clientset.CoreV1().ConfigMaps(migrate.DexNamespace).Get(ctx, migrate.DexConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, liveAccounts, cm.Data["config.yaml"])
}

func TestAccountImportWithoutAccountsInTheBackupSaysSo(t *testing.T) {
	clientset, dyn, _ := accountImportCluster("Completed", map[string][]runtime.Object{}, accountsBackup("daily-1"))

	_, err := importAccountsFromBackup(context.Background(), clientset, dyn, "daily-1")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "dex-config")
}

func TestAccountImportWithoutConnectorSecretsSkipsTheConnectors(t *testing.T) {
	backedUp := backedUpCluster()
	backedUp[migrate.DexNamespace] = backedUp[migrate.DexNamespace][:1]
	clientset, dyn, _ := accountImportCluster("Completed", backedUp, accountsBackup("daily-1"))

	result, err := importAccountsFromBackup(context.Background(), clientset, dyn, "daily-1")

	require.NoError(t, err)
	assert.Equal(t, []string{"github"}, result.dex.ConnectorsSkipped)
	assert.Equal(t, []string{"dev@example.com"}, result.dex.StaticPasswordsAdded)
}

func TestAccountImportKeepsTheDrillNamespaceWhileARestoreMayStillWrite(t *testing.T) {
	clientset, dyn, _ := accountImportCluster("Completed", backedUpCluster(), accountsBackup("daily-1"))
	dyn.PrependReactor("get", "restores", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the server is currently unable to handle the request")
	})

	_, err := importAccountsFromBackup(context.Background(), clientset, dyn, "daily-1")

	require.Error(t, err)
	_, err = clientset.CoreV1().Namespaces().Get(context.Background(), drillNamespace, metav1.GetOptions{})
	assert.NoError(t, err)
}

func TestAnInterruptedAccountImportKeepsTheDrillNamespaceForTheRunningRestore(t *testing.T) {
	clientset, dyn, _ := accountImportCluster("InProgress", backedUpCluster(), accountsBackup("daily-1"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := importAccountsFromBackup(ctx, clientset, dyn, "daily-1")

	require.ErrorIs(t, err, context.Canceled)
	_, err = clientset.CoreV1().Namespaces().Get(context.Background(), drillNamespace, metav1.GetOptions{})
	assert.NoError(t, err, "Velero may still write into it")
	cm, err := clientset.CoreV1().ConfigMaps(migrate.DexNamespace).Get(context.Background(), migrate.DexConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, liveAccounts, cm.Data["config.yaml"])
}

func TestUserImportTakesEitherAFileOrABackup(t *testing.T) {
	for name, args := range map[string][]string{"neither": nil, "both": {"snapshot.yaml"}} {
		t.Run(name, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.Flags().String("from-backup", "", "")
			cmd.Flags().Bool("restart-dex", false, "")
			if name == "both" {
				require.NoError(t, cmd.Flags().Set("from-backup", "daily-1"))
			}

			err := runUserImport(cmd, args)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "either a snapshot file or --from-backup")
		})
	}
}

func TestAccountImportWithoutRolesInTheBackupChangesNothing(t *testing.T) {
	backedUp := backedUpCluster()
	delete(backedUp, usergrants.Namespace)
	clientset, dyn, _ := accountImportCluster("Completed", backedUp, accountsBackup("daily-1"))
	ctx := context.Background()

	_, err := importAccountsFromBackup(ctx, clientset, dyn, "daily-1")

	require.Error(t, err)
	assert.Contains(t, err.Error(), usergrants.ConfigMapName)
	cm, err := clientset.CoreV1().ConfigMaps(migrate.DexNamespace).Get(ctx, migrate.DexConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, liveAccounts, cm.Data["config.yaml"])
}

func TestRestartingDexRetriesAWriteThatRacedTheConsole(t *testing.T) {
	clientset := kubefake.NewClientset(liveDex())
	conflicts := 1
	clientset.PrependReactor("update", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts == 0 {
			return false, nil, nil
		}
		conflicts--
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, "dex", errors.New("modified"))
	})

	require.NoError(t, rolloutRestartDex(context.Background(), clientset))

	deploy, err := clientset.AppsV1().Deployments(migrate.DexNamespace).Get(context.Background(), "dex", metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotEmpty(t, deploy.Spec.Template.Annotations["kipper.run/user-imported-at"])
}
