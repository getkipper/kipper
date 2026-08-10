package middleware

import (
	"log"
	"net/http"
	"time"
)

// Logging logs each request with method, path, status, and duration.
func Logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &statusWriter{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(wrapped, r)

		log.Printf("%s %s %d %s", //nolint:gosec // request method and path are safe to log
			r.Method, r.URL.Path, wrapped.status, time.Since(start))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush delegates to the underlying ResponseWriter if it supports flushing.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the wrapped ResponseWriter so http.ResponseController can
// reach the underlying connection. Without it, a handler's SetReadDeadline (or
// any other ResponseController call) silently fails once this middleware wraps
// the writer.
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
