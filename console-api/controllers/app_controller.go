package controllers

import (
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/appowner"
	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/secretname"
	"github.com/getkipper/kipper/controller/pkg/workload"
)

const (
	kipperLabel     = "app.kubernetes.io/managed-by"
	kipperValue     = "kipper"
	appFinalizer    = "kipper.run/app-cleanup"
	systemNamespace = "kipper-system"
)

// AppReconciler reconciles an App CR by ensuring the corresponding
// Deployment, Service, and Ingress exist and match the desired spec.
type AppReconciler struct {
	client.Client
	// APIReader is an uncached reader, used where an ownership decision must not
	// be made from a possibly-stale cache (route-host reservations).
	APIReader    client.Reader
	Scheme       *runtime.Scheme
	Domain       string // cluster domain for ingress generation
	SidecarImage string // container image for the instance-id sidecar proxy
	// Recorder puts a reconcile failure on the App itself, where `kubectl
	// describe` finds it without anyone tailing the controller's log. Nil in
	// unit tests.
	Recorder record.EventRecorder
}

// hostReader returns the uncached reader for reservation reads, falling back to
// the cached client when none is wired (unit tests construct the reconciler
// without one).
func (r *AppReconciler) hostReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// +kubebuilder:rbac:groups=kipper.run,resources=apps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kipper.run,resources=apps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list
// +kubebuilder:rbac:groups=core,resources=replicationcontrollers,verbs=get;list
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete

func (r *AppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, retErr error) {
	logger := log.FromContext(ctx)

	// Never reconcile resources in the system namespace — it contains
	// infrastructure deployments (Zot, console-api) that must not be
	// overwritten by the App reconciler.
	if req.Namespace == systemNamespace {
		return ctrl.Result{}, nil
	}

	var app kipperv1.App
	if err := r.Get(ctx, req.NamespacedName, &app); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// The status as this pass found it, so a pass that fails can tell whether it
	// changed anything before deciding to write. Taken before any step runs,
	// because conditions are set in memory throughout and compared once at the end.
	statusAtEntry := app.Status.DeepCopy()

	// Every failure below leaves the workload part-reconciled, so it belongs on
	// the object rather than only in a log line. The API server folds repeats of
	// the same reason and message into one event with a count, so a failure that
	// recurs on every pass does not flood the namespace.
	defer func() {
		if retErr != nil && r.Recorder != nil {
			r.Recorder.Event(&app, corev1.EventTypeWarning, "ReconcileFailed", retErr.Error())
		}
	}()

	// Handle deletion
	if !app.DeletionTimestamp.IsZero() {
		logger.Info("cleaning up app resources", "app", app.Name)
		if err := r.sweepWriterSecrets(ctx, &app); err != nil {
			return ctrl.Result{}, fmt.Errorf("sweeping writer secrets: %w", err)
		}
		// The host claim is sticky and is never released here. It stays held by
		// the project until another project reserves the host once this project
		// is gone (reserveHost then takes it over — see host_reservation.go), so
		// deleting a single app never frees its host for another project.
		return ctrl.Result{}, r.removeFinalizer(ctx, &app)
	}

	// Withdrawing a credential runs before anything that can fail for unrelated
	// reasons, the name claim included. An app whose binding is broken, whose
	// variables do not resolve, or whose name is held by another workload fails
	// the same step on every pass, and cleanup ordered after that point is
	// never reached — so a token whose source was removed would sit in the
	// namespace in plaintext for as long as the unrelated fault lasts. Each of
	// those states is one an App written straight to the Kubernetes API can
	// arrive in. Same reasoning as the link-policy withdrawal below.
	credentialRetry, err := r.sweepGitCredentials(ctx, &app)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("removing a git credential the app has moved off: %w", err)
	}
	if err := r.clearDetachedBuildStatus(ctx, &app); err != nil {
		return ctrl.Result{}, fmt.Errorf("clearing a detached build status: %w", err)
	}

	// See the Function reconciler: a CR written straight to the API server never
	// passed a reservation, and an App beside a Job shares no child to refuse.
	if heldBy, claimErr := reconcileNameClaim(ctx, r.Client, r.hostReader(), r.Scheme, &app, "app"); claimErr != nil {
		return ctrl.Result{}, claimErr
	} else if heldBy != "" {
		apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
			Type:               kipperv1.ConditionChildrenAdopted,
			Status:             metav1.ConditionFalse,
			Reason:             "NameHeldByAnotherWorkload",
			Message:            blockedMessage(app.Name, heldBy),
			ObservedGeneration: app.Generation,
		})
		app.Status.Phase = "Failed"
		r.writeStatusIfChanged(ctx, &app, statusAtEntry)
		return ctrl.Result{}, workload.NameTakenError{Name: app.Name, Kind: heldBy}
	}

	// The egress this app's links need, reconciled before anything else this app
	// owns. What it withdraws is an authorisation — a consent taken back, a link
	// removed, a target gone — and that has to converge whether or not the
	// workload does. Behind the Deployment it would not: a spec the cluster
	// cannot roll fails every pass at the same place, and an allowance nobody
	// agreed to any more would stand for as long as the app stayed broken.
	//
	// Each target's port is read from its App on every pass, through the same
	// rule that gives its Service a target port, so a target that gains, changes
	// or loses a port is followed rather than pinned.
	// Resolved once and carried through the pass. Resolving again for the pod's
	// addresses would be asking the same question of a cluster that has moved on:
	// a target deleted in between leaves the condition saying every link is open
	// while the Deployment rolls without the address it names.
	links, err := r.reconcileLinkPolicy(ctx, &app)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling link policy: %w", err)
	}
	// An app holding links gets swept periodically. The watches that notice a
	// changed target or a withdrawn consent map events through the cache, and a
	// map function has no way to report a failure or ask to be retried — so a
	// transient error at the wrong moment would otherwise leave a revoked
	// consent's policy standing until something unrelated touched this app.
	//
	// An app holding none needs no sweep: the pass that read the empty list
	// deleted the policy above, before anything here could fail, and a failure
	// to delete is returned and retried. A policy edited or removed out of band
	// comes back through Owns(NetworkPolicy).
	linkResync := ctrl.Result{}
	if len(app.Spec.Links) > 0 {
		linkResync = ctrl.Result{RequeueAfter: linkRefreshInterval}
	}

	// Ensure finalizer
	if !controllerutil.ContainsFinalizer(&app, appFinalizer) {
		controllerutil.AddFinalizer(&app, appFinalizer)
		if err := r.Update(ctx, &app); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Derive the per-binding credentials before the env Secret: the env render
	// resolves references against them, so a stale binding Secret here becomes a
	// stale credential baked into the pod's environment.
	renderedBindings, keepProjections, bindingHash, err := reconcileBindingSecrets(ctx, r.Client, r.Scheme, &app, secretname.KindApp, app.Spec.ServiceBindings)
	if err != nil {
		// Swept before the status write, so the conversion sentence it computes
		// is persisted with the refusal rather than mutated into a copy this
		// pass is about to discard. A binding can stay broken indefinitely, so
		// this is not a one-pass delay.
		r.sweepEnv(ctx, &app, app.Status.PublishedEnv,
			declaredProjectionNames(app.Spec.ServiceBindings, secretname.KindApp, app.Name))

		applyServiceBindingCondition(&app.Status.Conditions, app.Generation, len(app.Spec.ServiceBindings), []string{err.Error()})
		// Persist the reason before failing, or the retry loop is silent about
		// why the workload is not moving.
		if statusErr := r.Status().Update(ctx, &app); statusErr != nil {
			log.FromContext(ctx).Error(statusErr, "recording refused service bindings")
		}
		return ctrl.Result{}, fmt.Errorf("reconciling binding secrets: %w", err)
	}

	// One table for the env render and the pod both, built before either. The
	// injection gate used to run inside reconcileDeployment, after the env
	// render, so a resolver reading every declared binding would resolve
	// ${DB_PASSWORD} from a credential the pod is deliberately refused.
	sources, refusedBindings, err := appEnvSources(ctx, r.Client, &app, links, renderedBindings)
	if err != nil {
		// Report it too. A lookup that failed leaves the last answer standing,
		// and the last answer may be that everything was fine.
		// Swept before the status write, so the conversion sentence it computes
		// is persisted with the refusal rather than mutated into a copy this
		// pass is about to discard. A binding can stay broken indefinitely, so
		// this is not a one-pass delay.
		r.sweepEnv(ctx, &app, app.Status.PublishedEnv,
			declaredProjectionNames(app.Spec.ServiceBindings, secretname.KindApp, app.Name))

		applyServiceBindingCondition(&app.Status.Conditions, app.Generation, len(app.Spec.ServiceBindings), []string{err.Error()})
		if statusErr := r.Status().Update(ctx, &app); statusErr != nil {
			logger.Error(statusErr, "recording refused service bindings")
		}
		return ctrl.Result{}, fmt.Errorf("resolving service bindings: %w", err)
	}
	// A refused binding leaves the app without env it was written to expect, and
	// the pod fails on config rather than on anything naming the binding. Say so
	// where an operator will find it.
	applyServiceBindingCondition(&app.Status.Conditions, app.Generation, len(app.Spec.ServiceBindings), refusedBindings)
	if len(refusedBindings) > 0 {
		// Rendering the env Secret or rolling the pod template without a
		// declared binding takes credentials away from a workload that is
		// currently working, and `Optional: true` means the pod starts anyway
		// and fails on its first connection. Stop here instead and keep the last
		// good render while this retries.
		logger.Info("service bindings refused; leaving the workload on its current configuration",
			"app", app.Name, "namespace", app.Namespace, "refused", refusedBindings)
		// Swept before this write like the other two refusal paths, so the
		// conversion sentence lands with the refusal instead of in a copy this
		// pass discards.
		r.sweepEnv(ctx, &app, app.Status.PublishedEnv,
			declaredProjectionNames(app.Spec.ServiceBindings, secretname.KindApp, app.Name))

		if statusErr := r.Status().Update(ctx, &app); statusErr != nil {
			logger.Error(statusErr, "recording refused service bindings")
		}
		return ctrl.Result{}, fmt.Errorf("service bindings refused: %s", strings.Join(refusedBindings, "; "))
	}

	// Render the env Secret before the Deployment. The pod reads it via envFrom
	// at container start, so the Secret must be current before a spec change
	// rolls new pods — otherwise a restart triggered in the same pass could
	// start pods against the old env.
	generation, err := r.reconcileEnvSecret(ctx, &app, sources)
	if recErr := recordPublication(ctx, r.Client, &app, &app.Status.Conditions,
		&app.Status.PublishedEnv, app.Generation, generation, err); recErr != nil {
		logger.Error(recErr, "recording the published environment")
	}
	if err != nil {
		// Persist the reason before failing. A pod whose environment did not
		// publish does not start at all, and the reason it gives is
		// CreateContainerConfigError, which names nothing.
		if statusErr := r.Status().Update(ctx, &app); statusErr != nil {
			logger.Error(statusErr, "recording a failed environment publication")
		}
		return ctrl.Result{}, fmt.Errorf("reconciling env secret: %w", err)
	}

	if err := r.adoptWriterSecrets(ctx, &app); err != nil {
		return ctrl.Result{}, fmt.Errorf("adopting writer secrets: %w", err)
	}

	if err := r.reconcileChildren(ctx, &app, sources, generation, bindingHash); err != nil {
		// The pass stops here, and everything after it — the ingress, the
		// autoscaler, the sweep, the status write — is skipped. What this pass
		// did learn is still worth recording, or the workload goes on reporting
		// whatever the last complete pass found while a refused object holds it.
		r.sweepEnv(ctx, &app, generation, keepProjections)
		if obsErr := r.observeWorkload(ctx, &app); obsErr != nil {
			// Losing the observation does not change why the pass failed, and the
			// caller's error is the one worth returning.
			logger.Error(obsErr, "observing workload while recording a refused child", "app", app.Name)
		}
		apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
			Type:               kipperv1.ConditionChildrenAdopted,
			Status:             metav1.ConditionFalse,
			Reason:             "ChildReconcileFailed",
			Message:            err.Error(),
			ObservedGeneration: app.Generation,
		})
		r.withdrawAPIKeyGate(&app, err)
		r.writeStatusIfChanged(ctx, &app, statusAtEntry)
		return ctrl.Result{}, err
	}
	apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type:               kipperv1.ConditionChildrenAdopted,
		Status:             metav1.ConditionTrue,
		Reason:             "AllChildrenAdopted",
		Message:            "every child this workload owns reconciled",
		ObservedGeneration: app.Generation,
	})

	// Before the status write, so what the sweep learns about this workload's
	// conversion is persisted with everything else rather than mutated into a
	// copy the pass is about to discard.
	retryIn := r.sweepEnv(ctx, &app, generation, keepProjections)

	// Update status from the Deployment
	if err := r.updateStatus(ctx, &app); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	if retryIn > 0 && (linkResync.RequeueAfter == 0 || retryIn < linkResync.RequeueAfter) {
		linkResync.RequeueAfter = retryIn
	}
	if credentialRetry > 0 && (linkResync.RequeueAfter == 0 || credentialRetry < linkResync.RequeueAfter) {
		linkResync.RequeueAfter = credentialRetry
	}

	return linkResync, nil
}

