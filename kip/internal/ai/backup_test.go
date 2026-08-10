package ai

import (
	"context"
	"errors"
	"strings"
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
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// veleroDynamicFake builds a dynamic fake that knows about both the
// install-side resources and the Velero CRs we drive in this file. Each
// GVR the fake will see needs an entry in CustomListKinds, otherwise
// List calls fail.
func veleroDynamicFake() *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{
		namespaceGVR:              "NamespaceList",
		secretGVR:                 "SecretList",
		configMapGVR:              "ConfigMapList",
		ingressGVR:                "IngressList",
		helmChartGVR:              "HelmChartList",
		veleroBackupGVR:           "BackupList",
		veleroRestoreGVR:          "RestoreList",
		veleroDeleteRequestGVR:    "DeleteBackupRequestList",
		veleroBackupRepositoryGVR: "BackupRepositoryList",
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds)
}

// markCompletedOnGet rewrites every Get for a given GVR to inject a
// "Completed" terminal phase. Simulates Velero finishing the work that
// the fake apiserver never does on its own. The progress counter Velero
// writes differs by kind: Backup CRs use `itemsBackedUp`, Restore CRs
// use `itemsRestored`. Match Velero's contract so summariseBackup
// surfaces the same item count for both.
func markCompletedOnGet(client *dynamicfake.FakeDynamicClient, gvr schema.GroupVersionResource) {
	client.PrependReactor("get", gvr.Resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		obj, err := client.Tracker().Get(gvr, getAction.GetNamespace(), getAction.GetName())
		if err != nil {
			return true, nil, err
		}
		u := obj.(*unstructured.Unstructured).DeepCopy()
		_ = unstructured.SetNestedField(u.Object, "Completed", "status", "phase")
		counterField := "itemsBackedUp"
		if u.GetKind() == "Restore" {
			counterField = "itemsRestored"
		}
		_ = unstructured.SetNestedField(u.Object, int64(7), "status", "progress", counterField)
		return true, u, nil
	})
}

// deleteOnDeleteRequest installs a reactor that deletes the named
// Backup CR from the tracker as soon as a DeleteBackupRequest is
// created against it. Simulates the Velero controller's deletion flow.
func deleteOnDeleteRequest(client *dynamicfake.FakeDynamicClient) {
	client.PrependReactor("create", "deletebackuprequests", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ca := action.(k8stesting.CreateAction)
		req := ca.GetObject().(*unstructured.Unstructured)
		backupName, _, _ := unstructured.NestedString(req.Object, "spec", "backupName")
		if backupName != "" {
			_ = client.Tracker().Delete(veleroBackupGVR, VeleroNamespace, backupName)
		}
		return false, nil, nil
	})
}

// fastInstaller returns an Installer with tight polling so wait loops
// resolve in milliseconds, paired with a fake clientset that already
// has the seeded objects (if any).
func fastInstaller(client *dynamicfake.FakeDynamicClient, kubeObjs ...runtime.Object) *Installer {
	return &Installer{
		Clientset:    fake.NewSimpleClientset(kubeObjs...), //nolint:staticcheck
		Dynamic:      client,
		PollInterval: 5 * time.Millisecond,
		PollTimeout:  2 * time.Second,
	}
}

// installedNamespace returns a typed Namespace object for kipper-ai
// labelled as Kipper-owned, the precondition for `kip ai backup`.
// Callers seed it through fastInstaller's variadic kubeObjs.
func installedNamespace() *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   Namespace,
		Labels: map[string]string{managedByLabel: managedByValue},
	}}
}

func aiConfigSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      AIConfigSecretName,
			Namespace: SystemNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kipper",
				"app.kubernetes.io/part-of":    "kipper-ai",
			},
		},
		Data: map[string][]byte{"provider": []byte("ollama")},
	}
}

func TestCreateBackupCompletesWithSibling(t *testing.T) {
	client := veleroDynamicFake()
	markCompletedOnGet(client, veleroBackupGVR)
	inst := fastInstaller(client, installedNamespace(), aiConfigSecret())

	summary, err := inst.CreateBackup(context.Background(), "pre-upgrade")
	require.NoError(t, err)
	assert.Equal(t, "Completed", summary.Phase)
	assert.Equal(t, "pre-upgrade", summary.Name)
	// Both primary + sibling are 7 items each in the marker reactor.
	assert.Equal(t, int64(14), summary.ItemsBackedUp)

	// The sibling Backup must cover kipper-system (cluster-side AI
	// config Secret) and kube-system (HelmChart CRs that helm-controller
	// reconciles), filtered by both Kipper labels so unrelated
	// resources in those namespaces are excluded.
	sibling, err := client.Tracker().Get(veleroBackupGVR, VeleroNamespace, "pre-upgrade"+configBackupSuffix)
	require.NoError(t, err)
	u := sibling.(*unstructured.Unstructured)
	ns, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "includedNamespaces")
	assert.ElementsMatch(t, []string{SystemNamespace, "kube-system"}, ns)
	resources, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "includedResources")
	assert.Contains(t, resources, "secrets")
	assert.Contains(t, resources, "helmcharts.helm.cattle.io")
	sel, _, _ := unstructured.NestedStringMap(u.Object, "spec", "labelSelector", "matchLabels")
	assert.Equal(t, "kipper-ai", sel["app.kubernetes.io/part-of"])
	assert.Equal(t, "kipper", sel[managedByLabel])
}

func TestCreateBackupSkipsSiblingWhenNoCrossNsArtefacts(t *testing.T) {
	client := veleroDynamicFake()
	markCompletedOnGet(client, veleroBackupGVR)
	// No AI config Secret, no HelmCharts in kube-system → no sibling.
	inst := fastInstaller(client, installedNamespace())

	summary, err := inst.CreateBackup(context.Background(), "fresh")
	require.NoError(t, err)
	assert.Equal(t, "Completed", summary.Phase)

	_, err = client.Tracker().Get(veleroBackupGVR, VeleroNamespace, "fresh"+configBackupSuffix)
	require.True(t, apierrors.IsNotFound(err), "no sibling expected when nothing cross-namespace exists")
}

// TestCreateBackupSiblingCoversHelmChartsWithoutSecret: a user who
// declined install-time auto-config has no kipper-ai-config Secret,
// but the ollama/librechat HelmChart CRs in kube-system still need to
// be in the snapshot. The sibling must be created in this case too.
func TestCreateBackupSiblingCoversHelmChartsWithoutSecret(t *testing.T) {
	client := veleroDynamicFake()
	markCompletedOnGet(client, veleroBackupGVR)

	helmChart := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "helm.cattle.io/v1",
		"kind":       "HelmChart",
		"metadata": map[string]interface{}{
			"name":      "ollama",
			"namespace": "kube-system",
			"labels": map[string]interface{}{
				managedByLabel:              managedByValue,
				"app.kubernetes.io/part-of": "kipper-ai",
			},
		},
	}}
	require.NoError(t, client.Tracker().Create(helmChartGVR, helmChart, "kube-system"))

	inst := fastInstaller(client, installedNamespace()) // no Secret
	_, err := inst.CreateBackup(context.Background(), "no-secret")
	require.NoError(t, err)

	_, err = client.Tracker().Get(veleroBackupGVR, VeleroNamespace, "no-secret"+configBackupSuffix)
	require.NoError(t, err, "sibling should exist because a HelmChart needs backing up")
}

func TestCreateBackupAutoNamesWhenEmpty(t *testing.T) {
	client := veleroDynamicFake()
	markCompletedOnGet(client, veleroBackupGVR)
	inst := fastInstaller(client, installedNamespace())

	summary, err := inst.CreateBackup(context.Background(), "")
	require.NoError(t, err)
	assert.True(t, len(summary.Name) > len("kipper-ai-"), "auto-generated name should include a timestamp")
	assert.Contains(t, summary.Name, "kipper-ai-")
}

func TestCreateBackupRefusesReservedSuffix(t *testing.T) {
	client := veleroDynamicFake()
	inst := fastInstaller(client)
	_, err := inst.CreateBackup(context.Background(), "user"+configBackupSuffix)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reserved")
}

// TestCreateBackupRefusesIfNamespaceMissing: a backup taken when
// kipper-ai isn't installed would Complete with zero items and look
// healthy. Refuse up front.
func TestCreateBackupRefusesIfNamespaceMissing(t *testing.T) {
	client := veleroDynamicFake()
	inst := fastInstaller(client) // no namespace seeded
	_, err := inst.CreateBackup(context.Background(), "no-bundle")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kipper-ai not found")
	assert.Contains(t, err.Error(), "kip ai install")
}

// TestCreateBackupRefusesUnownedNamespace: a `kipper-ai` namespace
// without the Kipper managed-by label is not our bundle. Refuse to
// snapshot unrelated content under our backup labels.
func TestCreateBackupRefusesUnownedNamespace(t *testing.T) {
	client := veleroDynamicFake()
	unowned := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: Namespace}}
	inst := fastInstaller(client, unowned)
	_, err := inst.CreateBackup(context.Background(), "unowned")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not managed by Kipper")
}

// TestPrimaryBackupRequestsFsBackup: the primary AI backup must pin
// defaultVolumesToFsBackup=true so PVC contents (model cache,
// MongoDB, Meilisearch) are captured even if the cluster's Velero
// install drifts away from the default.
func TestPrimaryBackupRequestsFsBackup(t *testing.T) {
	client := veleroDynamicFake()
	markCompletedOnGet(client, veleroBackupGVR)
	inst := fastInstaller(client, installedNamespace())

	_, err := inst.CreateBackup(context.Background(), "with-pvc")
	require.NoError(t, err)

	got, err := client.Tracker().Get(veleroBackupGVR, VeleroNamespace, "with-pvc")
	require.NoError(t, err)
	u := got.(*unstructured.Unstructured)
	fs, found, _ := unstructured.NestedBool(u.Object, "spec", "defaultVolumesToFsBackup")
	require.True(t, found, "primary backup must declare defaultVolumesToFsBackup explicitly")
	assert.True(t, fs, "primary backup must enable filesystem volume backup")
}

// TestCreateBackupRejectsZeroItemsCompleted: Velero can finish in
// Completed phase with zero items captured (e.g. label selector
// matched nothing). Treat as failure so the user is not handed an
// empty snapshot.
func TestCreateBackupRejectsZeroItemsCompleted(t *testing.T) {
	client := veleroDynamicFake()
	client.PrependReactor("get", "backups", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		obj, err := client.Tracker().Get(veleroBackupGVR, getAction.GetNamespace(), getAction.GetName())
		if err != nil {
			return true, nil, err
		}
		u := obj.(*unstructured.Unstructured).DeepCopy()
		_ = unstructured.SetNestedField(u.Object, "Completed", "status", "phase")
		// Zero items captured.
		return true, u, nil
	})
	inst := fastInstaller(client, installedNamespace())
	_, err := inst.CreateBackup(context.Background(), "empty")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "zero items")
}

// TestWatchBackupForWindowFailedValidation: Velero uses
// FailedValidation for issues that prevent the backup from running
// (missing BackupStorageLocation, RBAC, malformed spec). The async
// warmup must classify it as terminal-failed, not "still running".
func TestWatchBackupForWindowFailedValidation(t *testing.T) {
	client := veleroDynamicFake()
	client.PrependReactor("get", "backups", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		obj, err := client.Tracker().Get(veleroBackupGVR, getAction.GetNamespace(), getAction.GetName())
		if err != nil {
			return true, nil, err
		}
		u := obj.(*unstructured.Unstructured).DeepCopy()
		_ = unstructured.SetNestedField(u.Object, "FailedValidation", "status", "phase")
		return true, u, nil
	})
	inst := fastInstaller(client, installedNamespace())

	started, err := inst.StartBackup(context.Background(), "bsl-down")
	require.NoError(t, err)
	state, summary, err := inst.WatchBackupForWindow(context.Background(), started, 100*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, BackupFailed, state, "FailedValidation must be classified as a terminal failure")
	require.NotNil(t, summary)
	assert.Equal(t, "FailedValidation", summary.Phase)
}

