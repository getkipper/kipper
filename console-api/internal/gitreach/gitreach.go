// Package gitreach answers whether a git repository can be cloned with the
// credential an app is about to be given, before the app is created.
//
// Without it an app configured against a private repository with no token
// builds anyway, and every build dies at clone with "could not read Username
// for ...", in a job log in another namespace. The operator sees an app that
// will not start and no reason for it.
package gitreach

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/getkipper/kipper/controller/pkg/netguard"
)

// Result is what a preflight concluded.
type Result int

const (
	// Reachable means the repository answered the reference advertisement with
	// the credential given, so a clone will work.
	Reachable Result = iota
	// NeedsCredential means the repository answered but refused the credential
	// given, which for an anonymous attempt means it is private.
	NeedsCredential
	// Unknown means the check could not be completed: DNS, a timeout, a proxy,
	// a server that is down. Nothing was learnt about the repository, so the
	// caller allows the write rather than blocking on its own network.
	Unknown
	// Unsupported means the URL is not one this can check, an SSH remote being
	// the usual case.
	Unsupported
	// Unsafe means the host did something a credential must not be exposed to,
	// such as redirecting an authenticated request onto plaintext. Distinct
	// from Unknown: nothing was learnt about the repository either way, but
	// this is evidence about the host rather than about the network, and a
	// caller must not wave it through.
	Unsafe
)

// errUnsafeRedirect marks a refusal by the redirect policy, so a caller can tell
// "the host tried to move the credential somewhere it must not go" from "this
// cluster could not reach the host". Both fail the request; only one of them
// is the host's doing.
var errUnsafeRedirect = errors.New("the host redirected the request somewhere the access token must not follow")

// timeout bounds the whole attempt including redirects. A preflight sits in
// front of an interactive create, so it fails fast rather than correctly.
const timeout = 5 * time.Second

// transport is the round tripper every check uses.
//
// It refuses to connect to a non-public address. The URL this probes is
// supplied by whoever configures an app, and this runs inside console-api,
// which reaches the whole cluster — so without the guard a deployer could name
// a cluster service or a metadata endpoint and have the control plane fetch it
// for them, and read the outcome from whether the write was accepted. The same
// boundary is already drawn for the Dockerfile probe.
//
// A genuinely private git host therefore fails to connect and reports Unknown,
// which callers allow: the build itself is not subject to this guard, so a
// self-hosted repository on a private network still deploys. It simply does
// not get the benefit of the check.
//
// A seam as well, so a test can trust a self-signed server: the token path only
// runs over TLS, and a test cannot exercise it against a certificate nothing
// trusts.
var transport http.RoundTripper = &http.Transport{
	DialContext: netguard.Dialer(timeout).DialContext,
}

// maxRedirects is how many hops are followed. Git hosts redirect http to
// https and add or drop a .git suffix; more than a few means a loop or a
// redirector, neither of which is a repository.
const maxRedirects = 3

// Check reports whether repoURL can be read with the given credential.
//
// token may be empty, which asks the question anonymously and is the whole
// point for a public repository: requiring a credential for every HTTPS
// source would break the common case and push operators into minting tokens
// they do not need.
//
// The check is the git smart-HTTP reference advertisement, which is what a
// clone starts with, so a repository that answers here is one the builder can
// clone. Nothing is fetched beyond the advertisement's headers.
func Check(ctx context.Context, repoURL, branch, username, token string) (Result, string) {
	parsed, err := url.Parse(strings.TrimSpace(repoURL))
	if err != nil {
		return Unsupported, "this does not look like a URL"
	}
	// A URL may carry its own credential in userinfo, and Go's transport replays
	// that as basic auth. Callers reject the shape before they get here, but
	// this is a shared helper and must not depend on that: the caller that
	// forgets is the one that leaks.
	if parsed.User != nil {
		return Unsupported, "this URL carries a username or password, which must not be sent to check it"
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		// A token sent over plaintext is readable by anything on the path, so
		// this refuses rather than checks. Without a token there is nothing to
		// leak, and the answer is still worth having.
		if token != "" {
			return Unsupported, "this is a plaintext http:// URL, and an access token must not be sent over it"
		}
	case "":
		// scp-like syntax: git@host:org/repo.git
		return Unsupported, "SSH remotes cannot be checked from here, so this one is taken on trust"
	default:
		return Unsupported, fmt.Sprintf("%s remotes cannot be checked from here", parsed.Scheme)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	advert := *parsed
	advert.Path = strings.TrimSuffix(advert.Path, "/") + "/info/refs"
	advert.RawQuery = "service=git-upload-pack"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, advert.String(), nil)
	if err != nil {
		return Unknown, "the check could not be started"
	}
	if token != "" {
		if username == "" {
			username = "git"
		}
		req.SetBasicAuth(username, token)
	}

	resp, err := clientFor(parsed.Scheme, normalisedHost(parsed)).Do(req)
	if err != nil {
		// A host that tries to move an authenticated request onto plaintext,
		// or onto another host, has told us something about itself. Reporting
		// that as "could not be reached" would let the caller wave it through
		// as a transient network problem, and the build's own git would then
		// follow the redirect this refused to.
		if errors.Is(err, errUnsafeRedirect) {
			return Unsafe, "this host redirects the clone somewhere your access token must not follow"
		}
		// A network this machine cannot cross says nothing about the
		// repository, and blocking a create on it would make the console
		// refuse work whenever its own egress is unhappy.
		if errors.Is(err, context.DeadlineExceeded) {
			return Unknown, "the repository did not answer in time"
		}
		return Unknown, "the repository could not be reached from the cluster"
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		if token == "" {
			return NeedsCredential, "this repository is private, so it needs an access token"
		}
		return NeedsCredential, "the access token was refused by the repository"
	case resp.StatusCode == http.StatusNotFound:
		// A private repository on some hosts is indistinguishable from a
		// missing one, deliberately. Either way the clone will fail.
		if token == "" {
			return NeedsCredential, "this repository is private or does not exist, so it needs an access token"
		}
		return NeedsCredential, "the repository was not found with this access token"
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if branch == "" {
			return Reachable, ""
		}
		return checkBranch(resp.Body, branch)
	default:
		return Unknown, fmt.Sprintf("the repository answered %d, which says nothing about whether it can be cloned", resp.StatusCode)
	}
}

