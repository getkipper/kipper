package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/controller/pkg/ai"
)

func TestAIBundleStatus_NoBundlesInstalled(t *testing.T) {
	// Clean cluster, no `kip ai install` ever run. Both bundles
	// installed=false; the console panel renders "Not installed" for
	// each.
	client := fake.NewSimpleClientset()
	h := &AIBundleStatus{Client: client}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/ai/bundle-status", nil)
	h.Get(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var got ai.Report
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&got))
	assert.False(t, got.Phase1.Installed)
	assert.False(t, got.RAG.Installed)
}

func TestAIBundleStatus_DriftSurfaced(t *testing.T) {
	// Phase 1 state ConfigMap present but every workload gone — the
	// exact scenario that prompted this checker. The response must
	// surface installed=true with the three missing resources.
	client := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ai.BundleStateConfigMapName, Namespace: ai.Namespace},
	})
	h := &AIBundleStatus{Client: client}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/ai/bundle-status", nil)
	h.Get(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var got ai.Report
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&got))
	assert.True(t, got.Phase1.Installed)
	assert.Len(t, got.Phase1.Missing, 3, "every Phase 1 workload missing → all three Resources reported")
	assert.True(t, got.HasDrift())
}
