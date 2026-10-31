package installer

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/getkipper/kipper/controller/pkg/platform"
)

func TestKubectlMoveClusterTellsAMissingObjectFromAnUnreadableOne(t *testing.T) {
	missing := &routedRunner{routes: []route{{contains: "get pvc", out: `Error from server (NotFound): persistentvolumeclaims "data-db-0" not found`, err: errors.New("exit 1")}}}
	c, err := KubectlMoveCluster{Server: missing}.ReadClaim("shop", "data-db-0")
	require.NoError(t, err)
	assert.Nil(t, c)

	garbled := &routedRunner{routes: []route{{contains: "get pvc", out: "Connection closed"}}}
	_, err = KubectlMoveCluster{Server: garbled}.ReadClaim("shop", "data-db-0")
	assert.Error(t, err, "an answer without an object is never read as a missing claim")

	broken := &routedRunner{routes: []route{{contains: "get pvc", out: "Unable to connect to the server", err: errors.New("exit 1")}}}
	_, err = KubectlMoveCluster{Server: broken}.ReadClaim("shop", "data-db-0")
	assert.Error(t, err)
}

func TestKubectlMoveClusterReadsALocalPathVolume(t *testing.T) {
	pv := `Warning: noise
{"metadata":{"name":"pvc-1","uid":"u1"},"spec":{"capacity":{"storage":"2Gi"},"local":{"path":"/var/lib/rancher/k3s/storage/pvc-1_shop_data-db-0"},
"persistentVolumeReclaimPolicy":"Delete","storageClassName":"local-path",
"claimRef":{"namespace":"shop","name":"data-db-0","uid":"c1"},
"nodeAffinity":{"required":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"kubernetes.io/hostname","operator":"In","values":["server-1"]}]}]}}},
"status":{"phase":"Bound"}}`
	host := &routedRunner{routes: []route{{contains: "get pv", out: pv}}}
	got, err := KubectlMoveCluster{Server: host}.ReadPV("pvc-1")
	require.NoError(t, err)
	assert.Equal(t, "/var/lib/rancher/k3s/storage/pvc-1_shop_data-db-0", got.Path)
	assert.Equal(t, "server-1", got.Node)
	assert.Equal(t, "c1", got.ClaimUID)
	assert.Equal(t, "2Gi", got.Capacity)
	assert.Equal(t, "Delete", got.Reclaim)
}

func TestKubectlMoveClusterStopsAndStartsWithTheSavedReplicas(t *testing.T) {
	sts := `{"metadata":{"name":"db","resourceVersion":"7"},"spec":{"replicas":2}}`
	host := &routedRunner{routes: []route{{contains: "get statefulset", out: sts}}}
	replicas, err := KubectlMoveCluster{Server: host}.StopStatefulSet("shop", "db", "kipper-storage-move-u", -1)
	require.NoError(t, err)
	assert.Equal(t, int32(2), replicas)
	require.Len(t, host.stdins, 1)
	var patch map[string]any
	require.NoError(t, json.Unmarshal([]byte(host.stdins[0]), &patch))
	assert.Equal(t, "7", patch["metadata"].(map[string]any)["resourceVersion"], "the stop is guarded by the resource version")
	assert.Equal(t, float64(0), patch["spec"].(map[string]any)["replicas"])
	annotations := patch["metadata"].(map[string]any)["annotations"].(map[string]any)
	assert.Equal(t, "2", annotations[platform.StorageMoveReplicasAnnotation])
	assert.Equal(t, "kipper-storage-move-u", annotations[platform.StorageMoveAnnotation])

	stopped := `{"metadata":{"name":"db","resourceVersion":"8","annotations":{"kipper.run/storage-move":"kipper-storage-move-u","kipper.run/storage-move-replicas":"2"}},"spec":{"replicas":0}}`
	host = &routedRunner{routes: []route{{contains: "get statefulset", out: stopped}}}
	replicas, err = KubectlMoveCluster{Server: host}.StopStatefulSet("shop", "db", "kipper-storage-move-u", -1)
	require.NoError(t, err)
	assert.Equal(t, int32(2), replicas, "a second stop keeps the first count")
	assert.Empty(t, host.stdins, "nothing is patched twice")

	replicas, started, err := KubectlMoveCluster{Server: host}.StartStatefulSet("shop", "db", -1)
	require.NoError(t, err)
	assert.Equal(t, int32(2), replicas)
	assert.True(t, started)
	require.Len(t, host.stdins, 1)
	assert.Contains(t, host.stdins[0], `"kipper.run/storage-move":null`)
	assert.Contains(t, host.stdins[0], `"replicas":2`)
}

// The move record's count wins over the StatefulSet's own annotation and
// replicas, which a project editor can change during the move.
func TestKubectlMoveClusterStartsTheCountTheMoveRecorded(t *testing.T) {
	edited := `{"metadata":{"name":"db","resourceVersion":"9","annotations":{"kipper.run/storage-move":"kipper-storage-move-u","kipper.run/storage-move-replicas":"5"}},"spec":{"replicas":1}}`
	host := &routedRunner{routes: []route{{contains: "get statefulset", out: edited}}}
	replicas, err := KubectlMoveCluster{Server: host}.StopStatefulSet("shop", "db", "kipper-storage-move-u", 2)
	require.NoError(t, err)
	assert.Equal(t, int32(2), replicas, "a resumed stop keeps the recorded count")

	host = &routedRunner{routes: []route{{contains: "get statefulset", out: edited}}}
	replicas, started, err := KubectlMoveCluster{Server: host}.StartStatefulSet("shop", "db", 2)
	require.NoError(t, err)
	assert.Equal(t, int32(2), replicas)
	assert.True(t, started)
	require.Len(t, host.stdins, 1)
	assert.Contains(t, host.stdins[0], `"replicas":2`)

	unmarked := `{"metadata":{"name":"db","resourceVersion":"9"},"spec":{"replicas":1}}`
	host = &routedRunner{routes: []route{{contains: "get statefulset", out: unmarked}}}
	replicas, started, err = KubectlMoveCluster{Server: host}.StartStatefulSet("shop", "db", 2)
	require.NoError(t, err)
	assert.Equal(t, int32(1), replicas, "an unmarked StatefulSet reports its own count")
	assert.False(t, started, "another run has handed it back")
	assert.Empty(t, host.stdins, "the move never scales a StatefulSet it has handed back")
}

