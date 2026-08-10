package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHealthReportsVersion(t *testing.T) {
	original := BuildVersion
	defer func() { BuildVersion = original }()
	BuildVersion = "v0.4.2"

	rec := httptest.NewRecorder()
	Health(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "ok", body["status"])
	assert.Equal(t, "v0.4.2", body["version"], "the CLI's version handshake reads this field")
}
