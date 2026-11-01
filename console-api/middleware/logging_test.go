package middleware

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
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

// An invite token in a request path is the credential itself, and anyone who
// can read the console-api log could redeem it.
func TestTheLogLineLeavesOutTokensInThePath(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	r := chi.NewRouter()
	r.Use(Logging)
	r.Get("/api/v1/invites/{token}", func(w http.ResponseWriter, _ *http.Request) {})
	r.Post("/api/v1/invites/{token}/accept", func(w http.ResponseWriter, _ *http.Request) {})
	r.Get("/api/v1/projects/{name}", func(w http.ResponseWriter, _ *http.Request) {})

	const token = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" //nolint:gosec // G101: a made-up invite token
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/v1/invites/"+token, nil),
		httptest.NewRequest(http.MethodPost, "/api/v1/invites/"+token+"/accept", nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/projects/shop", nil),
		httptest.NewRequest(http.MethodPut, "/api/v1/invites/"+token+"/info", nil),
		httptest.NewRequest(http.MethodOptions, "/api/v1/invites/"+token+"/accept", nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/invites/"+token[:20]+"%2F"+token[20:], nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/%69nvites/"+token, nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/invite%73/"+token, nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/invites%2F"+token, nil),
		httptest.NewRequest(http.MethodGet, "/api/v1%2Finvites/"+token, nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/invites//"+token, nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/invites%2F/"+token, nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/invites/%2F/"+token, nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/Invites/"+token, nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/INVITES/"+token, nil),
	} {
		r.ServeHTTP(httptest.NewRecorder(), req)
	}

	out := logged.String()
	assert.NotContains(t, out, token)
	assert.NotContains(t, out, token[20:], "an encoded slash must not split the token out of the redacted segment")
	assert.Contains(t, out, "GET /api/v1/invites/[redacted] 200")
	assert.Contains(t, out, "POST /api/v1/invites/[redacted]/accept 200")
	assert.Contains(t, out, "GET /api/v1/projects/shop 200", "other path values stay readable")
	assert.Contains(t, out, "PUT /api/v1/invites/[redacted]/info 404", "a path no route matches still carries the token")
}
