package doctor

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/getkipper/kipper/controller/pkg/hopca"
	"github.com/getkipper/kipper/controller/pkg/spki"
)

// Status is the outcome of one check.
type Status int

const (
	Pass Status = iota
	Warn
	Fail
	Skip
)

// Result is one check's outcome. Message is shown in the terminal. Detail holds
// raw evidence that must be scrubbed before inclusion in a shareable report.
type Result struct {
	ID      string
	Label   string
	Status  Status
	Message string
	Detail  string
}

func notChecked(id, label string, part Part, err error) Result {
	return Result{ID: id, Label: label, Status: Skip, Message: fmt.Sprintf("not checked (could not read the %s)", part), Detail: err.Error()}
}

// CheckNodes passes when at least one node exists and every node reports Ready.
// It skips the check if the node list could not be read.
func CheckNodes(snap Snapshot) Result {
	if err, ok := snap.Missing[PartNodes]; ok {
		return notChecked("nodes", "Nodes", PartNodes, err)
	}
	var notReady []string
	for _, n := range snap.Nodes {
		if !nodeReady(n) {
			notReady = append(notReady, n.Name)
		}
	}
	switch {
	case len(snap.Nodes) == 0:
		return Result{ID: "nodes", Label: "Nodes", Status: Fail, Message: "the cluster lists no nodes"}
	case len(notReady) > 0:
		return Result{ID: "nodes", Label: "Nodes", Status: Fail, Message: fmt.Sprintf("node %s is not Ready", strings.Join(notReady, ", "))}
	}
	return Result{ID: "nodes", Label: "Nodes", Status: Pass, Message: fmt.Sprintf("%d %s Ready", len(snap.Nodes), plural(len(snap.Nodes), "node", "nodes"))}
}

