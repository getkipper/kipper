package controllers

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/serviceui"
	"github.com/getkipper/kipper/console-api/share"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

// secretFromCluster reads the named secret back from the fake client
// so assertions can inspect the post-reconcile state. The test only
// uses string-valued fields, so the helper returns []byte → string
// for readability.
func secretFromCluster(t *testing.T, r *ServiceReconciler, name string) map[string]string {
	t.Helper()
	var s corev1.Secret
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: name}, &s))
	out := map[string]string{}
	for k, v := range s.Data {
		out[k] = string(v)
	}
	return out
}

func namedService(name, svcType string) *kipperv1.Service {
	return &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec:       kipperv1.ServiceSpec{Type: svcType},
	}
}

func bareService(svcType string) *kipperv1.Service {
	return &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"},
		Spec:       kipperv1.ServiceSpec{Type: svcType},
	}
}

func statefulSetContainer(t *testing.T, r *ServiceReconciler) corev1.Container {
	t.Helper()
	var sts appsv1.StatefulSet
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &sts))
	require.NotEmpty(t, sts.Spec.Template.Spec.Containers)
	return sts.Spec.Template.Spec.Containers[0]
}

// A service that pins no resources still gets a full set of requests and
// limits from its type's profile, so every service pod is quota-eligible and
// bounded. The StatefulSet also carries the profile label so the resource
// controller's floor and nil-defaults stay in lockstep. Memory values track
// profileResources(): database=1Gi, standard=128Mi, jvm=2Gi.
func TestReconcileStatefulSet_DefaultsResourcesPerProfile(t *testing.T) {
	cases := []struct {
		svcType      string
		wantProfile  string
		wantMemLimit string
	}{
		{"postgres", "database", "1Gi"},
		{"mysql", "database", "1Gi"},
		{"mongodb", "database", "1Gi"},
		{"redis", "standard", "128Mi"},
		{"minio", "standard", "128Mi"},
		{"rabbitmq", "standard", "128Mi"},
		{"mailhog", "standard", "128Mi"},
		{"opensearch", "jvm", "2Gi"},
	}
	for _, tc := range cases {
		t.Run(tc.svcType, func(t *testing.T) {
			svc := bareService(tc.svcType)
			r := &ServiceReconciler{
				Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc).Build(),
				Scheme: testScheme(),
			}
			require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

			var sts appsv1.StatefulSet
			require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &sts))
			require.NotEmpty(t, sts.Spec.Template.Spec.Containers)
			res := sts.Spec.Template.Spec.Containers[0].Resources

			assert.False(t, res.Requests.Cpu().IsZero(), "cpu request must be set")
			assert.False(t, res.Requests.Memory().IsZero(), "memory request must be set")
			assert.False(t, res.Limits.Cpu().IsZero(), "cpu limit must be set")
			assert.Equal(t, tc.wantMemLimit, res.Limits.Memory().String(), "memory limit must match the type profile")
			// The label must agree with the applied profile, or the resource
			// controller can re-default the pod through a different preset.
			assert.Equal(t, tc.wantProfile, sts.Labels["kipper.run/resource-profile"],
				"resource-profile label must match the catalog profile")
		})
	}
}

// An explicit CR value overrides the profile default for that field only.
func TestReconcileStatefulSet_ExplicitResourceOverridesProfile(t *testing.T) {
	svc := bareService("postgres")
	svc.Spec.Resources.MemoryLimit = "1Gi"
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc).Build(),
		Scheme: testScheme(),
	}
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))
	res := statefulSetContainer(t, r).Resources
	assert.Equal(t, "1Gi", res.Limits.Memory().String(), "explicit memory limit wins")
	assert.False(t, res.Requests.Cpu().IsZero(), "unspecified fields still fall back to the profile")
}

// A one-sided explicit override must mirror to the other side rather than
// combine with the profile default, so a low memory limit can't end up below
// the profile's higher request (which Kubernetes would reject).
func TestReconcileStatefulSet_PartialOverrideMirrorsToStayValid(t *testing.T) {
	svc := bareService("postgres") // database profile: 1Gi request, 1Gi limit
	svc.Spec.Resources.MemoryLimit = "512Mi"
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc).Build(),
		Scheme: testScheme(),
	}
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))
	res := statefulSetContainer(t, r).Resources
	memReq := res.Requests.Memory()
	memLim := res.Limits.Memory()
	assert.Equal(t, "512Mi", memLim.String(), "explicit memory limit is honoured")
	assert.Equal(t, "512Mi", memReq.String(), "unset memory request mirrors the limit, not the 1Gi profile default")
	assert.False(t, memReq.Cmp(*memLim) > 0, "request must not exceed limit")
	// CPU, untouched by the CR, still comes from the profile.
	assert.Equal(t, "500m", res.Requests.Cpu().String())
	assert.Equal(t, "500m", res.Limits.Cpu().String())
}

// The resource controller's OOM bumps live on the StatefulSet, not the CR. A
// reconcile of a resource-less service must keep those live values rather than
// resetting them to the profile default.
func TestReconcileStatefulSet_PreservesResourceControllerBumps(t *testing.T) {
	svc := bareService("postgres")
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc).Build(),
		Scheme: testScheme(),
	}
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	// Simulate an OOM bump: the controller raised the memory limit on the
	// running workload well above the profile default.
	var sts appsv1.StatefulSet
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &sts))
	sts.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("4Gi")
	require.NoError(t, r.Update(context.Background(), &sts))

	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))
	res := statefulSetContainer(t, r).Resources
	assert.Equal(t, "4Gi", res.Limits.Memory().String(), "reconcile must not stomp the controller's bump")
}

// Pinning one resource type must not reset the other. A memory bump on the
// live workload has to survive even when the CR pins CPU, so preservation
// works per resource type rather than only when the CR pins nothing at all.
func TestReconcileStatefulSet_PartialOverrideKeepsUnpinnedBump(t *testing.T) {
	svc := bareService("postgres")
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc).Build(),
		Scheme: testScheme(),
	}
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	// A VPA raised the memory limit on the running workload.
	var sts appsv1.StatefulSet
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &sts))
	sts.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("4Gi")
	require.NoError(t, r.Update(context.Background(), &sts))

	// The user later pins CPU only. Memory stays unpinned, so its live value
	// must survive; CPU comes from the CR.
	svc.Spec.Resources.CPULimit = "2"
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	res := statefulSetContainer(t, r).Resources
	assert.Equal(t, "4Gi", res.Limits.Memory().String(), "unpinned memory bump must survive a CPU-only override")
	assert.Equal(t, "2", res.Limits.Cpu().String(), "pinned CPU limit must come from the CR")
	assert.Equal(t, "2", res.Requests.Cpu().String(), "one-sided CPU limit mirrors to the request")
}

func TestRepairCredentials_RabbitMQAddsVHOSTAndDropsNAME(t *testing.T) {
	// Pre-existing rabbitmq secret in the old shape: NAME=app from
	// when every authed service inherited the database template.
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rabbit-credentials", Namespace: "ns"},
		Data: map[string][]byte{
			"HOST":     []byte("rabbit.ns.svc.cluster.local"),
			"PORT":     []byte("5672"),
			"USERNAME": []byte("kipper"),
			"PASSWORD": []byte("secret"),
			"NAME":     []byte("app"),
		},
	}
	r := &ServiceReconciler{Client: crfake.NewClientBuilder().WithObjects(existing).Build()}
	require.NoError(t, r.repairCredentials(context.Background(), existing, namedService("rabbit", "rabbitmq")))

	got := secretFromCluster(t, r, "rabbit-credentials")
	assert.Equal(t, "/", got["VHOST"], "VHOST should be added with the default vhost value")
	_, hasName := got["NAME"]
	assert.False(t, hasName, "NAME should be pruned from rabbitmq secrets — AMQP_NAME is meaningless")
	assert.Equal(t, "rabbit.ns.svc.cluster.local", got["HOST"], "HOST is the service's own address")
	assert.Equal(t, "kipper", got["USERNAME"], "USERNAME must not be touched")
}

func TestRepairCredentials_MinioDropsNAME(t *testing.T) {
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "obj-credentials", Namespace: "ns"},
		Data: map[string][]byte{
			"HOST":     []byte("obj.ns.svc.cluster.local"),
			"PORT":     []byte("9000"),
			"USERNAME": []byte("kipper"),
			"PASSWORD": []byte("secret"),
			"NAME":     []byte("app"),
		},
	}
	r := &ServiceReconciler{Client: crfake.NewClientBuilder().WithObjects(existing).Build()}
	require.NoError(t, r.repairCredentials(context.Background(), existing, namedService("obj", "minio")))

	got := secretFromCluster(t, r, "obj-credentials")
	_, hasName := got["NAME"]
	assert.False(t, hasName, "NAME should be pruned from minio secrets — buckets are per-binding")
	_, hasVHOST := got["VHOST"]
	assert.False(t, hasVHOST, "minio has no VHOST concept")
}

