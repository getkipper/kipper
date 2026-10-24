package doctor

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getkipper/kipper/controller/pkg/hopca"
	"github.com/getkipper/kipper/controller/pkg/spki"
)

func testCert(t *testing.T, commonName string) (*x509.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: commonName},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return cert, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func node(name string, ready corev1.ConditionStatus) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: ready}}},
	}
}

func deployment(ns, name string, available int32) *appsv1.Deployment {
	want := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       appsv1.DeploymentSpec{Replicas: &want},
		Status:     appsv1.DeploymentStatus{AvailableReplicas: available},
	}
}

func TestCheckNodes(t *testing.T) {
	assert.Equal(t, Pass, CheckNodes(Snapshot{Nodes: []corev1.Node{node("server-1", corev1.ConditionTrue)}}).Status)

	failed := CheckNodes(Snapshot{Nodes: []corev1.Node{node("server-1", corev1.ConditionFalse)}})
	assert.Equal(t, Fail, failed.Status)
	assert.Contains(t, failed.Message, "server-1 is not Ready")

	fenced := node("worker-1", corev1.ConditionTrue)
	fenced.Spec.Taints = []corev1.Taint{{Key: "node.kubernetes.io/out-of-service", Effect: corev1.TaintEffectNoExecute}}
	assert.Equal(t, Fail, CheckNodes(Snapshot{Nodes: []corev1.Node{node("server-1", corev1.ConditionTrue), fenced}}).Status, "a fenced node is not Ready")

	unread := CheckNodes(Snapshot{Missing: map[Part]error{PartNodes: errors.New("refused")}})
	assert.Equal(t, Skip, unread.Status)
	assert.Contains(t, unread.Message, "not checked")
}

func TestCheckWorkloads(t *testing.T) {
	snap := Snapshot{Workloads: map[string]*appsv1.Deployment{
		"traefik/traefik":                   deployment("traefik", "traefik", 1),
		"kube-system/coredns":               deployment("kube-system", "coredns", 1),
		"cert-manager/cert-manager-webhook": deployment("cert-manager", "cert-manager-webhook", 1),
		"dex/dex":                           deployment("dex", "dex", 1),
		"kipper-system/console-api":         deployment("kipper-system", "console-api", 0),
	}}

	results := CheckWorkloads(snap)
	require.Len(t, results, len(CoreWorkloads))
	byLabel := map[string]Result{}
	for _, r := range results {
		byLabel[r.Label] = r
	}
	assert.Equal(t, Pass, byLabel["Traefik"].Status)
	assert.Equal(t, Fail, byLabel["Console API"].Status)
	assert.Contains(t, byLabel["Console API"].Message, "0/1 available")

	delete(snap.Workloads, "dex/dex")
	for _, r := range CheckWorkloads(snap) {
		if r.Label == "Dex" {
			assert.Equal(t, Fail, r.Status)
			assert.Contains(t, r.Message, "not installed")
		}
	}
}

func TestCheckHopCertificate(t *testing.T) {
	cert, certPEM := testCert(t, "kipper hop")

	result, keys := CheckHopCertificate(Snapshot{HopCert: certPEM})
	assert.Equal(t, Pass, result.Status)
	assert.Equal(t, []string{spki.Fingerprint(cert)}, keys)

	result, keys = CheckHopCertificate(Snapshot{})
	assert.Equal(t, Fail, result.Status)
	assert.Contains(t, result.Message, "kipper-hop-cert")
	assert.Empty(t, keys)

	result, _ = CheckHopCertificate(Snapshot{HopCert: "not a certificate"})
	assert.Equal(t, Fail, result.Status)
}

func TestCheckHopCertificate_AcceptsTheRotationCandidateToo(t *testing.T) {
	current, currentPEM := testCert(t, "current")
	candidate, candidatePEM := testCert(t, "candidate")

	_, keys := CheckHopCertificate(Snapshot{HopCert: currentPEM, HopCandidate: candidatePEM})
	assert.ElementsMatch(t, []string{spki.Fingerprint(current), spki.Fingerprint(candidate)}, keys)
}

func TestCheckTLSStore(t *testing.T) {
	ours := TLSStore{Namespace: "kipper-system", Name: "default", DefaultCert: "kipper-hop-cert"}
	assert.Equal(t, Pass, CheckTLSStore(Snapshot{TLSStores: []TLSStore{ours}}).Status)

	missing := CheckTLSStore(Snapshot{})
	assert.Equal(t, Fail, missing.Status)
	assert.Contains(t, missing.Message, "no default TLSStore")

	foreign := CheckTLSStore(Snapshot{TLSStores: []TLSStore{ours, {Namespace: "shop", Name: "default", DefaultCert: "shop-cert"}}})
	assert.Equal(t, Fail, foreign.Status)
	assert.Contains(t, foreign.Message, "shop/default")

	wrongSecret := CheckTLSStore(Snapshot{TLSStores: []TLSStore{{Namespace: "kipper-system", Name: "default", DefaultCert: "other"}}})
	assert.Equal(t, Fail, wrongSecret.Status)
}

