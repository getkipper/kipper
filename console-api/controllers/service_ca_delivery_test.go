package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

const dbHost = "db.ns.svc.cluster.local"

func TestOnlyACertificateAVerifyingClientWouldAcceptIsPublished(t *testing.T) {
	now := time.Now()
	require.NoError(t, verifyIssuedCertificate(issuedSecret(t, "ok"), dbHost, now))

	wrongCA := issuedSecret(t, "ok")
	wrongCA.Data["ca.crt"] = newTestCA(t).pem
	assert.Error(t, verifyIssuedCertificate(wrongCA, dbHost, now), "a CA that did not sign the certificate")

	wrongHost := issuedSecretFor(t, "ok", []string{"other.ns.svc.cluster.local"}, now.Add(time.Hour))
	assert.Error(t, verifyIssuedCertificate(wrongHost, dbHost, now), "a certificate for another name")

	expired := issuedSecretFor(t, "ok", []string{dbHost}, now.Add(-time.Minute))
	assert.Error(t, verifyIssuedCertificate(expired, dbHost, now), "an expired certificate")

	noCA := issuedSecret(t, "ok")
	delete(noCA.Data, "ca.crt")
	assert.Error(t, verifyIssuedCertificate(noCA, dbHost, now))
}

func liveServiceCA(t *testing.T, r *ServiceReconciler) (*corev1.ConfigMap, error) {
	t.Helper()
	var cm corev1.ConfigMap
	err := r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db-ca"}, &cm)
	return &cm, err
}

func TestTheServiceCAIsPublishedWithoutTheKey(t *testing.T) {
	svc := tlsService()
	secret := issuedSecret(t, "cert-1")
	r := tlsTestReconciler(t, svc, secret)

	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	cm, err := liveServiceCA(t, r)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"ca.crt": string(secret.Data["ca.crt"])}, cm.Data, "only the CA, never tls.key")
	assert.Equal(t, svc.UID, metav1.GetControllerOf(cm).UID)
}

func TestAMismatchedCertificateIsNeitherServedNorPublished(t *testing.T) {
	svc := tlsService()
	secret := issuedSecret(t, "cert-1")
	secret.Data["ca.crt"] = newTestCA(t).pem
	r := tlsTestReconciler(t, svc, secret)

	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	_, err := liveServiceCA(t, r)
	assert.True(t, kerrors.IsNotFound(err), "apps told to trust this CA would fail verify-full")
	assert.Empty(t, liveStatefulSet(t, r).Spec.Template.Spec.Containers[0].Args)
	assert.Equal(t, "CertificateInvalid", tlsCondition(t, r).Reason)
}

func TestBoundAppsLearnWhereTheCAIsOnceItIsPublished(t *testing.T) {
	svc := tlsService()
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: "ns"},
		Data:       map[string][]byte{"HOST": []byte(dbHost), "PORT": []byte("5432"), "USERNAME": []byte("kipper"), "PASSWORD": []byte("pw"), "NAME": []byte("app")},
	}
	r := tlsTestReconciler(t, svc, issuedSecret(t, "cert-1"), creds)

	require.NoError(t, r.repairCredentials(context.Background(), creds, svc))
	assert.NotContains(t, secretFromCluster(t, r, "db-credentials"), sslRootCertKey, "nothing to point at before the CA exists")

	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db-credentials"}, creds))
	require.NoError(t, r.repairCredentials(context.Background(), creds, svc))
	assert.Equal(t, "/etc/kipper/service-ca/db/ca.crt", secretFromCluster(t, r, "db-credentials")[sslRootCertKey])

	svc.Spec.TLS = "off"
	require.NoError(t, r.Update(context.Background(), svc))
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))
	_, err := liveServiceCA(t, r)
	assert.True(t, kerrors.IsNotFound(err), "turning TLS off removes the CA")
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db-credentials"}, creds))
	require.NoError(t, r.repairCredentials(context.Background(), creds, svc))
	assert.NotContains(t, secretFromCluster(t, r, "db-credentials"), sslRootCertKey)
}

func TestAStaleCAPathIsCorrected(t *testing.T) {
	assert.True(t, convergedKey("postgres", sslRootCertKey), "a restored Secret must not keep pointing at another path")
}

func TestEachBoundServiceCAIsMountedOnceWhereSSLROOTCERTPoints(t *testing.T) {
	binding := func(service, value string) envSource {
		return envSource{origin: originBinding, service: service, held: true,
			values: []corev1.EnvVar{{Name: "HOST", Value: service}, {Name: sslRootCertKey, Value: value}}}
	}
	sources := []envSource{
		binding("orders", serviceCAPath("orders")),
		binding("orders", serviceCAPath("orders")),
		binding("billing", serviceCAPath("billing")),
		binding("cache", "/somewhere/else"),
		{origin: originBinding, service: "plain", held: true, values: []corev1.EnvVar{{Name: "HOST", Value: "plain"}}},
	}

	volumes, mounts, err := serviceCAVolumes(sources, nil, nil)
	require.NoError(t, err)

	require.Len(t, volumes, 2, "one per service, and only where the value is Kipper's path")
	assert.Equal(t, "billing-ca", volumes[0].ConfigMap.Name)
	assert.Equal(t, "orders-ca", volumes[1].ConfigMap.Name)
	assert.Equal(t, "/etc/kipper/service-ca/billing", mounts[0].MountPath)
	assert.True(t, mounts[0].ReadOnly)
	assert.Equal(t, volumes[0].Name, mounts[0].Name)
}

