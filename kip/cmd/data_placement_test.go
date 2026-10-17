package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/k8s"
	"github.com/getkipper/kipper/kip/internal/manifest"
	"github.com/getkipper/kipper/kip/internal/service"
)

func TestSingleCopyPlacementReportNamesWhatChangedAndWhatIsStillOffTheServer(t *testing.T) {
	var out bytes.Buffer
	reportSingleCopyPlacement(&out, installer.SingleCopyPlacement{
		Pinned:    []string{"shop/data-db-0", "kipper-system/zot-data"},
		Unpinned:  []string{"shop/data-cache-0: refused"},
		OffServer: []installer.OffServerCopy{{Volume: "shop/data-db-0", Node: "worker-2", Pinned: true}},
	})
	printed := out.String()
	assert.Contains(t, printed, "2 single-copy volumes pinned to the server")
	assert.Contains(t, printed, "shop/data-cache-0: refused")
	assert.Contains(t, printed, "Single-copy volumes off the server: shop/data-db-0 (on worker-2).")

	out.Reset()
	reportSingleCopyPlacement(&out, installer.SingleCopyPlacement{})
	assert.Empty(t, out.String(), "nothing to say on a cluster that was already pinned")
}

func TestJoinSummarySaysWhereTheDataLives(t *testing.T) {
	text := strings.Join(strings.Fields(joinDataSummary), " ")
	assert.Contains(t, text, "Kipper configures database, registry and in-cluster backup volumes to keep one copy on the server.")
	assert.Contains(t, text, "With volume data on the server, worker loss preserves that data")
	assert.Contains(t, text, "kip service info <name>")
}

func TestInClusterBackupsWarningAppliesOnlyToTheBundledMinIO(t *testing.T) {
	assert.True(t, backupsInCluster(nil))
	assert.True(t, backupsInCluster(&config.BackupStorageRef{Mode: "in-cluster"}))
	assert.True(t, backupsInCluster(&config.BackupStorageRef{}))
	assert.False(t, backupsInCluster(&config.BackupStorageRef{Mode: "external", Bucket: "b"}))
	assert.Contains(t, inClusterBackupWarning, "a lost server disk takes the backups with it")
}

// placementRunner answers the storage reads kip status makes over SSH.
type placementRunner struct{ replicas, selector string }

func (p placementRunner) Run(cmd string) (string, error) {
	switch {
	case strings.Contains(cmd, "get nodes -l"):
		return "node/server-1\n", nil
	case strings.Contains(cmd, "get pv "):
		return "longhorn-single\tpvc-db\n", nil
	case strings.Contains(cmd, "get volumes.longhorn.io"):
		return "pvc-db\t" + p.selector + "\tshop\tdata-db-0\n", nil
	case strings.Contains(cmd, "replicas.longhorn.io"):
		return p.replicas, nil
	}
	return "", errors.New("unexpected command " + cmd)
}

func (p placementRunner) RunStdin(cmd string, _ io.Reader) (string, error) { return p.Run(cmd) }

func TestStatusReportsDataPlacementOnlyOnClustersWithWorkers(t *testing.T) {
	two := []k8s.NodeInfo{{Name: "server-1"}, {Name: "worker-2"}}
	offServer := placementRunner{replicas: "pvc-db\tworker-2\t2026-10-16T01:00:00Z\t\n", selector: `["kipper-server"]`}

	var out bytes.Buffer
	reportDataPlacement(&out, offServer, &config.Cluster{}, two)
	assert.Contains(t, out.String(), "Single-copy volumes off the server: shop/data-db-0 (on worker-2).")
	assert.Contains(t, out.String(), "a lost server disk takes the backups with it")

	out.Reset()
	reportDataPlacement(&out, placementRunner{replicas: "pvc-db\tserver-1\t2026-10-16T01:00:00Z\t\n"}, &config.Cluster{BackupStorage: &config.BackupStorageRef{Mode: "external", Bucket: "b"}}, two)
	assert.Empty(t, out.String(), "data on the server with external backups needs no section")

	out.Reset()
	reportDataPlacement(&out, offServer, &config.Cluster{}, two[:1])
	assert.Empty(t, out.String(), "a single-node cluster keeps everything on the server")
}

