package controllers

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/share"
	"github.com/getkipper/kipper/controller/pkg/labels"
)

func tlsTestReconciler(t *testing.T, objs ...crclient.Object) *ServiceReconciler {
	t.Helper()
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(certificateGVK, meta.RESTScopeNamespace)
	for _, gvk := range []schema.GroupVersionKind{
		{Group: "apps", Version: "v1", Kind: "StatefulSet"},
		{Version: "v1", Kind: "Secret"}, {Version: "v1", Kind: "ConfigMap"}, {Version: "v1", Kind: "Service"},
		{Group: "kipper.run", Version: "v1alpha1", Kind: "Service"},
		{Group: "kipper.run", Version: "v1alpha1", Kind: "PlatformConfig"},
	} {
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	return &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithRESTMapper(mapper).WithObjects(objs...).
			WithStatusSubresource(&kipperv1.Service{}, &appsv1.StatefulSet{}).Build(),
		Scheme:      testScheme(),
		ShareGrants: share.NewGrantStore(k8sfake.NewClientset()),
	}
}

func tlsService() *kipperv1.Service {
	svc := bareService("postgres")
	svc.UID = "svc-uid"
	svc.Spec.TLS = "on"
	return svc
}

// issuedSecret builds a Secret with cert-manager metadata for service db
// and a fresh self-signed certificate whose common name is cn.
func issuedSecret(t *testing.T, cn string) *corev1.Secret {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-tls", Namespace: "ns",
			Labels:      map[string]string{labels.TLSFor: "db"},
			Annotations: map[string]string{certificateNameAnnotation: "db-tls"}},
		Data: map[string][]byte{
			"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			"tls.key": pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
			"ca.crt":  []byte("ca"),
		},
	}
}

func tlsCondition(t *testing.T, r *ServiceReconciler) *metav1.Condition {
	t.Helper()
	var svc kipperv1.Service
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &svc))
	return meta.FindStatusCondition(svc.Status.Conditions, kipperv1.ConditionTLSReady)
}

func liveCertificate(t *testing.T, r *ServiceReconciler) *unstructured.Unstructured {
	t.Helper()
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db-tls"}, cert))
	return cert
}

func liveStatefulSet(t *testing.T, r *ServiceReconciler) appsv1.StatefulSet {
	t.Helper()
	var sts appsv1.StatefulSet
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &sts))
	return sts
}

func TestANewServiceWaitsForItsFirstCertificate(t *testing.T) {
	svc := tlsService()
	r := tlsTestReconciler(t, svc)

	err := r.reconcileStatefulSet(context.Background(), svc)

	require.ErrorIs(t, err, errAwaitingCertificate, "starting without TLS and again with it would restart Postgres during initdb")
	assert.True(t, kerrors.IsNotFound(r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &appsv1.StatefulSet{})))
	assert.Equal(t, "Issuing", tlsCondition(t, r).Reason)

	cert := liveCertificate(t, r)
	dns, _, _ := unstructured.NestedStringSlice(cert.Object, "spec", "dnsNames")
	assert.Equal(t, []string{"db", "db.ns", "db.ns.svc", "db.ns.svc.cluster.local"}, dns)
	issuer, _, _ := unstructured.NestedString(cert.Object, "spec", "issuerRef", "name")
	assert.Equal(t, "kipper-service-ca", issuer)
	secretLabels, _, _ := unstructured.NestedStringMap(cert.Object, "spec", "secretTemplate", "labels")
	assert.Equal(t, "db", secretLabels[labels.TLSFor], "the issued Secret can be traced back to its service")
	assert.NotContains(t, secretLabels, "velero.io/exclude-from-backup",
		"a restore and a drill need the issued Secret to start the pod, and the backup holds the database itself")
}

func TestAServiceWhoseCertificateDoesNotArriveStartsWithoutTLS(t *testing.T) {
	svc := tlsService()
	r := tlsTestReconciler(t, svc)
	_, err := r.ensureCertificate(context.Background(), svc)
	require.NoError(t, err)
	cert := liveCertificate(t, r)
	cert.SetCreationTimestamp(metav1.NewTime(time.Now().Add(-2 * tlsIssuanceGrace)))
	require.NoError(t, r.Update(context.Background(), cert))

	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	assert.Empty(t, liveStatefulSet(t, r).Spec.Template.Spec.Containers[0].Args, "the pod never waits on a missing Secret")
}

