package controllers

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/intstr"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

const (
	sidecarContainerName        = "kipper-instance-proxy"
	inferredReadinessAnnotation = "kipper.run/inferred-readiness"

	probePeriodSeconds         = 5
	probeFailureThreshold      = 3
	defaultProbeTimeoutSeconds = 2
	preStopSleepSeconds        = 10
	// The 10s drain plus the 30s an app has after SIGTERM by default.
	shapedGracePeriodSeconds = 40
)

// inference records whether a ready pod accepted a connection (tcp), an
// older container refused one (none), or the result is still inconclusive.
type inference string

const (
	inferencePending       inference = ""
	inferredTCP            inference = "tcp"
	inferredNone           inference = "none"
	inferenceNotApplicable inference = "n/a"
)

// isPlaceholder identifies the temporary page used before the first Git build.
func isPlaceholder(app *kipperv1.App) bool {
	return app.Spec.Git != nil && app.Spec.Image == "busybox:latest"
}

// declaredReadiness builds the declared probe, using TCP on the app port
// while the Git placeholder runs. An omitted check or type none returns nil.
func declaredReadiness(app *kipperv1.App) *corev1.Probe {
	h := app.Spec.Health
	if h == nil || h.Type == "none" {
		return nil
	}
	timeout := int32(defaultProbeTimeoutSeconds)
	if h.TimeoutSeconds != nil {
		timeout = *h.TimeoutSeconds
	}
	if isPlaceholder(app) {
		return tcpProbe(app.Spec.Port, timeout)
	}
	port := app.Spec.Port
	if h.Port != nil {
		port = *h.Port
	}
	if h.Type == "http" {
		p := tcpProbe(port, timeout)
		p.ProbeHandler = corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: h.Path, Port: intstr.FromInt32(port), Scheme: corev1.URISchemeHTTP,
		}}
		return p
	}
	return tcpProbe(port, timeout)
}

// tcpProbe sets probe defaults explicitly so API server defaulting preserves
// equality between rendered and stored probes.
func tcpProbe(port, timeout int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)}},
		PeriodSeconds:    probePeriodSeconds,
		TimeoutSeconds:   timeout,
		FailureThreshold: probeFailureThreshold,
		SuccessThreshold: 1,
	}
}

// storedInference reads the recorded decision for an automatic app. Declared
// checks and Git placeholders bypass inference.
func storedInference(app *kipperv1.App, live *corev1.PodTemplateSpec) inference {
	if app.Spec.Health != nil || isPlaceholder(app) {
		return inferenceNotApplicable
	}
	if live == nil {
		return inferencePending
	}
	switch v := inference(live.Annotations[inferredReadinessAnnotation]); v {
	case inferredTCP, inferredNone:
		return v
	}
	return inferencePending
}

// applyPlatformShape adds inferred readiness, sidecar readiness, and supported
// shutdown hooks. The caller supplies any declared app probe.
func applyPlatformShape(tpl *corev1.PodTemplateSpec, app *kipperv1.App, inf inference, preStopSleep bool) {
	if app.Spec.Health == nil && !isPlaceholder(app) {
		tpl.Spec.Containers[0].ReadinessProbe = nil
		delete(tpl.Annotations, inferredReadinessAnnotation)
		if inf == inferredTCP {
			tpl.Spec.Containers[0].ReadinessProbe = tcpProbe(app.Spec.Port, defaultProbeTimeoutSeconds)
		}
		if inf == inferredTCP || inf == inferredNone {
			if tpl.Annotations == nil {
				tpl.Annotations = map[string]string{}
			}
			tpl.Annotations[inferredReadinessAnnotation] = string(inf)
		}
	}
	for i := range tpl.Spec.Containers {
		c := &tpl.Spec.Containers[i]
		if c.Name == sidecarContainerName {
			c.ReadinessProbe = tcpProbe(app.Spec.Port+10000, defaultProbeTimeoutSeconds)
		}
		if preStopSleep {
			c.Lifecycle = &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{
				Sleep: &corev1.SleepAction{Seconds: preStopSleepSeconds},
			}}
		}
	}
	if preStopSleep {
		grace := int64(shapedGracePeriodSeconds)
		tpl.Spec.TerminationGracePeriodSeconds = &grace
	}
}