// sweepEnv retires what this app has moved off, and reports how long to wait
// before looking again.
//
// It never fails the pass. Not sweeping leaves Secrets behind and nothing else,
// while failing here would undo a publication that succeeded.
//
// It is called on the failure paths above as well, with every declared binding
// treated as wanted. A pass that stops early — a Service that cannot be read, a
// binding refused — used to leave a projection whose binding was removed with
// nothing scheduled to look at it again, because the only sweep was at the end
// of a pass that never got there.
func (r *AppReconciler) sweepEnv(ctx context.Context, app *kipperv1.App, generation string, keep map[string]bool) time.Duration {
	retryIn, legacyReaders, err := retireEnvSecrets(ctx, r.Client, r.hostReader(), app, secretname.KindApp, generation, keep)
	if err != nil {
		// It has to come back: this is the only timer retirement has, so
		// dropping it strands whatever the failed pass had already marked.
		log.FromContext(ctx).Error(err, "retiring superseded environments", "app", app.Name)
		return envRetirementRetry
	}
	applyEnvPublishedConditionWithConversion(&app.Status.Conditions, app.Generation, nil, legacyReaders)
	return retryIn
}

func (r *AppReconciler) reconcileDeployment(ctx context.Context, app *kipperv1.App, sources []envSource, generation, bindingHash string) error {
	replicas := int32(1)
	if app.Spec.Replicas != nil {
		replicas = *app.Spec.Replicas
	}

	labels := map[string]string{
		"app":       app.Name,
		kipperLabel: kipperValue,
	}
	if app.Spec.Resources.Profile != "" {
		labels["kipper.run/resource-profile"] = app.Spec.Resources.Profile
	}

	container := corev1.Container{
		Name:  app.Name,
		Image: app.Spec.Image,
		Ports: []corev1.ContainerPort{
			{ContainerPort: app.Spec.Port},
		},
		Resources: buildAppResources(app.Spec.Resources),
	}

	// When a git-based app is waiting for its first build, the image is
	// busybox:latest. Override the command to serve a friendly "building"
	// page instead of a container that exits immediately.
	if app.Spec.Git != nil && app.Spec.Image == "busybox:latest" {
		container.Command = []string{"sh", "-c", fmt.Sprintf(
			`mkdir -p /www && cat > /www/index.html << 'EOHTML'
<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="10">
<title>Deploying %s</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,system-ui,sans-serif;background:#0a0a0a;color:#fff;display:flex;align-items:center;justify-content:center;min-height:100vh}
.c{text-align:center;padding:2rem}
h1{font-size:1.5rem;font-weight:600;margin-bottom:.75rem}
p{color:#9ca3af;font-size:.9rem;line-height:1.5}
.s{display:inline-block;width:20px;height:20px;border:2px solid #333;border-top-color:#3b82f6;border-radius:50%%;animation:r .8s linear infinite;margin-bottom:1rem}
@keyframes r{to{transform:rotate(360deg)}}
</style>
</head>
<body>
<div class="c">
<div class="s"></div>
<h1>Building %s</h1>
<p>Your app is being built and deployed.<br>This page will refresh automatically.</p>
</div>
</body>
</html>
EOHTML
httpd -p %d -h /www -f`, app.Name, app.Name, app.Spec.Port)}
	}

	// The env Secret, the app's own secrets and every accepted service binding,
	// in the order the kubelet applies them — and then the address of each app
	// this one links to, which the pod carries as container.Env and which
	// therefore beats all of them.
	//
	// A link's address is derived rather than stored: a stored copy goes stale
	// the moment the target moves, and the egress allowance would follow the
	// target while the caller went on dialling the old port. Because this lands
	// in the pod template, a target that moves rolls the caller's pods onto the
	// new address by itself.
	container.EnvFrom = envFrom(generation)
	container.Env = directEnv(sources)

	// Add shared volume mounts
	var podVolumes []corev1.Volume
	for _, vm := range app.Spec.Volumes {
		pvcName := "shared-" + vm.Name
		podVolumes = append(podVolumes, corev1.Volume{
			Name: vm.Name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvcName,
				},
			},
		})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      vm.Name,
			MountPath: vm.MountPath,
		})
	}

	// Inject instance-id sidecar proxy when enabled
	containers := []corev1.Container{container}
	if r.shouldInjectSidecar(app) {
		sidecarPort := app.Spec.Port + 10000

		sidecar := corev1.Container{
			Name:  "kipper-instance-proxy",
			Image: r.SidecarImage,
			Ports: []corev1.ContainerPort{{ContainerPort: sidecarPort}},
			Env: []corev1.EnvVar{
				{Name: "LISTEN_PORT", Value: fmt.Sprintf("%d", sidecarPort)},
				{Name: "UPSTREAM_PORT", Value: fmt.Sprintf("%d", app.Spec.Port)},
				{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
				}},
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("10m"),
					corev1.ResourceMemory: resource.MustParse("16Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("50m"),
					corev1.ResourceMemory: resource.MustParse("32Mi"),
				},
			},
		}
		containers = append(containers, sidecar)
	}

	pullSecrets, err := ensureImagePullSecret(ctx, r.Client, r.Scheme, app, app.Spec.Image)
	if err != nil {
		return err
	}

	historyLimit := int32(3)
	desired := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
			// A Function creates a Deployment and a Service too, and its
			// children carry the same two labels. Without this marker a
			// same-named Function would accept an app's Deployment as its own
			// on any pass where it has no controller reference yet.
			Labels: withResourceType(labels, "app"),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas:             &replicas,
			RevisionHistoryLimit: &historyLimit,
			Selector:             &metav1.LabelSelector{MatchLabels: map[string]string{"app": app.Name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: podAnnotations(app.Annotations, bindingHash),
				},
				Spec: corev1.PodSpec{
					Containers:       containers,
					Volumes:          podVolumes,
					ImagePullSecrets: pullSecrets,
				},
			},
		},
	}

	// Set owner reference so the Deployment is garbage-collected with the App
	if err := controllerutil.SetControllerReference(app, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference: %w", err)
	}

	// Read, decide and write under one retry. This controller and everything
	// else that touches a Deployment — the operator's own `kip app update`, the
	// console, a scale — write the same object, so a conflict here is ordinary
	// rather than exceptional. Returning it works, because a returned error is
	// requeued with backoff, but it logs a failure for something that did not
	// fail and buries the ones that did.
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Each attempt gets its own copy. applyDeployment decides from the live
		// object and writes what it decides into desired — holding the running
		// environment generation, keeping an unpinned resource value — so an
		// attempt that loses a conflict leaves those decisions behind. The next
		// attempt reads a different live object and would treat the previous
		// one's conclusions as its own, which is how a workload gets written
		// back to a generation that was already retired.
		return r.applyDeployment(ctx, app, desired.DeepCopy(), generation)
	})
}