// TestReconcileCredentialsSecret_MinioS3Shape locks the reconciler's
// fresh-create output to the S3-native shape, matching what the kip CLI
// path produces (see kip/internal/service TestAddMinIOCreatesResources)
// so a MinIO made either way binds identically.
func TestReconcileCredentialsSecret_MinioS3Shape(t *testing.T) {
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "storage", Namespace: "ns"},
		Spec:       kipperv1.ServiceSpec{Type: "minio"},
	}
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc).Build(),
		Scheme: testScheme(),
	}
	require.NoError(t, r.reconcileCredentialsSecret(context.Background(), svc))

	got := secretFromCluster(t, r, "storage-credentials")
	assert.Equal(t, "http://storage.ns.svc.cluster.local:9000", got["ENDPOINT"])
	assert.Equal(t, "kipper", got["ACCESS_KEY"])
	assert.NotEmpty(t, got["SECRET_KEY"])
	for _, k := range []string{"HOST", "PORT", "USERNAME", "PASSWORD", "NAME"} {
		_, present := got[k]
		assert.False(t, present, k+" must not appear on an S3 credentials secret")
	}

	// The MinIO server must take its root password from SECRET_KEY, not
	// the generic PASSWORD key that no longer exists on the secret.
	var rootPass *corev1.EnvVar
	env := serviceCatalog("minio").envVars("storage")
	for i := range env {
		if env[i].Name == "MINIO_ROOT_PASSWORD" {
			rootPass = &env[i]
		}
	}
	require.NotNil(t, rootPass)
	require.NotNil(t, rootPass.ValueFrom)
	assert.Equal(t, "SECRET_KEY", rootPass.ValueFrom.SecretKeyRef.Key)
	assert.Equal(t, "storage-credentials", rootPass.ValueFrom.SecretKeyRef.Name)
}

func TestRepairCredentials_PostgresPreservesName(t *testing.T) {
	// A postgres secret that the user has customised — NAME is the
	// default database name and must not be touched.
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: "ns"},
		Data: map[string][]byte{
			"HOST":     []byte("db.ns.svc.cluster.local"),
			"PORT":     []byte("5432"),
			"USERNAME": []byte("kipper"),
			"PASSWORD": []byte("secret"),
			"NAME":     []byte("custom_app"),
		},
	}
	r := &ServiceReconciler{Client: crfake.NewClientBuilder().WithObjects(existing).Build()}
	require.NoError(t, r.repairCredentials(context.Background(), existing, namedService("db", "postgres")))

	got := secretFromCluster(t, r, "db-credentials")
	assert.Equal(t, "custom_app", got["NAME"], "existing custom database name must be preserved")
}

func TestRepairCredentials_HandlesNilDataMap(t *testing.T) {
	// A Secret created out of band (or fully drained) can have Data
	// == nil. The reconciler must not panic on the assignment.
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rabbit-credentials", Namespace: "ns"},
		Data:       nil,
	}
	r := &ServiceReconciler{Client: crfake.NewClientBuilder().WithObjects(existing).Build()}
	require.NoError(t, r.repairCredentials(context.Background(), existing, namedService("rabbit", "rabbitmq")))

	got := secretFromCluster(t, r, "rabbit-credentials")
	assert.Equal(t, "/", got["VHOST"], "VHOST default must be added even when Data starts nil")
}

// uiCatalogFixtureSvc returns a Service whose catalog entry the test
// asserts has a UI block (mailhog at the time of writing). Using the
// real catalog rather than mocking keeps the test honest about
// catalog-driven behaviour — if we remove mailhog's UI block, the
// test fails loudly, which is the right signal.
func uiCatalogFixtureSvc() *kipperv1.Service {
	return &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "mailhog", Namespace: "blog-test", UID: "test-uid"},
		Spec:       kipperv1.ServiceSpec{Type: "mailhog"},
	}
}

func TestReconcileUIIngress_CreatesIngressAndMiddleware(t *testing.T) {
	svc := uiCatalogFixtureSvc()
	r := &ServiceReconciler{
		Client:              crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc).Build(),
		Scheme:              testScheme(),
		Domain:              "example.com",
		ConsoleAuthCheckURL: "https://console.example.com/api/v1/auth/check",
	}
	require.NoError(t, r.reconcileUIIngress(context.Background(), svc))

	var ing networkingv1.Ingress
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: svc.Namespace, Name: "mailhog-ui"}, &ing))
	assert.Equal(t, "mailhog-blog-test.example.com", ing.Spec.Rules[0].Host, "hostname must be <svc>-<ns>.<cluster-domain>")
	assert.Equal(t, "letsencrypt-prod", ing.Annotations["cert-manager.io/cluster-issuer"], "TLS must be requested via the prod ClusterIssuer")
	assert.Contains(t, ing.Annotations["traefik.ingress.kubernetes.io/router.middlewares"], "mailhog-forward-auth@kubernetescrd",
		"router must chain through the forwardAuth middleware so unauthenticated requests bounce to login")
	assert.Equal(t, "ui", ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Port.Name)

	mw := unstructuredMiddleware()
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: svc.Namespace, Name: "mailhog-forward-auth"}, &mw))
	fa := mw.Object["spec"].(map[string]interface{})["forwardAuth"].(map[string]interface{})
	assert.Equal(t, "https://console.example.com/api/v1/auth/check", fa["address"], "forwardAuth must point at the console-api /auth/check endpoint")
	assert.Equal(t, true, fa["trustForwardHeader"], "trustForwardHeader required so /auth/check can rebuild the original URL for the ?next= redirect")
	assert.Equal(t, []interface{}{"__Host-kipper-ui-mailhog-blog-test"}, fa["addAuthCookiesToResponse"],
		"Traefik must copy the re-minted per-host session cookie back to the browser")

	// The middleware chain order is load-bearing: rate-limit throttles
	// the public gate before any auth work, and cookie-strip runs after
	// forwardAuth so the backend never receives the kipper auth or share
	// cookies — the capability must not reach a possibly untrusted
	// container.
	chain := ing.Annotations["traefik.ingress.kubernetes.io/router.middlewares"]
	assert.Equal(t,
		"traefik-rate-limit@kubernetescrd,blog-test-mailhog-forward-auth@kubernetescrd,blog-test-mailhog-cookie-strip@kubernetescrd",
		chain, "chain must run rate-limit → forwardAuth → cookie-strip, in that order")

	strip := unstructuredMiddleware()
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: svc.Namespace, Name: "mailhog-cookie-strip"}, &strip))
	headers := strip.Object["spec"].(map[string]interface{})["headers"].(map[string]interface{})
	custom := headers["customRequestHeaders"].(map[string]interface{})
	assert.Equal(t, "", custom["Cookie"], "the backend must receive a blanked Cookie header")
}

func TestReconcileUIIngress_DeletesWhenUIRemoved(t *testing.T) {
	// First reconcile creates the Ingress + Middleware; second
	// reconcile with no Domain configured must tear them down so
	// disabling UI access is reversible without orphans.
	svc := uiCatalogFixtureSvc()
	r := &ServiceReconciler{
		Client:              crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc).Build(),
		Scheme:              testScheme(),
		Domain:              "example.com",
		ConsoleAuthCheckURL: "https://console.example.com/api/v1/auth/check",
	}
	require.NoError(t, r.reconcileUIIngress(context.Background(), svc))

	// Drop Domain so the next reconcile takes the delete path.
	r.Domain = ""
	r.ConsoleAuthCheckURL = ""
	require.NoError(t, r.reconcileUIIngress(context.Background(), svc))

	var ing networkingv1.Ingress
	err := r.Get(context.Background(), types.NamespacedName{Namespace: svc.Namespace, Name: "mailhog-ui"}, &ing)
	assert.True(t, kerrors.IsNotFound(err), "Ingress must be deleted on the no-UI path")

	mw := unstructuredMiddleware()
	err = r.Get(context.Background(), types.NamespacedName{Namespace: svc.Namespace, Name: "mailhog-forward-auth"}, &mw)
	assert.True(t, kerrors.IsNotFound(err), "Middleware must be deleted on the no-UI path")
}

func TestReconcileUIIngress_SkipsWithoutConsoleAuthURL(t *testing.T) {
	// Misconfiguration guard: a cluster without CONSOLE_DOMAIN
	// shouldn't end up with an Ingress that points at a broken
	// forwardAuth — the UI would still be reachable, but auth
	// would silently let everyone through.
	svc := uiCatalogFixtureSvc()
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc).Build(),
		Scheme: testScheme(),
		Domain: "example.com",
		// ConsoleAuthCheckURL intentionally empty.
	}
	require.NoError(t, r.reconcileUIIngress(context.Background(), svc))

	var ing networkingv1.Ingress
	err := r.Get(context.Background(), types.NamespacedName{Namespace: svc.Namespace, Name: "mailhog-ui"}, &ing)
	assert.True(t, kerrors.IsNotFound(err), "no Ingress without a working forwardAuth target")
}

func TestReconcileUINetworkPolicy_HonoursIngressControllerConfigMap(t *testing.T) {
	// Operator overrides the defaults: different label, different
	// value, and locks the policy to a specific namespace. The
	// resulting NetworkPolicy must reflect every override exactly,
	// so a chart upgrade that renames labels (or a swap to Nginx
	// ingress) is a ConfigMap edit rather than a Kipper release.
	svc := uiCatalogFixtureSvc()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "ingress-controller", Namespace: "kipper-system"},
		Data: map[string]string{
			"labelKey":   "app.kubernetes.io/component",
			"labelValue": "ingress-controller",
			"namespace":  "nginx-ingress",
		},
	}
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc, cm).Build(),
		Scheme: testScheme(),
	}
	require.NoError(t, r.reconcileUINetworkPolicy(context.Background(), svc))

	var np networkingv1.NetworkPolicy
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: svc.Namespace, Name: "mailhog-ui-traffic"}, &np))
	uiRule := np.Spec.Ingress[0]
	require.Len(t, uiRule.From, 1)
	assert.Equal(t, "ingress-controller", uiRule.From[0].PodSelector.MatchLabels["app.kubernetes.io/component"],
		"podSelector must use the custom label key + value from the ConfigMap")
	assert.Equal(t, "nginx-ingress", uiRule.From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"],
		"namespaceSelector must restrict to the override namespace when one is configured")
}

