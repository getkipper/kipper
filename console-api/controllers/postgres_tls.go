package controllers

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/servicecatalog"
)

var certificateGVK = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}

const (
	serviceCAIssuer = "kipper-service-ca"

	// certificateNameAnnotation identifies the Certificate that cert-manager
	// used to issue a Secret.
	certificateNameAnnotation = "cert-manager.io/certificate-name"
	// tlsCertHashAnnotation on the pod template restarts Postgres when its
	// certificate is renewed, so it serves the new one.
	tlsCertHashAnnotation = "kipper.run/tls-cert-hash"
	// backupVolumesExcludesAnnotation names pod volumes Velero's file-system
	// backup skips.
	backupVolumesExcludesAnnotation = "backup.velero.io/backup-volumes-excludes"
	// veleroRestoreNameLabel identifies the restore that created an object.
	veleroRestoreNameLabel = "velero.io/restore-name"

	postgresTLSDir       = "/etc/kipper/tls"
	postgresTLSSourceDir = "/etc/kipper/tls-source"
	postgresHBADir       = "/etc/kipper/pg-hba"

	tlsVolume       = "tls"
	tlsSourceVolume = "tls-source"
	hbaVolume       = "pg-hba"
	tlsInitName     = "tls-files"
)

// postgresHBA trusts local socket connections used by Kipper's exec-based
// tools and requires passwords for all network connections, including loopback.
// With required set, only TLS network connections are accepted. It uses md5
// before Postgres 14 and scram-sha-256 for later versions or an empty version,
// matching the catalog default.
func postgresHBA(version string, required bool) string {
	method := "scram-sha-256"
	major := version
	if i := strings.IndexFunc(version, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		major = version[:i]
	}
	if n, err := strconv.Atoi(major); err == nil && n < 14 {
		method = "md5"
	}
	if required {
		return "local all all trust\nhostssl all all all " + method + "\n"
	}
	return "local all all trust\nhost all all all " + method + "\n"
}

// tlsEnabled reports whether a Postgres service should serve TLS.
func tlsEnabled(svc *kipperv1.Service) bool {
	return svc.Spec.TLS == "on" || tlsRequired(svc)
}

// tlsRequired reports whether the service is configured to require TLS.
func tlsRequired(svc *kipperv1.Service) bool {
	return svc.Spec.TLS == "required"
}

// tlsIssuanceGrace lets a new service in mode on wait for its first certificate
// before starting with plaintext, to avoid a second start during initialisation.
const (
	tlsIssuanceGrace = 60 * time.Second
	tlsIssuanceRetry = 5 * time.Second
)

// errAwaitingCertificate holds back a new service's StatefulSet while its
// first certificate is being issued.
var errAwaitingCertificate = stderrors.New("waiting for the first TLS certificate")

// TLSSecretName names the Secret into which cert-manager writes a Postgres
// service's certificate, key and CA.
func TLSSecretName(service string) string { return service + "-tls" }

func hbaConfigMapName(service string) string { return service + "-pg-hba" }

// tlsSecretOwnedBy checks the service label and Certificate annotation to
// avoid using another service's TLS Secret.
func tlsSecretOwnedBy(secret *corev1.Secret, service string) bool {
	return secret.Labels[labels.TLSFor] == service && secret.Annotations[certificateNameAnnotation] == TLSSecretName(service)
}

// ServiceCA returns the ca.crt bytes from a non-nil Postgres TLS Secret for
// clients to use as trust roots. It returns an error if the service label or
// Certificate annotation differs from the expected value, or ca.crt contains
// no parseable PEM certificates. The returned slice shares the Secret's data.
// Callers must verify the server's certificate and DNS name when connecting.
func ServiceCA(secret *corev1.Secret, service string) ([]byte, error) {
	if !tlsSecretOwnedBy(secret, service) {
		return nil, fmt.Errorf("the TLS Secret %s was not issued for service %s", secret.Name, service)
	}
	ca := secret.Data[serviceCAFile]
	if !x509.NewCertPool().AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("the TLS Secret %s holds no usable CA certificate", secret.Name)
	}
	return ca, nil
}

