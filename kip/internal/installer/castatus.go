package installer

import (
	"time"

	"github.com/getkipper/kipper/controller/pkg/hopca"
	"github.com/getkipper/kipper/controller/pkg/hostnames"
)

// CAStatus reports certificate trust checks and the next recovery action.
// ReadCAStatus collects it over SSH, so it remains available during rotations
// and OIDC login failures.
type CAStatus struct {
	Phase              CAPhase
	Resume             string
	Authority          CertSummary
	Incoming, Outgoing *CertSummary
	// Leaf describes the stored certificate, which Traefik may not yet serve.
	Leaf CertSummary
	// TrustedByAPIServer requires both AnchorCovers and AnchorLoaded.
	TrustedByAPIServer         bool
	AnchorCovers, AnchorLoaded bool
	// AnchorLoadedUnknown distinguishes an unreadable metric from a confirmed mismatch.
	AnchorLoadedUnknown bool
	// AnchorUsed indicates that at least one configured issuer uses kipper.run.
	AnchorUsed bool
	// ServedByActive is nil when no gateway host or active authority is available to check.
	ServedByActive *bool
	Hosts          []string
	Problems       []string
}

// CertSummary is the human-facing description of one certificate.
type CertSummary struct {
	Subject     string
	Expires     time.Time
	Fingerprint string
}

// Healthy reports whether the cluster needs no attention: no rotation or
// malformed state, confirmed anchor coverage and loading, and a passing live
// certificate check when one is available.
func (s CAStatus) Healthy() bool {
	if s.Phase != CAPhaseSteady || len(s.Problems) > 0 || !s.TrustedByAPIServer {
		return false
	}
	// An unreadable metric leaves trust unconfirmed and still needs attention.
	if s.AnchorLoadedUnknown {
		return false
	}
	return s.ServedByActive == nil || *s.ServedByActive
}

// NextCommand returns the command to repair a confirmed config mismatch when
// the anchor covers the stored certificate and no malformed state is reported.
// It returns an empty string when no command applies, including when recovery
// requires the documented manual procedure.
func (s CAStatus) NextCommand() string {
	if len(s.Problems) > 0 {
		return ""
	}
	// Sync uses the existing anchor, so it can repair a confirmed load mismatch
	// only after the anchor itself is correct.
	if s.AnchorCovers && !s.AnchorLoaded && !s.AnchorLoadedUnknown {
		return "kip cluster auth sync"
	}
	return ""
}

// recordAnchorLoad records stored certificate coverage and checks whether the
// API server loaded the config rendered from the anchor and issuer hosts.
// The load check is skipped when no issuer is configured.
// TrustedByAPIServer requires both checks to pass. Custom-domain issuers use
// public certificates, so their rendered config is independent of the anchor.
// loadUnconfirmed is true when a probe runs without confirming the load.
// An unreadable metric sets AnchorLoadedUnknown. Other probe errors are returned.
func (s *CAStatus) recordAnchorLoad(client CommandRunner, state CAState) (loadUnconfirmed bool, err error) {
	s.AnchorCovers = state.anchorCoversLeaf()
	s.AnchorUsed = firstGatewayHost(state.DexHosts) != ""
	if len(state.DexHosts) == 0 {
		return false, nil
	}
	// Check loading even when coverage fails, because loading an invalid anchor
	// can break kipper.run logins when the gateway serves the stored certificate.
	loaded, known, err := anchorIsActive(client, state)
	if err != nil {
		return false, err
	}
	s.AnchorLoaded = loaded
	s.AnchorLoadedUnknown = !known
	s.TrustedByAPIServer = loaded && s.AnchorCovers
	return !loaded || !known, nil
}

// ReadCAStatus reads certificate state and, when the config names an issuer,
// checks the API server's loaded config. When a gateway host and active
// authority are available, it also verifies the served certificate against that
// authority, since Secrets alone cannot confirm what the cluster serves. An
// unreadable API server metric is reported through AnchorLoadedUnknown. Other
// read or probe errors are returned.
func ReadCAStatus(client CommandRunner) (CAStatus, error) {
	state, err := ReadCAState(client)
	if err != nil {
		return CAStatus{}, err
	}

	status := CAStatus{
		Phase:    state.Phase(),
		Hosts:    state.DexHosts,
		Problems: state.Anomalies(),
	}
	status.Authority = summarise(state.Active)
	status.Leaf = summarise(state.LeafCert)
	if state.Pending != "" {
		s := summarise(state.Pending)
		status.Incoming = &s
	}
	if state.Retained != "" {
		s := summarise(state.Retained)
		status.Outgoing = &s
	}

	loadUnconfirmed, err := status.recordAnchorLoad(client, state)
	if err != nil {
		return CAStatus{}, err
	}
	status.Resume = resumeFor(state, loadUnconfirmed)

	if host := firstGatewayHost(state.DexHosts); host != "" && state.Active != "" {
		served, serr := ServedByAuthority(client, host, state.Active)
		if serr != nil {
			return CAStatus{}, serr
		}
		status.ServedByActive = &served
	}
	return status, nil
}

// firstGatewayHost returns the first kipper.run host, or an empty string if none
// exists. Custom domains use WebPKI certificates, so probing them cannot verify
// the cluster's hop authority.
func firstGatewayHost(hosts []string) string {
	for _, h := range hosts {
		if hostnames.IsKipperRun(h) {
			return h
		}
	}
	return ""
}

// summarise describes one certificate, degrading rather than failing when the
// material is unreadable.
//
// Refusing here would be the wrong trade: damaged material is when an operator
// most needs the rest of the picture, and Anomalies already reports what cannot
// be read. Failing the whole command would replace a diagnosis naming the
// broken slot with a bare parse error naming nothing.
func summarise(certPEM string) CertSummary {
	if certPEM == "" {
		return CertSummary{}
	}
	cert, err := hopca.ParseCert([]byte(certPEM))
	if err != nil {
		return CertSummary{Subject: "unreadable"}
	}
	return CertSummary{
		Subject:     cert.Subject.CommonName,
		Expires:     cert.NotAfter,
		Fingerprint: shortFingerprint(certPEM),
	}
}