func TestReconcileUINetworkPolicy_PartialConfigMapKeepsDefaults(t *testing.T) {
	// Only `namespace` is set; the label key/value should fall
	// through to defaults. Lets operators tighten just one knob
	// without re-specifying the rest.
	svc := uiCatalogFixtureSvc()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "ingress-controller", Namespace: "kipper-system"},
		Data:       map[string]string{"namespace": "traefik"},
	}
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc, cm).Build(),
		Scheme: testScheme(),
	}
	require.NoError(t, r.reconcileUINetworkPolicy(context.Background(), svc))

	var np networkingv1.NetworkPolicy
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: svc.Namespace, Name: "mailhog-ui-traffic"}, &np))
	uiRule := np.Spec.Ingress[0]
	assert.Equal(t, "traefik", uiRule.From[0].PodSelector.MatchLabels["app.kubernetes.io/name"],
		"label defaults must apply when only namespace is overridden")
	assert.Equal(t, "traefik", uiRule.From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"])
}

func TestReconcileUINetworkPolicy_RestrictsToTraefikNamespace(t *testing.T) {
	svc := uiCatalogFixtureSvc()
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc).Build(),
		Scheme: testScheme(),
	}
	require.NoError(t, r.reconcileUINetworkPolicy(context.Background(), svc))

	var np networkingv1.NetworkPolicy
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: svc.Namespace, Name: "mailhog-ui-traffic"}, &np))
	require.Len(t, np.Spec.Ingress, 2,
		"two rules: UI port locked to Traefik, main service port open to all pods so SMTP / binding traffic isn't accidentally blocked")

	// Rule 0: UI port restricted to Traefik pods (any namespace).
	uiRule := np.Spec.Ingress[0]
	require.Len(t, uiRule.From, 1)
	require.NotNil(t, uiRule.From[0].PodSelector,
		"UI rule must select Traefik by pod label so the policy works across k3s layouts (kube-system vs traefik namespace)")
	assert.Equal(t, "traefik", uiRule.From[0].PodSelector.MatchLabels["app.kubernetes.io/name"])
	assert.NotNil(t, uiRule.From[0].NamespaceSelector,
		"empty NamespaceSelector required alongside PodSelector to match Traefik in any namespace")
	require.Len(t, uiRule.Ports, 1)
	assert.Equal(t, int32(8025), uiRule.Ports[0].Port.IntVal, "UI rule must target MailHog's UI port")

	// Rule 1: main port unrestricted.
	mainRule := np.Spec.Ingress[1]
	assert.Empty(t, mainRule.From, "main port (SMTP for mailhog) must be reachable from any bound app pod")
	require.Len(t, mainRule.Ports, 1)
	assert.Equal(t, int32(1025), mainRule.Ports[0].Port.IntVal, "main rule must target the binding port")

	assert.Equal(t, "mailhog", np.Spec.PodSelector.MatchLabels["app"], "podSelector must scope to the service's pod (app=<svc-name>)")
}

func unstructuredMiddleware() unstructured.Unstructured {
	mw := unstructured.Unstructured{}
	mw.SetGroupVersionKind(schema.GroupVersionKind{Group: "traefik.io", Version: "v1alpha1", Kind: "Middleware"})
	return mw
}

func TestRepairCredentials_NoChangesIsNoUpdate(t *testing.T) {
	// Secret already matches the desired shape — the helper should
	// not call Update (a no-op).
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rabbit-credentials", Namespace: "ns", ResourceVersion: "1"},
		Data: map[string][]byte{
			"HOST":     []byte("rabbit.ns.svc.cluster.local"),
			"PORT":     []byte("5672"),
			"USERNAME": []byte("kipper"),
			"PASSWORD": []byte("secret"),
			"VHOST":    []byte("/"),
		},
	}
	writes := 0
	c := crfake.NewClientBuilder().WithObjects(existing).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
				writes++
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()
	r := &ServiceReconciler{Client: c}
	require.NoError(t, r.repairCredentials(context.Background(), existing, namedService("rabbit", "rabbitmq")))

	assert.Zero(t, writes, "a Secret already in shape was written again, and the reconciler watches its own Secrets")
	got := secretFromCluster(t, r, "rabbit-credentials")
	assert.Equal(t, "/", got["VHOST"])
	_, hasName := got["NAME"]
	assert.False(t, hasName)
}

// A live workload edited out-of-band to carry a request without its matching
// limit must not be copied through verbatim: pairing only the request onto a
// desired that still has a smaller limit yields request > limit, which the API
// server rejects and wedges the reconcile. The pair is mirrored to stay valid.
func TestPreserveUnpinnedResourcesKeepsPairCoherent(t *testing.T) {
	desired := &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
	}
	live := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
	}

	preserveUnpinnedResources(desired, live, false, false)

	req := desired.Requests[corev1.ResourceMemory]
	lim := desired.Limits[corev1.ResourceMemory]
	assert.False(t, req.Cmp(lim) > 0, "request %s must not exceed limit %s", req.String(), lim.String())
	assert.Equal(t, "512Mi", req.String(), "live request must be preserved")
}

// TestServiceUICatalogConsistency: serviceui.Browseable and the
// reconciler's catalog `ui` block are two views of one fact. A type
// gaining a UI in one place but not the other would mint links for a
// host that never routes, or route a host no one can share.
func TestServiceUICatalogConsistency(t *testing.T) {
	for _, svcType := range []string{"postgres", "mysql", "mongodb", "redis", "rabbitmq", "opensearch", "minio", "mailhog"} {
		hasUI := serviceCatalog(svcType).ui != nil
		if serviceui.Browseable(svcType) != hasUI {
			t.Errorf("%s: serviceui.Browseable=%v but catalog ui block present=%v — keep them aligned",
				svcType, serviceui.Browseable(svcType), hasUI)
		}
	}
}

// TestServiceDeletionRevokesShareLinks pins the finalizer half of the
// share design: the grants die with the service, before the finalizer
// releases, so delete+recreate can never resurrect an old link.
func TestServiceDeletionRevokesShareLinks(t *testing.T) {
	now := metav1.Now()
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "mailhog",
			Namespace:         "supplemento-test",
			UID:               "uid-mailhog-1",
			DeletionTimestamp: &now,
			Finalizers:        []string{serviceFinalizer},
		},
		Spec: kipperv1.ServiceSpec{Type: "mailhog"},
	}

	grants := share.NewGrantStore(k8sfake.NewSimpleClientset())
	g, err := share.NewGrant("uid-mailhog-1", "mailhog", "supplemento-test", "mailhog-supplemento-test.example.com", "", "admin@example.com", time.Hour, time.Now())
	require.NoError(t, err)
	require.NoError(t, grants.Create(context.Background(), g))

	r := &ServiceReconciler{
		Client:      crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc).WithStatusSubresource(svc).Build(),
		Scheme:      testScheme(),
		ShareGrants: grants,
	}

	_, err = r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "mailhog", Namespace: "supplemento-test"},
	})
	require.NoError(t, err)

	assert.Nil(t, grants.Get(context.Background(), g.JTI), "the service's grant must die with it")

	var gone kipperv1.Service
	err = r.Get(context.Background(), types.NamespacedName{Name: "mailhog", Namespace: "supplemento-test"}, &gone)
	assert.True(t, kerrors.IsNotFound(err), "the finalizer should be released after grant cleanup")
}

// TestServiceDeletionFailsClosedWithoutGrantStore pins the fail-closed
// side: a reconciler with no grant store must keep the finalizer and error
// rather than release a deleting service with its links possibly intact.
func TestServiceDeletionFailsClosedWithoutGrantStore(t *testing.T) {
	now := metav1.Now()
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "mailhog",
			Namespace:         "supplemento-test",
			UID:               "uid-mailhog-1",
			DeletionTimestamp: &now,
			Finalizers:        []string{serviceFinalizer},
		},
		Spec: kipperv1.ServiceSpec{Type: "mailhog"},
	}

	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc).WithStatusSubresource(svc).Build(),
		Scheme: testScheme(),
		// ShareGrants deliberately nil.
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "mailhog", Namespace: "supplemento-test"},
	})
	require.Error(t, err, "deletion without a grant store must fail closed")

	var still kipperv1.Service
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "mailhog", Namespace: "supplemento-test"}, &still))
	assert.Contains(t, still.Finalizers, serviceFinalizer, "the finalizer must be retained so deletion retries")
}