func TestTLSServesTheIssuedCertificate(t *testing.T) {
	svc := tlsService()
	r := tlsTestReconciler(t, svc, issuedSecret(t, "cert-1"))

	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	tpl := liveStatefulSet(t, r).Spec.Template
	args := tpl.Spec.Containers[0].Args
	assert.Contains(t, args, "ssl=on")
	assert.Contains(t, args, "hba_file="+postgresHBADir+"/pg_hba.conf", "pg_hba comes from Kipper whenever TLS is on")
	require.Len(t, tpl.Spec.InitContainers, 2)
	copyStep := tpl.Spec.InitContainers[1]
	assert.Equal(t, tpl.Spec.Containers[0].Image, copyStep.Image, "the service's own image knows its postgres user")
	assert.Contains(t, copyStep.Command[2], "-o postgres -g postgres -m 0600")
	var memory bool
	for _, v := range tpl.Spec.Volumes {
		if v.Name == tlsVolume {
			memory = v.EmptyDir != nil && v.EmptyDir.Medium == corev1.StorageMediumMemory
		}
	}
	assert.True(t, memory, "the private key copy never touches disk")
	assert.NotEmpty(t, tpl.Annotations[tlsCertHashAnnotation])
	assert.Equal(t, tlsVolume, tpl.Annotations[backupVolumesExcludesAnnotation], "file-system backup must not copy the key out of the memory volume")
}

func TestARenewedCertificateRestartsPostgres(t *testing.T) {
	svc := tlsService()
	r := tlsTestReconciler(t, svc, issuedSecret(t, "cert-1"))
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))
	before := liveStatefulSet(t, r).Spec.Template.Annotations[tlsCertHashAnnotation]

	renewed := issuedSecret(t, "cert-2")
	require.NoError(t, r.Update(context.Background(), renewed))
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	assert.NotEqual(t, before, liveStatefulSet(t, r).Spec.Template.Annotations[tlsCertHashAnnotation])
}

