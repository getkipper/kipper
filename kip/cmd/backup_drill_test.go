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
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/controller/pkg/backupscope"
	"github.com/getkipper/kipper/kip/internal/installer"
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
	spec := drillRestoreSpec("daily-2", "shop-prod", "")

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

// restoreSelects reports whether objectLabels matches any of the spec's
// orLabelSelectors, as Velero does for directly selected resources.
func restoreSelects(t *testing.T, spec map[string]interface{}, objectLabels map[string]string) bool {
	t.Helper()
	selectors, ok := spec["orLabelSelectors"].([]interface{})
	require.True(t, ok, "the restore selects objects by orLabelSelectors")
	for _, s := range selectors {
		var sel metav1.LabelSelector
		require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(s.(map[string]interface{}), &sel))
		selector, err := metav1.LabelSelectorAsSelector(&sel)
		require.NoError(t, err)
		if selector.Matches(labels.Set(objectLabels)) {
			return true
		}
	}
	return false
}

// postgresServiceObjects returns representative labels on a Postgres service's
// resources, keyed by object description.
func postgresServiceObjects(service string) map[string]map[string]string {
	return map[string]map[string]string{
		"pod":                {"app": service, "app.kubernetes.io/managed-by": "kipper", "kipper.run/service-type": "postgres"},
		"credentials Secret": {"app": service, "app.kubernetes.io/managed-by": "kipper", "kipper.run/service-type": "postgres"},
		"TLS Secret":         {"app.kubernetes.io/managed-by": "kipper", "kipper.run/tls-for": service},
		"pg_hba ConfigMap":   {"app": service, "app.kubernetes.io/managed-by": "kipper", "kipper.run/tls-for": service},
		"data PVC":           {"app": service},
		"dump PVC":           {"app": service, "app.kubernetes.io/managed-by": "kipper", "kipper.run/dump-for": service},
	}
}

func TestADumpRestoreSelectsOnlyTheNamedServicesObjects(t *testing.T) {
	spec := drillRestoreSpec("daily-2", "shop-prod", "orders")

	assert.Equal(t, map[string]interface{}{"shop-prod": drillNamespace}, spec["namespaceMapping"])
	assert.NotContains(t, spec, "labelSelector", "Velero accepts either labelSelector or orLabelSelectors, but rejects requests containing both.")
	own := postgresServiceObjects("orders")
	for _, object := range []string{"pod", "credentials Secret", "TLS Secret", "pg_hba ConfigMap"} {
		assert.True(t, restoreSelects(t, spec, own[object]), "the restored pod needs the service's %s to start", object)
	}
	for object, objectLabels := range postgresServiceObjects("billing") {
		assert.False(t, restoreSelects(t, spec, objectLabels), "another service's %s stays out of a dump restore", object)
	}
	assert.False(t, restoreSelects(t, spec, map[string]string{"app": "orders"}),
		"an app pod that shares the service's name stays out")
}

func TestADrillStillSelectsEveryService(t *testing.T) {
	spec := drillRestoreSpec("daily-2", "shop-prod", "")

	for _, service := range []string{"orders", "billing"} {
		objects := postgresServiceObjects(service)
		for _, object := range []string{"pod", "credentials Secret", "TLS Secret", "pg_hba ConfigMap"} {
			assert.True(t, restoreSelects(t, spec, objects[object]), "the drill restores %s's %s", service, object)
		}
	}
}

func TestADumpRestoreSubmitsARestoreLimitedToTheService(t *testing.T) {
	clientset := kubefake.NewClientset()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{restoreGVR: "RestoreList"})
	var submitted *unstructured.Unstructured
	dyn.PrependReactor("create", "restores", func(action k8stesting.Action) (bool, runtime.Object, error) {
		submitted = action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
		return true, nil, errors.New("stop after the submission")
	})
	backup := &unstructured.Unstructured{}
	backup.SetName("daily-2")
	var restoreName string

	_, err := restoreIntoDrill(context.Background(), clientset, dyn, backup, "shop-prod", "orders", &restoreName)

	require.Error(t, err)
	require.NotNil(t, submitted)
	spec, _, _ := unstructured.NestedMap(submitted.Object, "spec")
	assert.True(t, restoreSelects(t, spec, postgresServiceObjects("orders")["pod"]))
	assert.False(t, restoreSelects(t, spec, postgresServiceObjects("billing")["pod"]),
		"another service's pod would bring its data and dump volumes into the restore")
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

