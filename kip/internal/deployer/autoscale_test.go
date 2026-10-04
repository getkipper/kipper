package deployer

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/controller/pkg/capacity"
)

func liveSpec(t *testing.T, dynClient *dynamicfake.FakeDynamicClient) map[string]interface{} {
	t.Helper()
	app, err := dynClient.Resource(AppGVR).Namespace("default").Get(context.Background(), "api", metav1.GetOptions{})
	require.NoError(t, err)
	spec, _, _ := unstructured.NestedMap(app.Object, "spec")
	return spec
}

func seedDeployment(t *testing.T, d *Deployer, replicas int32) {
	t.Helper()
	_, err := d.Client.AppsV1().Deployments("default").Create(context.Background(), &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
}

func TestEnableAutoscale(t *testing.T) {
	ctx := context.Background()

	t.Run("a first enable writes the spec with the defaults and touches nothing else", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "web:1", "replicas": int64(2)})
		client := d.Client.(*fake.Clientset)
		before := len(client.Actions())

		res, err := d.EnableAutoscale(ctx, "default", "api", AutoscaleChange{CPUTarget: i32(80)})

		require.NoError(t, err)
		assert.Equal(t, map[string]interface{}{"enabled": true, "minReplicas": int64(1), "maxReplicas": int64(5), "cpuTarget": int64(80)},
			liveSpec(t, dynClient)["autoscale"])
		assert.Empty(t, client.Actions()[before:], "the CLI writes no HPA and no Deployment; the reconciler builds the autoscaler")
		assert.Nil(t, res.ReplicasMoved)
	})

	t.Run("a flag that is not given keeps the stored value", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "web:1", "replicas": int64(3),
			"autoscale": map[string]interface{}{"enabled": false, "minReplicas": int64(2), "maxReplicas": int64(10), "cpuTarget": int64(70)}})

		_, err := d.EnableAutoscale(ctx, "default", "api", AutoscaleChange{CPUTarget: i32(80)})

		require.NoError(t, err)
		assert.Equal(t, map[string]interface{}{"enabled": true, "minReplicas": int64(2), "maxReplicas": int64(10), "cpuTarget": int64(80)},
			liveSpec(t, dynClient)["autoscale"])
	})

	t.Run("the CPU default is added only when no target exists anywhere", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "web:1", "replicas": int64(2),
			"autoscale": map[string]interface{}{"enabled": false, "minReplicas": int64(1), "maxReplicas": int64(4), "memoryTarget": int64(80)}})

		_, err := d.EnableAutoscale(ctx, "default", "api", AutoscaleChange{})

		require.NoError(t, err)
		as := liveSpec(t, dynClient)["autoscale"].(map[string]interface{})
		assert.NotContains(t, as, "cpuTarget", "a stored memory-only policy must not gain a CPU target")
		assert.Equal(t, int64(80), as["memoryTarget"])
	})

	t.Run("a target of 0 is removed", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "web:1", "replicas": int64(2),
			"autoscale": map[string]interface{}{"enabled": true, "minReplicas": int64(1), "maxReplicas": int64(4), "cpuTarget": int64(70), "memoryTarget": int64(80)}})

		_, err := d.EnableAutoscale(ctx, "default", "api", AutoscaleChange{MemoryTarget: i32(0)})

		require.NoError(t, err)
		assert.NotContains(t, liveSpec(t, dynClient)["autoscale"], "memoryTarget")
	})

	t.Run("new bounds move the stored count into them", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "web:1"})

		res, err := d.EnableAutoscale(ctx, "default", "api", AutoscaleChange{MinReplicas: i32(2), MaxReplicas: i32(5), CPUTarget: i32(70)})

		require.NoError(t, err)
		assert.Equal(t, int64(2), liveSpec(t, dynClient)["replicas"])
		require.NotNil(t, res.ReplicasMoved)
		assert.Equal(t, [2]int32{1, 2}, *res.ReplicasMoved)
	})

	t.Run("an invalid policy is refused and nothing is written", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "web:1", "replicas": int64(2)})

		_, err := d.EnableAutoscale(ctx, "default", "api", AutoscaleChange{MinReplicas: i32(6), MaxReplicas: i32(5), CPUTarget: i32(70)})

		require.ErrorContains(t, err, "minReplicas (6) must not exceed maxReplicas (5)")
		assert.NotContains(t, liveSpec(t, dynClient), "autoscale")
	})

	t.Run("a stopped app stores the policy and says so", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "web:1", "replicas": int64(2),
			"stopped": map[string]interface{}{"reason": "testing"}})

		res, err := d.EnableAutoscale(ctx, "default", "api", AutoscaleChange{CPUTarget: i32(70)})

		require.NoError(t, err)
		assert.True(t, res.Stopped)
		assert.Equal(t, true, liveSpec(t, dynClient)["autoscale"].(map[string]interface{})["enabled"])
	})
}