// applyDeployment brings one Deployment to the desired state, reading it fresh
// so a conflict retry decides on what the object is now rather than on what it
// was when the reconcile started.
func (r *AppReconciler) applyDeployment(ctx context.Context, app *kipperv1.App, desired *appsv1.Deployment, generation string) error {
	var existing appsv1.Deployment
	err := r.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, &existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	// Adjustments made directly on the Deployment (a VPA, an operator, a manual
	// edit) live on the workload, not the App CR. Keep them for any resource
	// type the CR does not pin, so pinning CPU alone doesn't reset a raised
	// memory limit back to the profile baseline.
	cpuPinned := app.Spec.Resources.CPURequest != "" || app.Spec.Resources.CPULimit != ""
	memPinned := app.Spec.Resources.MemoryRequest != "" || app.Spec.Resources.MemoryLimit != ""
	if (!cpuPinned || !memPinned) &&
		len(existing.Spec.Template.Spec.Containers) > 0 && len(desired.Spec.Template.Spec.Containers) > 0 {
		preserveUnpinnedResources(
			&desired.Spec.Template.Spec.Containers[0].Resources,
			existing.Spec.Template.Spec.Containers[0].Resources,
			cpuPinned, memPinned,
		)
	}

	// When autoscaling is active, let the HPA own the replica count.
	// Otherwise the reconciler would reset it on every loop. The change
	// check must run before the assignment, or a replicas-only edit
	// compares the already-synced value against itself and never
	// reaches the Deployment.
	replicasChanged := false
	if app.Spec.Autoscale == nil || !app.Spec.Autoscale.Enabled {
		replicasChanged = !equality.Semantic.DeepEqual(existing.Spec.Replicas, desired.Spec.Replicas)
		existing.Spec.Replicas = desired.Spec.Replicas
	}

	// Only update if the template or labels actually changed.
	// Unconditional updates cause an infinite reconciliation loop
	// because each update bumps the resourceVersion, which triggers
	// another reconciliation.
	templateChanged := !equality.Semantic.DeepEqual(existing.Spec.Template, desired.Spec.Template)
	labelsChanged := !equality.Semantic.DeepEqual(existing.Labels, desired.Labels)

	// Reconcile a permanent invariant: every source this app's environment
	// comes from must have a matching EnvFrom entry on the running container.
	// The full-template DeepEqual above usually catches drift, but
	// `equality.Semantic.DeepEqual` can return true for two templates
	// that differ only in EnvFrom ordering or normalized fields, and
	// the binding env is load-bearing for the pod's credentials. Any
	// drift here — whether from `kubectl edit`, a Velero restore, or a
	// third-party admission controller — forces a re-render.
	// Which generation the pod template names is a separate decision from which
	// one was published, and it exists to keep a product promise: editing env in
	// the console shows a "restart to apply" banner and does not restart a
	// running app.
	//
	// Every other trigger that should roll a workload already changes the
	// template in some other field — the credential fingerprint and the restart
	// stamp are both pod annotations, an image change is the container — so the
	// question reduces to whether anything but the generation differs. It is put
	// to the API server rather than answered here: the template built above sets
	// what this controller owns, the live one has been through admission, and
	// comparing those two directly says "different" on every cluster whatever
	// the environment is doing.
	running := generationOnContainer(existing.Spec.Template.Spec.Containers, secretname.KindApp, app.Name)
	if running != "" && running != generation {
		protected, err := hasLivePod(ctx, r.hostReader(), &existing)
		if err != nil {
			return err
		}
		// Holding a generation that has gone, or that something else now sits
		// at, would strand the workload for good: this pass republishes the
		// newest, so nothing ever rewrites the one the template names, and the
		// pods cannot start. Advancing is both the repair and the right answer —
		// an environment that vanished is not one worth protecting a pod's
		// place on.
		stillThere, err := generationUsable(ctx, r.hostReader(), app, secretname.KindApp, running)
		if err != nil {
			return err
		}
		if protected && stillThere {
			held := desired.DeepCopy()
			held.Spec.Template.Spec.Containers[0].EnvFrom = envFrom(running)
			settles, answered, err := templateSettlesAs(ctx, r.Client, &existing, held.Spec.Template)
			if err != nil {
				// A pass that cannot tell whether the app would roll asks again
				// rather than guessing. Guessing wrong here restarts a running
				// app nobody asked to restart.
				return err
			}
			if !answered && r.Recorder != nil {
				// The promise is that an env edit does not restart a running
				// app, and on this cluster it cannot be kept. Say so on the App,
				// where an operator wondering why their pods rolled will look.
				r.Recorder.Event(app, corev1.EventTypeWarning, "EnvHoldUnavailable",
					"this cluster does not allow the dry-run that decides whether an environment change can be applied without a restart, so the app restarts to pick it up")
			}
			if settles {
				// The live template already is what the write would store, so
				// it is what desired becomes: assigning the freshly built one
				// instead leaves templateChanged true for the same defaulting
				// reason, and the pass writes an object identical to the one
				// already there, every reconcile, for as long as the edit is
				// pending.
				desired.Spec.Template = existing.Spec.Template
				generation = running
			}
		}
		templateChanged = !equality.Semantic.DeepEqual(existing.Spec.Template, desired.Spec.Template)
	}

	bindingDriftDetected := generationOnContainer(
		existing.Spec.Template.Spec.Containers, secretname.KindApp, app.Name) != generation

	ownerOK := metav1.IsControlledBy(&existing, app)
	if err := adoptChild("Deployment", &existing, appOwner(app), r.Scheme); err != nil {
		return err
	}

	if ownerOK && !templateChanged && !labelsChanged && !replicasChanged && !bindingDriftDetected {
		return nil
	}

	existing.Spec.Template = desired.Spec.Template
	existing.Labels = desired.Labels
	return r.Update(ctx, &existing)
}

// injectableBindingSecret reports whether the named Secret may be injected into
// a workload for this binding, and errors on a non-NotFound lookup failure so
// the caller retries rather than rendering a workload without its credentials
// (an absent Secret is (false, nil) — the Optional envFrom no-ops).
//
// A binding names a Service, and a direct App/Function CR write bypasses the
// bind handler that validates it, so the Secret's PROVENANCE is the gate rather
// than its name. Two shapes are legitimate and nothing else is:
//
//   - the shared credentials, which must be controller-owned by the Service CR
//     the binding names, matched on UID;
//   - the derived per-binding Secret, which must be controller-owned by the
//     workload being rendered, again on UID.
//
// Matching the owner's kind and API group was not enough: any Service CR in the
// namespace satisfied it, so a binding could name a different service's
// credentials and have them injected. The UID ties the Secret to the one
// Service this binding actually declares.
func injectableBindingSecret(ctx context.Context, c client.Client, b kipperv1.ServiceBinding, kind secretname.Kind, owner client.Object) (bool, error) {
	svcType, typeKnown, err := bindingServiceType(ctx, c, b.Name, owner.GetNamespace())
	if err != nil {
		return false, err
	}
	secretName := bindingSecretName(b, svcType, typeKnown, kind, owner.GetName())

	var sec corev1.Secret
	err = c.Get(ctx, types.NamespacedName{Name: secretName, Namespace: owner.GetNamespace()}, &sec)
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	ref := metav1.GetControllerOf(&sec)
	if ref == nil {
		return false, nil
	}

	if bindingIsDerived(b, svcType, typeKnown) {
		// Derived: this workload renders it and must own it.
		return ref.UID == owner.GetUID(), nil
	}

	var svc kipperv1.Service
	if err := c.Get(ctx, types.NamespacedName{Name: b.Name, Namespace: owner.GetNamespace()}, &svc); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return ref.UID == svc.UID, nil
}

// middlewareGVK is the Traefik Middleware kind, which every middleware
// reconciler here reads and writes as an unstructured object because the
// project does not vendor Traefik's API types.
var middlewareGVK = schema.GroupVersionKind{Group: "traefik.io", Version: "v1alpha1", Kind: "Middleware"}

// applyOwnedMiddleware creates or updates one of an app's Middlewares. The
// object carries a controller reference so an out-of-band deletion triggers a
// reconcile that restores it, and an object already present under the same
// name is adopted only when adoptChild agrees it is this app's.
func (r *AppReconciler) applyOwnedMiddleware(ctx context.Context, app *kipperv1.App, desired *unstructured.Unstructured) error {
	if err := controllerutil.SetControllerReference(app, desired, r.Scheme); err != nil {
		return err
	}

	var existing unstructured.Unstructured
	existing.SetGroupVersionKind(middlewareGVK)
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if err := adoptChild("Middleware", &existing, appOwner(app), r.Scheme); err != nil {
		return err
	}
	existing.Object["spec"] = desired.Object["spec"]
	return r.Update(ctx, &existing)
}

// deleteOwnedMiddleware removes a Middleware when the route feature behind it
// is switched off. A Middleware under the same name that this app does not
// control belongs to somebody else and is left alone, so switching a feature
// off cannot destroy what switching it on refused to touch.
func (r *AppReconciler) deleteOwnedMiddleware(ctx context.Context, app *kipperv1.App, name string) error {
	var existing unstructured.Unstructured
	existing.SetGroupVersionKind(middlewareGVK)
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: app.Namespace}, &existing); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !ownedByWorkload(&existing, appOwner(app)) {
		return nil
	}
	if err := r.Delete(ctx, &existing); err != nil && !errors.IsNotFound(err) {
		return err
	}
	return nil
}

// hasLivePod reports whether advancing this app's environment now would disturb
// something already running.
//
// A Deployment scaled to zero with no pod left has nothing to protect, and a
// cold start must not come up on an environment older than the CR. Anything that
// has not reached Succeeded or Failed does count, including a Pending pod: it is
// already committed to the current template and will start on it. Reading
// readiness instead would hand an env-only edit an unrequested rollout every
// time an image pull ran long.
//
// The read is authoritative rather than cached, because a stale empty list says
// exactly what a genuinely empty one says and only one of them means it.
func hasLivePod(ctx context.Context, reader client.Reader, deploy *appsv1.Deployment) (bool, error) {
	if deploy.Spec.Replicas != nil && *deploy.Spec.Replicas > 0 {
		return true, nil
	}
	if deploy.Spec.Selector == nil {
		return false, nil
	}

	// The selector alone is not ownership. An App and a Function may share a
	// name in one namespace, and their pods carry the same `app` label, so a
	// selector-matched list can return pods belonging to the other one. Whose
	// pods these are is settled by walking the ReplicaSets this Deployment
	// owns and matching what the pods say controls them.
	var sets appsv1.ReplicaSetList
	if err := reader.List(ctx, &sets, client.InNamespace(deploy.Namespace),
		client.MatchingLabels(deploy.Spec.Selector.MatchLabels)); err != nil {
		return false, err
	}
	// Keyed by UID, not name. Kubernetes owner references carry a UID precisely
	// because names are reused: a replaced ReplicaSet can take the name of one
	// whose pod is still terminating, and matching on the name alone would count
	// that pod as this one's.
	ours := map[types.UID]bool{}
	for i := range sets.Items {
		if metav1.IsControlledBy(&sets.Items[i], deploy) {
			ours[sets.Items[i].UID] = true
		}
	}
	if len(ours) == 0 {
		return false, nil
	}

	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(deploy.Namespace),
		client.MatchingLabels(deploy.Spec.Selector.MatchLabels)); err != nil {
		return false, err
	}
	for i := range pods.Items {
		switch pods.Items[i].Status.Phase {
		case corev1.PodSucceeded, corev1.PodFailed:
			continue
		}
		if ref := metav1.GetControllerOf(&pods.Items[i]); ref != nil && ours[ref.UID] {
			return true, nil
		}
	}
	return false, nil
}

func (r *AppReconciler) reconcileService(ctx context.Context, app *kipperv1.App) error {
	labels := map[string]string{
		"app":       app.Name,
		kipperLabel: kipperValue,
	}

	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
			Labels:    withResourceType(labels, "app"),
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": app.Name},
			Ports: []corev1.ServicePort{
				{Port: app.Spec.Port, TargetPort: intstr.FromInt32(r.serviceTargetPort(app))},
			},
		},
	}

	if err := controllerutil.SetControllerReference(app, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference: %w", err)
	}

	var existing corev1.Service
	err := r.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, &existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	if err := adoptChild("Service", &existing, appOwner(app), r.Scheme); err != nil {
		return err
	}
	// Update ports if changed
	existing.Spec.Ports = desired.Spec.Ports
	existing.Spec.Selector = desired.Spec.Selector
	return r.Update(ctx, &existing)
}

