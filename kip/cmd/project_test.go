package cmd

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/kip/internal/manifest"
)

func TestWriteProjectEnvironments_KeepsQuotaOnSurvivingEnvs(t *testing.T) {
	dyn := newFakeDynBare(t)
	project := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kipper.run/v1alpha1",
			"kind":       "Project",
			"metadata":   map[string]interface{}{"name": "blog"},
			"spec": map[string]interface{}{
				"environments": []interface{}{
					map[string]interface{}{"name": "test"},
					map[string]interface{}{
						"name": "prod",
						"quota": map[string]interface{}{
							"cpuRequest":    "6",
							"cpuLimit":      "12",
							"memoryRequest": "12Gi",
							"memoryLimit":   "24Gi",
						},
					},
				},
			},
		},
	}
	ctx := context.Background()
	_, err := dyn.Resource(manifest.ProjectGVR).Create(ctx, project, metav1.CreateOptions{})
	require.NoError(t, err)

	// Add an environment: prod survives and must keep its quota override.
	err = writeProjectEnvironments(ctx, dyn, project, []string{"test", "prod", "acc"})
	require.NoError(t, err)

	stored, err := dyn.Resource(manifest.ProjectGVR).Get(ctx, "blog", metav1.GetOptions{})
	require.NoError(t, err)
	envs, _, err := unstructured.NestedSlice(stored.Object, "spec", "environments")
	require.NoError(t, err)
	require.Len(t, envs, 3)

	prod, ok := envs[1].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "prod", prod["name"])
	quota, ok := prod["quota"].(map[string]interface{})
	require.True(t, ok, "prod quota override lost by writeProjectEnvironments")
	assert.Equal(t, "6", quota["cpuRequest"])

	acc, ok := envs[2].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "acc", acc["name"])
	_, hasQuota := acc["quota"]
	assert.False(t, hasQuota, "new env must start without a quota override")
}

// The reconciler is what stops two projects sharing a namespace; this is what
// stops the second project being created in the first place, so an operator is
// told before half its namespaces come up with a conflict condition.
func TestNamespaceCollisionIsRefusedBeforeTheProjectIsWritten(t *testing.T) {
	project := func(name string, envs ...string) *unstructured.Unstructured {
		items := make([]interface{}, 0, len(envs))
		for _, e := range envs {
			items = append(items, map[string]interface{}{"name": e})
		}
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "kipper.run/v1alpha1",
			"kind":       "Project",
			"metadata":   map[string]interface{}{"name": name},
			"spec":       map[string]interface{}{"environments": items},
		}}
	}

	tests := []struct {
		name     string
		existing []*unstructured.Unstructured
		project  string
		envs     []string
		refused  bool
	}{
		{
			name:     "a new project takes an existing project's environment namespace",
			existing: []*unstructured.Unstructured{project("shop", "default", "prod")},
			project:  "shop-prod", envs: []string{"default"}, refused: true,
		},
		{
			name:     "a new environment takes an existing project's own namespace",
			existing: []*unstructured.Unstructured{project("shop-prod", "default")},
			project:  "shop", envs: []string{"prod"}, refused: true,
		},
		{
			name:     "no overlap",
			existing: []*unstructured.Unstructured{project("shop", "default", "staging")},
			project:  "blog", envs: []string{"default", "prod"},
		},
		{
			name:     "a project does not collide with itself",
			existing: []*unstructured.Unstructured{project("shop", "default", "prod")},
			project:  "shop", envs: []string{"default", "prod"},
		},
		{name: "the first project on an empty cluster", project: "shop", envs: []string{"default"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dyn := fakeProjectDynamic()
			for _, p := range tt.existing {
				if _, err := dyn.Resource(manifest.ProjectGVR).Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
					t.Fatalf("seeding %s: %v", p.GetName(), err)
				}
			}

			err := refuseNamespaceCollision(context.Background(), dyn, tt.project, tt.envs)
			if tt.refused {
				if err == nil {
					t.Fatal("this project would share a namespace with another one and was allowed")
				}
				if !strings.Contains(err.Error(), "already uses") {
					t.Errorf("the refusal must name the clash, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Errorf("nothing collides, so this must proceed: %v", err)
			}
		})
	}
}

