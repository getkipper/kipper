package copyenv

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = kipperv1.AddToScheme(s)
	return s
}

func int32Ptr(v int32) *int32 { return &v }

func TestCopier_CopiesAppsWithFreshRoutes(t *testing.T) {
	src := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "demo-test"},
		Spec: kipperv1.AppSpec{
			Image:    "nginx:1.27",
			Port:     80,
			Replicas: int32Ptr(2),
			Env:      map[string]string{"FOO": "bar"},
			Route: &kipperv1.AppRoute{
				Host: "web.example.com",
				Path: "/",
			},
			ServiceBindings: []kipperv1.ServiceBinding{{Name: "backend", Prefix: "DB_"}},
		},
	}
	backend := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: "demo-test"},
		Spec:       kipperv1.AppSpec{Image: "backend:1", Port: 8080, Replicas: int32Ptr(1)},
	}

	crClient := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(src, backend).Build()
	c := &Copier{CRClient: crClient, Client: fake.NewClientset()}

	summary, err := c.Run(context.Background(), Options{
		Source: "demo-test", Target: "demo-prod", TargetEnv: "prod",
		ClusterDomain: "example.com", AssignDefaultRoutes: true,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, summary.Apps)

	// `web` had a route → gets a fresh hostname under the cluster wildcard
	var copiedWeb kipperv1.App
	require.NoError(t, crClient.Get(context.Background(), crclient.ObjectKey{Namespace: "demo-prod", Name: "web"}, &copiedWeb))
	require.NotNil(t, copiedWeb.Spec.Route)
	assert.Equal(t, "web-prod.example.com", copiedWeb.Spec.Route.Host)
	assert.Equal(t, "/", copiedWeb.Spec.Route.Path)
	// Spec carried over verbatim, including bindings and env
	assert.Equal(t, "nginx:1.27", copiedWeb.Spec.Image)
	assert.Equal(t, "bar", copiedWeb.Spec.Env["FOO"])
	require.Len(t, copiedWeb.Spec.ServiceBindings, 1)
	assert.Equal(t, "backend", copiedWeb.Spec.ServiceBindings[0].Name)

	// `backend` had no route → stays without one (we don't fabricate URLs)
	var copiedBackend kipperv1.App
	require.NoError(t, crClient.Get(context.Background(), crclient.ObjectKey{Namespace: "demo-prod", Name: "backend"}, &copiedBackend))
	assert.Nil(t, copiedBackend.Spec.Route)
}

func TestCopier_RouteCopyDisabledLeavesAppsRouteless(t *testing.T) {
	src := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "demo-test"},
		Spec: kipperv1.AppSpec{
			Image: "nginx", Port: 80,
			Route: &kipperv1.AppRoute{Host: "web.example.com", Path: "/"},
		},
	}
	crClient := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(src).Build()
	c := &Copier{CRClient: crClient, Client: fake.NewClientset()}

	_, err := c.Run(context.Background(), Options{
		Source: "demo-test", Target: "demo-prod", TargetEnv: "prod",
		ClusterDomain: "example.com", AssignDefaultRoutes: false,
	})
	require.NoError(t, err)

	var copied kipperv1.App
	require.NoError(t, crClient.Get(context.Background(), crclient.ObjectKey{Namespace: "demo-prod", Name: "web"}, &copied))
	assert.Nil(t, copied.Spec.Route, "AssignDefaultRoutes=false must leave route nil")
}

func TestCopier_DefaultEnvUsesBareAppName(t *testing.T) {
	src := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "demo-test"},
		Spec:       kipperv1.AppSpec{Image: "nginx", Port: 80, Route: &kipperv1.AppRoute{Host: "x", Path: "/"}},
	}
	crClient := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(src).Build()
	c := &Copier{CRClient: crClient, Client: fake.NewClientset()}

	_, err := c.Run(context.Background(), Options{
		Source: "demo-test", Target: "demo", TargetEnv: "default",
		ClusterDomain: "example.com", AssignDefaultRoutes: true,
	})
	require.NoError(t, err)

	var copied kipperv1.App
	require.NoError(t, crClient.Get(context.Background(), crclient.ObjectKey{Namespace: "demo", Name: "web"}, &copied))
	require.NotNil(t, copied.Spec.Route)
	assert.Equal(t, "web.example.com", copied.Spec.Route.Host, "default env should not append -env to the hostname")
}

