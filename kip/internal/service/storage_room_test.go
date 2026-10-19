package service

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"
)

// A server disk observed in a live cluster: 40424828928 bytes, 30% reserved for
// the default disk and 27917287424 bytes already promised to volumes. Longhorn
// accepts (max - reserved) * 100% - scheduled = 380092826 bytes, about 362Mi.
const (
	liveMaximum   int64 = 40424828928
	liveReserved  int64 = 12127448678
	liveScheduled int64 = 27917287424
)

type testDisk struct {
	name                                    string
	maximum, reserved, scheduled, available int64
	schedulable, reason                     string
	allowScheduling, evictionRequested      bool
	diskType                                string
}

func roomyDisk(name string, maximum, reserved, scheduled int64) testDisk {
	return testDisk{name: name, maximum: maximum, reserved: reserved, scheduled: scheduled, available: maximum / 2,
		schedulable: "True", allowScheduling: true}
}

type testNode struct {
	name            string
	tags            []string
	ready           string
	schedulable     string
	allowScheduling bool
	disks           []testDisk
}

func longhornNodeObject(n testNode) *unstructured.Unstructured {
	specDisks := map[string]interface{}{}
	statusDisks := map[string]interface{}{}
	for _, d := range n.disks {
		diskType := d.diskType
		if diskType == "" {
			diskType = "filesystem"
		}
		specDisks[d.name] = map[string]interface{}{
			"allowScheduling":   d.allowScheduling,
			"evictionRequested": d.evictionRequested,
			"storageReserved":   d.reserved,
			"diskType":          diskType,
			"path":              "/var/lib/longhorn/",
		}
		statusDisks[d.name] = map[string]interface{}{
			"storageMaximum":   d.maximum,
			"storageScheduled": d.scheduled,
			"storageAvailable": d.available,
			"conditions": []interface{}{
				map[string]interface{}{"type": "Ready", "status": "True"},
				map[string]interface{}{"type": "Schedulable", "status": d.schedulable, "reason": d.reason},
			},
		}
	}
	tags := make([]interface{}, len(n.tags))
	for i, t := range n.tags {
		tags[i] = t
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "longhorn.io/v1beta2",
		"kind":       "Node",
		"metadata":   map[string]interface{}{"name": n.name, "namespace": "longhorn-system"},
		"spec":       map[string]interface{}{"allowScheduling": n.allowScheduling, "tags": tags, "disks": specDisks},
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{"type": "Ready", "status": n.ready},
				map[string]interface{}{"type": "Schedulable", "status": n.schedulable},
			},
			"diskStatus": statusDisks,
		},
	}}
}

func healthyNode(name string, tags []string, disks ...testDisk) testNode {
	return testNode{name: name, tags: tags, ready: "True", schedulable: "True", allowScheduling: true, disks: disks}
}

func longhornSetting(name, value string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "longhorn.io/v1beta2",
		"kind":       "Setting",
		"metadata":   map[string]interface{}{"name": name, "namespace": "longhorn-system"},
		"value":      value,
	}}
}

func localPlatformConfig() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "PlatformConfig",
		"metadata":   map[string]interface{}{"name": "platform"},
		"spec":       map[string]interface{}{"storage": "local"},
	}}
}

func storageClass(name, provisioner string, params map[string]string) *storagev1.StorageClass {
	return &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Provisioner: provisioner, Parameters: params}
}

// kipperClasses are the classes a Longhorn cluster has: single copies pinned to
// the server and the shared class with three copies on any node.
func kipperClasses() *fake.Clientset {
	return fake.NewSimpleClientset( //nolint:staticcheck
		storageClass("longhorn-single", "driver.longhorn.io", map[string]string{"numberOfReplicas": "1", "nodeSelector": "kipper-server"}),
		storageClass("longhorn", "driver.longhorn.io", map[string]string{"numberOfReplicas": "3"}),
		storageClass("local-path", "rancher.io/local-path", nil),
	)
}

func storageManager(clientset *fake.Clientset, objects ...runtime.Object) *Manager {
	return &Manager{Client: clientset, Dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{longhornNodeGVR: "NodeList"}, objects...)}
}