// clientFor builds the HTTP client for one origin.
//
// Redirects are bounded and confined to the origin the request started at,
// scheme included. A credential is attached with basic auth and Go replays
// those headers across a same-host redirect, so both halves matter: an open
// redirect would carry the token to another host, and an https-to-http
// redirect keeps the host while putting the token on the wire in clear. The
// second is the easier one to miss, because the host does compare equal.
func clientFor(scheme, host string) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			if normalisedHost(req.URL) != host {
				return fmt.Errorf("%w: to %s, which would carry it off %s", errUnsafeRedirect, req.URL.Host, host)
			}
			if req.URL.Scheme != scheme {
				return fmt.Errorf("%w: from %s to %s, which would put it on the wire in clear", errUnsafeRedirect, scheme, req.URL.Scheme)
			}
			return nil
		},
	}
}

// maxAdvertisement bounds what is read looking for a branch. A reference
// advertisement for a large repository is big, and none of it needs to be held
// to answer the question, so the read stops once the branch is found or the
// ceiling is reached.
const maxAdvertisement = 1 << 20

// checkBranch reports whether the advertisement names the branch asked for.
//
// A repository that answers is not the same as a branch that exists, and a
// build configured against a deleted or mistyped branch fails every time with
// the reason in a job log. The advertisement is pkt-line framed, and the ref
// names inside it are plain text, so this looks for the exact ref rather than
// parsing the framing: a substring match alone would accept "main" for a
// repository that only has "maintenance".
func checkBranch(body io.Reader, branch string) (Result, string) {
	advertisement, err := io.ReadAll(io.LimitReader(body, maxAdvertisement))
	if err != nil {
		return Unknown, "the repository's branch list could not be read"
	}
	ref := "refs/heads/" + branch
	for _, candidate := range refNames(string(advertisement)) {
		if candidate == ref {
			return Reachable, ""
		}
	}
	if len(advertisement) == maxAdvertisement {
		// Truncated, so absence proves nothing.
		return Reachable, ""
	}
	return NeedsCredential, fmt.Sprintf("the repository has no branch called %q", branch)
}

// refNames pulls the ref names out of an advertisement. Each is preceded by a
// space and ends at the first NUL, space, newline or carriage return, which is
// what distinguishes a whole ref from a name that merely starts with one.
func refNames(advertisement string) []string {
	var names []string
	for _, field := range strings.FieldsFunc(advertisement, func(r rune) bool {
		return r == ' ' || r == '\n' || r == '\r' || r == 0
	}) {
		if strings.HasPrefix(field, "refs/") {
			names = append(names, field)
		}
	}
	return names
}

// normalisedHost drops a port that is the scheme's default, so a host that
// redirects to itself with an explicit :443 is recognised as the same
// destination rather than refused as a hop to somewhere else.
func normalisedHost(u *url.URL) string {
	// Case and a trailing dot are spellings of one host, and git servers use
	// both: a redirect that only changes the capitalisation is the same
	// destination. Refusing it accuses the host of something it did not do and
	// blocks a repository git clones perfectly well.
	host := strings.TrimSuffix(strings.ToLower(u.Host), ".")
	switch {
	case u.Scheme == "https" && strings.HasSuffix(host, ":443"):
		return strings.TrimSuffix(host, ":443")
	case u.Scheme == "http" && strings.HasSuffix(host, ":80"):
		return strings.TrimSuffix(host, ":80")
	}
	return host
}
