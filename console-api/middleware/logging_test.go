package middleware

import (
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestAnAbortedRequestStillLeavesALogLine(t *testing.T) {
	var out strings.Builder
	prev := log.Writer()
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(prev) })
	handler := Logging(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	func() {
		defer func() {
			if p := recover(); p != http.ErrAbortHandler {
				t.Errorf("the abort did not reach net/http: recovered %v", p)
			}
		}()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/download", nil))
	}()

	if !strings.Contains(out.String(), "GET /download") {
		t.Errorf("an aborted request left no log line:\n%s", out.String())
	}
}