// TestWaitBackupReportsFailedValidation: same coverage on the
// blocking --wait path. Without this, --wait would burn the full
// 30-minute timeout on a backup that was permanently broken at
// validation.
func TestWaitBackupReportsFailedValidation(t *testing.T) {
	client := veleroDynamicFake()
	client.PrependReactor("get", "backups", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		obj, err := client.Tracker().Get(veleroBackupGVR, getAction.GetNamespace(), getAction.GetName())
		if err != nil {
			return true, nil, err
		}
		u := obj.(*unstructured.Unstructured).DeepCopy()
		_ = unstructured.SetNestedField(u.Object, "FailedValidation", "status", "phase")
		return true, u, nil
	})
	inst := fastInstaller(client, installedNamespace())

	_, err := inst.CreateBackup(context.Background(), "bsl-down")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "FailedValidation")
}

// TestWaitBackupRejectsCompletedWithErrors: the blocking --wait path
// must apply the same health predicate as the async warmup, list,
// and restore. A Velero Backup that finished in Completed phase with
// non-zero errors is not restorable, so `kip ai backup --wait` must
// report it as a failure rather than printing the success checkmark.
// TestInFlightBackupNamesSeesNonTerminal: an InProgress (or
// FailedValidation, or empty-phase) Kipper AI Backup must show up
// in the in-flight list so `kip ai uninstall` can refuse.
// FailedValidation aside, a Completed Backup must not, since the
// snapshot is safe to keep at that point.
func TestInFlightBackupNamesSeesNonTerminal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		phase    string
		expected bool
	}{
		{"in progress", "InProgress", true},
		{"new", "New", true},
		{"empty phase", "", true},
		{"completed", "Completed", false},
		{"failed", "Failed", false},
		{"failed validation", "FailedValidation", false},
		{"partially failed", "PartiallyFailed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := veleroDynamicFake()
			b := newBackup("snap", kipperLabels())
			if tc.phase != "" {
				_ = unstructured.SetNestedField(b.Object, tc.phase, "status", "phase")
			}
			require.NoError(t, client.Tracker().Create(veleroBackupGVR, b, VeleroNamespace))

			inst := fastInstaller(client)
			got, err := inst.InFlightBackupNames(context.Background())
			require.NoError(t, err)
			if tc.expected {
				assert.Contains(t, got, "snap")
			} else {
				assert.NotContains(t, got, "snap")
			}
		})
	}
}

// TestInFlightBackupNamesCollapsesSibling: when both primary and
// sibling are still running, the user should see a single entry
// under the logical name, not two.
func TestInFlightBackupNamesCollapsesSibling(t *testing.T) {
	client := veleroDynamicFake()
	primary := newBackup("snap", kipperLabels())
	_ = unstructured.SetNestedField(primary.Object, "InProgress", "status", "phase")
	sibling := newBackup("snap"+configBackupSuffix, kipperLabels())
	_ = unstructured.SetNestedField(sibling.Object, "InProgress", "status", "phase")
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, sibling, VeleroNamespace))

	inst := fastInstaller(client)
	got, err := inst.InFlightBackupNames(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"snap"}, got)
}

// TestInFlightBackupNamesFailsClosedOnAPIServerTimeout: a transient
// apiserver hiccup while listing Velero Backup CRs must surface as
// an error, not be silently swallowed. Otherwise `kip ai uninstall`
// would race an in-flight async backup whenever the cluster control
// plane is briefly unhealthy — exactly when the guard most needs to
// be conservative.
func TestInFlightBackupNamesFailsClosedOnAPIServerTimeout(t *testing.T) {
	client := veleroDynamicFake()
	client.PrependReactor("list", "backups", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServerTimeout(schema.GroupResource{Group: "velero.io", Resource: "backups"}, "list", 1)
	})

	inst := fastInstaller(client)
	_, err := inst.InFlightBackupNames(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listing backups")
}

func TestInFlightBackupNamesIgnoresForeignBackups(t *testing.T) {
	client := veleroDynamicFake()
	foreign := newBackup("ops-nightly", map[string]string{"created-by": "ops"})
	_ = unstructured.SetNestedField(foreign.Object, "InProgress", "status", "phase")
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, foreign, VeleroNamespace))

	inst := fastInstaller(client)
	got, err := inst.InFlightBackupNames(context.Background())
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestWaitBackupRejectsCompletedWithErrors(t *testing.T) {
	client := veleroDynamicFake()
	client.PrependReactor("get", "backups", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		obj, err := client.Tracker().Get(veleroBackupGVR, getAction.GetNamespace(), getAction.GetName())
		if err != nil {
			return true, nil, err
		}
		u := obj.(*unstructured.Unstructured).DeepCopy()
		_ = unstructured.SetNestedField(u.Object, "Completed", "status", "phase")
		_ = unstructured.SetNestedField(u.Object, int64(7), "status", "progress", "itemsBackedUp")
		_ = unstructured.SetNestedField(u.Object, int64(2), "status", "errors")
		return true, u, nil
	})
	inst := fastInstaller(client, installedNamespace())

	_, err := inst.CreateBackup(context.Background(), "noisy")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not restorable")
	assert.Contains(t, err.Error(), "completed with 2 errors")
}

func TestWatchBackupForWindowCompletedInTime(t *testing.T) {
	client := veleroDynamicFake()
	markCompletedOnGet(client, veleroBackupGVR)
	inst := fastInstaller(client, installedNamespace())

	started, err := inst.StartBackup(context.Background(), "fast")
	require.NoError(t, err)
	state, summary, err := inst.WatchBackupForWindow(context.Background(), started, 200*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, BackupCompleted, state)
	require.NotNil(t, summary)
	assert.Equal(t, "Completed", summary.Phase)
}

// TestWatchBackupForWindowRejectsZeroItemCompletion: a fast-finishing
// backup that captured zero items must not be reported as
// BackupCompleted. The blocking path's waitForBackup already rejects
// this; the async warmup must apply the same gate so the CLI does
// not print a green checkmark over an empty snapshot.
func TestWatchBackupForWindowRejectsZeroItemCompletion(t *testing.T) {
	client := veleroDynamicFake()
	client.PrependReactor("get", "backups", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		obj, err := client.Tracker().Get(veleroBackupGVR, getAction.GetNamespace(), getAction.GetName())
		if err != nil {
			return true, nil, err
		}
		u := obj.(*unstructured.Unstructured).DeepCopy()
		_ = unstructured.SetNestedField(u.Object, "Completed", "status", "phase")
		// itemsBackedUp deliberately left at 0.
		return true, u, nil
	})
	inst := fastInstaller(client, installedNamespace())

	started, err := inst.StartBackup(context.Background(), "empty")
	require.NoError(t, err)
	state, _, err := inst.WatchBackupForWindow(context.Background(), started, 100*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, BackupFailed, state, "Completed-with-zero-items must classify as failure, not BackupCompleted")
}

// TestWatchBackupForWindowPrimaryUnhealthyWhileSiblingRunning: a
// primary that finishes Completed-but-unhealthy (zero items, or
// errors > 0) while the sibling is still in flight must classify as
// BackupFailed immediately. Otherwise the watcher would wait for the
// sibling to finish and then time out the warmup window, returning
// BackupStillRunning over a snapshot that is already known to be
// unrestorable.
func TestWatchBackupForWindowPrimaryUnhealthyWhileSiblingRunning(t *testing.T) {
	client := veleroDynamicFake()
	client.PrependReactor("get", "backups", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		obj, err := client.Tracker().Get(veleroBackupGVR, getAction.GetNamespace(), getAction.GetName())
		if err != nil {
			return true, nil, err
		}
		u := obj.(*unstructured.Unstructured).DeepCopy()
		if !strings.HasSuffix(getAction.GetName(), configBackupSuffix) {
			// Primary is Completed but unhealthy: zero items.
			_ = unstructured.SetNestedField(u.Object, "Completed", "status", "phase")
		} else {
			_ = unstructured.SetNestedField(u.Object, "InProgress", "status", "phase")
		}
		return true, u, nil
	})
	inst := fastInstaller(client, installedNamespace(), aiConfigSecret())

	started, err := inst.StartBackup(context.Background(), "split-broken")
	require.NoError(t, err)
	require.True(t, started.HasSibling)
	state, _, err := inst.WatchBackupForWindow(context.Background(), started, 100*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, BackupFailed, state, "primary Completed-but-unhealthy must classify as failure even with sibling still running")
}

// TestWatchBackupForWindowSiblingUnhealthyWhilePrimaryRunning: same
// rule the other way around.
func TestWatchBackupForWindowSiblingUnhealthyWhilePrimaryRunning(t *testing.T) {
	client := veleroDynamicFake()
	client.PrependReactor("get", "backups", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		obj, err := client.Tracker().Get(veleroBackupGVR, getAction.GetNamespace(), getAction.GetName())
		if err != nil {
			return true, nil, err
		}
		u := obj.(*unstructured.Unstructured).DeepCopy()
		if strings.HasSuffix(getAction.GetName(), configBackupSuffix) {
			// Sibling is Completed but unhealthy.
			_ = unstructured.SetNestedField(u.Object, "Completed", "status", "phase")
			_ = unstructured.SetNestedField(u.Object, int64(7), "status", "progress", "itemsBackedUp")
			_ = unstructured.SetNestedField(u.Object, int64(2), "status", "errors")
		} else {
			_ = unstructured.SetNestedField(u.Object, "InProgress", "status", "phase")
		}
		return true, u, nil
	})
	inst := fastInstaller(client, installedNamespace(), aiConfigSecret())

	started, err := inst.StartBackup(context.Background(), "split-sibling-bad")
	require.NoError(t, err)
	require.True(t, started.HasSibling)
	state, _, err := inst.WatchBackupForWindow(context.Background(), started, 100*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, BackupFailed, state)
}

// TestWatchBackupForWindowExpectedSiblingMissingIsRunning: primary
// completes inside the window but the expected sibling hasn't reached
// terminal state yet. The watch must stay in BackupStillRunning, not
// declare success because primary is done.
func TestWatchBackupForWindowExpectedSiblingMissingIsRunning(t *testing.T) {
	client := veleroDynamicFake()
	client.PrependReactor("get", "backups", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		obj, err := client.Tracker().Get(veleroBackupGVR, getAction.GetNamespace(), getAction.GetName())
		if err != nil {
			return true, nil, err
		}
		u := obj.(*unstructured.Unstructured).DeepCopy()
		if !strings.HasSuffix(getAction.GetName(), configBackupSuffix) {
			_ = unstructured.SetNestedField(u.Object, "Completed", "status", "phase")
			_ = unstructured.SetNestedField(u.Object, int64(7), "status", "progress", "itemsBackedUp")
		} else {
			_ = unstructured.SetNestedField(u.Object, "InProgress", "status", "phase")
		}
		return true, u, nil
	})
	inst := fastInstaller(client, installedNamespace(), aiConfigSecret())

	started, err := inst.StartBackup(context.Background(), "split")
	require.NoError(t, err)
	require.True(t, started.HasSibling, "test setup expects sibling to be started so the watcher has something to wait on")
	state, _, err := inst.WatchBackupForWindow(context.Background(), started, 50*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, BackupStillRunning, state, "primary done + sibling running must stay BackupStillRunning")
}