// reconcileEnvSecret renders spec.env into the Secret the app's pods read,
// resolving each ${NAME} against the environment those pods will actually see.
//
// The CR keeps the template and only the Secret and the pod hold what it
// resolved to, which is what lets an operator compose a connection string
// without pasting the password into a field the console shows and `kip export`
// copies.
func (r *AppReconciler) reconcileEnvSecret(ctx context.Context, app *kipperv1.App, sources []envSource) (string, error) {
	table, err := effectiveEnv(ctx, r.Client, app.Namespace, sources)
	if err != nil {
		return "", err
	}
	resolved, diag := renderEnv(app.Spec.Env, table)
	// Recorded in memory; updateStatus persists it at the end of the pass.
	applyEnvResolvedCondition(&app.Status.Conditions, app.Generation, len(app.Spec.Env), diag)

	labels := map[string]string{"app": app.Name, kipperLabel: kipperValue}

	// The whole environment, flattened from the render's output rather than
	// from the table it resolved against, and published as one immutable object
	// the pod template names. Everything after this point reads that name.
	published, err := publishedEnv(ctx, r.Client, app.Namespace, sources, resolved)
	if err != nil {
		return "", err
	}
	generation, err := publishEnvGeneration(ctx, r.Client, r.Scheme, app, secretname.KindApp,
		published, withResourceType(labels, "app"))
	if err != nil {
		return "", err
	}

	// Updated while it exists, and created for nothing new. The banner no longer
	// reads it, so a workload created from here on never gets one, and an
	// existing one is kept current until retirement finds nothing naming it.
	if err := updateLegacyEnvSecret(ctx, r.Client, r.Scheme, app,
		secretname.Env(secretname.KindApp, app.Name), resolved, labels); err != nil {
		return "", err
	}
	return generation, nil
}

// updateLegacyEnvSecret refreshes the pre-generation env Secret and creates
// nothing.
//
// Pods from before the move to published generations still name it, so it stays
// current while any of them can restart. Creating it is a different matter:
// retirement removes it once nothing reads it, and a publication path that
// recreated it would rewrite the object an hour of waiting had just removed, for
// ever. Writing only what is already there lets the conversion finish on its own,
// with no migration to run.
func updateLegacyEnvSecret(ctx context.Context, c client.Client, scheme *runtime.Scheme,
	owner client.Object, secretName string, env, labels map[string]string) error {
	var existing corev1.Secret
	err := c.Get(ctx, types.NamespacedName{Name: secretName, Namespace: owner.GetNamespace()}, &existing)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return reconcileDerivedEnvSecret(ctx, c, scheme, owner, secretName, env, labels)
}

// reconcileDerivedEnvSecret renders a workload's spec.env into the Secret the
// published environment is assembled from — the pods themselves name a
// generation. spec.env is authoritative, so the map is mirrored
// exactly and removing a key — or all of them — clears it rather than leaving
// the old value to be loaded. Ownership is re-asserted on every pass, so a
// direct write that dropped the controller reference is repaired here and the
// Secret stays garbage-collected with its workload.
//
// Shared by all three workload controllers because they must not drift: pods
// read the Secret by name, and a difference in when it is written or cleared
// shows up as a workload running configuration nobody set.
func reconcileDerivedEnvSecret(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, secretName string, env, labels map[string]string) error {
	data := make(map[string][]byte, len(env))
	for k, v := range env {
		data[k] = []byte(v)
	}

	var existing corev1.Secret
	err := c.Get(ctx, types.NamespacedName{Name: secretName, Namespace: owner.GetNamespace()}, &existing)
	if errors.IsNotFound(err) {
		// Nothing to store yet, and no Secret to clear.
		if len(data) == 0 {
			return nil
		}
		desired := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: owner.GetNamespace(),
				Labels:    labels,
				// Stamp the first render so a pod already running when env is first
				// added is correctly reported as needing a restart.
				Annotations: map[string]string{kipperv1.DataUpdatedAtAnnotation: time.Now().Format(time.RFC3339Nano)},
			},
			Data: data,
		}
		if err := controllerutil.SetControllerReference(owner, desired, scheme); err != nil {
			return fmt.Errorf("setting owner reference: %w", err)
		}
		return c.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	ownerOK := metav1.IsControlledBy(&existing, owner)
	if err := controllerutil.SetControllerReference(owner, &existing, scheme); err != nil {
		return fmt.Errorf("setting owner reference: %w", err)
	}
	dataChanged := !bytesMapEqual(existing.Data, data)
	if !dataChanged && ownerOK {
		// Nothing to write: the Secret already matches spec.env and is owned.
		// Skipping the Update avoids needless writes on every reconcile.
		return nil
	}
	// Stamp the change time only when the data actually differs, so the console's
	// restart banner reflects a real env change by any writer (console, kip
	// apply, kip env set) and a no-op reconcile does not raise a false banner.
	if dataChanged {
		if existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		existing.Annotations[kipperv1.DataUpdatedAtAnnotation] = time.Now().Format(time.RFC3339Nano)
	}
	existing.Data = data
	return c.Update(ctx, &existing)
}

func bytesMapEqual(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		bv, ok := b[k]
		if !ok || !bytes.Equal(v, bv) {
			return false
		}
	}
	return true
}

// adoptWriterSecrets sets the App as controller of the credential Secrets that
// CLI and console writers create out-of-band — `app-<app>-secrets`, and
// the app's own git credential when spec.git references one — so deleting the
// App garbage-collects them instead of leaving
// plaintext credentials behind in the namespace. Data is untouched: these
// Secrets have no spec-side source of truth.
//
// Adoption requires the writer's labels, not just the conventional name.
// Secret names are namespace-global, and a name collision with an object
// created by GitOps, an operator, or a human must not convert that object
// into an App child that dies with the App. Every Kipper writer stamps
// `app.kubernetes.io/managed-by: kipper` plus a per-app label, so anything
// without those markers is foreign and stays untouched, as does a Secret
// anything else already owns and a git credentialsSecret that is not this
// app's own (which the builder resolves as a shared credential, or rejects).
func (r *AppReconciler) adoptWriterSecrets(ctx context.Context, app *kipperv1.App) error {
	// name → the label that must carry the app's name alongside managed-by.
	candidates := map[string]string{
		secretname.Secrets(secretname.KindApp, app.Name): "app",
	}
	if app.Spec.Git != nil && secretname.IsGitCredentialOf(app.Name, app.Spec.Git.CredentialsSecret) {
		candidates[app.Spec.Git.CredentialsSecret] = "kipper.run/app"
	}

	for name, appLabel := range candidates {
		var secret corev1.Secret
		err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: app.Namespace}, &secret)
		if errors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if secret.Labels[kipperLabel] != kipperValue || secret.Labels[appLabel] != app.Name {
			continue
		}
		// The same question the writers ask: an object nothing owns is free and
		// one this App already owns stays as it is. Anything else is left,
		// including an object still owned by the incarnation before a delete
		// and recreate, because garbage collection is already entitled to
		// remove it and adopting it would tie this App to an object that can
		// vanish.
		refs, mayOwn := appowner.Take(secret.OwnerReferences,
			appowner.Reference(kipperv1.GroupVersion.String(), app.Name, app.UID))
		if !mayOwn {
			continue
		}
		if equality.Semantic.DeepEqual(refs, secret.OwnerReferences) {
			continue
		}
		secret.OwnerReferences = refs
		if err := r.Update(ctx, &secret); err != nil {
			return fmt.Errorf("adopting %s: %w", name, err)
		}
	}
	return nil
}

// sweepWriterSecrets removes the app's credential Secrets during deletion.
// Adoption normally lets Kubernetes garbage-collect them with the App, but
// an app deleted before its first successful reconcile has adopted nothing —
// without this sweep those plaintext Secrets would outlive it silently.
func (r *AppReconciler) sweepWriterSecrets(ctx context.Context, app *kipperv1.App) error {
	candidates := map[string]string{
		secretname.Secrets(secretname.KindApp, app.Name): "app",
	}
	// Every generation of the credential, not only the one the spec names and
	// not only the name credentials used to have. Each is a plaintext token,
	// and once the App is gone nothing else knows they were its. Listed rather
	// than named because a generation's name carries a digest of its contents,
	// so the set is not derivable from the App alone.
	//
	// Unconditional, not gated on spec.git: an app whose source was detached
	// still has a token on disk if the sweep that removes it has not run yet,
	// and gating this would strand it in the namespace forever. The label and
	// ownership checks below are what keep a stranger's Secret safe.
	var owned corev1.SecretList
	if err := r.List(ctx, &owned, client.InNamespace(app.Namespace), client.MatchingLabels{
		kipperLabel:      kipperValue,
		"kipper.run/app": app.Name,
	}); err != nil {
		return fmt.Errorf("listing git credentials for %s: %w", app.Name, err)
	}
	for i := range owned.Items {
		if secretname.IsGitCredentialOf(app.Name, owned.Items[i].Name) {
			candidates[owned.Items[i].Name] = "kipper.run/app"
		}
	}

	for name, appLabel := range candidates {
		var secret corev1.Secret
		err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: app.Namespace}, &secret)
		if errors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if secret.Labels[kipperLabel] != kipperValue || secret.Labels[appLabel] != app.Name {
			continue
		}
		// Every owner counts, not only the controlling one: collection follows
		// them all, so a Secret anything else owns is not this app's to
		// remove. The same question Take asks before writing.
		if !appowner.OnlyOwnedBy(secret.OwnerReferences, app.UID) {
			continue
		}
		if err := r.Delete(ctx, &secret); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("sweeping %s: %w", name, err)
		}
	}
	return nil
}

// setRouteConflict marks the app's route as unavailable and logs why, so a
// refused hostname surfaces on status instead of failing silently.
func (r *AppReconciler) setRouteConflict(app *kipperv1.App, host, reason string) {
	apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type:    kipperv1.ConditionRouteReady,
		Status:  metav1.ConditionFalse,
		Reason:  "HostUnavailable",
		Message: fmt.Sprintf("route host %q is %s", host, reason),
	})
}

// deleteOwnedIngress removes the app's Ingress when its route is refused, so a
// rejected hostname never leaves a stale router serving.
func (r *AppReconciler) deleteOwnedIngress(ctx context.Context, app *kipperv1.App) error {
	var existing networkingv1.Ingress
	err := r.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, &existing)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	// A refused route must not take somebody else's Ingress with it. The name
	// is conventional, so a collision here is another object, not this app's.
	if !ownedByWorkload(&existing, appOwner(app)) {
		return nil
	}
	if err := r.Delete(ctx, &existing); err != nil && !errors.IsNotFound(err) {
		return err
	}
	return nil
}

