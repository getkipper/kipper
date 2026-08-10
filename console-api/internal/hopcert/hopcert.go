// Package hopcert provisions the cluster's stable gateway-hop serving
// certificate. The kipper.run gateway verifies its proxied TLS hop against a
// pinned SPKI fingerprint instead of WebPKI, so this certificate's job is to
// hold a stable keypair: Traefik serves it for every SNI without a named
// certificate via the default TLSStore, replacing the generated fallback
// certificate whose key changes on every dynamic-config rebuild and could
// never hold a pin. The gateway heartbeat asserts the fingerprint returned
// here (see gateway_heartbeat.go).
package hopcert

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/getkipper/kipper/controller/pkg/hopca"
	"github.com/getkipper/kipper/controller/pkg/hostnames"
	"github.com/getkipper/kipper/controller/pkg/spki"
)

const (
	// SecretName holds the hop keypair in kipper-system, where tenants cannot
	// read it. Served by Traefik through the default TLSStore.
	SecretName = "kipper-hop-cert" //nolint:gosec // G101: a Secret object name, not a credential
	// CASecretName holds the per-cluster certificate authority the hop leaf is
	// signed under. It is what the API server is given as its trust anchor for
	// a kipper.run issuer, so it must outlive every leaf reissue and key
	// rotation — see controller/pkg/hopca.
	CASecretName = "kipper-hop-ca" //nolint:gosec // G101: a Secret object name, not a credential
	// RetainedCAKey holds an authority that is trusted but no longer signs, set
	// while a rollover is in flight. Anything rendering the API server's trust
	// anchor must include it or it will disagree with what kip wrote.
	RetainedCAKey = hopca.RetainedCAKey
	// Namespace is where the Secrets and the TLSStore live.
	Namespace = "kipper-system"

	// RotateAnnotation on the Secret requests an explicit key rotation. The
	// next Ensure stages a candidate keypair; the heartbeat asserts its
	// fingerprint and swaps it live only after the gateway acknowledges it in
	// the accepted set, so a rotation never opens a 502 window.
	RotateAnnotation = "kipper.run/rotate-key"

	// candidateCertKey and candidateKeyKey stage the rotation keypair inside
	// the same Secret. The candidate never reaches Traefik until promoted.
	candidateCertKey = "candidate.crt"
	candidateKeyKey  = "candidate.key"

	// reissueWindow: the certificate is reissued with the SAME key when its
	// remaining validity drops below this. The SPKI fingerprint is unchanged,
	// so a reissue needs no gateway coordination at all. The validity it is
	// reissued for lives with the minting itself, in controller/pkg/hopca,
	// which also caps a leaf at its CA's expiry.
	reissueWindow = 365 * 24 * time.Hour

	tlsStoreName   = "default"
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "kipper"
)

var tlsStoreGVK = schema.GroupVersionKind{Group: "traefik.io", Version: "v1alpha1", Kind: "TLSStore"}

// State reports what Ensure provisioned and what the heartbeat should assert.
type State struct {
	// Fingerprint is the SPKI SHA-256 of the live serving keypair.
	Fingerprint string
	// CandidateFingerprint is the staged rotation keypair's SPKI, empty when
	// no rotation is in flight. When set, the heartbeat asserts it instead of
	// Fingerprint and calls PromoteCandidate once the gateway acknowledges.
	CandidateFingerprint string
	// ForeignStores lists TLSStore objects named "default" outside
	// kipper-system. Traefik cannot serve two default stores coherently, so
	// any entry here means the hop certificate may not be what the cluster
	// serves — the heartbeat must not assert the fingerprint until the
	// conflict is removed.
	ForeignStores []string
}

// Ensure idempotently provisions the hop keypair Secret and the default
// TLSStore, stages a requested key rotation, and reissues a certificate
// nearing expiry (same key, same fingerprint). It never regenerates a live
// key on its own: a changed key needs the two-phase rotation or the gateway
// would 502 the cluster until re-observation.
func Ensure(ctx context.Context, client kubernetes.Interface, cr crclient.Client) (State, error) {
	// The authority comes first and is never regenerated once it exists: the
	// API server holds it as the trust anchor for this cluster's kipper.run
	// issuer, and only the installer, over SSH, can hand it a different one.
	caCertPEM, caKeyPEM, err := ensureCA(ctx, client)
	if err != nil {
		return State{}, err
	}

	secret, err := ensureSecret(ctx, client, caCertPEM, caKeyPEM)
	if err != nil {
		return State{}, err
	}

	cert, err := hopca.ParseCert(secret.Data[corev1.TLSCertKey])
	if err != nil {
		return State{}, fmt.Errorf("parsing hop certificate: %w", err)
	}

	if err := reconcileTLSStore(ctx, cr); err != nil {
		return State{}, err
	}
	foreign, err := foreignDefaultStores(ctx, cr)
	if err != nil {
		return State{}, err
	}

	state := State{Fingerprint: spki.Fingerprint(cert), ForeignStores: foreign}
	if candidate := secret.Data[candidateCertKey]; len(candidate) > 0 {
		candidateCert, err := hopca.ParseCert(candidate)
		if err != nil {
			return State{}, fmt.Errorf("parsing candidate certificate: %w", err)
		}
		state.CandidateFingerprint = spki.Fingerprint(candidateCert)
	}
	return state, nil
}

