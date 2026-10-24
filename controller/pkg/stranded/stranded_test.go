package stranded

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func node(ready corev1.ConditionStatus, taints ...corev1.Taint) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Spec:       corev1.NodeSpec{Taints: taints},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: ready}}},
	}
}

func TestReadyExcludesOutOfService(t *testing.T) {
	taint := Taint()
	assert.True(t, Ready(node(corev1.ConditionTrue)))
	assert.False(t, Ready(node(corev1.ConditionFalse)))
	assert.False(t, Ready(node(corev1.ConditionUnknown)))
	assert.False(t, Ready(node(corev1.ConditionTrue, taint)), "a Ready node with the out-of-service taint is fenced")
	// The upstream mechanisms check the key only, so any effect counts.
	assert.True(t, OutOfService(node(corev1.ConditionTrue, corev1.Taint{Key: TaintKey, Effect: corev1.TaintEffectNoSchedule})))
	assert.False(t, OutOfService(node(corev1.ConditionTrue, corev1.Taint{Key: "other", Effect: corev1.TaintEffectNoExecute})))
	assert.False(t, Ready(nil))
}

func TestTaintIsNoExecuteNodeShutdown(t *testing.T) {
	taint := Taint()
	assert.Equal(t, "node.kubernetes.io/out-of-service", taint.Key)
	assert.Equal(t, "nodeshutdown", taint.Value)
	assert.Equal(t, corev1.TaintEffectNoExecute, taint.Effect)
}

func TestCurrent(t *testing.T) {
	cases := []struct {
		name string
		r    Replica
		want bool
	}{
		{"never failed", Replica{HealthyAt: "2026-10-24T08:00:00Z"}, true},
		{"rebuilt after its last failure", Replica{HealthyAt: "2026-10-24T09:00:00Z", LastFailedAt: "2026-10-24T08:00:00Z"}, true},
		{"failed now", Replica{HealthyAt: "2026-10-24T09:00:00Z", FailedAt: "2026-10-24T10:00:00Z", LastFailedAt: "2026-10-24T10:00:00Z"}, false},
		{"salvaged without a rebuild", Replica{HealthyAt: "2026-10-24T08:00:00Z", LastFailedAt: "2026-10-24T12:00:20Z"}, false},
		{"failure at the same second as healthy", Replica{HealthyAt: "2026-10-24T08:00:00Z", LastFailedAt: "2026-10-24T08:00:00Z"}, false},
		{"never healthy", Replica{}, false},
		{"unparsable healthyAt", Replica{HealthyAt: "yesterday"}, false},
		{"unparsable lastFailedAt", Replica{HealthyAt: "2026-10-24T08:00:00Z", LastFailedAt: "soon"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, Current(c.r))
		})
	}
}

func TestCurrentCopies(t *testing.T) {
	replicas := []Replica{
		{Name: "r1", Volume: "pvc-1", Node: "worker-1", HealthyAt: "2026-10-24T08:00:00Z"},
		{Name: "r2", Volume: "pvc-1", Node: "server-1", HealthyAt: "2026-10-24T08:00:00Z"},
		{Name: "r3", Volume: "pvc-1", Node: "worker-2", HealthyAt: "2026-10-24T08:00:00Z", FailedAt: "2026-10-24T09:00:00Z"},
		{Name: "r4", Volume: "pvc-2", Node: "server-1", HealthyAt: "2026-10-24T08:00:00Z"},
		{Name: "r5", Volume: "pvc-1", Node: "", HealthyAt: "2026-10-24T08:00:00Z"},
	}
	assert.Equal(t, []string{"server-1"}, CurrentCopies("pvc-1", "worker-1", replicas))
	assert.Empty(t, CurrentCopies("pvc-3", "worker-1", replicas))
	assert.Equal(t, []string{"r2", "r3"}, names(Elsewhere("pvc-1", "worker-1", replicas)))
}

func names(rs []Replica) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Name)
	}
	return out
}

func TestReplicasFromJSON(t *testing.T) {
	data := []byte(`{"apiVersion":"v1","kind":"List","items":[
	  {"metadata":{"name":"pvc-1-r-abc"},"spec":{"volumeName":"pvc-1","nodeID":"server-1","healthyAt":"2026-10-24T08:00:00Z","failedAt":"","lastFailedAt":"2026-10-23T08:00:00Z"}}
	]}`)
	got, err := ReplicasFromJSON(data)
	require.NoError(t, err)
	assert.Equal(t, []Replica{{Name: "pvc-1-r-abc", Volume: "pvc-1", Node: "server-1", HealthyAt: "2026-10-24T08:00:00Z", LastFailedAt: "2026-10-23T08:00:00Z"}}, got)
	_, err = ReplicasFromJSON([]byte("not json"))
	assert.Error(t, err)
}

func kipperPod(name, nodeName string, owner *metav1.OwnerReference, claims ...string) corev1.Pod {
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", Labels: map[string]string{"app.kubernetes.io/managed-by": "kipper"}},
		Spec:       corev1.PodSpec{NodeName: nodeName},
	}
	if owner != nil {
		p.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	for _, c := range claims {
		p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: c, VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: c}}})
	}
	return p
}