func nodeReady(n corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// CheckWorkloads returns results in CoreWorkloads order. A missing workload,
// zero desired replicas or fewer available replicas than requested fails the
// check. If workload reads failed, every workload check is skipped.
func CheckWorkloads(snap Snapshot) []Result {
	results := make([]Result, 0, len(CoreWorkloads))
	for _, w := range CoreWorkloads {
		id := "workload:" + w.Name
		if err, ok := snap.Missing[PartWorkloads]; ok {
			results = append(results, notChecked(id, w.Label, PartWorkloads, err))
			continue
		}
		d, ok := snap.Workloads[w.Key()]
		if !ok {
			results = append(results, Result{ID: id, Label: w.Label, Status: Fail, Message: w.Label + " is not installed"})
			continue
		}
		want := int32(1)
		if d.Spec.Replicas != nil {
			want = *d.Spec.Replicas
		}
		if d.Status.AvailableReplicas < want || want == 0 {
			results = append(results, Result{ID: id, Label: w.Label, Status: Fail,
				Message: fmt.Sprintf("%s %d/%d available", w.Label, d.Status.AvailableReplicas, want)})
			continue
		}
		results = append(results, Result{ID: id, Label: w.Label, Status: Pass, Message: w.Label + " running"})
	}
	return results
}

// CheckHopCertificate parses the hop certificate and any rotation candidate,
// and returns the SPKI fingerprints the cluster is meant to serve, current key
// first. An absent or unreadable candidate is ignored. Missing or invalid current
// certificates fail the check. A read failure skips it. Both return nil fingerprints.
func CheckHopCertificate(snap Snapshot) (Result, []string) {
	const id, label = "hop-secret", "Hop certificate"
	if err, ok := snap.Missing[PartHopCert]; ok {
		return notChecked(id, label, PartHopCert, err), nil
	}
	if snap.HopCert == "" {
		return Result{ID: id, Label: label, Status: Fail, Message: "the Secret kipper-system/kipper-hop-cert is missing"}, nil
	}
	cert, err := parseCertificate(snap.HopCert)
	if err != nil {
		return Result{ID: id, Label: label, Status: Fail, Message: "the Secret kipper-system/kipper-hop-cert holds no readable certificate", Detail: err.Error()}, nil
	}
	keys := []string{spki.Fingerprint(cert)}
	if candidate, err := parseCertificate(snap.HopCandidate); err == nil {
		keys = append(keys, spki.Fingerprint(candidate))
	}
	return Result{ID: id, Label: label, Status: Pass, Message: "hop certificate present"}, keys
}

// CheckTraefikCopy compares the SPKI fingerprint in Traefik's copy with keys[0],
// the live key returned by CheckHopCertificate. Only the live key may match,
// because Traefik loads this copy at startup before its Kubernetes provider
// syncs and must not serve a staged candidate. A copy read failure or empty keys
// skips the check. An absent copy warns, and an unreadable certificate or
// different key fails. A matching key still warns if the supplied Deployment
// lacks the copy's mount on the Traefik container at hopca.TraefikMountPath,
// or if a readable hop certificate differs from the copy. Otherwise it passes.
// The private key and rollout are not checked.
func CheckTraefikCopy(snap Snapshot, keys []string) Result {
	const id, label = "traefik-copy", "Traefik's copy"
	if err, ok := snap.Missing[PartTraefikCopy]; ok {
		return notChecked(id, label, PartTraefikCopy, err)
	}
	if len(keys) == 0 {
		return Result{ID: id, Label: label, Status: Skip, Message: "not checked (no hop certificate to compare with)"}
	}
	if snap.TraefikCopyCert == "" {
		return Result{ID: id, Label: label, Status: Warn,
			Message: "Traefik has no copy of the hop certificate, so after a restart it waits for its Kubernetes provider (run kip upgrade)"}
	}
	cert, err := parseCertificate(snap.TraefikCopyCert)
	if err != nil {
		return Result{ID: id, Label: label, Status: Fail, Message: "Traefik's copy of the hop certificate is unreadable", Detail: err.Error()}
	}
	if spki.Fingerprint(cert) != keys[0] {
		return Result{ID: id, Label: label, Status: Fail,
			Message: "Traefik's copy holds a different key from the hop certificate. Traefik may already serve this key and will load it on its next restart. The gateway refuses this key"}
	}
	if dep := snap.Workloads[hopca.TraefikNamespace+"/"+hopca.TraefikDeploymentName]; dep != nil && !mountsTraefikCopy(dep) {
		return Result{ID: id, Label: label, Status: Warn,
			Message: "Traefik does not mount its copy of the hop certificate, so after a restart it waits for its Kubernetes provider to load the certificate. Run a full kip upgrade"}
	}
	if hop, err := parseCertificate(snap.HopCert); err == nil && !bytes.Equal(hop.Raw, cert.Raw) {
		return Result{ID: id, Label: label, Status: Warn,
			Message: "Traefik's copy has the same public key as the hop certificate but a different certificate. console-api synchronizes the copy on each gateway heartbeat. If this persists, check the gateway heartbeat"}
	}
	return Result{ID: id, Label: label, Status: Pass, Message: "Traefik's copy matches the hop certificate's public key"}
}

// traefikContainer is the container name the Traefik chart gives its proxy.
const traefikContainer = "traefik"

func mountsTraefikCopy(dep *appsv1.Deployment) bool {
	pod := dep.Spec.Template.Spec
	for _, v := range pod.Volumes {
		if v.Secret == nil || v.Secret.SecretName != hopca.TraefikCopyName {
			continue
		}
		for _, c := range pod.Containers {
			if c.Name != traefikContainer {
				continue
			}
			for _, m := range c.VolumeMounts {
				if m.Name == v.Name && m.MountPath == hopca.TraefikMountPath {
					return true
				}
			}
		}
	}
	return false
}

func parseCertificate(pemText string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, fmt.Errorf("no PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

// CheckTLSStore passes when Traefik's default certificate is the hop
// certificate and no other namespace declares a competing default store.
// It skips the check if TLS stores could not be read.
func CheckTLSStore(snap Snapshot) Result {
	const id, label = "tlsstore", "TLS store"
	if err, ok := snap.Missing[PartTLSStores]; ok {
		return notChecked(id, label, PartTLSStores, err)
	}
	var ours *TLSStore
	var foreign []string
	for i, s := range snap.TLSStores {
		if s.Name != "default" {
			continue
		}
		if s.Namespace == hopNamespace {
			ours = &snap.TLSStores[i]
		} else {
			foreign = append(foreign, s.Namespace+"/"+s.Name)
		}
	}
	switch {
	case len(foreign) > 0:
		return Result{ID: id, Label: label, Status: Fail,
			Message: fmt.Sprintf("another default TLSStore (%s) displaces the hop certificate", strings.Join(foreign, ", "))}
	case ours == nil:
		return Result{ID: id, Label: label, Status: Fail, Message: "no default TLSStore points Traefik at the hop certificate"}
	case ours.DefaultCert != hopCertName:
		return Result{ID: id, Label: label, Status: Fail, Message: "the default TLSStore serves a different Secret than kipper-hop-cert"}
	}
	return Result{ID: id, Label: label, Status: Pass, Message: "the default TLSStore points at the hop certificate"}
}

// healthyBeatLines are the heartbeat lines of a beat that went well. Any
// other heartbeat line in the newest beat is a problem, so a failure logged
// before a renewal in the same beat cannot pass as healthy.
var healthyBeatLines = []string{
	"gateway heartbeat: renewed",
	"pausing renewal",
	"proved control of",
	"re-created the registration",
	"acknowledged the rotated hop key",
	"does not support certificate pinning yet",
}

// heartbeatReasons maps known log markers to explanations. The first match
// wins, and unmatched problem lines get a generic reason.
var heartbeatReasons = []struct{ marker, reason string }{
	{"NOT running", "the heartbeat is not running in console-api"},
	{"is pending", "the hop pin is pending: the gateway observes a different key than the cluster asserts"},
	{"not yet accepted", "the gateway has not accepted the cluster's proof of control"},
	{"proof", "the cluster could not prove control of its name to the gateway"},
	{"foreign default TLSStore", "the heartbeat refuses to assert a key while another default TLSStore exists"},
	{"no gateway token", "the cluster holds no gateway token, so it cannot prove control of its name"},
	{"HTTP ", "the gateway rejected the last heartbeat"},
	{"reading gateway credentials", "console-api cannot read the cluster's gateway credentials"},
	{"hop certificate", "console-api could not maintain the hop certificate"},
}

// beatOutcomes are the lines console-api logs as the last thing a beat does
// (console-api/gateway_heartbeat.go beat, pinInterval and handlePinResponse,
// and the paused renewal in gateway_name_usage.go). Problems found mid-beat,
// such as a failed proof, are always followed by one of these, so splitting
// at them keeps every line of a beat together however long the beat takes.
// The `/register"` marker assumes /register is not redirected, because a
// redirect would name another URL in the transport error.
var beatOutcomes = []string{
	"gateway heartbeat: renewed",
	// An older console-api logs only this, once, when the gateway does not pin.
	"does not support certificate pinning yet",
	"is pending",
	"acknowledged the rotated hop key",
	"unexpected pin state",
	"promoting rotated hop certificate:",
	"re-created the registration",
	"persisting re-issued gateway token",
	"pausing renewal",
	"gateway heartbeat: HTTP ",
	`/register"`,
	"gateway heartbeat: encoding request",
	"gateway heartbeat: build request",
}

// CheckHeartbeat checks the newest complete beat and any following lines from
// a beat still running. Heartbeat lines must be ordered oldest first. Outcome
// markers keep slow beats together, so a renewal cannot hide an earlier failure
// in the same beat. Missing or empty logs skip the check, and a disabled heartbeat
// produces a warning. now is used to describe renewal age, with no freshness limit.
// Message omits hosts and keys. Raw lines go into Detail for the scrubbed report.
func CheckHeartbeat(snap Snapshot, now time.Time) Result {
	const id, label = "heartbeat", "Gateway heartbeat"
	if err, ok := snap.Missing[PartHeartbeat]; ok {
		return notChecked(id, label, PartHeartbeat, err)
	}
	if len(snap.Heartbeat) == 0 {
		return Result{ID: id, Label: label, Status: Skip,
			Message: "not checked (no heartbeat lines in the last 48 hours). console-api may have restarted"}
	}
	detail := strings.Join(snap.Heartbeat, "\n")
	beat := currentBeat(snap.Heartbeat)

	for _, line := range beat {
		if strings.Contains(line, "gateway heartbeat disabled") {
			return Result{ID: id, Label: label, Status: Warn, Message: "the heartbeat is turned off for this cluster (gateway.register=false)", Detail: detail}
		}
	}
	for _, line := range beat {
		if isHealthyBeatLine(line) {
			continue
		}
		reason := "the last heartbeat logged a problem (see the report)"
		for _, r := range heartbeatReasons {
			if strings.Contains(line, r.marker) {
				reason = r.reason
				break
			}
		}
		if at, ok := lastRenewal(snap.Heartbeat); ok {
			reason += fmt.Sprintf(" (last renewal %s ago)", ago(now.Sub(at)))
		}
		return Result{ID: id, Label: label, Status: Fail, Message: reason, Detail: detail}
	}
	for _, line := range beat {
		if strings.Contains(line, "pausing renewal") {
			return Result{ID: id, Label: label, Status: Pass, Message: "renewal is paused because no app uses the name yet", Detail: detail}
		}
	}
	if at, ok := lastRenewal(beat); ok {
		return Result{ID: id, Label: label, Status: Pass, Message: fmt.Sprintf("last renewed the name %s ago", ago(now.Sub(at))), Detail: detail}
	}
	return Result{ID: id, Label: label, Status: Pass, Message: "the last heartbeat went through", Detail: detail}
}

func isHealthyBeatLine(line string) bool {
	return containsAny(line, healthyBeatLines)
}

// currentBeat returns the lines of the newest complete beat together with
// any lines logged after it by a beat still running.
func currentBeat(lines []string) []string {
	var outcomes []int
	for i, line := range lines {
		if containsAny(line, beatOutcomes) {
			outcomes = append(outcomes, i)
		}
	}
	if len(outcomes) < 2 {
		return lines
	}
	return lines[outcomes[len(outcomes)-2]+1:]
}

func containsAny(line string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(line, m) {
			return true
		}
	}
	return false
}

func lastRenewal(lines []string) (time.Time, bool) {
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], "gateway heartbeat: renewed") {
			return lineTime(lines[i])
		}
	}
	return time.Time{}, false
}

