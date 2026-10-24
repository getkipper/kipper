package installer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const driftedReplicatedClass = `{"kind":"StorageClass","metadata":{"name":"longhorn-replicated","uid":"abc"},"provisioner":"driver.longhorn.io","parameters":{"numberOfReplicas":"2","dataLocality":"best-effort","nodeSelector":"kipper-replica","replicaAutoBalance":"disabled","staleReplicaTimeout":"30","fsType":"ext4","dataEngine":"v1"}}`

func TestTheReplicatedClassIsCreatedWithLocalityAndAutoBalanceOff(t *testing.T) {
	h := pinHost(map[string]string{"get storageclass longhorn-replicated": ""})
	_, err := PinSingleCopiesToServer(h)
	require.NoError(t, err)
	created := h.ranWith("name: longhorn-replicated")
	require.Len(t, created, 1)
	for _, want := range []string{`numberOfReplicas: "2"`, `dataLocality: "disabled"`, `nodeSelector: "kipper-replica"`, `replicaAutoBalance: "disabled"`, `staleReplicaTimeout: "30"`, `dataEngine: "v1"`, "reclaimPolicy: Delete", "allowVolumeExpansion: true"} {
		assert.Contains(t, created[0], want)
	}
	assert.Empty(t, h.ranWith("delete storageclass"))
}

func TestAReplicatedClassWhoseParametersDriftedIsRecreated(t *testing.T) {
	h := pinHost(map[string]string{"get storageclass longhorn-replicated": driftedReplicatedClass})
	_, err := PinSingleCopiesToServer(h)
	require.NoError(t, err)
	require.Len(t, h.ranWith("delete storageclass longhorn-replicated"), 1, "StorageClass parameters cannot change in place")
	all := strings.Join(h.commands, "\n")
	assert.Less(t, strings.Index(all, "delete storageclass longhorn-replicated"), strings.Index(all, "name: longhorn-replicated"))
	assert.Empty(t, h.ranWith("delete storageclass longhorn-single"), "the pinned single-copy class stays")

	h = pinHost(nil)
	_, err = PinSingleCopiesToServer(h)
	require.NoError(t, err)
	assert.Empty(t, h.ranWith("kubectl apply"), "current classes are left alone")
}

func TestTheServerIsAlwaysAReplicaNode(t *testing.T) {
	h := pinHost(map[string]string{"get nodes.longhorn.io": `["kipper-server"]`})
	_, err := PinSingleCopiesToServer(h)
	require.NoError(t, err)
	patches := h.ranWith("patch nodes.longhorn.io")
	require.Len(t, patches, 1)
	assert.Contains(t, patches[0], `{"spec":{"tags":["kipper-server","kipper-replica"]}}`)
}