func TestCheckHeartbeat(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	healthy := CheckHeartbeat(Snapshot{Heartbeat: []string{
		"2026-10-06T00:07:48Z gateway heartbeat: renewed lab.kipper.run (hop pin active)",
	}}, now)
	assert.Equal(t, Pass, healthy.Status)
	assert.Contains(t, healthy.Message, "11 hours ago")

	pending := CheckHeartbeat(Snapshot{Heartbeat: []string{
		"2026-10-06T00:07:48Z gateway heartbeat: renewed lab.kipper.run (hop pin active)",
		"2026-10-06T00:30:01Z gateway heartbeat: hop pin for lab.kipper.run is pending: asserted SPKI aaaa, gateway observed \"bbbb\"",
	}}, now)
	assert.Equal(t, Fail, pending.Status)
	assert.Contains(t, pending.Message, "different key")
	assert.NotContains(t, pending.Message, "lab.kipper.run", "the message names no host")
	assert.NotContains(t, pending.Message, "aaaa")
	assert.Contains(t, pending.Detail, "is pending", "the raw lines go into the scrubbed excerpt")

	quiet := CheckHeartbeat(Snapshot{}, now)
	assert.Equal(t, Skip, quiet.Status)
	assert.Contains(t, quiet.Message, "no heartbeat lines")
}

func TestCheckClusterHost(t *testing.T) {
	assert.Equal(t, Pass, CheckClusterHost(Snapshot{ClusterHost: "203.0.113.10"}, "203.0.113.10").Status)

	moved := CheckClusterHost(Snapshot{ClusterHost: "203.0.113.20"}, "203.0.113.10")
	assert.Equal(t, Fail, moved.Status)
	assert.NotContains(t, moved.Message, "203.0.113", "addresses stay out of the message")

	assert.Equal(t, Skip, CheckClusterHost(Snapshot{}, "203.0.113.10").Status)
}

func TestCheckHeartbeat_JudgesTheWholeLastBeat(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	proofFailed := CheckHeartbeat(Snapshot{Heartbeat: []string{
		"2026-10-06T11:00:00Z gateway heartbeat: renewed lab.kipper.run (hop pin active)",
		"2026-10-06T11:59:58Z gateway heartbeat: proof for lab.kipper.run not yet accepted: HTTP 202 could not dial",
		"2026-10-06T11:59:58Z gateway heartbeat: renewed lab.kipper.run (hop pin active)",
	}}, now)
	assert.Equal(t, Fail, proofFailed.Status, "a renewal logged after a failed proof in the same beat must not hide it")
	assert.Contains(t, proofFailed.Message, "proof of control")

	recovered := CheckHeartbeat(Snapshot{Heartbeat: []string{
		"2026-10-06T10:00:00Z gateway heartbeat: proof for lab.kipper.run not yet accepted: HTTP 202 could not dial",
		"2026-10-06T10:00:01Z gateway heartbeat: renewed lab.kipper.run (hop pin active)",
		"2026-10-06T11:59:58Z gateway heartbeat: renewed lab.kipper.run (hop pin active)",
	}}, now)
	assert.Equal(t, Pass, recovered.Status, "an older beat's failure no longer counts")
}

func TestCheckHeartbeat_TreatsAPausedRenewalAsHealthy(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	idle := CheckHeartbeat(Snapshot{Heartbeat: []string{
		"2026-10-06T11:59:58Z gateway heartbeat: no Ingress host has used lab.kipper.run since 2026-09-01T00:00:00Z; pausing renewal; the name will expire under the gateway's retention policy",
	}}, now)
	assert.Equal(t, Pass, idle.Status)
	assert.Contains(t, idle.Message, "paused")
}

func TestCheckHeartbeat_TreatsAnyUnknownLineInTheBeatAsAProblem(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	for _, failure := range []string{
		"gateway heartbeat: loading hop signing key for proof: secret not found",
		"gateway heartbeat: signing proof challenge: bad key",
		"gateway heartbeat: proof: dial tcp: i/o timeout",
		"gateway heartbeat: hop certificate: conflict",
	} {
		r := CheckHeartbeat(Snapshot{Heartbeat: []string{
			"2026-10-06T11:59:58Z " + failure,
			"2026-10-06T11:59:58Z gateway heartbeat: renewed lab.kipper.run (hop pin active)",
		}}, now)
		assert.Equal(t, Fail, r.Status, failure)
	}
}