func TestDisableAutoscale(t *testing.T) {
	ctx := context.Background()
	enabled := map[string]interface{}{"enabled": true, "minReplicas": int64(2), "maxReplicas": int64(5), "cpuTarget": int64(70)}

	t.Run("a running app keeps the count the autoscaler set", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "web:1", "replicas": int64(2), "autoscale": enabled})
		seedDeployment(t, d, 4)

		res, err := d.DisableAutoscale(ctx, "default", "api")

		require.NoError(t, err)
		spec := liveSpec(t, dynClient)
		assert.Equal(t, int64(4), spec["replicas"])
		assert.Equal(t, false, spec["autoscale"].(map[string]interface{})["enabled"])
		assert.Equal(t, int64(2), spec["autoscale"].(map[string]interface{})["minReplicas"], "the bounds stay")
		assert.Equal(t, int32(4), res.Replicas)
	})

	t.Run("an app that is already off is left alone", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "web:1", "replicas": int64(3),
			"autoscale": map[string]interface{}{"enabled": false, "minReplicas": int64(2), "maxReplicas": int64(5)}})
		seedDeployment(t, d, 3)

		res, err := d.DisableAutoscale(ctx, "default", "api")

		require.NoError(t, err)
		assert.True(t, res.AlreadyOff)
		assert.Equal(t, int64(3), liveSpec(t, dynClient)["replicas"])
	})

	t.Run("a stopped app keeps its stored count", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "web:1", "replicas": int64(3), "autoscale": enabled,
			"stopped": map[string]interface{}{"reason": "testing"}})
		seedDeployment(t, d, 0)

		res, err := d.DisableAutoscale(ctx, "default", "api")

		require.NoError(t, err)
		assert.Equal(t, int64(3), liveSpec(t, dynClient)["replicas"])
		assert.True(t, res.Stopped)
	})

	t.Run("an unreadable running count leaves the stored count and says so", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "web:1", "replicas": int64(3), "autoscale": enabled})

		res, err := d.DisableAutoscale(ctx, "default", "api")

		require.NoError(t, err)
		assert.Equal(t, int64(3), liveSpec(t, dynClient)["replicas"])
		assert.True(t, res.CountUnknown)
		assert.Equal(t, false, liveSpec(t, dynClient)["autoscale"].(map[string]interface{})["enabled"])
	})
}

func TestRemoveAutoscale(t *testing.T) {
	ctx := context.Background()

	t.Run("bounds of a policy that is off are removed", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "web:1", "replicas": int64(8),
			"autoscale": map[string]interface{}{"enabled": false, "minReplicas": int64(2), "maxReplicas": int64(5)}})

		require.NoError(t, d.RemoveAutoscale(ctx, "default", "api"))
		assert.NotContains(t, liveSpec(t, dynClient), "autoscale")
		assert.Equal(t, int64(8), liveSpec(t, dynClient)["replicas"])
	})

	t.Run("a policy that is on must be switched off first", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "web:1",
			"autoscale": map[string]interface{}{"enabled": true, "maxReplicas": int64(5), "cpuTarget": int64(70)}})

		require.ErrorContains(t, d.RemoveAutoscale(ctx, "default", "api"), "--off")
		assert.Contains(t, liveSpec(t, dynClient), "autoscale")
	})
}

func TestScaleStaysWithinTheBounds(t *testing.T) {
	ctx := context.Background()
	d, dynClient := testDeployer()
	seedApp(t, dynClient, map[string]interface{}{"image": "web:1", "replicas": int64(3),
		"autoscale": map[string]interface{}{"enabled": false, "minReplicas": int64(2), "maxReplicas": int64(5)}})

	require.ErrorContains(t, d.Scale(ctx, "default", "api", 8), "replicas (8) must be between 2 and 5")
	assert.Equal(t, int64(3), liveSpec(t, dynClient)["replicas"])
	require.NoError(t, d.Scale(ctx, "default", "api", 4))
	assert.Equal(t, int64(4), liveSpec(t, dynClient)["replicas"])
}