func TestCopier_CopiesServicesVolumesFunctionsJobs(t *testing.T) {
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: "demo-test"},
		Spec:       kipperv1.ServiceSpec{Type: "postgres", Version: "16", Storage: "5Gi"},
	}
	vol := &kipperv1.Volume{
		ObjectMeta: metav1.ObjectMeta{Name: "uploads", Namespace: "demo-test"},
		Spec:       kipperv1.VolumeSpec{Size: "10Gi"},
	}
	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "thumbnailer", Namespace: "demo-test"},
		Spec:       kipperv1.FunctionSpec{Runtime: "node", Port: 8080},
	}
	job := &kipperv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly-cleanup", Namespace: "demo-test"},
		Spec:       kipperv1.JobSpec{Image: "busybox", Schedule: "0 3 * * *"},
	}

	crClient := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc, vol, fn, job).Build()
	c := &Copier{CRClient: crClient, Client: fake.NewClientset()}

	summary, err := c.Run(context.Background(), Options{
		Source: "demo-test", Target: "demo-prod", TargetEnv: "prod",
		ClusterDomain: "example.com", AssignDefaultRoutes: true,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Services)
	assert.Equal(t, 1, summary.Volumes)
	assert.Equal(t, 1, summary.Functions)
	assert.Equal(t, 1, summary.Jobs)

	var copiedSvc kipperv1.Service
	require.NoError(t, crClient.Get(context.Background(), crclient.ObjectKey{Namespace: "demo-prod", Name: "backend"}, &copiedSvc))
	assert.Equal(t, "postgres", copiedSvc.Spec.Type)
	assert.Equal(t, "5Gi", copiedSvc.Spec.Storage)
}

func TestCopier_SecretFiltering(t *testing.T) {
	tests := []struct {
		name   string
		secret *corev1.Secret
		copied bool
	}{
		{
			name: "user-managed secret is copied",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "stripe-keys",
					Namespace: "demo-test",
					Labels:    map[string]string{"app.kubernetes.io/managed-by": "kipper"},
				},
				Data: map[string][]byte{"STRIPE_KEY": []byte("sk_test_xxx")},
			},
			copied: true,
		},
		{
			name: "service credentials are skipped (regenerated)",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "backend-credentials",
					Namespace: "demo-test",
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": "kipper",
						"kipper.run/service-type":      "postgres",
					},
				},
			},
			copied: false,
		},
		{
			name: "per-binding secret is skipped",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "backend-web-credentials",
					Namespace: "demo-test",
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": "kipper",
						"kipper.run/binding":           "true",
					},
				},
			},
			copied: false,
		},
		{
			name: "registry secret is skipped (system-managed, staged per workload)",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "gitlab-registry",
					Namespace: "demo-test",
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": "kipper",
						"kipper.run/registry":          "true",
					},
				},
				Type: corev1.SecretTypeDockerConfigJson,
			},
			copied: false,
		},
		{
			name: "controller-owned secret is skipped (its controller will recreate it)",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "web-env",
					Namespace: "demo-test",
					Labels:    map[string]string{"app.kubernetes.io/managed-by": "kipper"},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "kipper.run/v1alpha1",
							Kind:       "App",
							Name:       "web",
							UID:        "abc",
							Controller: ptrBool(true),
						},
					},
				},
			},
			copied: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewClientset(tt.secret)
			crClient := crfake.NewClientBuilder().WithScheme(testScheme()).Build()
			c := &Copier{CRClient: crClient, Client: client}

			summary, err := c.Run(context.Background(), Options{
				Source: "demo-test", Target: "demo-prod", TargetEnv: "prod",
				ClusterDomain: "example.com",
			})
			require.NoError(t, err)

			_, getErr := client.CoreV1().Secrets("demo-prod").Get(context.Background(), tt.secret.Name, metav1.GetOptions{})
			if tt.copied {
				assert.NoError(t, getErr, "expected secret %s to be copied", tt.secret.Name)
				assert.Equal(t, 1, summary.Secrets)
			} else {
				assert.Error(t, getErr, "expected secret %s to be skipped", tt.secret.Name)
				assert.Equal(t, 0, summary.Secrets)
			}
		})
	}
}

func TestCopier_RejectsSameSourceAndTarget(t *testing.T) {
	c := &Copier{CRClient: crfake.NewClientBuilder().WithScheme(testScheme()).Build(), Client: fake.NewClientset()}
	_, err := c.Run(context.Background(), Options{Source: "x", Target: "x"})
	assert.Error(t, err)
}