func TestKubectlMoveClusterReportsAMissingStatefulSet(t *testing.T) {
	host := &routedRunner{routes: []route{{contains: "get statefulset", out: `Error from server (NotFound): statefulsets.apps "db" not found`, err: errors.New("exit 1")}}}
	_, err := KubectlMoveCluster{Server: host}.StopStatefulSet("shop", "db", "kipper-storage-move-u", 2)
	require.ErrorIs(t, err, errStatefulSetGone)
	_, _, err = KubectlMoveCluster{Server: host}.StartStatefulSet("shop", "db", 2)
	require.ErrorIs(t, err, errStatefulSetGone)
}

// A resumed move stops the service again: a StatefulSet scaled up during a
// pause goes back to 0 with its saved count kept, and one that lost its mark
// saves the count the move recorded.
func TestKubectlMoveClusterStopsAgainOnResume(t *testing.T) {
	restarted := `{"metadata":{"name":"db","resourceVersion":"9","annotations":{"kipper.run/storage-move":"kipper-storage-move-u","kipper.run/storage-move-replicas":"2"}},"spec":{"replicas":1}}`
	host := &routedRunner{routes: []route{{contains: "get statefulset", out: restarted}}}
	replicas, err := KubectlMoveCluster{Server: host}.StopStatefulSet("shop", "db", "kipper-storage-move-u", 2)
	require.NoError(t, err)
	assert.Equal(t, int32(2), replicas)
	require.Len(t, host.stdins, 1)
	assert.JSONEq(t, `{"metadata":{"resourceVersion":"9"},"spec":{"replicas":0}}`, host.stdins[0])

	unmarked := `{"metadata":{"name":"db","resourceVersion":"10"},"spec":{"replicas":0}}`
	host = &routedRunner{routes: []route{{contains: "get statefulset", out: unmarked}}}
	replicas, err = KubectlMoveCluster{Server: host}.StopStatefulSet("shop", "db", "kipper-storage-move-u", 3)
	require.NoError(t, err)
	assert.Equal(t, int32(3), replicas)
	require.Len(t, host.stdins, 1)
	assert.Contains(t, host.stdins[0], `"kipper.run/storage-move-replicas":"3"`)
}

func TestKubectlMoveClusterDeletesAClaimOnlyByUID(t *testing.T) {
	host := &routedRunner{}
	require.NoError(t, KubectlMoveCluster{Server: host}.DeleteClaim("shop", "data-db-0", "c1"))
	require.Len(t, host.commands, 1)
	assert.Contains(t, host.commands[0], "delete --raw '/api/v1/namespaces/shop/persistentvolumeclaims/data-db-0'")
	assert.JSONEq(t, `{"kind":"DeleteOptions","apiVersion":"v1","preconditions":{"uid":"c1"}}`, host.stdins[0])

	conflict := &routedRunner{routes: []route{{contains: "delete --raw", out: "Error from server (Conflict): Precondition failed: UID in precondition: c1, UID in object meta: c2", err: errors.New("exit 1")}}}
	assert.Error(t, KubectlMoveCluster{Server: conflict}.DeleteClaim("shop", "data-db-0", "c1"))
}

func TestKubectlMoveClusterDeletesAVolumeOnlyByUID(t *testing.T) {
	host := &routedRunner{}
	require.NoError(t, KubectlMoveCluster{Server: host}.DeletePV("kipper-move-src-01234567", "v1"))
	require.Len(t, host.commands, 1)
	assert.Contains(t, host.commands[0], "delete --raw '/api/v1/persistentvolumes/kipper-move-src-01234567'")
	assert.JSONEq(t, `{"kind":"DeleteOptions","apiVersion":"v1","preconditions":{"uid":"v1"}}`, host.stdins[0])
}

func TestKubectlMoveClusterPatchesAVolumeOnlyAtTheVersionItRead(t *testing.T) {
	host := &routedRunner{}
	require.NoError(t, KubectlMoveCluster{Server: host}.PatchPV("pvc-old", PVPatch{ResourceVersion: "42", Reclaim: "Delete"}))
	require.Len(t, host.stdins, 1)
	assert.JSONEq(t, `{"metadata":{"resourceVersion":"42"},"spec":{"persistentVolumeReclaimPolicy":"Delete"}}`, host.stdins[0])
}

func TestKubectlMoveClusterSeesAClaimBeingDeleted(t *testing.T) {
	host := &routedRunner{routes: []route{{contains: "get pvc", out: `{"metadata":{"name":"data-db-0","uid":"c1","deletionTimestamp":"2026-10-24T12:00:00Z"},"spec":{}}`}}}
	c, err := KubectlMoveCluster{Server: host}.ReadClaim("shop", "data-db-0")
	require.NoError(t, err)
	assert.True(t, c.Deleting)
}

