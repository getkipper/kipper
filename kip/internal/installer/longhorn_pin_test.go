package installer

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	serverStorageWait = joinTimeouts{ready: time.Millisecond, interval: time.Millisecond}
	pinWait = joinTimeouts{ready: time.Millisecond, interval: time.Millisecond}
}

const (
	replicatedClass = `{"kind":"StorageClass","metadata":{"name":"longhorn-replicated"},"provisioner":"driver.longhorn.io","parameters":{"numberOfReplicas":"2","dataLocality":"disabled","nodeSelector":"kipper-replica","replicaAutoBalance":"disabled","staleReplicaTimeout":"30","fsType":"ext4","dataEngine":"v1","disableRevisionCounter":"false"}}`
	pinnedClass     = `{"kind":"StorageClass","metadata":{"name":"longhorn-single","uid":"abc","resourceVersion":"7"},"provisioner":"driver.longhorn.io","parameters":{"numberOfReplicas":"1","nodeSelector":"kipper-server"}}`
	unpinnedClass   = `{"kind":"StorageClass","metadata":{"name":"longhorn-single","uid":"abc","resourceVersion":"7"},"provisioner":"driver.longhorn.io","parameters":{"numberOfReplicas":"1"}}`
)

func pinHost(replies map[string]string) *drainHost {
	base := map[string]string{
		"get nodes -l":                         "node/server-1\n",
		"get nodes --request":                  "node/server-1\nnode/worker-1\n",
		"get nodes.longhorn.io":                `["kipper-server","kipper-replica"]`,
		"get storageclass longhorn-single":     pinnedClass,
		"get storageclass longhorn-replicated": replicatedClass,
		"get pv ":                              "",
		"get volumes.longhorn.io":              "",
		"replicas.longhorn.io":                 "",
	}
	for k, v := range replies {
		base[k] = v
	}
	return &drainHost{replies: base}
}

func (h *drainHost) ranWith(fragment string) []string {
	var out []string
	for _, c := range h.commands {
		if strings.Contains(c, fragment) {
			out = append(out, c)
		}
	}
	return out
}

func TestServerStorageTagIsAddedAndKeepsTheNodesOtherTags(t *testing.T) {
	h := pinHost(map[string]string{"get nodes.longhorn.io": `["ssd"]`})
	_, err := PinSingleCopiesToServer(h)
	require.NoError(t, err)
	patches := h.ranWith("patch nodes.longhorn.io")
	require.Len(t, patches, 1)
	assert.Contains(t, patches[0], "nodes.longhorn.io server-1 ")
	assert.Contains(t, patches[0], `{"spec":{"tags":["ssd","kipper-server","kipper-replica"]}}`)

	h = pinHost(nil)
	_, err = PinSingleCopiesToServer(h)
	require.NoError(t, err)
	assert.Empty(t, h.ranWith("patch nodes.longhorn.io"), "a tagged server is left alone")
}

func TestServerStorageTagNeedsExactlyOneServer(t *testing.T) {
	h := pinHost(map[string]string{"get nodes -l": "node/server-1\nnode/server-2\n"})
	_, err := PinSingleCopiesToServer(h)
	assert.ErrorContains(t, err, "expected one server node")
}

