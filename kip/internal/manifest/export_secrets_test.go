package manifest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/controller/pkg/secretname"
)

func userSecrets(name string, keys ...string) *unstructured.Unstructured {
	data := map[string]any{}
	for _, k := range keys {
		data[k] = "c3ludGhldGlj"
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": name, "namespace": "shop-prod"},
		"data":     data,
	}}
}

func TestExportNamesTheSecretsItLeavesOut(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(),
		userSecrets(secretname.Secrets(secretname.KindApp, "web"), "STRIPE_KEY", "API_TOKEN", "API_TOKEN.__previous"),
		userSecrets(secretname.Secrets(secretname.KindFunction, "hook"), "SIGNING_KEY"))
	m := &Manifest{}

	noteSecrets(context.Background(), dyn, "shop-prod", secretname.KindApp, "web", m)
	noteSecrets(context.Background(), dyn, "shop-prod", secretname.KindFunction, "hook", m)
	noteSecrets(context.Background(), dyn, "shop-prod", secretname.KindApp, "plain", m)

	assert.Equal(t, []string{
		`app "web": the export omits the values of secrets API_TOKEN, STRIPE_KEY. Set them again with kip app secret set.`,
		`function "hook": the export omits the values of secret SIGNING_KEY. Set it again with kip function secret set.`,
	}, m.Notes)
}

func TestExportSaysWhenItCannotReadTheSecrets(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	dyn.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(secretsGVR.GroupResource(), "app-web-secrets", nil)
	})
	m := &Manifest{}

	noteSecrets(context.Background(), dyn, "shop-prod", secretname.KindApp, "web", m)

	assert.Len(t, m.Notes, 1)
	assert.Contains(t, m.Notes[0], `app "web": kip could not read its secrets, so the list of omitted secret keys may be incomplete`)
}
