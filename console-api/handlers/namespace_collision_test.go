package handlers

import (
	"context"
	goerrors "errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/controllers"
)

func projectWithEnvs(name string, envs ...string) *kipperv1.Project {
	p := &kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: name}}
	for _, e := range envs {
		p.Spec.Environments = append(p.Spec.Environments, kipperv1.ProjectEnvironment{Name: e})
	}
	return p
}

// The reconciler refusing to adopt another project's namespace contains the
// damage. This is what stops the collision being created: two projects whose
// environments resolve to one namespace name must not both exist.
func TestCollidingProjectNamespacesAreRefused(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, kipperv1.AddToScheme(scheme))

	tests := []struct {
		name     string
		existing []*kipperv1.Project
		project  string
		envs     []string
		refused  bool
	}{
		{
			name:     "a new project collides with an existing project's environment",
			existing: []*kipperv1.Project{projectWithEnvs("shop", "default", "prod")},
			project:  "shop-prod", envs: []string{"default"}, refused: true,
		},
		{
			name:     "a new environment collides with an existing project's name",
			existing: []*kipperv1.Project{projectWithEnvs("shop-prod", "default")},
			project:  "shop", envs: []string{"prod"}, refused: true,
		},
		{
			name:     "no overlap",
			existing: []*kipperv1.Project{projectWithEnvs("shop", "default", "staging")},
			project:  "blog", envs: []string{"default", "prod"},
		},
		{
			name:     "a project does not collide with itself",
			existing: []*kipperv1.Project{projectWithEnvs("shop", "default", "prod")},
			project:  "shop", envs: []string{"default", "prod"},
		},
		{
			name:    "the first project on an empty cluster",
			project: "shop", envs: []string{"default"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := crfake.NewClientBuilder().WithScheme(scheme)
			for _, p := range tt.existing {
				builder = builder.WithObjects(p)
			}
			handler := &Projects{Client: fake.NewClientset(), CRClient: builder.Build()}

			err := handler.refuseNamespaceCollision(context.Background(), tt.project, tt.envs)
			if tt.refused {
				require.Error(t, err, "this project would share a namespace with another one")
				assert.Contains(t, err.Error(), "already uses")
				return
			}
			assert.NoError(t, err)
		})
	}
}

// The check resolves namespaces with the reconciler's own function rather than a
// copy of the rule, because a copy would guard names nothing uses the moment the
// two drifted. This pins the shape the collision check depends on.
func TestNamespacesResolveTheWayTheReconcilerCreatesThem(t *testing.T) {
	assert.Equal(t, "shop", controllers.ResolveNamespace("shop", "default"))
	assert.Equal(t, "shop", controllers.ResolveNamespace("shop", ""))
	assert.Equal(t, "shop-prod", controllers.ResolveNamespace("shop", "prod"))
}

// The check reads the other project's environments as the reconciler will build
// them. A project written with an empty list still gets a namespace, and a check
// that cannot see it approves a name that namespace already occupies — which the
// reconcile then refuses, after the project exists.
func TestTheCheckSeesAnEnvironmentTheReconcilerWillDefault(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, kipperv1.AddToScheme(scheme))

	// Declares nothing, so the reconciler gives it "test" and namespace shop-test.
	silent := &kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}
	p := &Projects{Client: fake.NewClientset(), CRClient: crfake.NewClientBuilder().WithScheme(scheme).WithObjects(silent).Build()}

	err := p.refuseNamespaceCollision(context.Background(), "shop-test", []string{"default"})
	require.Error(t, err, "shop-test collides with the namespace shop's defaulted environment occupies")

	var collision *namespaceCollisionError
	assert.ErrorAs(t, err, &collision, "an overlap is the caller's to fix, and must read as one")
}