func TestCheckHeartbeat_AcceptsTheHealthyLinesOfABeat(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	r := CheckHeartbeat(Snapshot{Heartbeat: []string{
		"2026-10-06T11:59:57Z gateway heartbeat: proved control of lab.kipper.run",
		"2026-10-06T11:59:58Z gateway heartbeat: gateway acknowledged the rotated hop key (aaaa); serving it now",
		"2026-10-06T11:59:58Z gateway heartbeat: renewed lab.kipper.run (hop pin active)",
	}}, now)
	assert.Equal(t, Pass, r.Status)
}

func TestCheckHeartbeat_WarnsWhenTheHeartbeatIsTurnedOff(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	r := CheckHeartbeat(Snapshot{Heartbeat: []string{
		"2026-10-06T11:59:58Z gateway heartbeat disabled: the ClusterIdentity sets gateway.register=false, so lab.kipper.run is not renewed from here",
	}}, now)
	assert.Equal(t, Warn, r.Status)
}

func TestCheckHeartbeat_JudgesTheBeatAfterARetry(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	// A failed proof triggers a retry a minute later, so the later beat determines the result.
	r := CheckHeartbeat(Snapshot{Heartbeat: []string{
		"2026-10-06T11:58:30Z gateway heartbeat: proof for lab.kipper.run not yet accepted: HTTP 202",
		"2026-10-06T11:58:31Z gateway heartbeat: renewed lab.kipper.run (hop pin active)",
		"2026-10-06T11:59:31Z gateway heartbeat: proved control of lab.kipper.run",
		"2026-10-06T11:59:32Z gateway heartbeat: renewed lab.kipper.run (hop pin active)",
	}}, now)
	assert.Equal(t, Pass, r.Status)
}

func TestCheckHeartbeat_KeepsASlowBeatTogether(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	// One beat: the proof fails, then promoting the rotated key takes almost a minute and succeeds.
	r := CheckHeartbeat(Snapshot{Heartbeat: []string{
		"2026-10-06T11:58:00Z gateway heartbeat: renewed lab.kipper.run (hop pin active)",
		"2026-10-06T11:58:30Z gateway heartbeat: proof: Post \"https://kipper.run/proof\": dial tcp: i/o timeout",
		"2026-10-06T11:59:25Z gateway heartbeat: gateway acknowledged the rotated hop key (aaaa); serving it now",
	}}, now)
	assert.Equal(t, Fail, r.Status, "a failure stays visible however long the rest of its beat takes")
}

func TestCheckHeartbeat_TreatsARegistrationErrorAsTheEndOfABeat(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	r := CheckHeartbeat(Snapshot{Heartbeat: []string{
		"2026-10-06T11:57:00Z gateway heartbeat: Post \"https://kipper.run/register\": dial tcp: i/o timeout",
		"2026-10-06T11:58:31Z gateway heartbeat: proved control of lab.kipper.run",
		"2026-10-06T11:58:32Z gateway heartbeat: renewed lab.kipper.run (hop pin active)",
	}}, now)
	assert.Equal(t, Pass, r.Status, "a failed registration ends its beat, so the clean beat after it counts")

	failing := CheckHeartbeat(Snapshot{Heartbeat: []string{
		"2026-10-06T11:58:32Z gateway heartbeat: renewed lab.kipper.run (hop pin active)",
		"2026-10-06T11:59:40Z gateway heartbeat: HTTP 429 slow down",
	}}, now)
	assert.Equal(t, Fail, failing.Status)
}

func TestCheckHeartbeat_FailsWhenTheLastBeatEndedBadly(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	r := CheckHeartbeat(Snapshot{Heartbeat: []string{
		"2026-10-06T11:58:00Z gateway heartbeat: renewed lab.kipper.run (hop pin active)",
		"2026-10-06T11:59:58Z gateway heartbeat: unexpected pin state \"weird\"",
	}}, now)
	assert.Equal(t, Fail, r.Status)
}

func TestCheckHeartbeat_TreatsTheOldGatewayWarningAsTheEndOfABeat(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	// An older console-api logs only this warning, once, at the end of the first beat against a gateway without pinning.
	r := CheckHeartbeat(Snapshot{Heartbeat: []string{
		"2026-10-06T11:00:00Z gateway heartbeat: HTTP 503 unavailable",
		"2026-10-06T11:01:00Z gateway heartbeat: the gateway does not support certificate pinning yet; the lab.kipper.run hop stays unpinned until it is upgraded",
	}}, now)
	assert.Equal(t, Pass, r.Status)
}

func TestCheckHeartbeat_KeepsTheFirstUnpinnedBeatWhole(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	r := CheckHeartbeat(Snapshot{Heartbeat: []string{
		"2026-10-06T11:58:00Z gateway heartbeat: renewed lab.kipper.run (hop pin active)",
		"2026-10-06T11:59:00Z gateway heartbeat: hop certificate: conflict",
		"2026-10-06T11:59:01Z gateway heartbeat: renewed lab.kipper.run (unpinned): the gateway does not support certificate pinning yet; the hop stays unpinned until it is upgraded",
	}}, now)
	assert.Equal(t, Fail, r.Status, "the warning and the renewal are one line, so the beat stays whole")
}

