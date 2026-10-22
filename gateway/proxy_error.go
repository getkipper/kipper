package main

import (
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// unexpectedCertificateError marks a cluster TLS handshake rejected by the
// registry's pin or proof checks. Its message supplies the logged error detail.
type unexpectedCertificateError struct{ msg string }

func (e *unexpectedCertificateError) Error() string { return e.msg }

type proxyFailure struct {
	status  int
	scene   string
	kind    string
	heading string
	detail  string
	owner   string
}

var (
	failureUnexpectedCertificate = proxyFailure{
		status:  http.StatusBadGateway,
		scene:   "certificate",
		kind:    "unexpected certificate",
		heading: "This site isn't ready yet",
		detail:  "The server behind this address answered with a certificate the kipper.run gateway doesn't recognize. That often happens for a few minutes while a cluster starts up after a restart.",
		owner:   "Wait a few minutes after a restart. If this page still appears, run kip doctor. It reads cluster diagnostics over SSH and checks Traefik, console-api and the connection between the gateway and your cluster.",
	}
	failureNotAnswering = proxyFailure{
		status:  http.StatusGatewayTimeout,
		scene:   "timeout",
		kind:    "cluster not answering",
		heading: "This site isn't answering",
		detail:  "The server behind this address didn't respond in time. It may be turned off, restarting, or behind a firewall that drops the connection.",
		owner:   "Check that the server is running and that ports 80 and 443 are open to the internet, then run kip doctor.",
	}
	failureNotReachable = proxyFailure{
		status:  http.StatusBadGateway,
		scene:   "unreachable",
		kind:    "cluster not reachable",
		heading: "This site can't be reached right now",
		detail:  "The server behind this address refused the connection or couldn't be reached. It may be restarting or turned off.",
		owner:   "Check that the server is running and that Traefik is listening on port 443, then run kip doctor.",
	}
	failureUpstream = proxyFailure{
		status:  http.StatusBadGateway,
		scene:   "generic",
		kind:    "upstream error",
		heading: "Something went wrong reaching this site",
		detail:  "The kipper.run gateway couldn't complete the request to the server behind this address.",
		owner:   "Run kip doctor to check the cluster.",
	}
)

func classifyProxyError(err error) *proxyFailure {
	var certErr *unexpectedCertificateError
	if errors.As(err, &certErr) {
		return &failureUnexpectedCertificate
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &failureNotAnswering
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return &failureNotReachable
	}
	return &failureUpstream
}

// serveProxyError logs failures using the default reverse proxy handler's format.
// It returns HTML with troubleshooting steps when Accept contains text/html,
// or a one-line error otherwise. Requests with an ended context receive an empty 502.
func serveProxyError(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("http: proxy error: %v", err)
	if r.Context().Err() != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	writeProxyFailure(w, r, classifyProxyError(err), time.Now().UTC())
}

func writeProxyFailure(w http.ResponseWriter, r *http.Request, f *proxyFailure, now time.Time) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	if !strings.Contains(r.Header.Get("Accept"), "text/html") {
		h.Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(f.status)
		_, _ = fmt.Fprintf(w, "%d %s: %s\n", f.status, f.heading, f.detail)
		return
	}
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.WriteHeader(f.status)
	_ = proxyFailurePage.Execute(w, map[string]any{
		"Status":    f.status,
		"Scene":     f.scene,
		"Heading":   f.heading,
		"Detail":    f.detail,
		"Owner":     withCommands(f.owner),
		"IssueLink": proxyFailureIssueLink(f, now),
		"Time":      now.Format(time.RFC3339),
	})
}