func defaultSettings() []runtime.Object {
	return []runtime.Object{
		longhornSetting("storage-over-provisioning-percentage", "100"),
		longhornSetting("storage-minimal-available-percentage", "25"),
	}
}

func longhornCluster(nodes ...testNode) *Manager {
	objects := defaultSettings()
	for _, n := range nodes {
		objects = append(objects, longhornNodeObject(n))
	}
	return storageManager(kipperClasses(), objects...)
}

func postgresVolumes(t *testing.T, storage string) []NewVolume {
	t.Helper()
	volumes, err := ServiceVolumes("default", "db", "postgres", storage, true)
	require.NoError(t, err)
	return volumes
}

func TestStorageRoomRefusesAServiceTheServerDiskCannotSchedule(t *testing.T) {
	mgr := longhornCluster(healthyNode("server-1", []string{"kipper-server"}, roomyDisk("default-disk", liveMaximum, liveReserved, liveScheduled)))

	warning, err := mgr.CheckStorageRoom(context.Background(), postgresVolumes(t, "1Gi"), "--storage")

	require.Error(t, err)
	assert.Empty(t, warning)
	for _, want := range []string{"server-1", "362Mi", "2Gi", "data 1Gi", "dump 1Gi", "kip service delete", "--delete-data", "grow", "--storage"} {
		assert.Contains(t, err.Error(), want)
	}
}

func TestStorageRoomAcceptsAServiceThatFits(t *testing.T) {
	mgr := longhornCluster(healthyNode("server-1", []string{"kipper-server"}, roomyDisk("default-disk", liveMaximum, liveReserved, 0)))

	warning, err := mgr.CheckStorageRoom(context.Background(), postgresVolumes(t, "5Gi"), "--storage")

	require.NoError(t, err)
	assert.Empty(t, warning)
}

// Each volume alone fits in 1.5Gi, both together do not, and Longhorn places
// both on the one server disk.
func TestStorageRoomCountsEveryVolumeTheCommandCreates(t *testing.T) {
	maximum, reserved := gi(10), gi(3)
	scheduled := maximum - reserved - gi(1.5)
	mgr := longhornCluster(healthyNode("server-1", []string{"kipper-server"}, roomyDisk("default-disk", maximum, reserved, scheduled)))

	_, err := mgr.CheckStorageRoom(context.Background(), postgresVolumes(t, "1Gi"), "--storage")
	require.Error(t, err)

	redis, err := ServiceVolumes("default", "cache", "redis", "1Gi", false)
	require.NoError(t, err)
	_, err = mgr.CheckStorageRoom(context.Background(), redis, "--storage")
	require.NoError(t, err)
}

func TestStorageRoomPlacesSingleCopiesOnTheServerOnly(t *testing.T) {
	mgr := longhornCluster(
		healthyNode("server-1", []string{"kipper-server"}, roomyDisk("default-disk", liveMaximum, liveReserved, liveScheduled)),
		healthyNode("worker-1", nil, roomyDisk("default-disk", gi(500), gi(150), 0)),
	)

	_, err := mgr.CheckStorageRoom(context.Background(), postgresVolumes(t, "1Gi"), "--storage")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "server-1")
	assert.NotContains(t, err.Error(), "worker-1")
}

func TestStorageRoomLetsASharedVolumeUseAnyNode(t *testing.T) {
	mgr := longhornCluster(
		healthyNode("server-1", []string{"kipper-server"}, roomyDisk("default-disk", liveMaximum, liveReserved, liveScheduled)),
		healthyNode("worker-1", nil, roomyDisk("default-disk", gi(500), gi(150), 0)),
	)

	_, err := mgr.CheckStorageRoom(context.Background(), []NewVolume{{Name: "uploads", Shared: true, Bytes: gi(5)}}, "--size")

	require.NoError(t, err)
}

func TestStorageRoomRefusesASharedVolumeNoNodeCanHold(t *testing.T) {
	mgr := longhornCluster(
		healthyNode("server-1", []string{"kipper-server"}, roomyDisk("default-disk", gi(40), gi(12), gi(27))),
		healthyNode("worker-1", nil, roomyDisk("default-disk", gi(40), gi(12), gi(26))),
	)

	_, err := mgr.CheckStorageRoom(context.Background(), []NewVolume{{Name: "uploads", Shared: true, Bytes: gi(5)}}, "--size")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "worker-1", "names the node with the most room")
	assert.Contains(t, err.Error(), "--size")
}