func TestDeployReplicasStayWithinTheBounds(t *testing.T) {
	ctx := context.Background()
	d, dynClient := testDeployer()
	seedApp(t, dynClient, map[string]interface{}{"image": "ghcr.io/acme/api:v1", "replicas": int64(3),
		"autoscale": map[string]interface{}{"enabled": false, "minReplicas": int64(2), "maxReplicas": int64(5)}})

	err := d.Deploy(ctx, Options{Name: "api", Namespace: "default", Image: "ghcr.io/acme/api:v1", Replicas: 8,
		Changed: map[string]bool{"replicas": true}})
	require.ErrorContains(t, err, "replicas (8) must be between 2 and 5")
	assert.Equal(t, int64(3), liveSpec(t, dynClient)["replicas"])
}

func TestAutoscaled(t *testing.T) {
	d, dynClient := testDeployer()
	seedApp(t, dynClient, map[string]interface{}{"image": "web:1",
		"autoscale": map[string]interface{}{"enabled": true, "maxReplicas": int64(5), "cpuTarget": int64(70)}})
	on, err := d.Autoscaled(context.Background(), "default", "api")
	require.NoError(t, err)
	assert.True(t, on)
}

func TestAutoscaleReviewFixes(t *testing.T) {
	ctx := context.Background()

	t.Run("deploy refuses an explicit replica count of 0 or below when bounds exist", func(t *testing.T) {
		for _, n := range []int32{0, -1} {
			d, dynClient := testDeployer()
			seedApp(t, dynClient, map[string]interface{}{"image": "ghcr.io/acme/api:v1", "replicas": int64(3),
				"autoscale": map[string]interface{}{"enabled": false, "minReplicas": int64(1), "maxReplicas": int64(5)}})
			err := d.Deploy(ctx, Options{Name: "api", Namespace: "default", Image: "ghcr.io/acme/api:v1", Replicas: n,
				Changed: map[string]bool{"replicas": true}})
			require.Error(t, err, "replicas %d", n)
			assert.Equal(t, int64(3), liveSpec(t, dynClient)["replicas"])
		}
	})

	t.Run("deploy and scale refuse a negative count without bounds", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "ghcr.io/acme/api:v1", "replicas": int64(3)})
		err := d.Deploy(ctx, Options{Name: "api", Namespace: "default", Image: "ghcr.io/acme/api:v1", Replicas: -1,
			Changed: map[string]bool{"replicas": true}})
		require.ErrorContains(t, err, "must not be negative")
		require.ErrorContains(t, d.Scale(ctx, "default", "api", -1), "must not be negative")
		assert.Equal(t, int64(3), liveSpec(t, dynClient)["replicas"])
	})

	t.Run("disable with invalid bounds keeps the live count", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "web:1", "replicas": int64(2),
			"autoscale": map[string]interface{}{"enabled": true, "minReplicas": int64(10), "maxReplicas": int64(5), "cpuTarget": int64(70)}})
		seedDeployment(t, d, 3)

		res, err := d.DisableAutoscale(ctx, "default", "api")

		require.NoError(t, err)
		assert.Equal(t, int64(3), liveSpec(t, dynClient)["replicas"], "an invalid range must not make up a count")
		assert.True(t, res.InvalidBounds)
	})

	t.Run("disable reports a live count it moved into the bounds", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "web:1", "replicas": int64(2),
			"autoscale": map[string]interface{}{"enabled": true, "minReplicas": int64(2), "maxReplicas": int64(5), "cpuTarget": int64(70)}})
		seedDeployment(t, d, 8)

		res, err := d.DisableAutoscale(ctx, "default", "api")

		require.NoError(t, err)
		assert.Equal(t, int64(5), liveSpec(t, dynClient)["replicas"])
		assert.Equal(t, int32(8), res.Live)
		assert.Equal(t, int32(5), res.Replicas)
	})

	t.Run("explicit zero targets still get the CPU default on a first enable", func(t *testing.T) {
		for _, c := range []AutoscaleChange{{MemoryTarget: i32(0)}, {CPUTarget: i32(0)}, {CPUTarget: i32(0), MemoryTarget: i32(0)}} {
			d, dynClient := testDeployer()
			seedApp(t, dynClient, map[string]interface{}{"image": "web:1", "replicas": int64(2)})
			_, err := d.EnableAutoscale(ctx, "default", "api", c)
			require.NoError(t, err)
			as := liveSpec(t, dynClient)["autoscale"].(map[string]interface{})
			assert.Equal(t, int64(70), as["cpuTarget"])
			assert.NotContains(t, as, "memoryTarget")
		}
	})
}