func (r *AppReconciler) reconcileIngress(ctx context.Context, app *kipperv1.App) error {
	route := app.Spec.Route

	// Derive the host through the same env source and helper the release
	// ref-count uses, so reserve and release never disagree on an implicit
	// host and free one that is still served.
	host := ""
	if route != nil {
		env, err := resolveNamespaceEnvironment(ctx, r.hostReader(), app.Namespace)
		if err != nil {
			return err
		}
		host = appRouteHost(app, r.Domain, env)
	}

	if route == nil || host == "" {
		// The host claim is sticky and stays held by the project; only the
		// Ingress is torn down when the route goes away.
		if route == nil {
			return r.deleteOwnedIngress(ctx, app)
		}
		return nil
	}

	pathType := networkingv1.PathTypePrefix
	path := "/"
	if route.Path != "" {
		path = route.Path
	}

	// Claim the host cluster-wide before creating the Ingress. A host the
	// platform serves, or one another namespace already owns, is refused: no
	// Ingress is created and the conflict is surfaced on the App's status.
	// Ownership is at the host level, so a different project can't intercept a
	// subset of the traffic with a more specific path.
	reserved, err := hostReservedForPlatform(ctx, r.hostReader(), host, r.Domain)
	if err != nil {
		return err
	}
	if reserved {
		r.setRouteConflict(app, host, "reserved for a platform service")
		return r.deleteOwnedIngress(ctx, app)
	}
	owned, err := reserveHost(ctx, r.hostReader(), r.Client, app.Namespace, host)
	if err != nil {
		return err
	}
	if !owned {
		r.setRouteConflict(app, host, "already claimed by another project")
		return r.deleteOwnedIngress(ctx, app)
	}
	apimeta.RemoveStatusCondition(&app.Status.Conditions, kipperv1.ConditionRouteReady)

	// Redirect-source hosts are resolved only after the canonical host holds
	// its claim. A refused alias degrades the route — it is skipped and
	// reported on RouteReady — while the canonical host and the remaining
	// aliases keep serving.
	aliases, refused, err := resolveRedirectFromHosts(ctx, r.hostReader(), r.Client, app.Namespace, app.Name, r.Domain, canonicalRouteHost(host), route.RedirectFrom)
	if err != nil {
		return err
	}
	if len(refused) > 0 {
		parts := make([]string, len(refused))
		for i, ref := range refused {
			parts[i] = fmt.Sprintf("redirectFrom host %q is %s", ref.host, ref.reason)
		}
		apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
			Type:    kipperv1.ConditionRouteReady,
			Status:  metav1.ConditionFalse,
			Reason:  "RedirectHostUnavailable",
			Message: strings.Join(parts, "; "),
		})
	}

	// kipper.run subdomains route through the gateway, which terminates
	// the public TLS using its own `*.kipper.run` wildcard cert and then
	// proxies HTTPS to the cluster IP with the original hostname as the
	// SNI. Traefik on the cluster still needs a TLS router for that
	// hostname, so we keep `spec.tls.hosts` populated — only the
	// cert-manager annotation is dropped. cert-manager-issued certs are
	// pointless here: the HTTP-01 challenge would hit the gateway and
	// 404, so the Challenge sits pending forever. Without the
	// annotation Traefik falls back to its default cert for that SNI,
	// which is fine because the gateway hop uses InsecureSkipVerify.
	// For custom-domain Ingresses, keep cert-manager — the gateway's
	// wildcard does not cover arbitrary user domains.
	gatewayTLS := strings.HasSuffix(host, ".kipper.run")

	annotations := map[string]string{}
	// Aliases are never gateway hosts, so their certs always come from
	// cert-manager — even when the canonical host is gateway-fronted. The
	// gateway TLS entry carries no secretName, so the annotation requests
	// nothing for it.
	if !gatewayTLS || len(aliases) > 0 {
		annotations["cert-manager.io/cluster-issuer"] = "letsencrypt-prod"
	}

	// Add middleware annotations for security headers and rate limiting
	var middlewareParts []string
	if !route.NoSecurityHeaders {
		middlewareParts = append(middlewareParts, app.Namespace+"-"+app.Name+"-security@kubernetescrd")
	}
	if route.RateLimit > 0 {
		middlewareParts = append(middlewareParts, app.Namespace+"-"+app.Name+"-rate-limit@kubernetescrd")
	} else {
		middlewareParts = append(middlewareParts, "traefik-rate-limit@kubernetescrd")
	}
	// Redirect-source hosts 301 immediately after the rate limit: the
	// redirect stays metered, but fires ahead of strip-prefix and the path
	// redirect rules so a path-based route never rewrites a URL the host
	// redirect is about to replace. redirectRegex only acts when its regex
	// matches, so canonical-host traffic passes these untouched.
	for i := range aliases {
		middlewareParts = append(middlewareParts, app.Namespace+"-"+app.Name+fmt.Sprintf("-redirect-from-%d", i)+"@kubernetescrd")
	}
	// Strip path prefix for non-root paths so the backend receives requests
	// without the routing prefix (e.g. /domains-api/foo → /foo)
	if path != "/" {
		middlewareParts = append(middlewareParts, app.Namespace+"-"+app.Name+"-strip-prefix@kubernetescrd")
	}
	for i := range route.Redirects {
		middlewareParts = append(middlewareParts, app.Namespace+"-"+app.Name+fmt.Sprintf("-redirect-%d", i)+"@kubernetescrd")
	}
	if route.BasicAuth {
		middlewareParts = append(middlewareParts, app.Namespace+"-"+app.Name+"-basic-auth@kubernetescrd")
	}
	// API key enforcement runs last, after BasicAuth and redirects, so a
	// request rejected by an earlier gate never consumes the key's quota.
	// The identity-strip clears any forged X-Kipper-Key-* first; forwardAuth
	// then validates the key and injects the trusted consumer identity; the
	// strip middleware finally removes X-API-Key so it never reaches the
	// backend.
	if route.RequireAPIKey {
		middlewareParts = append(middlewareParts,
			app.Namespace+"-"+app.Name+"-apikey-identity-strip@kubernetescrd",
			app.Namespace+"-"+app.Name+"-apikey@kubernetescrd",
			app.Namespace+"-"+app.Name+"-apikey-strip@kubernetescrd")
	}
	annotations["traefik.ingress.kubernetes.io/router.middlewares"] = strings.Join(middlewareParts, ",")

	// secretName is only meaningful when cert-manager issues a cert.
	// For gateway-fronted hosts we leave it empty so Traefik knows the
	// host should match TLS routing but it falls back to the default
	// cert (which is fine because the gateway hop uses
	// InsecureSkipVerify).
	tlsEntry := networkingv1.IngressTLS{Hosts: []string{host}}
	if !gatewayTLS {
		tlsEntry.SecretName = app.Name + "-tls"
	}
	tls := []networkingv1.IngressTLS{tlsEntry}
	for _, alias := range aliases {
		// One entry and secret per alias, so an alias whose DNS does not
		// point at the cluster yet fails only its own issuance and never
		// blocks the canonical host's cert.
		tls = append(tls, networkingv1.IngressTLS{
			Hosts:      []string{alias},
			SecretName: aliasTLSSecretName(app.Name, alias),
		})
	}

	backend := networkingv1.IngressBackend{
		Service: &networkingv1.IngressServiceBackend{
			Name: app.Name,
			Port: networkingv1.ServiceBackendPort{
				Number: app.Spec.Port,
			},
		},
	}
	rules := []networkingv1.IngressRule{
		{
			Host: host,
			IngressRuleValue: networkingv1.IngressRuleValue{
				HTTP: &networkingv1.HTTPIngressRuleValue{
					Paths: []networkingv1.HTTPIngressPath{
						{
							Path:     path,
							PathType: &pathType,
							Backend:  backend,
						},
					},
				},
			},
		},
	}
	// An alias serves every path, whatever the canonical route's path is:
	// its only job is the host redirect, which fires in the middleware
	// chain before the backend is ever consulted.
	for _, alias := range aliases {
		rules = append(rules, networkingv1.IngressRule{
			Host: alias,
			IngressRuleValue: networkingv1.IngressRuleValue{
				HTTP: &networkingv1.HTTPIngressRuleValue{
					Paths: []networkingv1.HTTPIngressPath{
						{
							Path:     "/",
							PathType: &pathType,
							Backend:  backend,
						},
					},
				},
			},
		})
	}

	desired := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
			Labels: map[string]string{
				"app":       app.Name,
				kipperLabel: kipperValue,
			},
			Annotations: annotations,
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: strPtr("traefik"),
			TLS:              tls,
			Rules:            rules,
		},
	}

	if err := controllerutil.SetControllerReference(app, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference: %w", err)
	}

	var existing networkingv1.Ingress
	err = r.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, &existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	if err := adoptChild("Ingress", &existing, appOwner(app), r.Scheme); err != nil {
		return err
	}
	existing.Spec = desired.Spec
	existing.Annotations = desired.Annotations
	return r.Update(ctx, &existing)
}

func (r *AppReconciler) shouldInjectSidecar(app *kipperv1.App) bool {
	return r.SidecarImage != "" && app.Spec.Route != nil && !app.Spec.Route.NoInstanceHeader
}

func (r *AppReconciler) serviceTargetPort(app *kipperv1.App) int32 {
	if r.shouldInjectSidecar(app) {
		return app.Spec.Port + 10000
	}
	return app.Spec.Port
}

func (r *AppReconciler) buildCSP(allowlist []string) string {
	return buildWorkloadCSP(allowlist)
}

func (r *AppReconciler) reconcileSecurityMiddleware(ctx context.Context, app *kipperv1.App) error {
	route := app.Spec.Route
	mwName := app.Name + "-security"

	// Delete the middleware if the route was removed or security headers
	// were explicitly disabled.
	if route == nil || route.NoSecurityHeaders {
		return r.deleteOwnedMiddleware(ctx, app, mwName)
	}

	csp := r.buildCSP(route.CSPAllowlist)

	desired := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "traefik.io/v1alpha1",
			"kind":       "Middleware",
			"metadata": map[string]interface{}{
				"name":      mwName,
				"namespace": app.Namespace,
				"labels": map[string]interface{}{
					"app":       app.Name,
					kipperLabel: kipperValue,
				},
			},
			"spec": map[string]interface{}{
				"headers": map[string]interface{}{
					"stsSeconds":              int64(31536000),
					"stsIncludeSubdomains":    true,
					"stsPreload":              true,
					"frameDeny":               true,
					"contentTypeNosniff":      true,
					"browserXssFilter":        true,
					"referrerPolicy":          "strict-origin-when-cross-origin",
					"contentSecurityPolicy":   csp,
					"customFrameOptionsValue": "SAMEORIGIN",
					"customResponseHeaders": map[string]interface{}{
						"X-Powered-By": "",
						"Server":       "",
					},
				},
			},
		},
	}

	return r.applyOwnedMiddleware(ctx, app, desired)
}

// reconcileRateLimitMiddleware manages the per-app rateLimit Middleware that
// the Ingress references when Route.RateLimit is set. Burst is twice the
// average and the client is identified by IP, matching the cluster-default
// rate-limit Middleware the installer creates.
func (r *AppReconciler) reconcileRateLimitMiddleware(ctx context.Context, app *kipperv1.App) error {
	route := app.Spec.Route
	mwName := app.Name + "-rate-limit"

	// Delete the middleware if the route was removed or the app fell back
	// to the cluster default.
	if route == nil || route.RateLimit <= 0 {
		return r.deleteOwnedMiddleware(ctx, app, mwName)
	}

	desired := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "traefik.io/v1alpha1",
			"kind":       "Middleware",
			"metadata": map[string]interface{}{
				"name":      mwName,
				"namespace": app.Namespace,
				"labels": map[string]interface{}{
					"app":       app.Name,
					kipperLabel: kipperValue,
				},
			},
			"spec": map[string]interface{}{
				"rateLimit": map[string]interface{}{
					"average": int64(route.RateLimit),
					"burst":   int64(route.RateLimit * 2),
					"period":  "1s",
					"sourceCriterion": map[string]interface{}{
						"ipStrategy": map[string]interface{}{
							"depth": int64(1),
						},
					},
				},
			},
		},
	}

	return r.applyOwnedMiddleware(ctx, app, desired)
}