func TestSingleCopyClassIsRecreatedWithTheServerSelector(t *testing.T) {
	h := pinHost(map[string]string{"get storageclass longhorn-single": unpinnedClass})
	_, err := PinSingleCopiesToServer(h)
	require.NoError(t, err)
	require.Len(t, h.ranWith("delete storageclass longhorn-single"), 1, "StorageClass parameters cannot change in place")
	created := h.ranWith(`nodeSelector: "kipper-server"`)
	require.Len(t, created, 1)
	assert.Contains(t, created[0], `numberOfReplicas: "1"`)
	all := strings.Join(h.commands, "\n")
	assert.Less(t, strings.Index(all, "delete storageclass"), strings.Index(all, `nodeSelector: "kipper-server"`))

	h = pinHost(map[string]string{"get storageclass longhorn-single": ""})
	_, err = PinSingleCopiesToServer(h)
	require.NoError(t, err)
	assert.Empty(t, h.ranWith("delete storageclass"))
	assert.Len(t, h.ranWith(`nodeSelector: "kipper-server"`), 1, "a missing class is created")

	h = pinHost(nil)
	_, err = PinSingleCopiesToServer(h)
	require.NoError(t, err)
	assert.Empty(t, h.ranWith("kubectl apply"), "a pinned class is left alone")
	assert.Empty(t, h.ranWith("delete storageclass"))
}

func TestSingleCopyClassWithAnotherReplicaCountIsRecreated(t *testing.T) {
	drifted := strings.Replace(pinnedClass, `"numberOfReplicas":"1"`, `"numberOfReplicas":"2"`, 1)
	h := pinHost(map[string]string{"get storageclass longhorn-single": drifted})
	_, err := PinSingleCopiesToServer(h)
	require.NoError(t, err)
	require.Len(t, h.ranWith("delete storageclass longhorn-single"), 1, "copies 1 must mean one replica")
	created := h.ranWith(`nodeSelector: "kipper-server"`)
	require.Len(t, created, 1)
	assert.Contains(t, created[0], `numberOfReplicas: "1"`)
}

func TestExistingSingleCopyVolumesArePinnedToTheServer(t *testing.T) {
	h := pinHost(map[string]string{
		"get pv ": "longhorn-single\tpvc-db\nlonghorn\tpvc-shared\nlonghorn-single\tpvc-zot\nlonghorn-single\tpvc-done\nlonghorn-single\tpvc-custom\nlonghorn-single\tpvc-converted\n",
		"get volumes.longhorn.io": "pvc-db\t\tshop\tdata-db-0\t1\t5\n" +
			"pvc-shared\t\tshop\tfiles\t1\t5\n" +
			"pvc-zot\tnull\tkipper-system\tzot-data\t1\t5\n" +
			"pvc-done\t[\"kipper-server\"]\tshop\tdata-cache-0\t1\t5\n" +
			"pvc-custom\t[\"ssd\",\"kipper-server\"]\tshop\tdata-queue-0\t1\t5\n" +
			"pvc-converted\t[\"kipper-replica\"]\tshop\tdata-search-0\t2\t5\n",
	})
	placement, err := PinSingleCopiesToServer(h)
	require.NoError(t, err)
	patches := h.ranWith("patch volumes.longhorn.io")
	require.Len(t, patches, 3, "a single-copy class volume converted to two copies stays as it is")
	assert.Contains(t, patches[0], "volumes.longhorn.io pvc-db ")
	assert.Contains(t, patches[1], "volumes.longhorn.io pvc-zot ")
	assert.Contains(t, patches[2], "volumes.longhorn.io pvc-custom ")
	for _, p := range patches {
		assert.Contains(t, p, `{"metadata":{"resourceVersion":"5"},"spec":{"nodeSelector":["kipper-server"]}}`, "Longhorn needs every selected tag, so the server tag stands alone")
	}
	assert.Equal(t, []string{"shop/data-db-0", "kipper-system/zot-data", "shop/data-queue-0"}, placement.Pinned)
	assert.Empty(t, placement.Unpinned)
}

func TestARefusedVolumePatchIsReportedAndTheRestStillRun(t *testing.T) {
	h := pinHost(map[string]string{
		"get pv ":                 "longhorn-single\tpvc-db\nlonghorn-single\tpvc-zot\n",
		"get volumes.longhorn.io": "pvc-db\t\tshop\tdata-db-0\t1\t5\npvc-zot\t\tkipper-system\tzot-data\t1\t5\n",
	})
	h.failures = map[string]string{"patch volumes.longhorn.io pvc-db ": "admission webhook denied the request"}
	placement, err := PinSingleCopiesToServer(h)
	require.NoError(t, err)
	assert.Equal(t, []string{"kipper-system/zot-data"}, placement.Pinned)
	require.Len(t, placement.Unpinned, 1)
	assert.Contains(t, placement.Unpinned[0], "shop/data-db-0")
	assert.Contains(t, placement.Unpinned[0], "exit status 1")
}