// A check that could not run is not a check that passed. Reporting an API
// failure as a conflict tells the client to rename over what a retry would have
// cleared, and the name it would abandon was never the problem.
func TestACheckThatCouldNotRunIsNotAConflict(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, kipperv1.AddToScheme(scheme))

	failing := crfake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c crclient.WithWatch, list crclient.ObjectList, opts ...crclient.ListOption) error {
			return fmt.Errorf("the api server is having a moment")
		},
	}).Build()
	p := &Projects{Client: fake.NewClientset(), CRClient: failing}

	err := p.refuseNamespaceCollision(context.Background(), "shop", []string{"default"})
	require.Error(t, err)

	var collision *namespaceCollisionError
	assert.False(t, goerrors.As(err, &collision), "a failed check must not read as an overlap")

	rec := httptest.NewRecorder()
	respondNamespaceCollision(rec, err)
	assert.Equal(t, http.StatusInternalServerError, rec.Code, "a failed check is ours to fix, so 500")
	assert.NotContains(t, rec.Body.String(), "api server is having a moment",
		"the internal error text stays server-side")

	rec = httptest.NewRecorder()
	respondNamespaceCollision(rec, &namespaceCollisionError{msg: "shop-test is taken"})
	assert.Equal(t, http.StatusConflict, rec.Code, "an actual overlap is still 409")
	assert.Contains(t, rec.Body.String(), "shop-test is taken")
}

// Five places built an app's subdomain from the environment: the reconciler
// that serves it, and four handlers that report or reserve it. They each had
// their own copy of the rule, so correcting the reconciler's would have left
// the other four answering "web-default" for a namespace serving "web" — the
// same disagreement in a new direction.
func TestEverySurfaceBuildsTheSameHostForADefaultEnvironment(t *testing.T) {
	tests := []struct{ env, want string }{
		{"", "web"},
		{"default", "web"},
		{"prod", "web-prod"},
		{"test", "web-test"},
	}
	for _, tt := range tests {
		t.Run("env="+tt.env, func(t *testing.T) {
			assert.Equal(t, tt.want, controllers.AppHostPrefix("web", tt.env))
		})
	}

	// And the namespace an environment resolves to agrees with the host it
	// serves on, which is the property that was broken.
	assert.Equal(t, "shop", controllers.ResolveNamespace("shop", "default"))
	assert.Equal(t, "web", controllers.AppHostPrefix("web", "default"))
}

// Promote composes namespaces from the environment names in its request body.
// The gate in front of it resolved the caller against the project in the path,
// so an unchecked name is a way to act somewhere the gate never authorised:
// project "shop" declaring only "test" can name "prod" and land on "shop-prod",
// which belongs to the project of that name.
func TestPromoteRefusesAnEnvironmentTheProjectDoesNotDeclare(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, kipperv1.AddToScheme(scheme))
	// The namespaces each project actually holds. Resolving an environment now
	// establishes ownership as well as declaration, so a fixture without them
	// would be asserting against a cluster where neither project has anything.
	p := &Projects{
		Client: fake.NewClientset(
			newKipperNamespace("shop-test", "shop", "test", "0"),
			newKipperNamespace("shop-prod", "shop-prod", "default", "0"),
		),
		CRClient: crfake.NewClientBuilder().WithScheme(scheme).WithObjects(
			projectWithEnvs("shop", "test"),
			projectWithEnvs("shop-prod", "default"),
		).Build(),
	}

	_, err := p.projectEnvironmentNamespace(context.Background(), "shop", "prod")
	require.Error(t, err, "shop does not declare prod, so shop-prod is not shop's to reach")
	assert.Contains(t, err.Error(), "no environment")

	ns, err := p.projectEnvironmentNamespace(context.Background(), "shop", "test")
	require.NoError(t, err)
	assert.Equal(t, "shop-test", ns)

	// And a default environment resolves to the project's own name, not to a
	// suffixed one that never existed — promoting to it used to silently do
	// nothing and report success.
	ns, err = p.projectEnvironmentNamespace(context.Background(), "shop-prod", "default")
	require.NoError(t, err)
	assert.Equal(t, "shop-prod", ns)
}