func TestAutoscaleNegativeInputsAreRefused(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		stored map[string]interface{}
		change AutoscaleChange
	}{
		{"cpu -1, memory absent", nil, AutoscaleChange{CPUTarget: i32(-1)}},
		{"cpu -1, memory 0", nil, AutoscaleChange{CPUTarget: i32(-1), MemoryTarget: i32(0)}},
		{"cpu -1, memory positive", nil, AutoscaleChange{CPUTarget: i32(-1), MemoryTarget: i32(80)}},
		{"memory -1, cpu absent", nil, AutoscaleChange{MemoryTarget: i32(-1)}},
		{"stored negative cpu, no positive target", map[string]interface{}{"enabled": false, "maxReplicas": int64(5), "cpuTarget": int64(-1)}, AutoscaleChange{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, dynClient := testDeployer()
			spec := map[string]interface{}{"image": "web:1", "replicas": int64(2)}
			if tc.stored != nil {
				spec["autoscale"] = tc.stored
			}
			seedApp(t, dynClient, spec)
			before := liveSpec(t, dynClient)["autoscale"]

			_, err := d.EnableAutoscale(ctx, "default", "api", tc.change)

			require.ErrorContains(t, err, "must not be negative")
			assert.Equal(t, before, liveSpec(t, dynClient)["autoscale"], "a refused policy writes nothing")
		})
	}

	t.Run("a first deploy with a negative count creates nothing", func(t *testing.T) {
		d, dynClient := testDeployer()
		err := d.Deploy(ctx, Options{Name: "api", Namespace: "default", Image: "ghcr.io/acme/api:v1", Replicas: -1,
			Changed: map[string]bool{"replicas": true}})
		require.ErrorContains(t, err, "must not be negative")
		_, getErr := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
		assert.Error(t, getErr, "no App is created")
	})
}

func TestStoredZeroBoundsStaySet(t *testing.T) {
	ctx := context.Background()

	t.Run("SpecPolicy keeps an explicit zero and leaves an absent field nil", func(t *testing.T) {
		p := SpecPolicy(map[string]interface{}{"autoscale": map[string]interface{}{
			"enabled": false, "minReplicas": int64(0), "maxReplicas": int64(5), "memoryTarget": int64(0),
		}})
		require.NotNil(t, p.MinReplicas)
		assert.Equal(t, int32(0), *p.MinReplicas)
		assert.Equal(t, int32(5), *p.MaxReplicas)
		assert.Nil(t, p.CPUTarget)
		require.NotNil(t, p.MemoryTarget)
		assert.Error(t, capacity.Validate(p, nil), "a stored zero minimum is invalid, as admission says")
	})

	t.Run("an edit that keeps a stored zero minimum is refused until --min is given", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "web:1", "replicas": int64(3),
			"autoscale": map[string]interface{}{"enabled": false, "minReplicas": int64(0), "maxReplicas": int64(5)}})

		_, err := d.EnableAutoscale(ctx, "default", "api", AutoscaleChange{MaxReplicas: i32(8)})
		require.ErrorContains(t, err, "minReplicas must be at least 1")

		_, err = d.EnableAutoscale(ctx, "default", "api", AutoscaleChange{MinReplicas: i32(1), MaxReplicas: i32(8)})
		require.NoError(t, err)
		as := liveSpec(t, dynClient)["autoscale"].(map[string]interface{})
		assert.Equal(t, int64(1), as["minReplicas"])
		assert.Equal(t, int64(8), as["maxReplicas"])
	})
}