func claim(name, pv string, mode corev1.PersistentVolumeAccessMode) corev1.PersistentVolumeClaim {
	return corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: pv, AccessModes: []corev1.PersistentVolumeAccessMode{mode}},
	}
}

func longhornPV(name, handle string) corev1.PersistentVolume {
	return corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{Driver: LonghornDriver, VolumeHandle: handle}}},
	}
}

func TestFind(t *testing.T) {
	sts := &metav1.OwnerReference{Kind: "StatefulSet", Name: "db", APIVersion: "apps/v1"}
	pods := []corev1.Pod{
		kipperPod("db-0", "worker-1", sts, "data-db-0"),
		kipperPod("web-1", "worker-1", nil, "uploads"),
		kipperPod("db2-0", "server-1", sts, "data-db2-0"),
		kipperPod("cache-0", "worker-1", sts),
	}
	foreign := kipperPod("other-0", "worker-1", sts, "data-other-0")
	foreign.Labels = nil
	pods = append(pods, foreign)
	local := kipperPod("local-0", "worker-1", sts, "data-local-0")
	pods = append(pods, local)
	check := kipperPod("kipper-node-check-abc", "worker-1", nil, "data-db-0")
	check.Labels["kipper.run/node-check"] = "abc"
	pods = append(pods, check)

	claims := []corev1.PersistentVolumeClaim{
		claim("data-db-0", "restored-db", corev1.ReadWriteOnce),
		claim("uploads", "pvc-2", corev1.ReadWriteMany),
		claim("data-db2-0", "pvc-3", corev1.ReadWriteOnce),
		claim("data-other-0", "pvc-4", corev1.ReadWriteOnce),
		claim("data-local-0", "pvc-5", corev1.ReadWriteOnce),
	}
	hostPath := corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pvc-5"}}
	pvs := []corev1.PersistentVolume{longhornPV("restored-db", "pvc-1"), longhornPV("pvc-2", "pvc-2"), longhornPV("pvc-3", "pvc-3"), longhornPV("pvc-4", "pvc-4"), hostPath}

	got := Find("worker-1", pods, claims, pvs)
	assert.Equal(t, []Service{
		{Namespace: "shop", Pod: "db-0", Owner: "db", Claim: "data-db-0", PersistentVolume: "restored-db", Volume: "pvc-1", ReadWriteOnce: true},
		{Namespace: "shop", Pod: "web-1", Claim: "uploads", PersistentVolume: "pvc-2", Volume: "pvc-2"},
	}, got)
}

func TestFindKeepsUnboundClaimsOut(t *testing.T) {
	sts := &metav1.OwnerReference{Kind: "StatefulSet", Name: "db"}
	pods := []corev1.Pod{kipperPod("db-0", "worker-1", sts, "data-db-0")}
	claims := []corev1.PersistentVolumeClaim{claim("data-db-0", "", corev1.ReadWriteOnce)}
	assert.Empty(t, Find("worker-1", pods, claims, nil))
}

func TestCopiesOfAll(t *testing.T) {
	replicas := []Replica{
		{Name: "a1", Volume: "a", Node: "server-1", HealthyAt: "2026-10-24T08:00:00Z"},
		{Name: "a2", Volume: "a", Node: "worker-2", HealthyAt: "2026-10-24T08:00:00Z"},
		{Name: "b1", Volume: "b", Node: "worker-2", HealthyAt: "2026-10-24T08:00:00Z"},
	}
	assert.Equal(t, []string{"worker-2"}, CopiesOfAll([]string{"a", "b"}, "worker-1", replicas), "a node counts only with a current copy of every volume")
	assert.Equal(t, []string{"server-1", "worker-2"}, CopiesOfAll([]string{"a"}, "worker-1", replicas))
	assert.Empty(t, CopiesOfAll([]string{"a", "c"}, "worker-1", replicas))
	assert.Empty(t, CopiesOfAll(nil, "worker-1", replicas))
}

func TestFindUnmanagedListsOnlyPodsWithoutTheKipperLabel(t *testing.T) {
	sts := &metav1.OwnerReference{Kind: "StatefulSet", Name: "db", APIVersion: "apps/v1"}
	foreign := kipperPod("other-0", "worker-1", sts, "data-other-0")
	foreign.Labels = nil
	pods := []corev1.Pod{kipperPod("db-0", "worker-1", sts, "data-db-0"), foreign}
	claims := []corev1.PersistentVolumeClaim{
		claim("data-db-0", "pvc-1", corev1.ReadWriteOnce),
		claim("data-other-0", "pvc-4", corev1.ReadWriteOnce),
	}
	pvs := []corev1.PersistentVolume{longhornPV("pvc-1", "pvc-1"), longhornPV("pvc-4", "pvc-4")}
	assert.Equal(t, []Service{
		{Namespace: "shop", Pod: "other-0", Owner: "db", Claim: "data-other-0", PersistentVolume: "pvc-4", Volume: "pvc-4", ReadWriteOnce: true},
	}, FindUnmanaged("worker-1", pods, claims, pvs))
}
