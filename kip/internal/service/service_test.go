package service

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

// fakeDynamic builds a dynamic fake client preloaded with a Service CR per
// (name, type) pair, so List/Info tests can exercise the CR-read path.
func fakeDynamicForServices(t *testing.T, namespace string, services map[string]string) *dynamicfake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()
	objects := make([]runtime.Object, 0, len(services))
	for name, svcType := range services {
		objects = append(objects, &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "kipper.run/v1alpha1",
				"kind":       "Service",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"type":    svcType,
					"storage": "1Gi",
				},
				"status": map[string]interface{}{
					"phase": "Running",
				},
			},
		})
	}
	listKinds := map[schema.GroupVersionResource]string{
		manifest.ServiceGVR: "ServiceList",
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, objects...)
}

func TestSupportedTypesContainsAllServices(t *testing.T) {
	types := SupportedTypes()
	assert.Contains(t, types, "postgres")
	assert.Contains(t, types, "mysql")
	assert.Contains(t, types, "mongodb")
	assert.Contains(t, types, "redis")
	assert.Contains(t, types, "rabbitmq")
	assert.Contains(t, types, "opensearch")
	assert.Contains(t, types, "minio")
}

func TestAddPostgresCreatesResources(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // NewClientset requires generated apply configs
	mgr := &Manager{Client: client}
	ctx := context.Background()

	conn, err := mgr.Add(ctx, Options{
		Name:      "mydb",
		Namespace: "default",
		Type:      "postgres",
	})
	require.NoError(t, err)
	assert.Contains(t, conn.Host, "mydb.default.svc.cluster.local")
	assert.Equal(t, int32(5432), conn.Port)
	assert.Equal(t, "kipper", conn.Username)
	assert.NotEmpty(t, conn.Password)
	assert.Equal(t, "app", conn.Database)
	assert.Contains(t, conn.URL, "postgres://")

	// Verify StatefulSet
	ss, err := client.AppsV1().StatefulSets("default").Get(ctx, "mydb", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "postgres:16-alpine", ss.Spec.Template.Spec.Containers[0].Image)
	assert.Equal(t, "postgres", ss.Labels[labels.ServiceType])

	// Verify Service
	svc, err := client.CoreV1().Services("default").Get(ctx, "mydb", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, int32(5432), svc.Spec.Ports[0].Port)

	// Verify credentials Secret has components but no URL
	secret, err := client.CoreV1().Secrets("default").Get(ctx, "mydb-credentials", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "kipper", string(secret.Data["USERNAME"]))
	assert.NotEmpty(t, secret.Data["PASSWORD"])
	assert.NotEmpty(t, secret.Data["HOST"])
	assert.NotEmpty(t, secret.Data["PORT"])
	assert.NotEmpty(t, secret.Data["NAME"])
	assert.Empty(t, secret.Data["url"], "no URL should be stored in credentials secret")
}

func TestAddMySQLCreatesResources(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // NewClientset requires generated apply configs
	mgr := &Manager{Client: client}
	ctx := context.Background()

	conn, err := mgr.Add(ctx, Options{
		Name:      "mydb",
		Namespace: "default",
		Type:      "mysql",
	})
	require.NoError(t, err)
	assert.Equal(t, int32(3306), conn.Port)
	assert.Contains(t, conn.URL, "mysql://")

	ss, _ := client.AppsV1().StatefulSets("default").Get(ctx, "mydb", metav1.GetOptions{})
	assert.Equal(t, "mysql:8-oracle", ss.Spec.Template.Spec.Containers[0].Image)
}

func TestAddMongoDBCreatesResources(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // NewClientset requires generated apply configs
	mgr := &Manager{Client: client}
	ctx := context.Background()

	conn, err := mgr.Add(ctx, Options{
		Name:      "mydb",
		Namespace: "default",
		Type:      "mongodb",
	})
	require.NoError(t, err)
	assert.Equal(t, int32(27017), conn.Port)
	assert.Contains(t, conn.URL, "mongodb://")
}

