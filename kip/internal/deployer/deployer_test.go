package deployer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func testDeployer() (*Deployer, *dynamicfake.FakeDynamicClient) {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "kipper.run", Version: "v1alpha1", Kind: "App"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "kipper.run", Version: "v1alpha1", Kind: "AppList"},
		&unstructured.UnstructuredList{},
	)

	dynClient := dynamicfake.NewSimpleDynamicClient(scheme)
	k8sClient := fake.NewSimpleClientset() //nolint:staticcheck

	return &Deployer{Client: k8sClient, Dynamic: dynClient}, dynClient
}

func TestDeployCreatesAppCR(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()

	err := d.Deploy(ctx, Options{
		Name:      "api",
		Namespace: "default",
		Image:     "nginx:latest",
		Port:      8080,
		Replicas:  2,
		Domain:    "api-test.kipper.run",
	})
	require.NoError(t, err)

	app, err := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	require.NoError(t, err)

	image, _, _ := unstructured.NestedString(app.Object, "spec", "image")
	assert.Equal(t, "nginx:latest", image)

	replicas, _, _ := unstructured.NestedInt64(app.Object, "spec", "replicas")
	assert.Equal(t, int64(2), replicas)

	host, _, _ := unstructured.NestedString(app.Object, "spec", "route", "host")
	assert.Equal(t, "api-test.kipper.run", host)
}

func TestDeployWithEnvSetsEnvOnCR(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()

	err := d.Deploy(ctx, Options{
		Name:      "api",
		Namespace: "default",
		Image:     "nginx:latest",
		Port:      8080,
		Domain:    "api.kipper.run",
		Env:       map[string]string{"LOG_LEVEL": "debug"},
	})
	require.NoError(t, err)

	app, err := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	require.NoError(t, err)

	env, _, _ := unstructured.NestedStringMap(app.Object, "spec", "env")
	assert.Equal(t, "debug", env["LOG_LEVEL"])
}

func TestDeployWithResourcesSetsProfile(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()

	err := d.Deploy(ctx, Options{
		Name:        "api",
		Namespace:   "default",
		Image:       "nginx:latest",
		Port:        8080,
		MemoryLimit: "2Gi",
		CPULimit:    "500m",
	})
	require.NoError(t, err)

	app, err := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	require.NoError(t, err)

	memLimit, _, _ := unstructured.NestedString(app.Object, "spec", "resources", "memoryLimit")
	assert.Equal(t, "2Gi", memLimit)
	memRequest, _, _ := unstructured.NestedString(app.Object, "spec", "resources", "memoryRequest")
	assert.Equal(t, "2Gi", memRequest)

	cpuLimit, _, _ := unstructured.NestedString(app.Object, "spec", "resources", "cpuLimit")
	assert.Equal(t, "500m", cpuLimit)
	cpuRequest, _, _ := unstructured.NestedString(app.Object, "spec", "resources", "cpuRequest")
	assert.Equal(t, "500m", cpuRequest)
}

