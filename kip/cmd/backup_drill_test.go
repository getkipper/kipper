package cmd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/controller/pkg/backupscope"
)

func backupObject(name, phase, completed string, included, excluded []interface{}) unstructured.Unstructured {
	spec := map[string]interface{}{}
	if included != nil {
		spec["includedNamespaces"] = included
	}
	if excluded != nil {
		spec["excludedNamespaces"] = excluded
	}
	return unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": name},
		"spec":     spec,
		"status":   map[string]interface{}{"phase": phase, "completionTimestamp": completed},
	}}
}

func TestLatestBackupForPicksTheNewestCompletedBackupCoveringTheNamespace(t *testing.T) {
	backups := []unstructured.Unstructured{
		backupObject("daily-1", "Completed", "2026-10-16T03:05:00Z", nil, []interface{}{"velero"}),
		backupObject("daily-2", "Completed", "2026-10-17T03:05:00Z", nil, []interface{}{"velero"}),
		backupObject("daily-3", "PartiallyFailed", "2026-10-18T03:05:00Z", nil, []interface{}{"velero"}),
		backupObject("other-project", "Completed", "2026-10-18T09:00:00Z", []interface{}{"blog-prod"}, nil),
		backupObject("system", "Completed", "2026-10-18T04:05:00Z", []interface{}{"dex", "kipper-system"}, nil),
		backupObject("running", "InProgress", "", nil, nil),
	}

	got, err := latestBackupFor(backups, "shop-prod")
	require.NoError(t, err)
	assert.Equal(t, "daily-2", got.GetName(), "PartiallyFailed and other namespaces' backups do not count")

	got, err = latestBackupFor(backups, "blog-prod")
	require.NoError(t, err)
	assert.Equal(t, "other-project", got.GetName())

	_, err = latestBackupFor(backups, "velero")
	assert.Error(t, err, "no backup covers an excluded namespace")
}

func TestDrillRestoreSpecRestoresOnlyServicesIntoTheDrillNamespace(t *testing.T) {
	spec := drillRestoreSpec("daily-2", "shop-prod")

	assert.Equal(t, "daily-2", spec["backupName"])
	assert.Equal(t, []interface{}{"shop-prod"}, spec["includedNamespaces"])
	assert.Equal(t, map[string]interface{}{"shop-prod": drillNamespace}, spec["namespaceMapping"])
	assert.ElementsMatch(t, []interface{}{"pods", "persistentvolumeclaims", "persistentvolumes", "secrets", "configmaps"}, spec["includedResources"],
		"Deployments, ReplicaSets, Jobs, CronJobs, Ingresses and StatefulSets are excluded. Without PVs, Velero skips the step "+
			"that frees each PVC for a fresh volume and the PVC stays bound to a volume that does not exist")
	assert.Equal(t, []interface{}{
		map[string]interface{}{"matchExpressions": []interface{}{map[string]interface{}{"key": "kipper.run/service-type", "operator": "Exists"}}},
		map[string]interface{}{"matchExpressions": []interface{}{map[string]interface{}{"key": "kipper.run/tls-for", "operator": "Exists"}}},
	}, spec["orLabelSelectors"], "Restore only service pods, their credentials and their TLS objects. Exclude app pods.")
	assert.NotContains(t, spec, "labelSelector", "Velero accepts either labelSelector or orLabelSelectors, but rejects requests containing both.")
	assert.NotContains(t, spec, "includeClusterResources", "the PV behind each restored PVC still gets provisioned")
	assert.Contains(t, spec["excludedResources"], "projects.kipper.run")
}

func TestDrillNetworkPolicyDeniesEverythingButDNSAndTheNamespace(t *testing.T) {
	np := drillNetworkPolicy()

	assert.Equal(t, drillNamespace, np.Namespace)
	assert.Empty(t, np.Spec.PodSelector.MatchLabels, "applies to every pod in the namespace")
	assert.ElementsMatch(t, []string{"Ingress", "Egress"}, []string{string(np.Spec.PolicyTypes[0]), string(np.Spec.PolicyTypes[1])})
	require.Len(t, np.Spec.Ingress, 1)
	require.Len(t, np.Spec.Ingress[0].From, 1)
	assert.NotNil(t, np.Spec.Ingress[0].From[0].PodSelector, "ingress only from the drill's own pods")
	require.Len(t, np.Spec.Egress, 2)
	assert.NotNil(t, np.Spec.Egress[0].To[0].PodSelector, "egress to the drill's own pods")
	assert.Equal(t, int32(53), np.Spec.Egress[1].Ports[0].Port.IntVal, "and to DNS")
	require.Len(t, np.Spec.Egress[1].To, 1, "port 53 only to the cluster's DNS, not to any address")
	dns := np.Spec.Egress[1].To[0]
	assert.Equal(t, map[string]string{"kubernetes.io/metadata.name": "kube-system"}, dns.NamespaceSelector.MatchLabels)
	assert.Equal(t, map[string]string{"k8s-app": "kube-dns"}, dns.PodSelector.MatchLabels)
}

func TestDrillNamespaceStaysOutOfBackups(t *testing.T) {
	assert.Contains(t, backupscope.ExcludedNamespaces(), drillNamespace, "a kept drill never ends up in a nightly backup")
}

