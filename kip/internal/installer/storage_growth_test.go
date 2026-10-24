package installer

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

func TestStorageGrowthRefusesBeforeTouchingTheHost(t *testing.T) {
	for name, tc := range map[string]struct {
		routes []route
		want   string
	}{
		"already on Longhorn": {[]route{{contains: "{.spec.storage}", out: "longhorn"}}, "already uses Longhorn"},
		"platform without move support": {[]route{
			{contains: "{.spec.storage}", out: "local"},
			{contains: "validatingadmissionpolicy", out: ""},
		}, "kip upgrade"},
		"no CPU left for Longhorn": {[]route{
			{contains: "{.spec.storage}", out: "local"},
			{contains: "validatingadmissionpolicy", out: platform.StorageMovePodsPolicy},
			{contains: "get nodes", out: listJSON(t, cpuNode("server", "2"))},
			{contains: "get pods", out: listJSON(t, cpuPod("shop", "pg2-0", "server", "1000m"), cpuPod("kube-system", "platform", "server", "905m"))},
			{contains: "get namespaces", out: `{"items":[]}`},
		}, "240m"},
		"no CPU left for Longhorn behind a kubectl warning": {[]route{
			{contains: "{.spec.storage}", out: "local"},
			{contains: "validatingadmissionpolicy", out: platform.StorageMovePodsPolicy},
			{contains: "get nodes", out: "Warning: v1 Node is deprecated\n" + listJSON(t, cpuNode("server", "2"))},
			{contains: "get pods", out: listJSON(t, cpuPod("shop", "pg2-0", "server", "1000m"), cpuPod("kube-system", "platform", "server", "905m"))},
			{contains: "get namespaces", out: `{"items":[]}`},
		}, "240m"},
	} {
		t.Run(name, func(t *testing.T) {
			host := &routedRunner{routes: tc.routes}
			err := StorageGrowth{Server: host}.Run()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			for _, c := range host.commands {
				assert.NotContains(t, c, "patch platformconfig")
				assert.NotContains(t, c, "longhorn.yaml")
			}
		})
	}
}

func cpuNode(name, cpu string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse("8Gi")},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

func cpuPod(namespace, name, node, cpu string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)},
		}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func listJSON(t *testing.T, items ...any) string {
	t.Helper()
	out, err := json.Marshal(map[string]any{"items": items})
	require.NoError(t, err)
	return string(out)
}