// Nothing claims a credentials Secret on the strength of its name, its labels
// or its keys any more. The reconciler either owns the object or refuses to run
// the service against it, because an unowned Secret is refused by every binding
// too, and starting the engine against one only moves the failure into somebody
// else's application. Whoever put the object there says so by setting the
// controller reference; the migration receiver does exactly that for the bytes
// it writes.
func TestReconcileCredentialsSecret_RefusesASecretItDoesNotOwn(t *testing.T) {
	scheme := testScheme()
	ctx := context.Background()
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop-test", UID: types.UID("uid-db")},
		Spec:       kipperv1.ServiceSpec{Type: "postgres"},
	}
	controller := true
	credentialData := map[string][]byte{
		"HOST": []byte("db.shop-test.svc"), "PORT": []byte("5432"), "USERNAME": []byte("kipper"),
		"PASSWORD": []byte("from-the-source-cluster"), "NAME": []byte("app"),
	}

	for _, tc := range []struct {
		name   string
		secret *corev1.Secret
	}{
		{
			// The shape migration used to leave behind: correctly labelled,
			// correctly shaped, and owned by nobody. It is now the receiver's
			// job to have claimed this before the CR ever reconciles.
			name: "no controller at all",
			secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: "db-credentials", Namespace: "shop-test",
				Labels: map[string]string{
					"app": "db", kipperLabel: kipperValue, "kipper.run/service-type": "postgres",
				},
			}, Data: credentialData},
		},
		{
			name: "controlled by another service",
			secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: "db-credentials", Namespace: "shop-test",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: kipperv1.GroupVersion.String(), Kind: "Service",
					Name: "billing", UID: types.UID("uid-billing"), Controller: &controller,
				}},
			}, Data: credentialData},
		},
		{
			// A Velero restore's shape: the reference names this Service but the
			// CR it was written against is gone. Repairing it used to be a race
			// against garbage collection, which is not a thing to ship as a
			// restore strategy, so the mismatch is reported instead.
			name: "a stale owner UID",
			secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: "db-credentials", Namespace: "shop-test",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: kipperv1.GroupVersion.String(), Kind: "Service",
					Name: "db", UID: types.UID("uid-db-before-the-restore"), Controller: &controller,
				}},
			}, Data: credentialData},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := tc.secret.DeepCopy()
			c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(svc, tc.secret).Build()
			r := &ServiceReconciler{Client: c, Scheme: scheme}

			err := r.reconcileCredentialsSecret(ctx, svc)
			require.Error(t, err, "the reconcile must fail rather than run the service against credentials it cannot vouch for")
			assert.Contains(t, err.Error(), "db-credentials")

			var got corev1.Secret
			require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "db-credentials", Namespace: "shop-test"}, &got))
			assert.Equal(t, before.OwnerReferences, got.OwnerReferences, "the object must be left exactly as it was found")
			assert.Equal(t, before.Data, got.Data)
		})
	}
}

// A service's data outlives its credentials Secret: garbage collection removes
// one whose owner UID stops resolving, which is what a Velero restore leaves,
// and an operator can delete one by hand. Generating a replacement password is
// silent and the engine never learns it, so the service comes back locked out of
// its own database.
func TestReconcileCredentialsSecret_RefusesToMintOverExistingData(t *testing.T) {
	scheme := testScheme()
	ctx := context.Background()
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop-test", UID: types.UID("uid-db")},
		Spec:       kipperv1.ServiceSpec{Type: "postgres"},
	}
	restoredVolume := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-db-0", Namespace: "shop-test"},
	}

	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(svc, restoredVolume).Build()
	r := &ServiceReconciler{Client: c, Scheme: scheme}

	err := r.reconcileCredentialsSecret(ctx, svc)
	require.Error(t, err, "a service with data and no credentials must report the mismatch, not paper over it")
	assert.Contains(t, err.Error(), "data-db-0")

	var got corev1.Secret
	assert.True(t, kerrors.IsNotFound(c.Get(ctx, types.NamespacedName{Name: "db-credentials", Namespace: "shop-test"}, &got)),
		"no password may be generated against data that cannot be told about it")

	// The control: the same service with no volume is a new service, and gets
	// its credentials as usual.
	fresh := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(svc.DeepCopy()).Build()
	require.NoError(t, (&ServiceReconciler{Client: fresh, Scheme: scheme}).reconcileCredentialsSecret(ctx, svc))
	require.NoError(t, fresh.Get(ctx, types.NamespacedName{Name: "db-credentials", Namespace: "shop-test"}, &got))
	assert.NotEmpty(t, got.Data["PASSWORD"])
}

// A workload bound to a service that has gone fails reconcileBindingSecrets,
// and because that is fail-closed the whole reconcile aborts — no image change,
// no scale, no env update. The error tells the operator to unbind, and Unbind
// resolves the service first, so by then it is the one thing that cannot be
// done. The service therefore must not finish leaving until nothing is bound to
// it, whichever way it was deleted: kip, the console, kubectl or a GitOps
// prune all end here.
func TestServiceFinalizer_ClearsBindingsBeforeReleasing(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()

	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db", Namespace: "shop-test",
			DeletionTimestamp: &now,
			Finalizers:        []string{serviceFinalizer},
		},
		Spec: kipperv1.ServiceSpec{Type: "postgres"},
	}
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop-test", UID: types.UID("uid-api")},
		Spec: kipperv1.AppSpec{Image: "api:1", ServiceBindings: []kipperv1.ServiceBinding{
			{Name: "db", Prefix: "DB_", Database: "api_db"},
			{Name: "cache", Prefix: "REDIS_"},
		}},
	}
	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "resize", Namespace: "shop-test"},
		Spec:       kipperv1.FunctionSpec{ServiceBindings: []kipperv1.ServiceBinding{{Name: "db", Prefix: "DB_"}}},
	}
	controller := true
	// As the render leaves it: owned by the workload that projected it.
	derived := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "db-app-api-credentials", Namespace: "shop-test",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: kipperv1.GroupVersion.String(), Kind: "App",
			Name: "api", UID: types.UID("uid-api"), Controller: &controller,
		}},
	}}

	c := crfake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(svc, app, fn, derived).Build()
	r := &ServiceReconciler{Client: c, Scheme: testScheme(), ShareGrants: share.NewGrantStore(k8sfake.NewClientset())}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "db", Namespace: "shop-test"}})
	require.NoError(t, err)

	var gotApp kipperv1.App
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "api", Namespace: "shop-test"}, &gotApp))
	require.Len(t, gotApp.Spec.ServiceBindings, 1, "only the binding to the deleted service goes")
	assert.Equal(t, "cache", gotApp.Spec.ServiceBindings[0].Name)

	var gotFn kipperv1.Function
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "resize", Namespace: "shop-test"}, &gotFn))
	assert.Empty(t, gotFn.Spec.ServiceBindings, "a Function's binding must go too")

	// The projection outlives the service on purpose: a workload still serving
	// from a retained revision reads it on every container restart. Its own
	// reconcile retires it once nothing names it.
	assert.NoError(t,
		c.Get(ctx, types.NamespacedName{Name: "db-app-api-credentials", Namespace: "shop-test"}, &corev1.Secret{}),
		"clearing the binding is this finalizer's job; retiring the projection is the workload's")

}

// Failing to unbind must keep the finalizer, so the service stays and the
// deletion retries. Releasing it on a failed cleanup would strand exactly the
// workloads this exists to protect, with the service gone and no way back.
func TestServiceFinalizer_KeepsTheFinalizerWhenClearingFails(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()

	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db", Namespace: "shop-test",
			DeletionTimestamp: &now,
			Finalizers:        []string{serviceFinalizer},
		},
		Spec: kipperv1.ServiceSpec{Type: "postgres"},
	}
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop-test"},
		Spec: kipperv1.AppSpec{Image: "api:1", ServiceBindings: []kipperv1.ServiceBinding{
			{Name: "db", Prefix: "DB_", Database: "api_db"},
		}},
	}

	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc, app).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
				if _, isApp := obj.(*kipperv1.App); isApp {
					return kerrors.NewInternalError(errors.New("etcd unavailable"))
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()
	r := &ServiceReconciler{Client: c, Scheme: testScheme(), ShareGrants: share.NewGrantStore(k8sfake.NewClientset())}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "db", Namespace: "shop-test"}})
	require.Error(t, err, "a cleanup that could not finish must be reported so the deletion retries")

	var still kipperv1.Service
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "db", Namespace: "shop-test"}, &still))
	assert.Contains(t, still.Finalizers, serviceFinalizer, "the finalizer must be held until the bindings are gone")
}

// Deleting a service must not delete a Secret its bindings never derived.
//
// A `database` on a service type with no logical namespace derives nothing —
// redis has no databases to carve up — so that name belongs to whatever else is
// sitting under it. The cleanup decided from the database field alone, and
// moving it into the finalizer took that from a console-only mistake to one on
// every deletion path.
func TestServiceFinalizer_LeavesASecretItNeverDerived(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()

	cache := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cache", Namespace: "shop-test", UID: types.UID("uid-cache"),
			DeletionTimestamp: &now,
			Finalizers:        []string{serviceFinalizer},
		},
		Spec: kipperv1.ServiceSpec{Type: "redis"},
	}
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop-test", UID: types.UID("uid-api")},
		Spec: kipperv1.AppSpec{Image: "api:1", ServiceBindings: []kipperv1.ServiceBinding{
			{Name: "cache", Prefix: "REDIS_", Database: "2"},
		}},
	}
	// Somebody else's object at the name a derived Secret would have had.
	bystander := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cache-app-api-credentials", Namespace: "shop-test"},
		Data:       map[string][]byte{"NOT": []byte("ours")},
	}

	c := crfake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(cache, app, bystander).Build()
	r := &ServiceReconciler{Client: c, Scheme: testScheme(), ShareGrants: share.NewGrantStore(k8sfake.NewClientset())}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "cache", Namespace: "shop-test"}})
	require.NoError(t, err)

	var survived corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "cache-app-api-credentials", Namespace: "shop-test"}, &survived),
		"redis has no logical namespace, so this binding derived nothing and the name is not the service's to delete")
	assert.Equal(t, []byte("ours"), survived.Data["NOT"])

	var gotApp kipperv1.App
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "api", Namespace: "shop-test"}, &gotApp))
	assert.Empty(t, gotApp.Spec.ServiceBindings, "the binding still goes")
}

