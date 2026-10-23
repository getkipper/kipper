package doctor

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func unstructuredObject(apiVersion, kind, ns, name string, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind, "spec": spec}}
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}

func apiClients(t *testing.T) (*k8sfake.Clientset, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	ready := node("server-1", corev1.ConditionTrue)
	cs := k8sfake.NewClientset(&ready, deployment("traefik", "traefik", 1), deployment("default", "shop", 1))
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		{Group: "traefik.io", Version: "v1alpha1", Resource: "tlsstores"}:         "TLSStoreList",
		{Group: "kipper.run", Version: "v1alpha1", Resource: "clusteridentities"}: "ClusterIdentityList",
		{Group: "kipper.run", Version: "v1alpha1", Resource: "platformconfigs"}:   "PlatformConfigList",
		veleroBackupGVR:   "BackupList",
		veleroScheduleGVR: "ScheduleList",
	},
		unstructuredObject("traefik.io/v1alpha1", "TLSStore", "kipper-system", "default",
			map[string]any{"defaultCertificate": map[string]any{"secretName": "kipper-hop-cert"}}), //nolint:gosec // a Kubernetes field name, not a credential
		unstructuredObject("kipper.run/v1alpha1", "ClusterIdentity", "", "cluster",
			map[string]any{"gateway": map[string]any{"clusterHost": "203.0.113.10"}}),
		unstructuredObject("kipper.run/v1alpha1", "PlatformConfig", "", "platform", map[string]any{"profile": "small", "storage": "longhorn"}),
	)
	return cs, dyn
}

func TestFetchViaAPI_ReadsTheSameParts(t *testing.T) {
	cs, dyn := apiClients(t)

	snap := FetchViaAPI(context.Background(), cs, dyn)

	require.Len(t, snap.Nodes, 1)
	assert.Contains(t, snap.Workloads, "traefik/traefik")
	assert.NotContains(t, snap.Workloads, "default/shop")
	assert.Equal(t, []TLSStore{{Namespace: "kipper-system", Name: "default", DefaultCert: "kipper-hop-cert"}}, snap.TLSStores)
	assert.Equal(t, "203.0.113.10", snap.ClusterHost)
	assert.Equal(t, "small", snap.Profile)
	assert.Equal(t, "longhorn", snap.Storage)
}

func TestFetchViaAPI_NeverReadsASecret(t *testing.T) {
	cs, dyn := apiClients(t)

	snap := FetchViaAPI(context.Background(), cs, dyn)

	for _, action := range cs.Actions() {
		assert.NotEqual(t, "secrets", action.GetResource().Resource, "a Secret Get returns its private key too: %v", action)
	}
	require.Contains(t, snap.Missing, PartHopCert)
	assert.Contains(t, snap.Missing[PartHopCert].Error(), "SSH")
	assert.Empty(t, snap.HopCert)
}

func TestFetchViaAPI_RecordsAFailedPartAndReadsTheRest(t *testing.T) {
	cs, dyn := apiClients(t)
	cs.PrependReactor("list", "nodes", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, assert.AnError
	})

	snap := FetchViaAPI(context.Background(), cs, dyn)

	assert.Contains(t, snap.Missing, PartNodes)
	assert.Equal(t, "small", snap.Profile)
}

func TestNewestHeartbeatLines_OrdersByTimestampAcrossPods(t *testing.T) {
	lines := newestHeartbeatLines([]string{
		"2026-10-06T11:59:00Z gateway heartbeat: hop pin for x is pending",   // pod A, newest
		"2026-10-06T09:00:00Z gateway heartbeat: renewed x (hop pin active)", // pod B, older
	})

	require.Len(t, lines, 2)
	assert.Contains(t, lines[1], "is pending", "the newest event comes last whichever pod logged it")
}

func TestNewestPodLines_ReadsOnlyThePodRenewingNow(t *testing.T) {
	lines := newestPodLines(map[string][]string{
		"console-api-old": {
			"2026-10-06T11:58:00Z gateway heartbeat: proof for x not yet accepted: HTTP 202",
			"2026-10-06T11:59:59Z gateway heartbeat: renewed x (hop pin active)",
		},
		"console-api-new": {
			"2026-10-06T11:59:00Z gateway heartbeat: renewed x (hop pin active)",
		},
	})

	require.Len(t, lines, 2, "lines from two writers are never merged")
	assert.Contains(t, lines[0], "not yet accepted")
}
