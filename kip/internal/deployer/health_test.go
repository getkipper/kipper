package deployer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

func i32(v int32) *int32   { return &v }
func str(v string) *string { return &v }

func liveHealth(t *testing.T, d *Deployer) map[string]interface{} {
	t.Helper()
	app, err := d.Dynamic.Resource(AppGVR).Namespace("default").Get(context.Background(), "api", metav1.GetOptions{})
	require.NoError(t, err)
	health, _, _ := unstructured.NestedMap(app.Object, "spec", "health")
	return health
}

func TestUpdateHealth(t *testing.T) {
	tests := []struct {
		name    string
		live    map[string]interface{}
		edit    HealthEdit
		want    map[string]interface{}
		wantErr string
	}{
		{
			name: "declare an http check",
			edit: HealthEdit{Type: "http", Path: str("/actuator/health/readiness"), StartupTimeoutSeconds: i32(600)},
			want: map[string]interface{}{"type": "http", "path": "/actuator/health/readiness", "startupTimeoutSeconds": int64(600)},
		},
		{
			name: "a path alone means http",
			edit: HealthEdit{Path: str("/ready")},
			want: map[string]interface{}{"type": "http", "path": "/ready"},
		},
		{
			name: "a path alone switches a tcp check to http",
			live: map[string]interface{}{"type": "tcp", "port": int64(8081)},
			edit: HealthEdit{Path: str("/ready")},
			want: map[string]interface{}{"type": "http", "path": "/ready", "port": int64(8081)},
		},
		{
			name: "a change merges into the live check",
			live: map[string]interface{}{"type": "http", "path": "/ready", "startupTimeoutSeconds": int64(600)},
			edit: HealthEdit{Port: i32(8081)},
			want: map[string]interface{}{"type": "http", "path": "/ready", "port": int64(8081), "startupTimeoutSeconds": int64(600)},
		},
		{
			name: "a switch to tcp drops the path",
			live: map[string]interface{}{"type": "http", "path": "/ready", "port": int64(8081)},
			edit: HealthEdit{Type: "tcp"},
			want: map[string]interface{}{"type": "tcp", "port": int64(8081)},
		},
		{
			name: "none keeps only the startup timeout",
			live: map[string]interface{}{"type": "http", "path": "/ready", "port": int64(8081), "timeoutSeconds": int64(3), "startupTimeoutSeconds": int64(600)},
			edit: HealthEdit{Type: "none"},
			want: map[string]interface{}{"type": "none", "startupTimeoutSeconds": int64(600)},
		},
		{
			name: "auto clears it",
			live: map[string]interface{}{"type": "tcp"},
			edit: HealthEdit{Type: "auto"},
		},
		{name: "a port with nothing to check", edit: HealthEdit{Port: i32(8081)}, wantErr: "--health tcp or --health http"},
		{name: "http without a path", edit: HealthEdit{Type: "http"}, wantErr: "needs a path"},
		{name: "the instance proxy's port", edit: HealthEdit{Type: "tcp", Port: i32(13000)}, wantErr: "instance proxy"},
		{name: "a startup timeout out of range", edit: HealthEdit{Type: "tcp", StartupTimeoutSeconds: i32(5)}, wantErr: "between 10 and 3600"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, dynClient := testDeployer()
			spec := map[string]interface{}{"image": "ghcr.io/acme/shop:v1", "port": int64(3000)}
			if tt.live != nil {
				spec["health"] = tt.live
			}
			seedApp(t, dynClient, spec)

			err := d.UpdateHealth(context.Background(), "default", "api", tt.edit)

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.Equal(t, tt.live, liveHealth(t, d), "a refused change writes nothing")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, liveHealth(t, d))
		})
	}
}

func TestDeployWritesTheHealthCheck(t *testing.T) {
	d, _ := testDeployer()
	ctx := context.Background()
	opts := Options{Name: "api", Namespace: "default", Image: "ghcr.io/acme/shop:v1", Port: 3000,
		Health: HealthEdit{Type: "tcp", StartupTimeoutSeconds: i32(900)}}
	require.NoError(t, d.Deploy(ctx, opts))
	assert.Equal(t, map[string]interface{}{"type": "tcp", "startupTimeoutSeconds": int64(900)}, liveHealth(t, d))

	opts = Options{Name: "api", Namespace: "default", Image: "ghcr.io/acme/shop:v2", Changed: map[string]bool{"image": true}}
	require.NoError(t, d.Deploy(ctx, opts))
	assert.Equal(t, map[string]interface{}{"type": "tcp", "startupTimeoutSeconds": int64(900)}, liveHealth(t, d), "a redeploy without health flags keeps the check")

	opts = Options{Name: "api", Namespace: "default", Health: HealthEdit{Type: "auto"}}
	require.NoError(t, d.Deploy(ctx, opts))
	assert.Nil(t, liveHealth(t, d), "and --health auto on a redeploy clears it")
}

func TestDeployWithoutHealthFlagsDeclaresNothing(t *testing.T) {
	d, _ := testDeployer()
	require.NoError(t, d.Deploy(context.Background(), Options{Name: "api", Namespace: "default", Image: "ghcr.io/acme/shop:v1", Port: 3000}))
	assert.Nil(t, liveHealth(t, d), "an app with no check declared is automatic")
}

// Detect older CRD schemas that silently discard health settings.
func TestHealthWritesNoticeAnOlderSchema(t *testing.T) {
	prune := func(d *dynamicfake.FakeDynamicClient) {
		d.PrependReactor("*", "apps", func(action k8stesting.Action) (bool, runtime.Object, error) {
			// Create and update actions both carry the object being written.
			write, ok := action.(interface{ GetObject() runtime.Object })
			if !ok {
				return false, nil, nil
			}
			if u, ok := write.GetObject().(*unstructured.Unstructured); ok {
				unstructured.RemoveNestedField(u.Object, "spec", "health")
			}
			return false, nil, nil
		})
	}

	t.Run("update", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "ghcr.io/acme/shop:v1", "port": int64(3000)})
		prune(dynClient)
		err := d.UpdateHealth(context.Background(), "default", "api", HealthEdit{Type: "tcp"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "kip upgrade")
	})
	t.Run("deploy", func(t *testing.T) {
		d, dynClient := testDeployer()
		prune(dynClient)
		err := d.Deploy(context.Background(), Options{Name: "api", Namespace: "default", Image: "ghcr.io/acme/shop:v1", Port: 3000, Health: HealthEdit{Type: "tcp"}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "kip upgrade")
	})
	t.Run("auto needs no field", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "ghcr.io/acme/shop:v1", "port": int64(3000)})
		prune(dynClient)
		require.NoError(t, d.UpdateHealth(context.Background(), "default", "api", HealthEdit{Type: "auto"}))
	})
}
