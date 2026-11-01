package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/controller/pkg/backupscope"
	"github.com/getkipper/kipper/controller/pkg/dexsecrets"
	"github.com/getkipper/kipper/controller/pkg/migrate"
	"github.com/getkipper/kipper/controller/pkg/usergrants"
)

// accountImport is what importAccountsFromBackup added to the live cluster.
type accountImport struct {
	dex   migrate.DexUserImportReport
	roles []string
}

// importAccountsFromBackup copies the dex ConfigMaps and Secrets and the
// kipper-system ConfigMaps from backupName into the drill namespace, then adds
// the console roles, accounts and connectors found there to the live cluster as
// migrate.ImportUserRoles and migrate.ImportDexBackup describe. The drill
// namespace is scheduled for deletion afterward unless a restore may still be
// writing to it. Added roles remain if the subsequent Dex import fails.
// It refuses to start while the drill namespace exists.
func importAccountsFromBackup(ctx context.Context, clientset kubernetes.Interface, dyn dynamic.Interface, backupName string) (accountImport, error) {
	var result accountImport
	backup, err := chooseAccountBackup(ctx, dyn, backupName)
	if err != nil {
		return result, err
	}
	if err := refuseLeftoverDrill(ctx, clientset, migrate.DexNamespace); err != nil {
		return result, err
	}
	if err := createDrillNamespace(ctx, clientset); err != nil {
		return result, err
	}
	if restores, err := dyn.Resource(restoreGVR).Namespace("velero").List(ctx, metav1.ListOptions{}); err == nil {
		for _, name := range unfinishedRestores(restores.Items) {
			fmt.Printf("  ·  Restore %s is unfinished. Velero processes restore requests one at a time, so the import may wait for it\n", name)
		}
	}
	var restoreName string
	defer func() {
		if err := finishDrill(context.Background(), clientset, dyn, restoreName, migrate.DexNamespace); err != nil {
			fmt.Printf("  ⚠  %v\n", err)
		}
	}()
	if err := restoreForImport(ctx, dyn, backup.GetName(), migrate.DexNamespace, &restoreName, "configmaps", "secrets"); err != nil {
		return result, err
	}
	if err := restoreForImport(ctx, dyn, backup.GetName(), usergrants.Namespace, &restoreName, "configmaps"); err != nil {
		return result, err
	}

	copies := clientset.CoreV1()
	dexConfig, err := copies.ConfigMaps(drillNamespace).Get(ctx, migrate.DexConfigMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return result, fmt.Errorf("backup %s contains no %s/%s", backup.GetName(), migrate.DexNamespace, migrate.DexConfigMapName)
	}
	if err != nil {
		return result, fmt.Errorf("reading the restored %s: %w", migrate.DexConfigMapName, err)
	}
	configYAML, ok := dexConfig.Data["config.yaml"]
	if !ok {
		return result, fmt.Errorf("the %s in backup %s has no config.yaml", migrate.DexConfigMapName, backup.GetName())
	}
	var connectorSecrets map[string][]byte
	secret, err := copies.Secrets(drillNamespace).Get(ctx, dexsecrets.SecretName, metav1.GetOptions{})
	switch {
	case err == nil:
		connectorSecrets = secret.Data
	case !apierrors.IsNotFound(err):
		return result, fmt.Errorf("reading the restored %s: %w", dexsecrets.SecretName, err)
	}
	users, err := copies.ConfigMaps(drillNamespace).Get(ctx, usergrants.ConfigMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return result, fmt.Errorf("backup %s contains no %s/%s", backup.GetName(), usergrants.Namespace, usergrants.ConfigMapName)
	}
	if err != nil {
		return result, fmt.Errorf("reading the restored %s: %w", usergrants.ConfigMapName, err)
	}
	if result.roles, err = migrate.ImportUserRoles(ctx, clientset, users.Data); err != nil {
		return result, err
	}

	result.dex, err = migrate.ImportDexBackup(ctx, clientset, []byte(configYAML), connectorSecrets)
	return result, err
}

// chooseAccountBackup returns the Completed backup named name if it covers the
// dex and kipper-system namespaces. If namespace coverage is insufficient, the
// error names the latest completed backup covering both, when one can be found.
func chooseAccountBackup(ctx context.Context, dyn dynamic.Interface, name string) (*unstructured.Unstructured, error) {
	backups := dyn.Resource(backupGVR).Namespace("velero")
	b, err := backups.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading backup %s: %w", name, err)
	}
	if phase, _, _ := unstructured.NestedString(b.Object, "status", "phase"); phase != "Completed" {
		return nil, fmt.Errorf("backup %s is %s. An import requires a Completed backup", name, phase)
	}
	if backupCoversAll(b, migrate.DexNamespace, usergrants.Namespace) {
		return b, nil
	}
	msg := fmt.Sprintf("backup %s does not cover namespaces %s and %s", name, migrate.DexNamespace, usergrants.Namespace)
	if list, err := backups.List(ctx, metav1.ListOptions{}); err == nil {
		if latest, err := latestBackupFor(list.Items, migrate.DexNamespace, usergrants.Namespace); err == nil {
			return nil, fmt.Errorf("%s. The latest completed backup that does is %s", msg, latest.GetName())
		}
	}
	return nil, errors.New(msg)
}

// restoreForImport restores resources of the source namespace from backupName
// into the drill namespace and waits up to an hour for the restore to finish.
// It sets *restoreName before submitting, so cleanup can check the restore even
// when the submission's outcome is unknown. A restore that finishes in any
// phase but Completed returns an error.
func restoreForImport(ctx context.Context, dyn dynamic.Interface, backupName, source string, restoreName *string, resources ...string) error {
	name := "drill-accounts-" + source + "-" + time.Now().Format("20060102-150405")
	spec := map[string]interface{}{
		"backupName":         backupName,
		"includedNamespaces": []interface{}{source},
		"namespaceMapping":   map[string]interface{}{source: drillNamespace},
		"includedResources":  toAnySlice(resources),
	}
	backupscope.Restore(spec, false)
	restore := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1",
		"kind":       "Restore",
		"metadata":   map[string]interface{}{"name": name, "namespace": "velero"},
		"spec":       spec,
	}}
	*restoreName = name
	if _, err := dyn.Resource(restoreGVR).Namespace("velero").Create(ctx, restore, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("creating restore: %w", err)
	}
	fmt.Printf("  ·  Restore %s started\n", name)
	phase, err := waitForRestorePhase(ctx, dyn, name, time.Hour, nil)
	if err != nil {
		return err
	}
	if phase != "Completed" {
		outcome := ""
		if r, err := dyn.Resource(restoreGVR).Namespace("velero").Get(ctx, name, metav1.GetOptions{}); err == nil {
			outcome = restoreOutcome(r)
		}
		return fmt.Errorf("restore %s ended with phase %s%s", name, phase, outcome)
	}
	return nil
}