// SigningKey loads the live hop-certificate private key from the Secret so
// console-api can sign the gateway's registration proof-of-possession
// challenge (B16). It is the key whose public half Traefik serves at the
// cluster IP, so a signature over it proves possession of the destination's
// key to the gateway. It follows the live key across rotation (PromoteCandidate
// swaps this same key), so a beat after a rotation re-proves with the new key.
func SigningKey(ctx context.Context, client kubernetes.Interface) (*ecdsa.PrivateKey, error) {
	secret, err := client.CoreV1().Secrets(Namespace).Get(ctx, SecretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading hop certificate secret: %w", err)
	}
	block, _ := pem.Decode(secret.Data[corev1.TLSPrivateKeyKey])
	if block == nil {
		return nil, fmt.Errorf("hop key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing hop key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("hop key is not ECDSA")
	}
	return key, nil
}

// PromoteCandidate makes the staged rotation keypair the live one in a single
// Secret update, after the gateway acknowledged its fingerprint. Traefik's
// provider hot-swaps the served certificate; the gateway accepts old and new
// throughout (current + pending/previous), so no request window 502s.
// Idempotent: with no candidate staged it does nothing.
func PromoteCandidate(ctx context.Context, client kubernetes.Interface) error {
	secret, err := client.CoreV1().Secrets(Namespace).Get(ctx, SecretName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading hop certificate secret: %w", err)
	}
	if len(secret.Data[candidateCertKey]) == 0 {
		return nil
	}
	secret.Data[corev1.TLSCertKey] = secret.Data[candidateCertKey]
	secret.Data[corev1.TLSPrivateKeyKey] = secret.Data[candidateKeyKey]
	delete(secret.Data, candidateCertKey)
	delete(secret.Data, candidateKeyKey)
	if _, err := client.CoreV1().Secrets(Namespace).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("promoting candidate hop certificate: %w", err)
	}
	return nil
}

// ensureCA returns the cluster's certificate authority, creating it only if no
// one has yet. It is never regenerated: the API server was handed this exact
// anchor for its kipper.run issuer by the installer over SSH, and nothing in
// the cluster can hand it a replacement, so a second CA here would break
// operator authentication until an operator intervened.
//
// The create races another replica by design. A lost race is not an error: the
// winner's CA is read back and used, which is what keeps two console-api pods
// rolling at once from minting two authorities.
func ensureCA(ctx context.Context, client kubernetes.Interface) (caCertPEM, caKeyPEM []byte, err error) {
	secrets := client.CoreV1().Secrets(Namespace)
	existing, err := secrets.Get(ctx, CASecretName, metav1.GetOptions{})
	switch {
	case err == nil:
		return caFromSecret(existing)
	case !k8serrors.IsNotFound(err):
		return nil, nil, fmt.Errorf("reading hop CA secret: %w", err)
	}

	if err := refuseIfTheAuthorityWasDestroyed(ctx, client); err != nil {
		return nil, nil, err
	}

	material, err := hopca.New(hostnames.GatewayDomain)
	if err != nil {
		return nil, nil, fmt.Errorf("minting hop CA: %w", err)
	}
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      CASecretName,
			Namespace: Namespace,
			Labels:    map[string]string{managedByLabel: managedByValue},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       material.CACertPEM,
			corev1.TLSPrivateKeyKey: material.CAKeyPEM,
		},
	}
	created, err := secrets.Create(ctx, desired, metav1.CreateOptions{})
	if k8serrors.IsAlreadyExists(err) {
		won, getErr := secrets.Get(ctx, CASecretName, metav1.GetOptions{})
		if getErr != nil {
			return nil, nil, fmt.Errorf("reading hop CA secret after a lost create race: %w", getErr)
		}
		return caFromSecret(won)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("creating hop CA secret: %w", err)
	}
	return caFromSecret(created)
}