func TestTurningTLSOffRemovesWhatItAdded(t *testing.T) {
	svc := tlsService()
	r := tlsTestReconciler(t, svc, issuedSecret(t, "cert-1"))
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	svc.Spec.TLS = "off"
	require.NoError(t, r.Update(context.Background(), svc))
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	tpl := liveStatefulSet(t, r).Spec.Template
	assert.Empty(t, tpl.Spec.Containers[0].Args)
	require.Len(t, tpl.Spec.InitContainers, 1)
	assert.Equal(t, "remove-lost-found", tpl.Spec.InitContainers[0].Name)
	assert.Empty(t, tpl.Spec.Volumes)
	assert.NotContains(t, tpl.Annotations, tlsCertHashAnnotation)

	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	assert.True(t, kerrors.IsNotFound(r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db-tls"}, cert)))
	assert.True(t, kerrors.IsNotFound(r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db-tls"}, &corev1.Secret{})),
		"cert-manager leaves the Secret behind, so the controller deletes the private key")
	assert.True(t, kerrors.IsNotFound(r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db-pg-hba"}, &corev1.ConfigMap{})))
}

func TestAServiceWithoutTLSKeepsItsLiveTemplate(t *testing.T) {
	svc := bareService("postgres")
	r := tlsTestReconciler(t, svc)
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	// An init container that differs from today's default, as on a service
	// created by an older release, must not be rewritten (that would restart it).
	sts := liveStatefulSet(t, r)
	sts.Spec.Template.Spec.InitContainers[0].Image = "busybox:1.35"
	require.NoError(t, r.Update(context.Background(), &sts))

	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	assert.Equal(t, "busybox:1.35", liveStatefulSet(t, r).Spec.Template.Spec.InitContainers[0].Image)
}

func TestTLSWithoutCertManagerKeepsServingPlaintext(t *testing.T) {
	svc := tlsService()
	r := tlsTestReconciler(t, svc)
	r.Client = interceptor.NewClient(r.Client.(crclient.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
			if u, ok := obj.(*unstructured.Unstructured); ok && u.GroupVersionKind() == certificateGVK {
				return &meta.NoKindMatchError{GroupKind: certificateGVK.GroupKind()}
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))
	assert.Empty(t, liveStatefulSet(t, r).Spec.Template.Spec.Containers[0].Args)
}

func TestDeletingAServiceRemovesItsPrivateKey(t *testing.T) {
	svc := tlsService()
	r := tlsTestReconciler(t, svc, issuedSecret(t, "cert-1"))
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	done, _, err := r.cleanUp(context.Background(), svc)

	require.NoError(t, err)
	assert.True(t, done)
	assert.True(t, kerrors.IsNotFound(r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db-tls"}, &corev1.Secret{})),
		"cert-manager leaves the Secret behind, and a namesake created later would serve its key")
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	assert.True(t, kerrors.IsNotFound(r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db-tls"}, cert)))
}

func TestAForeignSecretIsNeverServed(t *testing.T) {
	svc := tlsService()
	foreign := issuedSecret(t, "someone-else")
	foreign.Labels = nil
	foreign.Annotations = nil
	r := tlsTestReconciler(t, svc, foreign)

	err := r.reconcileStatefulSet(context.Background(), svc)

	var taken *nameTakenError
	require.ErrorAs(t, err, &taken)
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	assert.True(t, kerrors.IsNotFound(r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db-tls"}, cert)),
		"no Certificate is pointed at a Secret that belongs to something else")
}

func TestAnUnusableCertificateIsNotServed(t *testing.T) {
	svc := tlsService()
	broken := issuedSecret(t, "cert-1")
	broken.Data["tls.key"] = issuedSecret(t, "other").Data["tls.key"]
	r := tlsTestReconciler(t, svc, broken)

	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	assert.Empty(t, liveStatefulSet(t, r).Spec.Template.Spec.Containers[0].Args, "Postgres would refuse to start with a key that does not match")
	assert.Equal(t, "CertificateInvalid", tlsCondition(t, r).Reason)
}

func TestPgHBAFollowsThePasswordMethodOfTheVersion(t *testing.T) {
	assert.Contains(t, postgresHBA(""), "scram-sha-256")
	assert.Contains(t, postgresHBA("16-alpine"), "scram-sha-256")
	assert.Contains(t, postgresHBA("13"), "host all all all md5", "Postgres 13 stores md5 passwords, which scram-sha-256 would reject")
	assert.Contains(t, postgresHBA("12.4-bookworm"), "md5")
}

func TestAnUnchangedCertificateIsNotWrittenAgain(t *testing.T) {
	svc := tlsService()
	r := tlsTestReconciler(t, svc, issuedSecret(t, "cert-1"))
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))
	before := liveCertificate(t, r).GetResourceVersion()

	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	assert.Equal(t, before, liveCertificate(t, r).GetResourceVersion())
}

func TestOtherServiceTypesNeverLookForTLSObjects(t *testing.T) {
	svc := bareService("redis")
	r := tlsTestReconciler(t, svc)
	r.Client = interceptor.NewClient(r.Client.(crclient.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
			if key.Name == "db-tls" || key.Name == "db-pg-hba" {
				t.Errorf("a %s service looked up %s", svc.Spec.Type, key.Name)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))
}

func TestAForbiddenCertificateKeepsTheServiceFromFinishingItsDeletion(t *testing.T) {
	svc := tlsService()
	r := tlsTestReconciler(t, svc, issuedSecret(t, "cert-1"))
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))
	r.Client = interceptor.NewClient(r.Client.(crclient.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
			if u, ok := obj.(*unstructured.Unstructured); ok && u.GroupVersionKind() == certificateGVK {
				return kerrors.NewForbidden(certificateGVK.GroupVersion().WithResource("certificates").GroupResource(), key.Name, nil)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	done, reason, err := r.cleanUp(context.Background(), svc)

	require.Error(t, err, "a Certificate kip cannot see may still exist and write the key again")
	assert.False(t, done)
	assert.Equal(t, "TLSNotRemoved", reason)
}

func TestTLSIsReportedAsServedOnlyOnceThePodRunsTheCertificate(t *testing.T) {
	svc := tlsService()
	r := tlsTestReconciler(t, svc, issuedSecret(t, "cert-1"))
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))
	assert.Equal(t, "RollingOut", tlsCondition(t, r).Reason, "an issued certificate is not yet a served one")

	sts := liveStatefulSet(t, r)
	sts.Status = appsv1.StatefulSetStatus{ObservedGeneration: sts.Generation, CurrentRevision: "r2", UpdateRevision: "r2", UpdatedReplicas: 1, ReadyReplicas: 1}
	require.NoError(t, r.Status().Update(context.Background(), &sts))
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	assert.Equal(t, metav1.ConditionTrue, tlsCondition(t, r).Status)
}

func TestAServiceScaledToZeroIsNotReportedAsServing(t *testing.T) {
	sts := &appsv1.StatefulSet{Spec: appsv1.StatefulSetSpec{Replicas: ptrInt32(0)},
		Status: appsv1.StatefulSetStatus{CurrentRevision: "r1", UpdateRevision: "r1"}}

	assert.False(t, statefulSetRolledOut(sts), "no pod serves anything")
}

func ptrInt32(v int32) *int32 { return &v }

func TestRestoredTLSObjectsAreTakenBack(t *testing.T) {
	svc := tlsService()
	// A restore brings these back without owner references.
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	cert.SetName("db-tls")
	cert.SetNamespace("ns")
	cert.SetLabels(map[string]string{kipperLabel: kipperValue, "app": "db", labels.TLSFor: "db"})
	hba := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "db-pg-hba", Namespace: "ns",
		Labels: map[string]string{kipperLabel: kipperValue, "app": "db", labels.TLSFor: "db"}}}
	r := tlsTestReconciler(t, svc, cert, hba, issuedSecret(t, "cert-1"))

	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc), "a restored service must reconcile by reclaiming its own objects")

	assert.Equal(t, svc.UID, metav1.GetControllerOf(liveCertificate(t, r)).UID)
	var cm corev1.ConfigMap
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db-pg-hba"}, &cm))
	assert.Equal(t, svc.UID, metav1.GetControllerOf(&cm).UID)
	assert.Contains(t, liveStatefulSet(t, r).Spec.Template.Spec.Containers[0].Args, "ssl=on")
}

func TestAnUnlabelledCertificateOfTheNameIsStillRefused(t *testing.T) {
	svc := tlsService()
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	cert.SetName("db-tls")
	cert.SetNamespace("ns")
	r := tlsTestReconciler(t, svc, cert)

	var taken *nameTakenError
	require.ErrorAs(t, r.reconcileStatefulSet(context.Background(), svc), &taken)
}

func TestAnOlderPgHBAConfigMapGainsTheRestoreLabel(t *testing.T) {
	svc := tlsService()
	r := tlsTestReconciler(t, svc, issuedSecret(t, "cert-1"))
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))
	var cm corev1.ConfigMap
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db-pg-hba"}, &cm))
	delete(cm.Labels, labels.TLSFor)
	require.NoError(t, r.Update(context.Background(), &cm))

	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db-pg-hba"}, &cm))
	assert.Equal(t, "db", cm.Labels[labels.TLSFor], "the label allows the service to reclaim its ConfigMap after a restore")
}

func TestATLSObjectWithAnyOwnerIsNotAdopted(t *testing.T) {
	svc := tlsService()
	notController := false
	hba := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "db-pg-hba", Namespace: "ns",
		Labels:          map[string]string{labels.TLSFor: "db"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "other", UID: "other-uid", Controller: &notController}}}}
	r := tlsTestReconciler(t, svc, hba, issuedSecret(t, "cert-1"))

	var taken *nameTakenError
	require.ErrorAs(t, r.reconcileStatefulSet(context.Background(), svc), &taken, "a non-controller owner is still an owner")
}

