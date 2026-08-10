package manifest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

// TestExportRoundTrip is the durable regression guard for kip export's
// silent field-drop bug. Every optional field on every CR must survive
// the trip:
//
//	build CR via Convert() → set live in fake apiserver → Export() →
//	compare result against the original Manifest input.
//
// Any field that ends up missing from the round-tripped Manifest is a
// drop bug — either the exporter didn't read the spec key (the original
// acme-tools bug) or the kip-side Manifest type doesn't carry the
// field yet (which is a separate gap to file before merging the new
// field on the controller side).
//
// New CR fields land here BEFORE they ship, not after a migration
// rediscovers them.
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
			"update-tlds": fullFunction(),
		},
	}

	ctx := context.Background()
	dynClient := fakeClientWithManifest(t, original, "blog-test")

	got, err := Export(ctx, dynClient, "blog", "test", "blog-test")
	require.NoError(t, err)

	// Compare per-resource — the maps may iterate in different order
	// and an empty Functions/Jobs/etc. is fine if there was nothing to
	// emit. The interesting test is "what was put in came out".
	assert.Equal(t, original.Apps["webapp"], got.Apps["webapp"], "webapp App must round-trip with every optional field intact")

	// A git app exports git-only: its built image is build output, not manifest
	// state, so export omits it. This keeps export→apply a no-op (a manifest
	// carrying image+git would fail apply's mutual-exclusion check).
	assert.Equal(t, original.Apps["docs"], got.Apps["docs"], "git App round-trips git-only, with no image field")

	assert.Equal(t, original.Services["db"], got.Services["db"])
	assert.Equal(t, original.Volumes["uploads"], got.Volumes["uploads"])
	assert.Equal(t, original.Jobs["nightly"], got.Jobs["nightly"])
	assert.Equal(t, original.Functions["update-tlds"], got.Functions["update-tlds"], "Function round-trip must preserve source/serviceBindings/CSP/volumes")
}

func TestExportRoundTrip_AppKeepsImageWithoutGit(t *testing.T) {
	// Regression for the kip-old `image:`+`git:` mutual-exclusion bug:
	// an image-only App must come out with no git block (and vice versa).
	// Today's exporter reads each independently from the CR, so the
	// failure mode is "CR has both, export emits both" — guard against
	// that by ensuring an image-only input stays image-only.
	original := &Manifest{
		Project: "p", Environment: "e",
		Apps: map[string]AppSpec{
			"img-only": {Image: "ghcr.io/x/y:1", Port: 80},
			"git-only": {Port: 80, Git: &GitSpec{URL: "https://github.com/x/y", Branch: "main"}},
		},
	}

	dynClient := fakeClientWithManifest(t, original, "p-e")
	got, err := Export(context.Background(), dynClient, "p", "e", "p-e")
	require.NoError(t, err)

	img := got.Apps["img-only"]
	assert.Equal(t, "ghcr.io/x/y:1", img.Image)
	assert.Nil(t, img.Git, "image-only App must not gain a git block on round-trip")

	gitApp := got.Apps["git-only"]
	assert.NotNil(t, gitApp.Git)
	assert.Equal(t, "https://github.com/x/y", gitApp.Git.URL)
	// A git app exports git-only. Convert() stamps a busybox placeholder into
	// the CR's required image field, but export omits it: the built image is
	// build output, not manifest state, so a re-applied manifest stays git-only
	// and passes apply's image/git mutual-exclusion check.
	assert.Empty(t, gitApp.Image, "a git app must export git-only, with no image field")
}

// fullApp returns an AppSpec with every optional field populated, so
// the round-trip test fails if any new field gets added to AppSpec but
// the export emitter forgets to read it.
func fullApp() AppSpec {
	return AppSpec{
		Image:    "ghcr.io/example/webapp:2.3.4",
		Port:     8080,
		Replicas: 3,
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
			MinReplicas:  2,
			MaxReplicas:  10,
			CPUTarget:    70,
			MemoryTarget: 80,
		},
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
		Image:   "ghcr.io/example/update-tlds:1",
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

// fakeClientWithManifest builds a dynamic fake client whose store
// contains the resources produced by Convert(manifest, namespace). The
// exporter then sees the same shape it would see against a live
// apiserver, so the round-trip exercises both halves of the pipeline.
func fakeClientWithManifest(t *testing.T, m *Manifest, namespace string) *fake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()

	// The fake client needs to know each resource is a list-kind. The
	// kip CRDs use `<Kind>List` consistently.
	gvrToListKind := map[schema.GroupVersionResource]string{
		AppGVR:      "AppList",
		ServiceGVR:  "ServiceList",
		VolumeGVR:   "VolumeList",
		JobGVR:      "JobList",
		FunctionGVR: "FunctionList",
		ProjectGVR:  "ProjectList",
	}

	resources := Convert(m, namespace)
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
