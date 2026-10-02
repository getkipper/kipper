package controllers

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

var updateDeploymentGolden = flag.Bool("update-deployment-golden", false, "rewrite the Deployment golden files")

// legacyFixtures covers app configurations whose base pod templates must
// remain stable across the rollout changes.
func legacyFixtures() map[string]*kipperv1.App {
	app := func(mutate func(*kipperv1.App)) *kipperv1.App {
		a := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "project-test", UID: "uid-shop"},
			Spec:       kipperv1.AppSpec{Image: "registry.example.com/shop:1.2.3", Port: 8080},
		}
		mutate(a)
		return a
	}
	two := int32(2)
	return map[string]*kipperv1.App{
		"routed-with-sidecar": app(func(a *kipperv1.App) {
			a.Spec.Replicas = &two
			a.Spec.Route = &kipperv1.AppRoute{}
		}),
		"route-less": app(func(a *kipperv1.App) {}),
		"no-instance-header": app(func(a *kipperv1.App) {
			a.Spec.Route = &kipperv1.AppRoute{NoInstanceHeader: true}
		}),
		"git-placeholder": app(func(a *kipperv1.App) {
			a.Spec.Image = "busybox:latest"
			a.Spec.Route = &kipperv1.AppRoute{}
			a.Spec.Git = &kipperv1.AppGitSource{URL: "https://git.example.com/shop.git"}
		}),
		"volumes-and-profile": app(func(a *kipperv1.App) {
			a.Spec.Route = &kipperv1.AppRoute{}
			a.Spec.Resources = kipperv1.AppResources{Profile: "jvm"}
			a.Spec.Volumes = []kipperv1.AppVolumeMount{{Name: "uploads", MountPath: "/data"}}
		}),
	}
}

func renderedDeployment(t *testing.T, app *kipperv1.App) []byte {
	t.Helper()
	ctx := context.Background()
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(app).Build()
	r := &AppReconciler{Client: c, Scheme: testScheme(), SidecarImage: "ghcr.io/getkipper/kipper-sidecar:latest"}
	dep, err := r.buildDeployment(ctx, app, nil, "gen-1", "")
	require.NoError(t, err)
	out, err := yaml.Marshal(&dep.Spec.Template)
	require.NoError(t, err)
	return out
}

// Changes to the base template can restart existing apps during an upgrade.
func TestLegacyDeploymentIsUnchanged(t *testing.T) {
	for name, app := range legacyFixtures() {
		t.Run(name, func(t *testing.T) {
			got := renderedDeployment(t, app)
			path := filepath.Join("testdata", "deployment-golden", name+".yaml")
			if *updateDeploymentGolden {
				require.NoError(t, os.WriteFile(path, got, 0o600))
				return
			}
			want, err := os.ReadFile(path)
			require.NoError(t, err, "missing golden file; run with -update-deployment-golden on the commit before the change")
			require.Equal(t, string(want), string(got))
		})
	}
}