func TestWatchBackupForWindowFailedInTime(t *testing.T) {
	client := veleroDynamicFake()
	client.PrependReactor("get", "backups", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		obj, err := client.Tracker().Get(veleroBackupGVR, getAction.GetNamespace(), getAction.GetName())
		if err != nil {
			return true, nil, err
		}
		u := obj.(*unstructured.Unstructured).DeepCopy()
		_ = unstructured.SetNestedField(u.Object, "Failed", "status", "phase")
		_ = unstructured.SetNestedField(u.Object, int64(2), "status", "errors")
		return true, u, nil
	})
	inst := fastInstaller(client, installedNamespace())

	started, err := inst.StartBackup(context.Background(), "broken")
	require.NoError(t, err)
	state, summary, err := inst.WatchBackupForWindow(context.Background(), started, 200*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, BackupFailed, state)
	require.NotNil(t, summary)
	assert.Equal(t, "Failed", summary.Phase)
}

// TestWatchBackupForWindowStillRunningAfterDeadline: a long-running
// snapshot that hasn't reached terminal state in the warmup window
// must report BackupStillRunning, not error or hang. The CLI uses
// this to print "running in the background; check with kip ai backup
// show".
func TestWatchBackupForWindowStillRunningAfterDeadline(t *testing.T) {
	client := veleroDynamicFake()
	client.PrependReactor("get", "backups", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		obj, err := client.Tracker().Get(veleroBackupGVR, getAction.GetNamespace(), getAction.GetName())
		if err != nil {
			return true, nil, err
		}
		u := obj.(*unstructured.Unstructured).DeepCopy()
		_ = unstructured.SetNestedField(u.Object, "InProgress", "status", "phase")
		return true, u, nil
	})
	inst := fastInstaller(client, installedNamespace())

	started, err := inst.StartBackup(context.Background(), "slow")
	require.NoError(t, err)
	state, summary, err := inst.WatchBackupForWindow(context.Background(), started, 50*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, BackupStillRunning, state)
	require.NotNil(t, summary)
	assert.Equal(t, "InProgress", summary.Phase)
}

func TestGetBackupStatusReturnsBothComponents(t *testing.T) {
	client := veleroDynamicFake()
	primary := newBackupWithStatus("snap", "Completed", 0)
	primary.SetAnnotations(map[string]string{configSiblingAnnotation: "true"})
	sibling := newBackupWithStatus("snap"+configBackupSuffix, "Completed", 0)
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, sibling, VeleroNamespace))

	inst := fastInstaller(client)
	status, err := inst.GetBackupStatus(context.Background(), "snap")
	require.NoError(t, err)
	require.NotNil(t, status.Primary)
	require.NotNil(t, status.Sibling)
	assert.True(t, status.ExpectsSibling)
	assert.Equal(t, "Completed", status.Primary.Phase)
	assert.Equal(t, "Completed", status.Sibling.Phase)
}

func TestGetBackupStatusReturnsNilSiblingWhenAbsent(t *testing.T) {
	client := veleroDynamicFake()
	primary := newBackupWithStatus("snap", "InProgress", 0)
	primary.SetAnnotations(map[string]string{configSiblingAnnotation: "false"})
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))

	inst := fastInstaller(client)
	status, err := inst.GetBackupStatus(context.Background(), "snap")
	require.NoError(t, err)
	require.NotNil(t, status.Primary)
	assert.Nil(t, status.Sibling)
	assert.False(t, status.ExpectsSibling)
}

// TestGetBackupStatusFlagsExpectedButMissingSibling: a primary
// annotated as expecting a sibling, but the sibling is gone (lost to
// a partial delete / manual kubectl). The status must mark
// ExpectsSibling=true so `kip ai backup show` can render the missing
// component as incomplete rather than "intentionally absent".
func TestGetBackupStatusFlagsExpectedButMissingSibling(t *testing.T) {
	client := veleroDynamicFake()
	primary := newBackupWithStatus("snap", "Completed", 0)
	primary.SetAnnotations(map[string]string{configSiblingAnnotation: "true"})
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))

	inst := fastInstaller(client)
	status, err := inst.GetBackupStatus(context.Background(), "snap")
	require.NoError(t, err)
	require.NotNil(t, status.Primary)
	assert.Nil(t, status.Sibling)
	assert.True(t, status.ExpectsSibling, "expected-sibling annotation must be honoured even when sibling has gone missing")
}

// TestGetBackupStatusRefusesForeignBackup: a Velero backup that
// happens to share a name with a Kipper AI backup (or a non-AI
// Kipper backup like a future cluster-snapshot subsystem) must not
// be presented by `kip ai backup show` as if it were ours.
func TestGetBackupStatusRefusesForeignBackup(t *testing.T) {
	client := veleroDynamicFake()
	foreign := newBackup("snap", map[string]string{"created-by": "ops"})
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, foreign, VeleroNamespace))

	inst := fastInstaller(client)
	_, err := inst.GetBackupStatus(context.Background(), "snap")
	require.Error(t, err)
	var notOwned *errNotOwnedByKipper
	require.True(t, errors.As(err, &notOwned), "expected errNotOwnedByKipper for foreign backup, got %T", err)
}

// TestBackupStatusRestorable covers the cases that decide whether
// `kip ai backup show` returns a clean verdict or flags the snapshot
// as unrestorable. These are the same gates `--wait`, list, and
// restore use, so show cannot disagree with them.
func TestBackupStatusRestorable(t *testing.T) {
	tests := []struct {
		name       string
		status     BackupStatus
		wantClean  bool
		wantReason string
	}{
		{
			name: "clean primary only",
			status: BackupStatus{
				Primary: &BackupSummary{Phase: "Completed", ItemsBackedUp: 7},
			},
			wantClean: true,
		},
		{
			name: "clean primary + sibling",
			status: BackupStatus{
				Primary:        &BackupSummary{Phase: "Completed", ItemsBackedUp: 7},
				Sibling:        &BackupSummary{Phase: "Completed", ItemsBackedUp: 3},
				ExpectsSibling: true,
			},
			wantClean: true,
		},
		{
			name: "primary missing",
			status: BackupStatus{
				ExpectsSibling: true,
			},
			wantReason: "primary backup missing",
		},
		{
			name: "primary still in progress",
			status: BackupStatus{
				Primary: &BackupSummary{Phase: "InProgress"},
			},
			wantReason: "primary in phase InProgress",
		},
		{
			name: "primary completed with errors",
			status: BackupStatus{
				Primary: &BackupSummary{Phase: "Completed", ItemsBackedUp: 7, Errors: 2},
			},
			wantReason: "completed with 2 errors",
		},
		{
			name: "primary completed with zero items",
			status: BackupStatus{
				Primary: &BackupSummary{Phase: "Completed"},
			},
			wantReason: "completed but captured zero items",
		},
		{
			name: "expected sibling missing",
			status: BackupStatus{
				Primary:        &BackupSummary{Phase: "Completed", ItemsBackedUp: 7},
				ExpectsSibling: true,
			},
			wantReason: "config sibling expected but missing",
		},
		{
			name: "sibling completed with errors",
			status: BackupStatus{
				Primary:        &BackupSummary{Phase: "Completed", ItemsBackedUp: 7},
				Sibling:        &BackupSummary{Phase: "Completed", ItemsBackedUp: 3, Errors: 1},
				ExpectsSibling: true,
			},
			wantReason: "config sibling completed with 1 errors",
		},
		{
			// Older snapshot taken before the expects-config-sibling
			// annotation existed: ExpectsSibling=false, but a sibling
			// CR is on the cluster and is broken. show must still
			// flag it; otherwise show && uninstall could chain over a
			// snapshot restore would later refuse.
			name: "annotation missing but sibling present and unhealthy",
			status: BackupStatus{
				Primary:        &BackupSummary{Phase: "Completed", ItemsBackedUp: 7},
				Sibling:        &BackupSummary{Phase: "Completed", ItemsBackedUp: 3, Errors: 1},
				ExpectsSibling: false,
			},
			wantReason: "config sibling completed with 1 errors",
		},
		{
			name: "annotation missing, sibling present and clean",
			status: BackupStatus{
				Primary:        &BackupSummary{Phase: "Completed", ItemsBackedUp: 7},
				Sibling:        &BackupSummary{Phase: "Completed", ItemsBackedUp: 3},
				ExpectsSibling: false,
			},
			wantClean: true,
		},
		{
			name: "sibling still in progress",
			status: BackupStatus{
				Primary:        &BackupSummary{Phase: "Completed", ItemsBackedUp: 7},
				Sibling:        &BackupSummary{Phase: "InProgress"},
				ExpectsSibling: true,
			},
			wantReason: "config sibling in phase InProgress",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reason := tc.status.Restorable()
			if tc.wantClean {
				assert.Empty(t, reason, "expected clean status, got reason: %s", reason)
				return
			}
			assert.Contains(t, reason, tc.wantReason)
		})
	}
}

func TestGetBackupStatusErrorsOnUnknownName(t *testing.T) {
	client := veleroDynamicFake()
	inst := fastInstaller(client)
	_, err := inst.GetBackupStatus(context.Background(), "nope")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestGetBackupStatusRefusesSiblingName(t *testing.T) {
	client := veleroDynamicFake()
	inst := fastInstaller(client)
	_, err := inst.GetBackupStatus(context.Background(), "snap"+configBackupSuffix)
	require.Error(t, err)
	assert.Contains(t, err.Error(), configBackupSuffix)
}

func TestCreateBackupReportsFailure(t *testing.T) {
	client := veleroDynamicFake()
	client.PrependReactor("get", "backups", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		obj, err := client.Tracker().Get(veleroBackupGVR, getAction.GetNamespace(), getAction.GetName())
		if err != nil {
			return true, nil, err
		}
		u := obj.(*unstructured.Unstructured).DeepCopy()
		_ = unstructured.SetNestedField(u.Object, "Failed", "status", "phase")
		_ = unstructured.SetNestedField(u.Object, int64(3), "status", "errors")
		return true, u, nil
	})
	inst := fastInstaller(client, installedNamespace())

	summary, err := inst.CreateBackup(context.Background(), "broken")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Failed")
	assert.Contains(t, err.Error(), "3 errors")
	require.NotNil(t, summary)
	assert.Equal(t, "Failed", summary.Phase)
}

// TestCreateBackupRefusesIfStaleSiblingExists: an orphan sibling from
// a partially failed earlier delete must not be silently paired with
// a fresh primary of the same logical name. CreateBackup pre-checks
// both physical names and refuses if either is taken.
func TestCreateBackupRefusesIfStaleSiblingExists(t *testing.T) {
	client := veleroDynamicFake()
	stale := newBackupWithStatus("snap"+configBackupSuffix, "Completed", 0)
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, stale, VeleroNamespace))

	inst := fastInstaller(client, installedNamespace())
	_, err := inst.CreateBackup(context.Background(), "snap")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
	assert.Contains(t, err.Error(), "kip ai backup delete")

	// And the primary was never created.
	_, err = client.Tracker().Get(veleroBackupGVR, VeleroNamespace, "snap")
	require.True(t, apierrors.IsNotFound(err))
}

func TestCreateBackupRejectsDuplicateName(t *testing.T) {
	client := veleroDynamicFake()
	markCompletedOnGet(client, veleroBackupGVR)
	inst := fastInstaller(client, installedNamespace())

	_, err := inst.CreateBackup(context.Background(), "twin")
	require.NoError(t, err)

	_, err = inst.CreateBackup(context.Background(), "twin")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

func TestListBackupsHidesSiblings(t *testing.T) {
	client := veleroDynamicFake()
	primary := newBackup("snap", kipperLabels())
	sibling := newBackup("snap"+configBackupSuffix, kipperLabels())
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, sibling, VeleroNamespace))

	inst := fastInstaller(client)
	got, err := inst.ListBackups(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "snap", got[0].Name)
}

