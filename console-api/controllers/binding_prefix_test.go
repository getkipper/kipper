package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

// TestDefaultBindingPrefixMatchesPreview guards the empty-prefix contract:
// a binding that does not set a prefix must resolve to the same env var
// prefix the console preview (InjectedEnvNames) reports. The shared table
// delegates to kipperv1.DefaultBindingPrefix, so a mailhog binding gets
// MAIL_ and an unknown service type gets its uppercased name. A regression
// to a local switch that returned "" for those cases would inject
// unprefixed env vars while the UI still promised MAIL_HOST etc.
func TestDefaultBindingPrefixMatchesPreview(t *testing.T) {
	scheme := testScheme()
	mail := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "inbox", Namespace: "project-test", UID: types.UID("uid-inbox")},
		Spec:       kipperv1.ServiceSpec{Type: "mailhog"},
	}
	kafka := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "events", Namespace: "project-test", UID: types.UID("uid-events")},
		Spec:       kipperv1.ServiceSpec{Type: "kafka"},
	}
	// The prefix is only reported for a binding that is actually injectable, so
	// each service needs the credentials it owns.
	ownedCreds := func(service string) *corev1.Secret {
		controller := true
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: service + "-credentials", Namespace: "project-test",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: kipperv1.GroupVersion.String(), Kind: "Service",
					Name: service, UID: types.UID("uid-" + service), Controller: &controller,
				}},
			},
			Data: map[string][]byte{"HOST": []byte(service + ".project-test.svc")},
		}
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(mail, kafka, ownedCreds("inbox"), ownedCreds("events")).Build()
	ctx := context.Background()

	prefixFor := func(service string) string {
		t.Helper()
		sources, _, err := bindingEnvSources(ctx, c,
			&kipperv1.App{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "project-test"}},
			secretname.KindApp, []kipperv1.ServiceBinding{{Name: service}}, nil)
		require.NoError(t, err)
		if len(sources) == 0 {
			return ""
		}
		return sources[0].prefix
	}

	assert.Equal(t, "MAIL_", prefixFor("inbox"))
	assert.Equal(t, "KAFKA_", prefixFor("events"))
	assert.Equal(t, "", prefixFor("missing"), "a service that is not there yields no prefix, and no binding either")
}
