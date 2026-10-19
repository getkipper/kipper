package main

import (
	"context"
	"errors"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/getkipper/kipper/gateway/internal/registry"
)

// pinnedProxy returns a proxy whose cluster has an established pin the test
// backend does not serve, so every request fails the hop handshake.
func pinnedProxy(t *testing.T) *httputil.ReverseProxy {
	t.Helper()
	ts, _ := pinBackend(t)
	now := time.Now().UTC().Format(time.RFC3339)
	longAgo := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	reg := registryFromSnapshot(t, `{"entries":[{"subdomain":"myapp","ip":"203.0.113.1","token":"tok","created_at":"`+now+
		`","last_seen":"`+now+`","cert_fingerprint":"`+strings.Repeat("a", 64)+
		`","pin_updated_at":"`+longAgo+`","first_pinned_at":"`+longAgo+`"}]}`)
	p := &Proxy{Registry: reg, BaseDomain: "kipper.run"}
	return proxyVia(t, p, reg.Lookup("myapp"), ts, true)
}

// dialingProxy returns a proxy for an unpinned cluster whose dial always fails with err.
func dialingProxy(t *testing.T, err error) *httputil.ReverseProxy {
	t.Helper()
	reg := registry.New()
	entry, _, _ := reg.Register("myapp", "203.0.113.1", "")
	p := &Proxy{Registry: reg, BaseDomain: "kipper.run"}
	rp := p.buildProxy("myapp.kipper.run", entry, false)
	rp.Transport.(*http.Transport).DialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: err}
	}
	return rp
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func get(rp *httputil.ReverseProxy, accept string) *http.Response {
	req := httptest.NewRequest(http.MethodGet, "https://myapp.kipper.run/", nil)
	req.RemoteAddr = "203.0.113.7"
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	w := httptest.NewRecorder()
	rp.ServeHTTP(w, req)
	return w.Result()
}

func bodyOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// issueLink returns the page's issue link the way a browser reads its href.
func issueLink(t *testing.T, body string) *url.URL {
	t.Helper()
	start := strings.Index(body, "https://github.com/getkipper/kipper/issues/new?")
	if start < 0 {
		t.Fatalf("no issue link:\n%s", body)
	}
	end := strings.IndexByte(body[start:], '"')
	u, err := url.Parse(html.UnescapeString(body[start : start+end]))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

const browserAccept = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"

func TestProxyErrorPage_ExplainsAnUnexpectedCertificate(t *testing.T) {
	resp := get(pinnedProxy(t), browserAccept)
	body := bodyOf(t, resp)

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
	for _, want := range []string{"certificate", "restart", "kip status"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q:\n%s", want, body)
		}
	}
}

func TestProxyErrorPage_SaysATimedOutClusterIsNotAnswering(t *testing.T) {
	resp := get(dialingProxy(t, timeoutError{}), browserAccept)

	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504", resp.StatusCode)
	}
	if body := bodyOf(t, resp); !strings.Contains(body, "isn&#39;t answering") && !strings.Contains(body, "isn't answering") {
		t.Errorf("page does not say the site isn't answering:\n%s", body)
	}
}

func TestProxyErrorPage_SaysARefusedClusterCannotBeReached(t *testing.T) {
	resp := get(dialingProxy(t, errors.New("connect: connection refused")), browserAccept)

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	if body := bodyOf(t, resp); !strings.Contains(body, "can&#39;t be reached") && !strings.Contains(body, "can't be reached") {
		t.Errorf("page does not say the site can't be reached:\n%s", body)
	}
}

func TestProxyErrorPage_AnswersNonBrowsersInPlainText(t *testing.T) {
	resp := get(dialingProxy(t, timeoutError{}), "*/*")
	body := bodyOf(t, resp)

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q", ct)
	}
	if strings.Contains(body, "<") || strings.Count(strings.TrimSpace(body), "\n") != 0 {
		t.Errorf("want one line of plain text, got %q", body)
	}
}