// reconcilePostgresTLS ensures the Certificate and pg_hba ConfigMap for
// Postgres with TLS enabled and records issuance failures in TLSReady. It verifies
// the certificate and key pair and publishes the service CA before returning a
// short certificate hash, with a mode suffix when TLS is required. An empty hash
// with no error means TLS is off, the certificate fails verification, the key
// pair is missing or unusable, or the Certificate API is missing or forbidden.
// Other API errors are returned. wait delays StatefulSet creation during the
// first certificate's issuance grace period. Required mode waits indefinitely
// for a usable certificate and also defers changes to existing StatefulSets,
// leaving their pods running.
func (r *ServiceReconciler) reconcilePostgresTLS(ctx context.Context, svc *kipperv1.Service) (certHash string, wait bool, err error) {
	if svc.Spec.Type != "postgres" {
		return "", false, nil
	}
	if !tlsEnabled(svc) {
		r.clearTLSCondition(ctx, svc)
		return "", false, nil
	}
	// Required mode waits for a usable certificate to preserve TLS, keeping
	// any existing pod running.
	unserved := func(reason, why string, grace bool) (string, bool, error) {
		msg := "Postgres serves without TLS " + why
		if tlsRequired(svc) {
			msg = "Postgres requires TLS, so startup and changes are paused " + why
		}
		r.setTLSCondition(ctx, svc, metav1.ConditionFalse, reason, msg)
		return "", grace || tlsRequired(svc), nil
	}
	secret, err := r.issuedSecret(ctx, svc)
	if err != nil {
		return "", false, err
	}
	cert, err := r.ensureCertificate(ctx, svc)
	if err != nil {
		if meta.IsNoMatchError(err) || errors.IsForbidden(err) {
			return unserved("CertManagerUnavailable", "until cert-manager and the service CA are available: "+err.Error(), false)
		}
		return "", false, fmt.Errorf("reconciling the TLS certificate: %w", err)
	}
	if err := r.ensureHBAConfigMap(ctx, svc); err != nil {
		return "", false, fmt.Errorf("reconciling pg_hba: %w", err)
	}
	if secret == nil {
		why := "until cert-manager issues its certificate"
		if ready := certificateReadyMessage(cert); ready != "" {
			why += " (" + ready + ")"
		}
		why += ". If issuance remains pending, run a full kip upgrade to install the service CA"
		created := cert.GetCreationTimestamp()
		return unserved("Issuing", why, created.IsZero() || time.Since(created.Time) < tlsIssuanceGrace)
	}
	crt := secret.Data[corev1.TLSCertKey]
	host := fmt.Sprintf("%s.%s.svc.cluster.local", svc.Name, svc.Namespace)
	// Request key replacement before certificate verification so a restore into
	// another namespace can use the same issuance for the new key and DNS names.
	if err := r.replaceRestoredKey(ctx, cert, secret); err != nil {
		return "", false, fmt.Errorf("replacing the restored TLS key: %w", err)
	}
	if problem := certificateProblem(secret, host); problem != "" {
		return unserved("CertificateInvalid", "because the certificate in Secret "+secret.Name+" fails verification: "+problem, false)
	}
	if !usableKeyPair(crt, secret.Data[corev1.TLSPrivateKeyKey]) {
		return unserved("CertificateInvalid", "because the issued certificate and key in Secret "+secret.Name+" do not form a usable pair", false)
	}
	if err := r.ensureServiceCA(ctx, svc, secret); err != nil {
		return "", false, fmt.Errorf("publishing the service CA: %w", err)
	}
	sum := sha256.Sum256(crt)
	// Including the mode in the rollout marker triggers a restart to load the
	// new pg_hba rules and lets the rollout report confirm the mode change.
	if tlsRequired(svc) {
		return hex.EncodeToString(sum[:8]) + "-required", false, nil
	}
	return hex.EncodeToString(sum[:8]), false, nil
}