func TestReadAutoscaleStatusReportsTheReadyCondition(t *testing.T) {
	seed := func(t *testing.T, conditions []interface{}) *Deployer {
		t.Helper()
		d, dynClient := testDeployer()
		_, err := dynClient.Resource(AppGVR).Namespace("default").Create(context.Background(), &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "kipper.run/v1alpha1",
			"kind":       "App",
			"metadata":   map[string]interface{}{"name": "api", "namespace": "default"},
			"spec": map[string]interface{}{"image": "web:1", "replicas": int64(8),
				"autoscale": map[string]interface{}{"enabled": false, "minReplicas": int64(1), "maxReplicas": int64(5)}},
			"status": map[string]interface{}{"conditions": conditions},
		}}, metav1.CreateOptions{})
		require.NoError(t, err)
		return d
	}

	t.Run("a false condition is returned with its reason and message", func(t *testing.T) {
		d := seed(t, []interface{}{
			map[string]interface{}{"type": "Ready", "status": "True", "reason": "Running"},
			map[string]interface{}{"type": "AutoscalingReady", "status": "False", "reason": "ReplicasOutsideBounds", "message": "replicas 8 is outside 1 to 5"},
		})

		st, err := d.ReadAutoscaleStatus(context.Background(), "default", "api")

		require.NoError(t, err)
		require.NotNil(t, st.Condition)
		assert.Equal(t, AutoscalingCondition{Status: "False", Reason: "ReplicasOutsideBounds", Message: "replicas 8 is outside 1 to 5"}, *st.Condition)
	})

	failHPARead := func(d *Deployer, err error) {
		d.Client.(*fake.Clientset).PrependReactor("get", "horizontalpodautoscalers", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, err
		})
	}
	hpaResource := schema.GroupResource{Group: "autoscaling", Resource: "horizontalpodautoscalers"}

	t.Run("an autoscaler the caller may not read leaves the metrics unknown and keeps the condition", func(t *testing.T) {
		d := seed(t, []interface{}{
			map[string]interface{}{"type": "AutoscalingReady", "status": "False", "reason": "ReplicasOutsideBounds", "message": "replicas 8 is outside 1 to 5"},
		})
		failHPARead(d, apierrors.NewForbidden(hpaResource, "api", nil))

		st, err := d.ReadAutoscaleStatus(context.Background(), "default", "api")

		require.NoError(t, err)
		require.NotNil(t, st.Condition)
		assert.Equal(t, "ReplicasOutsideBounds", st.Condition.Reason)
		assert.False(t, st.HPAExists)
		assert.Empty(t, st.CurrentMetric)
	})

	t.Run("any other autoscaler read error is returned", func(t *testing.T) {
		d := seed(t, nil)
		failHPARead(d, apierrors.NewInternalError(assert.AnError))

		_, err := d.ReadAutoscaleStatus(context.Background(), "default", "api")

		require.ErrorContains(t, err, "reading the autoscaler")
	})

	t.Run("no condition leaves it nil", func(t *testing.T) {
		d := seed(t, nil)

		st, err := d.ReadAutoscaleStatus(context.Background(), "default", "api")

		require.NoError(t, err)
		assert.Nil(t, st.Condition)
	})
}

func TestDisableAutoscaleRefusalCarriesAnInvalidStoredPolicy(t *testing.T) {
	ctx := context.Background()
	refuse := func(dyn *dynamicfake.FakeDynamicClient) {
		refusal := apierrors.NewInvalid(schema.GroupKind{Group: "kipper.run", Kind: "App"}, "api", field.ErrorList{
			field.Invalid(field.NewPath("spec", "autoscale"), nil, "set maxReplicas with minReplicas, or leave both out to remove the bounds"),
		})
		dyn.PrependReactor("update", "apps", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, refusal
		})
	}

	t.Run("a block stored before the rules, with a minimum and no maximum", func(t *testing.T) {
		d, dyn := testDeployer()
		seedApp(t, dyn, map[string]interface{}{"image": "web:1", "replicas": int64(3),
			"autoscale": map[string]interface{}{"enabled": true, "minReplicas": int64(3), "cpuTarget": int64(70)}})
		seedDeployment(t, d, 3)
		refuse(dyn)

		_, err := d.DisableAutoscale(ctx, "default", "api")

		require.True(t, apierrors.IsInvalid(err), "the API refusal stays readable: %v", err)
		var invalid *InvalidPolicyRefusal
		require.ErrorAs(t, err, &invalid)
		assert.True(t, invalid.Policy.Enabled, "the policy is the stored one, before the switch-off")
		require.NotNil(t, invalid.Policy.MinReplicas)
		assert.Equal(t, int32(3), *invalid.Policy.MinReplicas)
		assert.Nil(t, invalid.Policy.MaxReplicas)
	})

	t.Run("a valid stored block is not blamed for a refusal", func(t *testing.T) {
		d, dyn := testDeployer()
		seedApp(t, dyn, map[string]interface{}{"image": "web:1", "replicas": int64(3),
			"autoscale": map[string]interface{}{"enabled": true, "minReplicas": int64(2), "maxReplicas": int64(5), "cpuTarget": int64(70)}})
		seedDeployment(t, d, 3)
		refuse(dyn)

		_, err := d.DisableAutoscale(ctx, "default", "api")

		require.True(t, apierrors.IsInvalid(err))
		var invalid *InvalidPolicyRefusal
		assert.False(t, errors.As(err, &invalid))
	})
}
