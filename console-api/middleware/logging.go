package middleware

import (
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Logging logs each request with method, path, status, and duration, including
// requests whose handlers panic. Panics propagate to the caller.
func Logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &statusWriter{ResponseWriter: w, status: http.StatusOK}

		// Defer logging so a download aborted with http.ErrAbortHandler still gets
		// a request log entry.
		defer func() {
			log.Printf("%s %s %d %s", //nolint:gosec // request method and path are safe to log
				r.Method, loggedPath(r), wrapped.status, time.Since(start))
		}()
		next.ServeHTTP(wrapped, r)
	})
}

// loggedPath redacts the token following each "invites" path component because
// invite tokens are credentials. It matches "invites" case-insensitively,
// recognizes encoded and repeated slashes, and replaces the entire escaped
// segment containing the token. Path-based redaction also covers unmatched
// requests and responses sent before routing.
func loggedPath(r *http.Request) string {
	segments := strings.Split(r.URL.EscapedPath(), "/")
	type part struct {
		text    string
		segment int
	}
	var parts []part
	for i, segment := range segments {
		decoded, err := url.PathUnescape(segment)
		if err != nil {
			decoded = segment
		}
		for _, text := range strings.Split(decoded, "/") {
			parts = append(parts, part{text, i})
		}
	}
	for i, p := range parts {
		if !strings.EqualFold(p.text, "invites") {
			continue
		}
		for _, next := range parts[i+1:] {
			if next.text != "" {
				segments[next.segment] = "[redacted]"
				break
			}
		}
	}
	return strings.Join(segments, "/")
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