func TestTLSKeepsOtherBackupExclusions(t *testing.T) {
	svc := tlsService()
	r := tlsTestReconciler(t, svc, issuedSecret(t, "cert-1"))
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))
	sts := liveStatefulSet(t, r)
	sts.Spec.Template.Annotations[backupVolumesExcludesAnnotation] = "scratch,tls"
	require.NoError(t, r.Update(context.Background(), &sts))

	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))
	assert.Equal(t, "scratch,tls", liveStatefulSet(t, r).Spec.Template.Annotations[backupVolumesExcludesAnnotation])

	svc.Spec.TLS = "off"
	require.NoError(t, r.Update(context.Background(), svc))
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))
	assert.Equal(t, "scratch", liveStatefulSet(t, r).Spec.Template.Annotations[backupVolumesExcludesAnnotation], "the operator's own exclusion survives")
}

func TestRestoredTLSObjectsAreRemovedWithoutBeingAdoptedFirst(t *testing.T) {
	for _, step := range []string{"off", "delete"} {
		t.Run(step, func(t *testing.T) {
			svc := tlsService()
			svc.Spec.TLS = "off"
			cert := &unstructured.Unstructured{}
			cert.SetGroupVersionKind(certificateGVK)
			cert.SetName("db-tls")
			cert.SetNamespace("ns")
			cert.SetLabels(map[string]string{labels.TLSFor: "db"})
			hba := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "db-pg-hba", Namespace: "ns", Labels: map[string]string{labels.TLSFor: "db"}}}
			r := tlsTestReconciler(t, svc, cert, hba, issuedSecret(t, "cert-1"))

			if step == "off" {
				require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))
			} else {
				_, _, err := r.cleanUp(context.Background(), svc)
				require.NoError(t, err)
			}

			gone := &unstructured.Unstructured{}
			gone.SetGroupVersionKind(certificateGVK)
			assert.True(t, kerrors.IsNotFound(r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db-tls"}, gone)), "a Certificate left behind would issue the key again")
			assert.True(t, kerrors.IsNotFound(r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db-pg-hba"}, &corev1.ConfigMap{})))
		})
	}
}