func TestListBackupsFiltersForeignBackups(t *testing.T) {
	client := veleroDynamicFake()
	mine := newBackup("mine", kipperLabels())
	foreign := newBackup("velero-nightly", map[string]string{"created-by": "ops"})

	require.NoError(t, client.Tracker().Create(veleroBackupGVR, mine, VeleroNamespace))
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, foreign, VeleroNamespace))

	inst := fastInstaller(client)
	got, err := inst.ListBackups(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "mine", got[0].Name)
}

// TestListBackupsRollsUpSiblingFailures: when the cross-namespace
// config backup is PartiallyFailed, the logical entry must reflect
// that rather than reporting clean because the primary is fine.
func TestListBackupsRollsUpSiblingFailures(t *testing.T) {
	client := veleroDynamicFake()
	primary := newBackupWithStatus("snap", "Completed", 0)
	sibling := newBackupWithStatus("snap"+configBackupSuffix, "PartiallyFailed", 5)
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, sibling, VeleroNamespace))

	inst := fastInstaller(client)
	got, err := inst.ListBackups(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "snap", got[0].Name)
	assert.Equal(t, "PartiallyFailed", got[0].Phase, "logical entry should reflect the worse sibling phase")
	assert.Equal(t, int64(5), got[0].Errors)
}

func TestListBackupsSortsNewestFirst(t *testing.T) {
	client := veleroDynamicFake()
	older := newBackupAt("older", time.Now().Add(-2*time.Hour))
	newer := newBackupAt("newer", time.Now().Add(-1*time.Minute))
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, older, VeleroNamespace))
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, newer, VeleroNamespace))

	inst := fastInstaller(client)
	got, err := inst.ListBackups(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "newer", got[0].Name)
	assert.Equal(t, "older", got[1].Name)
}

func TestDeleteBackupRefusesForeignBackup(t *testing.T) {
	client := veleroDynamicFake()
	foreign := newBackup("velero-nightly", map[string]string{"created-by": "ops"})
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, foreign, VeleroNamespace))

	inst := fastInstaller(client)
	err := inst.DeleteBackup(context.Background(), "velero-nightly")
	require.Error(t, err)
	var notOwned *errNotOwnedByKipper
	require.True(t, errors.As(err, &notOwned), "expected errNotOwnedByKipper, got %T", err)
}

// TestDeleteBackupRefusesNonAIKipperBackup: a Kipper-managed Velero
// Backup that belongs to another subsystem (e.g. cluster snapshots)
// must not be deletable through `kip ai backup delete`. The
// part-of=kipper-ai check is what isolates the AI bundle's scope
// from other Kipper-owned backups.
func TestDeleteBackupRefusesNonAIKipperBackup(t *testing.T) {
	client := veleroDynamicFake()
	otherSubsystem := newBackup("cluster-nightly", map[string]string{
		managedByLabel:              managedByValue,
		"app.kubernetes.io/part-of": "kipper-cluster",
	})
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, otherSubsystem, VeleroNamespace))

	inst := fastInstaller(client)
	err := inst.DeleteBackup(context.Background(), "cluster-nightly")
	require.Error(t, err)
	var notOwned *errNotOwnedByKipper
	require.True(t, errors.As(err, &notOwned), "expected errNotOwnedByKipper for non-AI Kipper backup, got %T", err)
}

func TestDeleteBackupGoesThroughDeleteRequest(t *testing.T) {
	// Real Velero only frees object-storage data when a
	// DeleteBackupRequest CR is created. This test verifies the request
	// is created and that DeleteBackup waits for the Backup CR to be
	// finalised by the Velero controller.
	client := veleroDynamicFake()
	deleteOnDeleteRequest(client)

	primary := newBackup("snap", kipperLabels())
	sibling := newBackup("snap"+configBackupSuffix, kipperLabels())
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, sibling, VeleroNamespace))

	createCount := 0
	client.PrependReactor("create", "deletebackuprequests", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createCount++
		return false, nil, nil
	})

	inst := fastInstaller(client)
	require.NoError(t, inst.DeleteBackup(context.Background(), "snap"))
	assert.Equal(t, 2, createCount, "expected one DeleteBackupRequest per backup (primary + sibling)")

	for _, name := range []string{"snap", "snap" + configBackupSuffix} {
		_, err := client.Tracker().Get(veleroBackupGVR, VeleroNamespace, name)
		require.True(t, apierrors.IsNotFound(err), "%s should be gone after delete request", name)
	}
}

// TestListBackupsSurfacesOrphanSibling: a partial delete leaves
// `<name>-aiconfig` behind. The list must surface that orphan
// (carrying credentials in object storage), not silently hide it.
func TestListBackupsSurfacesOrphanSibling(t *testing.T) {
	client := veleroDynamicFake()
	orphan := newBackupWithStatus("snap"+configBackupSuffix, "Completed", 0)
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, orphan, VeleroNamespace))

	inst := fastInstaller(client)
	got, err := inst.ListBackups(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "snap"+configBackupSuffix, got[0].Name)
	assert.Equal(t, "PartiallyFailed", got[0].Phase)
}

// TestDeleteBackupCleansOrphanSibling: an orphan `<name>-aiconfig`
// (primary already gone from a partial earlier delete) can still be
// cleaned by passing the logical primary name to DeleteBackup.
func TestDeleteBackupCleansOrphanSibling(t *testing.T) {
	client := veleroDynamicFake()
	deleteOnDeleteRequest(client)

	orphan := newBackup("snap"+configBackupSuffix, kipperLabels())
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, orphan, VeleroNamespace))

	inst := fastInstaller(client)
	require.NoError(t, inst.DeleteBackup(context.Background(), "snap"))
	_, err := client.Tracker().Get(veleroBackupGVR, VeleroNamespace, "snap"+configBackupSuffix)
	require.True(t, apierrors.IsNotFound(err), "orphan sibling should be cleaned by deleting via the logical primary name")
}

// TestRestoreRefusesIfHelmChartsLeftBehind: a half-uninstalled cluster
// (namespace gone, but kube-system HelmCharts still present) is not a
// safe restore target — helm-controller would race the restore.
func TestRestoreRefusesIfHelmChartsLeftBehind(t *testing.T) {
	client := veleroDynamicFake()
	mine := newBackupWithStatus("mine", "Completed", 0)
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, mine, VeleroNamespace))

	leftover := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "helm.cattle.io/v1",
		"kind":       "HelmChart",
		"metadata": map[string]interface{}{
			"name":      "ollama",
			"namespace": "kube-system",
			"labels": map[string]interface{}{
				managedByLabel:              managedByValue,
				"app.kubernetes.io/part-of": "kipper-ai",
			},
		},
	}}
	require.NoError(t, client.Tracker().Create(helmChartGVR, leftover, "kube-system"))

	inst := fastInstaller(client) // no kipper-ai namespace seeded
	_, err := inst.CreateRestore(context.Background(), "mine")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HelmChart")
	assert.Contains(t, err.Error(), "kip ai uninstall")
}

// TestDeleteBackupRefusesSiblingName: passing the physical sibling
// name to delete would orphan the primary's logical snapshot. Refuse
// up front and direct the user to the logical primary name.
func TestDeleteBackupRefusesSiblingName(t *testing.T) {
	client := veleroDynamicFake()
	inst := fastInstaller(client)
	err := inst.DeleteBackup(context.Background(), "snap"+configBackupSuffix)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "logical primary name")
}

// TestGetValidVeleroLabel mirrors Velero's pkg/label.GetValidName so
// our generated `velero.io/backup-name` label values match the ones
// the Velero deletion controller computes when it builds its
// existing-request selector. Keeping the algorithms in sync is what
// lets Velero dedupe in-flight requests for the same backup.
func TestGetValidVeleroLabel(t *testing.T) {
	t.Run("short name passes through", func(t *testing.T) {
		got := getValidVeleroLabel("snap")
		assert.Equal(t, "snap", got)
		assert.LessOrEqual(t, len(got), 63)
		assert.Empty(t, validation.IsValidLabelValue(got))
	})
	t.Run("63-char name passes through", func(t *testing.T) {
		name := strings.Repeat("a", 63)
		got := getValidVeleroLabel(name)
		assert.Equal(t, name, got)
		assert.Equal(t, 63, len(got))
	})
	t.Run("64-char name is truncated and hashed", func(t *testing.T) {
		name := strings.Repeat("a", 64)
		got := getValidVeleroLabel(name)
		assert.Equal(t, 63, len(got), "result must fit in label-value limit")
		assert.Empty(t, validation.IsValidLabelValue(got))
	})
	t.Run("different long names produce different labels", func(t *testing.T) {
		a := strings.Repeat("a", 200)
		b := strings.Repeat("a", 200) + "y"
		assert.NotEqual(t, getValidVeleroLabel(a), getValidVeleroLabel(b))
	})
	t.Run("deterministic for the same input", func(t *testing.T) {
		name := strings.Repeat("a", 200)
		assert.Equal(t, getValidVeleroLabel(name), getValidVeleroLabel(name))
	})
}

// TestStartDeleteAdoptsExistingInFlightRequest: a previous
// `kip ai backup delete` run created a DeleteBackupRequest that is
// still in flight (Velero hasn't finished). On retry, StartDelete
// must adopt the existing request rather than creating a fresh one.
// Without adoption, Velero's deduplication (by velero.io/backup-name)
// would delete the new request as a duplicate, and our watcher would
// then flag the cleanup as orphaned even though the older request is
// progressing fine.
func TestStartDeleteAdoptsExistingInFlightRequest(t *testing.T) {
	client := veleroDynamicFake()
	primary := newBackup("snap", kipperLabels())
	primary.SetUID("primary-uid-abc")
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))

	// Pre-existing in-flight request from an earlier run.
	existing := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1",
		"kind":       "DeleteBackupRequest",
		"metadata": map[string]interface{}{
			"name":      "snap-delete-prior",
			"namespace": VeleroNamespace,
			"labels": map[string]interface{}{
				"velero.io/backup-name": "snap",
				"velero.io/backup-uid":  "primary-uid-abc",
			},
		},
		"spec":   map[string]interface{}{"backupName": "snap"},
		"status": map[string]interface{}{"phase": "InProgress"},
	}}
	require.NoError(t, client.Tracker().Create(veleroDeleteRequestGVR, existing, VeleroNamespace))

	createCount := 0
	client.PrependReactor("create", "deletebackuprequests", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		createCount++
		return false, nil, nil
	})

	inst := fastInstaller(client)
	started, err := inst.StartDelete(context.Background(), "snap")
	require.NoError(t, err)
	assert.Equal(t, 0, createCount, "must not create a duplicate request when one is already in flight")
	require.Len(t, started.Requests, 1)
	assert.Equal(t, "snap-delete-prior", started.Requests[0], "must adopt the existing in-flight request")
}

// TestStartDeleteAdoptsRequestMissingBackupUIDLabel: an existing
// in-flight DeleteBackupRequest from a previous run that hasn't yet
// been picked up by Velero may not carry the velero.io/backup-uid
// label (Velero writes that label after starting to process). The
// adoption logic must still find and adopt it via the backup-name
// label + spec.backupName — otherwise we'd create a duplicate that
// Velero would dedupe, looking like an orphan to our watcher.
func TestStartDeleteAdoptsRequestMissingBackupUIDLabel(t *testing.T) {
	client := veleroDynamicFake()
	primary := newBackup("snap", kipperLabels())
	primary.SetUID("primary-uid-abc")
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))

	// Pre-existing New request with backup-name label only (no UID label yet).
	existing := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1",
		"kind":       "DeleteBackupRequest",
		"metadata": map[string]interface{}{
			"name":      "snap-delete-pending",
			"namespace": VeleroNamespace,
			"labels": map[string]interface{}{
				"velero.io/backup-name": "snap",
			},
		},
		"spec": map[string]interface{}{"backupName": "snap"},
	}}
	require.NoError(t, client.Tracker().Create(veleroDeleteRequestGVR, existing, VeleroNamespace))

	createCount := 0
	client.PrependReactor("create", "deletebackuprequests", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		createCount++
		return false, nil, nil
	})

	inst := fastInstaller(client)
	started, err := inst.StartDelete(context.Background(), "snap")
	require.NoError(t, err)
	assert.Equal(t, 0, createCount, "must adopt the existing request even when it's missing the backup-uid label")
	assert.Equal(t, "snap-delete-pending", started.Requests[0])
}