func TestParseRowCounts(t *testing.T) {
	counts, err := parseRowCounts("app.public.users|52\nshop.public.or|ders|0\n\n")
	require.NoError(t, err)
	assert.Equal(t, []tableCount{{Table: "app.public.users", Rows: 52}, {Table: "shop.public.or|ders", Rows: 0}}, counts,
		"the count follows the last separator, so a | in a name survives")

	_, err = parseRowCounts("ERROR:  relation does not exist\n")
	assert.Error(t, err)
}

func TestRowCountCommandCountsEveryRowWithoutStatistics(t *testing.T) {
	cmd, script, ok := rowCountCommand("postgres")
	require.True(t, ok)
	assert.Contains(t, cmd[len(cmd)-1], "pg_database", "every database is counted, not only the service's default one")
	assert.Contains(t, cmd[len(cmd)-1], "|| exit", "a failed enumeration fails the check")
	assert.Contains(t, script, "count(*)")
	assert.Contains(t, script, "current_database()")
	assert.NotContains(t, script, "n_live_tup", "statistics read 0 after crash recovery")

	cmd, script, ok = rowCountCommand("mysql")
	require.True(t, ok)
	assert.Contains(t, cmd[len(cmd)-1], "MYSQL_PWD", "the password stays out of the mysql command line")
	assert.Contains(t, cmd[len(cmd)-1], "|| exit", "a failed enumeration fails the check")
	assert.Contains(t, script, "COUNT(*)")
	assert.Contains(t, script, "QUOTE(", "names become escaped string literals")
	assert.Contains(t, script, "REPLACE(table_name, '`', '``')", "and escaped identifiers")

	_, _, ok = rowCountCommand("redis")
	assert.False(t, ok)
}

func TestMappedRestoreLeavesIngressesOutByDefault(t *testing.T) {
	spec, err := restoreSpec("nightly", "", "", "shop:shop-restored", false, false)
	require.NoError(t, err)
	assert.Contains(t, spec["excludedResources"], "ingresses.networking.k8s.io", "a duplicate hostname would confuse Traefik for the live app")
	assert.Contains(t, spec["excludedResources"], "certificates.cert-manager.io", "Excluding Certificates prevents the copy from renewing certificates for the live hostnames.")

	spec, err = restoreSpec("nightly", "", "", "shop:shop-restored", false, true)
	require.NoError(t, err)
	assert.NotContains(t, spec["excludedResources"], "ingresses.networking.k8s.io")
	assert.NotContains(t, spec["excludedResources"], "certificates.cert-manager.io")

	spec, err = restoreSpec("nightly", "shop", "", "", false, false)
	require.NoError(t, err)
	assert.NotContains(t, spec["excludedResources"], "ingresses.networking.k8s.io", "an in-place restore keeps its Ingresses")
	assert.NotContains(t, spec["excludedResources"], "certificates.cert-manager.io")
}

func TestFinishDrillKeepsTheNamespaceWhileTheRestoreStateIsUnknown(t *testing.T) {
	clientset := kubefake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: drillNamespace}})
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{restoreGVR: "RestoreList"})
	dyn.PrependReactor("get", "restores", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the server is currently unable to handle the request")
	})

	err := finishDrill(context.Background(), clientset, dyn, "drill-shop-1", "shop")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--clean")
	_, getErr := clientset.CoreV1().Namespaces().Get(context.Background(), drillNamespace, metav1.GetOptions{})
	assert.NoError(t, getErr, "the namespace stays while the restore may still be writing to it")
}

func TestFinishDrillDeletesTheNamespaceOnceTheRestoreIsGone(t *testing.T) {
	clientset := kubefake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: drillNamespace}})
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{restoreGVR: "RestoreList"})

	require.NoError(t, finishDrill(context.Background(), clientset, dyn, "drill-shop-1", "shop"))
	_, getErr := clientset.CoreV1().Namespaces().Get(context.Background(), drillNamespace, metav1.GetOptions{})
	assert.Error(t, getErr)
}

func TestUnfinishedRestoresNamesWhatADrillWouldWaitBehind(t *testing.T) {
	restore := func(name, phase string) unstructured.Unstructured {
		r := unstructured.Unstructured{Object: map[string]interface{}{}}
		r.SetName(name)
		if phase != "" {
			_ = unstructured.SetNestedField(r.Object, phase, "status", "phase")
		}
		return r
	}
	got := unfinishedRestores([]unstructured.Unstructured{
		restore("done", "Completed"), restore("stuck", "InProgress"), restore("queued", ""), restore("broken", "PartiallyFailed"),
	})
	assert.Equal(t, []string{"stuck", "queued"}, got)
}

func TestCleanWaitsUntilTheNamespaceIsGone(t *testing.T) {
	clientset := kubefake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: drillNamespace}})
	require.NoError(t, deleteDrillNamespace(context.Background(), clientset))

	assert.NoError(t, waitForDrillNamespaceGone(context.Background(), clientset, time.Second))
}

func TestCleanKeepsWaitingWhileTheNamespaceTerminates(t *testing.T) {
	clientset := kubefake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: drillNamespace}})
	gets := 0
	clientset.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		gets++
		if gets < 2 {
			return true, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: drillNamespace}}, nil
		}
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, drillNamespace)
	})

	require.NoError(t, waitForDrillNamespaceGone(context.Background(), clientset, time.Minute))
	assert.Equal(t, 2, gets, "a terminating namespace is polled until it is gone")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stuck := kubefake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: drillNamespace}})
	assert.ErrorIs(t, waitForDrillNamespaceGone(ctx, stuck, time.Minute), context.Canceled)
}