// A binding that does derive a Secret still only owns the one it rendered.
// writeDerivedBindingSecret refuses to overwrite an object it does not own, so
// a collision at that name survives the render — and must survive the deletion
// too, or the service takes something with it that was never its projection.
func TestServiceFinalizer_LeavesADerivedNameItDoesNotOwn(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()

	db := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db", Namespace: "shop-test", UID: types.UID("uid-db"),
			DeletionTimestamp: &now,
			Finalizers:        []string{serviceFinalizer},
		},
		Spec: kipperv1.ServiceSpec{Type: "postgres"},
	}
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop-test", UID: types.UID("uid-api")},
		Spec: kipperv1.AppSpec{Image: "api:1", ServiceBindings: []kipperv1.ServiceBinding{
			{Name: "db", Prefix: "DB_", Database: "api_db"},
		}},
	}
	controller := true
	// The shared credentials of a service that happens to be called
	// db-app-api, sitting where this binding's projection would go.
	collision := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db-app-api-credentials", Namespace: "shop-test",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(), Kind: "Service",
				Name: "db-app-api", UID: types.UID("uid-db-app-api"), Controller: &controller,
			}},
		},
		Data: map[string][]byte{"PASSWORD": []byte("someone-elses")},
	}

	c := crfake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(db, app, collision).Build()
	r := &ServiceReconciler{Client: c, Scheme: testScheme(), ShareGrants: share.NewGrantStore(k8sfake.NewClientset())}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "db", Namespace: "shop-test"}})
	require.NoError(t, err)

	var survived corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "db-app-api-credentials", Namespace: "shop-test"}, &survived),
		"a Secret this workload never owned is not its projection to delete")
	assert.Equal(t, []byte("someone-elses"), survived.Data["PASSWORD"])
}

// TestReconcileCredentialsSecret_NoPasswordWithoutAuth pins the rule at the
// point it decides what a bound workload receives.
//
// redis, opensearch and mailhog all start with authentication off, and every
// one of them used to be minted with a generated PASSWORD anyway. That
// password reached every bound workload, and redis does worse than ignore it:
// it answers AUTH with an error when no password is set, so a connection
// string built from ${REDIS_PASSWORD} fails and names the wrong cause.
func TestReconcileCredentialsSecret_NoPasswordWithoutAuth(t *testing.T) {
	for _, tc := range []struct {
		svcType string
		wantPw  bool
	}{
		{"redis", false},
		{"opensearch", false},
		{"mailhog", false},
		{"postgres", true},
		{"rabbitmq", true},
	} {
		t.Run(tc.svcType, func(t *testing.T) {
			svc := &kipperv1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns", UID: "svc-uid"},
				Spec:       kipperv1.ServiceSpec{Type: tc.svcType},
			}
			r := &ServiceReconciler{
				Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc).Build(),
				Scheme: testScheme(),
			}
			require.NoError(t, r.reconcileCredentialsSecret(context.Background(), svc))

			got := secretFromCluster(t, r, "svc-credentials")
			assert.NotEmpty(t, got["HOST"], "every type carries HOST")
			assert.NotEmpty(t, got["PORT"], "every type carries PORT")

			_, hasPw := got["PASSWORD"]
			_, hasUser := got["USERNAME"]
			assert.Equal(t, tc.wantPw, hasPw, "PASSWORD presence follows whether the server authenticates")
			assert.Equal(t, tc.wantPw, hasUser, "USERNAME travels with PASSWORD")
		})
	}
}

// TestRepairCredentials_PrunesCredentialsWithoutAuth covers the Secrets
// already on the three live clusters, which were minted before the rule.
//
// Pruning is safe in the direction that matters: none of these servers read the
// password when they started, so removing it locks nothing out of its own data.
func TestRepairCredentials_PrunesCredentialsWithoutAuth(t *testing.T) {
	for _, tc := range []struct {
		svcType string
		wantPw  bool
	}{
		{"redis", false},
		{"opensearch", false},
		{"mailhog", false},
		{"postgres", true},
	} {
		t.Run(tc.svcType, func(t *testing.T) {
			existing := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "cache-credentials", Namespace: "ns"},
				Data: map[string][]byte{
					"HOST":     []byte("cache.ns.svc.cluster.local"),
					"PORT":     []byte("6379"),
					"USERNAME": []byte("kipper"),
					"PASSWORD": []byte("generated-and-never-accepted"),
				},
			}
			r := &ServiceReconciler{Client: crfake.NewClientBuilder().WithObjects(existing).Build()}
			require.NoError(t, r.repairCredentials(context.Background(), existing, namedService("cache", tc.svcType)))

			got := secretFromCluster(t, r, "cache-credentials")
			_, hasPw := got["PASSWORD"]
			_, hasUser := got["USERNAME"]
			assert.Equal(t, tc.wantPw, hasPw, "a stale PASSWORD is pruned where the server has no auth")
			assert.Equal(t, tc.wantPw, hasUser, "so is the USERNAME beside it")
			assert.Equal(t, "cache.ns.svc.cluster.local", got["HOST"], "HOST must survive the prune")
			assert.Equal(t, fmt.Sprintf("%d", serviceCatalog(tc.svcType).port), got["PORT"],
				"the address keys are the service's own, whatever the Secret arrived carrying")
		})
	}
}

// TestRepairCredentials_MinioKeepsItsOwnCredentials guards the edge the
// prune could have taken with it: MinIO authenticates, but under
// ACCESS_KEY/SECRET_KEY rather than USERNAME/PASSWORD.
func TestRepairCredentials_MinioKeepsItsOwnCredentials(t *testing.T) {
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "obj-credentials", Namespace: "ns"},
		Data: map[string][]byte{
			"ENDPOINT":   []byte("http://obj.ns.svc.cluster.local:9000"),
			"ACCESS_KEY": []byte("kipper"),
			"SECRET_KEY": []byte("secret"),
		},
	}
	r := &ServiceReconciler{Client: crfake.NewClientBuilder().WithObjects(existing).Build()}
	require.NoError(t, r.repairCredentials(context.Background(), existing, namedService("obj", "minio")))

	got := secretFromCluster(t, r, "obj-credentials")
	assert.Equal(t, "kipper", got["ACCESS_KEY"], "MinIO's access key is its username")
	assert.Equal(t, "secret", got["SECRET_KEY"], "MinIO's secret key is its password")
}

// A service whose credentials Secret belongs to something else never reaches
// updateStatus, so without this it sits at Pending with the reason visible only
// in the controller's log. The state is reachable by a name collision no
// create-time check can see, a restore among them, so the object has to say why
// it is stuck.
func TestReportCredentialsBlocked_SaysWhyOnTheService(t *testing.T) {
	svc := bareService("postgres")
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc).WithStatusSubresource(svc).Build(),
		Scheme: testScheme(),
	}

	r.reportCredentialsBlocked(context.Background(), svc,
		&credentialsNotOursError{Secret: "db-credentials"}, "SecretNotOwned")

	var live kipperv1.Service
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &live))
	assert.Equal(t, "Failed", live.Status.Phase, "a blocked service reported itself as healthy")

	cond := meta.FindStatusCondition(live.Status.Conditions, "CredentialsReady")
	require.NotNil(t, cond, "nothing on the object says why it is stuck")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Contains(t, cond.Message, "not owned by this service")
}

// The condition describes a state, so it has to go the moment that state does,
// which is when the credentials reconcile succeeds rather than when the whole
// reconcile does. Deferring it to the last step means a later failure keeps the
// object blaming credentials that are fine.
func TestRetractCredentialsBlocked_TakesTheConditionOff(t *testing.T) {
	svc := bareService("postgres")
	svc.Status.Conditions = []metav1.Condition{{
		Type: kipperv1.ConditionCredentialsReady, Status: metav1.ConditionFalse,
		Reason: "SecretNotOwned", Message: "an older failure",
		LastTransitionTime: metav1.Now(),
	}}
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc).WithStatusSubresource(svc).Build(),
		Scheme: testScheme(),
	}

	require.NoError(t, r.retractCredentialsBlocked(context.Background(), svc))

	var live kipperv1.Service
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &live))
	assert.Nil(t, meta.FindStatusCondition(live.Status.Conditions, kipperv1.ConditionCredentialsReady),
		"a condition describing a state that has passed was left on the object")
}

