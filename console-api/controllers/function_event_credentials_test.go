package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func TestLoadServiceConnReadsOnlyTheServicesOwnCredentials(t *testing.T) {
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop", UID: types.UID("svc-uid")},
		Spec:       kipperv1.ServiceSpec{Type: "postgres"},
	}
	secret := func(owned bool) *corev1.Secret {
		s := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: "shop"},
			Data:       map[string][]byte{"HOST": []byte("db.shop"), "PORT": []byte("5432"), "USERNAME": []byte("app"), "PASSWORD": []byte("pw")},
		}
		if owned {
			yes := true
			s.OwnerReferences = []metav1.OwnerReference{{APIVersion: "kipper.run/v1alpha1", Kind: "Service", Name: "db", UID: svc.UID, Controller: &yes}}
		}
		return s
	}
	fn := &kipperv1.Function{ObjectMeta: metav1.ObjectMeta{Name: "on-insert", Namespace: "shop"}}

	for name, owned := range map[string]bool{"owned": true, "not owned": false} {
		c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc.DeepCopy(), secret(owned)).Build()
		r := &FunctionReconciler{Client: c, Scheme: testScheme()}

		conn, err := r.loadServiceConn(context.Background(), fn, "shop", "db", nil)

		if owned {
			require.NoError(t, err, name)
			assert.Equal(t, "pw", conn.Password, name)
			continue
		}
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "db-credentials Secret is missing or is not owned", name)
		assert.Nil(t, conn, name)
	}
}

func TestFunctionReconcileStopsWhenTheEventSourceCredentialsAreNotOwned(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "default", UID: types.UID("svc-uid")},
		Spec:       kipperv1.ServiceSpec{Type: "postgres"},
	}
	unowned := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: "default"},
		Data:       map[string][]byte{"HOST": []byte("db"), "PASSWORD": []byte("pw")},
	}
	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "on-insert", Namespace: "default"},
		Spec: kipperv1.FunctionSpec{
			Image:    "on-insert:v1",
			Port:     8080,
			Triggers: []kipperv1.FunctionTrigger{{Type: "postgres", Config: map[string]string{"source": "db", "table": "orders"}}},
		},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn, svc, unowned).WithStatusSubresource(fn).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "on-insert", Namespace: "default"}})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip service credentials --project default --repair")
	var dep appsv1.Deployment
	assert.True(t, apierrors.IsNotFound(c.Get(ctx, types.NamespacedName{Name: "on-insert", Namespace: "default"}, &dep)), "no Deployment without its poller")
}