func TestRedeployPreservesFieldsTheCLIDoesNotManage(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()

	// An app that already carries config added outside the deploy flags:
	// service bindings, volumes, autoscale, route-level auth, and resources
	// (set via the console or kip apply).
	existing := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kipper.run/v1alpha1",
			"kind":       "App",
			"metadata": map[string]interface{}{
				"name":      "api",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"image":    "ghcr.io/acme/api:v1",
				"replicas": int64(5), // scaled up via kip app scale
				"serviceBindings": []interface{}{
					map[string]interface{}{"name": "db", "prefix": "DB"},
				},
				"volumes": []interface{}{
					map[string]interface{}{"name": "data", "mountPath": "/data"},
				},
				"autoscale": map[string]interface{}{"minReplicas": int64(2), "maxReplicas": int64(6)},
				"route": map[string]interface{}{
					"host":          "custom.example.com", // set via the console
					"requireApiKey": true,
					"basicAuth":     true,
				},
				"resources": map[string]interface{}{
					"profile":       "custom",
					"cpuRequest":    "100m",
					"cpuLimit":      "1",
					"memoryRequest": "512Mi",
					"memoryLimit":   "1Gi",
				},
			},
		},
	}
	_, err := dynClient.Resource(AppGVR).Namespace("default").Create(ctx, existing, metav1.CreateOptions{})
	require.NoError(t, err)

	// A CI-style redeploy that bumps the image and memory only. Everything the
	// deploy flags didn't touch — scale, custom host, route auth, bindings —
	// must survive. Domain is always derived and passed, but "route" is not in
	// Changed, so the custom host must not be clobbered.
	err = d.Deploy(ctx, Options{
		Name:        "api",
		Namespace:   "default",
		Image:       "ghcr.io/acme/api:v2",
		Port:        8080,
		Domain:      "api-default.kipper.run", // derived, but --route not set
		MemoryLimit: "2Gi",
		Changed:     map[string]bool{"image": true, "memory": true},
	})
	require.NoError(t, err)

	app, err := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	require.NoError(t, err)

	image, _, _ := unstructured.NestedString(app.Object, "spec", "image")
	assert.Equal(t, "ghcr.io/acme/api:v2", image, "the image should be updated")

	// F1: a redeploy without --replicas must not reset a scaled app to 1.
	replicas, _, _ := unstructured.NestedInt64(app.Object, "spec", "replicas")
	assert.Equal(t, int64(5), replicas, "replicas must survive a redeploy that didn't set --replicas")

	// F6: a redeploy without --route must not overwrite a console-set host.
	host, _, _ := unstructured.NestedString(app.Object, "spec", "route", "host")
	assert.Equal(t, "custom.example.com", host, "custom route host must survive a redeploy")

	bindings, found, _ := unstructured.NestedSlice(app.Object, "spec", "serviceBindings")
	assert.True(t, found, "serviceBindings must survive a redeploy")
	assert.Len(t, bindings, 1)

	volumes, found, _ := unstructured.NestedSlice(app.Object, "spec", "volumes")
	assert.True(t, found, "volumes must survive a redeploy")
	assert.Len(t, volumes, 1)

	autoscale, found, _ := unstructured.NestedMap(app.Object, "spec", "autoscale")
	assert.True(t, found, "autoscale must survive a redeploy")
	assert.Equal(t, int64(6), autoscale["maxReplicas"])

	requireAPIKey, _, _ := unstructured.NestedBool(app.Object, "spec", "route", "requireApiKey")
	assert.True(t, requireAPIKey, "route.requireApiKey must survive a redeploy")
	basicAuth, _, _ := unstructured.NestedBool(app.Object, "spec", "route", "basicAuth")
	assert.True(t, basicAuth, "route.basicAuth must survive a redeploy")

	cpuLimit, _, _ := unstructured.NestedString(app.Object, "spec", "resources", "cpuLimit")
	assert.Equal(t, "1", cpuLimit, "cpuLimit must survive a memory-only redeploy")
	memLimit, _, _ := unstructured.NestedString(app.Object, "spec", "resources", "memoryLimit")
	assert.Equal(t, "2Gi", memLimit, "memoryLimit should be updated")
}