// replaceRestoredKey requests a new key pair for a restored Secret because the
// backup retains a copy of its private key. The restored key remains available
// until its replacement arrives. Once issuance is requested or already in
// progress, it removes the restore label to prevent repeated requests.
// Errors updating the Certificate status or reading or updating the Secret are returned.
func (r *ServiceReconciler) replaceRestoredKey(ctx context.Context, cert *unstructured.Unstructured, secret *corev1.Secret) error {
	if secret.Labels[veleroRestoreNameLabel] == "" {
		return nil
	}
	conditions, _, _ := unstructured.NestedSlice(cert.Object, "status", "conditions")
	kept := make([]interface{}, 0, len(conditions)+1)
	issuing := false
	for _, c := range conditions {
		m, ok := c.(map[string]interface{})
		if ok && m["type"] == "Issuing" {
			issuing = m["status"] == "True"
			continue
		}
		kept = append(kept, c)
	}
	if !issuing {
		kept = append(kept, map[string]interface{}{
			"type": "Issuing", "status": "True", "reason": "ManuallyTriggered",
			"message":            "Kipper requested a new private key to replace the one restored from a backup",
			"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
		})
		if err := unstructured.SetNestedSlice(cert.Object, kept, "status", "conditions"); err != nil {
			return err
		}
		if err := r.Status().Update(ctx, cert); err != nil {
			return err
		}
	}
	// Re-read the Secret on conflicts to preserve any key cert-manager issued
	// while the restore label was being removed, without requesting issuance again.
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var live corev1.Secret
		if err := r.Get(ctx, client.ObjectKeyFromObject(secret), &live); err != nil {
			return err
		}
		if live.Labels[veleroRestoreNameLabel] == "" {
			return nil
		}
		delete(live.Labels, veleroRestoreNameLabel)
		return r.Update(ctx, &live)
	})
}

// reportTLSRollout records whether Postgres serves the certificate whose
// hash is certHash. TLS is ready once the live StatefulSet carries that hash
// and its pods are ready on that template. live is nil before the
// StatefulSet exists.
func (r *ServiceReconciler) reportTLSRollout(ctx context.Context, svc *kipperv1.Service, certHash string, live *appsv1.StatefulSet) {
	if certHash == "" {
		return
	}
	if live != nil && live.Spec.Template.Annotations[tlsCertHashAnnotation] == certHash && statefulSetRolledOut(live) {
		msg := "Postgres serves TLS with a certificate from the cluster's service CA"
		if tlsRequired(svc) {
			msg += " and refuses network connections without TLS"
		}
		r.setTLSCondition(ctx, svc, metav1.ConditionTrue, "Serving", msg)
		return
	}
	r.setTLSCondition(ctx, svc, metav1.ConditionFalse, "RollingOut",
		"The certificate has been issued. Once its pod restarts, Postgres will serve it using the service's configured TLS mode")
}

// statefulSetRolledOut reports whether sts has replicas and every one of them
// is ready and runs its current template.
func statefulSetRolledOut(sts *appsv1.StatefulSet) bool {
	want := int32(1)
	if sts.Spec.Replicas != nil {
		want = *sts.Spec.Replicas
	}
	st := sts.Status
	return st.ObservedGeneration >= sts.Generation && st.UpdateRevision != "" &&
		st.CurrentRevision == st.UpdateRevision && st.UpdatedReplicas == want && st.ReadyReplicas == want && want > 0
}

// takeBackTLSObject accepts obj if svc is its controller owner, or adopts it
// if it has no owner references and carries the service's tls-for label.
// This allows adoption after a restore drops owner references. On success,
// true means the caller must persist the added owner reference. Other objects
// cause a nameTakenError, and errors setting the owner reference are returned.
func (r *ServiceReconciler) takeBackTLSObject(kind string, obj client.Object, svc *kipperv1.Service) (bool, error) {
	owner := metav1.GetControllerOf(obj)
	switch {
	case owner != nil && owner.UID == svc.UID:
		return false, nil
	case len(obj.GetOwnerReferences()) == 0 && obj.GetLabels()[labels.TLSFor] == svc.Name:
		return true, controllerutil.SetControllerReference(svc, obj, r.Scheme)
	}
	return false, &nameTakenError{Kind: kind, Name: obj.GetName(), Namespace: svc.Namespace}
}

