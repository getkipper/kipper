package ingest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestTS wraps an already-constructed Server in an httptest server.
func newTestTS(t *testing.T, srv *Server) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// decodeJSON decodes a response body.
func decodeJSON(resp *http.Response, v any) error {
	return json.NewDecoder(resp.Body).Decode(v)
}