// TestStuckDeleteRequestsToleratesDedupedConcurrentRequest: two
// concurrent `kip ai backup delete` calls each create their own
// DeleteBackupRequest. Velero's deduplication keeps one and removes
// the other. The watcher whose request was removed must NOT
// classify that as stuck — it must check for any other active
// request via the labels and treat the surviving request as the one
// progressing the deletion.
func TestStuckDeleteRequestsToleratesDedupedConcurrentRequest(t *testing.T) {
	client := veleroDynamicFake()
	primary := newBackup("snap", kipperLabels())
	primary.SetUID("primary-uid-abc")
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))

	inst := fastInstaller(client)
	started, err := inst.StartDelete(context.Background(), "snap")
	require.NoError(t, err)
	require.Len(t, started.Requests, 1)

	// Simulate Velero dedupe: our request is removed.
	require.NoError(t, client.Tracker().Delete(veleroDeleteRequestGVR, VeleroNamespace, started.Requests[0]))

	// A concurrent kip invocation's request is now the active one.
	rival := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1",
		"kind":       "DeleteBackupRequest",
		"metadata": map[string]interface{}{
			"name":      "snap-delete-rival",
			"namespace": VeleroNamespace,
			"labels": map[string]interface{}{
				"velero.io/backup-name": "snap",
				"velero.io/backup-uid":  "primary-uid-abc",
			},
		},
		"spec":   map[string]interface{}{"backupName": "snap"},
		"status": map[string]interface{}{"phase": "InProgress"},
	}}
	require.NoError(t, client.Tracker().Create(veleroDeleteRequestGVR, rival, VeleroNamespace))

	stuck, err := inst.stuckDeleteRequests(context.Background(), started)
	require.NoError(t, err)
	assert.Empty(t, stuck, "deduped request must not be flagged as stuck while another active request is doing the work")
}

// TestStuckDeleteRequestsSurfacesRivalErrorsAfterDedupe: a
// concurrent retry where Velero deletes the watched request and the
// surviving rival fails (status.errors non-empty) must surface the
// rival's name, not the deleted watched request's name. Otherwise
// the CLI points the operator at a CR that no longer exists.
func TestStuckDeleteRequestsSurfacesRivalErrorsAfterDedupe(t *testing.T) {
	client := veleroDynamicFake()
	primary := newBackup("snap", kipperLabels())
	primary.SetUID("primary-uid-abc")
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))

	inst := fastInstaller(client)
	started, err := inst.StartDelete(context.Background(), "snap")
	require.NoError(t, err)

	require.NoError(t, client.Tracker().Delete(veleroDeleteRequestGVR, VeleroNamespace, started.Requests[0]))

	rival := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1",
		"kind":       "DeleteBackupRequest",
		"metadata": map[string]interface{}{
			"name":      "snap-delete-rival",
			"namespace": VeleroNamespace,
			"labels": map[string]interface{}{
				"velero.io/backup-name": "snap",
			},
		},
		"spec": map[string]interface{}{"backupName": "snap"},
		"status": map[string]interface{}{
			"phase":  "Processed",
			"errors": []interface{}{"object storage location unreachable"},
		},
	}}
	require.NoError(t, client.Tracker().Create(veleroDeleteRequestGVR, rival, VeleroNamespace))

	stuck, err := inst.stuckDeleteRequests(context.Background(), started)
	require.NoError(t, err)
	require.Len(t, stuck, 1)
	assert.Equal(t, "snap-delete-rival", stuck[0],
		"must surface the failing rival's name, not the deleted watched request's name")
}

// TestStuckDeleteRequestsFlagsDedupedRequestWhenRivalAlsoStuck: a
// concurrent retry where Velero deletes the watched request and the
// surviving rival is itself still phase=New (controller not picking
// either up) must classify as stuck. Two stalled requests are no
// better than one — without this check the deduped+rival-also-New
// scenario would silently report "running in the background" forever.
func TestStuckDeleteRequestsFlagsDedupedRequestWhenRivalAlsoStuck(t *testing.T) {
	client := veleroDynamicFake()
	primary := newBackup("snap", kipperLabels())
	primary.SetUID("primary-uid-abc")
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))

	inst := fastInstaller(client)
	started, err := inst.StartDelete(context.Background(), "snap")
	require.NoError(t, err)
	require.Len(t, started.Requests, 1)

	require.NoError(t, client.Tracker().Delete(veleroDeleteRequestGVR, VeleroNamespace, started.Requests[0]))

	// Rival is present but also stuck in empty/New phase: Velero
	// hasn't picked either up.
	rival := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1",
		"kind":       "DeleteBackupRequest",
		"metadata": map[string]interface{}{
			"name":      "snap-delete-rival",
			"namespace": VeleroNamespace,
			"labels": map[string]interface{}{
				"velero.io/backup-name": "snap",
			},
		},
		"spec": map[string]interface{}{"backupName": "snap"},
		// status.phase deliberately empty.
	}}
	require.NoError(t, client.Tracker().Create(veleroDeleteRequestGVR, rival, VeleroNamespace))

	stuck, err := inst.stuckDeleteRequests(context.Background(), started)
	require.NoError(t, err)
	require.Len(t, stuck, 1, "deduped request must be flagged as stuck when no rival is actually progressing")
	assert.Equal(t, started.Requests[0], stuck[0])
}

// TestStuckDeleteRequestsTreatsQueuedSiblingAsRunning: when the
// primary request is InProgress and the sibling is still in New
// phase past the warmup window, the watcher must NOT classify the
// sibling as stuck — Velero may be serialising work, so the sibling
// is queued behind the primary, not actually stalled.
func TestStuckDeleteRequestsTreatsQueuedSiblingAsRunning(t *testing.T) {
	client := veleroDynamicFake()
	primary := newBackup("snap", kipperLabels())
	primary.SetUID("primary-uid-abc")
	sibling := newBackup("snap"+configBackupSuffix, kipperLabels())
	sibling.SetUID("sibling-uid-def")
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, sibling, VeleroNamespace))

	client.PrependReactor("get", "deletebackuprequests", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		obj, err := client.Tracker().Get(veleroDeleteRequestGVR, getAction.GetNamespace(), getAction.GetName())
		if err != nil {
			return true, nil, err
		}
		u := obj.(*unstructured.Unstructured).DeepCopy()
		// Primary InProgress, sibling left in default (empty) phase.
		spec, _, _ := unstructured.NestedString(u.Object, "spec", "backupName")
		if spec == "snap" {
			_ = unstructured.SetNestedField(u.Object, "InProgress", "status", "phase")
		}
		return true, u, nil
	})

	inst := fastInstaller(client)
	started, err := inst.StartDelete(context.Background(), "snap")
	require.NoError(t, err)
	require.Len(t, started.Requests, 2)

	stuck, err := inst.stuckDeleteRequests(context.Background(), started)
	require.NoError(t, err)
	assert.Empty(t, stuck, "queued-behind-active sibling must not be flagged as stuck")
}

// TestStartDeleteIgnoresProcessedExistingRequest: an existing request
// in phase=Processed (Velero hasn't GC'd it yet) does not block a
// fresh delete. Without this, a stale Processed request would be
// adopted and the watcher would think it's done, leaving the actual
// (still-present) Backup CR un-deleted.
func TestStartDeleteIgnoresProcessedExistingRequest(t *testing.T) {
	client := veleroDynamicFake()
	primary := newBackup("snap", kipperLabels())
	primary.SetUID("primary-uid-abc")
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))

	stale := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1",
		"kind":       "DeleteBackupRequest",
		"metadata": map[string]interface{}{
			"name":      "snap-delete-old",
			"namespace": VeleroNamespace,
			"labels": map[string]interface{}{
				"velero.io/backup-name": "snap",
				"velero.io/backup-uid":  "primary-uid-abc",
			},
		},
		"spec":   map[string]interface{}{"backupName": "snap"},
		"status": map[string]interface{}{"phase": "Processed"},
	}}
	require.NoError(t, client.Tracker().Create(veleroDeleteRequestGVR, stale, VeleroNamespace))

	inst := fastInstaller(client)
	started, err := inst.StartDelete(context.Background(), "snap")
	require.NoError(t, err)
	assert.NotEqual(t, "snap-delete-old", started.Requests[0],
		"Processed-but-not-yet-GCd requests must not be adopted")
}

// TestStartDeleteSetsVeleroCorrelationLabels: every DeleteBackupRequest
// we create must carry `velero.io/backup-name` (Velero-sanitized) and
// `velero.io/backup-uid` so Velero's deletion controller can dedupe
// in-flight requests for the same backup. Without these, repeated
// kip ai backup delete calls would race instead of being merged.
func TestStartDeleteSetsVeleroCorrelationLabels(t *testing.T) {
	client := veleroDynamicFake()
	primary := newBackup("snap", kipperLabels())
	primary.SetUID("primary-uid-123")
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))

	inst := fastInstaller(client)
	started, err := inst.StartDelete(context.Background(), "snap")
	require.NoError(t, err)
	require.Len(t, started.Requests, 1)

	got, err := client.Tracker().Get(veleroDeleteRequestGVR, VeleroNamespace, started.Requests[0])
	require.NoError(t, err)
	labels := got.(*unstructured.Unstructured).GetLabels()
	assert.Equal(t, "snap", labels["velero.io/backup-name"])
	assert.Equal(t, "primary-uid-123", labels["velero.io/backup-uid"])
}

// TestBoundedDeleteRequestName covers the short-name (passthrough),
// long-name (truncate + hash), and DNS-validity edge cases. We
// validate the result with Kubernetes' own IsDNS1123Subdomain
// function so any subdomain rule (length, charset, label boundaries)
// is checked, not just length.
func TestBoundedDeleteRequestName(t *testing.T) {
	t.Run("short name is preserved as-is", func(t *testing.T) {
		got := boundedDeleteRequestName("snap", 1234567890)
		assert.Equal(t, "snap-delete-1234567890", got)
		assert.Empty(t, validation.IsDNS1123Subdomain(got))
	})
	t.Run("long name is truncated with hash suffix", func(t *testing.T) {
		long := strings.Repeat("x", 230)
		got := boundedDeleteRequestName(long, 1234567890)
		assert.Empty(t, validation.IsDNS1123Subdomain(got), "must be a valid k8s DNS-1123 subdomain")
		// Same input must produce the same name (deterministic hash).
		assert.Equal(t, got, boundedDeleteRequestName(long, 1234567890))
	})
	t.Run("different long names produce different bounded names", func(t *testing.T) {
		a := strings.Repeat("a", 230)
		b := strings.Repeat("a", 230) + "y" // different content
		assert.NotEqual(t, boundedDeleteRequestName(a, 1), boundedDeleteRequestName(b, 1),
			"hash suffix must differentiate distinct long names")
	})
	t.Run("dotted name with dot at truncation boundary stays DNS-valid", func(t *testing.T) {
		// Force a dot at byte index 31 so the naïve truncation
		// `[0:32]` would end in `.`, then concatenating `-<hash>`
		// would produce an empty label between `.` and `-`.
		dotted := strings.Repeat("a", 31) + "." + strings.Repeat("b", 220)
		require.Equal(t, byte('.'), dotted[31], "test fixture must place the boundary-violating dot at byte 31")
		got := boundedDeleteRequestName(dotted, 1234567890)
		assert.Empty(t, validation.IsDNS1123Subdomain(got),
			"truncation must not leave a trailing dot before the hash boundary; got %q", got)
		assert.False(t, strings.Contains(got, ".-"),
			"generated name must not contain the empty-label `.-` boundary; got %q", got)
	})
	t.Run("name that is all non-alphanumeric falls back to synthetic", func(t *testing.T) {
		// Pathological case: backup name made of dots and hyphens.
		// (Not actually a valid backup name, but the helper must not
		// panic or produce an invalid result.)
		got := boundedDeleteRequestName(strings.Repeat("-", 240), 1234567890)
		assert.Empty(t, validation.IsDNS1123Subdomain(got))
		assert.True(t, strings.HasPrefix(got, "dbr-"))
	})
}

