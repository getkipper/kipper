package controllers

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func healthApp(health *kipperv1.AppHealth) *kipperv1.App {
	return &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "shop-prod"},
		Spec:       kipperv1.AppSpec{Image: "registry.example.com/shop:1", Port: 8080, Health: health},
	}
}

func placeholderApp(health *kipperv1.AppHealth) *kipperv1.App {
	app := healthApp(health)
	app.Spec.Image = "busybox:latest"
	app.Spec.Git = &kipperv1.AppGitSource{URL: "https://git.example.com/shop.git"}
	return app
}

func shapeTemplate(withSidecar bool) *corev1.PodTemplateSpec {
	tpl := &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "shop"}}}}
	if withSidecar {
		tpl.Spec.Containers = append(tpl.Spec.Containers, corev1.Container{Name: sidecarContainerName})
	}
	return tpl
}

func TestDeclaredReadiness(t *testing.T) {
	i32 := func(v int32) *int32 { return &v }
	tcp8080 := &corev1.Probe{
		ProbeHandler:  corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8080)}},
		PeriodSeconds: 5, TimeoutSeconds: 2, FailureThreshold: 3, SuccessThreshold: 1,
	}
	tests := []struct {
		name string
		app  *kipperv1.App
		want *corev1.Probe
	}{
		{name: "automatic declares nothing", app: healthApp(nil)},
		{name: "none declares nothing", app: healthApp(&kipperv1.AppHealth{Type: "none", StartupTimeoutSeconds: i32(600)})},
		{name: "tcp on the app port", app: healthApp(&kipperv1.AppHealth{Type: "tcp"}), want: tcp8080},
		{
			name: "http on the management port with its own timeout",
			app:  healthApp(&kipperv1.AppHealth{Type: "http", Path: "/actuator/health/readiness", Port: i32(8081), TimeoutSeconds: i32(5)}),
			want: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
					Path: "/actuator/health/readiness", Port: intstr.FromInt32(8081), Scheme: corev1.URISchemeHTTP,
				}},
				PeriodSeconds: 5, TimeoutSeconds: 5, FailureThreshold: 3, SuccessThreshold: 1,
			},
		},
		{
			name: "the placeholder answers http only on its own port and only at /",
			app:  placeholderApp(&kipperv1.AppHealth{Type: "http", Path: "/ready", Port: i32(8081)}),
			want: tcp8080,
		},
		{name: "the placeholder keeps none", app: placeholderApp(&kipperv1.AppHealth{Type: "none"})},
		{name: "the placeholder of an automatic app has no check", app: placeholderApp(nil)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, declaredReadiness(tt.app))
		})
	}
}

func TestApplyPlatformShape(t *testing.T) {
	tcp := func(port int32) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler:  corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)}},
			PeriodSeconds: 5, TimeoutSeconds: 2, FailureThreshold: 3, SuccessThreshold: 1,
		}
	}
	sleep := &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{Sleep: &corev1.SleepAction{Seconds: 10}}}

	t.Run("an inferred tcp check, the sidecar probe, and the drain", func(t *testing.T) {
		tpl := shapeTemplate(true)
		applyPlatformShape(tpl, healthApp(nil), inferredTCP, true)

		app, sidecar := tpl.Spec.Containers[0], tpl.Spec.Containers[1]
		assert.Equal(t, tcp(8080), app.ReadinessProbe)
		assert.Equal(t, "tcp", tpl.Annotations[inferredReadinessAnnotation])
		assert.Equal(t, tcp(18080), sidecar.ReadinessProbe)
		assert.Equal(t, sleep, app.Lifecycle)
		assert.Equal(t, sleep, sidecar.Lifecycle)
		require.NotNil(t, tpl.Spec.TerminationGracePeriodSeconds)
		assert.Equal(t, int64(40), *tpl.Spec.TerminationGracePeriodSeconds)
		assert.Nil(t, app.LivenessProbe, "no liveness by default")
		assert.Nil(t, app.StartupProbe)
	})

	t.Run("pending renders no app check and no annotation", func(t *testing.T) {
		tpl := shapeTemplate(false)
		applyPlatformShape(tpl, healthApp(nil), inferencePending, true)
		assert.Nil(t, tpl.Spec.Containers[0].ReadinessProbe)
		assert.NotContains(t, tpl.Annotations, inferredReadinessAnnotation)
	})

	t.Run("none is recorded but renders no check", func(t *testing.T) {
		tpl := shapeTemplate(false)
		applyPlatformShape(tpl, healthApp(nil), inferredNone, true)
		assert.Nil(t, tpl.Spec.Containers[0].ReadinessProbe)
		assert.Equal(t, "none", tpl.Annotations[inferredReadinessAnnotation])
	})

	t.Run("a declared check is left alone and nothing is inferred", func(t *testing.T) {
		app := healthApp(&kipperv1.AppHealth{Type: "tcp"})
		tpl := shapeTemplate(false)
		tpl.Spec.Containers[0].ReadinessProbe = declaredReadiness(app)
		applyPlatformShape(tpl, app, inferredNone, true)
		assert.Equal(t, tcp(8080), tpl.Spec.Containers[0].ReadinessProbe)
		assert.NotContains(t, tpl.Annotations, inferredReadinessAnnotation)
	})

	t.Run("no sidecar, no sidecar probe", func(t *testing.T) {
		tpl := shapeTemplate(false)
		applyPlatformShape(tpl, healthApp(nil), inferredTCP, true)
		require.Len(t, tpl.Spec.Containers, 1)
	})

	t.Run("without the sleep action the drain and the grace change are left out", func(t *testing.T) {
		tpl := shapeTemplate(true)
		applyPlatformShape(tpl, healthApp(nil), inferredTCP, false)
		assert.Nil(t, tpl.Spec.Containers[0].Lifecycle)
		assert.Nil(t, tpl.Spec.Containers[1].Lifecycle)
		assert.Nil(t, tpl.Spec.TerminationGracePeriodSeconds)
		assert.Equal(t, tcp(8080), tpl.Spec.Containers[0].ReadinessProbe, "the checks do not need it")
		assert.Equal(t, tcp(18080), tpl.Spec.Containers[1].ReadinessProbe)
	})

	t.Run("the placeholder is never inferred", func(t *testing.T) {
		tpl := shapeTemplate(true)
		applyPlatformShape(tpl, placeholderApp(nil), inferredTCP, true)
		assert.Nil(t, tpl.Spec.Containers[0].ReadinessProbe)
		assert.NotContains(t, tpl.Annotations, inferredReadinessAnnotation)
	})
}