func TestABoundAppAndFunctionMountTheCA(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "ns", UID: "uid-shop"},
		Spec:       kipperv1.AppSpec{Image: "registry.example.com/shop:1.2.3", Port: 8080},
	}
	sources := []envSource{{origin: originBinding, service: "db", held: true, prefix: "DB_",
		values: []corev1.EnvVar{{Name: sslRootCertKey, Value: serviceCAPath("db")}}}}
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(app).Build()
	r := &AppReconciler{Client: c, Scheme: testScheme()}

	dep, err := r.buildDeployment(context.Background(), app, sources, "gen-1", "")
	require.NoError(t, err)

	spec := dep.Spec.Template.Spec
	require.Len(t, spec.Volumes, 1)
	assert.Equal(t, "db-ca", spec.Volumes[0].ConfigMap.Name)
	assert.Contains(t, spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: spec.Volumes[0].Name, MountPath: "/etc/kipper/service-ca/db", ReadOnly: true})

	fn := &kipperv1.Function{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "ns"}, Spec: kipperv1.FunctionSpec{Image: "registry.example.com/fn:1"}}
	batch, err := batchPodSpec(fn, "cron", nil, sources, "gen-1")
	require.NoError(t, err)
	require.Len(t, batch.Volumes, 1, "batch runs see the same CA as the function's Deployment")
	assert.Equal(t, "db-ca", batch.Volumes[0].ConfigMap.Name)
	assert.Len(t, batch.Containers[0].VolumeMounts, 1)
}

func TestCAVolumesAreOptionalAndAvoidTakenNames(t *testing.T) {
	sources := []envSource{{origin: originBinding, service: "db", held: true,
		values: []corev1.EnvVar{{Name: sslRootCertKey, Value: serviceCAPath("db")}}}}

	volumes, mounts, err := serviceCAVolumes(sources, map[string]bool{"kipper-service-ca-0": true}, nil)
	require.NoError(t, err)

	require.Len(t, volumes, 1)
	assert.Equal(t, "kipper-service-ca-1", volumes[0].Name, "a shared volume may already use the first name")
	assert.Equal(t, volumes[0].Name, mounts[0].Name)
	require.NotNil(t, volumes[0].ConfigMap.Optional)
	assert.True(t, *volumes[0].ConfigMap.Optional, "a Job made before TLS went off must still start without the CA")
}

func TestARenewalLeavesThePublishedCAAlone(t *testing.T) {
	svc := tlsService()
	r := tlsTestReconciler(t, svc, issuedSecret(t, "cert-1"))
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))
	before, err := liveServiceCA(t, r)
	require.NoError(t, err)

	require.NoError(t, r.Update(context.Background(), issuedSecret(t, "cert-2")))
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	after, err := liveServiceCA(t, r)
	require.NoError(t, err)
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "apps keep trusting the same CA across renewals")
}

func TestADeclaredMountOverAServiceCAIsRefused(t *testing.T) {
	sources := []envSource{{origin: originBinding, service: "db", held: true,
		values: []corev1.EnvVar{{Name: sslRootCertKey, Value: serviceCAPath("db")}}}}
	for _, path := range []string{"/etc/kipper/service-ca/db", "/etc/kipper/service-ca/db/", "/etc/kipper", "/etc/kipper/service-ca/db/sub",
		"/etc//kipper/service-ca/db/ca.crt", "/etc/./kipper/service-ca/db", "/", "/etc/kipper/x/../service-ca/db"} {
		_, _, err := serviceCAVolumes(sources, nil, []corev1.VolumeMount{{Name: "certs", MountPath: path}})
		assert.Error(t, err, "SSLROOTCERT would name a file Kipper does not control (%s)", path)
	}
	for _, path := range []string{"/etc/kipper/service-ca/dbx", "/etc//kipper/service-ca/dbx", "/data"} {
		_, _, err := serviceCAVolumes(sources, nil, []corev1.VolumeMount{{Name: "data", MountPath: path}})
		assert.NoError(t, err, "%s does not overlap", path)
	}
	var err error

	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "ns", UID: "uid-shop"},
		Spec: kipperv1.AppSpec{Image: "registry.example.com/shop:1.2.3", Port: 8080,
			Volumes: []kipperv1.AppVolumeMount{{Name: "certs", MountPath: "/etc/kipper/service-ca/db"}}},
	}
	r := &AppReconciler{Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(app).Build(), Scheme: testScheme()}
	_, err = r.buildDeployment(context.Background(), app, sources, "gen-1", "")
	assert.Error(t, err, "the app reports the overlap instead of a template Kubernetes rejects")

	fn := &kipperv1.Function{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "ns"},
		Spec: kipperv1.FunctionSpec{Image: "registry.example.com/fn:1", Volumes: []kipperv1.AppVolumeMount{{Name: "certs", MountPath: "/etc/kipper/service-ca/db"}}}}
	_, err = batchPodSpec(fn, "cron", nil, sources, "gen-1")
	assert.Error(t, err)
}

