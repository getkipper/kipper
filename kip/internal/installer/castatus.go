package installer

import (
	"time"

	"github.com/getkipper/kipper/controller/pkg/hopca"
	"github.com/getkipper/kipper/controller/pkg/hostnames"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

// CAStatus answers the question an operator actually has: does everything
// agree, and if not, what do I run? It is safe to ask at any time, including
// while a rotation is part-way through and including when login is broken —
// it reads over SSH and never depends on the OIDC path it reports on.
type CAStatus struct {
	// Phase is where a rotation has got to; steady means none is in flight.
	Phase CAPhase
	// Resume names the step of the documented procedure to carry on from,
	// empty when nothing is in flight.
	Resume string
	// Authority describes the authority currently signing.
	Authority CertSummary
	// Incoming and Outgoing are populated only during a rotation.
	Incoming, Outgoing *CertSummary
	// Leaf describes the certificate the cluster serves.
	Leaf CertSummary
	// TrustedByAPIServer is the whole invariant: the anchor covers whatever
	// signed the served certificate, AND the API server has loaded that anchor.
	TrustedByAPIServer bool
	// AnchorCovers and AnchorLoaded are the two halves, reported separately
	// because they fail for different reasons and need different fixes. An
	// anchor that covers but is not loaded means a sync was interrupted; one
	// that does not cover means the wrong authority is anchored.
	AnchorCovers, AnchorLoaded bool
	// AnchorLoadedUnknown records that the API server could not be asked what
	// it has loaded. Reported rather than folded into AnchorLoaded: "it has not
	// taken this config" and "it did not answer" need different responses, and
	// only one of them is repaired by a sync.
	AnchorLoadedUnknown bool
	// ServedByActive is whether the wire shows a certificate the active
	// authority signed. Nil when there is no gateway-fronted host to ask.
	ServedByActive *bool
	// Hosts are the issuers the API server trusts.
	Hosts []string
	// Problems are malformed states that need attention before anything else.
	Problems []string
}

// CertSummary is the human-facing description of one certificate.
type CertSummary struct {
	Subject string
	Expires time.Time
	// Fingerprint distinguishes two authorities with the same subject, which
	// is exactly the situation during a rotation.
	Fingerprint string
}

// Healthy reports whether the cluster needs no attention: no rotation in
// flight, nothing malformed, and both sides of the trust relationship agree.
func (s CAStatus) Healthy() bool {
	if s.Phase != CAPhaseSteady || len(s.Problems) > 0 || !s.TrustedByAPIServer {
		return false
	}
	// A check that could not run is not a check that passed. Saying "nothing to
	// do" on the strength of an unanswered question is how an operator stops
	// looking at a cluster that still needs them.
	if s.AnchorLoadedUnknown {
		return false
	}
	return s.ServedByActive == nil || *s.ServedByActive
}

// NextCommand is the command that moves this cluster toward a healthy state,
// empty when there is nothing to run — either because nothing needs doing, or
// because what is wrong is repaired by the documented procedure rather than by
// a command. This is the field that makes the status worth printing: a
// diagnosis nobody can act on is not a diagnosis.
func (s CAStatus) NextCommand() string {
	if len(s.Problems) > 0 {
		return ""
	}
	// Sync re-renders the config from the anchor already on disk, so it repairs
	// an anchor the API server never loaded. It cannot repair an anchor that
	// names the wrong authority, because it adds nothing to that file, and it
	// is not offered on the strength of a check that never ran.
	if s.AnchorCovers && !s.AnchorLoaded && !s.AnchorLoadedUnknown {
		return "kip cluster auth sync"
	}
	return ""
}

// ReadCAStatus assembles the full picture, including one live check against
// the wire. The wire check is what turns "the Secrets look right" into "the
// cluster is actually serving what we think", which is the distinction three
// abandoned designs failed on.
func ReadCAStatus(client *ssh.Client) (CAStatus, error) {
	state, err := ReadCAState(client)
	if err != nil {
		return CAStatus{}, err
	}

	status := CAStatus{
		Phase:    state.Phase(),
		Resume:   state.ResumePoint(),
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

	// The invariant has two halves, and reporting only the first is what made
	// the abandoned designs unsafe. The anchor must cover whatever signed the
	// served certificate — and the API server must have LOADED that anchor. A
	// file on disk is an input to the API server, not evidence about it.
	anchorCovers := state.LeafCert == "" || hopca.SignedByAny([]byte(state.LeafCert), []byte(state.Anchor))
	if anchorCovers && len(state.DexHosts) > 0 {
		loaded, known, aerr := anchorIsActive(client, state)
		if aerr != nil {
			return CAStatus{}, aerr
		}
		status.AnchorLoaded = loaded
		status.AnchorLoadedUnknown = !known
		status.TrustedByAPIServer = loaded
	} else {
		status.TrustedByAPIServer = false
	}
	status.AnchorCovers = anchorCovers

	if host := firstGatewayHost(state.DexHosts); host != "" && state.Active != "" {
		served, serr := ServedByAuthority(client, host, state.Active)
		if serr != nil {
			return CAStatus{}, serr
		}
		status.ServedByActive = &served
	}
	return status, nil
}

// firstGatewayHost returns a host served the cluster's own hop certificate.
// Custom domains carry WebPKI certificates and say nothing about the hop
// authority, so they are not worth asking.
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