func TestSingleCopiesOffTheServerAreListedWithTheirNode(t *testing.T) {
	h := pinHost(map[string]string{
		"get pv ": "longhorn-single\tpvc-db\nlonghorn\tpvc-loki\nlonghorn-single\tpvc-zot\n",
		"get volumes.longhorn.io": "pvc-db\t[\"kipper-server\"]\tshop\tdata-db-0\t1\t5\n" +
			"pvc-loki\t\tmonitoring\tstorage-loki-0\t1\t5\n" +
			"pvc-zot\t[\"kipper-server\"]\tkipper-system\tzot-data\t1\t5\n",
		"replicas.longhorn.io": "pvc-db\tworker-2\t2026-10-16T01:00:00Z\t\t\n" +
			"pvc-loki\tworker-1\t2026-10-16T01:00:00Z\t\t\n" +
			"pvc-zot\tserver-1\t2026-10-16T01:00:00Z\t\t\n",
	})
	placement, err := PinSingleCopiesToServer(h)
	require.NoError(t, err)
	assert.Equal(t, []OffServerCopy{{Volume: "shop/data-db-0", Node: "worker-2", Pinned: true}}, placement.OffServer,
		"replicated volumes and copies already on the server are not listed")
}

func TestOffServerNoticeNamesTheRebootThatMovesTheCopy(t *testing.T) {
	assert.Empty(t, OffServerNotice(nil))
	assert.Equal(t, "Single-copy volumes off the server: gate/data-drainme-0 (on worker-2). 'kip node reboot worker-2' moves the copy to the server when the server has room for it; otherwise the reboot refuses: the drain blocks until --drain-timeout, then fails, and kip cancels the reboot.",
		OffServerNotice([]OffServerCopy{{Volume: "gate/data-drainme-0", Node: "worker-2", Pinned: true}}))
	assert.Equal(t, "Single-copy volumes off the server: shop/a (on worker-1), shop/b (on worker-2). 'kip node reboot <worker>' moves the copy to the server when the server has room for it; otherwise the reboot refuses: the drain blocks until --drain-timeout, then fails, and kip cancels the reboot.",
		OffServerNotice([]OffServerCopy{{Volume: "shop/a", Node: "worker-1", Pinned: true}, {Volume: "shop/b", Node: "worker-2", Pinned: true}}))
}

func TestAnUnpinnedCopyIsNotPromisedAMoveToTheServer(t *testing.T) {
	notice := OffServerNotice([]OffServerCopy{{Volume: "shop/a", Node: "worker-1"}})
	assert.NotContains(t, notice, "moves the copy to the server", "without the selector a reboot can pick another worker")
	assert.Contains(t, notice, "Single-copy volumes not pinned to the server: shop/a (on worker-1).")
	assert.Contains(t, notice, "Run 'kip upgrade' to pin them.")
}

func TestARefusedPatchLeavesTheCopyListedAsNotPinned(t *testing.T) {
	h := pinHost(map[string]string{
		"get pv ":                 "longhorn-single\tpvc-db\n",
		"get volumes.longhorn.io": "pvc-db\t\tshop\tdata-db-0\t1\t5\n",
		"replicas.longhorn.io":    "pvc-db\tworker-2\t2026-10-16T01:00:00Z\t\t\n",
	})
	h.failures = map[string]string{"patch volumes.longhorn.io": "denied"}
	placement, err := PinSingleCopiesToServer(h)
	require.NoError(t, err)
	assert.Equal(t, []OffServerCopy{{Volume: "shop/data-db-0", Node: "worker-2"}}, placement.OffServer)
}

