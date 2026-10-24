package installer

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var quickTagWait = joinTimeouts{ready: time.Second, interval: time.Millisecond}

func tagHost(tags string) *hostScript {
	return &hostScript{reply: func(command string) (string, error) {
		if strings.Contains(command, "jsonpath='{.spec.tags}'") {
			return tags, nil
		}
		return "", nil
	}}
}

func patchOf(t *testing.T, h *hostScript) string {
	t.Helper()
	for _, c := range h.commands {
		if strings.Contains(c, "patch nodes.longhorn.io") {
			return c
		}
	}
	return ""
}

func TestSetReplicaTagAddsTheTagAndKeepsTheOthers(t *testing.T) {
	h := tagHost(`["kipper-server"]`)
	require.NoError(t, setReplicaTag(h, "worker-1", true, quickTagWait))
	assert.Contains(t, patchOf(t, h), `nodes.longhorn.io worker-1`)
	assert.Contains(t, patchOf(t, h), `{"spec":{"tags":["kipper-server","kipper-replica"]}}`)
}

func TestSetReplicaTagRemovesOnlyTheReplicaTag(t *testing.T) {
	h := tagHost(`["kipper-server","kipper-replica"]`)
	require.NoError(t, setReplicaTag(h, "worker-1", false, quickTagWait))
	assert.Contains(t, patchOf(t, h), `{"spec":{"tags":["kipper-server"]}}`)
}

func TestSetReplicaTagThatIsAlreadyRightWritesNothing(t *testing.T) {
	h := tagHost(`["kipper-replica"]`)
	require.NoError(t, setReplicaTag(h, "worker-1", true, quickTagWait))
	assert.Empty(t, patchOf(t, h))
}

func TestSetReplicaTagRefusesANodeNameThatIsNotOne(t *testing.T) {
	h := tagHost(`[]`)
	assert.Error(t, setReplicaTag(h, "worker-1; rm -rf /", true, quickTagWait))
	assert.Empty(t, h.commands)
}

func TestSetReplicaTagQuotesTagsItReadBack(t *testing.T) {
	h := tagHost(`["it's'; touch /tmp/x; '"]`)
	require.NoError(t, setReplicaTag(h, "worker-1", true, quickTagWait))
	assert.Contains(t, patchOf(t, h), `-p '{"spec":{"tags":["it'\''s'\''; touch /tmp/x; '\''","kipper-replica"]}}'`,
		"a quote in an existing tag stays inside the argument")
}

func TestReplicaTaggedReadsTheTag(t *testing.T) {
	tagged, err := ReplicaTagged(tagHost(`["kipper-server","kipper-replica"]`), "worker-1")
	require.NoError(t, err)
	assert.True(t, tagged)
	tagged, err = ReplicaTagged(tagHost(`["kipper-server"]`), "worker-1")
	require.NoError(t, err)
	assert.False(t, tagged)
	_, err = ReplicaTagged(tagHost(`not json`), "worker-1")
	assert.Error(t, err)
}

func TestReplicasOnNodeCountsItsReplicas(t *testing.T) {
	h := &hostScript{reply: func(command string) (string, error) {
		return "worker-1\nserver-1\nworker-1\n", nil
	}}
	n, err := ReplicasOnNode(h, "worker-1")
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	h = &hostScript{reply: func(string) (string, error) { return "", errors.New("unreachable") }}
	_, err = ReplicasOnNode(h, "worker-1")
	assert.Error(t, err, "an unknown count never reads as none")
}

func TestEngineReplicaTimeoutReadsTheLiveSetting(t *testing.T) {
	h := &hostScript{reply: func(string) (string, error) { return "5", nil }}
	assert.Equal(t, 5*time.Second, EngineReplicaTimeout(h))

	h = &hostScript{reply: func(string) (string, error) { return "", errors.New("no") }}
	assert.Equal(t, DefaultEngineReplicaTimeout, EngineReplicaTimeout(h))
	h = &hostScript{reply: func(string) (string, error) { return "soon", nil }}
	assert.Equal(t, DefaultEngineReplicaTimeout, EngineReplicaTimeout(h))
}

func TestEnsureStoragePackagesInstallsWhatIsMissing(t *testing.T) {
	var out bytes.Buffer
	h := &hostScript{reply: func(command string) (string, error) {
		if strings.Contains(command, "os-release") {
			return "ID=ubuntu\nID_LIKE=debian\nVERSION_ID=\"24.04\"\n", nil
		}
		return "", nil
	}}
	require.NoError(t, EnsureStoragePackages(h, &out))
	assert.Contains(t, strings.Join(h.commands, "\n"), "fio")
}

func inventoryHost(replicas, nodes, objects string) *hostScript {
	return &hostScript{reply: func(command string) (string, error) {
		switch {
		case strings.Contains(command, "get replicas.longhorn.io"):
			return replicas, nil
		case strings.Contains(command, "get nodes.longhorn.io"):
			return nodes, nil
		case strings.Contains(command, "services.kipper.run"):
			return objects, nil
		}
		return "", nil
	}}
}

func TestCopiesInventoryNamesWorkersThatHoldCopiesWithoutTheTag(t *testing.T) {
	h := inventoryHost(
		"server-1\t2026-10-01T00:00:00Z\t\nworker-1\t2026-10-01T00:00:00Z\t\nworker-2\t2026-10-01T00:00:00Z\t2026-10-02T00:00:00Z\nworker-3\t2026-10-01T00:00:00Z\t\nworker-1\t2026-10-01T00:00:00Z\t\n",
		"server-1\t[\"kipper-server\",\"kipper-replica\"]\nworker-1\t\nworker-2\t\nworker-3\t[\"kipper-replica\"]\n",
		"Service\tshop\tdb\t\nService\tshop\tcache\t1\nVolume\tshop\tuploads\t\n",
	)
	inv, err := ReadCopiesInventory(h)
	require.NoError(t, err)
	assert.Equal(t, []string{"worker-1"}, inv.UntaggedHolders, "worker-2's only copy failed and worker-3 is tagged")
	assert.Equal(t, 2, inv.Unmanaged)
}

func TestCopiesInventoryFailsWhenLonghornCannotBeRead(t *testing.T) {
	h := &hostScript{reply: func(string) (string, error) { return "", errors.New("unreachable") }}
	_, err := ReadCopiesInventory(h)
	assert.Error(t, err)
}
