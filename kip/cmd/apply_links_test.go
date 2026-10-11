package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/getkipper/kipper/kip/internal/deployer"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

func linkedApp(name, ns string, links ...interface{}) *unstructured.Unstructured {
	spec := map[string]interface{}{"image": "web:1", "port": int64(8080)}
	if len(links) > 0 {
		spec["links"] = links
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1", "kind": "App",
		"metadata": map[string]interface{}{"name": name, "namespace": ns},
		"spec":     spec,
	}}
}

func linkTo(app, ns string) map[string]interface{} {
	return map[string]interface{}{"app": app, "namespace": ns}
}

func TestRefuseUnreadableLinkTargets(t *testing.T) {
	ctx := context.Background()
	dyn := fakeWorkloadDynamic()
	for _, obj := range []*unstructured.Unstructured{
		linkedApp("web", "shop-test", linkTo("ledger", "finance-test")),
		linkedApp("docuseal", "docuseal-test"),
	} {
		_, err := dyn.Resource(deployer.AppGVR).Namespace(obj.GetNamespace()).Create(ctx, obj, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	resource := func(links ...interface{}) []manifest.Resource {
		return []manifest.Resource{{GVR: deployer.AppGVR, Object: linkedApp("web", "shop-test", links...)}}
	}

	assert.NoError(t, refuseUnreadableLinkTargets(ctx, dyn, "shop-test", resource(linkTo("ledger", "finance-test"))),
		"a link that is already live is not checked again")
	assert.NoError(t, refuseUnreadableLinkTargets(ctx, dyn, "shop-test", resource(linkTo("docuseal", "docuseal-test"))))
	assert.NoError(t, refuseUnreadableLinkTargets(ctx, dyn, "shop-test", resource(linkTo("api", "shop-test"))),
		"a target in the manifest's own namespace may be created by the same apply")
	assert.ErrorContains(t, refuseUnreadableLinkTargets(ctx, dyn, "shop-test", resource(linkTo("hidden", "other-test"))), "hidden")
}
