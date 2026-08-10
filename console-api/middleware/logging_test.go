package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The Logging middleware wraps the ResponseWriter. If that wrapper doesn't
// expose the underlying writer, a handler's http.ResponseController calls
// (notably SetReadDeadline, used to bound slow-drip request bodies) silently
// fail with ErrNotSupported. This exercises the real path through a live
// server so the Unwrap wiring can't regress.
func TestLoggingWriterAllowsReadDeadline(t *testing.T) {
	var deadlineErr error
	handler := Logging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadlineErr = http.NewResponseController(w).SetReadDeadline(time.Now().Add(time.Second))
		w.WriteHeader(http.StatusOK)
	}))

	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	_ = resp.Body.Close()

	if deadlineErr != nil {
		t.Fatalf("SetReadDeadline must reach the connection through the logging middleware, got: %v", deadlineErr)
	}
}
