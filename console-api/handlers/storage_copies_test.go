package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

// The console creates one copy, so what it creates is managed from the start
// and never left with copies unset.
func TestServiceCreate_WritesOneCopy(t *testing.T) {
	c := testCRClient()
	handler := &Services{Client: k8sfake.NewClientset(), CRClient: c}

	rec := httptest.NewRecorder()
	handler.Create(rec, httptest.NewRequest(http.MethodPost, "/services",
		strings.NewReader(`{"name":"db","type":"postgres","namespace":"shop-prod"}`)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var svc kipperv1.Service
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Namespace: "shop-prod", Name: "db"}, &svc))
	require.NotNil(t, svc.Spec.Copies)
	assert.EqualValues(t, 1, *svc.Spec.Copies)
}

func TestVolumes_CreateWritesOneCopy(t *testing.T) {
	c := testCRClient(newPlatformConfig(platform.ProfileSmall, nil, kipperv1.PlatformConfigStatus{}))
	require.Equal(t, http.StatusOK, createVolume(t, c).Code)

	var vol kipperv1.Volume
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Namespace: "shop", Name: "uploads"}, &vol))
	require.NotNil(t, vol.Spec.Copies)
	assert.EqualValues(t, 1, *vol.Spec.Copies)
}
