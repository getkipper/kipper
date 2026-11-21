package migration

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

func ownedBy(kind, name string) []metav1.OwnerReference {
	controller := true
	return []metav1.OwnerReference{{
		APIVersion: kipperv1.GroupVersion.String(),
		Kind:       kind,
		Name:       name,
		UID:        types.UID("uid-" + name),
		Controller: &controller,
	}}
}

// A workload's published environment is named for a digest of its own content,
// so a copy lands at exactly the name the target will compute — carrying an
// owner reference to a UID that does not exist there. The receiving controller
// refuses it as somebody else's object and stops publishing, and the workload
// never starts. It has to stay behind and be republished.
func TestTransferableSecret_LeavesAPublishedEnvironmentBehind(t *testing.T) {
	cases := []struct {
		name   string
		secret *corev1.Secret
		want   bool
	}{
		{
			name: "an app's published environment",
			secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name:            secretname.EnvGeneration(secretname.KindApp, "api", "9f2c1a7b40de"),
				OwnerReferences: ownedBy("App", "api"),
			}},
			want: false,
		},
		{
			name: "a function's",
			secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name:            secretname.EnvGeneration(secretname.KindFunction, "resize", "aabbccddeeff"),
				OwnerReferences: ownedBy("Function", "resize"),
			}},
			want: false,
		},
		{
			name: "a job's",
			secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name:            secretname.EnvGeneration(secretname.KindJob, "migrate", "112233445566"),
				OwnerReferences: ownedBy("Job", "migrate"),
			}},
			want: false,
		},
		{
			name: "the pre-generation env Secret, which still travels",
			secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name:            secretname.Env(secretname.KindApp, "api"),
				OwnerReferences: ownedBy("App", "api"),
			}},
			want: true,
		},
		{
			name: "an ordinary Secret whose name only looks like one",
			secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: secretname.EnvGeneration(secretname.KindApp, "api", "9f2c1a7b40de"),
			}},
			want: true,
		},
		{
			name: "a workload's own secrets, which travel",
			secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name:            secretname.Secrets(secretname.KindApp, "api"),
				OwnerReferences: ownedBy("App", "api"),
			}},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, transferableSecret(tc.secret, map[string]bool{}, nil))
		})
	}
}

func TestTransferableSecret_LeavesAServiceCertificateBehind(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "db-tls", Labels: map[string]string{labels.TLSFor: "db"}}}

	assert.False(t, transferableSecret(secret, nil, nil), "the target cluster issues its own key from its own CA")
}

// Only the certificates Kipper's own routes request stay behind: the target's
// cert-manager writes those names again. Every other TLS Secret travels, even
// one from the same issuer, because nothing on the target recreates it.
func TestTransferableSecret_LeavesOnlyRouteCertificatesBehind(t *testing.T) {
	ingress := func(name, secretName string, managed bool) *networkingv1.Ingress {
		ing := &networkingv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: "shop-prod",
				Annotations: map[string]string{"cert-manager.io/cluster-issuer": "letsencrypt-prod"},
			},
			Spec: networkingv1.IngressSpec{TLS: []networkingv1.IngressTLS{{Hosts: []string{name + ".example.com"}, SecretName: secretName}}},
		}
		if managed {
			ing.Labels = map[string]string{"app.kubernetes.io/managed-by": "kipper"}
		}
		return ing
	}
	issuedBy := func(name string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop-prod", Annotations: map[string]string{
				"cert-manager.io/certificate-name": name,
				"cert-manager.io/issuer-kind":      "ClusterIssuer",
				"cert-manager.io/issuer-name":      "letsencrypt-prod",
			}},
			Type: corev1.SecretTypeTLS,
		}
	}
	h := &Handler{Client: fake.NewSimpleClientset(
		ingress("web", "web-tls", true),
		ingress("partner", "partner-tls", false),
	)}

	routeCerts, err := h.routeCertificates(context.Background(), "shop-prod")
	require.NoError(t, err)

	uploaded := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "own-cert"}, Type: corev1.SecretTypeTLS}
	assert.False(t, transferableSecret(issuedBy("web-tls"), nil, routeCerts), "the target issues the certificate for the route it serves there")
	assert.True(t, transferableSecret(issuedBy("partner-tls"), nil, routeCerts), "a certificate for a route the user runs themselves must travel")
	assert.True(t, transferableSecret(issuedBy("external-tls"), nil, routeCerts), "a certificate from a Certificate of the user's own must travel")
	assert.True(t, transferableSecret(uploaded, nil, routeCerts), "a certificate the user uploaded must travel")
}