// ownsTLSObject reports whether svc is obj's controller owner or obj has no
// owner references and carries the service's tls-for label. This allows
// deletion of restored objects before adoption.
func ownsTLSObject(obj client.Object, svc *kipperv1.Service) bool {
	if owner := metav1.GetControllerOf(obj); owner != nil {
		return owner.UID == svc.UID
	}
	return len(obj.GetOwnerReferences()) == 0 && obj.GetLabels()[labels.TLSFor] == svc.Name
}

// withListEntry appends entry to a comma-separated list if no trimmed item
// matches it. Existing list formatting is preserved unless the list is blank.
func withListEntry(list, entry string) string {
	for _, item := range strings.Split(list, ",") {
		if strings.TrimSpace(item) == entry {
			return list
		}
	}
	if strings.TrimSpace(list) == "" {
		return entry
	}
	return list + "," + entry
}

// withoutListEntry removes all matches for entry from a comma-separated list,
// trims the remaining items and discards empty items.
func withoutListEntry(list, entry string) string {
	var kept []string
	for _, item := range strings.Split(list, ",") {
		if item = strings.TrimSpace(item); item != "" && item != entry {
			kept = append(kept, item)
		}
	}
	return strings.Join(kept, ",")
}

// usableKeyPair reports whether crt and key parse and belong together, as
// Postgres requires before it starts.
func usableKeyPair(crt, key []byte) bool {
	_, err := tls.X509KeyPair(crt, key)
	return err == nil
}

// issuedSecret returns the service's certificate Secret once cert-manager has
// written both certificate and key, or nil while either is missing. It rejects
// Secrets whose service label or Certificate annotation differs from the
// expected name, to avoid serving another service's key.
func (r *ServiceReconciler) issuedSecret(ctx context.Context, svc *kipperv1.Service) (*corev1.Secret, error) {
	var secret corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: TLSSecretName(svc.Name)}, &secret)
	if errors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !tlsSecretOwnedBy(&secret, svc.Name) {
		return nil, &nameTakenError{Kind: "Secret", Name: secret.Name, Namespace: svc.Namespace}
	}
	if len(secret.Data[corev1.TLSCertKey]) == 0 || len(secret.Data[corev1.TLSPrivateKeyKey]) == 0 {
		return nil, nil
	}
	return &secret, nil
}

// certificateReadyMessage returns the message of a Certificate's Ready
// condition, or an empty string if that message is absent.
func certificateReadyMessage(cert *unstructured.Unstructured) string {
	conditions, _, _ := unstructured.NestedSlice(cert.Object, "status", "conditions")
	for _, c := range conditions {
		m, ok := c.(map[string]interface{})
		if ok && m["type"] == "Ready" {
			msg, _ := m["message"].(string)
			return msg
		}
	}
	return ""
}

func (r *ServiceReconciler) setTLSCondition(ctx context.Context, svc *kipperv1.Service, status metav1.ConditionStatus, reason, message string) {
	if !meta.SetStatusCondition(&svc.Status.Conditions, metav1.Condition{
		Type: kipperv1.ConditionTLSReady, Status: status, Reason: reason, Message: message, ObservedGeneration: svc.Generation,
	}) {
		return
	}
	if err := r.Status().Update(ctx, svc); err != nil {
		log.FromContext(ctx).Error(err, "recording the TLS state of the service")
	}
}

func (r *ServiceReconciler) clearTLSCondition(ctx context.Context, svc *kipperv1.Service) {
	if !meta.RemoveStatusCondition(&svc.Status.Conditions, kipperv1.ConditionTLSReady) {
		return
	}
	if err := r.Status().Update(ctx, svc); err != nil {
		log.FromContext(ctx).Error(err, "clearing the TLS state of the service")
	}
}