func TestKubectlMoveClusterRebindsAVolumeByName(t *testing.T) {
	host := &routedRunner{}
	require.NoError(t, KubectlMoveCluster{Server: host}.PatchPV("pvc-1", PVPatch{RebindTo: &ClaimRef{Namespace: "shop", Name: "data-db-0"}}))
	assert.Contains(t, host.stdins[0], `"claimRef":{"name":"data-db-0","namespace":"shop","resourceVersion":null,"uid":null}`)
}

func TestKubectlMoveClusterFencesTheNamespaceUnderItsVersion(t *testing.T) {
	host := &routedRunner{}
	require.NoError(t, KubectlMoveCluster{Server: host}.WriteMoveFence("shop", MoveFence{
		Claims: []string{"db-dump", "data-db-0"}, Lapsed: []string{"data-db-0"}, Version: "41",
	}))
	assert.Contains(t, host.stdins[0], `"resourceVersion":"41"`)
	assert.Contains(t, host.stdins[0], `"kipper.run/storage-move-claims":"data-db-0,db-dump"`)
	assert.Contains(t, host.stdins[0], `"kipper.run/storage-move-lapsed":"data-db-0"`)
	assert.Contains(t, host.stdins[0], `"labels":{"kipper.run/storage-move":"true"}`)

	host = &routedRunner{}
	require.NoError(t, KubectlMoveCluster{Server: host}.WriteMoveFence("shop", MoveFence{Version: "42"}))
	assert.Contains(t, host.stdins[0], `"labels":{"kipper.run/storage-move":null}`, "the label goes with the last claim")
	assert.Contains(t, host.stdins[0], `"kipper.run/storage-move-claims":null`)
	assert.Contains(t, host.stdins[0], `"kipper.run/storage-move-lapsed":null`)
}

// The policy matches only namespaces with the move label, so claims listed
// without it are not enforced. They are still listed, because other moves'
// claims must survive a resume that writes the label back.
func TestKubectlMoveClusterReadsTheFenceAndWhetherItApplies(t *testing.T) {
	fenced := `{"metadata":{"name":"shop","resourceVersion":"41","labels":{"kipper.run/storage-move":"true"},"annotations":{"kipper.run/storage-move-claims":"data-db-0,db-dump","kipper.run/storage-move-lapsed":"data-db-0"}}}`
	fence, err := KubectlMoveCluster{Server: &routedRunner{routes: []route{{contains: "get namespace", out: fenced}}}}.ReadMoveFence("shop")
	require.NoError(t, err)
	assert.Equal(t, MoveFence{Claims: []string{"data-db-0", "db-dump"}, Enforced: true, Lapsed: []string{"data-db-0"}, Version: "41"}, fence)

	unlabelled := `{"metadata":{"name":"shop","resourceVersion":"42","annotations":{"kipper.run/storage-move-claims":"data-db-0,db-dump"}}}`
	fence, err = KubectlMoveCluster{Server: &routedRunner{routes: []route{{contains: "get namespace", out: unlabelled}}}}.ReadMoveFence("shop")
	require.NoError(t, err)
	assert.Equal(t, MoveFence{Claims: []string{"data-db-0", "db-dump"}, Version: "42"}, fence)
}

const denyingBinding = `{"metadata":{"name":"kipper-storage-move-pods"},"spec":{"policyName":"kipper-storage-move-pods","validationActions":["Deny"]}}`

func probeRunner(binding, probeOut string, probeErr error) *routedRunner {
	return &routedRunner{routes: []route{
		{contains: "get validatingadmissionpolicybinding", out: binding},
		{contains: "--dry-run=server", out: probeOut, err: probeErr},
	}}
}

func TestKubectlMoveClusterProbesAdmission(t *testing.T) {
	refused := probeRunner(denyingBinding, `The pods "kipper-move-probe" is invalid: : ValidatingAdmissionPolicy 'kipper-storage-move-pods' with binding 'kipper-storage-move-pods' denied request: no pod may mount data-db-0`, errors.New("exit 1"))
	ok, err := KubectlMoveCluster{Server: refused}.AdmissionRefuses("shop", "data-db-0")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Contains(t, refused.stdins[0], `"claimName":"data-db-0"`)

	admitted := probeRunner(denyingBinding, "pod/kipper-move-probe created (server dry run)", nil)
	ok, err = KubectlMoveCluster{Server: admitted}.AdmissionRefuses("shop", "data-db-0")
	require.NoError(t, err)
	assert.False(t, ok)

	other := probeRunner(denyingBinding, "forbidden: exceeded quota", errors.New("exit 1"))
	_, err = KubectlMoveCluster{Server: other}.AdmissionRefuses("shop", "data-db-0")
	assert.Error(t, err)
}

// A binding set to Warn names the policy in a warning and still admits the
// pod. Only a request that failed with the policy's denial is a refusal.
func TestKubectlMoveClusterTakesOnlyADenialAsARefusal(t *testing.T) {
	warned := probeRunner(denyingBinding, "Warning: Validation failed for ValidatingAdmissionPolicy 'kipper-storage-move-pods' with binding 'kipper-storage-move-pods': no pod may mount data-db-0\npod/kipper-move-probe created (server dry run)", nil)
	ok, err := KubectlMoveCluster{Server: warned}.AdmissionRefuses("shop", "data-db-0")
	require.NoError(t, err)
	assert.False(t, ok, "an admitted probe is never a refusal")

	warnedThenFailed := probeRunner(denyingBinding, "Warning: Validation failed for ValidatingAdmissionPolicy 'kipper-storage-move-pods' with binding 'kipper-storage-move-pods': no pod may mount data-db-0\nError from server (Forbidden): exceeded quota", errors.New("exit 1"))
	_, err = KubectlMoveCluster{Server: warnedThenFailed}.AdmissionRefuses("shop", "data-db-0")
	assert.Error(t, err, "a warning and an unrelated failure prove nothing")
}