// platformProbeOwned treats an annotated or absent probe as platform-owned.
// An unannotated probe is treated as a declared check.
func platformProbeOwned(tpl *corev1.PodTemplateSpec) bool {
	_, inferred := tpl.Annotations[inferredReadinessAnnotation]
	return inferred || tpl.Spec.Containers[0].ReadinessProbe == nil
}

// copyPlatformShape copies the live platform settings into a candidate
// template so comparison can isolate changes that require a rollout.
func copyPlatformShape(dst, src *corev1.PodTemplateSpec, app *kipperv1.App) {
	automatic := app.Spec.Health == nil && !isPlaceholder(app)
	if automatic && platformProbeOwned(src) {
		dst.Spec.Containers[0].ReadinessProbe = src.Spec.Containers[0].ReadinessProbe.DeepCopy()
	}
	// The inference record belongs to an automatic app only, so declaring a
	// check that equals the inferred one still changes the template.
	if v, ok := src.Annotations[inferredReadinessAnnotation]; ok && automatic {
		if dst.Annotations == nil {
			dst.Annotations = map[string]string{}
		}
		dst.Annotations[inferredReadinessAnnotation] = v
	} else {
		delete(dst.Annotations, inferredReadinessAnnotation)
	}
	dst.Spec.Containers[0].Lifecycle = src.Spec.Containers[0].Lifecycle.DeepCopy()
	if i := containerIndex(dst.Spec.Containers, sidecarContainerName); i >= 0 {
		var probe *corev1.Probe
		var lifecycle *corev1.Lifecycle
		if j := containerIndex(src.Spec.Containers, sidecarContainerName); j >= 0 {
			probe = src.Spec.Containers[j].ReadinessProbe.DeepCopy()
			lifecycle = src.Spec.Containers[j].Lifecycle.DeepCopy()
		}
		dst.Spec.Containers[i].ReadinessProbe = probe
		dst.Spec.Containers[i].Lifecycle = lifecycle
	}
	if src.Spec.TerminationGracePeriodSeconds == nil {
		dst.Spec.TerminationGracePeriodSeconds = nil
	} else {
		grace := *src.Spec.TerminationGracePeriodSeconds
		dst.Spec.TerminationGracePeriodSeconds = &grace
	}
}

// platformShape groups probe, shutdown, and inference fields for comparison.
type platformShape struct {
	AppReadiness     *corev1.Probe
	Inference        string
	AppLifecycle     *corev1.Lifecycle
	SidecarReadiness *corev1.Probe
	SidecarLifecycle *corev1.Lifecycle
	Grace            int64
}

func platformShapeOf(tpl *corev1.PodTemplateSpec) platformShape {
	s := platformShape{
		AppReadiness: tpl.Spec.Containers[0].ReadinessProbe,
		Inference:    tpl.Annotations[inferredReadinessAnnotation],
		AppLifecycle: tpl.Spec.Containers[0].Lifecycle,
		Grace:        corev1.DefaultTerminationGracePeriodSeconds,
	}
	if i := containerIndex(tpl.Spec.Containers, sidecarContainerName); i >= 0 {
		s.SidecarReadiness = tpl.Spec.Containers[i].ReadinessProbe
		s.SidecarLifecycle = tpl.Spec.Containers[i].Lifecycle
	}
	if tpl.Spec.TerminationGracePeriodSeconds != nil {
		s.Grace = *tpl.Spec.TerminationGracePeriodSeconds
	}
	return s
}

func platformShapeEqual(a, b *corev1.PodTemplateSpec) bool {
	return equality.Semantic.DeepEqual(platformShapeOf(a), platformShapeOf(b))
}