// A disk at or under the minimal-available share takes nothing, however little
// is scheduled on it.
func TestStorageRoomHonorsTheMinimalAvailableSetting(t *testing.T) {
	disk := roomyDisk("default-disk", gi(40), gi(12), 0)
	disk.available = gi(10)
	disk.schedulable, disk.reason = "False", "DiskPressure"
	mgr := longhornCluster(healthyNode("server-1", []string{"kipper-server"}, disk))

	_, err := mgr.CheckStorageRoom(context.Background(), postgresVolumes(t, "1Gi"), "--storage")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "0Mi")
}

func TestStorageRoomHonorsOverProvisioning(t *testing.T) {
	objects := []runtime.Object{
		longhornSetting("storage-over-provisioning-percentage", "200"),
		longhornSetting("storage-minimal-available-percentage", "25"),
		longhornNodeObject(healthyNode("server-1", []string{"kipper-server"}, roomyDisk("default-disk", liveMaximum, liveReserved, liveScheduled))),
	}
	mgr := storageManager(kipperClasses(), objects...)

	_, err := mgr.CheckStorageRoom(context.Background(), postgresVolumes(t, "5Gi"), "--storage")

	require.NoError(t, err, "twice the 26.3Gi left after the reserve, minus what is scheduled, leaves room for two 5Gi volumes")
}

func TestStorageRoomUsesAnyDiskOnTheServer(t *testing.T) {
	mgr := longhornCluster(healthyNode("server-1", []string{"kipper-server"},
		roomyDisk("default-disk", liveMaximum, liveReserved, liveScheduled),
		roomyDisk("extra-disk", gi(100), 0, 0),
	))

	_, err := mgr.CheckStorageRoom(context.Background(), postgresVolumes(t, "5Gi"), "--storage")

	require.NoError(t, err)
}

func TestStorageRoomSkipsClustersOnLocalStorage(t *testing.T) {
	mgr := storageManager(kipperClasses(), localPlatformConfig())

	warning, err := mgr.CheckStorageRoom(context.Background(), postgresVolumes(t, "1Gi"), "--storage")

	require.NoError(t, err)
	assert.Empty(t, warning)
}

func TestStorageRoomWarnsWhenLonghornCannotBeRead(t *testing.T) {
	mgr := longhornCluster(healthyNode("server-1", []string{"kipper-server"}, roomyDisk("default-disk", liveMaximum, liveReserved, liveScheduled)))
	mgr.Dynamic.(*dynamicfake.FakeDynamicClient).PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("nodes.longhorn.io is forbidden")
	})

	warning, err := mgr.CheckStorageRoom(context.Background(), postgresVolumes(t, "1Gi"), "--storage")

	require.NoError(t, err)
	assert.Contains(t, warning, "forbidden")
}

func TestStorageRoomWarnsWhenASettingIsMissing(t *testing.T) {
	mgr := storageManager(kipperClasses(), longhornNodeObject(healthyNode("server-1", []string{"kipper-server"}, roomyDisk("default-disk", liveMaximum, liveReserved, liveScheduled))))

	warning, err := mgr.CheckStorageRoom(context.Background(), postgresVolumes(t, "1Gi"), "--storage")

	require.NoError(t, err)
	assert.Contains(t, warning, "storage-over-provisioning-percentage")
}

