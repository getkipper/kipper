package controllers

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/stretchr/testify/assert"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/hopcert"
	"github.com/getkipper/kipper/controller/pkg/authncfg"
	"github.com/getkipper/kipper/controller/pkg/hopca"
)

type fakeMetrics struct {
	body string
	err  error
}

func (f fakeMetrics) ReadMetrics(context.Context) (string, error) { return f.body, f.err }

func ciInTransition(fromDex, toDex string) *kipperv1.ClusterIdentity {
	return &kipperv1.ClusterIdentity{
		Status: kipperv1.ClusterIdentityStatus{
			Transition: &kipperv1.TransitionStatus{
				From: &kipperv1.ResolvedHosts{Dex: fromDex},
				To:   &kipperv1.ResolvedHosts{Dex: toDex},
			},
		},
	}
}

func metricsWithHash(hash string) string {
	return `apiserver_authentication_config_controller_last_config_info{apiserver_id_hash="sha256:abc",hash="` + hash + `"} 1` + "\n"
}

func TestAuthnConfigStagedGate(t *testing.T) {
	ci := ciInTransition("dex.old.example.com", "dex.new.example.com")
	stagedHash := authncfg.Hash(authncfg.Render("", authncfg.HostsFor("dex.old.example.com", "dex.new.example.com")...))

	t.Run("staged union hash present → proceed", func(t *testing.T) {
		r := &ClusterIdentityReconciler{Client: emptyClient(), Metrics: fakeMetrics{body: metricsWithHash(stagedHash)}}
		ok, _ := r.authnConfigStaged(context.Background(), ci)
		assert.True(t, ok)
	})

	t.Run("only the old single-issuer hash present → park", func(t *testing.T) {
		oldOnly := authncfg.Hash(authncfg.Render("", "dex.old.example.com"))
		r := &ClusterIdentityReconciler{Client: emptyClient(), Metrics: fakeMetrics{body: metricsWithHash(oldOnly)}}
		ok, msg := r.authnConfigStaged(context.Background(), ci)
		assert.False(t, ok)
		assert.Contains(t, msg, "kip cluster domain --sync")
	})

	t.Run("metrics unreadable → fail closed (park)", func(t *testing.T) {
		r := &ClusterIdentityReconciler{Client: emptyClient(), Metrics: fakeMetrics{err: assertAnErr{}}}
		ok, _ := r.authnConfigStaged(context.Background(), ci)
		assert.False(t, ok)
	})

	t.Run("no MetricsReader wired → gate inert (tests)", func(t *testing.T) {
		r := &ClusterIdentityReconciler{}
		ok, _ := r.authnConfigStaged(context.Background(), ci)
		assert.True(t, ok)
	})
}

type assertAnErr struct{}

func (assertAnErr) Error() string { return "metrics endpoint unreachable" }

// emptyClient is a cluster with nothing in it, which is what a reconciler needs
// to read the certificate authority the rendered config anchors on. Absent is a
// valid answer — a cluster that only ever served a custom domain has no CA — so
// these gates render the same empty anchor kip would.
func emptyClient() crclient.Client {
	return crfake.NewClientBuilder().WithScheme(testScheme()).Build()
}

// kip writes the API server's trust anchor to a file and this reconciler hashes
// its own rendering of the same material to decide whether the API server has
// caught up. If the two disagree by a single byte the gate can never pass, so a
// rollover — which is the only time a second authority exists — must render
// identically on both sides.
func TestGateAnchorsOnTheSameBundleKipWrites(t *testing.T) {
	active, err := hopca.New("kipper.run")
	if err != nil {
		t.Fatal(err)
	}
	retained, err := hopca.New("kipper.run")
	if err != nil {
		t.Fatal(err)
	}

	client := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: hopcert.CASecretName, Namespace: hopcert.Namespace},
		Data: map[string][]byte{
			corev1.TLSCertKey:       active.CACertPEM,
			hopcert.RetainedCAKey:   retained.CACertPEM,
			corev1.TLSPrivateKeyKey: active.CAKeyPEM,
		},
	}).Build()
	r := &ClusterIdentityReconciler{Client: client}

	got, err := r.hopCA(context.Background())
	if err != nil {
		t.Fatalf("reading the anchor: %v", err)
	}
	want := string(hopca.Bundle(active.CACertPEM, retained.CACertPEM))
	if got != want {
		t.Error("the reconciler's anchor does not match the bundle kip writes, so the cutover gate can never pass")
	}
	// Both authorities have to be in there, or a rollover mid-flight parks.
	if !strings.Contains(got, strings.TrimSpace(string(retained.CACertPEM))) {
		t.Error("the retained authority is missing from the rendered anchor")
	}

	// With no rollover in flight the anchor is just the active authority.
	steadyClient := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: hopcert.CASecretName, Namespace: hopcert.Namespace},
		Data:       map[string][]byte{corev1.TLSCertKey: active.CACertPEM, corev1.TLSPrivateKeyKey: active.CAKeyPEM},
	}).Build()
	steady := &ClusterIdentityReconciler{Client: steadyClient}
	steadyAnchor, err := steady.hopCA(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if steadyAnchor != string(hopca.Bundle(active.CACertPEM, nil)) {
		t.Error("the steady-state anchor must be exactly the active authority")
	}

	// A cluster that never had one renders no anchor, which is what a
	// custom-domain-only cluster legitimately has.
	none := &ClusterIdentityReconciler{Client: crfake.NewClientBuilder().WithScheme(testScheme()).Build()}
	if anchor, err := none.hopCA(context.Background()); err != nil || anchor != "" {
		t.Errorf("no authority must render no anchor, got %q err %v", anchor, err)
	}
}