// reconcileAPIKeyMiddlewares manages the three Middlewares behind
// Route.RequireAPIKey: a headers middleware that strips inbound
// X-Kipper-Key-* so a client cannot forge the consumer identity, a
// forwardAuth that sends the X-API-Key to authz (the gated app's identity
// travels as trusted query parameters written here, never client input) and
// copies the consumer identity authz returns back onto the request, and a
// headers middleware that removes X-API-Key so the key never reaches the
// backend.
func (r *AppReconciler) reconcileAPIKeyMiddlewares(ctx context.Context, app *kipperv1.App) error {
	authName := app.Name + "-apikey"
	stripName := app.Name + "-apikey-strip"
	identityStripName := app.Name + "-apikey-identity-strip"
	route := app.Spec.Route

	if route == nil || !route.RequireAPIKey {
		for _, name := range []string{authName, stripName, identityStripName} {
			if err := r.deleteOwnedMiddleware(ctx, app, name); err != nil {
				return err
			}
		}
		return nil
	}

	authAddress := fmt.Sprintf("http://kipper-authz.kipper-system.svc.cluster.local:8080/authorize?namespace=%s&app=%s",
		url.QueryEscape(app.Namespace), url.QueryEscape(app.Name))

	specs := []struct {
		name string
		spec map[string]interface{}
	}{
		{identityStripName, map[string]interface{}{
			"headers": map[string]interface{}{
				// Clear any client-supplied identity headers before
				// forwardAuth injects the trusted values. Traefik does not
				// remove authResponseHeaders the auth server omits, so
				// without this a client could forge them (e.g. an empty
				// display name).
				"customRequestHeaders": map[string]interface{}{
					"X-Kipper-Key-Prefix": "",
					"X-Kipper-Key-Name":   "",
				},
			},
		}},
		{authName, map[string]interface{}{
			"forwardAuth": map[string]interface{}{
				"address": authAddress,
				// Access-Control-Request-Method lets authz recognise a CORS
				// preflight (browsers cannot send X-API-Key on those) and
				// wave it through to the app. X-Forwarded-For gives authz the
				// client IP for denial forensics; Traefik only trusts it from
				// a configured proxy, so it cannot be spoofed.
				"authRequestHeaders": []interface{}{"X-API-Key", "Access-Control-Request-Method", "X-Forwarded-For"},
				// authz returns the consumer's non-secret identity on an
				// allow; copy it onto the upstream request so the backend
				// knows which key called.
				"authResponseHeaders": []interface{}{"X-Kipper-Key-Prefix", "X-Kipper-Key-Name"},
			},
		}},
		{stripName, map[string]interface{}{
			"headers": map[string]interface{}{
				// An empty value removes the header from the proxied
				// request.
				"customRequestHeaders": map[string]interface{}{
					"X-API-Key": "",
				},
			},
		}},
	}

	for _, mw := range specs {
		desired := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "traefik.io/v1alpha1",
				"kind":       "Middleware",
				"metadata": map[string]interface{}{
					"name":      mw.name,
					"namespace": app.Namespace,
					"labels": map[string]interface{}{
						"app":       app.Name,
						kipperLabel: kipperValue,
					},
				},
				"spec": mw.spec,
			},
		}

		// Owning the gate Middlewares means an out-of-band deletion or edit
		// triggers a reconcile that restores them, so the APIKeyGateReady
		// condition tracks the live gate rather than only the last successful
		// reconcile.
		if err := r.applyOwnedMiddleware(ctx, app, desired); err != nil {
			return err
		}
	}
	return nil
}

func (r *AppReconciler) reconcileStripPrefixMiddleware(ctx context.Context, app *kipperv1.App) error {
	route := app.Spec.Route
	mwName := app.Name + "-strip-prefix"

	// Only needed for non-root paths
	if route == nil || route.Path == "" || route.Path == "/" {
		// Delete if it exists (path was changed back to /)
		return r.deleteOwnedMiddleware(ctx, app, mwName)
	}

	desired := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "traefik.io/v1alpha1",
			"kind":       "Middleware",
			"metadata": map[string]interface{}{
				"name":      mwName,
				"namespace": app.Namespace,
				"labels": map[string]interface{}{
					"app":       app.Name,
					kipperLabel: kipperValue,
				},
			},
			"spec": map[string]interface{}{
				"stripPrefix": map[string]interface{}{
					"prefixes": []interface{}{route.Path},
				},
			},
		},
	}

	return r.applyOwnedMiddleware(ctx, app, desired)
}

func (r *AppReconciler) reconcileRedirectMiddleware(ctx context.Context, app *kipperv1.App) error {
	route := app.Spec.Route

	// List existing redirect middlewares for this app
	var existingList unstructured.UnstructuredList
	existingList.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "traefik.io", Version: "v1alpha1", Kind: "MiddlewareList",
	})
	if err := r.List(ctx, &existingList, client.InNamespace(app.Namespace), client.MatchingLabels{
		"app":                        app.Name,
		"kipper.run/middleware-type": "redirect",
	}); err != nil {
		// CRD may not be installed yet, skip silently
		existingList.Items = nil
	}

	// Determine desired redirect count
	var redirects []kipperv1.RedirectRule
	if route != nil {
		redirects = route.Redirects
	}

	// Create or update desired middlewares
	for i, rule := range redirects {
		mwName := fmt.Sprintf("%s-redirect-%d", app.Name, i)

		// Traefik redirectRegex matches against the full URL including
		// scheme and host. If the user provides a path-only pattern
		// (starting with / or ^/), prepend the URL prefix automatically.
		regex := rule.Source
		if strings.HasPrefix(regex, "/") || strings.HasPrefix(regex, "^/") {
			prefix := "^https?://[^/]+"
			if strings.HasPrefix(regex, "^") {
				regex = prefix + regex[1:]
			} else {
				regex = prefix + regex
			}
		}

		desired := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "traefik.io/v1alpha1",
				"kind":       "Middleware",
				"metadata": map[string]interface{}{
					"name":      mwName,
					"namespace": app.Namespace,
					"labels": map[string]interface{}{
						"app":                        app.Name,
						kipperLabel:                  kipperValue,
						"kipper.run/middleware-type": "redirect",
					},
				},
				"spec": map[string]interface{}{
					"redirectRegex": map[string]interface{}{
						"regex":       regex,
						"replacement": rule.Target,
						"permanent":   rule.Permanent,
					},
				},
			},
		}

		if err := r.applyOwnedMiddleware(ctx, app, desired); err != nil {
			return fmt.Errorf("reconciling redirect middleware %d: %w", i, err)
		}
	}

	// Delete orphaned redirect middlewares (index >= len(redirects))
	for _, item := range existingList.Items {
		name := item.GetName()
		isOrphan := true
		for i := range redirects {
			if name == fmt.Sprintf("%s-redirect-%d", app.Name, i) {
				isOrphan = false
				break
			}
		}
		if isOrphan {
			if err := r.deleteOwnedMiddleware(ctx, app, name); err != nil {
				return fmt.Errorf("deleting orphaned redirect middleware %q: %w", name, err)
			}
		}
	}

	return nil
}

// reconcileRedirectFromMiddleware maintains one redirectRegex middleware per
// accepted redirectFrom host, sending every URL on that host to the same
// path and query on the route's canonical host with a 301. Middlewares are
// named {app}-redirect-from-{i} over the accepted list; orphans beyond it
// are deleted.
func (r *AppReconciler) reconcileRedirectFromMiddleware(ctx context.Context, app *kipperv1.App) error {
	var existingList unstructured.UnstructuredList
	existingList.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "traefik.io", Version: "v1alpha1", Kind: "MiddlewareList",
	})
	if err := r.List(ctx, &existingList, client.InNamespace(app.Namespace), client.MatchingLabels{
		"app":                        app.Name,
		"kipper.run/middleware-type": "redirect-from",
	}); err != nil {
		// CRD may not be installed yet, skip silently
		existingList.Items = nil
	}

	var aliases []string
	host := ""
	if app.Spec.Route != nil && len(app.Spec.Route.RedirectFrom) > 0 {
		env, err := resolveNamespaceEnvironment(ctx, r.hostReader(), app.Namespace)
		if err != nil {
			return err
		}
		host = canonicalRouteHost(appRouteHost(app, r.Domain, env))
		if host != "" {
			accepted, _, err := resolveRedirectFromHosts(ctx, r.hostReader(), r.Client, app.Namespace, app.Name, r.Domain, host, app.Spec.Route.RedirectFrom)
			if err != nil {
				return err
			}
			aliases = accepted
		}
	}

	for i, alias := range aliases {
		mwName := fmt.Sprintf("%s-redirect-from-%d", app.Name, i)
		desired := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "traefik.io/v1alpha1",
				"kind":       "Middleware",
				"metadata": map[string]interface{}{
					"name":      mwName,
					"namespace": app.Namespace,
					"labels": map[string]interface{}{
						"app":                        app.Name,
						kipperLabel:                  kipperValue,
						"kipper.run/middleware-type": "redirect-from",
					},
				},
				"spec": map[string]interface{}{
					"redirectRegex": map[string]interface{}{
						"regex":       "^https?://" + regexp.QuoteMeta(alias) + "/(.*)",
						"replacement": "https://" + host + "/${1}",
						"permanent":   true,
					},
				},
			},
		}

		if err := r.applyOwnedMiddleware(ctx, app, desired); err != nil {
			return fmt.Errorf("reconciling redirect-from middleware %d: %w", i, err)
		}
	}

	for _, item := range existingList.Items {
		name := item.GetName()
		isOrphan := true
		for i := range aliases {
			if name == fmt.Sprintf("%s-redirect-from-%d", app.Name, i) {
				isOrphan = false
				break
			}
		}
		if isOrphan {
			if err := r.deleteOwnedMiddleware(ctx, app, name); err != nil {
				return fmt.Errorf("deleting orphaned redirect-from middleware %q: %w", name, err)
			}
		}
	}

	return nil
}

func (r *AppReconciler) reconcileBasicAuthMiddleware(ctx context.Context, app *kipperv1.App) error {
	mwName := app.Name + "-basic-auth"
	route := app.Spec.Route

	enabled := route != nil && route.BasicAuth

	if !enabled {
		return r.deleteOwnedMiddleware(ctx, app, mwName)
	}

	desired := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "traefik.io/v1alpha1",
			"kind":       "Middleware",
			"metadata": map[string]interface{}{
				"name":      mwName,
				"namespace": app.Namespace,
				"labels": map[string]interface{}{
					"app":       app.Name,
					kipperLabel: kipperValue,
				},
			},
			"spec": map[string]interface{}{
				"basicAuth": map[string]interface{}{
					"secret": app.Name + "-basic-auth",
				},
			},
		},
	}

	return r.applyOwnedMiddleware(ctx, app, desired)
}

