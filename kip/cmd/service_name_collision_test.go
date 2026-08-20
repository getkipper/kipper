package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/getkipper/kipper/controller/pkg/secretname"
	"github.com/getkipper/kipper/kip/internal/deployer"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

// The CLI refuses what the console refuses, for the same reason and through the
// same predicate: an app named web on the pre-digest name keeps its git token in
// web-git-credentials, which is exactly where a service named web-git would keep
// its credentials.
func TestServiceAddRefusesANameThatSharesAnAppsGitCredential(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", secretname.LegacyGitCredential("web")))

	err := refuseServiceNameSharingAnAppCredential(context.Background(), dyn, "shop-prod", "web-git")

	require.Error(t, err)
	assert.Contains(t, err.Error(), secretname.ServiceCredentials("web-git"))
	assert.Contains(t, err.Error(), "web")
}

func TestServiceAddAllowsTheNameWhenNoSuchAppExists(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme())

	require.NoError(t, refuseServiceNameSharingAnAppCredential(context.Background(), dyn, "shop-prod", "web-git"))
}

func TestServiceAddDoesNotCheckANameThatCannotCollide(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", secretname.LegacyGitCredential("web")))

	for _, name := range []string{"database", "cache", "web", "web-gitlab"} {
		require.NoError(t, refuseServiceNameSharingAnAppCredential(context.Background(), dyn, "shop-prod", name),
			"%q was refused", name)
	}
}

// A manifest is a create path too, and the comment in applyResource says so: it
// is the one place that reaches every kind, so a kipper.yaml must not be able to
// create what `kip service add` and the console both refuse.
func TestApplyRefusesAServiceNameThatSharesAnAppsGitCredential(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", secretname.LegacyGitCredential("web")))
	res := manifest.Resource{
		GVR: manifest.ServiceGVR,
		Object: &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": manifest.ServiceGVR.GroupVersion().String(),
			"kind":       "Service",
			"metadata":   map[string]any{"name": "web-git", "namespace": "shop-prod"},
			"spec":       map[string]any{"type": "postgres"},
		}},
	}

	_, err := applyResource(context.Background(), dyn, "shop-prod", res, false, nil, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), secretname.ServiceCredentials("web-git"))
}

// The app existing is not the collision. An app on a digest-named credential has
// nothing at the object the service would take, and no writer can put anything
// there, so the name is free.
func TestServiceAddAllowsTheNameWhenTheAppIsOnADigestCredential(t *testing.T) {
	digest := secretname.GitCredential("web", secretname.GitCredentialDigest("a-token", "git.example.com"))
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", digest))

	require.NoError(t, refuseServiceNameSharingAnAppCredential(context.Background(), dyn, "shop-prod", "web-git"))
}

// And an app with no git source at all has no token to collide with.
func TestServiceAddAllowsTheNameWhenTheAppHasNoGitSource(t *testing.T) {
	app := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": deployer.AppGVR.GroupVersion().String(),
		"kind":       "App",
		"metadata":   map[string]any{"name": "web", "namespace": "shop-prod"},
		"spec":       map[string]any{"image": "nginx", "port": int64(80)},
	}}
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), app)

	require.NoError(t, refuseServiceNameSharingAnAppCredential(context.Background(), dyn, "shop-prod", "web-git"))
}