// A down or cordoned server prevents placement, so the check warns and allows
// the command to proceed. Only a definite capacity shortage causes refusal.
func TestStorageRoomWarnsWhenNoNodeIsTakingReplicas(t *testing.T) {
	notReady := healthyNode("server-1", []string{"kipper-server"}, roomyDisk("default-disk", liveMaximum, liveReserved, liveScheduled))
	notReady.ready = "False"
	diskDown := roomyDisk("default-disk", liveMaximum, liveReserved, liveScheduled)
	diskDown.schedulable, diskDown.reason = "False", "DiskNotReady"
	cordoned := healthyNode("server-1", []string{"kipper-server"}, roomyDisk("default-disk", liveMaximum, liveReserved, liveScheduled))
	cordoned.schedulable = "False"
	noScheduling := healthyNode("server-1", []string{"kipper-server"}, roomyDisk("default-disk", liveMaximum, liveReserved, liveScheduled))
	noScheduling.allowScheduling = false
	evicting := roomyDisk("default-disk", liveMaximum, liveReserved, liveScheduled)
	evicting.evictionRequested = true
	blockDisk := roomyDisk("default-disk", liveMaximum, liveReserved, liveScheduled)
	blockDisk.diskType = "block"
	untagged := healthyNode("server-1", nil, roomyDisk("default-disk", liveMaximum, liveReserved, liveScheduled))

	for name, node := range map[string]testNode{
		"node not ready":           notReady,
		"disk not ready":           healthyNode("server-1", []string{"kipper-server"}, diskDown),
		"node cordoned":            cordoned,
		"node scheduling disabled": noScheduling,
		"disk evicting":            healthyNode("server-1", []string{"kipper-server"}, evicting),
		"block disk":               healthyNode("server-1", []string{"kipper-server"}, blockDisk),
		"server not tagged":        untagged,
	} {
		t.Run(name, func(t *testing.T) {
			mgr := longhornCluster(node)

			warning, err := mgr.CheckStorageRoom(context.Background(), postgresVolumes(t, "1Gi"), "--storage")

			require.NoError(t, err)
			assert.Contains(t, warning, "longhorn-single")
		})
	}
}

func TestServiceVolumesListsWhatAServiceCreates(t *testing.T) {
	volumes, err := ServiceVolumes("shop", "db", "postgres", "", true)
	require.NoError(t, err)
	assert.Equal(t, []NewVolume{
		{Name: "data", Bytes: gi(1), Namespace: "shop", Claim: "data-db-0"},
		{Name: "dump", Bytes: gi(1), Namespace: "shop", Claim: "db-dump"},
	}, volumes)

	volumes, err = ServiceVolumes("shop", "db", "postgres", "2Gi", false)
	require.NoError(t, err)
	assert.Equal(t, []NewVolume{{Name: "data", Bytes: gi(2), Namespace: "shop", Claim: "data-db-0"}}, volumes)

	volumes, err = ServiceVolumes("shop", "files", "minio", "", false)
	require.NoError(t, err)
	assert.Equal(t, []NewVolume{{Name: "data", Bytes: gi(1), Namespace: "shop", Claim: "data-files-0"}}, volumes, "the Service CRD's default, whatever the type")

	assert.Equal(t, NewVolume{Name: "uploads", Shared: true, Bytes: gi(2), Namespace: "shop", Claim: "shared-uploads"}, SharedVolume("shop", "uploads", gi(2)))

	_, err = ServiceVolumes("shop", "cache", "redis", "lots", false)
	require.Error(t, err)
}

func TestServerStorageRoomReportsTheServersBestDisk(t *testing.T) {
	mgr := longhornCluster(
		healthyNode("server-1", []string{"kipper-server"}, roomyDisk("default-disk", liveMaximum, liveReserved, liveScheduled)),
		healthyNode("worker-1", nil, roomyDisk("default-disk", gi(500), gi(150), 0)),
	)

	room, err := mgr.ServerStorageRoom(context.Background())

	require.NoError(t, err)
	assert.Equal(t, "server-1", room.Node)
	assert.Equal(t, liveMaximum-liveReserved-liveScheduled, room.Bytes)
}

func TestServerStorageRoomIsAnErrorWithoutLonghorn(t *testing.T) {
	_, err := storageManager(kipperClasses(), localPlatformConfig()).ServerStorageRoom(context.Background())
	require.Error(t, err)
}

func sizedServiceCR(name, serviceType, storage string, consistentBackups bool) *unstructured.Unstructured {
	spec := map[string]interface{}{"type": serviceType}
	if storage != "" {
		spec["storage"] = storage
	}
	if consistentBackups {
		spec["consistentBackups"] = true
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "Service",
		"metadata":   map[string]interface{}{"name": name, "namespace": "default"},
		"spec":       spec,
	}}
}