func (r *AppReconciler) reconcileHPA(ctx context.Context, app *kipperv1.App) error {
	hpaName := types.NamespacedName{Name: app.Name, Namespace: app.Namespace}

	if app.Spec.Autoscale == nil || !app.Spec.Autoscale.Enabled {
		var existing autoscalingv2.HorizontalPodAutoscaler
		if err := r.Get(ctx, hpaName, &existing); err != nil {
			if errors.IsNotFound(err) {
				return nil
			}
			return err
		}
		// Only this app's own autoscaler. Turning autoscaling off must not
		// delete somebody else's object that happens to share the name.
		if !ownedByWorkload(&existing, appOwner(app)) {
			return nil
		}
		if err := r.Delete(ctx, &existing); err != nil && !errors.IsNotFound(err) {
			return err
		}
		return nil
	}

	as := app.Spec.Autoscale

	var metrics []autoscalingv2.MetricSpec
	if as.CPUTarget > 0 {
		metrics = append(metrics, autoscalingv2.MetricSpec{
			Type: autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{
				Name: "cpu",
				Target: autoscalingv2.MetricTarget{
					Type:               autoscalingv2.UtilizationMetricType,
					AverageUtilization: &as.CPUTarget,
				},
			},
		})
	}
	if as.MemoryTarget > 0 {
		metrics = append(metrics, autoscalingv2.MetricSpec{
			Type: autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{
				Name: "memory",
				Target: autoscalingv2.MetricTarget{
					Type:               autoscalingv2.UtilizationMetricType,
					AverageUtilization: &as.MemoryTarget,
				},
			},
		})
	}

	if len(metrics) == 0 {
		return nil
	}

	labels := map[string]string{
		"app":       app.Name,
		kipperLabel: kipperValue,
	}

	desired := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
			Labels:    labels,
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       app.Name,
			},
			MinReplicas: &as.MinReplicas,
			MaxReplicas: as.MaxReplicas,
			Metrics:     metrics,
		},
	}

	if err := controllerutil.SetControllerReference(app, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference: %w", err)
	}

	var existing autoscalingv2.HorizontalPodAutoscaler
	err := r.Get(ctx, hpaName, &existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	if err := adoptChild("HorizontalPodAutoscaler", &existing, appOwner(app), r.Scheme); err != nil {
		return err
	}
	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	return r.Update(ctx, &existing)
}

// applyServiceBindingCondition records whether every declared binding reached
// the pod. It only edits the in-memory status — the caller's updateStatus
// persists it at the end of the reconcile — because a status write of its own
// would land on every pass and, since the watch accepts status-only updates,
// enqueue the workload again.
//
// A workload with no bindings carries no condition at all: leaving the last
// refusal behind after the binding is removed would report a failure whose
// cause is gone.
//
// Apps and Functions share it, because they now share the policy: a binding
// that cannot be read stops the pass on both.
func applyServiceBindingCondition(conditions *[]metav1.Condition, generation int64, declared int, refused []string) {
	if declared == 0 {
		apimeta.RemoveStatusCondition(conditions, kipperv1.ConditionServiceBindingsReady)
		return
	}
	cond := metav1.Condition{
		Type:               kipperv1.ConditionServiceBindingsReady,
		Status:             metav1.ConditionTrue,
		Reason:             "BindingsInjected",
		Message:            "every declared service binding was injected",
		ObservedGeneration: generation,
	}
	if len(refused) > 0 {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "BindingSecretUnusable"
		cond.Message = "not injected: " + joinWithinConditionMessage(refused)
	}
	apimeta.SetStatusCondition(conditions, cond)
}

// conditionMessageBudget keeps the rendered message well inside the 32 KiB the
// condition schema allows. A CR written directly can declare any number of
// bindings with names of any length, and an oversized message makes the whole
// status update invalid — which would suppress the very condition that exists to
// make this failure visible.
const conditionMessageBudget = 1024

// joinWithinConditionMessage lists as many entries as fit the budget and counts
// the rest.
func joinWithinConditionMessage(entries []string) string {
	var b strings.Builder
	for i, e := range entries {
		sep := ""
		if i > 0 {
			sep = ", "
		}
		if b.Len()+len(sep)+len(e) > conditionMessageBudget {
			fmt.Fprintf(&b, " and %d more", len(entries)-i)
			break
		}
		b.WriteString(sep)
		b.WriteString(e)
	}
	return b.String()
}

// gateStepError marks a failure the API key step already described on this
// pass, so the caller's blanket withdrawal defers to the more specific message.
//
// Deferring on the generation alone is not enough: two passes in one generation
// can stop at different steps, and a pass that never reached the gate must not
// inherit the previous pass's explanation of it.
type gateStepError struct{ err error }

func (e gateStepError) Error() string { return e.err.Error() }
func (e gateStepError) Unwrap() error { return e.err }

// reconcileChildren reconciles every object this App owns, in the order the
// later ones depend on the earlier ones.
//
// Route-derived resources are reconciled unconditionally so that when
// Spec.Route is cleared (toggle off, removed from a route group) the downstream
// Ingress and middlewares are deleted, not orphaned. Each step owns the full
// create/update/delete lifecycle.
//
// A step that fails stops the rest. That is deliberate for a refused child —
// adopting an object Kipper cannot establish as its own would make somebody
// else's object die with this workload — but it means the caller is responsible
// for saying so, because nothing downstream of the failure will run.
func (r *AppReconciler) reconcileChildren(ctx context.Context, app *kipperv1.App, sources []envSource, generation string, bindingHash string) error {
	steps := []struct {
		name string
		fn   func() error
	}{
		{"deployment", func() error { return r.reconcileDeployment(ctx, app, sources, generation, bindingHash) }},
		{"service", func() error { return r.reconcileService(ctx, app) }},
		{"security middleware", func() error { return r.reconcileSecurityMiddleware(ctx, app) }},
		{"rate limit middleware", func() error { return r.reconcileRateLimitMiddleware(ctx, app) }},
		{"api key middlewares", func() error { return r.reconcileAPIKeyMiddlewares(ctx, app) }},
		{"strip prefix middleware", func() error { return r.reconcileStripPrefixMiddleware(ctx, app) }},
		{"redirect middleware", func() error { return r.reconcileRedirectMiddleware(ctx, app) }},
		// Added when this list absorbed the inline block: redirect-from landed on
		// develop after the extraction was written, so the merge would otherwise
		// have stopped reconciling it — silently, since a middleware that is
		// never reconciled is also never deleted when its route is cleared.
		{"redirect-from middleware", func() error { return r.reconcileRedirectFromMiddleware(ctx, app) }},
		{"basic auth middleware", func() error { return r.reconcileBasicAuthMiddleware(ctx, app) }},
		{"ingress", func() error { return r.reconcileIngress(ctx, app) }},
		{"hpa", func() error { return r.reconcileHPA(ctx, app) }},
	}

	for _, step := range steps {
		if err := step.fn(); err != nil {
			if step.name == "api key middlewares" {
				// The forwardAuth gate is not in place. Say so specifically, so the
				// toggle cannot silently leave the route anonymous while this
				// retries. The caller's blanket withdrawal defers to this.
				apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
					Type:               kipperv1.ConditionAPIKeyGateReady,
					Status:             metav1.ConditionFalse,
					Reason:             "MiddlewareReconcileFailed",
					Message:            fmt.Sprintf("the API key gate could not be applied: %v; until it succeeds the route may still be reachable without an API key", err),
					ObservedGeneration: app.Generation,
				})
			}
			wrapped := fmt.Errorf("reconciling %s: %w", step.name, err)
			if step.name == "api key middlewares" {
				return gateStepError{err: wrapped}
			}
			return wrapped
		}
	}
	return nil
}

// withdrawAPIKeyGate takes back the gate claim when a pass stops before the gate
// was reconciled.
//
// updateStatus asserts the gate is engaged on the strength of having been
// reached — the control flow is the whole proof — so a True written under an
// earlier generation must not outlive a pass that stopped short of it. Without
// this, a workload whose security middleware is refused would keep reporting a
// gate that this pass never checked.
//
// A pass that stopped at the gate step itself recorded a more specific False,
// and that one is kept. Any other stopping point means the gate was not looked
// at on this pass, whatever an earlier one concluded.
func (r *AppReconciler) withdrawAPIKeyGate(app *kipperv1.App, cause error) {
	if app.Spec.Route == nil || !app.Spec.Route.RequireAPIKey {
		// Clearing the condition when the toggle is off is updateStatus's job,
		// and updateStatus is downstream of the failure. Doing it here too means
		// switching the gate off while an earlier child is refused does not leave
		// a route reported as gated when it no longer asks to be.
		apimeta.RemoveStatusCondition(&app.Status.Conditions, kipperv1.ConditionAPIKeyGateReady)
		return
	}
	var gateErr gateStepError
	if stderrors.As(cause, &gateErr) {
		return
	}
	apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type:               kipperv1.ConditionAPIKeyGateReady,
		Status:             metav1.ConditionFalse,
		Reason:             "ReconcileIncomplete",
		Message:            "the pass stopped before the API key gate was reconciled; whether the route is gated is unverified",
		ObservedGeneration: app.Generation,
	})
}

// writeStatusIfChanged persists the status only when this pass actually changed
// it.
//
// A workload held by a refused child fails on every pass for as long as the
// object stays refused, and an unconditional write there is not merely wasteful:
// the controller watches its own updates, so each write would enqueue the next
// pass and the failure would spin as fast as the API server allows.
func (r *AppReconciler) writeStatusIfChanged(ctx context.Context, app *kipperv1.App, before *kipperv1.AppStatus) {
	if equality.Semantic.DeepEqual(before, &app.Status) {
		return
	}
	if err := r.Status().Update(ctx, app); err != nil {
		log.FromContext(ctx).Error(err, "recording reconcile status", "app", app.Name, "namespace", app.Namespace)
	}
}

// observeWorkload records what the Deployment currently reports. It edits status
// in memory and leaves persistence to the caller, so a pass that is about to
// fail can still carry the observation into the one status write it makes.
func (r *AppReconciler) observeWorkload(ctx context.Context, app *kipperv1.App) error {
	var deploy appsv1.Deployment
	err := r.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, &deploy)
	if errors.IsNotFound(err) {
		app.Status.Phase = "Pending"
		app.Status.Replicas = 0
		app.Status.ReadyReplicas = 0
		return nil
	}
	if err != nil {
		return err
	}

	app.Status.Replicas = deploy.Status.Replicas
	app.Status.ReadyReplicas = deploy.Status.ReadyReplicas
	app.Status.Image = app.Spec.Image

	switch {
	case deploy.Status.AvailableReplicas > 0:
		app.Status.Phase = "Running"
	case deploy.Status.Replicas == 0:
		app.Status.Phase = "Stopped"
	default:
		app.Status.Phase = "Pending"
	}
	return nil
}