func TestAFailedDrillRestoreSaysWhatWentWrong(t *testing.T) {
	r := &unstructured.Unstructured{Object: map[string]interface{}{"status": map[string]interface{}{
		"phase": "PartiallyFailed", "errors": int64(2), "warnings": int64(1)}}}
	assert.Equal(t, " (2 errors, 1 warning)", restoreOutcome(r))
	r.Object["status"] = map[string]interface{}{"phase": "Failed", "failureReason": "found a restore with status InProgress during the server starting"}
	assert.Equal(t, " (found a restore with status InProgress during the server starting)", restoreOutcome(r))
	r.Object["status"] = map[string]interface{}{"phase": "PartiallyFailed"}
	assert.Empty(t, restoreOutcome(r))
}

func unschedulablePod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "db-0", Namespace: drillNamespace, UID: "pod-1"},
		Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
			Message: "0/1 nodes are available: 1 Insufficient cpu", LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour)),
		}}},
	}
}

func TestADrillGivesUpOnAPodTheNodeCannotFit(t *testing.T) {
	grace := drillUnschedulableGrace
	drillUnschedulableGrace = 50 * time.Millisecond
	t.Cleanup(func() { drillUnschedulableGrace = grace })
	clientset := kubefake.NewClientset(unschedulablePod())
	check := unschedulableWatch(clientset, "drill-x")
	ctx := context.Background()

	assert.NoError(t, check(ctx), "an old condition timestamp is not how long kip has seen the pod waiting")
	time.Sleep(60 * time.Millisecond)
	err := check(ctx)
	assert.ErrorContains(t, err, "Insufficient cpu")
	assert.ErrorContains(t, err, "kip stopped waiting for restore drill-x. The restore is still running")
	assert.ErrorContains(t, err, "kip backup restore --cancel drill-x", "A stuck restore blocks all later restores, so the message gives the command to end it.")
}

func TestAPodThatGetsScheduledResetsTheDrillsPatience(t *testing.T) {
	grace := drillUnschedulableGrace
	drillUnschedulableGrace = 50 * time.Millisecond
	t.Cleanup(func() { drillUnschedulableGrace = grace })
	pod := unschedulablePod()
	clientset := kubefake.NewClientset(pod)
	check := unschedulableWatch(clientset, "drill-x")
	ctx := context.Background()

	require.NoError(t, check(ctx))
	pod.Status.Conditions[0].Status = corev1.ConditionTrue
	_, err := clientset.CoreV1().Pods(drillNamespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
	require.NoError(t, err)
	time.Sleep(60 * time.Millisecond)
	require.NoError(t, check(ctx))
	pod.Status.Conditions[0].Status = corev1.ConditionFalse
	_, err = clientset.CoreV1().Pods(drillNamespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
	require.NoError(t, err)
	assert.NoError(t, check(ctx), "the wait starts again after the pod was schedulable")
}

func TestADrillIsRefusedOnTheNanoProfile(t *testing.T) {
	err := drillProfileAllowed(context.Background(), platformClient(installer.ProfileNano))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nano")
	assert.Contains(t, err.Error(), "kip service restore-dump")

	for _, profile := range []string{installer.ProfileSmall, installer.ProfileMedium, installer.ProfileLarge, installer.ProfileXLarge} {
		assert.NoError(t, drillProfileAllowed(context.Background(), platformClient(profile)), profile)
	}
}

func TestADrillIsAllowedOnAClusterWithoutPlatformConfig(t *testing.T) {
	assert.NoError(t, drillProfileAllowed(context.Background(), platformClient("")))
}

func TestADrillStopsWhenThePlatformConfigCannotBeRead(t *testing.T) {
	refusing := platformClient(installer.ProfileSmall)
	refusing.PrependReactor("get", "platformconfigs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("the apiserver is busy")
	})
	assert.Error(t, drillProfileAllowed(context.Background(), refusing))
}
