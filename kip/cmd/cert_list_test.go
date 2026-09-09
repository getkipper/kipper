package cmd

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestListCertificatesShowsHostStateAgeAndFailureReason(t *testing.T) {
	now := time.Date(2026, 9, 7, 3, 0, 0, 0, time.UTC)
	ready := certificateObject("kipper-system", "platform-tls", []string{"console.example.com"}, map[string]interface{}{
		"type":               "Ready",
		"status":             "True",
		"reason":             "Ready",
		"lastTransitionTime": now.Add(-2 * time.Hour).Format(time.RFC3339),
	})
	failed := certificateObject("shop-prod", "store-tls", []string{"store.example.com", "www.store.example.com"}, map[string]interface{}{
		"type":               "Ready",
		"status":             "False",
		"reason":             "DoesNotExist",
		"message":            "Issuing certificate as Secret does not exist",
		"lastTransitionTime": now.Add(-3 * time.Minute).Format(time.RFC3339),
	})
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{certificateGVR: "CertificateList"},
		ready,
		failed,
	)

	var out bytes.Buffer
	require.NoError(t, listCertificates(context.Background(), &out, dyn, now))

	printed := out.String()
	assert.Contains(t, printed, "NAMESPACE")
	assert.Contains(t, printed, "HOST")
	assert.Contains(t, printed, "STATE")
	assert.Contains(t, printed, "AGE")
	assert.Contains(t, printed, "kipper-system")
	assert.Contains(t, printed, "console.example.com")
	assert.Contains(t, printed, "Ready")
	assert.Contains(t, printed, "2h")
	assert.Contains(t, printed, "shop-prod")
	assert.Contains(t, printed, "store.example.com,www.store.example.com")
	assert.Contains(t, printed, "Not Ready")
	assert.Contains(t, printed, "3m")
	assert.Contains(t, printed, "DoesNotExist: Issuing certificate as Secret does not exist")
}

func TestListCertificatesSaysWhenThereAreNone(t *testing.T) {
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{certificateGVR: "CertificateList"},
	)

	var out bytes.Buffer
	require.NoError(t, listCertificates(context.Background(), &out, dyn, time.Now()))
	assert.Equal(t, "\n  No certificates found\n\n", out.String())
}

func TestListCertificatesReturnsListFailure(t *testing.T) {
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{certificateGVR: "CertificateList"},
	)
	dyn.PrependReactor("list", "certificates", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("api unavailable")
	})

	err := listCertificates(context.Background(), &bytes.Buffer{}, dyn, time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listing certificates")
}

func certificateObject(namespace, name string, hosts []string, readyCondition map[string]interface{}) *unstructured.Unstructured {
	hostValues := make([]interface{}, len(hosts))
	for i, host := range hosts {
		hostValues[i] = host
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata": map[string]interface{}{
			"namespace": namespace,
			"name":      name,
		},
		"spec": map[string]interface{}{
			"dnsNames": hostValues,
		},
		"status": map[string]interface{}{
			"conditions": []interface{}{readyCondition},
		},
	}}
}
