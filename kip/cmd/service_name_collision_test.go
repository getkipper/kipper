package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/getkipper/kipper/controller/pkg/secretname"
)

// The CLI refuses what the console refuses, for the same reason and through the
// same predicate: an app named web on the pre-digest name keeps its git token in
// web-git-credentials, which is exactly where a service named web-git would keep
// its credentials.
func TestServiceAddRefusesANameThatSharesAnAppsGitCredential(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", "forge"))

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
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", "forge"))

	for _, name := range []string{"database", "cache", "web", "web-gitlab"} {
		require.NoError(t, refuseServiceNameSharingAnAppCredential(context.Background(), dyn, "shop-prod", name),
			"%q was refused", name)
	}
}