func lineTime(line string) (time.Time, bool) {
	stamp, _, _ := strings.Cut(line, " ")
	t, err := time.Parse(time.RFC3339Nano, stamp)
	return t, err == nil
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d %s", int(d.Minutes()), plural(int(d.Minutes()), "minute", "minutes"))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d %s", int(d.Hours()), plural(int(d.Hours()), "hour", "hours"))
	}
	days := int(d.Hours() / 24)
	return fmt.Sprintf("%d %s", days, plural(days, "day", "days"))
}

// CheckClusterHost passes when the address the cluster registers with the
// gateway is the one kip has on record for it. It skips the check if the address
// is empty or could not be read.
func CheckClusterHost(snap Snapshot, configHost string) Result {
	const id, label = "cluster-host", "Registered address"
	if err, ok := snap.Missing[PartClusterHost]; ok {
		return notChecked(id, label, PartClusterHost, err)
	}
	if snap.ClusterHost == "" {
		return Result{ID: id, Label: label, Status: Skip, Message: "not checked (the cluster records no gateway address)"}
	}
	if snap.ClusterHost != configHost {
		return Result{ID: id, Label: label, Status: Fail, Message: "the cluster registers a different address with the gateway than kip has on record",
			Detail: fmt.Sprintf("cluster: %s, kip: %s", snap.ClusterHost, configHost)}
	}
	return Result{ID: id, Label: label, Status: Pass, Message: "the cluster registers the address kip has on record"}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