func TestDumpVolumeIsWhatTurningOnConsistentBackupsCreates(t *testing.T) {
	mgr := storageManager(kipperClasses(),
		sizedServiceCR("db", "postgres", "2Gi", false),
		sizedServiceCR("defaulted", "postgres", "", false),
		sizedServiceCR("dumping", "postgres", "2Gi", true),
		sizedServiceCR("cache", "redis", "1Gi", false),
	)
	ctx := context.Background()

	volumes, err := mgr.DumpVolume(ctx, "default", "db", "")
	require.NoError(t, err)
	assert.Equal(t, []NewVolume{{Name: "dump", Bytes: gi(2), Namespace: "default", Claim: "db-dump"}}, volumes)

	volumes, err = mgr.DumpVolume(ctx, "default", "db", "8Gi")
	require.NoError(t, err)
	assert.Equal(t, []NewVolume{{Name: "dump", Bytes: gi(8), Namespace: "default", Claim: "db-dump"}}, volumes, "a storage change in the same command sizes the dump volume")

	volumes, err = mgr.DumpVolume(ctx, "default", "defaulted", "")
	require.NoError(t, err)
	assert.Equal(t, []NewVolume{{Name: "dump", Bytes: gi(1), Namespace: "default", Claim: "defaulted-dump"}}, volumes)

	for _, name := range []string{"dumping", "cache", "missing"} {
		volumes, err = mgr.DumpVolume(ctx, "default", name, "")
		require.NoError(t, err)
		assert.Empty(t, volumes, name)
	}
}

// Both classes draw on the same disks, so a single-copy volume and a shared one
// on a single-node cluster share the server's room.
func TestStorageRoomCountsBothClassesAgainstTheSameDisks(t *testing.T) {
	maximum, reserved := gi(20), gi(4)
	server := roomyDisk("default-disk", maximum, reserved, maximum-reserved-gi(6))
	mgr := longhornCluster(healthyNode("server-1", []string{"kipper-server"}, server))
	volumes := append(postgresVolumes(t, "2Gi"), NewVolume{Name: "uploads", Shared: true, Bytes: gi(4)})

	_, err := mgr.CheckStorageRoom(context.Background(), volumes, "--storage")
	require.Error(t, err, "8Gi do not fit in 6Gi")

	withWorker := longhornCluster(
		healthyNode("server-1", []string{"kipper-server"}, server),
		healthyNode("worker-1", nil, roomyDisk("default-disk", gi(20), gi(4), 0)),
	)
	warning, err := withWorker.CheckStorageRoom(context.Background(), volumes, "--storage")
	require.NoError(t, err, "the worker takes the shared volume")
	assert.Empty(t, warning)
}

// 6Gi and 10Gi fit on disks with 10Gi and 6Gi left in either order.
func TestStorageRoomFindsAPlacementWhateverTheOrder(t *testing.T) {
	maximum, reserved := gi(40), gi(4)
	mgr := longhornCluster(healthyNode("server-1", []string{"kipper-server"},
		roomyDisk("disk-a", maximum, reserved, maximum-reserved-gi(10)),
		roomyDisk("disk-b", maximum, reserved, maximum-reserved-gi(6)),
	))
	small, large := NewVolume{Name: "small", Bytes: gi(6)}, NewVolume{Name: "large", Bytes: gi(10)}

	for _, volumes := range [][]NewVolume{{small, large}, {large, small}} {
		warning, err := mgr.CheckStorageRoom(context.Background(), volumes, "--storage")
		require.NoError(t, err)
		assert.Empty(t, warning)
	}
}

// 3Gi, 3Gi, 2Gi, 2Gi and 2Gi fit on two disks with 6Gi left each, but placing
// the largest first on the roomiest disk misses that placement. Since a valid
// placement exists, the check must warn rather than refuse the volumes.
func TestStorageRoomWarnsWhenItCannotProveAShortage(t *testing.T) {
	maximum, reserved := gi(40), gi(4)
	mgr := longhornCluster(healthyNode("server-1", []string{"kipper-server"},
		roomyDisk("disk-a", maximum, reserved, maximum-reserved-gi(6)),
		roomyDisk("disk-b", maximum, reserved, maximum-reserved-gi(6)),
	))
	volumes := []NewVolume{{Name: "a", Bytes: gi(3)}, {Name: "b", Bytes: gi(3)}, {Name: "c", Bytes: gi(2)}, {Name: "d", Bytes: gi(2)}, {Name: "e", Bytes: gi(2)}}

	warning, err := mgr.CheckStorageRoom(context.Background(), volumes, "--storage")

	require.NoError(t, err)
	assert.NotEmpty(t, warning)
}