func TestLonghornCPURefusal(t *testing.T) {
	server := cpuNode("server", "2")

	err := longhornCPURefusal([]corev1.Node{server}, []corev1.Pod{
		cpuPod("shop", "pg2-0", "server", "500m"), cpuPod("shop", "pg3-0", "server", "500m"), cpuPod("kube-system", "platform", "server", "905m"),
	}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "server has 95m CPU")
	assert.Contains(t, err.Error(), "240m")
	assert.Contains(t, err.Error(), "kip service update")

	assert.NoError(t, longhornCPURefusal([]corev1.Node{server}, []corev1.Pod{cpuPod("shop", "pg2-0", "server", "1700m")}, nil))

	assert.NoError(t, longhornCPURefusal([]corev1.Node{server}, []corev1.Pod{
		cpuPod("shop", "pg2-0", "server", "1700m"), cpuPod("longhorn-system", "instance-manager-1", "server", "240m"),
	}, nil), "a rerun counts Longhorn's own pods as its reservation")

	err = longhornCPURefusal([]corev1.Node{cpuNode("server", "1999m")}, []corev1.Pod{cpuPod("shop", "pg2-0", "server", "1760m")}, nil)
	require.Error(t, err, "Longhorn rounds 12% of 1999m up to 240m")
	assert.Contains(t, err.Error(), "240m")

	cordoned := cpuNode("server", "2")
	cordoned.Spec.Unschedulable = true
	err = longhornCPURefusal([]corev1.Node{cordoned}, []corev1.Pod{cpuPod("shop", "pg2-0", "server", "1900m")}, nil)
	require.Error(t, err, "a node that takes no new pods still runs an instance manager")

	err = longhornCPURefusal([]corev1.Node{server, cpuNode("worker", "2")}, []corev1.Pod{
		cpuPod("shop", "pg2-0", "server", "500m"), cpuPod("shop", "web", "worker", "1900m"),
	}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worker has 100m CPU")
}

func TestGrowthPreflight(t *testing.T) {
	ok := SystemInfo{OS: "ubuntu", OSVersion: "24.04", RAMMB: 7800, DiskMB: 60000, DiskSizeMB: 80000}
	warning, err := growthPreflight(ok, false)
	require.NoError(t, err)
	assert.Empty(t, warning)

	small := ok
	small.RAMMB = 3900
	_, err = growthPreflight(small, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Resize the server and run the command again")
	warning, err = growthPreflight(small, true)
	require.NoError(t, err)
	assert.NotEmpty(t, warning, "the hidden override still warns")

	full := ok
	full.DiskMB = 15000
	_, err = growthPreflight(full, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "25%")

	held := ok
	held.OS, held.OSVersion = "rocky", "9.7"
	_, err = growthPreflight(held, false)
	assert.Error(t, err)
}

func TestStorageGrowthKeepsTheFirstHardeningDecision(t *testing.T) {
	recorded := &routedRunner{routes: []route{{contains: "storage-growth-harden}", out: "harden[false]"}}}
	harden, err := StorageGrowth{Server: recorded}.hardenDecision()
	require.NoError(t, err)
	assert.False(t, harden, "a rerun reuses the decision made before the packages")
	for _, c := range recorded.commands {
		assert.NotContains(t, c, "ss -tulnH", "a rerun does not audit a host the packages may have changed")
	}

	fresh := &routedRunner{routes: []route{{contains: "storage-growth-harden}", out: "harden[]"}}}
	harden, err = StorageGrowth{Server: fresh}.hardenDecision()
	require.NoError(t, err)
	assert.True(t, harden, "a clean host is hardened again after the packages")
	assert.True(t, fresh.ran("kipper.run/storage-growth-harden=true"))

	exposed := &routedRunner{routes: []route{
		{contains: "storage-growth-harden}", out: "harden[]"},
		{contains: ":111$", out: "udp UNCONN 0 0 0.0.0.0:111 0.0.0.0:*"},
	}}
	harden, err = StorageGrowth{Server: exposed}.hardenDecision()
	require.NoError(t, err)
	assert.False(t, harden, "a host with findings is left as the operator chose")
	assert.True(t, exposed.ran("kipper.run/storage-growth-harden=false"))
}

func TestLocalServiceVolumesListsOnlyServiceDataOnLocalPath(t *testing.T) {
	services := `{"items":[{"metadata":{"name":"db","namespace":"shop"}},{"metadata":{"name":"cache","namespace":"shop"}},{"metadata":{"name":"pg","namespace":"blog"}}]}`
	claims := `{"items":[
{"metadata":{"name":"data-db-0","namespace":"shop"},"spec":{"storageClassName":"local-path"}},
{"metadata":{"name":"data-cache-0","namespace":"shop"},"spec":{"storageClassName":"longhorn-single"}},
{"metadata":{"name":"data-pg-0","namespace":"blog"},"spec":{"storageClassName":"local-path"}},
{"metadata":{"name":"zot-data","namespace":"kipper-system"},"spec":{"storageClassName":"local-path"}}]}`
	du := "2048\t/var/lib/rancher/k3s/storage/pvc-1_shop-staging_data-db-0/\n1024\t/var/lib/rancher/k3s/storage/pvc-2_blog_data-pg-0/\n"
	namespaces := `{"items":[{"metadata":{"name":"shop-staging","labels":{"kipper.run/project":"shop","kipper.run/environment":"staging"}}},{"metadata":{"name":"blog"}}]}`
	host := &routedRunner{routes: []route{
		{contains: "services.kipper.run -A", out: strings.ReplaceAll(services, `"shop"`, `"shop-staging"`)},
		{contains: "get pvc -A", out: strings.ReplaceAll(claims, `"shop"`, `"shop-staging"`)},
		{contains: "du -sk", out: du},
		{contains: "get namespaces", out: namespaces},
	}}
	got, err := LocalServiceVolumes(host)
	require.NoError(t, err)
	assert.Equal(t, []LocalServiceVolume{
		{Namespace: "blog", Service: "pg", Claim: "data-pg-0", UsedBytes: 1 << 20, Flags: " --project blog"},
		{Namespace: "shop-staging", Service: "db", Claim: "data-db-0", UsedBytes: 2 << 20, Flags: " --project shop --environment staging"},
	}, got, "the flags match the CPU refusal's, and a namespace without a project label is named as the project")
}

// retainedNow is the clock for the retained-copy tests. Their moves finished
// a day and an hour before it.
var retainedNow = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

// retainedCopyJSON is a retained volume whose move verified the claim with
// UID destination.
func retainedCopyJSON(name, service, state string, used int64, destination string) string {
	b, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"name":            name,
			"resourceVersion": "7",
			"labels":          map[string]string{platform.RetainedLocalCopyLabel: "true"},
			"annotations": map[string]string{
				platform.RetainedLocalCopyServiceAnnotation:   service,
				platform.RetainedLocalCopyStateAnnotation:     state,
				platform.RetainedLocalCopyUsedBytesAnnotation: strconv.FormatInt(used, 10),
				retainedDestinationAnnotation:                 destination,
				platform.RetainedLocalCopyMovedAtAnnotation:   retainedNow.Add(-25 * time.Hour).Format(time.RFC3339),
			},
		},
	})
	return string(b)
}