// refuseIfTheAuthorityWasDestroyed stops a missing CA Secret being read as a
// cluster that never had one.
//
// A cluster already serving a certificate that some authority signed has had an
// authority, and it is gone. Minting a replacement and adopting the leaf under
// it would put the cluster on an authority the API server's trust anchor does
// not name, and nothing in the cluster can hand the API server a new anchor:
// that is every operator locked out of the login path with no in-cluster
// repair. Refusing leaves the cluster serving exactly what it serves now, which
// keeps working, and the missing authority is reported by 'kip cluster ca
// status' where someone with SSH can restore it.
//
// A leaf that predates the authority is self-signed, and that one is still
// adopted — it is the migration this whole path exists for.
func refuseIfTheAuthorityWasDestroyed(ctx context.Context, client kubernetes.Interface) error {
	secret, err := client.CoreV1().Secrets(Namespace).Get(ctx, SecretName, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading hop certificate secret: %w", err)
	}
	// From here the Secret exists, which is already evidence this is not a fresh
	// cluster. Everything below decides whether it is evidence of a DESTROYED
	// authority; anything it cannot rule out is treated as one, because minting
	// wrongly locks every operator out and refusing wrongly only stops a
	// reconcile that an operator can unblock.
	leafPEM := secret.Data[corev1.TLSCertKey]
	if len(leafPEM) == 0 {
		return destroyedAuthorityError("the certificate it serves is missing")
	}
	cert, err := hopca.ParseCert(leafPEM)
	if err != nil {
		return destroyedAuthorityError("the certificate it serves cannot be read")
	}
	// Self-SIGNED, checked cryptographically against the certificate's own key.
	// Comparing issuer and subject names proves only that a certificate is
	// self-ISSUED, which any certificate can claim: a leaf signed by another key
	// under a matching subject name would pass that and be read as predating the
	// authority. CheckSignatureFrom is not the primitive either — it refuses a
	// parent that is not a CA, which every legacy leaf here is not.
	if cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature) == nil {
		return nil
	}
	return destroyedAuthorityError(fmt.Sprintf("the certificate it serves was issued by %q", cert.Issuer.CommonName))
}

func destroyedAuthorityError(evidence string) error {
	return fmt.Errorf(
		"%s/%s is missing and %s, so the authority that signed it has been destroyed; "+
			"minting a new one would leave the API server trusting an authority this cluster no longer serves and lock operators out of the login path. "+
			"Restore the authority from a backup, or replace it with the documented procedure",
		Namespace, CASecretName, evidence)
}

// caFromSecret reads the authority out of its Secret, refusing a shape it
// cannot sign with rather than failing later inside a certificate operation.
func caFromSecret(secret *corev1.Secret) (caCertPEM, caKeyPEM []byte, err error) {
	caCertPEM = secret.Data[corev1.TLSCertKey]
	caKeyPEM = secret.Data[corev1.TLSPrivateKeyKey]
	if len(caCertPEM) == 0 || len(caKeyPEM) == 0 {
		return nil, nil, fmt.Errorf("hop CA secret %s/%s is missing its certificate or key", Namespace, CASecretName)
	}
	cert, err := hopca.ParseCert(caCertPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing hop CA certificate: %w", err)
	}
	if !cert.IsCA {
		return nil, nil, fmt.Errorf("hop CA secret %s/%s does not hold a certificate authority", Namespace, CASecretName)
	}
	return caCertPEM, caKeyPEM, nil
}

// ensureSecret returns the hop-cert Secret, creating it on first run and
// applying the adoption, staged-rotation and reissue transitions. Every
// certificate it writes is signed under the cluster's CA.
func ensureSecret(ctx context.Context, client kubernetes.Interface, caCertPEM, caKeyPEM []byte) (*corev1.Secret, error) {
	secrets := client.CoreV1().Secrets(Namespace)
	secret, err := secrets.Get(ctx, SecretName, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		created, createErr := createSecret(ctx, client, caCertPEM, caKeyPEM)
		if k8serrors.IsAlreadyExists(createErr) {
			secret, err = secrets.Get(ctx, SecretName, metav1.GetOptions{})
			if err != nil {
				return nil, fmt.Errorf("reading hop certificate secret: %w", err)
			}
			return secret, nil
		}
		if createErr != nil {
			return nil, createErr
		}
		return created, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading hop certificate secret: %w", err)
	}

	changed := false

	// An explicit rotation request stages a fresh keypair as candidate. The
	// annotation is consumed in the same update, so the request is recorded
	// exactly once; the candidate's presence carries the state from here.
	if _, requested := secret.Annotations[RotateAnnotation]; requested {
		if len(secret.Data[candidateCertKey]) == 0 {
			certPEM, keyPEM, err := hopca.NewLeaf(caCertPEM, caKeyPEM, hostnames.GatewayDomain)
			if err != nil {
				return nil, fmt.Errorf("staging rotation candidate: %w", err)
			}
			secret.Data[candidateCertKey] = certPEM
			secret.Data[candidateKeyKey] = keyPEM
		}
		delete(secret.Annotations, RotateAnnotation)
		changed = true
	}

	cert, err := hopca.ParseCert(secret.Data[corev1.TLSCertKey])
	if err != nil {
		return nil, fmt.Errorf("parsing hop certificate: %w", err)
	}

	// Adopt a leaf this CA did not sign, and reissue one nearing expiry. Both
	// re-sign the SAME key, so the SPKI fingerprint — and with it the gateway's
	// pin — is untouched and no registration has to be re-observed.
	//
	// Adoption covers a cluster whose leaf predates the CA: it is self-signed
	// with no names, which the API server can never verify. Reissue must sign
	// under the CA too, or the first renewal would silently strip the anchor
	// and leave operator OIDC broken with no in-cluster repair path.
	switch {
	case !hopca.SignedBy(secret.Data[corev1.TLSCertKey], caCertPEM):
		resigned, err := hopca.SignLeaf(caCertPEM, caKeyPEM, secret.Data[corev1.TLSPrivateKeyKey], hostnames.GatewayDomain)
		if err != nil {
			return nil, fmt.Errorf("adopting hop certificate under the cluster CA: %w", err)
		}
		secret.Data[corev1.TLSCertKey] = resigned
		changed = true
	case hopca.NeedsReissue(cert, reissueWindow):
		reissued, err := hopca.SignLeaf(caCertPEM, caKeyPEM, secret.Data[corev1.TLSPrivateKeyKey], hostnames.GatewayDomain)
		if err != nil {
			return nil, fmt.Errorf("reissuing hop certificate: %w", err)
		}
		secret.Data[corev1.TLSCertKey] = reissued
		changed = true
	}

	if changed {
		updated, err := secrets.Update(ctx, secret, metav1.UpdateOptions{})
		if err != nil {
			return nil, fmt.Errorf("updating hop certificate secret: %w", err)
		}
		return updated, nil
	}
	return secret, nil
}