// The API server fills in spec.storage from the Service CRD when a command
// leaves it out, so that default is the size the volumes get.
func TestDefaultStorageIsTheServiceCRDsDefault(t *testing.T) {
	raw, err := os.ReadFile("../installer/crds/kipper.run_services.yaml")
	require.NoError(t, err)
	var crd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema struct {
						Properties struct {
							Spec struct {
								Properties struct {
									Storage struct {
										Default string `json:"default"`
									} `json:"storage"`
								} `json:"properties"`
							} `json:"spec"`
						} `json:"properties"`
					} `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &crd))
	require.NotEmpty(t, crd.Spec.Versions)
	for _, v := range crd.Spec.Versions {
		assert.Equal(t, DefaultStorage, v.Schema.OpenAPIV3Schema.Properties.Spec.Properties.Storage.Default)
	}
}

// The first volume the placement cannot find a place for is a shared one that
// might still fit, but the database volume cannot fit on the server at all.
func TestStorageRoomRefusesACertainShortageBehindAnUncertainOne(t *testing.T) {
	maximum, reserved := gi(40), gi(4)
	mgr := longhornCluster(
		healthyNode("server-1", []string{"kipper-server"}, roomyDisk("default-disk", maximum, reserved, maximum-reserved-gi(1))),
		healthyNode("worker-a", nil, roomyDisk("default-disk", maximum, reserved, maximum-reserved-gi(6))),
		healthyNode("worker-b", nil, roomyDisk("default-disk", maximum, reserved, maximum-reserved-gi(6))),
	)
	volumes := []NewVolume{
		{Name: "a", Shared: true, Bytes: gi(4)}, {Name: "b", Shared: true, Bytes: gi(4)}, {Name: "c", Shared: true, Bytes: gi(3)},
		{Name: "db data", Bytes: gi(1.5)},
	}

	_, err := mgr.CheckStorageRoom(context.Background(), volumes, "--storage")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "on the server")
	assert.Contains(t, err.Error(), "server-1")
}

// A service deleted without its data leaves its claim behind, and a service of
// the same name takes that claim over instead of a new volume.
func TestStorageRoomSkipsClaimsThatAlreadyExist(t *testing.T) {
	clientset := kipperClasses()
	_, err := clientset.CoreV1().PersistentVolumeClaims("default").Create(context.Background(),
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-db-0", Namespace: "default"}}, metav1.CreateOptions{})
	require.NoError(t, err)
	objects := append(defaultSettings(), longhornNodeObject(healthyNode("server-1", []string{"kipper-server"},
		roomyDisk("default-disk", gi(40), gi(12), gi(27)))))
	mgr := storageManager(clientset, objects...)

	warning, err := mgr.CheckStorageRoom(context.Background(), postgresVolumes(t, "1Gi"), "--storage")
	require.NoError(t, err, "only the 1Gi dump volume is new, and 1Gi is left")
	assert.Empty(t, warning)

	_, err = mgr.CheckStorageRoom(context.Background(), postgresVolumes(t, "2Gi"), "--storage")
	require.Error(t, err, "the 2Gi dump volume is new and does not fit")
	assert.Contains(t, err.Error(), "dump 2Gi")
	assert.NotContains(t, err.Error(), "data 2Gi")
}

func TestStorageRoomWarnsWhenAClaimCannotBeRead(t *testing.T) {
	clientset := kipperClasses()
	clientset.PrependReactor("get", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("persistentvolumeclaims is forbidden")
	})
	objects := append(defaultSettings(), longhornNodeObject(healthyNode("server-1", []string{"kipper-server"},
		roomyDisk("default-disk", liveMaximum, liveReserved, liveScheduled))))

	warning, err := storageManager(clientset, objects...).CheckStorageRoom(context.Background(), postgresVolumes(t, "1Gi"), "--storage")

	require.NoError(t, err)
	assert.Contains(t, warning, "forbidden")
}