// ensureCertificate creates or corrects the service's Certificate and returns
// the live object. It writes when labels or managed spec fields differ or an
// owner reference is added, preserving unmanaged top-level spec fields.
// Existing Certificates must have this service as their controller owner or
// have no owner references and carry the service's tls-for label for adoption.
func (r *ServiceReconciler) ensureCertificate(ctx context.Context, svc *kipperv1.Service) (*unstructured.Unstructured, error) {
	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(certificateGVK)
	desired.SetName(TLSSecretName(svc.Name))
	desired.SetNamespace(svc.Namespace)
	desired.SetLabels(map[string]string{kipperLabel: kipperValue, "app": svc.Name, labels.TLSFor: svc.Name})
	spec := map[string]interface{}{
		"secretName": TLSSecretName(svc.Name),
		"secretTemplate": map[string]interface{}{
			"labels": map[string]interface{}{
				kipperLabel:   kipperValue,
				labels.TLSFor: svc.Name,
			},
		},
		"duration":    "8760h0m0s",
		"renewBefore": "720h0m0s",
		"privateKey":  map[string]interface{}{"algorithm": "ECDSA", "size": int64(256), "rotationPolicy": "Always"},
		"dnsNames": []interface{}{
			svc.Name,
			svc.Name + "." + svc.Namespace,
			svc.Name + "." + svc.Namespace + ".svc",
			svc.Name + "." + svc.Namespace + ".svc.cluster.local",
		},
		"issuerRef": map[string]interface{}{"name": serviceCAIssuer, "kind": "ClusterIssuer", "group": "cert-manager.io"},
	}
	if err := unstructured.SetNestedField(desired.Object, spec, "spec"); err != nil {
		return nil, err
	}
	if err := controllerutil.SetControllerReference(svc, desired, r.Scheme); err != nil {
		return nil, err
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(certificateGVK)
	err := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: desired.GetName()}, existing)
	if errors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return nil, err
		}
		return desired, nil
	}
	if err != nil {
		return nil, err
	}
	adopted, err := r.takeBackTLSObject("Certificate", existing, svc)
	if err != nil {
		return nil, err
	}
	liveSpec, _, _ := unstructured.NestedMap(existing.Object, "spec")
	if liveSpec == nil {
		liveSpec = map[string]interface{}{}
	}
	changed := adopted || !stringMapEqual(existing.GetLabels(), desired.GetLabels())
	for k, v := range spec {
		if !equality.Semantic.DeepEqual(liveSpec[k], v) {
			liveSpec[k] = v
			changed = true
		}
	}
	if !changed {
		return existing, nil
	}
	existing.Object["spec"] = liveSpec
	existing.SetLabels(desired.GetLabels())
	return existing, r.Update(ctx, existing)
}

func (r *ServiceReconciler) ensureHBAConfigMap(ctx context.Context, svc *kipperv1.Service) error {
	data := map[string]string{"pg_hba.conf": postgresHBA(svc.Spec.Version, tlsRequired(svc))}
	var existing corev1.ConfigMap
	err := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: hbaConfigMapName(svc.Name)}, &existing)
	if errors.IsNotFound(err) {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: hbaConfigMapName(svc.Name), Namespace: svc.Namespace,
				Labels: map[string]string{kipperLabel: kipperValue, "app": svc.Name, labels.TLSFor: svc.Name},
			},
			Data: data,
		}
		if err := controllerutil.SetControllerReference(svc, cm, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, cm)
	}
	if err != nil {
		return err
	}
	adopted, err := r.takeBackTLSObject("ConfigMap", &existing, svc)
	if err != nil {
		return err
	}
	labeled := existing.Labels[labels.TLSFor] == svc.Name
	if !adopted && labeled && stringMapEqual(existing.Data, data) {
		return nil
	}
	if !labeled {
		if existing.Labels == nil {
			existing.Labels = map[string]string{}
		}
		existing.Labels[labels.TLSFor] = svc.Name
	}
	existing.Data = data
	return r.Update(ctx, &existing)
}

