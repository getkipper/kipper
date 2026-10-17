package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

func createVolume(t *testing.T, c crclient.Client) *httptest.ResponseRecorder {
	t.Helper()
	h := &Volumes{CRClient: c}
	r := chi.NewRouter()
	r.Post("/projects/{name}/volumes", h.Create)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/projects/shop/volumes", strings.NewReader(`{"name":"uploads","size":"1Gi"}`)))
	return rec
}

func TestVolumes_CreateRefusesASharedVolumeOnLocalStorage(t *testing.T) {
	local := newPlatformConfig(platform.ProfileNano, nil, kipperv1.PlatformConfigStatus{})
	local.Spec.Storage = platform.StorageLocal
	c := testCRClient(local)

	rec := createVolume(t, c)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, platform.SharedVolumesUnsupported, body["error"])

	err := c.Get(context.Background(), crclient.ObjectKey{Namespace: "shop", Name: "uploads"}, &kipperv1.Volume{})
	assert.True(t, apierrors.IsNotFound(err), "no Volume is created")
}

func TestVolumes_CreateOnLonghorn(t *testing.T) {
	rec := createVolume(t, testCRClient(newPlatformConfig(platform.ProfileSmall, nil, kipperv1.PlatformConfigStatus{})))
	assert.Equal(t, http.StatusOK, rec.Code)
}
