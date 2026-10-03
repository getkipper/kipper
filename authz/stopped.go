package main

import (
	"bytes"
	"html/template"
	"mime"
	"net/http"
	"strings"
)

// stoppedPage identifies the host without exposing the operator's stop record.
var stoppedPage = template.Must(template.New("stopped").Parse(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>This app is stopped</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,system-ui,sans-serif;background:#0a0a0a;color:#fff;display:flex;align-items:center;justify-content:center;min-height:100vh}
.c{text-align:center;padding:2rem}
h1{font-size:1.5rem;font-weight:600;margin-bottom:.75rem}
p{color:#9ca3af;font-size:.9rem;line-height:1.5}
</style>
</head>
<body>
<div class="c">
<h1>This app is stopped</h1>
<p>{{.}} is not running at the moment.<br>Please try again later.</p>
</div>
</body>
</html>
`))

// handleStopped serves the stopped page for Traefik's errors middleware.
// Read only Host and Accept to keep visitor credentials out of the response and logs.
func handleStopped(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if wantsJSON(r.Header.Get("Accept")) {
		writeDenial(w, http.StatusServiceUnavailable, "app_stopped", "This app is stopped.", 0)
		return
	}
	var page bytes.Buffer
	if err := stoppedPage.Execute(&page, r.Host); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write(page.Bytes())
}

// wantsJSON reports whether the client lists JSON before HTML.
func wantsJSON(accept string) bool {
	for _, part := range strings.Split(accept, ",") {
		mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		switch {
		case mediaType == "application/json" || strings.HasSuffix(mediaType, "+json"):
			return true
		case mediaType == "text/html":
			return false
		}
	}
	return false
}