func TestServiceInfoShowsWhereTheServiceRunsAndWhereItsDataIs(t *testing.T) {
	replica := func(name, node, failedAt string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "longhorn.io/v1beta2",
			"kind":       "Replica",
			"metadata":   map[string]interface{}{"name": name, "namespace": "longhorn-system", "labels": map[string]interface{}{"longhornvolume": "pvc-db"}},
			"spec":       map[string]interface{}{"nodeID": node, "healthyAt": "2026-10-16T01:00:00Z", "failedAt": failedAt},
		}}
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{manifest.ServiceGVR: "ServiceList", service.LonghornReplicaGVR: "ReplicaList"},
		healthyServiceCR("db", "postgres"),
		replica("pvc-db-r-1", "server-1", ""),
		replica("pvc-db-r-2", "worker-1", "2026-10-16T02:00:00Z"),
	)
	client := k8sfake.NewSimpleClientset( //nolint:staticcheck
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: "default"},
			Data:       map[string][]byte{"HOST": []byte("db.default.svc.cluster.local"), "PORT": []byte("5432")},
		},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "db-0", Namespace: "default"}, Spec: corev1.PodSpec{NodeName: "worker-2"}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-db-0", Namespace: "default"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "restored-db"}},
		&corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "restored-db"}, Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "driver.longhorn.io", VolumeHandle: "pvc-db"}},
		}},
	)
	mgr := &service.Manager{Client: client, Dynamic: dyn}

	var out bytes.Buffer
	require.NoError(t, writeServiceInfo(context.Background(), &out, mgr, "default", "db"))
	assert.Contains(t, out.String(), "Pod node:  worker-2")
	assert.Contains(t, out.String(), "Data node: server-1 (1 copy)", "a failed replica is not a copy, and replicas are found by the PV's volume handle")
}

func TestStatusDoesNotPromiseAMoveForAnUnpinnedCopy(t *testing.T) {
	var out bytes.Buffer
	reportDataPlacement(&out, placementRunner{replicas: "pvc-db\tworker-2\t2026-10-16T01:00:00Z\t\n"}, &config.Cluster{}, []k8s.NodeInfo{{Name: "server-1"}, {Name: "worker-2"}})
	assert.Contains(t, out.String(), "Single-copy volumes not pinned to the server: shop/data-db-0 (on worker-2).")
}

type brokenLonghorn struct{}

func (brokenLonghorn) Run(string) (string, error) {
	return "Internal error occurred: failed calling webhook", errors.New("exit status 1")
}

func (b brokenLonghorn) RunStdin(cmd string, _ io.Reader) (string, error) { return b.Run(cmd) }

func TestAPinFailureDoesNotStopTheUpgrade(t *testing.T) {
	var out bytes.Buffer
	pinForUpgrade(&out, brokenLonghorn{})
	assert.Contains(t, out.String(), "could not keep single-copy volumes on the server")
	assert.Contains(t, out.String(), "The upgrade continues")
}

func TestUpgradeHelpNamesTheStorageStepThatSkipSystemRuns(t *testing.T) {
	assert.Contains(t, upgradeCmd.Flags().Lookup("skip-system").Usage, "pin single-copy volumes to the server")
}

func TestTheBackupWarningPromisesNoMigration(t *testing.T) {
	assert.NotContains(t, inClusterBackupWarning, "migration")
	assert.Contains(t, inClusterBackupWarning, "External backup storage is chosen at install time")
}

type failOpenHost struct{ marker string }

func (f failOpenHost) Run(cmd string) (string, error) {
	if strings.Contains(cmd, "ufw-failopen") {
		return f.marker, nil
	}
	return "", nil
}

func (f failOpenHost) RunStdin(cmd string, _ io.Reader) (string, error) { return f.Run(cmd) }

func TestStatusReportsAFirewallRollbackThatSwitchedUFWOff(t *testing.T) {
	var out bytes.Buffer
	reportUFWFailOpen(&out, failOpenHost{marker: "abc123 2026-10-16T12:00:00Z\n"})
	assert.Contains(t, out.String(), "run abc123, 2026-10-16T12:00:00Z")
	assert.Contains(t, out.String(), "Run 'kip cluster harden' to turn it back on")

	out.Reset()
	reportUFWFailOpen(&out, failOpenHost{})
	assert.Empty(t, out.String())
}

func TestWritePlacementSaysWhyAPodIsNotScheduled(t *testing.T) {
	var out bytes.Buffer
	writePlacement(&out, service.Placement{Unscheduled: "0/1 nodes are available: 1 Insufficient cpu."}, nil)
	assert.Contains(t, out.String(), "Pod node:  not scheduled (0/1 nodes are available: 1 Insufficient cpu.)")

	out.Reset()
	writePlacement(&out, service.Placement{}, nil)
	assert.Contains(t, out.String(), "Pod node:  not scheduled\n", "no reason yet, nothing invented")
}

// localStorageHost answers the storage-mode read with local storage and records
// every other command.
type localStorageHost struct{ others []string }

func (h *localStorageHost) Run(cmd string) (string, error) {
	if strings.Contains(cmd, "platformconfig platform") && strings.Contains(cmd, "spec.storage") {
		return "local", nil
	}
	h.others = append(h.others, cmd)
	return "", nil
}

func (h *localStorageHost) RunStdin(cmd string, _ io.Reader) (string, error) { return h.Run(cmd) }

func TestUpgradePinsNothingOnLocalStorage(t *testing.T) {
	var out bytes.Buffer
	host := &localStorageHost{}
	pinForUpgrade(&out, host)
	assert.Empty(t, host.others, "a local-storage cluster has no Longhorn volumes to pin")
	assert.Empty(t, out.String())
}
