package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// kip and the backup tooling find a service's pod by its kipper.run labels, so
// a StatefulSet created before a label rename has to carry the current ones
// down to its pods, or `kip exec` finds no running pod.
func TestReconcileStatefulSet_GivesAnOlderServicesPodsTheCurrentLabels(t *testing.T) {
	ctx := context.Background()
	svc := bareService("postgres")
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc).Build(),
		Scheme: testScheme(),
	}
	require.NoError(t, r.reconcileStatefulSet(ctx, svc))

	var sts appsv1.StatefulSet
	key := types.NamespacedName{Namespace: "ns", Name: "db"}
	require.NoError(t, r.Get(ctx, key, &sts))
	sts.Spec.Template.Labels = map[string]string{
		"app":                          "db",
		"app.kubernetes.io/managed-by": "skipper",
		"legacy.example.com/team":      "data",
	}
	require.NoError(t, r.Update(ctx, &sts))

	require.NoError(t, r.reconcileStatefulSet(ctx, svc))

	require.NoError(t, r.Get(ctx, key, &sts))
	labels := sts.Spec.Template.Labels
	assert.Equal(t, "postgres", labels["kipper.run/service-type"])
	assert.Equal(t, "kipper", labels["app.kubernetes.io/managed-by"])
	assert.Equal(t, "db", labels["app"])
	assert.Equal(t, "data", labels["legacy.example.com/team"], "a label someone else put on the template stays")
}