func TestAddRedisNoCredentials(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // NewClientset requires generated apply configs
	mgr := &Manager{Client: client}
	ctx := context.Background()

	conn, err := mgr.Add(ctx, Options{
		Name:      "cache",
		Namespace: "default",
		Type:      "redis",
	})
	require.NoError(t, err)
	assert.Equal(t, int32(6379), conn.Port)
	assert.Contains(t, conn.URL, "redis://")

	// The name of this test promised this and nothing checked it: redis
	// starts with no --requirepass, so a generated password here is one the
	// server refuses, and every bound workload would receive it.
	secret, err := client.CoreV1().Secrets("default").Get(ctx, "cache-credentials", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Empty(t, secret.Data["PASSWORD"])
	assert.Empty(t, secret.Data["USERNAME"])
	assert.Equal(t, "cache.default.svc.cluster.local", string(secret.Data["HOST"]))
	assert.Equal(t, "6379", string(secret.Data["PORT"]))
}

// TestAddCredentialKeysMatchTheReconciler pins the CLI writer against the same
// rule the console reconciler applies, because either can create a service's
// credentials and a workload bound to it cannot tell which one did. The two
// disagreed before this: the CLI dropped USERNAME and PASSWORD for opensearch
// alone while the reconciler wrote them for every type.
func TestAddCredentialKeysMatchTheReconciler(t *testing.T) {
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
			client := fake.NewSimpleClientset() //nolint:staticcheck // NewClientset requires generated apply configs
			mgr := &Manager{Client: client}
			ctx := context.Background()

			_, err := mgr.Add(ctx, Options{Name: "svc", Namespace: "default", Type: tc.svcType})
			require.NoError(t, err)

			secret, err := client.CoreV1().Secrets("default").Get(ctx, "svc-credentials", metav1.GetOptions{})
			require.NoError(t, err)
			assert.NotEmpty(t, secret.Data["HOST"], "every type carries HOST")
			assert.NotEmpty(t, secret.Data["PORT"], "every type carries PORT")

			_, hasPw := secret.Data["PASSWORD"]
			_, hasUser := secret.Data["USERNAME"]
			assert.Equal(t, tc.wantPw, hasPw, "PASSWORD presence follows whether the server authenticates")
			assert.Equal(t, tc.wantPw, hasUser, "USERNAME travels with PASSWORD")
		})
	}
}

func TestAddRabbitMQCreatesResources(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // NewClientset requires generated apply configs
	mgr := &Manager{Client: client}
	ctx := context.Background()

	conn, err := mgr.Add(ctx, Options{
		Name:      "queue",
		Namespace: "default",
		Type:      "rabbitmq",
	})
	require.NoError(t, err)
	assert.Equal(t, int32(5672), conn.Port)
	assert.Contains(t, conn.URL, "amqp://")

	// Verify management port
	svc, _ := client.CoreV1().Services("default").Get(ctx, "queue", metav1.GetOptions{})
	assert.Len(t, svc.Spec.Ports, 2)
}

func TestAddMinIOCreatesResources(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // NewClientset requires generated apply configs
	mgr := &Manager{Client: client}
	ctx := context.Background()

	conn, err := mgr.Add(ctx, Options{
		Name:      "storage",
		Namespace: "default",
		Type:      "minio",
	})
	require.NoError(t, err)
	assert.Equal(t, int32(9000), conn.Port)
	assert.Contains(t, conn.URL, "http://")

	// Verify console port
	svc, _ := client.CoreV1().Services("default").Get(ctx, "storage", metav1.GetOptions{})
	assert.Len(t, svc.Spec.Ports, 2)

	// Verify credentials carry the S3-shaped keys (endpoint URL + access
	// key + secret key), and none of the generic host/user/pass baseline.
	secret, _ := client.CoreV1().Secrets("default").Get(ctx, "storage-credentials", metav1.GetOptions{})
	assert.Equal(t, "http://storage.default.svc.cluster.local:9000", string(secret.Data["ENDPOINT"]))
	assert.Equal(t, "kipper", string(secret.Data["ACCESS_KEY"]))
	assert.NotEmpty(t, secret.Data["SECRET_KEY"])
	assert.Empty(t, secret.Data["USERNAME"])
	assert.Empty(t, secret.Data["PASSWORD"])
	assert.Empty(t, secret.Data["HOST"])
}