func TestStoredInference(t *testing.T) {
	live := func(annotation string, probe bool) *corev1.PodTemplateSpec {
		tpl := shapeTemplate(false)
		if annotation != "" {
			tpl.Annotations = map[string]string{inferredReadinessAnnotation: annotation}
		}
		if probe {
			tpl.Spec.Containers[0].ReadinessProbe = &corev1.Probe{}
		}
		return tpl
	}
	assert.Equal(t, inferencePending, storedInference(healthApp(nil), nil), "a new Deployment starts pending")
	assert.Equal(t, inferencePending, storedInference(healthApp(nil), live("", false)), "a legacy template has no decision")
	assert.Equal(t, inferredTCP, storedInference(healthApp(nil), live("tcp", true)))
	assert.Equal(t, inferredNone, storedInference(healthApp(nil), live("none", false)))
	assert.Equal(t, inferencePending, storedInference(healthApp(nil), live("bogus", false)))
	assert.Equal(t, inferenceNotApplicable, storedInference(healthApp(&kipperv1.AppHealth{Type: "tcp"}), live("tcp", true)))
	assert.Equal(t, inferenceNotApplicable, storedInference(placeholderApp(nil), live("tcp", true)))
}

func TestPlatformShape_EqualAndCopy(t *testing.T) {
	app := healthApp(nil)
	shaped := func(inf inference, sleep bool) *corev1.PodTemplateSpec {
		tpl := shapeTemplate(true)
		applyPlatformShape(tpl, app, inf, sleep)
		return tpl
	}
	legacy := func() *corev1.PodTemplateSpec {
		tpl := shapeTemplate(true)
		grace := int64(30)
		tpl.Spec.TerminationGracePeriodSeconds = &grace
		return tpl
	}

	t.Run("equal", func(t *testing.T) {
		assert.True(t, platformShapeEqual(shaped(inferredTCP, true), shaped(inferredTCP, true)))
		assert.True(t, platformShapeEqual(shaped(inferencePending, false), shaped(inferencePending, false)))
		assert.True(t, platformShapeEqual(legacy(), func() *corev1.PodTemplateSpec {
			tpl := shapeTemplate(true)
			tpl.Spec.TerminationGracePeriodSeconds = nil
			return tpl
		}()), "an unset grace period is the API server's 30 seconds")
	})
	t.Run("different", func(t *testing.T) {
		assert.False(t, platformShapeEqual(shaped(inferredTCP, true), shaped(inferencePending, true)), "the inferred check")
		assert.False(t, platformShapeEqual(shaped(inferredNone, true), shaped(inferencePending, true)), "the recorded decision")
		assert.False(t, platformShapeEqual(shaped(inferredTCP, true), shaped(inferredTCP, false)), "the drain")
		assert.False(t, platformShapeEqual(shaped(inferencePending, false), legacy()), "the sidecar's check")
	})

	t.Run("copying live's platform fields leaves everything else", func(t *testing.T) {
		candidate := shaped(inferredTCP, true)
		candidate.Spec.Containers[0].Image = "registry.example.com/shop:2"
		live := legacy()
		live.Spec.Containers[0].Image = "registry.example.com/shop:1"

		copyPlatformShape(candidate, live, app)

		assert.True(t, platformShapeEqual(candidate, live))
		assert.Equal(t, "registry.example.com/shop:2", candidate.Spec.Containers[0].Image)
	})

	t.Run("a probe the user declared on live is not copied over a cleared check", func(t *testing.T) {
		declared := healthApp(&kipperv1.AppHealth{Type: "tcp", Port: func() *int32 { p := int32(9000); return &p }()})
		live := legacy()
		live.Spec.Containers[0].ReadinessProbe = declaredReadiness(declared)
		candidate := shaped(inferencePending, true)

		copyPlatformShape(candidate, live, app)

		assert.Nil(t, candidate.Spec.Containers[0].ReadinessProbe, "clearing health is the user's change and rolls")
	})

	t.Run("a declared check in the candidate is the user's and stays", func(t *testing.T) {
		declared := healthApp(&kipperv1.AppHealth{Type: "tcp"})
		candidate := shapeTemplate(true)
		candidate.Spec.Containers[0].ReadinessProbe = declaredReadiness(declared)
		applyPlatformShape(candidate, declared, inferenceNotApplicable, true)

		copyPlatformShape(candidate, legacy(), declared)

		assert.Equal(t, declaredReadiness(declared), candidate.Spec.Containers[0].ReadinessProbe)
		assert.Nil(t, candidate.Spec.Containers[1].ReadinessProbe, "the sidecar check is still the platform's")
	})
}