func TestProjectNamespaceMirrorsTheReconciler(t *testing.T) {
	if got := projectNamespace("shop", "default"); got != "shop" {
		t.Errorf("default environment = %q, want the project's own name", got)
	}
	if got := projectNamespace("shop", ""); got != "shop" {
		t.Errorf("empty environment = %q, want the project's own name", got)
	}
	if got := projectNamespace("shop", "prod"); got != "shop-prod" {
		t.Errorf("named environment = %q, want shop-prod", got)
	}
	// The collision this exists for: two different projects, one namespace.
	if projectNamespace("shop", "prod") != projectNamespace("shop-prod", "default") {
		t.Error("this test no longer describes the collision it guards against")
	}
}

// The check has to be wired into the write, not merely exist. This began as a
// bypass — the CLI wrote the Project straight through the dynamic client while
// the check lived only in the console API — so what matters is that a colliding
// project is never persisted, and that the non-colliding one still is.
func TestACollidingProjectIsNeverWritten(t *testing.T) {
	seed := func(dyn dynamic.Interface, name string, envs ...string) {
		items := make([]interface{}, 0, len(envs))
		for _, e := range envs {
			items = append(items, map[string]interface{}{"name": e})
		}
		_, err := dyn.Resource(manifest.ProjectGVR).Create(context.Background(), &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "kipper.run/v1alpha1", "kind": "Project",
				"metadata": map[string]interface{}{"name": name},
				"spec":     map[string]interface{}{"environments": items},
			}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}
	newCR := func(name string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "kipper.run/v1alpha1", "kind": "Project",
			"metadata": map[string]interface{}{"name": name},
			"spec":     map[string]interface{}{"environments": []interface{}{map[string]interface{}{"name": "default"}}},
		}}
	}

	t.Run("creating a colliding project writes nothing", func(t *testing.T) {
		dyn := fakeProjectDynamic()
		seed(dyn, "shop", "default", "prod")

		err := createProjectCR(context.Background(), dyn, newCR("shop-prod"), "shop-prod", []string{"default"})
		if err == nil {
			t.Fatal("a project whose namespace another project uses must be refused")
		}
		if _, getErr := dyn.Resource(manifest.ProjectGVR).Get(context.Background(), "shop-prod", metav1.GetOptions{}); getErr == nil {
			t.Error("the colliding project was written anyway")
		}
	})

	t.Run("creating a project that does not collide still works", func(t *testing.T) {
		dyn := fakeProjectDynamic()
		seed(dyn, "shop", "default")

		if err := createProjectCR(context.Background(), dyn, newCR("blog"), "blog", []string{"default"}); err != nil {
			t.Fatalf("a project that collides with nothing must be created: %v", err)
		}
		if _, err := dyn.Resource(manifest.ProjectGVR).Get(context.Background(), "blog", metav1.GetOptions{}); err != nil {
			t.Errorf("the project was refused despite no collision: %v", err)
		}
	})

	t.Run("adding a colliding environment changes nothing", func(t *testing.T) {
		dyn := fakeProjectDynamic()
		seed(dyn, "shop", "default")
		seed(dyn, "shop-prod", "default")

		project, err := dyn.Resource(manifest.ProjectGVR).Get(context.Background(), "shop", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := addProjectEnvironment(context.Background(), dyn, project, []string{"default", "prod"}, "prod"); err == nil {
			t.Fatal("an environment whose namespace another project uses must be refused")
		}

		after, err := dyn.Resource(manifest.ProjectGVR).Get(context.Background(), "shop", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		envs, _, _ := unstructured.NestedSlice(after.Object, "spec", "environments")
		if len(envs) != 1 {
			t.Errorf("the environment was added anyway: %v", envs)
		}
	})
}

// A project written with no environments still gets one, and its namespace is
// taken. The CLI writes Projects through the dynamic client, so this check is
// the only thing standing between that and a project created onto a name the
// reconciler will refuse.
func TestTheCLICheckSeesAnEnvironmentTheReconcilerWillDefault(t *testing.T) {
	dyn := newFakeDynBare(t)
	ctx := context.Background()
	silent := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kipper.run/v1alpha1",
			"kind":       "Project",
			"metadata":   map[string]interface{}{"name": "shop"},
			"spec":       map[string]interface{}{},
		},
	}
	_, err := dyn.Resource(manifest.ProjectGVR).Create(ctx, silent, metav1.CreateOptions{})
	require.NoError(t, err)

	err = refuseNamespaceCollision(ctx, dyn, "shop-test", []string{"default"})
	require.Error(t, err, "shop-test collides with the namespace shop's defaulted environment occupies")
	assert.Contains(t, err.Error(), "shop-test")
}