func seedApp(t *testing.T, dynClient *dynamicfake.FakeDynamicClient, spec map[string]interface{}) {
	t.Helper()
	_, err := dynClient.Resource(AppGVR).Namespace("default").Create(context.Background(), &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kipper.run/v1alpha1",
			"kind":       "App",
			"metadata":   map[string]interface{}{"name": "api", "namespace": "default"},
			"spec":       spec,
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
}

func TestRedeployWithReplicasFlagUpdatesReplicas(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()
	seedApp(t, dynClient, map[string]interface{}{"image": "ghcr.io/acme/api:v1", "replicas": int64(5)})

	err := d.Deploy(ctx, Options{
		Name: "api", Namespace: "default", Image: "ghcr.io/acme/api:v1", Replicas: 3,
		Changed: map[string]bool{"replicas": true},
	})
	require.NoError(t, err)

	app, _ := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	replicas, _, _ := unstructured.NestedInt64(app.Object, "spec", "replicas")
	assert.Equal(t, int64(3), replicas, "an explicit --replicas must be applied")
}

func TestRedeployImageClearsGitSource(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()
	seedApp(t, dynClient, map[string]interface{}{
		"image":    "busybox:latest",
		"replicas": int64(2),
		"git":      map[string]interface{}{"url": "https://github.com/acme/api.git", "branch": "main"},
	})

	// Switch the git app to a prebuilt image.
	err := d.Deploy(ctx, Options{
		Name: "api", Namespace: "default", Image: "ghcr.io/acme/api:v2", Port: 8080,
		Changed: map[string]bool{"image": true},
	})
	require.NoError(t, err)

	app, _ := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	_, gitFound, _ := unstructured.NestedMap(app.Object, "spec", "git")
	assert.False(t, gitFound, "spec.git must be cleared when switching to --image")
	image, _, _ := unstructured.NestedString(app.Object, "spec", "image")
	assert.Equal(t, "ghcr.io/acme/api:v2", image)
}

func TestRedeployGitOntoImageAppKeepsServingImage(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()
	seedApp(t, dynClient, map[string]interface{}{
		"image":    "ghcr.io/acme/api:v1",
		"replicas": int64(2),
	})

	// Switch a prebuilt-image app to a git source. The git clear only fires on
	// an --image switch, so the old image stays in spec alongside the new git
	// block. The app keeps serving the last image until the first build
	// finishes, rather than dropping to a placeholder — no downtime.
	err := d.Deploy(ctx, Options{
		Name: "api", Namespace: "default", Port: 8080,
		GitURL: "https://github.com/acme/api.git", GitBranch: "main",
		Changed: map[string]bool{"git": true, "branch": true},
	})
	require.NoError(t, err)

	app, _ := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	gitURL, _, _ := unstructured.NestedString(app.Object, "spec", "git", "url")
	assert.Equal(t, "https://github.com/acme/api.git", gitURL, "the git source must be set")
	image, _, _ := unstructured.NestedString(app.Object, "spec", "image")
	assert.Equal(t, "ghcr.io/acme/api:v1", image, "the old image must stay until the first git build lands")
}

func TestRedeploySwitchingGitRepoDropsOldBranchAndCredentials(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()
	seedApp(t, dynClient, map[string]interface{}{
		"image": "busybox:latest",
		"git": map[string]interface{}{ //nolint:gosec // G101 false positive: credentialsSecret is a K8s Secret name, not a credential value
			"url":               "https://gitlab.example.com/team/old.git",
			"branch":            "develop",
			"credentialsSecret": "api-git-credentials",
		},
	})

	// Point the app at a different repo without passing --branch or a token.
	err := d.Deploy(ctx, Options{
		Name: "api", Namespace: "default", Port: 8080,
		GitURL:  "https://github.com/team/new.git",
		Changed: map[string]bool{"git": true},
	})
	require.NoError(t, err)

	app, _ := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	git, _, _ := unstructured.NestedMap(app.Object, "spec", "git")
	assert.Equal(t, "https://github.com/team/new.git", git["url"])
	_, hasBranch := git["branch"]
	assert.False(t, hasBranch, "the old repo's branch must not carry over to a different repo")
	_, hasCreds := git["credentialsSecret"]
	assert.False(t, hasCreds, "the old repo's credentials must not carry over to a different repo")
}

func TestRedeploySameGitRepoKeepsBranch(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()
	seedApp(t, dynClient, map[string]interface{}{
		"image": "busybox:latest",
		"git": map[string]interface{}{ //nolint:gosec // G101 false positive: credentialsSecret is a K8s Secret name, not a credential value
			"url":               "https://github.com/team/api.git",
			"branch":            "main",
			"credentialsSecret": "api-git-credentials",
		},
	})

	// Redeploying the same repo (e.g. re-passing --git for a rebuild) keeps the
	// existing branch and credentials.
	err := d.Deploy(ctx, Options{
		Name: "api", Namespace: "default", Port: 8080,
		GitURL:  "https://github.com/team/api.git",
		Changed: map[string]bool{"git": true},
	})
	require.NoError(t, err)

	app, _ := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	branch, _, _ := unstructured.NestedString(app.Object, "spec", "git", "branch")
	assert.Equal(t, "main", branch, "same-repo redeploy must keep the branch")
	creds, _, _ := unstructured.NestedString(app.Object, "spec", "git", "credentialsSecret")
	assert.Equal(t, "api-git-credentials", creds, "same-repo redeploy must keep credentials")
}

func TestRedeployResetsSecurityHeaderFlag(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()
	seedApp(t, dynClient, map[string]interface{}{
		"image": "ghcr.io/acme/api:v1",
		"route": map[string]interface{}{"host": "custom.example.com", "noSecurityHeaders": true},
	})

	// Turn security headers back on with an explicit --no-security-headers=false.
	err := d.Deploy(ctx, Options{
		Name: "api", Namespace: "default", Domain: "api-default.kipper.run",
		NoSecurityHeaders: false,
		Changed:           map[string]bool{"no-security-headers": true},
	})
	require.NoError(t, err)

	app, _ := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	noSec, _, _ := unstructured.NestedBool(app.Object, "spec", "route", "noSecurityHeaders")
	assert.False(t, noSec, "explicit --no-security-headers=false must reset the field")
	// The host must not be clobbered by the derived domain.
	host, _, _ := unstructured.NestedString(app.Object, "spec", "route", "host")
	assert.Equal(t, "custom.example.com", host, "toggling security headers must not overwrite the host")
}

func TestRedeployRateLimitOnRoutelessAppIsRejected(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()
	// An app with no route (internal-only, route removed via the console).
	seedApp(t, dynClient, map[string]interface{}{"image": "ghcr.io/acme/api:v1", "replicas": int64(2)})

	// A stray --rate-limit must not resurrect a public route.
	err := d.Deploy(ctx, Options{
		Name: "api", Namespace: "default", RateLimit: 100,
		Changed: map[string]bool{"rate-limit": true},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no route")

	app, _ := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	_, routeFound, _ := unstructured.NestedMap(app.Object, "spec", "route")
	assert.False(t, routeFound, "a route-only flag must not create a route on a routeless app")
}

func TestRedeployRateLimitWithRouteFlagIsAllowed(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()
	seedApp(t, dynClient, map[string]interface{}{"image": "ghcr.io/acme/api:v1"})

	// Passing --route alongside --rate-limit is an explicit request for a route.
	err := d.Deploy(ctx, Options{
		Name: "api", Namespace: "default", Domain: "api.example.com", RateLimit: 100,
		Changed: map[string]bool{"route": true, "rate-limit": true},
	})
	require.NoError(t, err)

	app, _ := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	host, _, _ := unstructured.NestedString(app.Object, "spec", "route", "host")
	assert.Equal(t, "api.example.com", host)
	rl, _, _ := unstructured.NestedInt64(app.Object, "spec", "route", "rateLimit")
	assert.Equal(t, int64(100), rl)
}

func TestDeleteRemovesAppCR(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()

	_ = d.Deploy(ctx, Options{
		Name: "api", Namespace: "default", Image: "nginx", Port: 80,
	})

	err := d.Delete(ctx, "default", "api")
	require.NoError(t, err)

	_, err = dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	assert.Error(t, err)
}

func TestDeleteNotFoundReturnsError(t *testing.T) {
	d, _ := testDeployer()
	ctx := context.Background()

	err := d.Delete(ctx, "default", "nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestScaleUpdatesAppCR(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()

	_ = d.Deploy(ctx, Options{
		Name: "api", Namespace: "default", Image: "nginx", Port: 80,
	})

	err := d.Scale(ctx, "default", "api", 5)
	require.NoError(t, err)

	app, _ := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	replicas, _, _ := unstructured.NestedInt64(app.Object, "spec", "replicas")
	assert.Equal(t, int64(5), replicas)
}

// With autoscaling enabled the HPA owns the replica count and the reconciler
// ignores spec.replicas, so accepting the write would report a scale that
// never happens — the trap that broke the migration write-freeze guidance.
func TestScaleRefusesAutoscaledApp(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()

	_ = d.Deploy(ctx, Options{
		Name: "api", Namespace: "default", Image: "nginx", Port: 80,
	})
	app, _ := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	require.NoError(t, unstructured.SetNestedField(app.Object, true, "spec", "autoscale", "enabled"))
	_, err := dynClient.Resource(AppGVR).Namespace("default").Update(ctx, app, metav1.UpdateOptions{})
	require.NoError(t, err)

	err = d.Scale(ctx, "default", "api", 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "autoscal")
}

func TestUpdateImageUpdatesAppCR(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()

	_ = d.Deploy(ctx, Options{
		Name: "api", Namespace: "default", Image: "nginx:v1", Port: 80,
	})

	err := d.UpdateImage(ctx, "default", "api", "nginx:v2")
	require.NoError(t, err)

	app, _ := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	image, _, _ := unstructured.NestedString(app.Object, "spec", "image")
	assert.Equal(t, "nginx:v2", image)
}

func TestAppStatusFromCRRunning(t *testing.T) {
	cr := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"metadata": map[string]interface{}{"name": "api"},
			"spec": map[string]interface{}{
				"image":    "ghcr.io/acme/api:v1",
				"replicas": int64(2),
			},
			"status": map[string]interface{}{
				"phase":         "Running",
				"image":         "ghcr.io/acme/api:v1",
				"replicas":      int64(2),
				"readyReplicas": int64(2),
			},
		},
	}

	status := appStatusFromCR(cr)
	assert.Equal(t, "api", status.Name)
	assert.Equal(t, "running", status.Status)
	assert.Equal(t, "ghcr.io/acme/api:v1", status.Image)
	assert.Equal(t, int32(2), status.Replicas)
	assert.Equal(t, int32(2), status.Ready)
}

func TestAppStatusFromCRStopped(t *testing.T) {
	cr := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"metadata": map[string]interface{}{"name": "stopped"},
			"spec": map[string]interface{}{
				"image":    "nginx:latest",
				"replicas": int64(0),
			},
			"status": map[string]interface{}{
				"phase": "Stopped",
			},
		},
	}

	status := appStatusFromCR(cr)
	assert.Equal(t, "stopped", status.Status)
	assert.Equal(t, int32(0), status.Replicas)
}

