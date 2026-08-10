package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func rollup(namespace, name, day string) *kipperv1.UsageRollup {
	return &kipperv1.UsageRollup{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       kipperv1.UsageRollupSpec{KeyPrefix: "kp_abcdef", Day: day},
	}
}

func TestSweepExpiredRollups(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, kipperv1.AddToScheme(scheme))

	recentDay := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	expiredDay := time.Now().UTC().AddDate(0, 0, -(rollupRetentionDays + 10)).Format("2006-01-02")

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		rollup("shop-prod", "kp-abcdef-"+expiredDay, expiredDay),
		rollup("shop-prod", "kp-abcdef-"+recentDay, recentDay),
		// The authz freshness canary is permanently older than any cutoff and
		// must survive every sweep — deleting it stalls the authz freshness
		// clock and fails all key-gated routes closed.
		rollup(authzCanaryNamespace, authzCanaryName, "2000-01-01"),
	).Build()

	sweepExpiredRollups(context.Background(), c)

	var remaining kipperv1.UsageRollupList
	require.NoError(t, c.List(context.Background(), &remaining))

	names := map[string]bool{}
	for _, r := range remaining.Items {
		names[r.Namespace+"/"+r.Name] = true
	}
	assert.False(t, names["shop-prod/kp-abcdef-"+expiredDay], "expired rollup must be deleted")
	assert.True(t, names["shop-prod/kp-abcdef-"+recentDay], "recent rollup must be kept")
	assert.True(t, names[authzCanaryNamespace+"/"+authzCanaryName], "authz freshness canary must never be swept")

	var canary kipperv1.UsageRollup
	assert.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: authzCanaryNamespace, Name: authzCanaryName}, &canary))
}