// TestStartDeleteAcceptsLongBackupName: a logical backup name long
// enough to push the rendered Kubernetes object names past every
// validation limit (label values capped at 63, metadata.name capped
// at 253) must still produce two valid DeleteBackupRequest CRs.
//
// Earlier iterations failed two ways:
//  1. Embedding the backup name in a `velero.io/backup-name` label
//     silently capped at 63 chars, so the sibling Create failed and
//     orphaned the primary delete.
//  2. The DeleteBackupRequest's metadata.name was `<backupName>-delete-<19-digit-nanos>`,
//     so a >225 char backup name pushed the request name past 253.
//
// boundedDeleteRequestName + dropping the label both fix this.
func TestStartDeleteAcceptsLongBackupName(t *testing.T) {
	client := veleroDynamicFake()
	// 230 chars: longest practical backup name that still fits the
	// k8s 253-char DNS subdomain limit, but the +27-char delete suffix
	// would have blown past it under the old code.
	longName := strings.Repeat("x", 230)
	require.LessOrEqual(t, len(longName+configBackupSuffix), 253, "test setup: backup names themselves must still be valid")

	primary := newBackup(longName, kipperLabels())
	sibling := newBackup(longName+configBackupSuffix, kipperLabels())
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, sibling, VeleroNamespace))

	// Reactor that enforces both limits the real apiserver enforces
	// on Create. If the implementation regresses (re-adds the long
	// label, or drops the bounded request name), the Create call
	// fails and the test catches it.
	client.PrependReactor("create", "deletebackuprequests", func(action k8stesting.Action) (bool, runtime.Object, error) {
		obj := action.(k8stesting.CreateAction).GetObject()
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return false, nil, nil
		}
		if len(u.GetName()) > 253 {
			return true, nil, apierrors.NewInvalid(schema.GroupKind{}, u.GetName(),
				field.ErrorList{field.Invalid(field.NewPath("metadata", "name"), u.GetName(),
					"must be no more than 253 characters")})
		}
		for k, v := range u.GetLabels() {
			if len(v) > 63 {
				return true, nil, apierrors.NewInvalid(schema.GroupKind{}, u.GetName(),
					field.ErrorList{field.Invalid(field.NewPath("metadata", "labels", k), v,
						"must be no more than 63 characters")})
			}
		}
		return false, nil, nil
	})

	inst := fastInstaller(client)
	started, err := inst.StartDelete(context.Background(), longName)
	require.NoError(t, err)
	assert.Len(t, started.Targets, 2, "both primary and sibling must have been queued for deletion")
	assert.Len(t, started.Requests, 2)
	for _, reqName := range started.Requests {
		assert.LessOrEqual(t, len(reqName), 253, "request name %q must respect the k8s DNS subdomain limit", reqName)
	}
}

func TestWatchDeleteForWindowCompletedInTime(t *testing.T) {
	client := veleroDynamicFake()
	deleteOnDeleteRequest(client)

	primary := newBackup("snap", kipperLabels())
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))

	inst := fastInstaller(client)
	started, err := inst.StartDelete(context.Background(), "snap")
	require.NoError(t, err)
	state, failed, err := inst.WatchDeleteForWindow(context.Background(), started, 200*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, DeleteCompleted, state)
	assert.Empty(t, failed)
}

// TestWatchDeleteForWindowFailedInTime: Velero populates
// status.errors on the DeleteBackupRequest when it can't process the
// deletion (e.g. the backup is missing in object storage, or the
// BackupStorageLocation is unreachable). The async warmup must
// surface that as DeleteFailed plus the failing CR name instead of
// waiting full window and reporting DeleteStillRunning.
func TestWatchDeleteForWindowFailedInTime(t *testing.T) {
	client := veleroDynamicFake()
	primary := newBackup("snap", kipperLabels())
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))

	// Reactor: when Velero would normally process the request, mark
	// it with status.errors instead. The Backup CR is left in place
	// to prove the watcher reports failure on validation errors even
	// when the Backup hasn't been deleted yet.
	client.PrependReactor("get", "deletebackuprequests", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		obj, err := client.Tracker().Get(veleroDeleteRequestGVR, getAction.GetNamespace(), getAction.GetName())
		if err != nil {
			return true, nil, err
		}
		u := obj.(*unstructured.Unstructured).DeepCopy()
		_ = unstructured.SetNestedStringSlice(u.Object, []string{"backup not found in object storage"}, "status", "errors")
		return true, u, nil
	})

	inst := fastInstaller(client)
	started, err := inst.StartDelete(context.Background(), "snap")
	require.NoError(t, err)
	state, failed, err := inst.WatchDeleteForWindow(context.Background(), started, 200*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, DeleteFailed, state)
	require.Len(t, failed, 1)
	assert.Equal(t, started.Requests[0], failed[0], "failure list must name the actual failing DeleteBackupRequest CR")
}

// TestWaitDeleteFailsFastOnRequestErrors: the blocking --wait path
// must surface DeleteBackupRequest status.errors as soon as Velero
// writes them, instead of polling allTargetsGone for 30 minutes
// while the request will obviously never succeed. CI scripts that
// use --wait depend on this.
func TestWaitDeleteFailsFastOnRequestErrors(t *testing.T) {
	client := veleroDynamicFake()
	primary := newBackup("snap", kipperLabels())
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))

	client.PrependReactor("get", "deletebackuprequests", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		obj, err := client.Tracker().Get(veleroDeleteRequestGVR, getAction.GetNamespace(), getAction.GetName())
		if err != nil {
			return true, nil, err
		}
		u := obj.(*unstructured.Unstructured).DeepCopy()
		_ = unstructured.SetNestedStringSlice(u.Object, []string{"backup not found in object storage"}, "status", "errors")
		return true, u, nil
	})

	inst := fastInstaller(client)
	started, err := inst.StartDelete(context.Background(), "snap")
	require.NoError(t, err)
	err = inst.WaitDelete(context.Background(), started)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reported errors")
	assert.Contains(t, err.Error(), started.Requests[0])
}

// TestWatchDeleteForWindowSiblingOnlyFailure: when ONLY the sibling
// DeleteBackupRequest reports errors, the failure list must point at
// the sibling's CR (label velero.io/backup-name=<name>-aiconfig), not
// the primary's. Otherwise the CLI's `kubectl describe ...` hint
// sends operators to a request that's making progress.
func TestWatchDeleteForWindowSiblingOnlyFailure(t *testing.T) {
	client := veleroDynamicFake()
	primary := newBackup("snap", kipperLabels())
	sibling := newBackup("snap"+configBackupSuffix, kipperLabels())
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, sibling, VeleroNamespace))

	client.PrependReactor("get", "deletebackuprequests", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		obj, err := client.Tracker().Get(veleroDeleteRequestGVR, getAction.GetNamespace(), getAction.GetName())
		if err != nil {
			return true, nil, err
		}
		u := obj.(*unstructured.Unstructured).DeepCopy()
		// Only mark errors for the sibling-targeting request.
		backupName, _, _ := unstructured.NestedString(u.Object, "spec", "backupName")
		if backupName == "snap"+configBackupSuffix {
			_ = unstructured.SetNestedStringSlice(u.Object, []string{"sibling kopia repo unreachable"}, "status", "errors")
		}
		return true, u, nil
	})

	inst := fastInstaller(client)
	started, err := inst.StartDelete(context.Background(), "snap")
	require.NoError(t, err)
	state, failed, err := inst.WatchDeleteForWindow(context.Background(), started, 200*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, DeleteFailed, state)
	require.Len(t, failed, 1)
	// started.Requests[1] is the sibling's request CR (primary first).
	assert.Equal(t, started.Requests[1], failed[0], "sibling-only failure must name the sibling DeleteBackupRequest, not the primary")
}

// TestWatchDeleteForWindowStillRunningAfterDeadline: an acknowledged
// but slow DeleteBackupRequest (Velero controller is processing the
// Kopia repo's multi-GB metadata) must classify as
// DeleteStillRunning so the CLI prints "running in the background;
// check with kip ai backup list". Differs from the unacknowledged
// case below: status.phase is InProgress here.
func TestWatchDeleteForWindowStillRunningAfterDeadline(t *testing.T) {
	client := veleroDynamicFake()
	primary := newBackup("snap", kipperLabels())
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))

	client.PrependReactor("get", "deletebackuprequests", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		obj, err := client.Tracker().Get(veleroDeleteRequestGVR, getAction.GetNamespace(), getAction.GetName())
		if err != nil {
			return true, nil, err
		}
		u := obj.(*unstructured.Unstructured).DeepCopy()
		_ = unstructured.SetNestedField(u.Object, "InProgress", "status", "phase")
		return true, u, nil
	})

	inst := fastInstaller(client)
	started, err := inst.StartDelete(context.Background(), "snap")
	require.NoError(t, err)
	state, failed, err := inst.WatchDeleteForWindow(context.Background(), started, 50*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, DeleteStillRunning, state)
	assert.Empty(t, failed)
}

// TestWatchDeleteForWindowOrphanRequest: a DeleteBackupRequest gets
// deleted (e.g. someone runs kubectl delete deletebackuprequest)
// while its target Backup is still present. There is no controller
// work pending that will remove the snapshot, so the watcher must
// classify this as DeleteFailed.
func TestWatchDeleteForWindowOrphanRequest(t *testing.T) {
	client := veleroDynamicFake()
	primary := newBackup("snap", kipperLabels())
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))

	inst := fastInstaller(client)
	started, err := inst.StartDelete(context.Background(), "snap")
	require.NoError(t, err)

	// Delete the request after StartDelete created it. Target Backup
	// is intentionally left in place.
	require.NoError(t, client.Tracker().Delete(veleroDeleteRequestGVR, VeleroNamespace, started.Requests[0]))

	state, failed, err := inst.WatchDeleteForWindow(context.Background(), started, 50*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, DeleteFailed, state, "orphaned request (target present, request gone) must classify as DeleteFailed")
	require.Len(t, failed, 1)
	assert.Equal(t, started.Requests[0], failed[0])
}

// TestWaitDeleteFailsAfterAckWindowOnStuckRequest: a DeleteBackupRequest
// stuck in empty/New phase (Velero controller down) must surface as
// an error within the ackWindow on the --wait path, not after the
// full poll timeout.
func TestWaitDeleteFailsAfterAckWindowOnStuckRequest(t *testing.T) {
	client := veleroDynamicFake()
	primary := newBackup("snap", kipperLabels())
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))

	// No reactor: the dynamic fake returns the request as-is, with
	// empty status. Mimics a controller that never observed the
	// request.

	inst := fastInstaller(client)
	inst.AckWindow = 20 * time.Millisecond
	started, err := inst.StartDelete(context.Background(), "snap")
	require.NoError(t, err)

	start := time.Now()
	err = inst.WaitDelete(context.Background(), started)
	elapsed := time.Since(start)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not picked up by Velero")
	assert.Contains(t, err.Error(), started.Requests[0])
	assert.Less(t, elapsed, inst.PollTimeout, "must fail within the ack window, not the full poll timeout")
}