func TestAppStatusFromCRPendingFallsBackToSpec(t *testing.T) {
	// Freshly created CR — controller hasn't reconciled yet, so status
	// fields are empty. The CLI should still show the requested image
	// and replicas from spec, with phase "pending".
	cr := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"metadata": map[string]interface{}{"name": "fresh"},
			"spec": map[string]interface{}{
				"image":    "ghcr.io/acme/api:v1",
				"replicas": int64(3),
			},
		},
	}

	status := appStatusFromCR(cr)
	assert.Equal(t, "pending", status.Status)
	assert.Equal(t, "ghcr.io/acme/api:v1", status.Image)
	assert.Equal(t, int32(3), status.Replicas)
	assert.Equal(t, int32(0), status.Ready)
}

func TestRestartStampsAppCRAnnotation(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()
	seedApp(t, dynClient, map[string]interface{}{"image": "ghcr.io/acme/api:v1"})

	require.NoError(t, d.Restart(ctx, "default", "api"))

	app, err := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	require.NoError(t, err)
	annotations := app.GetAnnotations()
	// Restart must stamp the App CR (the reconciler's source of truth for the
	// pod-template annotation), not patch the Deployment directly.
	assert.NotEmpty(t, annotations["kipper.run/restartedAt"], "restart must set restartedAt on the App CR")
}