func TestAReplacementThatKeepsFailingPutsThePreviousClassBack(t *testing.T) {
	h := pinHost(map[string]string{"get storageclass longhorn-single": unpinnedClass})
	h.failures = map[string]string{`nodeSelector: "kipper-server"`: "denied by policy"}
	_, err := PinSingleCopiesToServer(h)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "The previous class was restored")
	restored := h.ranWith(`"provisioner":"driver.longhorn.io"`)
	require.Len(t, restored, 1)
	assert.NotContains(t, restored[0], "resourceVersion", "a restored class is created afresh")
	assert.NotContains(t, restored[0], `"uid"`)
}

func TestAClassThatCannotBeCreatedAgainSaysHowToRecover(t *testing.T) {
	h := pinHost(map[string]string{"get storageclass longhorn-single": unpinnedClass})
	h.failures = map[string]string{"kubectl apply": "the server is currently unable to handle the request"}
	_, err := PinSingleCopiesToServer(h)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Run 'kip upgrade' again to create it")
	assert.NotEmpty(t, h.ranWith("delete storageclass"))
}

func TestASingleNodeClusterSkipsTheVolumeAndReplicaReads(t *testing.T) {
	h := pinHost(map[string]string{"get nodes --request": "node/server-1\n", "get storageclass longhorn-single": unpinnedClass})
	placement, err := PinSingleCopiesToServer(h)
	require.NoError(t, err)
	assert.NotEmpty(t, h.ranWith(`nodeSelector: "kipper-server"`), "the class is still pinned for the first worker")
	assert.Empty(t, h.ranWith("get pv "))
	assert.Empty(t, h.ranWith("replicas.longhorn.io"))
	assert.Empty(t, placement.OffServer)
}

func TestPinningForAnUpgradeGivesUpQuicklyOnALonghornProblem(t *testing.T) {
	saved, savedPin := serverStorageWait, pinWait
	serverStorageWait = joinTimeouts{ready: 10 * time.Second, interval: 100 * time.Millisecond}
	pinWait = joinTimeouts{ready: 200 * time.Millisecond, interval: 100 * time.Millisecond}
	t.Cleanup(func() { serverStorageWait, pinWait = saved, savedPin })
	h := pinHost(nil)
	h.failures = map[string]string{"get nodes.longhorn.io": "the server is currently unable to handle the request"}
	start := time.Now()
	_, err := PinSingleCopiesToServer(h)
	require.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second, "only a fresh install waits for Longhorn to register the server")
}

func TestEveryPinRequestIsBoundedByARequestTimeout(t *testing.T) {
	h := pinHost(map[string]string{
		"get nodes.longhorn.io":            `[]`,
		"get storageclass longhorn-single": unpinnedClass,
		"get pv ":                          "longhorn-single\tpvc-db\n",
		"get volumes.longhorn.io":          "pvc-db\t\tshop\tdata-db-0\t1\t5\n",
	})
	_, err := PinSingleCopiesToServer(h)
	require.NoError(t, err)
	for _, c := range h.commands {
		if strings.Contains(c, "kubectl") {
			assert.Contains(t, c, "--request-timeout=", "a stalled API request must not hold the upgrade: %s", c)
		}
	}
}

func TestTheFirstJoinPinsExistingVolumesWhileTheServerIsTheOnlyNode(t *testing.T) {
	h := pinHost(map[string]string{
		"get nodes --request":     "node/server-1\n",
		"get pv ":                 "longhorn-single\tpvc-db\n",
		"get volumes.longhorn.io": "pvc-db\t\tshop\tdata-db-0\t1\t5\n",
	})
	placement, err := PinSingleCopiesForJoin(h)
	require.NoError(t, err)
	require.Len(t, h.ranWith("patch volumes.longhorn.io pvc-db "), 1, "a volume from before the first worker must be pinned before that worker can hold it")
	assert.Equal(t, []string{"shop/data-db-0"}, placement.Pinned)
}