func TestAddOpenSearchNoCredentials(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // NewClientset requires generated apply configs
	mgr := &Manager{Client: client}
	ctx := context.Background()

	conn, err := mgr.Add(ctx, Options{
		Name:      "search",
		Namespace: "default",
		Type:      "opensearch",
	})
	require.NoError(t, err)
	assert.Equal(t, int32(9200), conn.Port)

	secret, _ := client.CoreV1().Secrets("default").Get(ctx, "search-credentials", metav1.GetOptions{})
	assert.Empty(t, secret.Data["USERNAME"])
	assert.Empty(t, secret.Data["NAME"])
}

func TestAddUnsupportedTypeErrors(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // NewClientset requires generated apply configs
	mgr := &Manager{Client: client}
	ctx := context.Background()

	_, err := mgr.Add(ctx, Options{
		Name:      "test",
		Namespace: "default",
		Type:      "oracle",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported service type")
}

func TestAddWithCustomStorage(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // NewClientset requires generated apply configs
	mgr := &Manager{Client: client}
	ctx := context.Background()

	_, _ = mgr.Add(ctx, Options{
		Name:      "bigdb",
		Namespace: "default",
		Type:      "postgres",
		Storage:   "50Gi",
	})

	ss, _ := client.AppsV1().StatefulSets("default").Get(ctx, "bigdb", metav1.GetOptions{})
	storage := ss.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests.Storage().String()
	assert.Equal(t, "50Gi", storage)
}

func TestAddWithResourceLimits(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // NewClientset requires generated apply configs
	mgr := &Manager{Client: client}
	ctx := context.Background()

	_, _ = mgr.Add(ctx, Options{
		Name:        "db",
		Namespace:   "default",
		Type:        "postgres",
		MemoryLimit: "1Gi",
		CPULimit:    "500m",
	})

	ss, _ := client.AppsV1().StatefulSets("default").Get(ctx, "db", metav1.GetOptions{})
	container := ss.Spec.Template.Spec.Containers[0]
	assert.Equal(t, "1Gi", container.Resources.Limits.Memory().String())
	assert.Equal(t, "500m", container.Resources.Limits.Cpu().String())
}

func TestAddWithVersionOverride(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // NewClientset requires generated apply configs
	mgr := &Manager{Client: client}
	ctx := context.Background()

	_, _ = mgr.Add(ctx, Options{
		Name:         "db",
		Namespace:    "default",
		Type:         "postgres",
		ImageVersion: "15",
	})

	ss, _ := client.AppsV1().StatefulSets("default").Get(ctx, "db", metav1.GetOptions{})
	assert.Equal(t, "postgres:15-alpine", ss.Spec.Template.Spec.Containers[0].Image)
}

func TestDeleteRequiresDeleteDataFlag(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // NewClientset requires generated apply configs
	mgr := &Manager{Client: client}
	ctx := context.Background()

	err := mgr.Delete(ctx, "default", "mydb", false)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "delete-data")
}

func TestDeleteWithFlag(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // NewClientset requires generated apply configs
	mgr := &Manager{Client: client}
	ctx := context.Background()

	_, _ = mgr.Add(ctx, Options{Name: "mydb", Namespace: "default", Type: "postgres"})
	err := mgr.Delete(ctx, "default", "mydb", true)
	assert.NoError(t, err)

	_, err = client.AppsV1().StatefulSets("default").Get(ctx, "mydb", metav1.GetOptions{})
	assert.Error(t, err)
}

func TestListReturnsServices(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // NewClientset requires generated apply configs
	dynClient := fakeDynamicForServices(t, "default", map[string]string{
		"db":    "postgres",
		"cache": "redis",
	})
	mgr := &Manager{Client: client, Dynamic: dynClient}
	ctx := context.Background()

	services, err := mgr.List(ctx, "default")
	require.NoError(t, err)
	assert.Len(t, services, 2)
}

func TestInfoReturnsConnectionDetails(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // NewClientset requires generated apply configs
	mgr := &Manager{Client: client}
	ctx := context.Background()

	_, _ = mgr.Add(ctx, Options{Name: "mydb", Namespace: "default", Type: "postgres"})

	conn, err := mgr.Info(ctx, "default", "mydb")
	require.NoError(t, err)
	assert.Contains(t, conn.Host, "mydb.default")
	assert.Equal(t, "kipper", conn.Username)
	assert.NotEmpty(t, conn.Password)
}

func TestInfoErrorsWhenNotFound(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // NewClientset requires generated apply configs
	mgr := &Manager{Client: client}
	ctx := context.Background()

	_, err := mgr.Info(ctx, "default", "nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestUpdateImageVersion(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck
	mgr := &Manager{Client: client}
	ctx := context.Background()

	_, _ = mgr.Add(ctx, Options{Name: "db", Namespace: "default", Type: "postgres"})

	result, err := mgr.Update(ctx, "default", "db", Options{ImageVersion: "15"})
	require.NoError(t, err)
	assert.True(t, result.ImageChanged)
	assert.True(t, result.NeedsRestart)

	ss, _ := client.AppsV1().StatefulSets("default").Get(ctx, "db", metav1.GetOptions{})
	assert.Equal(t, "postgres:15-alpine", ss.Spec.Template.Spec.Containers[0].Image)
}

func TestUpdateResourceLimits(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck
	mgr := &Manager{Client: client}
	ctx := context.Background()

	_, _ = mgr.Add(ctx, Options{Name: "db", Namespace: "default", Type: "postgres"})

	result, err := mgr.Update(ctx, "default", "db", Options{MemoryLimit: "2Gi", CPULimit: "1"})
	require.NoError(t, err)
	assert.True(t, result.ResourcesChanged)
	assert.True(t, result.NeedsRestart)

	ss, _ := client.AppsV1().StatefulSets("default").Get(ctx, "db", metav1.GetOptions{})
	assert.Equal(t, "2Gi", ss.Spec.Template.Spec.Containers[0].Resources.Limits.Memory().String())
}

func TestUpdateNotFoundErrors(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck
	mgr := &Manager{Client: client}
	ctx := context.Background()

	_, err := mgr.Update(ctx, "default", "nonexistent", Options{MemoryLimit: "1Gi"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// A catalog entry names a version and, often, a variant. Applying --version has
// to keep the variant, because postgres:15 and postgres:15-alpine are different
// images — but only where the default tag actually leads with a version. MinIO
// tags a release as RELEASE.2025-09-07T16-13-09Z, whose hyphens sit inside the
// timestamp, so splitting on the first one appended half the old date to the
// new tag.
func TestImageWithVersion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		image   string
		version string
		want    string
	}{
		{"postgres keeps its alpine variant", "postgres:16-alpine", "15", "postgres:15-alpine"},
		{"mysql keeps its oracle variant", "mysql:8-oracle", "9", "mysql:9-oracle"},
		{"redis keeps its alpine variant", "redis:7-alpine", "8", "redis:8-alpine"},
		{"rabbitmq keeps a multi-part variant", "rabbitmq:3-management-alpine", "4", "rabbitmq:4-management-alpine"},
		{"mongo has no variant to keep", "mongo:7", "8", "mongo:8"},
		{"opensearch has no variant to keep", "opensearchproject/opensearch:2", "3", "opensearchproject/opensearch:3"},
		{"alpine floats on a dotted line", "alpine:3.24", "3.23", "alpine:3.23"},
		{"mailhog leads with a v, not a version", "mailhog/mailhog:v1.0.1", "v1.0.2", "mailhog/mailhog:v1.0.2"},
		{
			"minio replaces the whole release tag",
			"minio/minio:RELEASE.2025-09-07T16-13-09Z",
			"RELEASE.2026-01-15T10-00-00Z",
			"minio/minio:RELEASE.2026-01-15T10-00-00Z",
		},
		{"an image with no tag at all still gets one", "someregistry.io/thing", "1.2", "someregistry.io/thing:1.2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, imageWithVersion(tc.image, tc.version))
		})
	}
}

// Every catalog entry has to survive a --version override, since the CLI offers
// one for all of them. This walks the real catalog rather than a copy, so a new
// service with an exotic tag cannot quietly reintroduce the MinIO bug.
func TestImageWithVersion_EveryCatalogEntry(t *testing.T) {
	for serviceType, spec := range catalog {
		t.Run(serviceType, func(t *testing.T) {
			got := imageWithVersion(spec.Image, "9.9.9")
			assert.Contains(t, got, "9.9.9", "the requested version has to appear in the tag")
			base, _, _ := strings.Cut(spec.Image, ":")
			assert.True(t, strings.HasPrefix(got, base+":"), "the image name must not change")
			_, tag, _ := strings.Cut(got, ":")
			assert.NotContains(t, tag, "9.9.9-0", "no fragment of the old tag may be appended to the new version")
		})
	}
}
