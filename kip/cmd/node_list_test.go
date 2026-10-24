package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/k8s"
)

func TestNodeListWithoutWorkersKeepsItsColumns(t *testing.T) {
	var out bytes.Buffer
	nodes := []k8s.NodeInfo{{Name: "server-1", Role: "master", Status: "Ready", Version: "v1.33.1+k3s1", IP: "203.0.113.10"}}

	writeNodeList(&out, nodes, statusRecords()[:1])

	assert.Equal(t, "\n  NAME                 ROLE     STATUS                    VERSION        IP\n"+
		"  server-1             master   Ready                     v1.33.1+k3s1   203.0.113.10\n\n", out.String())
}

func TestNodeListShowsModeAndAddressForRecordedNodes(t *testing.T) {
	var out bytes.Buffer
	records := statusRecords()
	records[1].Mode = installer.NetworkModePrivate
	records[1].Storage = &config.NodeStorage{Copies: true}
	records[2].Storage = &config.NodeStorage{Copies: false}
	nodes := []k8s.NodeInfo{
		{Name: "server-1", Role: "master", Status: "Ready", Version: "v1.33.1+k3s1", IP: "10.0.0.2"},
		{Name: "worker-1", Role: "worker", Status: "Ready", Version: "v1.33.1+k3s1", IP: "10.0.0.3"},
		{Name: "stray", Role: "worker", Status: "NotReady", Version: "v1.33.1+k3s1", IP: "10.0.0.9"},
	}

	writeNodeList(&out, nodes, records)

	got := out.String()
	assert.Contains(t, got, "MODE      COPIES    ADDRESS\n")
	assert.Contains(t, got, "  server-1             master   Ready                     v1.33.1+k3s1   10.0.0.2         -         yes       203.0.113.10\n")
	assert.Contains(t, got, "  worker-1             worker   Ready                     v1.33.1+k3s1   10.0.0.3         private   yes       203.0.113.11\n")
	assert.Contains(t, got, "  stray                worker   NotReady                  v1.33.1+k3s1   10.0.0.9         -         -         -\n")

	nodes = append(nodes, k8s.NodeInfo{Name: "worker-2", Role: "worker", Status: "Ready", Version: "v1.33.1+k3s1", IP: "10.0.0.4"})
	out.Reset()
	writeNodeList(&out, nodes, records)
	assert.Contains(t, out.String(), "  worker-2             worker   Ready                     v1.33.1+k3s1   10.0.0.4         -         no        203.0.113.12\n")
}

// The records are read through the API, so any operator machine sees the
// workers, not only the one that added them.
func TestNodeRecordsFromTheAPI(t *testing.T) {
	rec := statusRecords()[1]
	value, err := json.Marshal(rec)
	require.NoError(t, err)
	client := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: installer.NodeRecordsConfigMap, Namespace: installer.NodeRecordsNamespace},
		Data:       map[string]string{rec.MachineID: string(value)},
	})

	got, err := nodeRecordsFromAPI(context.Background(), client, &config.Cluster{})
	require.NoError(t, err)
	assert.Equal(t, []installer.NodeRecord{rec}, got)
}

func TestNodeRecordsWithoutAConfigMapUseTheLocalCopy(t *testing.T) {
	local := statusRecords()
	got, err := nodeRecordsFromAPI(context.Background(), fake.NewSimpleClientset(), &config.Cluster{Nodes: local})
	require.NoError(t, err)
	assert.Equal(t, local, got)
}

// Records that cannot be read or are invalid fall back to the local copy, and
// the caller is told, because that copy can miss workers added elsewhere.
func TestNodeRecordsFallBackToTheLocalCopyAndSaySo(t *testing.T) {
	local := statusRecords()
	forbidden := fake.NewSimpleClientset()
	forbidden.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, installer.NodeRecordsConfigMap, errors.New("no access"))
	})
	invalid := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: installer.NodeRecordsConfigMap, Namespace: installer.NodeRecordsNamespace},
		Data:       map[string]string{"x": "not a record"},
	})

	for name, client := range map[string]*fake.Clientset{"forbidden": forbidden, "invalid": invalid} {
		got, err := nodeRecordsFromAPI(context.Background(), client, &config.Cluster{Nodes: local})
		assert.Error(t, err, name)
		assert.Equal(t, local, got, name)
	}
}

func TestStatusWarnsWhenWorkersComeFromTheLocalCopy(t *testing.T) {
	var out bytes.Buffer
	reportRecordFallback(&out, errors.New("forbidden"))
	assert.Equal(t, "  Node records:\n    ⚠  not read from the cluster (forbidden). Worker checks use this machine's copy, which can miss workers added from another machine\n\n", out.String())

	out.Reset()
	reportRecordFallback(&out, nil)
	assert.Empty(t, out.String())
}