// And it writes nothing when there was nothing to retract, for the same reason
// the reporter does not: an identical status write is an event that brings the
// object straight back round.
//
// The resourceVersion is captured into a string before the call. Status().Update
// writes the new one back into the object it was handed, so reading the field
// afterwards compares the store against itself and passes whatever the code does.
func TestRetractCredentialsBlocked_WritesNothingWhenThereIsNoCondition(t *testing.T) {
	svc := bareService("postgres")
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc).WithStatusSubresource(svc).Build(),
		Scheme: testScheme(),
	}
	var live kipperv1.Service
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &live))
	before := live.ResourceVersion

	require.NoError(t, r.retractCredentialsBlocked(context.Background(), &live))

	var after kipperv1.Service
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &after))
	assert.Equal(t, before, after.ResourceVersion, "an empty retraction wrote the object")
}

// updateStatus runs on every pass of a healthy service, so writing an identical
// status each time keeps the queue busy with work that changes nothing. Same
// capture-before-the-call rule as above.
func TestUpdateStatus_WritesNothingWhenNothingChanged(t *testing.T) {
	svc := bareService("postgres")
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc).WithStatusSubresource(svc).Build(),
		Scheme: testScheme(),
	}
	require.NoError(t, r.updateStatus(context.Background(), svc))

	var settled kipperv1.Service
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &settled))
	before := settled.ResourceVersion

	require.NoError(t, r.updateStatus(context.Background(), &settled))

	var after kipperv1.Service
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &after))
	assert.Equal(t, before, after.ResourceVersion,
		"an unchanged status was written again, which is an event that brings the object back round")
}

// A status write is an update event on the object being reconciled, so writing
// the same failure on every pass feeds the queue its own tail: the failed
// reconcile is requeued with backoff, and the event this emits brings it
// straight back. A blocked service would reconcile continuously until repaired.
func TestReportCredentialsBlocked_WritesOnlyWhenItSaysSomethingNew(t *testing.T) {
	svc := bareService("postgres")
	client := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc).WithStatusSubresource(svc).Build()
	r := &ServiceReconciler{Client: client, Scheme: testScheme()}
	cause := &credentialsNotOursError{Secret: "db-credentials"}

	r.reportCredentialsBlocked(context.Background(), svc, cause, "SecretNotOwned")

	var afterFirst kipperv1.Service
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &afterFirst))
	first := afterFirst.ResourceVersion

	// The same refusal, again, exactly as a requeued reconcile would report it.
	r.reportCredentialsBlocked(context.Background(), &afterFirst, cause, "SecretNotOwned")

	var afterSecond kipperv1.Service
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &afterSecond))
	assert.Equal(t, first, afterSecond.ResourceVersion,
		"reporting the same failure again wrote the object and emitted another event")
}

// Driven through Reconcile rather than through the helper, because the classifier
// is the wiring: with the helper called directly, deleting the classification from
// Reconcile leaves every test green.
func TestReconcile_ReportsAForeignOwnedCredentialsSecret(t *testing.T) {
	svc := bareService("postgres")
	svc.UID = "the-service"
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: secretname.ServiceCredentials("db"), Namespace: "ns",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "kipper.run/v1alpha1", Kind: "App", Name: "db",
				UID: "somebody-else", Controller: ptr.To(true),
			}},
		},
	}
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).
			WithObjects(svc, foreign).WithStatusSubresource(svc).Build(),
		Scheme: testScheme(),
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "db"},
	})
	require.Error(t, err, "the reconcile has to keep failing so it is requeued")

	var live kipperv1.Service
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &live))
	cond := meta.FindStatusCondition(live.Status.Conditions, kipperv1.ConditionCredentialsReady)
	require.NotNil(t, cond, "the object says nothing about why it is stuck")
	assert.Equal(t, "SecretNotOwned", cond.Reason)
	assert.Equal(t, "Failed", live.Status.Phase)
}

// The second permanent refusal, reached by following the first one's own advice:
// told the Secret belongs to something else and offered "remove it if the service
// holds no data yet", an operator who removes it lands on a volume with no
// credentials. Reporting only the first would leave the object describing a
// Secret that is no longer there.
func TestReconcile_ReportsDataLeftWithoutCredentials(t *testing.T) {
	svc := bareService("postgres")
	svc.UID = "the-service"
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-db-0", Namespace: "ns"},
	}
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).
			WithObjects(svc, claim).WithStatusSubresource(svc).Build(),
		Scheme: testScheme(),
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "db"},
	})
	require.Error(t, err)

	var live kipperv1.Service
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &live))
	cond := meta.FindStatusCondition(live.Status.Conditions, kipperv1.ConditionCredentialsReady)
	require.NotNil(t, cond, "the refusal an operator has to clear was not reported")
	assert.Equal(t, "DataWithoutCredentials", cond.Reason,
		"the object still blames the Secret an operator was told to remove")
	assert.Contains(t, cond.Message, "restore")
}

// A transient failure is not a state an operator can clear, so it must not reach
// the object at all.
func TestPermanentCredentialsFailure_IgnoresATransientError(t *testing.T) {
	_, permanent := permanentCredentialsFailure(errors.New(`secrets "db-credentials" already exists`))
	assert.False(t, permanent, "a routine create race would have been stamped on the object")
}

// Driven through Reconcile, because the call site is the thing at issue: the
// helper being correct says nothing about it being called, and it is called
// where the state it describes stops being true rather than at the end of a
// fully successful pass. A later step failing must not keep the object blaming
// credentials that are fine.
func TestReconcile_RetractsTheConditionOnceTheCredentialsReconcile(t *testing.T) {
	svc := bareService("postgres")
	svc.UID = "the-service"
	svc.Status.Conditions = []metav1.Condition{{
		Type: kipperv1.ConditionCredentialsReady, Status: metav1.ConditionFalse,
		Reason: "SecretNotOwned", Message: "a failure that has since been cleared",
		LastTransitionTime: metav1.Now(),
	}}
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).
			WithObjects(svc).WithStatusSubresource(svc).Build(),
		Scheme: testScheme(),
	}

	// The reconcile may still fail further down; the retraction is not
	// conditional on it finishing.
	_, _ = r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "db"},
	})

	var live kipperv1.Service
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &live))
	assert.Nil(t, meta.FindStatusCondition(live.Status.Conditions, kipperv1.ConditionCredentialsReady),
		"the object still blames credentials that reconciled")
}

// A service type whose server asks for no credential cannot be locked out of its
// own data, because the Secret it carries is HOST and PORT — both derived from
// the service's own name and its type's port. Refusing to mint them leaves a
// restored redis, opensearch or mailhog permanently blocked, and the refusal's
// own remedy tells the operator to delete the volume.
func TestReconcileCredentials_MintsForAPasswordlessServiceOverExistingData(t *testing.T) {
	ctx := context.Background()
	svc := bareService("redis")
	svc.UID = "the-service"
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-db-0", Namespace: "ns"},
	}
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).
			WithObjects(svc, claim).WithStatusSubresource(svc).Build(),
		Scheme: testScheme(), ShareGrants: share.NewGrantStore(k8sfake.NewClientset()),
	}

	require.NoError(t, r.reconcileCredentialsSecret(ctx, svc),
		"a service with no password was refused its own reconstructible connection details")

	var minted corev1.Secret
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "db-credentials"}, &minted))
	assert.Equal(t, "db.ns.svc.cluster.local", string(minted.Data["HOST"]))
	assert.NotEmpty(t, minted.Data["PORT"])
	assert.NotContains(t, minted.Data, "PASSWORD")
}

// The same guard on a type that does authenticate still refuses: postgres reads
// its password once, at initialisation, so a fresh one over an initialised
// volume locks every bound workload out with no indication of why.
func TestReconcileCredentials_StillRefusesAnAuthenticatedServiceOverExistingData(t *testing.T) {
	ctx := context.Background()
	svc := bareService("postgres")
	svc.UID = "the-service"
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-db-0", Namespace: "ns"},
	}
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).
			WithObjects(svc, claim).WithStatusSubresource(svc).Build(),
		Scheme: testScheme(), ShareGrants: share.NewGrantStore(k8sfake.NewClientset()),
	}

	var missing *credentialsMissingError
	require.ErrorAs(t, r.reconcileCredentialsSecret(ctx, svc), &missing,
		"a password was minted over a database that had already made up its mind")
}

// Ownership is not readiness. A restore can bring back a Secret with the right
// controller reference and none of the keys a bound workload reads, and the
// derived ones are reconstructible, so they are restored rather than reported.
func TestReconcileCredentials_RestoresDerivedKeysOnADrainedSecret(t *testing.T) {
	ctx := context.Background()
	svc := bareService("redis")
	svc.UID = "the-service"
	drained := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db-credentials", Namespace: "ns",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(), Kind: "Service",
				Name: "db", UID: "the-service", Controller: ptr.To(true),
			}},
		},
	}
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).
			WithObjects(svc, drained).WithStatusSubresource(svc).Build(),
		Scheme: testScheme(), ShareGrants: share.NewGrantStore(k8sfake.NewClientset()),
	}

	require.NoError(t, r.reconcileCredentialsSecret(ctx, svc))

	var repaired corev1.Secret
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "db-credentials"}, &repaired))
	assert.Equal(t, "db.ns.svc.cluster.local", string(repaired.Data["HOST"]),
		"a bound workload reads HOST out of this Secret and it was left absent")
	assert.NotEmpty(t, repaired.Data["PORT"])
}