func TestRetainedCopiesArePrunableOnlyOnceTheServiceIsOnLonghorn(t *testing.T) {
	list := `{"items":[` + strings.Join([]string{
		retainedCopyJSON("pvc-a", "shop/db", platform.RetainedLocalCopyMoved, 3<<30, "c1"),
		retainedCopyJSON("pvc-b", "shop/cache", platform.RetainedLocalCopyMoving, 1<<30, ""),
		retainedCopyJSON("pvc-c", "blog/pg", platform.RetainedLocalCopyMoved, 1<<30, "c0"),
		retainedCopyJSON("pvc-d", "shop/lost", platform.RetainedLocalCopyMoved, 1<<30, "c4"),
		retainedCopyJSON("pvc-e", "shop/gone", platform.RetainedLocalCopyMoved, 1<<30, "c5"),
		retainedCopyJSON("pvc-f", "shop/pending", platform.RetainedLocalCopyMoved, 1<<30, "c3"),
		retainedCopyJSON("pvc-g", "shop/replaced", platform.RetainedLocalCopyMoved, 1<<30, "c8"),
		retainedCopyJSON("pvc-h", "shop/unrecorded", platform.RetainedLocalCopyMoved, 1<<30, ""),
		retainedCopyJSON("pvc-i", "shop/finishing", platform.RetainedLocalCopyMoved, 1<<30, "c7"),
	}, ",") + `]}`
	host := &routedRunner{routes: retainedCopyRoutes(list)}
	got, err := RetainedCopies(host, retainedNow, PruneWaivers{})
	require.NoError(t, err)
	require.Len(t, got, 9)
	byService := map[string]RetainedCopy{}
	for _, c := range got {
		byService[c.Namespace+"/"+c.Service] = c
	}
	assert.True(t, byService["shop/db"].Prunable)
	assert.Equal(t, int64(3<<30), byService["shop/db"].UsedBytes)
	assert.False(t, byService["shop/cache"].Prunable, "an unfinished move keeps its copy")
	assert.False(t, byService["blog/pg"].Prunable, "a service back on its local volume keeps the copy")
	assert.False(t, byService["shop/lost"].Prunable, "a service that lost its Longhorn claim keeps what may be its only copy")
	assert.Contains(t, byService["shop/lost"].Note, "only one left")
	assert.False(t, byService["shop/pending"].Prunable, "an unbound Longhorn claim is no proof the data is there")
	assert.False(t, byService["shop/replaced"].Prunable, "a Longhorn claim the move did not verify is no proof the data survived")
	assert.Contains(t, byService["shop/replaced"].Note, "replaced")
	assert.False(t, byService["shop/unrecorded"].Prunable, "a copy without a verified claim is kept")
	assert.Contains(t, byService["shop/unrecorded"].Note, "recorded no verified data volume")
	assert.True(t, byService["shop/gone"].Prunable, "a deleted service's copy can go")
	assert.False(t, byService["shop/finishing"].Prunable, "a committed move that has not finished keeps its copy")
	assert.Contains(t, byService["shop/finishing"].Note, "kip service storage longhorn finishing")
	assert.Contains(t, byService["shop/gone"].Note, "service was deleted")
}