// removeStalePostgresTLS deletes what TLS added to a Postgres service whose
// TLS is off. Call it after removing those references from the StatefulSet
// template. The running pod may still be rolling out that change.
func (r *ServiceReconciler) removeStalePostgresTLS(ctx context.Context, svc *kipperv1.Service) error {
	if svc.Spec.Type != "postgres" || tlsEnabled(svc) {
		return nil
	}
	return r.removePostgresTLS(ctx, svc, false)
}

// removePostgresTLS deletes what TLS added, including the certificate
// Secret, which cert-manager leaves behind. Certificates and ConfigMaps must
// satisfy ownsTLSObject, and Secrets must carry the service's TLS label.
// Unless always is set, it looks for the Certificate only when the pg_hba or
// CA ConfigMap or the Secret still exists, which the cache answers without a
// call to the API server.
func (r *ServiceReconciler) removePostgresTLS(ctx context.Context, svc *kipperv1.Service, always bool) error {
	var cm corev1.ConfigMap
	cmFound, err := r.found(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: hbaConfigMapName(svc.Name)}, &cm)
	if err != nil {
		return err
	}
	var secret corev1.Secret
	secretFound, err := r.found(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: TLSSecretName(svc.Name)}, &secret)
	if err != nil {
		return err
	}
	var caMap corev1.ConfigMap
	caFound, err := r.found(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: serviceCAConfigMapName(svc.Name)}, &caMap)
	if err != nil {
		return err
	}
	if !always && !cmFound && !secretFound && !caFound {
		return nil
	}

	// Delete the Certificate first to prevent cert-manager from recreating the Secret.
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	err = r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: TLSSecretName(svc.Name)}, cert)
	switch {
	case err == nil:
		if ownsTLSObject(cert, svc) {
			if err := r.Delete(ctx, cert); err != nil && !errors.IsNotFound(err) {
				return err
			}
		}
	case errors.IsNotFound(err), meta.IsNoMatchError(err):
	default:
		// Retry even on Forbidden because the Certificate may still exist
		// and recreate the Secret.
		return err
	}

	if secretFound && secret.Labels[labels.TLSFor] == svc.Name {
		if err := r.Delete(ctx, &secret); err != nil && !errors.IsNotFound(err) {
			return err
		}
	}
	if caFound {
		if ownsTLSObject(&caMap, svc) {
			if err := r.Delete(ctx, &caMap); err != nil && !errors.IsNotFound(err) {
				return err
			}
		}
	}
	if cmFound {
		if ownsTLSObject(&cm, svc) {
			if err := r.Delete(ctx, &cm); err != nil && !errors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

// found reads key into obj and reports whether it exists.
func (r *ServiceReconciler) found(ctx context.Context, key types.NamespacedName, obj client.Object) (bool, error) {
	err := r.Get(ctx, key, obj)
	if errors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// applyPostgresTLS makes the pod template serve TLS with the certificate
// whose hash is certHash. An init container from the service's own image
// copies the certificate and key into a memory-backed volume, setting file
// ownership to that image's postgres user and key permissions to 0600,
// as required for a key owned by the Postgres user.
func applyPostgresTLS(tpl *corev1.PodTemplateSpec, svcName, image, certHash string) {
	spec := &tpl.Spec
	c := &spec.Containers[0]
	c.Args = append(c.Args,
		"-c", "ssl=on",
		"-c", "ssl_cert_file="+postgresTLSDir+"/tls.crt",
		"-c", "ssl_key_file="+postgresTLSDir+"/tls.key",
		"-c", "hba_file="+postgresHBADir+"/pg_hba.conf",
	)
	c.VolumeMounts = append(c.VolumeMounts,
		corev1.VolumeMount{Name: tlsVolume, MountPath: postgresTLSDir, ReadOnly: true},
		corev1.VolumeMount{Name: hbaVolume, MountPath: postgresHBADir, ReadOnly: true},
	)
	spec.Volumes = append(spec.Volumes, postgresTLSVolumes(svcName)...)

	initCPURequest, initMemoryRequest := servicecatalog.InitContainerRequests()
	spec.InitContainers = append(spec.InitContainers, corev1.Container{
		Name:  tlsInitName,
		Image: image,
		Command: []string{"sh", "-c",
			"install -o postgres -g postgres -m 0600 " + postgresTLSSourceDir + "/tls.key " + postgresTLSDir + "/tls.key && " +
				"install -o postgres -g postgres -m 0644 " + postgresTLSSourceDir + "/tls.crt " + postgresTLSDir + "/tls.crt"},
		VolumeMounts: []corev1.VolumeMount{
			{Name: tlsSourceVolume, MountPath: postgresTLSSourceDir, ReadOnly: true},
			{Name: tlsVolume, MountPath: postgresTLSDir},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(initCPURequest),
				corev1.ResourceMemory: resource.MustParse(initMemoryRequest),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
		},
	})

	if tpl.Annotations == nil {
		tpl.Annotations = map[string]string{}
	}
	tpl.Annotations[tlsCertHashAnnotation] = certHash
	// The Secret is already backed up, so exclude the duplicate key in the
	// memory volume from file-system backups.
	tpl.Annotations[backupVolumesExcludesAnnotation] = withListEntry(tpl.Annotations[backupVolumesExcludesAnnotation], tlsVolume)
}

func postgresTLSVolumes(svcName string) []corev1.Volume {
	return []corev1.Volume{
		{Name: tlsSourceVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: TLSSecretName(svcName)}}},
		{Name: tlsVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}},
		{Name: hbaVolume, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: hbaConfigMapName(svcName)},
		}}},
	}
}