func TestAJoinRefusesWhenAnExistingVolumeCannotBePinned(t *testing.T) {
	replies := map[string]string{
		"get nodes --request":     "node/server-1\n",
		"get pv ":                 "longhorn-single\tpvc-db\n",
		"get volumes.longhorn.io": "pvc-db\t\tshop\tdata-db-0\t1\t5\n",
	}
	h := pinHost(replies)
	h.failures = map[string]string{"patch volumes.longhorn.io pvc-db ": "admission webhook denied the request"}
	_, err := PinSingleCopiesForJoin(h)
	require.Error(t, err, "a worker must not join while an existing copy could still move to it")
	assert.Contains(t, err.Error(), "shop/data-db-0")
	assert.Contains(t, err.Error(), "run kip node add again")

	replies["get nodes --request"] = "node/server-1\nnode/worker-1\n"
	h = pinHost(replies)
	h.failures = map[string]string{"patch volumes.longhorn.io pvc-db ": "admission webhook denied the request"}
	placement, err := PinSingleCopiesToServer(h)
	require.NoError(t, err, "an upgrade only warns")
	assert.Len(t, placement.Unpinned, 1)
}

// conflictHost injects resourceVersion conflicts until its conflicts counter
// reaches zero and returns reread when the volume is read again.
type conflictHost struct {
	*drainHost
	reread    string
	conflicts int
}

func (h *conflictHost) Run(cmd string) (string, error) {
	if strings.Contains(cmd, "patch volumes.longhorn.io") && h.conflicts > 0 {
		h.conflicts--
		h.commands = append(h.commands, cmd)
		return `Error from server (Conflict): Operation cannot be fulfilled on volumes.longhorn.io "pvc-db": the object has been modified; please apply your changes to the latest version and try again`, errors.New("exit status 1")
	}
	if strings.Contains(cmd, "get volumes.longhorn.io pvc-db ") {
		h.commands = append(h.commands, cmd)
		return h.reread, nil
	}
	return h.drainHost.Run(cmd)
}

func conflictingPin(reread string) *conflictHost {
	return &conflictHost{drainHost: pinHost(map[string]string{
		"get pv ":                 "longhorn-single\tpvc-db\n",
		"get volumes.longhorn.io": "pvc-db\t\tshop\tdata-db-0\t1\t5\n",
	}), reread: reread, conflicts: 1}
}

// A copies conversion that raised the count after the listing changed the
// volume, so the pin leaves its selector alone.
func TestAPinNeverOverwritesAVolumeConvertedAfterTheListing(t *testing.T) {
	h := conflictingPin("[\"kipper-replica\"]\t2\t9")

	placement, err := PinSingleCopiesForJoin(h)

	require.NoError(t, err, "a converted volume no longer needs a pin")
	assert.Len(t, h.ranWith("patch volumes.longhorn.io"), 1)
	assert.Empty(t, placement.Pinned)
	assert.Empty(t, placement.Unpinned)
}

// Longhorn updates a volume's status often, so a conflict on a volume that
// still has one copy is retried with the fresh resourceVersion.
func TestAPinRetriesAConflictOnAVolumeThatStillHasOneCopy(t *testing.T) {
	h := conflictingPin("\t1\t9")

	placement, err := PinSingleCopiesToServer(h)

	require.NoError(t, err)
	patches := h.ranWith("patch volumes.longhorn.io")
	require.Len(t, patches, 2)
	assert.Contains(t, patches[1], `{"metadata":{"resourceVersion":"9"},"spec":{"nodeSelector":["kipper-server"]}}`)
	assert.Equal(t, []string{"shop/data-db-0"}, placement.Pinned)
}