func TestProxyErrorPage_IsNeverCachedAndLoadsNothingElse(t *testing.T) {
	resp := get(pinnedProxy(t), browserAccept)

	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := resp.Header.Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'none'") {
		t.Errorf("Content-Security-Policy = %q", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
}

func TestProxyErrorPage_LinksAnIssueThatNamesNeitherTheHostNorTheClient(t *testing.T) {
	u := issueLink(t, bodyOf(t, get(pinnedProxy(t), browserAccept)))

	link := u.Query().Encode()
	for _, private := range []string{"myapp", "203.0.113.1", "203.0.113.7"} {
		if strings.Contains(link, private) {
			t.Errorf("issue link carries %q: %s", private, link)
		}
	}
	if u.Query().Get("template") != "bug_report.yml" || !strings.Contains(u.Query().Get("title"), "certificate") {
		t.Errorf("issue link lacks the template or the error class: %s", link)
	}
}

func TestProxyErrorPage_KeepsTheProxyErrorLogLine(t *testing.T) {
	buf := captureLog(t)

	get(pinnedProxy(t), browserAccept)

	if !strings.Contains(buf.String(), "http: proxy error: ") || !strings.Contains(buf.String(), "which matches no pinned fingerprint") {
		t.Errorf("log line changed: %q", buf.String())
	}
}

func TestProxyErrorPage_FallsBackToAGenericPageForOtherUpstreamErrors(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "https://myapp.kipper.run/", nil)
	req.Header.Set("Accept", browserAccept)
	w := httptest.NewRecorder()

	serveProxyError(w, req, io.ErrUnexpectedEOF)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Something went wrong reaching this site") {
		t.Errorf("not the generic page:\n%s", w.Body.String())
	}
}

func TestProxyErrorPage_WritesNoPageForAClientThatWentAway(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "https://myapp.kipper.run/", nil).WithContext(ctx)
	req.Header.Set("Accept", browserAccept)
	w := httptest.NewRecorder()

	serveProxyError(w, req, context.Canceled)

	if w.Code != http.StatusBadGateway || w.Body.Len() != 0 {
		t.Errorf("got %d with %d bytes, want an empty 502", w.Code, w.Body.Len())
	}
}

func TestProxyErrorPage_SendsNoReferrer(t *testing.T) {
	if got := get(pinnedProxy(t), browserAccept).Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q", got)
	}
}

func TestProxyErrorPage_IssueLinkLeavesTheEnvironmentFieldToTheReporter(t *testing.T) {
	q := issueLink(t, bodyOf(t, get(pinnedProxy(t), browserAccept))).Query()

	if q.Has("environment") {
		t.Error("environment is the form's OS and architecture field")
	}
	if !strings.Contains(q.Get("what-happened"), "kipper.run gateway answered 502") {
		t.Errorf("what-happened = %q", q.Get("what-happened"))
	}
}

func TestProxyErrorPage_IssueLinkGuidesTheOwnerWithoutNamingTheSite(t *testing.T) {
	q := issueLink(t, bodyOf(t, get(pinnedProxy(t), browserAccept))).Query()

	if repro := q.Get("repro"); !strings.Contains(repro, "kipper.run address") {
		t.Errorf("repro = %q", repro)
	}
	what := q.Get("what-happened")
	for _, want := range []string{failureUnexpectedCertificate.detail, "kip status", "Logs or output", "only if you're comfortable"} {
		if !strings.Contains(what, want) {
			t.Errorf("what-happened lacks %q:\n%s", want, what)
		}
	}
}

func TestProxyErrorPage_ShowsTheKipperMarkAndTheBrokenHop(t *testing.T) {
	for _, tc := range []struct {
		f     proxyFailure
		scene string
	}{
		{failureNotAnswering, "timeout"},
		{failureNotReachable, "unreachable"},
		{failureUnexpectedCertificate, "certificate"},
		{failureUpstream, "generic"},
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "https://myapp.kipper.run/", nil)
		req.Header.Set("Accept", browserAccept)
		writeProxyFailure(w, req, &tc.f, time.Now())
		body := w.Body.String()

		if !strings.Contains(body, `aria-label="Kipper"`) {
			t.Errorf("%s: no Kipper mark", tc.scene)
		}
		if !strings.Contains(body, `data-scene="`+tc.scene+`"`) {
			t.Errorf("%s: diagram does not show the %s hop", tc.f.kind, tc.scene)
		}
	}
}

func TestProxyErrorPage_LoadsNothingFromElsewhere(t *testing.T) {
	body := bodyOf(t, get(pinnedProxy(t), browserAccept))

	for _, banned := range []string{"<script", "<img", "<link", "src=", "@import", "url("} {
		if strings.Contains(body, banned) {
			t.Errorf("page contains %q", banned)
		}
	}
	if !strings.Contains(body, "prefers-reduced-motion") {
		t.Error("page animates without honoring reduced motion")
	}
}

func TestProxyErrorPage_ShowsKipStatusAsACommand(t *testing.T) {
	body := bodyOf(t, get(pinnedProxy(t), browserAccept))

	if !strings.Contains(body, "<code>kip status</code>") {
		t.Errorf("kip status is not marked up as a command:\n%s", body)
	}
}

func TestProxyErrorPage_AnimatesOnceSoMotionEndsWithinFiveSeconds(t *testing.T) {
	body := bodyOf(t, get(pinnedProxy(t), browserAccept))

	if strings.Contains(body, " 3 both") || !strings.Contains(body, "travel 2.8s cubic-bezier(.45, 0, .3, 1) .5s 1 both") {
		t.Error("the signal animation must run once")
	}
}