func TestCopier_StripsPromotionAnnotations(t *testing.T) {
	src := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web",
			Namespace: "demo-test",
			Annotations: map[string]string{
				"kipper.run/promoted-from":  "acc",
				"kipper.run/promoted-image": "nginx:old",
				"author":                    "team",
			},
		},
		Spec: kipperv1.AppSpec{Image: "nginx", Port: 80},
	}
	crClient := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(src).Build()
	c := &Copier{CRClient: crClient, Client: fake.NewClientset()}

	_, err := c.Run(context.Background(), Options{Source: "demo-test", Target: "demo-prod", TargetEnv: "prod"})
	require.NoError(t, err)

	var copied kipperv1.App
	require.NoError(t, crClient.Get(context.Background(), crclient.ObjectKey{Namespace: "demo-prod", Name: "web"}, &copied))
	assert.NotContains(t, copied.Annotations, "kipper.run/promoted-from")
	assert.NotContains(t, copied.Annotations, "kipper.run/promoted-image")
	assert.Equal(t, "team", copied.Annotations["author"], "non-promotion annotations must survive")
}

func ptrBool(b bool) *bool { return &b }

func TestCopier_AppOverrides_Route(t *testing.T) {
	src := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "demo-test"},
		Spec: kipperv1.AppSpec{
			Image: "nginx", Port: 80,
			Route: &kipperv1.AppRoute{Host: "web-test.example.com", Path: "/"},
		},
	}
	crClient := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(src).Build()
	c := &Copier{CRClient: crClient, Client: fake.NewClientset()}

	_, err := c.Run(context.Background(), Options{
		Source: "demo-test", Target: "demo-prod", TargetEnv: "prod",
		ClusterDomain: "example.com", AssignDefaultRoutes: true,
		AppOverrides: map[string]AppOverride{
			"web": {Route: &RouteOverride{Host: "app.example.com", Path: "/api"}},
		},
	})
	require.NoError(t, err)

	var copied kipperv1.App
	require.NoError(t, crClient.Get(context.Background(), crclient.ObjectKey{Namespace: "demo-prod", Name: "web"}, &copied))
	require.NotNil(t, copied.Spec.Route)
	assert.Equal(t, "app.example.com", copied.Spec.Route.Host, "wizard host override must beat the auto-default")
	assert.Equal(t, "/api", copied.Spec.Route.Path)
}

func TestCopier_AppOverrides_EmptyHostDropsRoute(t *testing.T) {
	src := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "demo-test"},
		Spec: kipperv1.AppSpec{
			Image: "nginx", Port: 80,
			Route: &kipperv1.AppRoute{Host: "web-test.example.com", Path: "/"},
		},
	}
	crClient := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(src).Build()
	c := &Copier{CRClient: crClient, Client: fake.NewClientset()}

	_, err := c.Run(context.Background(), Options{
		Source: "demo-test", Target: "demo-prod", TargetEnv: "prod",
		ClusterDomain: "example.com", AssignDefaultRoutes: true,
		AppOverrides: map[string]AppOverride{
			"web": {Route: &RouteOverride{Host: ""}},
		},
	})
	require.NoError(t, err)

	var copied kipperv1.App
	require.NoError(t, crClient.Get(context.Background(), crclient.ObjectKey{Namespace: "demo-prod", Name: "web"}, &copied))
	assert.Nil(t, copied.Spec.Route, "explicit empty host means user wants no route on the new env")
}

func TestCopier_AppOverrides_EnvReplaces(t *testing.T) {
	src := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "demo-test"},
		Spec: kipperv1.AppSpec{
			Image: "nginx", Port: 80,
			Env: map[string]string{
				"FOO":        "test-value",
				"STRIPE_KEY": "sk_test_xxx",
				"LOG_LEVEL":  "debug",
			},
		},
	}
	crClient := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(src).Build()
	c := &Copier{CRClient: crClient, Client: fake.NewClientset()}

	_, err := c.Run(context.Background(), Options{
		Source: "demo-test", Target: "demo-prod", TargetEnv: "prod",
		AppOverrides: map[string]AppOverride{
			"web": {Env: map[string]string{
				"FOO":        "test-value",
				"STRIPE_KEY": "sk_live_yyy",
				"LOG_LEVEL":  "info",
			}},
		},
	})
	require.NoError(t, err)

	var copied kipperv1.App
	require.NoError(t, crClient.Get(context.Background(), crclient.ObjectKey{Namespace: "demo-prod", Name: "web"}, &copied))
	assert.Equal(t, "sk_live_yyy", copied.Spec.Env["STRIPE_KEY"], "edited value must take precedence")
	assert.Equal(t, "info", copied.Spec.Env["LOG_LEVEL"])
	assert.Equal(t, "test-value", copied.Spec.Env["FOO"], "untouched keys carry through unchanged")
}