func TestCheckTraefikCopy(t *testing.T) {
	live, livePEM := testCert(t, "kipper hop")
	_, otherPEM := testCert(t, "other")
	keys := []string{spki.Fingerprint(live)}

	assert.Equal(t, Pass, CheckTraefikCopy(Snapshot{TraefikCopyCert: livePEM}, keys).Status)

	absent := CheckTraefikCopy(Snapshot{}, keys)
	assert.Equal(t, Warn, absent.Status)
	assert.Contains(t, absent.Message, "kip upgrade")

	stale := CheckTraefikCopy(Snapshot{TraefikCopyCert: otherPEM}, keys)
	assert.Equal(t, Fail, stale.Status)
	assert.Contains(t, stale.Message, "different key")

	unread := CheckTraefikCopy(Snapshot{Missing: map[Part]error{PartTraefikCopy: errors.New("no SSH")}}, keys)
	assert.Equal(t, Skip, unread.Status)
}

func TestCheckTraefikCopy_ComparesWithTheLiveKeyOnly(t *testing.T) {
	live, _ := testCert(t, "live")
	_, candidatePEM := testCert(t, "candidate")
	candidate, _ := parseCertificate(candidatePEM)

	r := CheckTraefikCopy(Snapshot{TraefikCopyCert: candidatePEM}, []string{spki.Fingerprint(live), spki.Fingerprint(candidate)})
	assert.Equal(t, Fail, r.Status, "the copy must never serve a rotation candidate")
}

func testCertsSharingAKey(t *testing.T) (first, second string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	sign := func(serial int64) string {
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "kipper hop"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		require.NoError(t, err)
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	}
	return sign(1), sign(2)
}

func traefikWithCopy(declared, mounted bool) *appsv1.Deployment {
	dep := deployment(hopca.TraefikNamespace, hopca.TraefikDeploymentName, 1)
	container := corev1.Container{Name: "traefik"}
	if declared {
		dep.Spec.Template.Spec.Volumes = []corev1.Volume{{
			Name:         "kipper-hop",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: hopca.TraefikCopyName}},
		}}
	}
	if mounted {
		container.VolumeMounts = []corev1.VolumeMount{{Name: "kipper-hop", MountPath: hopca.TraefikMountPath, ReadOnly: true}}
	}
	dep.Spec.Template.Spec.Containers = []corev1.Container{container}
	return dep
}

func TestCheckTraefikCopy_WarnsWhenTheCopyLagsBehindARenewedCertificate(t *testing.T) {
	renewed, lagging := testCertsSharingAKey(t)
	hop, err := parseCertificate(renewed)
	require.NoError(t, err)
	keys := []string{spki.Fingerprint(hop)}

	r := CheckTraefikCopy(Snapshot{HopCert: renewed, TraefikCopyCert: lagging}, keys)
	assert.Equal(t, Warn, r.Status)
	assert.Contains(t, r.Message, "different certificate")

	assert.Equal(t, Pass, CheckTraefikCopy(Snapshot{HopCert: renewed, TraefikCopyCert: renewed}, keys).Status)
}

func TestCheckTraefikCopy_WarnsWhenTraefikDoesNotMountTheCopy(t *testing.T) {
	live, livePEM := testCert(t, "kipper hop")
	keys := []string{spki.Fingerprint(live)}
	key := hopca.TraefikNamespace + "/" + hopca.TraefikDeploymentName

	check := func(dep *appsv1.Deployment) Result {
		return CheckTraefikCopy(Snapshot{HopCert: livePEM, TraefikCopyCert: livePEM,
			Workloads: map[string]*appsv1.Deployment{key: dep}}, keys)
	}

	absent := check(traefikWithCopy(false, false))
	assert.Equal(t, Warn, absent.Status)
	assert.Contains(t, absent.Message, "does not mount")
	assert.Contains(t, absent.Message, "kip upgrade")

	assert.Equal(t, Warn, check(traefikWithCopy(true, false)).Status, "a declared volume no container mounts is not a mount")
	assert.Equal(t, Pass, check(traefikWithCopy(true, true)).Status)

	sidecar := traefikWithCopy(true, false)
	sidecar.Spec.Template.Spec.Containers = append(sidecar.Spec.Template.Spec.Containers, corev1.Container{
		Name: "sidecar", VolumeMounts: []corev1.VolumeMount{{Name: "kipper-hop", MountPath: hopca.TraefikMountPath}},
	})
	assert.Equal(t, Warn, check(sidecar).Status, "another container's mount does not reach Traefik")
}