func TestARestoredCAKeepsBoundAppsVerifying(t *testing.T) {
	svc := tlsService()
	secret := issuedSecret(t, "cert-1")
	restored := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "db-ca", Namespace: "ns",
		Labels: map[string]string{labels.TLSFor: "db"}}, Data: map[string]string{"ca.crt": string(secret.Data["ca.crt"])}}
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: "ns"},
		Data:       map[string][]byte{"HOST": []byte(dbHost), "PORT": []byte("5432"), "USERNAME": []byte("kipper"), "PASSWORD": []byte("pw"), "NAME": []byte("app")},
	}
	r := tlsTestReconciler(t, svc, secret, restored, creds)

	require.NoError(t, r.repairCredentials(context.Background(), creds, svc))
	assert.Equal(t, serviceCAPath("db"), secretFromCluster(t, r, "db-credentials")[sslRootCertKey],
		"Credentials are reconciled before the StatefulSet, so they must include the restored CA's path before its ConfigMap is adopted.")

	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))
	cm, err := liveServiceCA(t, r)
	require.NoError(t, err)
	assert.Equal(t, svc.UID, metav1.GetControllerOf(cm).UID)
}

func TestAReplacedCARollsBoundWorkloads(t *testing.T) {
	svc := tlsService()
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: "ns", OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "kipper.run/v1alpha1", Kind: "Service", Name: "db", UID: svc.UID, Controller: ptrBool(true)}}},
		Data: map[string][]byte{"HOST": []byte(dbHost), sslRootCertKey: []byte(serviceCAPath("db"))},
	}
	ca := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "db-ca", Namespace: "ns", Labels: map[string]string{labels.TLSFor: "db"}},
		Data: map[string]string{"ca.crt": "old root"}}
	app := &kipperv1.App{ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "ns"},
		Spec: kipperv1.AppSpec{ServiceBindings: []kipperv1.ServiceBinding{{Name: "db"}}}}
	r := tlsTestReconciler(t, svc, creds, ca, app)
	ctx := context.Background()

	_, _, before, err := reconcileBindingSecrets(ctx, r.Client, r.Scheme, app, secretname.KindApp, app.Spec.ServiceBindings)
	require.NoError(t, err)
	ca.Data["ca.crt"] = "new root"
	require.NoError(t, r.Update(ctx, ca))
	_, _, after, err := reconcileBindingSecrets(ctx, r.Client, r.Scheme, app, secretname.KindApp, app.Spec.ServiceBindings)
	require.NoError(t, err)

	assert.NotEqual(t, before, after, "An app that loaded the old root into memory must restart to load the new one.")

	reqs := workloadsBoundToServiceCA(ca, []kipperv1.App{*app}, func(a *kipperv1.App) (string, string, []kipperv1.ServiceBinding) {
		return a.Name, a.Namespace, a.Spec.ServiceBindings
	})
	assert.Len(t, reqs, 1, "A CA change requests reconciliation of the bound app.")
	assert.Empty(t, workloadsBoundToServiceCA(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "ns",
		Labels: map[string]string{labels.TLSFor: "db"}}}, []kipperv1.App{*app}, func(a *kipperv1.App) (string, string, []kipperv1.ServiceBinding) {
		return a.Name, a.Namespace, a.Spec.ServiceBindings
	}))
}

func ptrBool(v bool) *bool { return &v }

func TestARequiredServiceKeepsDeliveringItsCA(t *testing.T) {
	svc := tlsService()
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: "ns"},
		Data:       map[string][]byte{"HOST": []byte(dbHost), "PORT": []byte("5432"), "USERNAME": []byte("kipper"), "PASSWORD": []byte("pw"), "NAME": []byte("app")},
	}
	r := tlsTestReconciler(t, svc, issuedSecret(t, "cert-1"), creds)
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))
	require.NoError(t, r.repairCredentials(context.Background(), creds, svc))
	require.Equal(t, "/etc/kipper/service-ca/db/ca.crt", secretFromCluster(t, r, "db-credentials")[sslRootCertKey])

	svc.Spec.TLS = "required"
	require.NoError(t, r.Update(context.Background(), svc))
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db-credentials"}, creds))
	require.NoError(t, r.repairCredentials(context.Background(), creds, svc))

	assert.Equal(t, "/etc/kipper/service-ca/db/ca.crt", secretFromCluster(t, r, "db-credentials")[sslRootCertKey],
		"apps verifying the server must keep their CA when TLS becomes required")
	_, err := liveServiceCA(t, r)
	assert.NoError(t, err)
}