// retainedCopyRoutes answers the live reads behind the prune decision for
// the services in TestRetainedCopiesArePrunableOnlyOnceTheServiceIsOnLonghorn.
func retainedCopyRoutes(list string) []route {
	missing := errors.New("exit 1")
	return []route{
		{contains: "get pv -l", out: list},
		{contains: "get pvc 'data-db-0'", out: `{"metadata":{"name":"data-db-0","uid":"c1"},"spec":{"storageClassName":"longhorn-single"},"status":{"phase":"Bound"}}`},
		{contains: "get pvc 'data-pg-0'", out: `{"metadata":{"name":"data-pg-0","uid":"c2"},"spec":{"storageClassName":"local-path"},"status":{"phase":"Bound"}}`},
		{contains: "get pvc 'data-replaced-0'", out: `{"metadata":{"name":"data-replaced-0","uid":"c9"},"spec":{"storageClassName":"longhorn-single"},"status":{"phase":"Bound"}}`},
		{contains: "get pvc 'data-unrecorded-0'", out: `{"metadata":{"name":"data-unrecorded-0","uid":"c6"},"spec":{"storageClassName":"longhorn-single"},"status":{"phase":"Bound"}}`},
		{contains: "get pvc 'data-pending-0'", out: `{"metadata":{"name":"data-pending-0","uid":"c3"},"spec":{"storageClassName":"longhorn-single"},"status":{"phase":"Pending"}}`},
		{contains: "get pvc 'data-lost-0'", out: `Error from server (NotFound): persistentvolumeclaims "data-lost-0" not found`, err: missing},
		{contains: "get pvc 'data-gone-0'", out: `Error from server (NotFound): persistentvolumeclaims "data-gone-0" not found`, err: missing},
		{contains: "get pvc 'data-finishing-0'", out: `{"metadata":{"name":"data-finishing-0","uid":"c7"},"spec":{"storageClassName":"longhorn-single"},"status":{"phase":"Bound"}}`},
		{contains: "get pod 'db-0'", out: `{"metadata":{"name":"db-0"},"spec":{"volumes":[{"name":"data","persistentVolumeClaim":{"claimName":"data-db-0"}}]},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True","lastTransitionTime":"2026-10-25T10:00:00Z"}]}}`},
		{contains: "get backups.velero.io", out: `{"items":[]}`},
		{contains: "get podvolumebackups.velero.io", out: `{"items":[]}`},
		{contains: "get services.kipper.run 'pg'", out: `{"metadata":{"uid":"s4"},"spec":{}}`},
		{contains: "get services.kipper.run 'pending'", out: `{"metadata":{"uid":"s5"},"spec":{}}`},
		{contains: "get services.kipper.run 'replaced'", out: `{"metadata":{"uid":"s6"},"spec":{}}`},
		{contains: "get services.kipper.run 'unrecorded'", out: `{"metadata":{"uid":"s7"},"spec":{}}`},
		{contains: "get services.kipper.run 'lost'", out: `{"metadata":{"uid":"s1"},"spec":{}}`},
		{contains: "get services.kipper.run 'db'", out: `{"metadata":{"uid":"s2"},"spec":{}}`},
		{contains: "get services.kipper.run 'finishing'", out: `{"metadata":{"uid":"s3","annotations":{"kipper.run/storage-move":"kipper-storage-move-s3"}},"spec":{}}`},
		{contains: "get services.kipper.run 'gone'", out: `Error from server (NotFound): services.kipper.run "gone" not found`, err: missing},
	}
}

func TestPruneRetainedCopyHandsTheVolumeToTheProvisioner(t *testing.T) {
	moved := retainedCopyJSON("pvc-a", "shop/db", platform.RetainedLocalCopyMoved, 1, "c1")
	host := &routedRunner{routes: append([]route{{contains: "get pv 'pvc-a'", out: moved}}, retainedCopyRoutes("")...)}
	gone, err := PruneRetainedCopy(host, "pvc-a", retainedNow, PruneWaivers{}, 2, time.Millisecond, func(time.Duration) {})
	require.NoError(t, err)
	assert.False(t, gone, "a volume still there after the wait is reported, not hidden")
	require.Len(t, host.stdins, 1)
	assert.Contains(t, host.stdins[0], `"persistentVolumeReclaimPolicy":"Delete"`)
	assert.Contains(t, host.stdins[0], `"resourceVersion":"7"`, "a volume changed since the check is not released")

	moving := retainedCopyJSON("pvc-b", "shop/cache", platform.RetainedLocalCopyMoving, 1, "")
	host = &routedRunner{routes: append([]route{{contains: "get pv 'pvc-b'", out: moving}}, retainedCopyRoutes("")...)}
	_, err = PruneRetainedCopy(host, "pvc-b", retainedNow, PruneWaivers{}, 2, time.Millisecond, func(time.Duration) {})
	require.Error(t, err)
	assert.Empty(t, host.stdins, "an unfinished move's copy is never released")
}