func TestRestartErrorsWhenAppMissing(t *testing.T) {
	d, _ := testDeployer()
	err := d.Restart(context.Background(), "default", "ghost")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// TestDeployWithProfileSetsNamedProfile: --profile writes the named profile
// with no explicit request/limit values, so the reconciler applies the
// profile's own defaults.
func TestDeployWithProfileSetsNamedProfile(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()

	err := d.Deploy(ctx, Options{
		Name:      "api",
		Namespace: "default",
		Image:     "eclipse-temurin:21",
		Port:      8080,
		Profile:   "jvm",
		Changed:   map[string]bool{"image": true, "profile": true},
	})
	require.NoError(t, err)

	app, err := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	require.NoError(t, err)

	profile, _, _ := unstructured.NestedString(app.Object, "spec", "resources", "profile")
	assert.Equal(t, "jvm", profile)
	resources, _, _ := unstructured.NestedMap(app.Object, "spec", "resources")
	assert.Len(t, resources, 1, "a named profile must not carry explicit request/limit values")
}

// TestRedeployProfileReplacesCustomResources pins the profile-switch
// semantics: leftover custom request/limit values would override the
// profile in the reconciler, so the switch replaces the resources block.
func TestRedeployProfileReplacesCustomResources(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()

	require.NoError(t, d.Deploy(ctx, Options{
		Name:      "api",
		Namespace: "default",
		Image:     "eclipse-temurin:21",
		Port:      8080,
		CPULimit:  "750m",
		Changed:   map[string]bool{"image": true, "cpu": true},
	}))

	require.NoError(t, d.Deploy(ctx, Options{
		Name:      "api",
		Namespace: "default",
		Profile:   "jvm",
		Changed:   map[string]bool{"profile": true},
	}))

	app, err := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	require.NoError(t, err)

	resources, _, _ := unstructured.NestedMap(app.Object, "spec", "resources")
	assert.Equal(t, map[string]interface{}{"profile": "jvm"}, resources)
}

// TestRedeployCPUOntoProfileMeansCustom: explicit values switch the app off
// the named profile.
func TestRedeployCPUOntoProfileMeansCustom(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()

	require.NoError(t, d.Deploy(ctx, Options{
		Name:      "api",
		Namespace: "default",
		Image:     "eclipse-temurin:21",
		Port:      8080,
		Profile:   "jvm",
		Changed:   map[string]bool{"image": true, "profile": true},
	}))

	require.NoError(t, d.Deploy(ctx, Options{
		Name:      "api",
		Namespace: "default",
		CPULimit:  "750m",
		Changed:   map[string]bool{"cpu": true},
	}))

	app, err := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	require.NoError(t, err)

	profile, _, _ := unstructured.NestedString(app.Object, "spec", "resources", "profile")
	assert.Equal(t, "custom", profile)
	cpuLimit, _, _ := unstructured.NestedString(app.Object, "spec", "resources", "cpuLimit")
	assert.Equal(t, "750m", cpuLimit)
}

// TestUpdateProfileReplacesResources covers the kip app update path.
func TestUpdateProfileReplacesResources(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()

	require.NoError(t, d.Deploy(ctx, Options{
		Name:      "api",
		Namespace: "default",
		Image:     "eclipse-temurin:21",
		Port:      8080,
		CPULimit:  "750m",
		Changed:   map[string]bool{"image": true, "cpu": true},
	}))

	require.NoError(t, d.UpdateProfile(ctx, "default", "api", "memory-heavy"))

	app, err := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	require.NoError(t, err)
	resources, _, _ := unstructured.NestedMap(app.Object, "spec", "resources")
	assert.Equal(t, map[string]interface{}{"profile": "memory-heavy"}, resources)
}

// Redirect domains were settable from kipper.yaml and the console but not from
// kip, while every route field beside them was. This is that gap closed.
func TestDeployWritesRedirectFromOntoTheRoute(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()
	seedApp(t, dynClient, map[string]interface{}{
		"image": "ghcr.io/acme/shop:v1",
		"route": map[string]interface{}{"host": "example.com"},
	})

	err := d.Deploy(ctx, Options{
		Name: "api", Namespace: "default",
		RedirectFrom: []string{"www.example.com", "old-brand.example"},
		Changed:      map[string]bool{"redirect-from": true},
	})
	require.NoError(t, err)

	app, _ := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	got, found, _ := unstructured.NestedStringSlice(app.Object, "spec", "route", "redirectFrom")
	require.True(t, found, "the flag must reach spec.route.redirectFrom")
	assert.Equal(t, []string{"www.example.com", "old-brand.example"}, got)

	// The host it was already serving is untouched: --redirect-from is a
	// route-affecting flag, not a request to rewrite the route.
	host, _, _ := unstructured.NestedString(app.Object, "spec", "route", "host")
	assert.Equal(t, "example.com", host, "a console-set host must survive the flag")
}

// The converse, and the reason the route block is guarded at all: a deploy that
// says nothing about redirects must leave the ones already there alone.
func TestDeployLeavesRedirectFromAloneWhenNotAsked(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()
	seedApp(t, dynClient, map[string]interface{}{
		"image": "ghcr.io/acme/shop:v1",
		"route": map[string]interface{}{
			"host":         "example.com",
			"redirectFrom": []interface{}{"www.example.com"},
		},
	})

	err := d.Deploy(ctx, Options{
		Name: "api", Namespace: "default", Image: "ghcr.io/acme/shop:v2",
		Changed: map[string]bool{"image": true},
	})
	require.NoError(t, err)

	app, _ := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	got, _, _ := unstructured.NestedStringSlice(app.Object, "spec", "route", "redirectFrom")
	assert.Equal(t, []string{"www.example.com"}, got, "an image bump must not clear the redirect domains")
}

// Changing redirect domains is a configuration edit, not a deployment. This
// pins the thing that makes it one: the pod template is untouched, so nothing
// restarts, and the rest of the route survives.
func TestUpdateRedirectFromLeavesTheWorkloadAlone(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()
	seedApp(t, dynClient, map[string]interface{}{
		"image":    "ghcr.io/acme/shop:v1",
		"port":     int64(3000),
		"replicas": int64(3),
		"route": map[string]interface{}{
			"host": "example.com", "rateLimit": int64(50),
		},
	})

	require.NoError(t, d.UpdateRedirectFrom(ctx, "default", "api", []string{"www.example.com"}))

	app, _ := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	got, _, _ := unstructured.NestedStringSlice(app.Object, "spec", "route", "redirectFrom")
	assert.Equal(t, []string{"www.example.com"}, got)

	// Everything that decides what the pods look like is exactly as it was.
	image, _, _ := unstructured.NestedString(app.Object, "spec", "image")
	replicas, _, _ := unstructured.NestedInt64(app.Object, "spec", "replicas")
	port, _, _ := unstructured.NestedInt64(app.Object, "spec", "port")
	assert.Equal(t, "ghcr.io/acme/shop:v1", image, "the image must not change")
	assert.Equal(t, int64(3), replicas, "nor the replica count")
	assert.Equal(t, int64(3000), port, "nor the port")

	// And the rest of the route survives.
	host, _, _ := unstructured.NestedString(app.Object, "spec", "route", "host")
	rl, _, _ := unstructured.NestedInt64(app.Object, "spec", "route", "rateLimit")
	assert.Equal(t, "example.com", host)
	assert.Equal(t, int64(50), rl, "a sibling route field must survive")
}

// Passing the flag with no hosts clears them, which is the imperative way out
// that --redirect-from on deploy deliberately does not offer.
func TestUpdateRedirectFromWithNoHostsClearsThem(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()
	seedApp(t, dynClient, map[string]interface{}{
		"image": "ghcr.io/acme/shop:v1",
		"route": map[string]interface{}{
			"host": "example.com", "redirectFrom": []interface{}{"www.example.com"},
		},
	})

	require.NoError(t, d.UpdateRedirectFrom(ctx, "default", "api", nil))

	app, _ := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	_, found, _ := unstructured.NestedStringSlice(app.Object, "spec", "route", "redirectFrom")
	assert.False(t, found, "the list is gone, not emptied in place")
	host, _, _ := unstructured.NestedString(app.Object, "spec", "route", "host")
	assert.Equal(t, "example.com", host, "clearing redirects must not take the route with it")
}

// A redirect domain answers for a hostname this app serves. With no route there
// is no such hostname, and writing the list would leave the reconciler with
// redirects pointing nowhere.
func TestUpdateRedirectFromRefusesARoutelessApp(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()
	seedApp(t, dynClient, map[string]interface{}{"image": "ghcr.io/acme/shop:v1"})

	err := d.UpdateRedirectFrom(ctx, "default", "api", []string{"www.example.com"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no route")

	app, _ := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	_, found, _ := unstructured.NestedMap(app.Object, "spec", "route")
	assert.False(t, found, "a refused update must not invent a route")
}

// The flag replaces the list, it does not add to it. Documented as a contract in
// domains.md and the skill files, so it is pinned here rather than left to be
// discovered by someone who adds a second redirect and loses the first.
func TestUpdateRedirectFromReplacesRatherThanAppends(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()
	seedApp(t, dynClient, map[string]interface{}{
		"image": "ghcr.io/acme/shop:v1",
		"route": map[string]interface{}{
			"host":         "example.com",
			"redirectFrom": []interface{}{"www.example.com"},
		},
	})

	require.NoError(t, d.UpdateRedirectFrom(ctx, "default", "api", []string{"old-brand.example"}))

	app, _ := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	got, _, _ := unstructured.NestedStringSlice(app.Object, "spec", "route", "redirectFrom")
	assert.Equal(t, []string{"old-brand.example"}, got,
		"the new list stands alone; keeping www.example.com means passing it again")
}
