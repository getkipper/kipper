package installer

import (
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/getkipper/kipper/controller/pkg/authncfg"
	"github.com/getkipper/kipper/controller/pkg/hostnames"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

const (
	// authnConfigPath is where the kube-apiserver reads its structured
	// authentication configuration; the flag in k3sConfig points here and
	// the API server reloads the file when it changes.
	authnConfigPath = "/etc/rancher/k3s/authn-config.yaml"

	// auditPolicyPath is the audit policy the apiserver flag references.
	auditPolicyPath = "/etc/rancher/k3s/audit-policy.yaml"
)

// authnConfigStub is what the API server boots with: a valid configuration
// carrying zero authenticators. Upstream validation explicitly supports this
// ("the API server is initially set up without any authenticators and then
// authenticators are added later via dynamic config"), which dissolves the
// bootstrap ordering problem — the flag never points at a missing file, and
// the server is never pointed at an issuer that does not exist yet.
const authnConfigStub = `apiVersion: apiserver.config.k8s.io/v1
kind: AuthenticationConfiguration
`

// auditPolicy attributes every API action without capturing payloads: all
// levels are Metadata or None, so no request or response body — and no
// secret data — ever enters the log. The None rules drop the components
// whose steady-state chatter would drown operator actions.
const auditPolicy = `apiVersion: audit.k8s.io/v1
kind: Policy
omitStages:
  - RequestReceived
rules:
  # The control plane's own identities are fully dropped: their write
  # traffic is the cluster's heartbeat, and an attacker holding these
  # credentials owns the node the log lives on anyway.
  - level: None
    users:
      - system:kube-proxy
      - system:apiserver
      - system:kube-controller-manager
      - system:kube-scheduler
  # Nodes and kube-system workloads keep their read chatter out of the
  # log, and nothing more: their writes stay attributed, so a stolen
  # kube-system ServiceAccount token cannot act invisibly.
  - level: None
    verbs:
      - get
      - list
      - watch
    userGroups:
      - system:nodes
      - system:serviceaccounts:kube-system
  - level: None
    nonResourceURLs:
      - /healthz*
      - /readyz*
      - /livez*
      - /metrics
      - /version
  - level: Metadata
`

// renderAuthnConfig builds the jwt authenticator(s) for the cluster's Dex
// hosts. It delegates to the shared authncfg package so kip (which writes the
// file) and console-api (which gates a cutover on the apiserver's active hash)
// render byte-identical content. Multiple hosts produce a union authenticator
// set — the primitive a domain cutover uses to trust old and new issuers at
// once and drain sessions across the flip.
func renderAuthnConfig(caPEM string, dexHosts ...string) string {
	return authncfg.Render(caPEM, dexHosts...)
}

// writeAuthnStubAndAuditPolicy places the files the apiserver flags
// reference; InstallK3s calls it before the k3s installer runs. Re-runs must
// not clobber a live authenticator config back to the stub, so the stub only
// lands when no config exists yet.
func writeAuthnStubAndAuditPolicy(client *ssh.Client) error {
	if _, err := client.Run(authnStubWriteCmd()); err != nil {
		return fmt.Errorf("writing authentication config stub: %w", err)
	}
	policyCmd := fmt.Sprintf("cat > %s << 'KIPEOF'\n%sKIPEOF", auditPolicyPath, auditPolicy)
	if _, err := client.Run(policyCmd); err != nil {
		return fmt.Errorf("writing audit policy: %w", err)
	}
	return nil
}

// authnStubWriteCmd writes the boot stub only when no authentication config
// exists: a re-run over a live cluster must never clobber the active
// authenticator back to the stub, which would silently turn OIDC off.
func authnStubWriteCmd() string {
	return fmt.Sprintf("[ -f %s ] || cat > %s << 'KIPEOF'\n%sKIPEOF", authnConfigPath, authnConfigPath, authnConfigStub)
}

// dexHostsPinMarker tags the one managed /etc/hosts line so re-runs replace
// it instead of accumulating.
const dexHostsPinMarker = "# kipper-dex-pin"

// errMetricsUnreadable means the API server could not be asked what it has
// loaded. It is not the same answer as "it has not loaded this", and callers
// that treat it as such send an operator to repair a config the API server may
// already be running.
var errMetricsUnreadable = errors.New("the API server's metrics could not be read, so what it has loaded is unknown")

// authnConfigActive reports whether the apiserver's active authentication
// config is exactly content, by matching the last_config_info hash label
// exactly (never a substring).
//
// The metrics read is kept out of the pipeline on purpose. Piping it into grep
// cannot distinguish the two failures: an API server that cannot be reached
// produces no output, so grep finds nothing and exits 1 — the same status as a
// healthy API server that simply has not loaded this config. Even pipefail
// reports grep's 1 as the rightmost non-zero status, so the two stay
// indistinguishable. Capturing first makes the API server's own failure the
// script's failure.
func authnConfigActive(client *ssh.Client, content string) (bool, error) {
	out, err := client.Run(`raw=$(kubectl get --raw /metrics) || exit 97
printf '%s\n' "$raw" | grep 'apiserver_authentication_config_controller_last_config_info' || true`)
	if err != nil {
		return false, fmt.Errorf("%w: %v", errMetricsUnreadable, err)
	}
	return authncfg.ActiveHashMatches(out, authncfg.Hash(content)), nil
}

// parsePinnedHosts extracts the managed loopback pin's host list from an
// /etc/hosts body: the hosts on the single line carrying the pin marker,
// after the 127.0.0.1 and before the marker comment. Empty when no pin
// exists.
func parsePinnedHosts(hostsBody string) []string {
	for _, line := range strings.Split(hostsBody, "\n") {
		if !strings.Contains(line, dexHostsPinMarker) {
			continue
		}
		line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil
		}
		return fields[1:] // drop the 127.0.0.1
	}
	return nil
}