func TestCopier_AppOverrides_ReplicasAndResources(t *testing.T) {
	src := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "demo-test"},
		Spec: kipperv1.AppSpec{
			Image: "nginx", Port: 80, Replicas: int32Ptr(1),
			Resources: kipperv1.AppResources{Profile: "standard", MemoryLimit: "256Mi"},
		},
	}
	crClient := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(src).Build()
	c := &Copier{CRClient: crClient, Client: fake.NewClientset()}

	_, err := c.Run(context.Background(), Options{
		Source: "demo-test", Target: "demo-prod", TargetEnv: "prod",
		AppOverrides: map[string]AppOverride{
			"web": {
				Replicas:  int32Ptr(3),
				Resources: &kipperv1.AppResources{Profile: "standard", MemoryLimit: "1Gi"},
			},
		},
	})
	require.NoError(t, err)

	var copied kipperv1.App
	require.NoError(t, crClient.Get(context.Background(), crclient.ObjectKey{Namespace: "demo-prod", Name: "web"}, &copied))
	require.NotNil(t, copied.Spec.Replicas)
	assert.Equal(t, int32(3), *copied.Spec.Replicas)
	assert.Equal(t, "1Gi", copied.Spec.Resources.MemoryLimit)
}

func TestCopier_AppOverrides_LeaveOthersAlone(t *testing.T) {
	web := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "demo-test"},
		Spec: kipperv1.AppSpec{
			Image: "nginx", Port: 80,
			Env: map[string]string{"FOO": "bar"},
		},
	}
	worker := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "demo-test"},
		Spec: kipperv1.AppSpec{
			Image: "worker", Port: 8080, Replicas: int32Ptr(1),
			Env: map[string]string{"FOO": "bar"},
		},
	}
	crClient := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(web, worker).Build()
	c := &Copier{CRClient: crClient, Client: fake.NewClientset()}

	_, err := c.Run(context.Background(), Options{
		Source: "demo-test", Target: "demo-prod", TargetEnv: "prod",
		AppOverrides: map[string]AppOverride{
			"web": {Env: map[string]string{"FOO": "edited"}},
		},
	})
	require.NoError(t, err)

	var copiedWeb, copiedWorker kipperv1.App
	require.NoError(t, crClient.Get(context.Background(), crclient.ObjectKey{Namespace: "demo-prod", Name: "web"}, &copiedWeb))
	require.NoError(t, crClient.Get(context.Background(), crclient.ObjectKey{Namespace: "demo-prod", Name: "worker"}, &copiedWorker))
	assert.Equal(t, "edited", copiedWeb.Spec.Env["FOO"])
	assert.Equal(t, "bar", copiedWorker.Spec.Env["FOO"], "apps without an override copy verbatim")
}

// A copied environment must not depend on the one it was copied from. A link
// inside the project moves along with the copy; one naming another project is
// dropped, because the equivalent over there may not exist and may not have
// consented, and a prod environment quietly calling a test-side backend looks
// entirely healthy while it does it.
//
// Only the declaration moves. The address each link injects is derived from it
// and stored nowhere, so it follows the rewrite by itself.
func TestCopiedLinksDoNotReachBackIntoTheSourceEnvironment(t *testing.T) {
	src := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "hrportal-backend", Namespace: "hrportal-test"},
		Spec: kipperv1.AppSpec{
			Image: "hrportal:v1", Port: 8080,
			Env: map[string]string{"LOG_LEVEL": "debug"},
			Links: []kipperv1.AppLink{
				{App: "worker", Namespace: "hrportal-test"},
				{App: "docuseal", Namespace: "docuseal-test"},
			},
		},
	}

	out, _ := newAppForTarget(src, Options{Source: "hrportal-test", Target: "hrportal-prod", TargetEnv: "prod"})

	assert.Equal(t, []kipperv1.AppLink{{App: "worker", Namespace: "hrportal-prod"}}, out.Spec.Links,
		"the project's own dependency follows the copy; the other project's is dropped")
	assert.Equal(t, map[string]string{"LOG_LEVEL": "debug"}, out.Spec.Env,
		"what the operator typed is copied verbatim, links being no part of it")
}