func TestKubectlMoveClusterRequiresAnEnforcingBinding(t *testing.T) {
	for name, binding := range map[string]string{
		"warn only":    `{"metadata":{"name":"kipper-storage-move-pods"},"spec":{"policyName":"kipper-storage-move-pods","validationActions":["Warn"]}}`,
		"other policy": `{"metadata":{"name":"kipper-storage-move-pods"},"spec":{"policyName":"something-else","validationActions":["Deny"]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			host := probeRunner(binding, `ValidatingAdmissionPolicy 'kipper-storage-move-pods' with binding 'kipper-storage-move-pods' denied request`, errors.New("exit 1"))
			ok, err := KubectlMoveCluster{Server: host}.AdmissionRefuses("shop", "data-db-0")
			require.Error(t, err)
			assert.False(t, ok)
			assert.Contains(t, err.Error(), "kip upgrade")
		})
	}

	missing := &routedRunner{routes: []route{{contains: "get validatingadmissionpolicybinding", err: errors.New("exit 1"),
		out: `Error from server (NotFound): validatingadmissionpolicybindings.admissionregistration.k8s.io "kipper-storage-move-pods" not found`}}}
	_, err := KubectlMoveCluster{Server: missing}.AdmissionRefuses("shop", "data-db-0")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip upgrade")
}

func TestKubectlMoveClusterListsLivePodsOnTheClaims(t *testing.T) {
	pods := `{"items":[
{"metadata":{"name":"db-0","uid":"p-db","ownerReferences":[{"kind":"StatefulSet","name":"db","controller":true}]},"spec":{"volumes":[{"name":"data","persistentVolumeClaim":{"claimName":"data-db-0"}}]},"status":{"phase":"Running"}},
{"metadata":{"name":"dump-1"},"spec":{"volumes":[{"name":"d","persistentVolumeClaim":{"claimName":"db-dump"}}]},"status":{"phase":"Succeeded"}},
{"metadata":{"name":"web"},"spec":{"volumes":[{"name":"c","persistentVolumeClaim":{"claimName":"other"}}]},"status":{"phase":"Running"}},
{"metadata":{"name":"dt-1-import","uid":"p-dt"},"spec":{"volumes":[{"name":"d","persistentVolumeClaim":{"claimName":"data-db-0"}}]},"status":{"phase":"Pending"}}]}`
	host := &routedRunner{routes: []route{{contains: "get pods", out: pods}}}
	got, err := KubectlMoveCluster{Server: host}.PodsMounting("shop", []string{"data-db-0", "db-dump"})
	require.NoError(t, err)
	assert.Equal(t, []MovePod{{Name: "db-0", UID: "p-db", StatefulSet: "db"}, {Name: "dt-1-import", UID: "p-dt"}}, got)
}

func TestKubectlMoveClusterTrustsOnlyAFlushedCopy(t *testing.T) {
	result := func(phase, log string) CopyResult {
		host := &routedRunner{routes: []route{
			{contains: "get pod", out: `{"metadata":{"name":"kipper-move-copy-db"},"status":{"phase":"` + phase + `"}}`},
			{contains: "logs", out: log},
		}}
		r, err := KubectlMoveCluster{Server: host}.CopyResult("shop", "kipper-move-copy-db")
		require.NoError(t, err)
		return r
	}
	assert.Equal(t, CopyResult{}, result("Running", ""))
	assert.Equal(t, CopyResult{Finished: true, Succeeded: true, Log: "kipper-move-copied"}, result("Succeeded", "kipper-move-copied"))
	assert.False(t, result("Succeeded", "").Succeeded, "an exit without the copy's last line is no proof")
	assert.False(t, result("Succeeded", "kipper-move-verified 12 entries").Succeeded, "the copy pod does not verify")
	assert.False(t, result("Failed", "sync: error syncing '/dst': Input/output error").Succeeded)
}

func TestKubectlMoveClusterTrustsOnlyAVerification(t *testing.T) {
	result := func(phase, log string) CopyResult {
		host := &routedRunner{routes: []route{
			{contains: "get pod", out: `{"metadata":{"name":"kipper-move-verify-db"},"status":{"phase":"` + phase + `"}}`},
			{contains: "logs", out: log},
		}}
		r, err := KubectlMoveCluster{Server: host}.VerifyResult("shop", "kipper-move-verify-db")
		require.NoError(t, err)
		return r
	}
	assert.Equal(t, CopyResult{}, result("Running", ""))
	assert.Equal(t, CopyResult{Finished: true, Succeeded: true, Log: "kipper-move-verified 12 entries"}, result("Succeeded", "kipper-move-verified 12 entries"))
	assert.False(t, result("Succeeded", "kipper-move-copied").Succeeded, "a copy's success is no verification")
	differs := result("Failed", "kipper-move-mismatch: the copy differs from the source at ./x")
	assert.True(t, differs.Differs)
	assert.False(t, differs.Succeeded)
	killed := result("Failed", "comparing the copy with the source")
	assert.False(t, killed.Differs, "a verification that did not finish says nothing about the copy")
	assert.False(t, killed.Succeeded)
}

// The verify pod reads the copy back through a fresh mount, so it cannot
// write to either tree and its reads do not come from the copy's page cache.
func TestKubectlMoveClusterVerifiesFromReadOnlyMounts(t *testing.T) {
	host := &routedRunner{routes: []route{{contains: "kubectl create", out: "kipper-uid[pod-2]"}}}
	uid, err := KubectlMoveCluster{Server: host}.StartVerify(CopyPod{Namespace: "shop", Name: "kipper-move-verify-db", Source: "kipper-move-src-db", Target: "data-db-0"})
	require.NoError(t, err)
	assert.Equal(t, "pod-2", uid)
	var pod corev1.Pod
	require.NoError(t, json.Unmarshal([]byte(host.stdins[0]), &pod))
	assert.Equal(t, "kipper-move-verify-db", pod.Name)
	require.Len(t, pod.Spec.Volumes, 2)
	for _, v := range pod.Spec.Volumes {
		assert.True(t, v.PersistentVolumeClaim.ReadOnly, v.Name)
	}
	require.Len(t, pod.Spec.Containers, 1)
	c := pod.Spec.Containers[0]
	for _, m := range c.VolumeMounts {
		assert.True(t, m.ReadOnly, m.Name)
	}
	assert.Equal(t, moveVerifyScript, c.Command[2])
	limit := c.Resources.Limits[corev1.ResourceMemory]
	assert.Equal(t, "128Mi", limit.String(), "the verify pod starts at the probe pod's limit")
	assert.Equal(t, "true", pod.Labels["velero.io/exclude-from-backup"])
	assert.Equal(t, "src,dst", pod.Annotations["backup.velero.io/backup-volumes-excludes"])
}

func TestKubectlMoveClusterReadsWhetherTheVolumeIsDetached(t *testing.T) {
	state := func(out string, err error) (bool, error) {
		host := &routedRunner{routes: []route{{contains: "get volumes.longhorn.io 'pvc-new'", out: out, err: err}}}
		return KubectlMoveCluster{Server: host}.VolumeDetached("pvc-new")
	}
	detached, err := state(`{"status":{"state":"detached"}}`, nil)
	require.NoError(t, err)
	assert.True(t, detached)
	detached, err = state(`{"status":{"state":"detaching"}}`, nil)
	require.NoError(t, err)
	assert.False(t, detached)
	_, err = state(`Error from server (NotFound): volumes.longhorn.io "pvc-new" not found`, errors.New("exit 1"))
	assert.Error(t, err, "a volume that is gone is never taken for a detached one")
}

func TestKubectlMoveClusterExpandsAClaim(t *testing.T) {
	host := &routedRunner{}
	require.NoError(t, KubectlMoveCluster{Server: host}.ExpandClaim("shop", "data-db-0", "6Gi"))
	assert.Contains(t, host.commands[0], "-n 'shop' patch pvc 'data-db-0'")
	assert.JSONEq(t, `{"spec":{"resources":{"requests":{"storage":"6Gi"}}}}`, host.stdins[0])
	assert.Error(t, KubectlMoveCluster{Server: host}.ExpandClaim("shop", "data-db-0", "lots"))
}

func TestKubectlMoveClusterReadsAClaimsResize(t *testing.T) {
	pvc := `{"metadata":{"name":"data-db-0","uid":"c1"},"spec":{"resources":{"requests":{"storage":"6Gi"}}},
"status":{"phase":"Bound","capacity":{"storage":"5Gi"},"conditions":[{"type":"FileSystemResizePending","status":"True"}]}}`
	c, err := KubectlMoveCluster{Server: &routedRunner{routes: []route{{contains: "get pvc", out: pvc}}}}.ReadClaim("shop", "data-db-0")
	require.NoError(t, err)
	assert.Equal(t, "6Gi", c.Size)
	assert.Equal(t, "5Gi", c.Capacity)
	assert.True(t, c.ResizePending)
}

func TestKubectlMoveClusterSaysHowTheCopyPodEnded(t *testing.T) {
	read := func(pod string) CopyResult {
		host := &routedRunner{routes: []route{{contains: "get pod", out: pod}, {contains: "logs", out: ""}}}
		r, err := KubectlMoveCluster{Server: host}.CopyResult("shop", "kipper-move-copy-db")
		require.NoError(t, err)
		return r
	}
	oom := read(`{"metadata":{"name":"kipper-move-copy-db","uid":"pod-1"},"status":{"phase":"Failed","containerStatuses":[{"name":"copy","state":{"terminated":{"reason":"OOMKilled","exitCode":137}}}]}}`)
	assert.Equal(t, "pod-1", oom.UID)
	assert.True(t, oom.Finished)
	assert.False(t, oom.Succeeded)
	assert.Equal(t, "OOMKilled (exit 137)", oom.Reason)

	evicted := read(`{"metadata":{"name":"kipper-move-copy-db","uid":"pod-1"},"status":{"phase":"Failed","reason":"Evicted","message":"The node was low on resource: memory."}}`)
	assert.Equal(t, "Evicted: The node was low on resource: memory.", evicted.Reason)

	running := read(`{"metadata":{"name":"kipper-move-copy-db","uid":"pod-1"},"status":{"phase":"Running"}}`)
	assert.Equal(t, CopyResult{UID: "pod-1"}, running, "a running pod is known by its UID")
}

func TestKubectlMoveClusterBuildsAReadOnlySourceView(t *testing.T) {
	host := &routedRunner{}
	require.NoError(t, KubectlMoveCluster{Server: host}.CreateSourceVolume(SourceVolume{
		PV: "kipper-move-src-01234567", Claim: "kipper-move-src-db", Namespace: "shop",
		Path: "/var/lib/rancher/k3s/storage/pvc-1_shop_data-db-0", Node: "server-1", Capacity: "2Gi",
	}))
	require.Len(t, host.stdins, 2)
	assert.Contains(t, host.stdins[0], `"hostPath":{"path":"/var/lib/rancher/k3s/storage/pvc-1_shop_data-db-0","type":"Directory"}`)
	assert.Contains(t, host.stdins[0], `"persistentVolumeReclaimPolicy":"Retain"`, "deleting the view never deletes the directory")
	assert.Contains(t, host.stdins[0], `"values":["server-1"]`)
	assert.Contains(t, host.stdins[1], `"volumeName":"kipper-move-src-01234567"`)
	assert.Contains(t, host.stdins[1], `"storageClassName":""`)

	host = &routedRunner{routes: []route{{contains: "kubectl create", out: "kipper-uid[pod-1]"}}}
	_, err := KubectlMoveCluster{Server: host}.StartCopy(CopyPod{Namespace: "shop", Name: "kipper-move-copy-db", Source: "kipper-move-src-db", Target: "data-db-0"})
	require.NoError(t, err)
	assert.Contains(t, host.stdins[0], `{"name":"src","persistentVolumeClaim":{"claimName":"kipper-move-src-db","readOnly":true}}`)
	assert.Contains(t, host.stdins[0], `{"name":"src","readOnly":true,"mountPath":"/src"}`)
	assert.True(t, strings.Contains(host.stdins[0], `"runAsUser":0`))
}

// A backup that ran during a copy and was restored later would start the copy
// pod again, and its first act is to clear the target. Velero leaves out the
// pod, the read-only view and its volume.
func TestKubectlMoveClusterKeepsTheMoveOutOfBackups(t *testing.T) {
	host := &routedRunner{}
	require.NoError(t, KubectlMoveCluster{Server: host}.CreateSourceVolume(SourceVolume{
		PV: "kipper-move-src-01234567", Claim: "kipper-move-src-db", Namespace: "shop",
		Path: "/var/lib/rancher/k3s/storage/pvc-1_shop_data-db-0", Node: "server-1", Capacity: "2Gi",
	}))
	require.Len(t, host.stdins, 2)
	for i, what := range []string{"source volume", "source claim"} {
		var obj struct {
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		}
		require.NoError(t, json.Unmarshal([]byte(host.stdins[i]), &obj))
		assert.Equal(t, "true", obj.Metadata.Labels["velero.io/exclude-from-backup"], what)
	}

	host = &routedRunner{routes: []route{{contains: "kubectl create", out: "kipper-uid[pod-1]"}}}
	uid, err := KubectlMoveCluster{Server: host}.StartCopy(CopyPod{Namespace: "shop", Name: "kipper-move-copy-db", Source: "kipper-move-src-db", Target: "data-db-0"})
	require.NoError(t, err)
	assert.Equal(t, "pod-1", uid, "the move records which pod is its own")
	var pod struct {
		Metadata struct {
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	require.NoError(t, json.Unmarshal([]byte(host.stdins[0]), &pod))
	assert.Equal(t, "true", pod.Metadata.Labels["velero.io/exclude-from-backup"])
	assert.Equal(t, "src,dst", pod.Metadata.Annotations["backup.velero.io/backup-volumes-excludes"])
}

// cp writes through the page cache, and dirty pages awaiting a slow Longhorn
// device count towards container memory. Copying a 6 GiB Postgres volume
// with a 128 MiB limit caused the container to be OOM-killed.
func TestKubectlMoveClusterGivesTheCopyRoomForItsWriteCache(t *testing.T) {
	host := &routedRunner{routes: []route{{contains: "kubectl create", out: "kipper-uid[pod-1]"}}}
	_, err := KubectlMoveCluster{Server: host}.StartCopy(CopyPod{Namespace: "shop", Name: "kipper-move-copy-db", Source: "kipper-move-src-db", Target: "data-db-0"})
	require.NoError(t, err)
	var pod corev1.Pod
	require.NoError(t, json.Unmarshal([]byte(host.stdins[0]), &pod))
	require.Len(t, pod.Spec.Containers, 1)
	limit := pod.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]
	assert.GreaterOrEqual(t, limit.Cmp(resource.MustParse("1Gi")), 0, "memory limit %s", limit.String())
}

// A copy pod the kernel kills prints nothing of its own, so the script names
// each phase as it starts and the log tail shows where it stopped.
func TestMoveCopyScriptNamesItsPhases(t *testing.T) {
	assert.Contains(t, moveCopyScript, `echo "copying the data"`)
	assert.Contains(t, moveCopyScript, `echo "flushing the copy to the volume"`)
	assert.Contains(t, moveVerifyScript, `echo "comparing the copy with the source"`)
}

// cp returns once the data is in the page cache. syncfs reports a write-back
// error on the Longhorn volume, which would otherwise surface at unmount where
// nobody reads it, so the copy succeeds only once the flush did.
func TestMoveCopyScriptFlushesTheCopy(t *testing.T) {
	assert.True(t, strings.HasPrefix(moveCopyScript, "set -eu -o pipefail\n"), "any failing command fails the copy")
	cp, flush, done := strings.Index(moveCopyScript, "cp -a /src/. /dst/"), strings.Index(moveCopyScript, "sync -f /dst"), strings.Index(moveCopyScript, "kipper-move-copied")
	require.Positive(t, cp)
	assert.Greater(t, flush, cp, "the flush follows the copy")
	assert.Greater(t, done, flush, "the copy reports success only after the flush")
	assert.NotContains(t, moveCopyScript, "sha256sum", "the copy pod no longer verifies; its reads would come from its own page cache")
}

func TestKubectlMoveClusterMeasuresTheLargerSize(t *testing.T) {
	host := &routedRunner{routes: []route{{contains: "du -s", out: "kipper-used[4096 1073741824]"}}}
	used, err := KubectlMoveCluster{Server: host}.UsedBytes("/var/lib/rancher/k3s/storage/x")
	require.NoError(t, err)
	assert.Equal(t, int64(1073741824), used, "a sparse file is filled by the copy")
	assert.Contains(t, host.commands[0], "--apparent-size")
	assert.Equal(t, 2, strings.Count(host.commands[0], "du -s -l "), "a hard-linked file is copied once per name, so it is counted once per name")
}

func TestKubectlMoveClusterCountsTheEntriesTheVerifyCompares(t *testing.T) {
	host := &routedRunner{routes: []route{{contains: "find ", out: "kipper-entries[151357]"}}}
	n, err := KubectlMoveCluster{Server: host}.CountEntries("/var/lib/rancher/k3s/storage/x")
	require.NoError(t, err)
	assert.Equal(t, int64(151357), n)
	assert.Contains(t, host.commands[0], "find '/var/lib/rancher/k3s/storage/x'")
	assert.Contains(t, host.commands[0], "set -o pipefail", "a walk that failed gives no count")

	_, err = KubectlMoveCluster{Server: &routedRunner{routes: []route{{contains: "find ", out: "find: permission denied"}}}}.CountEntries("/x")
	assert.Error(t, err)
}

// The copy pod has a fixed memory limit, so the comparison may not hold the
// whole tree: BusyBox sort keeps its input in memory, and a manifest file in
// /tmp lives in the container's writable layer.
func TestMoveCopyScriptStreamsItsComparison(t *testing.T) {
	assert.NotContains(t, moveVerifyScript, "sort")
	assert.NotContains(t, moveVerifyScript, "/tmp/")
	assert.Contains(t, moveVerifyScript, "set -eu -o pipefail")
	assert.NotContains(t, moveVerifyScript, "rm ", "the verify pod only reads")
}

func TestKubectlMoveClusterReadsTheCountAMoveRestores(t *testing.T) {
	marked := `{"metadata":{"name":"db","annotations":{"kipper.run/storage-move":"kipper-storage-move-u","kipper.run/storage-move-replicas":"2"}},"spec":{"replicas":0}}`
	sts, err := KubectlMoveCluster{Server: &routedRunner{routes: []route{{contains: "get statefulset", out: marked}}}}.ReadStatefulSet("shop", "db")
	require.NoError(t, err)
	assert.Equal(t, int32(2), sts.Replicas)

	running := `{"metadata":{"name":"db"},"spec":{"replicas":3}}`
	sts, err = KubectlMoveCluster{Server: &routedRunner{routes: []route{{contains: "get statefulset", out: running}}}}.ReadStatefulSet("shop", "db")
	require.NoError(t, err)
	assert.Equal(t, int32(3), sts.Replicas)
}

func TestKubectlMoveClusterFindsTheVolumeAnUnfinishedMoveKept(t *testing.T) {
	pv := func(name, uid, state string) string {
		return `{"metadata":{"name":"` + name + `","uid":"v-` + name + `","labels":{"kipper.run/retained-local-copy":"true"},"annotations":{` +
			`"kipper.run/retained-local-copy-service-uid":"` + uid + `","kipper.run/retained-local-copy-state":"` + state + `"}},` +
			`"spec":{"local":{"path":"/var/lib/rancher/k3s/storage/` + name + `"}}}`
	}
	list := func(items ...string) *routedRunner {
		return &routedRunner{routes: []route{{contains: "get pv -l kipper.run/retained-local-copy=true", out: `{"items":[` + strings.Join(items, ",") + `]}`}}}
	}
	got, err := KubectlMoveCluster{Server: list(pv("pvc-a", "other", "moving"), pv("pvc-b", "u1", "moved"), pv("pvc-c", "u1", "moving"))}.RetainedVolume("u1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "pvc-c", got.Name)
	assert.Equal(t, "/var/lib/rancher/k3s/storage/pvc-c", got.Path)

	got, err = KubectlMoveCluster{Server: list(pv("pvc-a", "other", "moving"))}.RetainedVolume("u1")
	require.NoError(t, err)
	assert.Nil(t, got)

	_, err = KubectlMoveCluster{Server: list(pv("pvc-c", "u1", "moving"), pv("pvc-d", "u1", "moving"))}.RetainedVolume("u1")
	assert.Error(t, err, "two candidates leave kip unable to tell which one holds the data")
}

const namespacesBinding = `{"metadata":{"name":"kipper-storage-move-namespaces"},"spec":{"policyName":"kipper-storage-move-namespaces","validationActions":["Deny"]}}`

func TestKubectlMoveClusterProbesTheNamespaceDeleteGuard(t *testing.T) {
	probe := func(binding, out string, err error) *routedRunner {
		return &routedRunner{routes: []route{
			{contains: "get validatingadmissionpolicybinding kipper-storage-move-namespaces", out: binding},
			{contains: "delete namespace 'shop' --dry-run=server", out: out, err: err},
		}}
	}
	refused := probe(namespacesBinding, `Error from server (Forbidden): namespaces "shop" is forbidden: ValidatingAdmissionPolicy 'kipper-storage-move-namespaces' with binding 'kipper-storage-move-namespaces' denied request: finish or roll back the move first`, errors.New("exit 1"))
	ok, err := KubectlMoveCluster{Server: refused}.NamespaceDeleteRefused("shop")
	require.NoError(t, err)
	assert.True(t, ok)

	admitted := probe(namespacesBinding, `namespace "shop" deleted (server dry run)`, nil)
	ok, err = KubectlMoveCluster{Server: admitted}.NamespaceDeleteRefused("shop")
	require.NoError(t, err)
	assert.False(t, ok, "a dry run that went through is no refusal")

	podsDenial := probe(namespacesBinding, `ValidatingAdmissionPolicy 'kipper-storage-move-pods' with binding 'kipper-storage-move-pods' denied request`, errors.New("exit 1"))
	_, err = KubectlMoveCluster{Server: podsDenial}.NamespaceDeleteRefused("shop")
	assert.Error(t, err, "only the namespace policy's denial counts")

	warn := probe(`{"spec":{"policyName":"kipper-storage-move-namespaces","validationActions":["Warn"]}}`, "", nil)
	_, err = KubectlMoveCluster{Server: warn}.NamespaceDeleteRefused("shop")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip upgrade")
	assert.False(t, warn.ran("delete namespace"), "nothing is sent before the binding is known to deny")
}

func TestKubectlMoveClusterSeesANamespaceBeingDeleted(t *testing.T) {
	const live = `{"metadata":{"name":"shop","uid":"ns-1"}}`
	const deleting = `{"metadata":{"name":"shop","uid":"ns-1","deletionTimestamp":"2026-10-25T08:00:00Z"}}`
	const projects = `{"items":[
{"metadata":{"name":"other","deletionTimestamp":"2026-10-25T08:00:00Z"},"status":{"namespaces":["other"]}},
{"metadata":{"name":"acme"},"status":{"namespaceClaims":[{"name":"shop","uid":"ns-1"}]}}]}`
	const acmeDeleting = `{"items":[
{"metadata":{"name":"acme","deletionTimestamp":"2026-10-25T08:00:00Z"},"status":{"namespaceClaims":[{"name":"shop","uid":"ns-1"}]}}]}`
	const recordedOnly = `{"items":[
{"metadata":{"name":"acme","deletionTimestamp":"2026-10-25T08:00:00Z"},"status":{"namespaces":["shop"]}}]}`
	read := func(ns, projects string) NamespaceDeletion {
		d, err := KubectlMoveCluster{Server: &routedRunner{routes: []route{
			{contains: "get namespace 'shop'", out: ns},
			{contains: "get projects.kipper.run", out: projects},
		}}}.NamespaceTerminating("shop")
		require.NoError(t, err)
		return d
	}
	assert.Equal(t, NamespaceDeletion{}, read(live, projects))
	assert.Equal(t, NamespaceDeletion{Namespace: true, Why: "namespace shop is being deleted"}, read(deleting, projects))
	assert.Equal(t, NamespaceDeletion{Why: "project acme, which holds namespace shop, is being deleted"}, read(live, acmeDeleting),
		"a Project being deleted leaves its namespace active")
	assert.Equal(t, NamespaceDeletion{Why: "project acme, which holds namespace shop, is being deleted"}, read(live, recordedOnly))

	_, err := KubectlMoveCluster{Server: &routedRunner{routes: []route{
		{contains: "get namespace", out: live},
		{contains: "get projects.kipper.run", out: "Unable to connect to the server", err: errors.New("exit 1")},
	}}}.NamespaceTerminating("shop")
	assert.Error(t, err, "an unreadable Project list is no all-clear")
}

func TestKubectlMoveClusterListsClaimsByLabel(t *testing.T) {
	host := &routedRunner{routes: []route{{contains: "get pvc -l", out: `{"items":[{"metadata":{"name":"db-dump","uid":"d1","labels":{"kipper.run/dump-for":"db"}},"spec":{"storageClassName":"local-path"},"status":{"phase":"Bound"}}]}`}}}
	claims, err := KubectlMoveCluster{Server: host}.ClaimsLabeled("shop", "kipper.run/dump-for=db")
	require.NoError(t, err)
	require.Len(t, claims, 1)
	assert.Equal(t, "db-dump", claims[0].Name)
	assert.Equal(t, platform.LocalPathClass, claims[0].Class)
	assert.True(t, host.ran("kubectl -n 'shop' get pvc -l 'kipper.run/dump-for=db'"))
}

func TestKubectlMoveClusterLabelsTheSourceClaim(t *testing.T) {
	host := &routedRunner{}
	require.NoError(t, KubectlMoveCluster{Server: host}.CreateSourceVolume(SourceVolume{
		PV: "kipper-move-src-01234567", Claim: "kipper-move-src-db", Namespace: "shop", Service: "db",
		Path: "/var/lib/rancher/k3s/storage/pvc-1_shop_data-db-0", Node: "server-1", Capacity: "2Gi",
	}))
	require.Len(t, host.stdins, 2)
	assert.Contains(t, host.stdins[1], `"kipper.run/storage-move-service":"db"`)
}

func TestKubectlMoveClusterReadsTheResourcesAMoveNeeds(t *testing.T) {
	host := &routedRunner{routes: []route{
		{contains: "kubectl get nodes", out: listJSON(t, moveNode("server-1", "2", "4Gi"))},
		{contains: "kubectl get pods -A", out: listJSON(t, servicePod("shop", "db", "server-1", "500m", "1Gi"))},
		{contains: "kubectl -n 'shop' get resourcequota", out: listJSON(t, moveQuota("kipper-quota", list("pods", "10"), list("pods", "1")))},
		{contains: "kubectl get priorityclasses", out: `{"items":[{"metadata":{"name":"system-node-critical"},"value":2000001000},` +
			`{"metadata":{"name":"low"},"value":10,"globalDefault":true},{"metadata":{"name":"standard"},"value":100,"globalDefault":true}]}`},
	}}
	res, err := KubectlMoveCluster{Server: host}.ResourceState("shop")
	require.NoError(t, err)
	assert.Equal(t, "low", res.DefaultPriorityClass, "the API server gives a pod without a class the lowest global default")
	require.Len(t, res.Nodes, 1)
	require.Len(t, res.Pods, 1)
	require.Len(t, res.Quotas, 1)
	assert.Equal(t, "kipper-quota", res.Quotas[0].Name)
	assert.Equal(t, "db", res.Pods[0].Labels["app"])
}