// The key that cannot be reconstructed is the password, and an initialised
// volume already knows the old one. That is the same permanent state as a Secret
// deleted outright, so it is reported the same way rather than passing as a
// successful reconcile that leaves the pod in CreateContainerConfigError.
func TestReconcileCredentials_RefusesADrainedPasswordOverExistingData(t *testing.T) {
	ctx := context.Background()
	svc := bareService("postgres")
	svc.UID = "the-service"
	drained := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db-credentials", Namespace: "ns",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(), Kind: "Service",
				Name: "db", UID: "the-service", Controller: ptr.To(true),
			}},
		},
		Data: map[string][]byte{"HOST": []byte("db.ns.svc.cluster.local"), "PORT": []byte("5432")},
	}
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-db-0", Namespace: "ns"},
	}
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).
			WithObjects(svc, drained, claim).WithStatusSubresource(svc).Build(),
		Scheme: testScheme(), ShareGrants: share.NewGrantStore(k8sfake.NewClientset()),
	}

	var missing *credentialsMissingError
	require.ErrorAs(t, r.reconcileCredentialsSecret(ctx, svc), &missing,
		"an owned Secret with no password passed as reconciled, so nothing on the object says why the pod will not start")
}

// With no volume the engine has not initialised, so nothing disagrees with a
// fresh password. This is the same rule the mint path already follows, applied
// to a Secret that survived with its password missing.
func TestReconcileCredentials_MintsAPasswordIntoADrainedSecretWithNoData(t *testing.T) {
	ctx := context.Background()
	svc := bareService("postgres")
	svc.UID = "the-service"
	drained := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db-credentials", Namespace: "ns",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(), Kind: "Service",
				Name: "db", UID: "the-service", Controller: ptr.To(true),
			}},
		},
	}
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).
			WithObjects(svc, drained).WithStatusSubresource(svc).Build(),
		Scheme: testScheme(), ShareGrants: share.NewGrantStore(k8sfake.NewClientset()),
	}

	require.NoError(t, r.reconcileCredentialsSecret(ctx, svc))

	var repaired corev1.Secret
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "db-credentials"}, &repaired))
	assert.NotEmpty(t, repaired.Data["PASSWORD"], "a service with no data was left with no password")
	assert.Equal(t, "kipper", string(repaired.Data["USERNAME"]))
}

// A retraction that fails to write is invisible to every later check in the
// pass: the condition is already gone from the copy in memory, so the status
// diff at the end compares two objects that both lack it and writes nothing.
// On a settled service nothing else changes either, so the pass ends clean and
// the object goes on claiming its credentials are blocked until something else
// happens to it. The reconcile has to fail so it is requeued.
func TestReconcile_RequeuesWhenTheRetractionCannotBeWritten(t *testing.T) {
	ctx := context.Background()
	svc := bareService("postgres")
	svc.UID = "the-service"
	svc.Status = kipperv1.ServiceStatus{
		Phase:             "Pending",
		Host:              "db.ns.svc.cluster.local",
		Port:              serviceCatalog("postgres").port,
		CredentialsSecret: "db-credentials",
		Conditions: []metav1.Condition{{
			Type: kipperv1.ConditionCredentialsReady, Status: metav1.ConditionFalse,
			Reason: "SecretNotOwned", Message: "cleared since", LastTransitionTime: metav1.Now(),
		}},
	}
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db-credentials", Namespace: "ns",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(), Kind: "Service",
				Name: "db", UID: "the-service", Controller: ptr.To(true),
			}},
		},
		Data: map[string][]byte{
			"HOST": []byte("db.ns.svc.cluster.local"), "PORT": []byte("5432"),
			"USERNAME": []byte("kipper"), "PASSWORD": []byte("already-set"), "NAME": []byte("app"),
		},
	}

	writes := 0
	c := crfake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(svc, creds).WithStatusSubresource(svc).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl crclient.Client, sub string, obj crclient.Object, opts ...crclient.SubResourceUpdateOption) error {
				writes++
				// Only the retraction, which is the first status write of the
				// pass. Failing every one of them would make the closing status
				// write fail too, and the reconcile would report that instead.
				if writes == 1 {
					return kerrors.NewInternalError(errors.New("etcd unavailable"))
				}
				return cl.Status().Update(ctx, obj, opts...)
			},
		}).Build()
	r := &ServiceReconciler{Client: c, Scheme: testScheme(), ShareGrants: share.NewGrantStore(k8sfake.NewClientset())}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "db"}})
	require.Error(t, err, "the failed retraction ended the pass clean, so nothing will retract it")

	var live kipperv1.Service
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "db"}, &live))
	assert.NotNil(t, meta.FindStatusCondition(live.Status.Conditions, kipperv1.ConditionCredentialsReady),
		"the write failed, so the object still carries the condition and the requeue is what clears it")
}

// A Secret restored into another namespace carries the old namespace's host, and
// filling only what is absent would leave it there: every workload bound to the
// restored service would open its connections against the service it was copied
// from. The address keys say where the service is, nothing more, so they are
// computed from the Service on every pass.
func TestRepairCredentials_ConvergesAnAddressFromAnotherNamespace(t *testing.T) {
	restored := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: "ns"},
		Data: map[string][]byte{
			"HOST":     []byte("db.production.svc.cluster.local"),
			"PORT":     []byte("5432"),
			"USERNAME": []byte("kipper"),
			"PASSWORD": []byte("carried-over"),
		},
	}
	r := &ServiceReconciler{Client: crfake.NewClientBuilder().WithObjects(restored).Build()}

	require.NoError(t, r.repairCredentials(context.Background(), restored, namedService("db", "postgres")))

	got := secretFromCluster(t, r, "db-credentials")
	assert.Equal(t, "db.ns.svc.cluster.local", got["HOST"],
		"bound workloads would have connected to the namespace this was copied from")
	assert.Equal(t, "carried-over", got["PASSWORD"], "the password the volume knows must survive")
}

// MinIO's endpoint is an address like any other, and it carries the namespace
// twice over: in the host and in the URL a bound workload is handed whole.
func TestRepairCredentials_ConvergesTheMinioEndpoint(t *testing.T) {
	restored := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "obj-credentials", Namespace: "ns"},
		Data: map[string][]byte{
			"ENDPOINT":   []byte("http://obj.production.svc.cluster.local:9000"),
			"ACCESS_KEY": []byte("kipper"),
			"SECRET_KEY": []byte("carried-over"),
		},
	}
	r := &ServiceReconciler{Client: crfake.NewClientBuilder().WithObjects(restored).Build()}

	require.NoError(t, r.repairCredentials(context.Background(), restored, namedService("obj", "minio")))

	got := secretFromCluster(t, r, "obj-credentials")
	assert.Contains(t, got["ENDPOINT"], "obj.ns.svc.cluster.local")
	assert.Equal(t, "carried-over", got["SECRET_KEY"], "the key minio initialised with must survive")
}

// The username is not an address. The StatefulSet passes a fixed one, which an
// engine reads only when it initialises, so a database that came up under a
// different name still answers to that name alone and the Secret is the only
// record of it. Overwriting it would lock out every workload bound to a service
// older than the current default.
func TestRepairCredentials_KeepsAUsernameItDidNotChoose(t *testing.T) {
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: "ns"},
		Data: map[string][]byte{
			"HOST":     []byte("db.ns.svc.cluster.local"),
			"PORT":     []byte("5432"),
			"USERNAME": []byte("postgres"),
			"PASSWORD": []byte("secret"),
		},
	}
	// The volume is the premise: it is what makes the name in the Secret the one
	// the engine answers to rather than a value somebody typed.
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-db-0", Namespace: "ns"},
	}
	r := &ServiceReconciler{Client: crfake.NewClientBuilder().WithObjects(existing, claim).Build()}

	require.NoError(t, r.repairCredentials(context.Background(), existing, namedService("db", "postgres")))

	got := secretFromCluster(t, r, "db-credentials")
	assert.Equal(t, "postgres", got["USERNAME"], "the engine answers to this name and nothing else")
}

// MinIO authenticates under ACCESS_KEY and SECRET_KEY, so a USERNAME or PASSWORD
// on its Secret is read by nothing and injected into every bound workload.
func TestRepairCredentials_PrunesTheKeysMinioDoesNotAuthenticateWith(t *testing.T) {
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "obj-credentials", Namespace: "ns"},
		Data: map[string][]byte{
			"ENDPOINT":   []byte("http://obj.ns.svc.cluster.local:9000"),
			"ACCESS_KEY": []byte("kipper"),
			"SECRET_KEY": []byte("secret"),
			"USERNAME":   []byte("kipper"),
			"PASSWORD":   []byte("read-by-nothing"),
		},
	}
	r := &ServiceReconciler{Client: crfake.NewClientBuilder().WithObjects(existing).Build()}

	require.NoError(t, r.repairCredentials(context.Background(), existing, namedService("obj", "minio")))

	got := secretFromCluster(t, r, "obj-credentials")
	assert.NotContains(t, got, "USERNAME")
	assert.NotContains(t, got, "PASSWORD")
	assert.Equal(t, "secret", got["SECRET_KEY"], "the key it does authenticate with must stay")
}

