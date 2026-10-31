package manifest

import (
	"context"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

// TestExportRoundTrip checks that populated fixture fields survive conversion and export.
// Add new manifest fields to the fixtures to include them in this coverage.
func TestExportRoundTrip(t *testing.T) {
	original := &Manifest{
		Project:     "blog",
		Environment: "test",
		Apps: map[string]AppSpec{
			"webapp": fullApp(),
			"docs":   gitApp(),
		},
		Services: map[string]SvcSpec{
			// A tuned service: version and the full, asymmetric
			// request/limit shape must round-trip, or export→apply would
			// revert the tuning to catalog defaults.
			"db": {Type: "postgres", Version: "16", Storage: "10Gi", Resources: &ResourceSpec{
				CPURequest:    "250m",
				CPULimit:      "750m",
				MemoryRequest: "1Gi",
				MemoryLimit:   "2Gi",
			}},
		},
		Volumes: map[string]VolSpec{
			"uploads": {
				Size: "5Gi",
				Mounts: []MountSpec{
					{App: "webapp", MountPath: "/data"},
				},
			},
		},
		Jobs: map[string]JobSpec{
			"nightly": {
				Image:    "ghcr.io/example/cleanup:1.0",
				Schedule: "0 3 * * *",
				Command:  []string{"/bin/cleanup", "--all"},
				Env:      map[string]string{"DRY_RUN": "false"},
			},
		},
		Functions: map[string]FuncSpec{
			"nightly-sync": fullFunction(),
		},
	}

	ctx := context.Background()
	dynClient := fakeClientWithManifest(t, original, "blog-test")

	got, err := Export(ctx, dynClient, "blog", "blog", "test", "blog-test")
	require.NoError(t, err)

	assert.Equal(t, original.Apps["webapp"], got.Apps["webapp"], "webapp App must round-trip with every optional field intact")

	// Omit build output so the exported manifest passes image/git validation on apply.
	assert.Equal(t, original.Apps["docs"], got.Apps["docs"], "git App round-trips git-only, with no image field")

	assert.Equal(t, original.Services["db"], got.Services["db"])
	assert.Equal(t, original.Volumes["uploads"], got.Volumes["uploads"])
	assert.Equal(t, original.Jobs["nightly"], got.Jobs["nightly"])
	assert.Equal(t, original.Functions["nightly-sync"], got.Functions["nightly-sync"], "Function round-trip must preserve source/serviceBindings/CSP/volumes")
}

func TestExportRoundTrip_AppKeepsImageWithoutGit(t *testing.T) {
	original := &Manifest{
		Project: "p", Environment: "e",
		Apps: map[string]AppSpec{
			"img-only": {Image: "ghcr.io/x/y:1", Port: 80},
			"git-only": {Port: 80, Git: &GitSpec{URL: "https://github.com/x/y", Branch: "main"}},
		},
	}

	dynClient := fakeClientWithManifest(t, original, "p-e")
	got, err := Export(context.Background(), dynClient, "p", "p", "e", "p-e")
	require.NoError(t, err)

	img := got.Apps["img-only"]
	assert.Equal(t, "ghcr.io/x/y:1", img.Image)
	assert.Nil(t, img.Git, "image-only App must not gain a git block on round-trip")

	gitApp := got.Apps["git-only"]
	assert.NotNil(t, gitApp.Git)
	assert.Equal(t, "https://github.com/x/y", gitApp.Git.URL)
	// Convert supplies a placeholder image for git apps; export must omit it.
	assert.Empty(t, gitApp.Image, "a git app must export git-only, with no image field")
}

func fullApp() AppSpec {
	return AppSpec{
		Image:    "ghcr.io/example/webapp:2.3.4",
		Port:     8080,
		Replicas: i32(3),
		Env:      map[string]string{"LOG_LEVEL": "info", "REGION": "eu-west-1"},
		SecretRefs: []string{
			"webapp-secrets",
			"shared-api-keys",
		},
		Route: &RouteSpec{
			Host:              "webapp.example.com",
			RedirectFrom:      []string{"www.webapp.example.com", "old.example.com"},
			Path:              "/api",
			Group:             "blog",
			NoSecurityHeaders: true,
			NoInstanceHeader:  true,
			RateLimit:         200,
			CSPAllowlist:      []string{"fonts.googleapis.com", "cdn.example.com"},
			Redirects: []RedirectSpec{
				{Source: "/old", Target: "/new", Permanent: true},
			},
			BasicAuth:     true,
			RequireAPIKey: true,
			InternalPaths: []string{"/admin", "/internal"},
			PublicPaths:   []string{"/actuator/prometheus"},
		},
		Resources: &ResourceSpec{
			Profile:       "jvm",
			CPURequest:    "200m",
			CPULimit:      "1000m",
			MemoryRequest: "512Mi",
			MemoryLimit:   "2Gi",
		},
		ServiceBindings: []BindingSpec{
			{Name: "db", Prefix: "DB_", Database: "webapp_test"},
			{Name: "cache", Prefix: "REDIS_"},
		},
		Volumes: []VolumeMountSpec{
			{Name: "uploads", MountPath: "/data/uploads"},
		},
		Autoscale: &AutoscaleSpec{
			Enabled:      true,
			MinReplicas:  i32(2),
			MaxReplicas:  i32(10),
			CPUTarget:    i32(70),
			MemoryTarget: i32(80),
		},
		Health: &HealthSpec{
			Type:                  "http",
			Path:                  "/actuator/health/readiness",
			Port:                  8081,
			StartupTimeoutSeconds: 600,
			TimeoutSeconds:        3,
		},
		Stopped: &StoppedSpec{Reason: "freeing memory", ForMigration: true},
	}
}

func gitApp() AppSpec {
	return AppSpec{
		Port: 8080,
		Git: &GitSpec{
			URL:               "https://github.com/example/docs",
			Branch:            "main",
			CredentialsSecret: "github-pat",
			DockerfilePath:    "deploy/Dockerfile",
			Context:           "deploy",
			BuildArgs:         map[string]string{"VERSION": "1.2"},
		},
	}
}

func fullFunction() FuncSpec {
	return FuncSpec{
		Image:   "ghcr.io/example/nightly-sync:1",
		Port:    8080,
		Runtime: "node",
		Source: &FuncSourceSpec{
			Code:    "export default async () => 'ok'",
			Handler: "index.js",
			Dependencies: map[string]string{
				"node-fetch": "3.3.2",
			},
		},
		Env: map[string]string{"LOG_LEVEL": "info"},
		Resources: &ResourceSpec{
			CPURequest:    "100m",
			CPULimit:      "500m",
			MemoryRequest: "128Mi",
			MemoryLimit:   "256Mi",
		},
		ServiceBindings: []BindingSpec{
			{Name: "db", Prefix: "DB_"},
		},
		Volumes: []VolumeMountSpec{
			{Name: "shared", MountPath: "/mnt/shared"},
		},
		Triggers: []TriggerSpec{
			{Type: "cron", Schedule: "0 4 * * *"},
			{Type: "http", Config: map[string]string{"path": "/"}},
		},
		NoSecurityHeaders: true,
		CSPAllowlist:      []string{"example.com"},
	}
}

// fakeClientWithManifest seeds the fake client with converted resources so
// round-trip tests exercise both conversion and export.
func fakeClientWithManifest(t *testing.T, m *Manifest, namespace string) *fake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()

	// Register list kinds so the fake client can serve the exporter's List calls.
	gvrToListKind := map[schema.GroupVersionResource]string{
		AppGVR:      "AppList",
		ServiceGVR:  "ServiceList",
		VolumeGVR:   "VolumeList",
		JobGVR:      "JobList",
		FunctionGVR: "FunctionList",
		ProjectGVR:  "ProjectList",
	}

	resources := Convert(m, namespace, nil)
	objs := make([]runtime.Object, 0, len(resources))
	for _, r := range resources {
		// Stamp the managed-by label the exporter selects on.
		md, _ := r.Object.Object["metadata"].(map[string]interface{})
		if md == nil {
			md = map[string]interface{}{}
			r.Object.Object["metadata"] = md
		}
		labels, _ := md["labels"].(map[string]interface{})
		if labels == nil {
			labels = map[string]interface{}{}
			md["labels"] = labels
		}
		labels["app.kubernetes.io/managed-by"] = "kipper"
		objs = append(objs, r.Object)
	}

	// The exporter also reads a Project CR for displayName + environments.
	// Add one so the metadata path is exercised.
	project := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kipper.run/v1alpha1",
			"kind":       "Project",
			"metadata":   map[string]interface{}{"name": m.Project},
			"spec": map[string]interface{}{
				"displayName": "Round-Trip",
				"environments": []interface{}{
					map[string]interface{}{"name": m.Environment},
					map[string]interface{}{"name": "prod"},
				},
			},
		},
	}
	objs = append(objs, project)

	_ = metav1.AddMetaToScheme(scheme)
	return fake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind, objs...)
}

// A stop without a reason is still a stop. Losing the empty block on the way
// through YAML would start the app on the next apply.
func TestStopWithoutAReasonSurvivesYAML(t *testing.T) {
	out, err := yaml.Marshal(AppSpec{Port: 8080, Stopped: &StoppedSpec{}})
	if err != nil {
		t.Fatal(err)
	}
	var back AppSpec
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if back.Stopped == nil {
		t.Fatalf("the stop was lost:\n%s", out)
	}
}

func TestExportToleratesAProjectWithoutSpec(t *testing.T) {
	dynClient := fakeClientWithManifest(t, &Manifest{Project: "other"}, "p-e")
	bare := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1", "kind": "Project",
		"metadata": map[string]interface{}{"name": "p"},
	}}
	_, err := dynClient.Resource(ProjectGVR).Create(context.Background(), bare, metav1.CreateOptions{})
	require.NoError(t, err)

	got, err := Export(context.Background(), dynClient, "p", "p", "e", "p-e")

	require.NoError(t, err)
	assert.Empty(t, got.DisplayName)
	assert.Empty(t, got.Environments)
}