// A check that could not run is not a check that passed. Continuing on a listing
// error would write a project onto a name nothing verified, and a project cannot
// be renamed afterwards — whereas the command in front of the operator can just
// be run again.
func TestTheCLIRefusesToWriteWhenTheCheckCannotRun(t *testing.T) {
	dyn := newFakeDynBare(t)
	dyn.PrependReactor("list", "projects", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("the api server is having a moment")
	})

	err := refuseNamespaceCollision(context.Background(), dyn, "shop", []string{"default"})
	require.Error(t, err, "a check that could not run must stop the write")
	assert.Contains(t, err.Error(), "Nothing was created")
}

// A project that declares no environments still runs in one: the reconciler
// substitutes "test" and creates <project>-test, and workloads live there.
//
// loadProjectCR feeds every caller that goes on to write the list back, so
// returning the declared slice meant adding an environment replaced that one
// rather than joining it. The reconciler then built its keep-list from the new
// list, found <project>-test unaccounted for, and deleted the namespace with
// everything in it.
//
// The console handler had the same defect at its own mutation surface. One rule
// now answers for both.
func TestLoadProjectCR_ReportsTheEnvironmentAProjectHasRatherThanDeclares(t *testing.T) {
	dyn := newFakeDynBare(t)
	ctx := context.Background()

	bare := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kipper.run/v1alpha1",
			"kind":       "Project",
			"metadata":   map[string]interface{}{"name": "scratch"},
			"spec":       map[string]interface{}{},
		},
	}
	_, err := dyn.Resource(manifest.ProjectGVR).Create(ctx, bare, metav1.CreateOptions{})
	require.NoError(t, err)

	_, envs, err := loadProjectCR(ctx, dyn, "scratch")
	require.NoError(t, err)
	assert.Equal(t, []string{"test"}, envs,
		"a project declaring nothing still has the environment the reconciler gave it")

	// The shape the defect produced: appending to what was returned, then
	// writing it back, must not drop it.
	require.NoError(t, writeProjectEnvironments(ctx, dyn, bare, append(envs, "acc")))

	stored, err := dyn.Resource(manifest.ProjectGVR).Get(ctx, "scratch", metav1.GetOptions{})
	require.NoError(t, err)
	written, _, err := unstructured.NestedSlice(stored.Object, "spec", "environments")
	require.NoError(t, err)

	names := make([]string, 0, len(written))
	for _, e := range written {
		names = append(names, e.(map[string]interface{})["name"].(string))
	}
	assert.Equal(t, []string{"test", "acc"}, names,
		"the environment the project already had must survive the addition")
}

// A declared list is the whole answer, so nothing is added to a project that
// names its environments explicitly.
func TestLoadProjectCR_LeavesADeclaredListAlone(t *testing.T) {
	dyn := newFakeDynBare(t)
	ctx := context.Background()

	declared := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kipper.run/v1alpha1",
			"kind":       "Project",
			"metadata":   map[string]interface{}{"name": "blog"},
			"spec": map[string]interface{}{
				"environments": []interface{}{map[string]interface{}{"name": "prod"}},
			},
		},
	}
	_, err := dyn.Resource(manifest.ProjectGVR).Create(ctx, declared, metav1.CreateOptions{})
	require.NoError(t, err)

	_, envs, err := loadProjectCR(ctx, dyn, "blog")
	require.NoError(t, err)
	assert.Equal(t, []string{"prod"}, envs, "a project naming only prod has only prod")
}