// proxyFailureIssueLink prefills a bug report with fixed failure text and the time.
// It omits the hostname, which can encode the cluster IP, and the visitor's address
// to keep them private when the link opens on GitHub. The report asks the owner
// to supply diagnostic output and optionally share the site's address.
func proxyFailureIssueLink(f *proxyFailure, now time.Time) string {
	q := url.Values{
		"template": {"bug_report.yml"},
		"title":    {"kipper.run gateway: " + f.kind},
		"what-happened": {fmt.Sprintf("The kipper.run gateway answered %d (%s) at %s.\n\n%s\n\n"+
			"If this is your cluster, run `kip doctor` and paste its output into Logs or output. "+
			"Add the site's address only if you're comfortable making it public.",
			f.status, f.kind, now.Format("2006-01-02T15:04Z"), f.detail)},
		"repro": {"Opened a site on a kipper.run address and got the gateway's error page."},
	}
	return "https://github.com/getkipper/kipper/issues/new?" + q.Encode()
}

// withCommands escapes text and formats each occurrence of kip doctor as HTML code.
func withCommands(text string) template.HTML {
	escaped := template.HTMLEscapeString(text)
	return template.HTML(strings.ReplaceAll(escaped, "kip doctor", "<code>kip doctor</code>")) //nolint:gosec // The text is escaped above, and only fixed markup is added.
}