func (r *AppReconciler) updateStatus(ctx context.Context, app *kipperv1.App) error {
	// Reaching updateStatus means every route resource reconciled, so the API
	// key gate is engaged when the toggle is on. Clear the condition when it is
	// off so a stale warning never lingers.
	if app.Spec.Route != nil && app.Spec.Route.RequireAPIKey {
		apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
			Type:               kipperv1.ConditionAPIKeyGateReady,
			Status:             metav1.ConditionTrue,
			Reason:             "GateEngaged",
			Message:            "the API key gate is in place",
			ObservedGeneration: app.Generation,
		})
	} else {
		apimeta.RemoveStatusCondition(&app.Status.Conditions, kipperv1.ConditionAPIKeyGateReady)
	}

	if err := r.observeWorkload(ctx, app); err != nil {
		return err
	}

	return r.Status().Update(ctx, app)
}

func (r *AppReconciler) removeFinalizer(ctx context.Context, app *kipperv1.App) error {
	controllerutil.RemoveFinalizer(app, appFinalizer)
	return r.Update(ctx, app)
}

func buildAppResources(res kipperv1.AppResources) corev1.ResourceRequirements {
	pCPUReq, pCPULim, pMemReq, pMemLim := profileResources(res.Profile)
	cpuReq, cpuLim := ResolveResourcePair(res.CPURequest, res.CPULimit, pCPUReq, pCPULim)
	memReq, memLim := ResolveResourcePair(res.MemoryRequest, res.MemoryLimit, pMemReq, pMemLim)

	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpuReq),
			corev1.ResourceMemory: resource.MustParse(memReq),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpuLim),
			corev1.ResourceMemory: resource.MustParse(memLim),
		},
	}
}

// AppRequests returns the CPU (millicores) and memory (bytes) one replica of
// an app requests, resolved exactly as the reconciler builds its container.
// The migration capacity precheck uses it to size demand from specs rather
// than from live pods, which vanish when the operator freezes writes.
func AppRequests(res kipperv1.AppResources) (cpuMillis, memBytes int64) {
	reqs := buildAppResources(res).Requests
	return reqs.Cpu().MilliValue(), reqs.Memory().Value()
}

// profileResources returns the (cpuRequest, cpuLimit, memoryRequest, memoryLimit)
// for a profile. Most profiles run at request==limit (Guaranteed QoS). The jvm
// profile is burstable — low request, high limit — so cold-start JIT can use
// a full core without reserving one on the node.
func profileResources(profile string) (cpuReq, cpuLim, memReq, memLim string) {
	switch profile {
	case "lightweight":
		return "50m", "50m", "64Mi", "64Mi"
	case "standard":
		return "100m", "100m", "128Mi", "128Mi"
	case "compute-heavy":
		return "500m", "500m", "256Mi", "256Mi"
	case "memory-heavy":
		return "100m", "100m", "512Mi", "512Mi"
	case "jvm":
		return "100m", "1000m", "2Gi", "2Gi"
	case "database":
		// Matches the resource controller's "database" floor
		// (profileDefaults) so services default and adjust consistently.
		return "500m", "500m", "1Gi", "1Gi"
	default:
		return "100m", "100m", "128Mi", "128Mi"
	}
}

// podAnnotations returns the pod-template annotations that decide when a
// workload's pods are replaced.
//
// kipper.run/restartedAt carries an explicit restart request from the CR, which
// is what turns `kip app restart` into a rolling replacement.
//
// bindingHash digests the service credentials this workload derives. A rotated
// password is not stale configuration an operator can apply at their
// convenience: the old value stops working the moment the service accepts the
// new one, so a pod holding it is already broken. Putting the digest in the pod
// template rolls those pods automatically. Plain env and own-secret edits
// deliberately do not do this — they keep the restart banner, so a live service
// is never cycled without someone asking.
func podAnnotations(crAnnotations map[string]string, bindingHash string) map[string]string {
	out := map[string]string{}
	if restartedAt, ok := crAnnotations["kipper.run/restartedAt"]; ok {
		out["kipper.run/restartedAt"] = restartedAt
	}
	if bindingHash != "" {
		out["kipper.run/binding-credentials"] = bindingHash
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func strPtr(s string) *string {
	return &s
}

// enqueueAppsForGitCredential maps a per-app git credential back to its App, so
// the sweep sees an object the App has never named.
func (r *AppReconciler) enqueueAppsForGitCredential(_ context.Context, obj client.Object) []reconcile.Request {
	marks := obj.GetLabels()
	if marks[kipperLabel] != kipperValue {
		return nil
	}
	app := marks["kipper.run/app"]
	if app == "" || !secretname.IsGitCredentialOf(app, obj.GetName()) {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: app},
	}}
}

// SetupWithManager registers the App reconciler with the controller manager.
func (r *AppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Watch the Traefik Middlewares behind the API key gate so deleting or
	// editing one out-of-band triggers a reconcile that restores it. The gate
	// Middlewares carry an App owner reference (set in reconcileAPIKeyMiddlewares);
	// this Owns filters to those.
	gateMiddleware := &unstructured.Unstructured{}
	gateMiddleware.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "traefik.io", Version: "v1alpha1", Kind: "Middleware",
	})
	// Links name a target by namespace and app, and the allowance they open
	// follows that target's port. Indexing them is what lets a change to the
	// target reach the apps that depend on it.
	if err := IndexAppLinks(context.Background(), mgr.GetFieldIndexer()); err != nil {
		return err
	}
	if err := IndexAppLinkNamespaces(context.Background(), mgr.GetFieldIndexer()); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&kipperv1.App{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Owns(&networkingv1.Ingress{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		// The link policy is a security resource whose whole value is that it
		// says what it says; deleting or widening it out of band must be undone
		// rather than waiting for the next unrelated event on the caller.
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(gateMiddleware).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.enqueueForRegistryCredentials)).
		// A rotated service password has to reach the bindings that derive from
		// it, rather than waiting for something unrelated to touch each app.
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.enqueueAppsForServiceCredentials)).
		// A git credential is created before the App names it, so one left
		// behind by a write that then failed carries no owner reference and no
		// Owns reaches it. Without this the only thing that collects it is a
		// reconcile the App happens to get for another reason.
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.enqueueAppsForGitCredential)).
		Watches(&kipperv1.ClusterIdentity{}, handler.EnqueueRequestsFromMapFunc(r.enqueueAppsForClusterIdentity)).
		Watches(&kipperv1.App{}, handler.EnqueueRequestsFromMapFunc(r.enqueueCallersOfLinkTarget)).
		// Consent is the target project's decision, and withdrawing it has to
		// actually close the paths it authorised rather than wait for something
		// unrelated to touch each caller.
		Watches(&kipperv1.Project{}, handler.EnqueueRequestsFromMapFunc(r.enqueueCallersOfProject)).
		Complete(r)
}

// sweepGitCredentials removes the git credentials an app owns that its live
// spec does not name.
//
// A credential is one object per token-and-host pair, so a rotation leaves the
// previous one behind and a write whose CR update failed leaves one nothing
// ever referenced. Both are plaintext tokens, so something has to collect them.
// Detaching git names nothing at all, which is how every generation goes.
//
// Only a Secret carrying the writer labels is considered, because names are
// namespace-global and a collision must not turn a stranger's object into this
// app's to delete. And only one nothing else owns: an owner that does not
// control still governs collection, so an object co-owned with another actor is
// not this app's to remove.
//
// A credential younger than the grace period is left alone whatever the spec
// says: the object is created before the CR update that names it, so a sweep
// racing a rotation would otherwise delete the credential that update is about
// to point at. What was held back decides how long to wait before looking
// again. A Secret event does reach this App, but a grace expiring raises no
// event at all, so without the requeue a skipped object waits for an unrelated
// reason to reconcile that may never come.
//
// Idempotent by construction: a missing Secret is the goal state, so a pass
// that half-succeeded is simply run again by the next reconcile.
func (r *AppReconciler) sweepGitCredentials(ctx context.Context, app *kipperv1.App) (time.Duration, error) {
	const grace = 2 * time.Minute

	var secrets corev1.SecretList
	if err := r.List(ctx, &secrets, client.InNamespace(app.Namespace), client.MatchingLabels{
		kipperLabel:      kipperValue,
		"kipper.run/app": app.Name,
	}); err != nil {
		return 0, fmt.Errorf("listing git credentials for %s: %w", app.Name, err)
	}

	var retryIn time.Duration
	named := ""
	if app.Spec.Git != nil {
		named = app.Spec.Git.CredentialsSecret
	}

	for i := range secrets.Items {
		secret := &secrets.Items[i]
		if !secretname.IsGitCredentialOf(app.Name, secret.Name) || secret.Name == named {
			continue
		}
		// The later of creation and the last claim. A writer that finds the
		// object already there reuses it, so age alone would let a rotation
		// back to a pair the app recently held be swept out from under the
		// commit that is naming it.
		since := secret.CreationTimestamp.Time
		// A claim says a writer is committing the App onto this object now, so
		// one further ahead than the grace is not a claim at all: a machine
		// with a wild clock, or a hand-edited annotation. Trusting it would
		// hold a plaintext token for as long as it says, because every pass
		// would measure the grace afresh from a time that never arrives.
		if claimed, err := time.Parse(time.RFC3339, secret.Annotations[labels.AnnoGitCredentialClaimed]); err == nil &&
			claimed.After(since) && !claimed.After(time.Now().Add(grace)) {
			since = claimed
		}
		if held := grace - time.Since(since); held > 0 {
			if retryIn == 0 || held < retryIn {
				retryIn = held
			}
			continue
		}
		if !appowner.OnlyOwnedBy(secret.OwnerReferences, app.UID) {
			continue
		}
		// Preconditioned on what this pass read, so a claim landing between the
		// read and the delete refuses it rather than losing the race silently.
		if err := r.Delete(ctx, secret, client.Preconditions{ResourceVersion: &secret.ResourceVersion}); err != nil &&
			!errors.IsNotFound(err) && !errors.IsConflict(err) {
			return 0, fmt.Errorf("removing the superseded git credential %s: %w", secret.Name, err)
		}
	}
	return retryIn, nil
}

// clearDetachedBuildStatus drops a build result belonging to a source that is
// gone, and reports whether it changed anything.
//
// A build status outliving its source makes the console report a failed build
// for an app that has no builds, which is what an operator saw after switching
// an app from git to a prebuilt image.
func (r *AppReconciler) clearDetachedBuildStatus(ctx context.Context, app *kipperv1.App) error {
	if !detachedBuildStatus(app) {
		return nil
	}
	app.Status.Build = nil
	// Written here rather than left for the status write at the end of the
	// pass. This runs before the steps that can fail for unrelated reasons, and
	// an app failing one of those never reaches that write — so the console
	// would go on reporting a failed build for an app that has no builds, for
	// as long as the unrelated fault lasts.
	return r.Status().Update(ctx, app)
}

// detachedBuildStatus reports whether a build result belongs to a source that
// is gone.
func detachedBuildStatus(app *kipperv1.App) bool {
	return app.Spec.Git == nil && app.Status.Build != nil
}