// TestWatchDeleteForWindowUnacknowledgedAfterDeadline: the Velero
// controller is down/stuck, so the DeleteBackupRequest never leaves
// status.phase="New" (or empty). The watcher must classify that as
// DeleteFailed at deadline expiry, not as "running in the background"
// — otherwise the user gets no failure signal and the snapshot data
// stays retained indefinitely.
func TestWatchDeleteForWindowUnacknowledgedAfterDeadline(t *testing.T) {
	client := veleroDynamicFake()
	primary := newBackup("snap", kipperLabels())
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))

	// Reactor leaves status.phase empty: the dynamic fake returns the
	// CR exactly as the tracker has it, and StartDelete creates it
	// with no status. That mimics a Velero controller that never
	// observed the request.

	inst := fastInstaller(client)
	started, err := inst.StartDelete(context.Background(), "snap")
	require.NoError(t, err)
	state, failed, err := inst.WatchDeleteForWindow(context.Background(), started, 50*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, DeleteFailed, state)
	require.Len(t, failed, 1)
	assert.Equal(t, started.Requests[0], failed[0],
		"unacknowledged request name must be surfaced so the operator can describe it")
}

func TestDeleteBackupNotFound(t *testing.T) {
	client := veleroDynamicFake()
	inst := fastInstaller(client)
	err := inst.DeleteBackup(context.Background(), "ghost")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// TestDeleteBackupRetriesSiblingAfterPartialFailure: an earlier delete
// got the primary but failed on the sibling. A retry must clean up the
// orphan, not refuse with "not found".
func TestDeleteBackupRetriesSiblingAfterPartialFailure(t *testing.T) {
	client := veleroDynamicFake()
	deleteOnDeleteRequest(client)

	// Only the sibling remains; primary already gone from a prior run.
	sibling := newBackup("snap"+configBackupSuffix, kipperLabels())
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, sibling, VeleroNamespace))

	inst := fastInstaller(client)
	require.NoError(t, inst.DeleteBackup(context.Background(), "snap"))

	_, err := client.Tracker().Get(veleroBackupGVR, VeleroNamespace, "snap"+configBackupSuffix)
	require.True(t, apierrors.IsNotFound(err), "orphaned sibling should be cleaned by retry")
}

// TestDeleteBackupRefusesForeignSibling: a foreign Velero Backup that
// happens to be named `<owned>-aiconfig` must not be deleted just
// because the primary is Kipper-owned.
func TestDeleteBackupRefusesForeignSibling(t *testing.T) {
	client := veleroDynamicFake()
	deleteOnDeleteRequest(client)

	primary := newBackup("snap", kipperLabels())
	foreignSibling := newBackup("snap"+configBackupSuffix, map[string]string{"created-by": "ops"})
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, foreignSibling, VeleroNamespace))

	createCount := 0
	client.PrependReactor("create", "deletebackuprequests", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		createCount++
		return false, nil, nil
	})

	inst := fastInstaller(client)
	err := inst.DeleteBackup(context.Background(), "snap")
	require.Error(t, err)
	var notOwned *errNotOwnedByKipper
	require.True(t, errors.As(err, &notOwned), "expected errNotOwnedByKipper for foreign sibling, got %T", err)
	assert.Equal(t, 0, createCount, "must not delete anything when the sibling is unowned")

	// Both backups should still exist after the refusal.
	_, err = client.Tracker().Get(veleroBackupGVR, VeleroNamespace, "snap")
	require.NoError(t, err)
	_, err = client.Tracker().Get(veleroBackupGVR, VeleroNamespace, "snap"+configBackupSuffix)
	require.NoError(t, err)
}

func TestRestoreRefusesIfNamespaceExists(t *testing.T) {
	client := veleroDynamicFake()
	mine := newBackupWithStatus("mine", "Completed", 0)
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, mine, VeleroNamespace))

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: Namespace}}
	inst := fastInstaller(client, ns)
	_, err := inst.CreateRestore(context.Background(), "mine")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
	assert.Contains(t, err.Error(), "kip ai uninstall")
}

func TestRestoreRefusesForeignBackup(t *testing.T) {
	client := veleroDynamicFake()
	foreign := newBackup("velero-nightly", map[string]string{"created-by": "ops"})
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, foreign, VeleroNamespace))

	inst := fastInstaller(client)
	_, err := inst.CreateRestore(context.Background(), "velero-nightly")
	require.Error(t, err)
	var notOwned *errNotOwnedByKipper
	require.True(t, errors.As(err, &notOwned), "expected errNotOwnedByKipper, got %T", err)
}

func TestRestoreRefusesSiblingName(t *testing.T) {
	client := veleroDynamicFake()
	inst := fastInstaller(client)
	_, err := inst.CreateRestore(context.Background(), "snap"+configBackupSuffix)
	require.Error(t, err)
	assert.Contains(t, err.Error(), configBackupSuffix)
}

func TestRestoreHappyPathPrimaryOnly(t *testing.T) {
	client := veleroDynamicFake()
	mine := newBackupWithStatus("mine", "Completed", 0)
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, mine, VeleroNamespace))
	markCompletedOnGet(client, veleroRestoreGVR)

	inst := fastInstaller(client)
	summary, err := inst.CreateRestore(context.Background(), "mine")
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Equal(t, "Completed", summary.Phase)
}

// TestCreateBackupAnnotatesSiblingExpectation: the primary records
// whether a sibling was created, so list/restore can distinguish "no
// sibling expected" from "sibling lost".
func TestCreateBackupAnnotatesSiblingExpectation(t *testing.T) {
	for _, tc := range []struct {
		name          string
		seedSecret    bool
		expectAnnoVal string
	}{
		{"sibling created", true, "true"},
		{"no sibling expected", false, "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := veleroDynamicFake()
			markCompletedOnGet(client, veleroBackupGVR)
			var inst *Installer
			if tc.seedSecret {
				inst = fastInstaller(client, installedNamespace(), aiConfigSecret())
			} else {
				inst = fastInstaller(client, installedNamespace())
			}

			_, err := inst.CreateBackup(context.Background(), "annotated")
			require.NoError(t, err)

			got, err := client.Tracker().Get(veleroBackupGVR, VeleroNamespace, "annotated")
			require.NoError(t, err)
			anno := got.(*unstructured.Unstructured).GetAnnotations()
			assert.Equal(t, tc.expectAnnoVal, anno[configSiblingAnnotation])
		})
	}
}

// TestRestoreRefusesOrphanedPrimary: primary annotated as expecting a
// config sibling, but sibling is missing → refuse, do not create
// Restore CRs.
func TestRestoreRefusesOrphanedPrimary(t *testing.T) {
	client := veleroDynamicFake()
	primary := newBackupWithStatus("orphan", "Completed", 0)
	primary.SetAnnotations(map[string]string{configSiblingAnnotation: "true"})
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))

	createCount := 0
	client.PrependReactor("create", "restores", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		createCount++
		return false, nil, nil
	})

	inst := fastInstaller(client)
	_, err := inst.CreateRestore(context.Background(), "orphan")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "incomplete")
	assert.Contains(t, err.Error(), "missing")
	assert.Equal(t, 0, createCount)
}

// TestListBackupsFlagsOrphanedPrimary: a primary that was annotated as
// expecting a sibling but no longer has one shows up as
// PartiallyFailed, so users do not assume it is healthy.
func TestListBackupsFlagsOrphanedPrimary(t *testing.T) {
	client := veleroDynamicFake()
	primary := newBackupWithStatus("orphan", "Completed", 0)
	primary.SetAnnotations(map[string]string{configSiblingAnnotation: "true"})
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))

	inst := fastInstaller(client)
	got, err := inst.ListBackups(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "PartiallyFailed", got[0].Phase)
}

// TestRestoreForcesExistingResourcePolicyUpdate: every Restore CR must
// carry `existingResourcePolicy: update`. The sibling needs it so the
// restored kipper-ai-config Secret overwrites the live one. The
// primary needs it so Velero silently overwrites the kube-root-ca.crt
// ConfigMap that the cluster's RootCAConfigMap controller auto-injects
// between namespace creation and the Velero restore loop, instead of
// emitting a "ConfigMap kube-root-ca.crt already exists" warning that
// inflates the restore's warning count.
func TestRestoreForcesExistingResourcePolicyUpdate(t *testing.T) {
	client := veleroDynamicFake()
	primary := newBackupWithStatus("with-cfg", "Completed", 0)
	primary.SetAnnotations(map[string]string{configSiblingAnnotation: "true"})
	sibling := newBackupWithStatus("with-cfg"+configBackupSuffix, "Completed", 0)
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, sibling, VeleroNamespace))
	markCompletedOnGet(client, veleroRestoreGVR)

	inst := fastInstaller(client)
	_, err := inst.CreateRestore(context.Background(), "with-cfg")
	require.NoError(t, err)

	restores, err := client.Resource(veleroRestoreGVR).Namespace(VeleroNamespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, restores.Items, 2, "expected one primary plus one sibling Restore CR")

	for _, r := range restores.Items {
		backupName, _, _ := unstructured.NestedString(r.Object, "spec", "backupName")
		policy, _, _ := unstructured.NestedString(r.Object, "spec", "existingResourcePolicy")
		assert.Equal(t, "update", policy, "Restore for backup %q must use existingResourcePolicy=update", backupName)
	}
}

// TestRestoreSummaryReadsItemsRestored: Velero writes the processed
// counter on Restore CRs as `itemsRestored`, not `itemsBackedUp`. The
// summariser must pick the right field by kind so the CLI does not
// print a misleading "0 items" line on a successful restore.
func TestRestoreSummaryReadsItemsRestored(t *testing.T) {
	client := veleroDynamicFake()
	primary := newBackupWithStatus("counts-mine", "Completed", 0)
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))
	markCompletedOnGet(client, veleroRestoreGVR)

	inst := fastInstaller(client)
	summary, err := inst.CreateRestore(context.Background(), "counts-mine")
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Equal(t, int64(7), summary.ItemsBackedUp, "Restore item count must come from itemsRestored, not itemsBackedUp")
}

// TestRestoreLabelsBackupNameAndUID: every Restore CR must carry the
// `velero.io/backup-name` (passed through GetValidName truncation) and
// `velero.io/backup-uid` labels so `velero restore describe` and
// upstream label selectors can find it.
func TestRestoreLabelsBackupNameAndUID(t *testing.T) {
	client := veleroDynamicFake()
	primary := newBackupWithStatus("labels-mine", "Completed", 0)
	primary.SetUID("primary-uid-1234")
	primary.SetAnnotations(map[string]string{configSiblingAnnotation: "true"})
	sibling := newBackupWithStatus("labels-mine"+configBackupSuffix, "Completed", 0)
	sibling.SetUID("sibling-uid-5678")
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, sibling, VeleroNamespace))
	markCompletedOnGet(client, veleroRestoreGVR)

	inst := fastInstaller(client)
	_, err := inst.CreateRestore(context.Background(), "labels-mine")
	require.NoError(t, err)

	restores, err := client.Resource(veleroRestoreGVR).Namespace(VeleroNamespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, restores.Items, 2, "expected one primary plus one sibling Restore CR")

	for _, r := range restores.Items {
		labels := r.GetLabels()
		backupName, _, _ := unstructured.NestedString(r.Object, "spec", "backupName")

		assert.Equal(t, backupName, labels["velero.io/backup-name"], "Restore for %s should label backup-name", backupName)
		switch backupName {
		case "labels-mine":
			assert.Equal(t, "primary-uid-1234", labels["velero.io/backup-uid"])
		case "labels-mine" + configBackupSuffix:
			assert.Equal(t, "sibling-uid-5678", labels["velero.io/backup-uid"])
		default:
			t.Fatalf("unexpected restore for backup %q", backupName)
		}
	}
}