var proxyFailurePage = template.Must(template.New("proxy-failure").Parse(`{{define "mark"}}<path fill="#BDE4FF" d="M106.36 445.6l106.58 0 87.72 -107.26 62.99 -76.32 -88.13 -106.77 -62.59 75.83 87.72 107.26 -105.76 0 -88.54 107.26z"/><path fill="#0EA5E9" d="M106.36 231.07l-106.36 0 88.54 107.26 -88.54 107.26 106.36 0 88.54 -107.26 -88.54 -107.26z"/><path fill="#0EA5E9" d="M296.34 0l-74.66 90.45 141.97 171.56 -62.99 76.32 62.99 76.32 -141.97 171.57 74.66 90.45c93.65,-112.68 187.1,-225.35 280.37,-338.34 -93.27,-112.98 -186.72,-225.66 -280.37,-338.34z"/><circle fill="#fff" cx="440.9" cy="343.39" r="34.88"/><path fill="#fff" d="M106.36 231.08l106.58 0 87.72 107.26 -105.76 0 -88.54 -107.26zm194.3 107.26l-87.72 107.26 62.59 75.82 88.13 -106.77 -62.99 -76.32z"/><path fill="#7DC3F5" d="M286.59 534.83l-1.29 0 0 -1.56 1.29 1.56zm0 -392.99l-1.29 0 0 1.56 1.29 -1.56zm-180.99 89.23l107.33 0 62.59 -75.83 -107.33 0 -47.4 57.43 -15.19 18.4zm0 214.52l107.33 0 62.59 75.82 -107.33 0 -47.4 -57.43 -15.19 -18.39z"/>{{end}}<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Heading}}</title>
<style>
:root {
  color-scheme: light dark;
  --bg: #f4f8fc; --surface: #ffffff; --ink: #1e2761; --muted: #56608a; --line: #d6e2f0;
  --sky: #0ea5e9; --link: #0369a1; --amber: #b45309;
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #0f1638; --surface: #17204a; --ink: #e7ecfa; --muted: #a3aecf; --line: #2a3566;
    --sky: #38bdf8; --link: #7dc3f5; --amber: #f59e0b;
  }
}
* { box-sizing: border-box; }
body { margin: 0; background: var(--bg); color: var(--ink); font: 1rem/1.6 system-ui, -apple-system, "Segoe UI", Roboto, sans-serif; }
main { max-width: 38rem; margin: 0 auto; padding: clamp(2rem, 9vh, 5.5rem) 16px 3rem; }
.brand { display: flex; align-items: center; gap: .55rem; color: var(--muted); font-weight: 600; font-size: .95rem; }
.brand svg { width: 1.15rem; height: auto; }
.route { margin: 2.25rem 0 1.75rem; }
.route > svg { display: block; width: 100%; height: auto; overflow: visible; }
h1 { font-family: ui-rounded, "SF Pro Rounded", system-ui, sans-serif; font-weight: 700; font-size: clamp(1.65rem, 4.6vw, 2.2rem); line-height: 1.15; letter-spacing: -0.015em; margin: 0 0 .85rem; }
p { margin: 0 0 1rem; max-width: 34rem; }
.lead { font-size: 1.08rem; }
.visitor { color: var(--muted); }
.owner { margin-top: 2.5rem; padding-top: 1.5rem; border-top: 1px solid var(--line); }
h2 { font-size: 1rem; margin: 0 0 .5rem; }
a { color: var(--link); text-underline-offset: .18em; }
a:focus-visible { outline: 2px solid var(--link); outline-offset: 3px; border-radius: 2px; }
footer { margin-top: 2.5rem; color: var(--muted); font-size: .85rem; }
footer time { white-space: nowrap; }
code { white-space: nowrap; font: .92em ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; background: var(--line); padding: .08em .35em; border-radius: 4px; }

.node { fill: var(--surface); stroke: var(--ink); stroke-width: 2.5; }
.detail { stroke: var(--ink); stroke-width: 2.5; stroke-linecap: round; fill: none; }
.dot-ink { fill: var(--ink); }
.label { fill: var(--muted); font: 600 13px system-ui, -apple-system, "Segoe UI", sans-serif; text-anchor: middle; }
.hop { stroke: var(--sky); stroke-width: 3.5; stroke-linecap: round; }
.hop-lost { stroke: var(--line); stroke-width: 3.5; stroke-linecap: round; }
.hop-fading { stroke: var(--sky); stroke-width: 3.5; stroke-linecap: round; stroke-dasharray: 3 9; opacity: .55; }
.faded { opacity: .38; }
.mark-amber { stroke: var(--amber); stroke-width: 3.5; stroke-linecap: round; stroke-linejoin: round; fill: none; }
.fill-amber { fill: var(--amber); }
.knock { stroke: var(--surface); stroke-width: 3; stroke-linecap: round; }
.signal { fill: var(--sky); }
.scene-timeout { --stop: 262px; }
.scene-unreachable { --stop: 236px; }
.scene-certificate { --stop: 316px; }
.scene-generic { --stop: 234px; }
.signal { animation: travel 2.8s cubic-bezier(.45, 0, .3, 1) .5s 1 both; }
.break { transform-box: fill-box; transform-origin: center; animation: flare 2.8s ease-out .5s 1 both; }
@keyframes travel {
  0% { transform: translateX(0); opacity: 0; }
  8% { opacity: 1; }
  68% { transform: translateX(var(--stop)); opacity: 1; }
  86%, 100% { transform: translateX(var(--stop)); opacity: 0; }
}
@keyframes flare {
  0%, 64% { transform: scale(1); }
  74% { transform: scale(1.22); }
  100% { transform: scale(1); }
}
@media (max-width: 480px) {
  .label { font-size: 19px; }
}
@media (prefers-reduced-motion: reduce) {
  .signal { display: none; }
  .break { animation: none; }
}
</style>
</head>
<body>
<main>
<div class="brand"><svg viewBox="0 0 576.71 676.67" role="img" aria-label="Kipper">{{template "mark"}}</svg><span>kipper.run gateway</span></div>
<figure class="route">
<svg viewBox="0 0 560 146" class="scene-{{.Scene}}" data-scene="{{.Scene}}" role="img" aria-label="{{if eq .Scene "certificate"}}Your browser reached the kipper.run gateway, and the site's server answered with a certificate the gateway doesn't recognize.{{else if eq .Scene "timeout"}}Your browser reached the kipper.run gateway, but the site's server didn't answer in time.{{else if eq .Scene "unreachable"}}Your browser reached the kipper.run gateway, but the gateway couldn't connect to the site's server.{{else}}Your browser reached the kipper.run gateway, but the request to the site's server failed.{{end}}">
<rect class="node" x="26" y="38" width="92" height="66" rx="9"/>
<line class="detail" x1="26" y1="54" x2="118" y2="54"/>
<circle class="dot-ink" cx="37" cy="46" r="2.6"/><circle class="dot-ink" cx="46" cy="46" r="2.6"/><circle class="dot-ink" cx="55" cy="46" r="2.6"/>
<line class="detail" x1="40" y1="72" x2="88" y2="72"/><line class="detail" x1="40" y1="86" x2="74" y2="86"/>
<text class="label" x="72" y="134">You</text>
<line class="hop" x1="128" y1="71" x2="234" y2="71"/>
<svg x="244" y="31" width="68" height="80" viewBox="0 0 576.71 676.67">{{template "mark"}}</svg>
<text class="label" x="278" y="134">kipper.run gateway</text>
{{if eq .Scene "timeout"}}
<line class="hop-fading" x1="324" y1="71" x2="440" y2="71"/>
<g class="faded">
{{else if eq .Scene "unreachable"}}
<line class="hop" x1="324" y1="71" x2="362" y2="71"/>
<line class="hop-lost" x1="404" y1="71" x2="440" y2="71"/>
<g class="break"><line class="mark-amber" x1="373" y1="60" x2="393" y2="82"/><line class="mark-amber" x1="393" y1="60" x2="373" y2="82"/></g>
<g>
{{else if eq .Scene "certificate"}}
<line class="hop" x1="324" y1="71" x2="440" y2="71"/>
<g>
{{else}}
<line class="hop" x1="324" y1="71" x2="360" y2="71"/>
<polyline class="mark-amber break" points="360,71 368,58 378,84 388,58 396,71"/>
<line class="hop-lost" x1="404" y1="71" x2="440" y2="71"/>
<g>
{{end}}
<rect class="node" x="450" y="34" width="86" height="74" rx="9"/>
<line class="detail" x1="450" y1="59" x2="536" y2="59"/><line class="detail" x1="450" y1="84" x2="536" y2="84"/>
<circle class="dot-ink" cx="466" cy="47" r="3"/><circle class="dot-ink" cx="466" cy="72" r="3"/><circle class="dot-ink" cx="466" cy="96" r="3"/>
<line class="detail" x1="490" y1="47" x2="520" y2="47"/><line class="detail" x1="490" y1="72" x2="520" y2="72"/><line class="detail" x1="490" y1="96" x2="520" y2="96"/>
</g>
{{if eq .Scene "timeout"}}
<g class="break"><circle class="fill-amber" cx="536" cy="34" r="14"/><line class="knock" x1="536" y1="34" x2="536" y2="26"/><line class="knock" x1="536" y1="34" x2="542" y2="38"/></g>
{{else if eq .Scene "certificate"}}
<g class="break"><circle class="fill-amber" cx="536" cy="34" r="14"/><line class="knock" x1="536" y1="27" x2="536" y2="36"/><line class="knock" x1="536" y1="41.5" x2="536" y2="41.6"/></g>
{{end}}
<text class="label" x="493" y="134">The site's server</text>
<circle class="signal" cx="128" cy="71" r="5.5"/>
</svg>
</figure>
<h1>{{.Heading}}</h1>
<p class="lead">{{.Detail}}</p>
<p class="visitor">If you're visiting this site, try again in a few minutes.</p>
<section class="owner">
<h2>Is this your cluster?</h2>
<p>{{.Owner}}</p>
<p>Think this is a Kipper bug? <a href="{{.IssueLink}}" rel="noopener noreferrer">Open an issue</a>. Opening the link sends GitHub the error status, kind, time, and a fixed explanation and reproduction step. It keeps this site's address private.</p>
</section>
<footer>Error {{.Status}} from the kipper.run gateway at <time datetime="{{.Time}}">{{.Time}}</time></footer>
</main>
</body>
</html>
`))