// syncPostgresTLSTemplate carries the TLS parts of desired onto a live
// template, or strips them when desired has none. It leaves the template
// unchanged when neither template has TLS, preserving init containers and
// volumes from older releases without triggering a restart.
func syncPostgresTLSTemplate(live, desired *corev1.PodTemplateSpec) {
	hash, on := desired.Annotations[tlsCertHashAnnotation]
	_, wasOn := live.Annotations[tlsCertHashAnnotation]
	if !on && !wasOn {
		return
	}
	if on {
		live.Spec.InitContainers = desired.Spec.InitContainers
		for _, v := range desired.Spec.Volumes {
			live.Spec.Volumes = withVolume(live.Spec.Volumes, v)
		}
		if live.Annotations == nil {
			live.Annotations = map[string]string{}
		}
		live.Annotations[tlsCertHashAnnotation] = hash
		live.Annotations[backupVolumesExcludesAnnotation] = withListEntry(live.Annotations[backupVolumesExcludesAnnotation], tlsVolume)
		return
	}
	var inits []corev1.Container
	for _, c := range live.Spec.InitContainers {
		if c.Name != tlsInitName {
			inits = append(inits, c)
		}
	}
	live.Spec.InitContainers = inits
	var volumes []corev1.Volume
	for _, v := range live.Spec.Volumes {
		if v.Name != tlsVolume && v.Name != tlsSourceVolume && v.Name != hbaVolume {
			volumes = append(volumes, v)
		}
	}
	live.Spec.Volumes = volumes
	delete(live.Annotations, tlsCertHashAnnotation)
	if rest := withoutListEntry(live.Annotations[backupVolumesExcludesAnnotation], tlsVolume); rest == "" {
		delete(live.Annotations, backupVolumesExcludesAnnotation)
	} else {
		live.Annotations[backupVolumesExcludesAnnotation] = rest
	}
}

// serviceForTLSSecret maps a certificate Secret to the service it serves.
func serviceForTLSSecret(_ context.Context, obj client.Object) []reconcile.Request {
	name := obj.GetLabels()[labels.TLSFor]
	if name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}}}
}