// TestRestoreLabelsTruncateLongBackupName: a backup name longer than 63
// characters must be truncated using Velero's GetValidName algorithm
// (57-char prefix + 6 hex sha256). Anything else and Velero's own
// selectors miss the Restore CR.
func TestRestoreLabelsTruncateLongBackupName(t *testing.T) {
	longName := strings.Repeat("a", 80)
	client := veleroDynamicFake()
	primary := newBackupWithStatus(longName, "Completed", 0)
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))
	markCompletedOnGet(client, veleroRestoreGVR)

	inst := fastInstaller(client)
	_, err := inst.CreateRestore(context.Background(), longName)
	require.NoError(t, err)

	restores, err := client.Resource(veleroRestoreGVR).Namespace(VeleroNamespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, restores.Items, 1)

	got := restores.Items[0].GetLabels()["velero.io/backup-name"]
	assert.Len(t, got, 63, "long backup names truncate to 63 chars")
	assert.Equal(t, getValidVeleroLabel(longName), got)
}

// TestListBackupsFlagsZeroItemPrimary: a stale Completed-but-empty
// Backup CR (left over from a failed create) must not present as
// healthy in the listing. The restore path also rejects it.
func TestListBackupsFlagsZeroItemPrimary(t *testing.T) {
	client := veleroDynamicFake()
	primary := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1",
		"kind":       "Backup",
		"metadata": map[string]interface{}{
			"name":              "stale",
			"namespace":         VeleroNamespace,
			"creationTimestamp": time.Now().UTC().Format(time.RFC3339),
			"labels": map[string]interface{}{
				managedByLabel:              managedByValue,
				"app.kubernetes.io/part-of": "kipper-ai",
			},
		},
		"status": map[string]interface{}{
			"phase":    "Completed",
			"progress": map[string]interface{}{"itemsBackedUp": int64(0)},
		},
	}}
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))

	inst := fastInstaller(client)
	got, err := inst.ListBackups(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "PartiallyFailed", got[0].Phase, "zero-item primary should not list as Completed")
}

// TestRestoreRefusesZeroItemBackup: a leftover Completed-but-empty
// Backup must be refused at restore time, not silently replayed.
func TestRestoreRefusesZeroItemBackup(t *testing.T) {
	client := veleroDynamicFake()
	primary := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1",
		"kind":       "Backup",
		"metadata": map[string]interface{}{
			"name":              "stale",
			"namespace":         VeleroNamespace,
			"creationTimestamp": time.Now().UTC().Format(time.RFC3339),
			"labels": map[string]interface{}{
				managedByLabel:              managedByValue,
				"app.kubernetes.io/part-of": "kipper-ai",
			},
		},
		"status": map[string]interface{}{
			"phase":    "Completed",
			"progress": map[string]interface{}{"itemsBackedUp": int64(0)},
		},
	}}
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))

	createCount := 0
	client.PrependReactor("create", "restores", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		createCount++
		return false, nil, nil
	})

	inst := fastInstaller(client)
	_, err := inst.CreateRestore(context.Background(), "stale")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "zero items")
	assert.Equal(t, 0, createCount)
}

// TestRestoreWaitsForTerminatingNamespace: the namespace is mid-deletion
// (Phase=Terminating) when restore starts; restore should wait for it
// to disappear, not refuse with "already exists". `kip ai uninstall`
// returns immediately after issuing the Delete, so a chained
// uninstall→restore must not race.
func TestRestoreWaitsForTerminatingNamespace(t *testing.T) {
	client := veleroDynamicFake()
	mine := newBackupWithStatus("mine", "Completed", 0)
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, mine, VeleroNamespace))
	markCompletedOnGet(client, veleroRestoreGVR)

	terminating := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: Namespace},
		Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceTerminating},
	}
	inst := fastInstaller(client, terminating)

	// Simulate the namespace finishing termination after a few ticks.
	getCount := 0
	inst.Clientset.(*fake.Clientset).PrependReactor("get", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) { //nolint:staticcheck
		getCount++
		if getCount > 2 {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, Namespace)
		}
		return false, nil, nil
	})

	_, err := inst.CreateRestore(context.Background(), "mine")
	require.NoError(t, err, "restore should wait for terminating namespace to finish")
}

// TestRestoreRefusesActiveNamespace: an Active (not Terminating)
// namespace means the bundle is still installed; refuse immediately.
func TestRestoreRefusesActiveNamespace(t *testing.T) {
	client := veleroDynamicFake()
	mine := newBackupWithStatus("mine", "Completed", 0)
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, mine, VeleroNamespace))

	active := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: Namespace},
		Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
	}
	inst := fastInstaller(client, active)
	_, err := inst.CreateRestore(context.Background(), "mine")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

// TestRestoreRefusesNonCompletedBackup: an InProgress / Failed /
// PartiallyFailed primary must be refused before any Restore CR is
// created. Restoring from an unfinished or unhealthy backup leaves
// the AI bundle half-rehydrated.
func TestRestoreRefusesNonCompletedBackup(t *testing.T) {
	client := veleroDynamicFake()
	primary := newBackupWithStatus("mine", "InProgress", 0)
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))

	createCount := 0
	client.PrependReactor("create", "restores", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		createCount++
		return false, nil, nil
	})

	inst := fastInstaller(client)
	_, err := inst.CreateRestore(context.Background(), "mine")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InProgress")
	assert.Equal(t, 0, createCount)
}

// TestRestoreRefusesUnhealthyCompletedBackup: a Completed backup with
// recorded errors must also be refused, since some resources did not
// snapshot cleanly.
func TestRestoreRefusesUnhealthyCompletedBackup(t *testing.T) {
	client := veleroDynamicFake()
	primary := newBackupWithStatus("mine", "Completed", 3)
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))

	inst := fastInstaller(client)
	_, err := inst.CreateRestore(context.Background(), "mine")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "3 errors")
}

// TestRestoreRefusesUnhealthySibling: even a clean primary must not
// be restored if the cross-namespace config backup is unhealthy.
// Otherwise restore reports success while AI client config is missing
// or partial.
func TestRestoreRefusesUnhealthySibling(t *testing.T) {
	client := veleroDynamicFake()
	primary := newBackupWithStatus("mine", "Completed", 0)
	sibling := newBackupWithStatus("mine"+configBackupSuffix, "Failed", 1)
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, sibling, VeleroNamespace))

	inst := fastInstaller(client)
	_, err := inst.CreateRestore(context.Background(), "mine")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Failed")
}

// TestRestoreRefusesForeignSibling: a foreign Velero Backup named
// `<owned>-aiconfig` must not be restored just because the primary is
// Kipper-owned. Ownership is validated before any Restore CR is
// created.
func TestRestoreRefusesForeignSibling(t *testing.T) {
	client := veleroDynamicFake()
	primary := newBackupWithStatus("mine", "Completed", 0)
	foreignSibling := newBackup("mine"+configBackupSuffix, map[string]string{"created-by": "ops"})
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, primary, VeleroNamespace))
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, foreignSibling, VeleroNamespace))

	createCount := 0
	client.PrependReactor("create", "restores", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		createCount++
		return false, nil, nil
	})

	inst := fastInstaller(client)
	_, err := inst.CreateRestore(context.Background(), "mine")
	require.Error(t, err)
	var notOwned *errNotOwnedByKipper
	require.True(t, errors.As(err, &notOwned), "expected errNotOwnedByKipper for foreign sibling, got %T", err)
	assert.Equal(t, 0, createCount, "must not create any Restore CRs when sibling is unowned")
}

func TestRestoreHappyPathWithSibling(t *testing.T) {
	client := veleroDynamicFake()
	mine := newBackupWithStatus("mine", "Completed", 0)
	sibling := newBackupWithStatus("mine"+configBackupSuffix, "Completed", 0)
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, mine, VeleroNamespace))
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, sibling, VeleroNamespace))
	markCompletedOnGet(client, veleroRestoreGVR)

	inst := fastInstaller(client)
	summary, err := inst.CreateRestore(context.Background(), "mine")
	require.NoError(t, err)
	require.NotNil(t, summary)
	// Primary + sibling each contribute 7 items via the marker reactor.
	assert.Equal(t, int64(14), summary.ItemsBackedUp)
}

func TestRestoreRequiresName(t *testing.T) {
	client := veleroDynamicFake()
	inst := fastInstaller(client)
	_, err := inst.CreateRestore(context.Background(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "required")
}

// TestRestoreCompletedWithErrorsIsFailure: Velero documents that a
// Restore can reach phase Completed while individual resources fail
// to apply. The CLI must not print a success checkmark over that
// state.
func TestRestoreCompletedWithErrorsIsFailure(t *testing.T) {
	client := veleroDynamicFake()
	mine := newBackupWithStatus("mine", "Completed", 0)
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, mine, VeleroNamespace))

	client.PrependReactor("get", "restores", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		obj, err := client.Tracker().Get(veleroRestoreGVR, getAction.GetNamespace(), getAction.GetName())
		if err != nil {
			return true, nil, err
		}
		u := obj.(*unstructured.Unstructured).DeepCopy()
		_ = unstructured.SetNestedField(u.Object, "Completed", "status", "phase")
		_ = unstructured.SetNestedField(u.Object, int64(2), "status", "errors")
		return true, u, nil
	})

	inst := fastInstaller(client)
	_, err := inst.CreateRestore(context.Background(), "mine")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "completed with 2 errors")
	assert.Contains(t, err.Error(), "describe restore")
}

func kipperLabels() map[string]string {
	return map[string]string{
		managedByLabel:              managedByValue,
		"app.kubernetes.io/part-of": "kipper-ai",
	}
}

// newBackupWithStatus produces a Kipper-owned Velero Backup whose
// status.phase, status.errors, and items-backed-up are pre-populated,
// simulating Velero having already reconciled it. A Completed backup
// always lands with at least one item, since the health check refuses
// zero-item Completed backups.
func newBackupWithStatus(name, phase string, errs int64) *unstructured.Unstructured {
	u := newBackup(name, kipperLabels())
	_ = unstructured.SetNestedField(u.Object, phase, "status", "phase")
	if errs > 0 {
		_ = unstructured.SetNestedField(u.Object, errs, "status", "errors")
	}
	if phase == "Completed" {
		_ = unstructured.SetNestedField(u.Object, int64(7), "status", "progress", "itemsBackedUp")
	}
	return u
}

// newBackup builds an unstructured Velero Backup with the given labels
// and a fresh creationTimestamp.
func newBackup(name string, labels map[string]string) *unstructured.Unstructured {
	labelMap := make(map[string]interface{}, len(labels))
	for k, v := range labels {
		labelMap[k] = v
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1",
		"kind":       "Backup",
		"metadata": map[string]interface{}{
			"name":              name,
			"namespace":         VeleroNamespace,
			"creationTimestamp": time.Now().UTC().Format(time.RFC3339),
			"labels":            labelMap,
		},
	}}
}

// newBackupAt builds a Kipper-owned Velero Backup with a specific
// creationTimestamp so list-ordering tests have predictable input.
func newBackupAt(name string, created time.Time) *unstructured.Unstructured {
	u := newBackup(name, kipperLabels())
	_ = unstructured.SetNestedField(u.Object, created.UTC().Format(time.RFC3339), "metadata", "creationTimestamp")
	return u
}