// The wizard sends the whole env map whenever the operator edits any single
// value. That map and the link list are separate things now, and a copy must
// not let one reach into the other in either direction.
func TestAnEnvOverrideLeavesTheLinkRewriteAlone(t *testing.T) {
	src := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "hrportal-backend", Namespace: "hrportal-test"},
		Spec: kipperv1.AppSpec{
			Image: "hrportal:v1", Port: 8080,
			Env: map[string]string{"LOG_LEVEL": "debug"},
			Links: []kipperv1.AppLink{
				{App: "worker", Namespace: "hrportal-test"},
				{App: "docuseal", Namespace: "docuseal-test"},
			},
		},
	}

	out, _ := newAppForTarget(src, Options{
		Source: "hrportal-test", Target: "hrportal-prod", TargetEnv: "prod",
		AppOverrides: map[string]AppOverride{"hrportal-backend": {Env: map[string]string{"LOG_LEVEL": "info"}}},
	})

	assert.Equal(t, []kipperv1.AppLink{{App: "worker", Namespace: "hrportal-prod"}}, out.Spec.Links)
	assert.Equal(t, "info", out.Spec.Env["LOG_LEVEL"], "the operator's edit wins")
}

// A link naming neither the source nor the target is another project's and is
// dropped, and an app with no env at all must not panic on the way through.
func TestLinkRewriteHandlesAThirdNamespaceAndNoEnv(t *testing.T) {
	src := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "hrportal-test"},
		Spec: kipperv1.AppSpec{
			Image: "worker:v1", Port: 9000,
			Links: []kipperv1.AppLink{{App: "billing", Namespace: "billing-prod"}},
		},
	}
	out, _ := newAppForTarget(src, Options{Source: "hrportal-test", Target: "hrportal-prod", TargetEnv: "prod"})
	assert.Empty(t, out.Spec.Links, "a link into a third namespace belongs to whoever owns it, not to the copy")
}

// An app linked before addresses were derived carries a stored address as well
// as the link, and nothing migrates it. Dropping the link without the address
// hands the copy a URL for a dependency it no longer declares, with no allowance
// to reach it — it dials the environment this copy exists to be separate from
// and fails on a connection nothing on either surface explains.
func TestADroppedLinkTakesItsStoredAddressWithIt(t *testing.T) {
	src := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "hrportal-backend", Namespace: "hrportal-test"},
		Spec: kipperv1.AppSpec{
			Image: "hrportal:v1", Port: 8080,
			// The shape an upgraded app is in: stored address plus declaration.
			Env: map[string]string{
				"DOCUSEAL_URL": "http://docuseal.docuseal-test.svc.cluster.local:3000",
				"LOG_LEVEL":    "debug",
			},
			Links: []kipperv1.AppLink{{App: "docuseal", Namespace: "docuseal-test"}},
		},
	}

	out, dropped := newAppForTarget(src, Options{Source: "hrportal-test", Target: "hrportal-prod", TargetEnv: "prod"})

	assert.Empty(t, out.Spec.Links)
	assert.NotContains(t, out.Spec.Env, "DOCUSEAL_URL",
		"the address goes with the link it belonged to")
	assert.Equal(t, "debug", out.Spec.Env["LOG_LEVEL"], "the operator's own variables stay")
	assert.Equal(t, []string{"docuseal-test/docuseal"}, dropped,
		"and the operator is told which dependency the copy could not bring along")
}

// A link into a sibling environment of the same project is a deliberate
// cross-environment dependency. The copy has no basis to reproduce it, so it is
// dropped like any other — but saying nothing leaves the operator to find out
// when something stops working.
func TestDroppingASiblingEnvironmentLinkIsReported(t *testing.T) {
	src := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "hrportal-test"},
		Spec: kipperv1.AppSpec{
			Image: "w:v1", Port: 9000,
			Links: []kipperv1.AppLink{{App: "api", Namespace: "hrportal-acc"}},
		},
	}
	out, dropped := newAppForTarget(src, Options{Source: "hrportal-test", Target: "hrportal-prod", TargetEnv: "prod"})
	assert.Empty(t, out.Spec.Links)
	assert.Equal(t, []string{"hrportal-acc/api"}, dropped)
}