// One pass, one write. The reconciler watches the Secrets it owns, so a second
// update in the same pass is a second event bringing the reconcile straight
// back to do nothing.
func TestRepairCredentials_WritesOnceForAllOfIt(t *testing.T) {
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rabbit-credentials", Namespace: "ns"},
		Data: map[string][]byte{
			"USERNAME": []byte("kipper"),
			"PASSWORD": []byte("secret"),
			"NAME":     []byte("app"),
		},
	}
	writes := 0
	c := crfake.NewClientBuilder().WithObjects(existing).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
				if _, isSecret := obj.(*corev1.Secret); isSecret {
					writes++
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()
	r := &ServiceReconciler{Client: c}

	// Restores an absent HOST and PORT, adds the VHOST default, prunes NAME.
	require.NoError(t, r.repairCredentials(context.Background(), existing, namedService("rabbit", "rabbitmq")))

	assert.Equal(t, 1, writes, "the repair and the defaults each wrote, so the pass fed the queue twice")
	got := secretFromCluster(t, r, "rabbit-credentials")
	assert.Equal(t, "rabbit.ns.svc.cluster.local", got["HOST"])
	assert.Equal(t, "/", got["VHOST"])
	assert.NotContains(t, got, "NAME")
}

// A minio Secret in the shape every authenticating service once shared holds the
// root credential under PASSWORD, and the container reads MINIO_ROOT_PASSWORD
// from SECRET_KEY. Refusing it as material that cannot be reconstructed would
// send an operator to a backup, or to deleting the volume, while the credential
// its data was written under is in the same Secret under the other name.
func TestRepairCredentials_CarriesALegacyMinioCredentialOver(t *testing.T) {
	legacy := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "obj-credentials", Namespace: "ns"},
		Data: map[string][]byte{
			"HOST":     []byte("obj.ns.svc.cluster.local"),
			"PORT":     []byte("9000"),
			"USERNAME": []byte("kipper"),
			"PASSWORD": []byte("what-the-volume-knows"),
			"NAME":     []byte("app"),
		},
	}
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-obj-0", Namespace: "ns"},
	}
	r := &ServiceReconciler{Client: crfake.NewClientBuilder().WithObjects(legacy, claim).Build()}

	require.NoError(t, r.repairCredentials(context.Background(), legacy, namedService("obj", "minio")),
		"the credential was in the Secret all along, so there was nothing to refuse")

	got := secretFromCluster(t, r, "obj-credentials")
	assert.Equal(t, "what-the-volume-knows", got["SECRET_KEY"],
		"the root credential the volume was written under must reach the key minio reads")
	assert.Equal(t, "kipper", got["ACCESS_KEY"])
	assert.NotContains(t, got, "PASSWORD", "the key minio reads nothing from must not stay behind")
}

// MinIO carries no HOST or PORT, so a legacy-shaped Secret's pair was converged
// by nothing and pruned by nothing. A restore from another namespace left them
// pointing at it, and a workload reading HOST rather than ENDPOINT connected
// there. A key this type does not carry is stale whatever it holds.
func TestRepairCredentials_PrunesAnAddressTheTypeDoesNotCarry(t *testing.T) {
	restored := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "obj-credentials", Namespace: "ns"},
		Data: map[string][]byte{
			"HOST":       []byte("obj.production.svc.cluster.local"),
			"PORT":       []byte("9000"),
			"ENDPOINT":   []byte("http://obj.production.svc.cluster.local:9000"),
			"ACCESS_KEY": []byte("kipper"),
			"SECRET_KEY": []byte("carried-over"),
		},
	}
	r := &ServiceReconciler{Client: crfake.NewClientBuilder().WithObjects(restored).Build()}

	require.NoError(t, r.repairCredentials(context.Background(), restored, namedService("obj", "minio")))

	got := secretFromCluster(t, r, "obj-credentials")
	assert.NotContains(t, got, "HOST", "minio has no HOST, so this one pointed at another namespace for ever")
	assert.NotContains(t, got, "PORT")
	assert.Contains(t, got["ENDPOINT"], "obj.ns.svc.cluster.local")
}

// The same rule from the other side: the S3 pair on a service that authenticates
// under USERNAME and PASSWORD is read by nothing and injected into everything.
func TestRepairCredentials_PrunesTheKeysOfAnotherShape(t *testing.T) {
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: "ns"},
		Data: map[string][]byte{
			"HOST":       []byte("db.ns.svc.cluster.local"),
			"PORT":       []byte("5432"),
			"USERNAME":   []byte("kipper"),
			"PASSWORD":   []byte("secret"),
			"ACCESS_KEY": []byte("kipper"),
			"SECRET_KEY": []byte("read-by-nothing"),
		},
	}
	r := &ServiceReconciler{Client: crfake.NewClientBuilder().WithObjects(existing).Build()}

	require.NoError(t, r.repairCredentials(context.Background(), existing, namedService("db", "postgres")))

	got := secretFromCluster(t, r, "db-credentials")
	assert.NotContains(t, got, "ACCESS_KEY")
	assert.NotContains(t, got, "SECRET_KEY")
	assert.Equal(t, "secret", got["PASSWORD"], "the key this type does authenticate with must stay")
}

// The identity is the other half of what the engine keeps, and it is no more
// reconstructible than the password. Filling in the current default over a
// volume would publish an account the database has never heard of, and every
// bound workload would authenticate as nobody.
func TestRepairCredentials_RefusesADrainedUsernameOverExistingData(t *testing.T) {
	svc := bareService("postgres")
	svc.UID = "the-service"
	drained := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: "ns"},
		Data: map[string][]byte{
			"HOST":     []byte("db.ns.svc.cluster.local"),
			"PORT":     []byte("5432"),
			"PASSWORD": []byte("still-here"),
		},
	}
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-db-0", Namespace: "ns"},
	}
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc, drained, claim).Build(),
		Scheme: testScheme(),
	}

	var missing *credentialsMissingError
	require.ErrorAs(t, r.repairCredentials(context.Background(), drained, svc), &missing,
		"the default username was published for a database that may answer to another")
}

// With no volume nothing has initialised, so the default identity is the one the
// engine will come up under.
func TestRepairCredentials_FillsADrainedUsernameWithNoData(t *testing.T) {
	svc := bareService("postgres")
	svc.UID = "the-service"
	drained := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: "ns"},
		Data:       map[string][]byte{"PASSWORD": []byte("still-here")},
	}
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc, drained).Build(),
		Scheme: testScheme(),
	}

	require.NoError(t, r.repairCredentials(context.Background(), drained, svc))

	got := secretFromCluster(t, r, "db-credentials")
	assert.Equal(t, "kipper", got["USERNAME"])
	assert.Equal(t, "still-here", got["PASSWORD"], "a password that was never missing must not be replaced")
}

// MinIO's root user is not engine state: the StatefulSet passes it at every
// start, so the running server answers to that name and a Secret saying
// otherwise is simply wrong.
func TestRepairCredentials_ConvergesTheMinioRootUser(t *testing.T) {
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "obj-credentials", Namespace: "ns"},
		Data: map[string][]byte{
			"ENDPOINT":   []byte("http://obj.ns.svc.cluster.local:9000"),
			"ACCESS_KEY": []byte("someone-else"),
			"SECRET_KEY": []byte("carried-over"),
		},
	}
	r := &ServiceReconciler{Client: crfake.NewClientBuilder().WithObjects(existing).Build()}

	require.NoError(t, r.repairCredentials(context.Background(), existing, namedService("obj", "minio")))

	got := secretFromCluster(t, r, "obj-credentials")
	assert.Equal(t, "kipper", got["ACCESS_KEY"], "the server answers to the name its container was given")
	assert.Equal(t, "carried-over", got["SECRET_KEY"])
}

// A refusal that misnames its cause sends the operator to look at the wrong
// thing: told the password is missing, they open the Secret, find one, and
// conclude the condition is lying. It names the key that is actually absent.
func TestRepairCredentials_NamesTheKeyThatIsMissing(t *testing.T) {
	svc := bareService("postgres")
	svc.UID = "the-service"
	drained := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: "ns"},
		Data: map[string][]byte{
			"HOST":     []byte("db.ns.svc.cluster.local"),
			"PORT":     []byte("5432"),
			"PASSWORD": []byte("still-here"),
		},
	}
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-db-0", Namespace: "ns"},
	}
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc, drained, claim).Build(),
		Scheme: testScheme(),
	}

	err := r.repairCredentials(context.Background(), drained, svc)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "USERNAME", "the operator was sent to look for the wrong missing key")
	assert.NotContains(t, err.Error(), "no password", "the password is right there in the Secret")
}

// With no volume there is no history to preserve. The engine has not started, so
// it will come up as whatever the StatefulSet passes it, and a Secret naming
// somebody else would hand every bound workload an account that does not exist.
func TestRepairCredentials_ConvergesAUsernameWithNoDataBehindIt(t *testing.T) {
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: "ns"},
		Data: map[string][]byte{
			"HOST":     []byte("db.ns.svc.cluster.local"),
			"PORT":     []byte("5432"),
			"USERNAME": []byte("postgres"),
			"PASSWORD": []byte("secret"),
		},
	}
	r := &ServiceReconciler{Client: crfake.NewClientBuilder().WithObjects(existing).Build()}

	require.NoError(t, r.repairCredentials(context.Background(), existing, namedService("db", "postgres")))

	got := secretFromCluster(t, r, "db-credentials")
	assert.Equal(t, "kipper", got["USERNAME"],
		"the statefulset will initialise this database as kipper, whatever the Secret says")
	assert.Equal(t, "secret", got["PASSWORD"], "a password that was never missing must not be replaced")
}