func TestPruneRetainedCopyRechecksTheServiceRightBeforeDeleting(t *testing.T) {
	lost := retainedCopyJSON("pvc-d", "shop/lost", platform.RetainedLocalCopyMoved, 1, "c4")
	host := &routedRunner{routes: append([]route{{contains: "get pv 'pvc-d'", out: lost}}, retainedCopyRoutes("")...)}
	_, err := PruneRetainedCopy(host, "pvc-d", retainedNow, PruneWaivers{}, 2, time.Millisecond, func(time.Duration) {})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only one left")
	assert.Empty(t, host.stdins, "the copy is kept")
}

// The refusal names the services whose lower CPU would make room, and by how
// much, with the project and environment flags that reach each one. kip only
// suggests it and never lowers a request itself.
func TestLonghornCPURefusalNamesWhatWouldMakeRoom(t *testing.T) {
	server := cpuNode("server", "2")
	platformPod := func(cpu string) corev1.Pod { return cpuPod("kube-system", "platform", "server", cpu) }
	svc := func(name, cpu string) corev1.Pod { return servicePod("shop-staging", name, "server", cpu, "256Mi") }
	staging := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop-staging", Labels: map[string]string{
		labels.Project: "shop", labels.Environment: "staging",
	}}}
	namespaces := []corev1.Namespace{staging}

	err := longhornCPURefusal([]corev1.Node{server}, []corev1.Pod{svc("db", "500m"), svc("cache", "450m"), platformPod("905m")}, namespaces)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Lower db in shop-staging from 500m to 405m with 'kip service update db --cpu 405m --project shop --environment staging', or give the node more CPU")

	err = longhornCPURefusal([]corev1.Node{server}, []corev1.Pod{svc("db", "180m"), svc("cache", "170m"), platformPod("1500m")}, namespaces)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Lower db in shop-staging from 180m to 100m with 'kip service update db --cpu 100m --project shop --environment staging' and cache in shop-staging from 170m to 160m with 'kip service update cache --cpu 160m --project shop --environment staging', or give the node more CPU")

	err = longhornCPURefusal([]corev1.Node{server}, []corev1.Pod{svc("db", "150m"), svc("cache", "150m"), platformPod("1600m")}, namespaces)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "Lower db", "no plan that keeps every service at 100m makes room")
	assert.Contains(t, err.Error(), "give the node more CPU")

	projectOnly := staging
	projectOnly.Labels = map[string]string{labels.Project: "shop"}
	err = longhornCPURefusal([]corev1.Node{server}, []corev1.Pod{svc("db", "500m"), platformPod("1355m")}, []corev1.Namespace{projectOnly})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "with 'kip service update db --cpu 405m --project shop', or")

	err = longhornCPURefusal([]corev1.Node{server}, []corev1.Pod{svc("db", "500m"), platformPod("1355m")}, nil)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "Lower db", "a namespace without a project label names no command")
	assert.Contains(t, err.Error(), "Lower a service's CPU with 'kip service update <name> --cpu <cpu> --project <project>', or give the node more CPU")
}

// Pending pods may consume room needed by Longhorn's instance manager,
// so this check uses the same conservative accounting as the move's fit check.
func TestLonghornCPURefusalCountsPendingPodsThatFit(t *testing.T) {
	server := cpuNode("server", "2")
	running := []corev1.Pod{cpuPod("kube-system", "platform", "server", "1055m"), cpuPod("shop-staging", "pg1-0", "server", "500m"), cpuPod("shop-staging", "pg2-0", "server", "200m")}
	pending := func(cpu string) corev1.Pod {
		p := servicePod("shop-staging", "pg3", "", cpu, "1Gi")
		p.Status.Phase = corev1.PodPending
		return p
	}
	require.NoError(t, longhornCPURefusal([]corev1.Node{server}, running, nil), "245m unrequested holds the 240m")

	err := longhornCPURefusal([]corev1.Node{server}, append(running, pending("200m")), nil)
	require.Error(t, err, "pg3 would start in the room Longhorn needs")
	assert.Contains(t, err.Error(), "server has 45m CPU that no pod requests once the pending pods that could start there (shop-staging/pg3-0) take their share")

	assert.NoError(t, longhornCPURefusal([]corev1.Node{server}, append(running, pending("500m")), nil),
		"a pod that does not fit even without Longhorn keeps waiting either way")

	elsewhere := pending("200m")
	elsewhere.Spec.NodeSelector = map[string]string{"kipper.run/pool": "gpu"}
	assert.NoError(t, longhornCPURefusal([]corev1.Node{server}, append(running, elsewhere), nil), "a pod that may not run on the node takes nothing there")
}