// unionHosts returns the sorted, deduped union of two host lists.
func unionHosts(a, b []string) []string {
	seen := map[string]bool{}
	for _, h := range append(append([]string{}, a...), b...) {
		if h != "" {
			seen[h] = true
		}
	}
	out := make([]string, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// writePinLine atomically rewrites the managed /etc/hosts pin line to exactly
// hosts (all pointed at loopback). The atomic filter-into-temp-then-copy-back
// pattern means a failure leaves either the old or the new line, never a
// hosts file missing its other entries. Callers must have validated hosts.
func writePinLine(client *ssh.Client, hosts []string) error {
	line := "127.0.0.1 " + strings.Join(hosts, " ") + " " + dexHostsPinMarker
	cmd := fmt.Sprintf(
		"sed '/%s/d' /etc/hosts > /etc/hosts.kipper-tmp && echo '%s' >> /etc/hosts.kipper-tmp && cat /etc/hosts.kipper-tmp > /etc/hosts && rm -f /etc/hosts.kipper-tmp",
		dexHostsPinMarker, line)
	if _, err := client.Run(cmd); err != nil {
		return fmt.Errorf("writing dex loopback pin: %w", err)
	}
	return nil
}

// readPinnedHosts reads the current managed pin host list from the server.
func readPinnedHosts(client *ssh.Client) ([]string, error) {
	out, err := client.Run("cat /etc/hosts")
	if err != nil {
		return nil, fmt.Errorf("reading /etc/hosts: %w", err)
	}
	return parsePinnedHosts(out), nil
}

// hopCAPath is where the installer writes the cluster's certificate authority
// for the API server and this probe to read. It lives under /etc/rancher/k3s so
// uninstall's removal of that tree takes it with everything else k3s owns,
// rather than leaving an anchor behind for the next install to inherit.
const hopCAPath = "/etc/rancher/k3s/kipper-hop-ca.crt"

// readHopCA returns the cluster certificate authority, or empty when the
// cluster has none. A cluster serving only a custom domain never needs one.
//
// Absent and unreadable are answered differently, which is the same distinction
// ensureHopCA makes about the Secret and for the same reason. `cat || true`
// turned a permission error, an I/O error and a directory in the file's place
// all into "this cluster has no authority" — and the callers act on that: the
// probe stops verifying, the status reports the API server as untrusting, and
// the render drops the anchor the API server needs.
func readHopCA(client commandRunner) (string, error) {
	// -e alone is false for a dangling symlink and for a path whose parent
	// cannot be traversed, both of which are "could not read this", not "there
	// is none". -L catches the first; the cat that follows reports the rest.
	out, err := client.Run(fmt.Sprintf(
		"if [ ! -e %s ] && [ ! -L %s ]; then exit 0; fi; cat %s", hopCAPath, hopCAPath, hopCAPath))
	if err != nil {
		return "", fmt.Errorf("reading the cluster certificate authority at %s: %w", hopCAPath, err)
	}
	return strings.TrimSpace(out), nil
}

// probeCommand builds the retrying probe for one host, verifying it the way the
// API server will. A gateway-fronted host is served the cluster's own hop
// certificate and so is verified against the cluster CA; a custom domain carries
// a WebPKI certificate and keeps the system trust store, which is the gate that
// protects a cutover from proceeding on an unverified certificate.
//
// A gateway-fronted host with no anchor is refused outright rather than probed
// against a trust store that can never accept it: that produces a certificate
// error naming a cause that was never involved, which is exactly how this cost
// an afternoon of diagnosis.
//
// The anchor arrives on stdin rather than being re-read from hopCAPath, so the
// probe attests to the exact bytes the caller is about to render into the
// authentication config. Verifying against the file instead would let an anchor
// that changed after the caller read it satisfy a probe for a config built from
// the superseded one, which ends with the API server trusting an authority the
// cluster no longer serves.
// Both host classes set pipefail. Without it `curl | grep -q` reports the
// grep's status, so a curl that died part-way through the body reads as a
// certificate failure. The two classes must fail for the same reasons as each
// other, or the same server appears healthy on one path and broken on the other.
func probeCommand(host, caPEM string) (string, error) {
	fetch := fmt.Sprintf(
		`for i in $(seq 1 24); do curl -fsS %%s--max-time 5 https://%s/dex/.well-known/openid-configuration | grep -q '"issuer"' && exit 0; sleep 5; done; exit 1`,
		host)
	if !hostnames.IsKipperRun(host) {
		return "set -o pipefail; " + fmt.Sprintf(fetch, ""), nil
	}
	if caPEM == "" {
		return "", fmt.Errorf("dex issuer %s is a gateway-fronted host but the cluster certificate authority is missing from %s; the API server would have nothing to verify it with", host, hopCAPath)
	}
	// The whole thing is one && chain through to the loop. Ending the chain
	// before it and starting the loop after a ';' let the retries run with an
	// empty --cacert when mktemp failed: two minutes of handshakes that could
	// not succeed, reported as a problem with the cluster's certificate.
	return `set -o pipefail; tmp=$(mktemp) && trap 'rm -f "$tmp"' EXIT && cat > "$tmp" && ` +
		fmt.Sprintf(fetch, `--cacert "$tmp" `), nil
}

// probeNeedsAnchor reports whether this host's probe reads the anchor from
// stdin. Only a gateway-fronted host is verified against the cluster's own
// authority; a custom domain carries a WebPKI certificate.
func probeNeedsAnchor(host string) bool { return hostnames.IsKipperRun(host) }

// anchorChangedMarker is how the fenced write reports that it refused, told
// apart from any other failure of the same command.
const anchorChangedMarker = "KIPPER_ANCHOR_CHANGED"

// refuseChangedAnchor stops a run whose evidence has gone stale, before it can
// decide there is nothing to do.
//
// This is not the fence — writeAuthnConfigFenced is. It exists for the case the
// fence cannot see: if the anchor moved to B while this run holds A, and the
// config already matches what A renders to, the write is skipped as "already
// active" and the run reports success having ignored the anchor the operator
// asked it to rebuild from. Checking first turns that into a refusal.
func refuseChangedAnchor(client *ssh.Client, probed string) error {
	current, err := readHopCA(client)
	if err != nil {
		return err
	}
	if current != probed {
		return anchorMovedError()
	}
	return nil
}

func anchorMovedError() error {
	return fmt.Errorf(
		"the trust anchor at %s changed while this was running, so what was verified is not what would be installed.\n"+
			"    The authentication config was left alone; the API server is still on the one it had.\n"+
			"    Re-run once whatever is changing the anchor has finished", hopCAPath)
}

// writeAuthnConfigFenced installs the rendered config only while the anchor is
// still the one the probes verified, with the comparison and the write in a
// single remote command.
//
// One command is the point. Re-reading the anchor over one SSH connection and
// writing over another leaves exactly the window this exists to remove: the
// anchor can move after the check and the stale config lands anyway, which puts
// the API server on an authority the cluster no longer serves. That is operator
// lockout, and it is the failure that ended four attempts at automating the
// replacement.
//
// What remains is the interval inside the script itself, between reading the
// anchor and renaming the config into place. A file takes no compare-and-swap,
// so that cannot be closed, only made small. An operator editing the anchor by
// hand is outside any fence this could build, which is why the documented
// procedure tells them to do one thing at a time.
func writeAuthnConfigFenced(client *ssh.Client, desired, probed string) error {
	out, err := client.Run(fencedWriteScript(hopCAPath, authnConfigPath, desired, probed))
	if err == nil {
		return nil
	}
	if strings.Contains(out, anchorChangedMarker) {
		return anchorMovedError()
	}
	return fmt.Errorf("writing authentication config: %w", err)
}

// fencedWriteScript is the remote half of the fence, taking its paths as
// arguments so the shell that ships can be run against real files in a test.
// Its correctness matters beyond this command: every install writes the
// authentication config through it.
//
// The config is staged beside its destination and renamed, so the API server
// never reads a half-written file. The delimiters cannot collide with the
// content: a heredoc ends only on a line equal to the delimiter, and neither a
// PEM body line nor a rendered config line is ever exactly one of these.
// Both sides of the comparison have every whitespace character removed, which
// is the same normalisation CAState.anchorHas uses on the Go side. Comparing
// raw bytes looked right and was not: readHopCA trims both ends while the
// shell's $(cat) strips trailing newlines only, so an anchor beginning with a
// blank line compared unequal to itself and refused every write, permanently,
// naming a concurrent writer that did not exist.
//
// The staging file is unique. A fixed name is shared by every install, cutover
// and sync on the node, so two of them truncate each other's staged config and
// one renames the other's bytes into place.
func fencedWriteScript(anchorPath, configPath, desired, probed string) string {
	return fmt.Sprintf(`set -e
probed=$(printf %%s %s | base64 -d | tr -d '[:space:]')
if [ -e %s ] || [ -L %s ]; then actual=$(tr -d '[:space:]' < %s); else actual=; fi
if [ "$actual" != "$probed" ]; then echo %s; exit 97; fi
staged=$(mktemp %s.kipper-XXXXXX)
trap 'rm -f "$staged"' EXIT
printf %%s %s | base64 -d > "$staged"
chmod 0644 "$staged"
mv "$staged" %s
trap - EXIT`,
		shellQuote(base64.StdEncoding.EncodeToString([]byte(probed))),
		anchorPath, anchorPath, anchorPath, anchorChangedMarker, configPath,
		shellQuote(base64.StdEncoding.EncodeToString([]byte(desired))), configPath)
}

// shellQuote wraps a value so the shell reads it as one literal argument.
// Combined with base64 it means no caller-supplied byte is ever interpreted:
// the anchor is a file an operator edits by hand during a replacement, and a
// line in it that happened to match a heredoc delimiter used to end the
// document and run as root.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// listenerDiagnosis reports when nothing is answering on the port the pin
// resolves to, which is a different problem from a certificate one and worth
// separating: the ingress controller runs as a single replica, and with
// externalTrafficPolicy Local a replica on any node but this one leaves this
// node's :443 dropping traffic. Traefik is pinned to the server node for exactly
// that reason (see traefik.go), so reaching this means something overrode it —
// or the ingress is simply not up yet.
func listenerDiagnosis(client *ssh.Client) string {
	if _, err := client.Run("curl -sS -o /dev/null -k --max-time 5 https://127.0.0.1/ 2>&1"); err != nil {
		return "; nothing is answering https on this node, so the ingress controller is not running here, " +
			"check that Traefik is scheduled on the server node and Ready"
	}
	return ""
}

// probeHint names the likely cause for the host class that failed, so the error
// points at the right thing instead of at cert-manager for a host cert-manager
// never issues for.
func probeHint(host string) string {
	if hostnames.IsKipperRun(host) {
		return "the cluster may not be serving its hop certificate yet, or it is not the one the cluster CA signed"
	}
	return "cert-manager may still be issuing its certificate"
}

// EnsureOperatorAuth converges the API server's OIDC authentication onto
// exactly dexHosts, maintaining one invariant at every instant: the
// /etc/hosts pin set is a superset of the authenticator config's issuer
// hosts. Every trusted issuer's discovery and JWKS fetch therefore stays
// loopback-bound, and a re-registered old domain can never feed the apiserver
// foreign JWKS.
//
// The single ordering — pin-superset, then config, then pin-exact — holds the
// invariant in both directions. Expanding (staging a new issuer): the pin
// gains the new host before the config trusts it. Contracting (dropping an
// old issuer): the config drops it while the pin still covers it, then the
// pin tightens. Certificate authentication is untouched throughout, so a
// failure here never severs cluster access.
func EnsureOperatorAuth(client *ssh.Client, dexHosts ...string) error {
	if len(dexHosts) == 0 {
		return fmt.Errorf("no dex hosts given")
	}
	for _, h := range dexHosts {
		if err := authncfg.ValidateDexHost(h); err != nil {
			return err
		}
	}

	// The anchor the API server will be given for a gateway-fronted issuer, and
	// the one this function's own probe verifies with. Absent is not an error:
	// a cluster that only ever served a custom domain has none and needs none.
	caPEM, err := readHopCA(client)
	if err != nil {
		return err
	}

	// 1. Pin superset: union the current pin with the requested hosts, so
	// every host the config is about to trust is already loopback-bound.
	current, perr := readPinnedHosts(client)
	if perr != nil {
		return perr
	}
	if err := writePinLine(client, unionHosts(current, dexHosts)); err != nil {
		return err
	}

	// 2. Probe each requested host through the pin with a verifying TLS
	// handshake, retrying for ~2 minutes because a fresh cluster or a cutover
	// reaches here right as the ingress and its certificate come up.
	//
	// The probe verifies the way the API server will, which is the only thing
	// that makes it a gate rather than a formality: it is about to be told to
	// trust this issuer, and it fetches discovery and JWKS from it moments
	// later over the same loopback pin. The two host classes are verified
	// differently because they are served differently. A custom domain carries
	// a WebPKI certificate, so the system trust store is the right verifier and
	// this is the certificate gate firing before anything session-affecting. A
	// gateway-fronted host is served the cluster's own hop certificate, which no
	// public authority signed, so it verifies against the cluster CA — the same
	// anchor written into the authentication config below.
	//
	// Pre-flip a new host legitimately still advertises the old issuer, so only
	// reachability and certificate validity are checked, never the advertised
	// issuer value.
	for _, h := range dexHosts {
		probeCmd, cerr := probeCommand(h, caPEM)
		if cerr != nil {
			return cerr
		}
		var perr error
		if probeNeedsAnchor(h) {
			_, perr = client.RunStdin(probeCmd, strings.NewReader(caPEM))
		} else {
			_, perr = client.Run(probeCmd)
		}
		if perr != nil {
			return fmt.Errorf("dex issuer %s did not become reachable over verified TLS through the loopback pin (%s)%s: %w",
				h, probeHint(h), listenerDiagnosis(client), perr)
		}
	}

	// 3. Config: write and verify only on a real change. The active hash
	// match proves the running apiserver loaded this exact content.
	//
	// The anchor is re-read first: the probes above take up to two minutes, and
	// installing a config rendered from an anchor that has since been replaced
	// is how the API server ends up trusting an authority the cluster no longer
	// serves.
	if err := refuseChangedAnchor(client, caPEM); err != nil {
		return err
	}
	desired := authncfg.Render(caPEM, dexHosts...)
	active, err := authnConfigActive(client, desired)
	if err != nil {
		return err
	}
	if !active {
		if werr := writeAuthnConfigFenced(client, desired, caPEM); werr != nil {
			return werr
		}
		waitCmd := fmt.Sprintf(
			`for i in $(seq 1 30); do kubectl get --raw /metrics | grep 'apiserver_authentication_config_controller_last_config_info' | grep -q '%s' && exit 0; sleep 2; done; exit 1`,
			authncfg.Hash(desired))
		if _, werr := client.Run(waitCmd); werr != nil {
			return fmt.Errorf("the API server did not activate the authentication config (OIDC is not active; certificate auth is unaffected): %w", werr)
		}
	}

	// 4. Pin exact: tighten the pin to exactly the trusted hosts now that the
	// config no longer references any dropped host.
	return writePinLine(client, unionHosts(dexHosts, nil))
}

// InstallOperatorAuth activates OIDC authentication for a single Dex host at
// install time. It is the thin wrapper over EnsureOperatorAuth; the
// convergent mutator is what a domain cutover drives with two hosts.
func InstallOperatorAuth(client *ssh.Client, dexHost string) error {
	return EnsureOperatorAuth(client, dexHost)
}