func createSecret(ctx context.Context, client kubernetes.Interface, caCertPEM, caKeyPEM []byte) (*corev1.Secret, error) {
	certPEM, keyPEM, err := hopca.NewLeaf(caCertPEM, caKeyPEM, hostnames.GatewayDomain)
	if err != nil {
		return nil, fmt.Errorf("minting hop certificate: %w", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SecretName,
			Namespace: Namespace,
			Labels:    map[string]string{managedByLabel: managedByValue},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       certPEM,
			corev1.TLSPrivateKeyKey: keyPEM,
		},
	}
	created, err := client.CoreV1().Secrets(Namespace).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("creating hop certificate secret: %w", err)
	}
	return created, nil
}

// reconcileTLSStore upserts the Kipper-labelled default TLSStore pointing
// Traefik's default certificate at the hop-cert Secret.
func reconcileTLSStore(ctx context.Context, cr crclient.Client) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(tlsStoreGVK)
	err := cr.Get(ctx, crclient.ObjectKey{Namespace: Namespace, Name: tlsStoreName}, existing)
	if k8serrors.IsNotFound(err) {
		if err := cr.Create(ctx, desiredTLSStore()); err != nil {
			return fmt.Errorf("creating default TLSStore: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading default TLSStore: %w", err)
	}

	secretName, _, _ := unstructured.NestedString(existing.Object, "spec", "defaultCertificate", "secretName")
	if secretName != SecretName || existing.GetLabels()[managedByLabel] != managedByValue {
		desired := desiredTLSStore()
		desired.SetResourceVersion(existing.GetResourceVersion())
		if err := cr.Update(ctx, desired); err != nil {
			return fmt.Errorf("updating default TLSStore: %w", err)
		}
	}
	return nil
}

func desiredTLSStore() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": tlsStoreGVK.GroupVersion().String(),
		"kind":       tlsStoreGVK.Kind,
		"metadata": map[string]any{
			"name":      tlsStoreName,
			"namespace": Namespace,
			"labels":    map[string]any{managedByLabel: managedByValue},
		},
		"spec": map[string]any{
			"defaultCertificate": map[string]any{"secretName": SecretName},
		},
	}}
}

// foreignDefaultStores lists TLSStores named "default" outside kipper-system.
// Traefik supports only one meaningful default store; a competitor would
// displace the hop certificate on some or all replicas and 502 a pinned
// cluster, so its existence must block pin assertion, not just be logged.
func foreignDefaultStores(ctx context.Context, cr crclient.Client) ([]string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: tlsStoreGVK.Group, Version: tlsStoreGVK.Version, Kind: tlsStoreGVK.Kind + "List"})
	if err := cr.List(ctx, list); err != nil {
		return nil, fmt.Errorf("listing TLSStores: %w", err)
	}
	var foreign []string
	for _, item := range list.Items {
		if item.GetName() == tlsStoreName && item.GetNamespace() != Namespace {
			foreign = append(foreign, item.GetNamespace()+"/"+tlsStoreName)
		}
	}
	sort.Strings(foreign)
	return foreign, nil
}
