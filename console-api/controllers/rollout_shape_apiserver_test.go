package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/apiservertest"
)

// API server defaulting must preserve platform field comparisons so
// unchanged templates do not trigger rollouts.
func TestRolloutShape_SurvivesTheAPIServer(t *testing.T) {
	cfg := apiservertest.Start(t)
	ctx := context.Background()
	c, err := crclient.New(cfg, crclient.Options{Scheme: testScheme()})
	require.NoError(t, err)
	require.NoError(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop-prod"}}))

	for _, health := range []*kipperv1.AppHealth{nil, {Type: "http", Path: "/ready"}, {Type: "tcp"}} {
		name := "automatic"
		if health != nil {
			name = health.Type
		}
		t.Run(name, func(t *testing.T) {
			app := healthApp(health)
			app.Name = "shop-" + name
			app.Spec.Route = &kipperv1.AppRoute{}
			r := &AppReconciler{Client: c, Scheme: testScheme(), SidecarImage: "ghcr.io/getkipper/kipper-sidecar:latest"}
			built, err := r.buildDeployment(ctx, app, nil, "gen-1", "")
			require.NoError(t, err)
			built.OwnerReferences = nil

			legacy := built.DeepCopy()
			legacy.Spec.Template.Spec.Containers[0].ReadinessProbe = nil
			require.NoError(t, c.Create(ctx, legacy))

			shaped := built.DeepCopy()
			applyPlatformShape(&shaped.Spec.Template, app, inferredTCP, true)

			candidate := shaped.DeepCopy()
			copyPlatformShape(&candidate.Spec.Template, &legacy.Spec.Template, app)
			if health == nil {
				settles, answered, err := templateSettlesAs(ctx, c, legacy, candidate.Spec.Template)
				require.NoError(t, err)
				require.True(t, answered)
				assert.True(t, settles, "a legacy app with nothing changed settles, so the upgrade rolls nothing")
			}
			settles, _, err := templateSettlesAs(ctx, c, legacy, shaped.Spec.Template)
			require.NoError(t, err)
			assert.False(t, settles, "the shape itself is a change")

			var stored appsv1.Deployment
			require.NoError(t, c.Get(ctx, crclient.ObjectKeyFromObject(legacy), &stored))
			stored.Spec.Template = shaped.Spec.Template
			require.NoError(t, c.Update(ctx, &stored))
			require.NoError(t, c.Get(ctx, crclient.ObjectKeyFromObject(legacy), &stored))

			assert.True(t, platformShapeEqual(&shaped.Spec.Template, &stored.Spec.Template),
				"admission adds nothing to the fields Kipper owns:\nbuilt  %+v\nstored %+v",
				platformShapeOf(&shaped.Spec.Template), platformShapeOf(&stored.Spec.Template))
			settles, _, err = templateSettlesAs(ctx, c, &stored, shaped.Spec.Template)
			require.NoError(t, err)
			assert.True(t, settles, "a shaped template settles as itself, so nothing churns")
			require.NotNil(t, stored.Spec.Template.Spec.Containers[0].Lifecycle.PreStop.Sleep)
		})
	}
}
