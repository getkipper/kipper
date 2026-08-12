package controllers

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/domain"
	"github.com/getkipper/kipper/controller/pkg/secretname"
	"github.com/getkipper/kipper/controller/pkg/workload"
)

// hasHTTPTrigger returns true when the function should have HTTP
// infrastructure (Deployment + Service + HTTPScaledObject + Ingress).
//
// Functions with no triggers default to HTTP for backward compatibility.
// Cron is the only trigger type that does not require HTTP — every other
// trigger (postgres, redis, mysql, mongodb, rabbitmq, opensearch, minio)
// is event-polled by the kipper-poll sidecar and invokes the function
// over HTTP. So HTTP infrastructure is needed unless every trigger is
// explicitly cron.
func hasHTTPTrigger(fn *kipperv1.Function) bool {
	if len(fn.Spec.Triggers) == 0 {
		return true
	}
	for _, t := range fn.Spec.Triggers {
		if t.Type != "cron" {
			return true
		}
	}
	return false
}

// cronTrigger returns the first cron trigger on the function, or nil if
// none. v1 supports a single cron trigger per function; if users need
// multiple schedules they can create multiple functions sharing the
// same source code.
func cronTrigger(fn *kipperv1.Function) *kipperv1.FunctionTrigger {
	for i := range fn.Spec.Triggers {
		if fn.Spec.Triggers[i].Type == "cron" {
			return &fn.Spec.Triggers[i]
		}
	}
	return nil
}

// eventTrigger returns the first non-HTTP, non-cron trigger (postgres,
// mysql, redis, minio, etc.). These are the trigger types served by the
// kipper-poll sidecar + KEDA ScaledObject path.
func eventTrigger(fn *kipperv1.Function) *kipperv1.FunctionTrigger {
	for i := range fn.Spec.Triggers {
		t := fn.Spec.Triggers[i].Type
		if t != "" && t != "http" && t != "cron" {
			return &fn.Spec.Triggers[i]
		}
	}
	return nil
}

// kipperPollImage is the per-function poller sidecar that bridges
// event sources (postgres rows, redis lists, minio objects) into HTTP
// invocations of the function's /event endpoint.
const kipperPollImage = "ghcr.io/getkipper/kipper-poll:latest"

// serviceConn holds the bits of a bound Service we need to wire up an
// event trigger. Populated from the Service CR's spec and its
// credentials Secret.
type serviceConn struct {
	Type     string
	Host     string
	Port     int32
	Username string
	Password string
	Database string
}

// URL builds a connection URL of the right shape for the service type.
// Mirrors the kip-side service.formatURL logic.
func (c *serviceConn) URL() string {
	switch c.Type {
	case "postgres":
		return fmt.Sprintf("postgres://%s:%s@%s:%d/%s", c.Username, c.Password, c.Host, c.Port, c.Database)
	case "mysql":
		return fmt.Sprintf("mysql://%s:%s@%s:%d/%s", c.Username, c.Password, c.Host, c.Port, c.Database)
	case "mongodb":
		return fmt.Sprintf("mongodb://%s:%s@%s:%d/%s?authSource=admin", c.Username, c.Password, c.Host, c.Port, c.Database)
	case "redis":
		return fmt.Sprintf("redis://%s:%d", c.Host, c.Port)
	case "minio":
		return fmt.Sprintf("http://%s:%s@%s:%d", c.Username, c.Password, c.Host, c.Port)
	}
	return ""
}

const functionFinalizer = "kipper.run/function-cleanup"

// FunctionReconciler reconciles a Function CR.
type FunctionReconciler struct {
	client.Client
	// APIReader is an uncached reader for ownership decisions (route-host
	// reservations) that must not be made from a possibly-stale cache.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Domain    string
	// Recorder puts a reconcile failure on the Function itself, where `kubectl
	// describe` finds it without anyone tailing the controller's log. Nil in
	// unit tests.
	Recorder record.EventRecorder
}

func (r *FunctionReconciler) hostReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *FunctionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, retErr error) {
	logger := log.FromContext(ctx)

	var fn kipperv1.Function
	if err := r.Get(ctx, req.NamespacedName, &fn); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	statusAtEntry := fn.Status.DeepCopy()
	// Every failure below leaves the function part-reconciled, so it belongs on
	// the object rather than only in a log line. The API server folds repeats of
	// the same reason and message into one event with a count.
	defer func() {
		if retErr != nil && r.Recorder != nil {
			r.Recorder.Event(&fn, corev1.EventTypeWarning, "ReconcileFailed", retErr.Error())
		}
	}()

	if !fn.DeletionTimestamp.IsZero() {
		logger.Info("cleaning up function resources", "function", fn.Name)
		if err := r.sweepWriterSecret(ctx, &fn); err != nil {
			return ctrl.Result{}, fmt.Errorf("sweeping writer secret: %w", err)
		}
		if err := r.cleanupSharedNamespaceObjects(ctx, &fn); err != nil {
			return ctrl.Result{}, err
		}
		// The host claim is sticky and is never released here. It stays held by
		// the project until another project reserves the host once this project
		// is gone (reserveHost then takes it over — see host_reservation.go), so
		// deleting a single function never frees its host for another project.
		controllerutil.RemoveFinalizer(&fn, functionFinalizer)
		return ctrl.Result{}, r.Update(ctx, &fn)
	}

	// A CR written straight to the API server never passed a reservation, so
	// this is where that collision is caught. Stopping is the point: an App and
	// a Function contend on the Deployment further down, but an App and a Job do
	// not, and nothing else would refuse them.
	if heldBy, claimErr := reconcileNameClaim(ctx, r.Client, r.hostReader(), r.Scheme, &fn, "function"); claimErr != nil {
		return ctrl.Result{}, claimErr
	} else if heldBy != "" {
		apimeta.SetStatusCondition(&fn.Status.Conditions, metav1.Condition{
			Type:               kipperv1.ConditionChildrenAdopted,
			Status:             metav1.ConditionFalse,
			Reason:             "NameHeldByAnotherWorkload",
			Message:            blockedMessage(fn.Name, heldBy),
			ObservedGeneration: fn.Generation,
		})
		fn.Status.Phase = "Failed"
		r.writeStatusIfChanged(ctx, &fn, statusAtEntry)
		return ctrl.Result{}, workload.NameTakenError{Name: fn.Name, Kind: heldBy}
	}

	if !controllerutil.ContainsFinalizer(&fn, functionFinalizer) {
		controllerutil.AddFinalizer(&fn, functionFinalizer)
		if err := r.Update(ctx, &fn); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Reconcile inline code ConfigMap before the workloads so the volume
	// mount is ready for both the HTTP Deployment and the cron CronJob.
	if fn.Spec.Source != nil && fn.Spec.Source.Code != "" {
		if err := r.reconcileCodeConfigMap(ctx, &fn); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconciling code configmap: %w", err)
		}
	}

	// Derive per-binding credentials before the env Secret, for the same reason
	// the App reconciler does: the env render resolves against them.
	rendered, keepProjections, bindingHash, err := reconcileBindingSecrets(ctx, r.Client, r.Scheme, &fn, secretname.KindFunction, fn.Spec.ServiceBindings)
	if err != nil {
		// Swept before the status write, so the conversion sentence it computes
		// is persisted with the refusal rather than mutated into a copy this
		// pass is about to discard. A binding can stay broken indefinitely, so
		// this is not a one-pass delay.
		r.sweepEnv(ctx, &fn, fn.Status.PublishedEnv,
			declaredProjectionNames(fn.Spec.ServiceBindings, secretname.KindFunction, fn.Name))

		applyServiceBindingCondition(&fn.Status.Conditions, fn.Generation, len(fn.Spec.ServiceBindings), []string{err.Error()})
		if statusErr := r.Status().Update(ctx, &fn); statusErr != nil {
			logger.Error(statusErr, "recording refused service bindings")
		}
		return ctrl.Result{}, fmt.Errorf("reconciling binding secrets: %w", err)
	}

	// Refuse once, here, as the App does. A Function used to skip a binding it
	// could not read and render the environment without it, so ${DB_PASSWORD}
	// reached the process as written and the pod failed on its first connection
	// with nothing naming the binding. Stopping keeps the last good template
	// while the cause is reported and the reconcile retries.
	envSources, refusedBindings, err := functionEnvSources(ctx, r.Client, &fn, rendered)
	if err != nil {
		// Report it too. A lookup that failed leaves the last answer standing,
		// and the last answer may be that everything was fine.
		// Swept before the status write, so the conversion sentence it computes
		// is persisted with the refusal rather than mutated into a copy this
		// pass is about to discard. A binding can stay broken indefinitely, so
		// this is not a one-pass delay.
		r.sweepEnv(ctx, &fn, fn.Status.PublishedEnv,
			declaredProjectionNames(fn.Spec.ServiceBindings, secretname.KindFunction, fn.Name))

		applyServiceBindingCondition(&fn.Status.Conditions, fn.Generation, len(fn.Spec.ServiceBindings), []string{err.Error()})
		if statusErr := r.Status().Update(ctx, &fn); statusErr != nil {
			logger.Error(statusErr, "recording refused service bindings")
		}
		return ctrl.Result{}, fmt.Errorf("resolving service bindings: %w", err)
	}
	applyServiceBindingCondition(&fn.Status.Conditions, fn.Generation, len(fn.Spec.ServiceBindings), refusedBindings)
	if len(refusedBindings) > 0 {
		logger.Info("service bindings refused; leaving the function on its current configuration",
			"function", fn.Name, "namespace", fn.Namespace, "refused", refusedBindings)
		// Swept before this write like the other two refusal paths, so the
		// conversion sentence lands with the refusal instead of in a copy this
		// pass discards.
		r.sweepEnv(ctx, &fn, fn.Status.PublishedEnv,
			declaredProjectionNames(fn.Spec.ServiceBindings, secretname.KindFunction, fn.Name))

		if statusErr := r.Status().Update(ctx, &fn); statusErr != nil {
			logger.Error(statusErr, "recording refused service bindings")
		}
		return ctrl.Result{}, fmt.Errorf("service bindings refused: %s", strings.Join(refusedBindings, "; "))
	}

	// Materialise Spec.Env into a Secret named function-<fn>-env. The container's
	// EnvFrom references this Secret with Optional=true, so a missing
	// Secret leaves the env vars unset rather than failing the deploy.
	generation, err := r.reconcileEnvSecret(ctx, &fn, envSources)
	if recErr := recordPublication(ctx, r.Client, &fn, &fn.Status.Conditions,
		&fn.Status.PublishedEnv, fn.Generation, generation, err); recErr != nil {
		logger.Error(recErr, "recording the published environment")
	}
	if err != nil {
		if statusErr := r.Status().Update(ctx, &fn); statusErr != nil {
			logger.Error(statusErr, "recording a failed environment publication")
		}
		return ctrl.Result{}, fmt.Errorf("reconciling env secret: %w", err)
	}

	if err := r.adoptWriterSecret(ctx, &fn); err != nil {
		return ctrl.Result{}, fmt.Errorf("adopting writer secret: %w", err)
	}

	if err := r.reconcileChildren(ctx, &fn, bindingHash, envSources, generation, rendered); err != nil {
		// The pass stops here and everything after it is skipped, including the
		// status write, so what stopped it has to go on the object. A function
		// whose name another kind already owns is refused on every pass for
		// ever, and reporting only into the log left it showing an empty phase
		// (which reads as idle), an unrelated true condition, and a URL that
		// 404s. The message names the object and its owner, which is the remedy.
		apimeta.SetStatusCondition(&fn.Status.Conditions, metav1.Condition{
			Type:               kipperv1.ConditionChildrenAdopted,
			Status:             metav1.ConditionFalse,
			Reason:             "ChildReconcileFailed",
			Message:            err.Error(),
			ObservedGeneration: fn.Generation,
		})
		fn.Status.Phase = "Failed"
		// Before the status write, as the other refusal paths above do, so what
		// the sweep learns lands with the refusal rather than in a copy this
		// pass discards. A blocked function persists until someone renames it,
		// so skipping the sweep would strand superseded env Secrets for as long.
		r.sweepEnv(ctx, &fn, generation, keepProjections)
		r.writeStatusIfChanged(ctx, &fn, statusAtEntry)
		return ctrl.Result{}, err
	}
	apimeta.SetStatusCondition(&fn.Status.Conditions, metav1.Condition{
		Type:               kipperv1.ConditionChildrenAdopted,
		Status:             metav1.ConditionTrue,
		Reason:             "AllChildrenAdopted",
		Message:            "every child this workload owns reconciled",
		ObservedGeneration: fn.Generation,
	})

	// Before the status write, so what the sweep learns about this workload's
	// conversion is persisted rather than mutated into a copy the pass discards.
	retryIn := r.sweepEnv(ctx, &fn, generation, keepProjections)

	if err := r.updateStatus(ctx, &fn); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	return ctrl.Result{RequeueAfter: retryIn}, nil
}

// reconcileChildren brings every object this function owns into line: the
// serving path for an HTTP trigger, the KEDA objects for an event trigger, and
// the CronJob for a cron one, each cleaning up what an earlier spec left behind.
//
// Any child that fails stops the pass, so the caller records why on the status
// rather than only in the log.
func (r *FunctionReconciler) reconcileChildren(ctx context.Context, fn *kipperv1.Function, bindingHash string, envSources []envSource, generation string, rendered renderedBindings) error {
	// HTTP path: Deployment + Service + KEDA HTTPScaledObject + Ingress.
	// Cron-only functions skip all of this — they have no need for a
	// long-lived Pod, an HTTP service, or external routing.
	if hasHTTPTrigger(fn) {
		if err := r.reconcileDeployment(ctx, fn, bindingHash, envSources, generation, rendered); err != nil {
			return fmt.Errorf("reconciling deployment: %w", err)
		}
		if err := r.reconcileService(ctx, fn); err != nil {
			return fmt.Errorf("reconciling service: %w", err)
		}
		// HTTPScaledObject and Ingress are managed via unstructured client
		// since KEDA types are not in our scheme.
		if err := r.reconcileHTTPScaledObject(ctx, fn); err != nil {
			return fmt.Errorf("reconciling http scaled object: %w", err)
		}
		if err := r.reconcileIngress(ctx, fn); err != nil {
			return fmt.Errorf("reconciling ingress: %w", err)
		}
	} else if err := r.cleanupHTTPServing(ctx, fn); err != nil {
		// A function edited from HTTP to cron-only no longer serves a route.
		// Without this its host claim and shared keda Ingress would leak,
		// locking the host against other projects until an unrelated reconcile
		// in the namespace happened to sweep it.
		return fmt.Errorf("cleaning up http serving: %w", err)
	}

	// Event path: postgres, mysql, redis, minio triggers. The function
	// gets a kipper-poll sidecar in its Pod (added during
	// reconcileDeployment above) and a KEDA ScaledObject scales the
	// Deployment based on the source. Postgres/MySQL also need a KEDA
	// TriggerAuthentication referencing the bound Service's credentials.
	if t := eventTrigger(fn); t != nil {
		if err := r.reconcileEventScaledObject(ctx, fn, t); err != nil {
			return fmt.Errorf("reconciling event scaled object: %w", err)
		}
		if t.Type == "postgres" || t.Type == "mysql" {
			if err := r.reconcileTriggerAuth(ctx, fn, t); err != nil {
				return fmt.Errorf("reconciling trigger auth: %w", err)
			}
		}
	} else {
		// No event trigger — clean up any stale KEDA objects from a
		// previous spec.
		for _, stale := range []struct{ kind, name string }{
			{"ScaledObject", fn.Name},
			{"TriggerAuthentication", fn.Name + "-trigger-auth"},
		} {
			if err := r.deleteOwnedKEDAObject(ctx, fn, stale.kind, stale.name); err != nil {
				return fmt.Errorf("deleting stale %s: %w", stale.kind, err)
			}
		}
	}

	// Cron path: a CronJob runs the function image in batch mode on the
	// configured schedule. Independent of the HTTP path — a function may
	// have both, in which case both run.
	if t := cronTrigger(fn); t != nil {
		if err := r.reconcileCronJob(ctx, fn, t, envSources, generation); err != nil {
			return fmt.Errorf("reconciling cron job: %w", err)
		}
		return nil
	}

	// No cron trigger on the spec — clean up any stale CronJob left behind from
	// an earlier revision that did have one.
	var stale batchv1.CronJob
	err := r.Get(ctx, types.NamespacedName{Name: fn.Name + "-cron", Namespace: fn.Namespace}, &stale)
	switch {
	case errors.IsNotFound(err):
		// Nothing left over.
	case err != nil:
		return fmt.Errorf("checking for stale cron job: %w", err)
	case ownedByWorkload(&stale, functionOwner(fn)):
		if delErr := r.Delete(ctx, &stale); delErr != nil && !errors.IsNotFound(delErr) {
			return fmt.Errorf("deleting stale cron job: %w", delErr)
		}
	}
	return nil
}

// writeStatusIfChanged persists status only when this pass actually changed it.
//
// The controller watches Functions without a status-change predicate, so an
// unconditional write on a permanently blocked function would enqueue itself
// again on every pass and spin rather than back off. The App reconciler carries
// the same guard for the same reason.
func (r *FunctionReconciler) writeStatusIfChanged(ctx context.Context, fn *kipperv1.Function, before *kipperv1.FunctionStatus) {
	if equality.Semantic.DeepEqual(before, &fn.Status) {
		return
	}
	if err := r.Status().Update(ctx, fn); err != nil {
		log.FromContext(ctx).Error(err, "recording reconcile status", "function", fn.Name, "namespace", fn.Namespace)
	}
}

// sweepEnv retires what this function has moved off, and reports how long to
// wait before looking again. It never fails the pass: not sweeping leaves
// Secrets behind and nothing else, while failing here would undo a publication
// that succeeded.
//
// The failure paths above call it too, treating every declared binding as
// wanted. A pass that stops early used to leave a projection whose binding was
// removed with nothing scheduled to look at it again.
func (r *FunctionReconciler) sweepEnv(ctx context.Context, fn *kipperv1.Function, generation string, keep map[string]bool) time.Duration {
	retryIn, legacyReaders, err := retireEnvSecrets(ctx, r.Client, r.hostReader(), fn, secretname.KindFunction, generation, keep)
	if err != nil {
		log.FromContext(ctx).Error(err, "retiring superseded environments", "function", fn.Name)
		return envRetirementRetry
	}
	applyEnvPublishedConditionWithConversion(&fn.Status.Conditions, fn.Generation, nil, legacyReaders)
	return retryIn
}

func runtimeImage(runtime string) string {
	switch runtime {
	case "node":
		return "ghcr.io/getkipper/kipper-runtime-node:latest"
	case "python":
		return "ghcr.io/getkipper/kipper-runtime-python:latest"
	default:
		return ""
	}
}

// effectiveFunctionImage is the image a Function's container actually runs:
// the explicit spec image, or the runtime image for inline functions. Pull
// Secret staging resolves against this, so every path (HTTP Deployment, cron
// Job, test run) agrees on which registry the workload pulls from.
func effectiveFunctionImage(fn *kipperv1.Function) string {
	if fn.Spec.Image == "" && fn.Spec.Runtime != "" {
		return runtimeImage(fn.Spec.Runtime)
	}
	return fn.Spec.Image
}

func runtimeHandler(runtime string) (filename, path string) {
	switch runtime {
	case "node":
		return "index.js", "/app/function/index.js"
	case "python":
		return "handler.py", "/app/function/handler.py"
	default:
		return "handler", "/app/function/handler"
	}
}

func (r *FunctionReconciler) reconcileDeployment(ctx context.Context, fn *kipperv1.Function, bindingHash string, envSources []envSource, generation string, rendered renderedBindings) error {
	labels := functionLabels(fn)
	zero := int32(0)

	port := fn.Spec.Port
	if port == 0 {
		port = 8080
	}

	image := effectiveFunctionImage(fn)
	isInline := fn.Spec.Source != nil && fn.Spec.Source.Code != ""

	if isInline {
		labels["kipper.run/runtime"] = fn.Spec.Runtime
	}

	container := corev1.Container{
		Name:  fn.Name,
		Image: image,
		Ports: []corev1.ContainerPort{{ContainerPort: port}},
	}

	// For inline functions, mount the code ConfigMap. The path the runtime looks
	// for the handler at comes through the environment below, like every other
	// variable the pod sees.
	if isInline {
		container.VolumeMounts = []corev1.VolumeMount{
			{Name: "function-code", MountPath: "/app/function"},
		}
	}

	container.Resources = functionResources(fn.Spec.Resources)

	// The env Secret, the function's own secrets and every accepted service
	// binding, in the order the kubelet applies them, then the variables the
	// platform sets on the container itself.
	sources := withFunctionRuntime(fn, envSources, functionServing, "")
	container.EnvFrom = envFrom(generation)
	container.Env = directEnv(sources)

	// Mount any shared Volume CRs the function declares. These are
	// per-volume PersistentVolumeClaims (created by kip volume create).
	for _, vm := range fn.Spec.Volumes {
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      vm.Name,
			MountPath: vm.MountPath,
		})
	}

	containers := []corev1.Container{container}

	// Inject the kipper-poll sidecar for event triggers (postgres, mysql,
	// redis, minio). The sidecar polls the event source and POSTs each
	// event to the function's /event endpoint on localhost.
	if t := eventTrigger(fn); t != nil {
		if sidecar, err := r.buildPollSidecar(ctx, fn, t, port, rendered); err == nil {
			containers = append(containers, sidecar)
		} else {
			log.FromContext(ctx).Info("event trigger sidecar skipped", "function", fn.Name, "reason", err.Error())
		}
	}

	podSpec := corev1.PodSpec{
		Containers: containers,
	}

	// Mount code volume for inline functions
	if isInline {
		podSpec.Volumes = []corev1.Volume{
			{
				Name: "function-code",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: fn.Name + "-code",
						},
					},
				},
			},
		}
	}

	// Mount shared Volumes declared on the function. The PVC name follows
	// the same convention as App: shared-<volume-name>.
	for _, vm := range fn.Spec.Volumes {
		podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
			Name: vm.Name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: "shared-" + vm.Name,
				},
			},
		})
	}

	// Image pull secret for a private third-party image (staged, owned by the
	// fn). Resolved against the image the container actually runs — the
	// computed one, so an inline function's runtime image is covered too.
	pullSecrets, err := ensureImagePullSecret(ctx, r.Client, r.Scheme, fn, image)
	if err != nil {
		return err
	}
	podSpec.ImagePullSecrets = pullSecrets

	historyLimit := int32(3)
	desired := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fn.Name,
			Namespace: fn.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas:             &zero,
			RevisionHistoryLimit: &historyLimit,
			Selector:             &metav1.LabelSelector{MatchLabels: map[string]string{"app": fn.Name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					// An env change rewrites the env Secret without touching the
					// pod template, so a running function would keep serving the
					// old values. Projecting the CR's restart stamp is what lets
					// `kip function env set` reach a pod that is already up.
					Annotations: podAnnotations(fn.Annotations, bindingHash),
				},
				Spec: podSpec,
			},
		},
	}

	if err := controllerutil.SetControllerReference(fn, desired, r.Scheme); err != nil {
		return err
	}

	var existing appsv1.Deployment
	err = r.Get(ctx, types.NamespacedName{Name: fn.Name, Namespace: fn.Namespace}, &existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	// The same hold the App applies, and for the same promise: an env edit shows
	// a restart banner rather than restarting a running function. See the longer
	// note in the App's reconcileDeployment.
	running := generationOnContainer(existing.Spec.Template.Spec.Containers, secretname.KindFunction, fn.Name)
	if running != "" && running != generation {
		protected, err := hasLivePod(ctx, r.hostReader(), &existing)
		if err != nil {
			return err
		}
		usable, err := generationUsable(ctx, r.hostReader(), fn, secretname.KindFunction, running)
		if err != nil {
			return err
		}
		if protected && usable {
			held := desired.DeepCopy()
			held.Spec.Template.Spec.Containers[0].EnvFrom = envFrom(running)
			// Asked of the API server for the same reason as the App: the
			// template built here and the one that comes back from a Get differ
			// in everything admission fills in, so comparing them directly never
			// holds on a cluster.
			settles, answered, err := templateSettlesAs(ctx, r.Client, &existing, held.Spec.Template)
			if err != nil {
				return err
			}
			if !answered && r.Recorder != nil {
				r.Recorder.Event(fn, corev1.EventTypeWarning, "EnvHoldUnavailable",
					"this cluster does not allow the dry-run that decides whether an environment change can be applied without a restart, so the function restarts to pick it up")
			}
			if settles {
				// The live template is what the write would store, so keep it
				// rather than the freshly built one — see the App controller.
				desired.Spec.Template = existing.Spec.Template
			}
		}
	}

	// The adoption comes after the hold, as it does in the App. adoptChild
	// repairs a lost controller reference in memory and only a write makes that
	// real, so making it above would lose the repair on any path the hold
	// returns an error from — and the hold can, when the API server will not
	// answer whether the pod would roll.
	//
	// ownerOK is captured before the repair, because afterwards there is nothing
	// left to notice.
	ownerOK := metav1.IsControlledBy(&existing, fn)
	if err := adoptChild("Deployment", &existing, functionOwner(fn), r.Scheme); err != nil {
		return err
	}

	// Nothing to write is the held case: desired carries the live template
	// itself there, so this comparison is between one object and itself and does
	// not depend on defaulting to be right. Where they differ it writes, which
	// is what it did unconditionally before.
	//
	// ownerOK is part of it because a Deployment whose controller reference was
	// lost has an adoption waiting in memory, and returning here would throw it
	// away on every pass — leaving the Deployment behind when the Function goes.
	if ownerOK &&
		equality.Semantic.DeepEqual(existing.Spec.Template, desired.Spec.Template) &&
		equality.Semantic.DeepEqual(existing.Labels, desired.Labels) {
		return nil
	}

	existing.Spec.Template = desired.Spec.Template
	existing.Labels = desired.Labels
	return r.Update(ctx, &existing)
}

// reconcileCronJob materialises a Function's cron trigger as a Kubernetes
// CronJob. The Pod runs the same image as the HTTP Deployment but with
// KIPPER_MODE=batch, so the runtime image dispatches to the one-shot
// batch path instead of starting an HTTP server. No Service, no Ingress,
// no scale-to-zero infrastructure — a plain CronJob with the function's
// container.
func (r *FunctionReconciler) reconcileCronJob(ctx context.Context, fn *kipperv1.Function, trigger *kipperv1.FunctionTrigger, envSources []envSource, generation string) error {
	labels := functionLabels(fn)
	if fn.Spec.Source != nil && fn.Spec.Source.Code != "" {
		labels["kipper.run/runtime"] = fn.Spec.Runtime
	}

	// Stage the pull Secret here too: a cron-only Function never runs
	// reconcileDeployment, and BuildBatchPodSpec only references what this
	// staging call returns.
	pullSecrets, err := ensureImagePullSecret(ctx, r.Client, r.Scheme, fn, effectiveFunctionImage(fn))
	if err != nil {
		return err
	}

	podSpec := batchPodSpec(fn, "cron", pullSecrets, envSources, generation)

	successLimit := int32(3)
	failureLimit := int32(3)
	concurrency := batchv1.ForbidConcurrent

	desired := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fn.Name + "-cron",
			Namespace: fn.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.CronJobSpec{
			Schedule:                   trigger.Schedule,
			ConcurrencyPolicy:          concurrency,
			SuccessfulJobsHistoryLimit: &successLimit,
			FailedJobsHistoryLimit:     &failureLimit,
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: labels},
						Spec:       podSpec,
					},
				},
			},
		},
	}

	if err := controllerutil.SetControllerReference(fn, desired, r.Scheme); err != nil {
		return err
	}

	var existing batchv1.CronJob
	err = r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, &existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	if err := adoptChild("CronJob", &existing, functionOwner(fn), r.Scheme); err != nil {
		return err
	}
	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	return r.Update(ctx, &existing)
}

// loadServiceConn looks up a Service CR by name and reads its
// credentials Secret. Returns the connection details the kipper-poll
// sidecar and KEDA need.
func (r *FunctionReconciler) loadServiceConn(ctx context.Context, fn *kipperv1.Function, namespace, sourceName string, rendered renderedBindings) (*serviceConn, error) {
	if sourceName == "" {
		return nil, fmt.Errorf("event trigger has no source service")
	}

	var svc kipperv1.Service
	if err := r.Get(ctx, types.NamespacedName{Name: sourceName, Namespace: namespace}, &svc); err != nil {
		return nil, fmt.Errorf("looking up service %q: %w", sourceName, err)
	}

	// The snapshot this pass took, not a second read of the same mutable
	// Secret. Reading it again is how the poll sidecar and the main container
	// ended up in one pod template carrying two credential generations: the
	// main container gets the published environment, and the sidecar got
	// whatever the Secret held a moment later.
	//
	// Which snapshot depends on the binding, not on the service: a binding that
	// pins a database is held under its own derived name and carries that
	// database, so asking for the shared name would find nothing and fall back
	// to a live read of the service default — a sidecar pointed at the wrong
	// database as well as a later password.
	var secret corev1.Secret
	held, err := boundCredentialSnapshot(fn, sourceName, svc.Spec.Type, rendered)
	if err != nil {
		return nil, err
	}
	if held != nil {
		secret.Data = held
	} else if err := r.Get(ctx, types.NamespacedName{Name: secretname.ServiceCredentials(sourceName), Namespace: namespace}, &secret); err != nil {
		return nil, fmt.Errorf("loading credentials for service %q: %w", sourceName, err)
	}

	if svc.Spec.Type == "minio" {
		// S3 credentials carry a single endpoint URL plus access/secret
		// key. Parse host and port back out so the poller can build the
		// same connection string shape as every other trigger type.
		host, port := hostPortFromEndpoint(string(secret.Data["ENDPOINT"]))
		return &serviceConn{
			Type:     svc.Spec.Type,
			Host:     host,
			Port:     port,
			Username: string(secret.Data["ACCESS_KEY"]),
			Password: string(secret.Data["SECRET_KEY"]),
		}, nil
	}

	port := int32(0)
	if p := string(secret.Data["PORT"]); p != "" {
		_, _ = fmt.Sscanf(p, "%d", &port)
	}

	return &serviceConn{
		Type:     svc.Spec.Type,
		Host:     string(secret.Data["HOST"]),
		Port:     port,
		Username: string(secret.Data["USERNAME"]),
		Password: string(secret.Data["PASSWORD"]),
		Database: string(secret.Data["NAME"]),
	}, nil
}

// hostPortFromEndpoint splits a service endpoint URL like
// "http://minio.ns.svc.cluster.local:9000" back into host and port.
func hostPortFromEndpoint(endpoint string) (string, int32) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", 0
	}
	var port int32
	if p := u.Port(); p != "" {
		_, _ = fmt.Sscanf(p, "%d", &port)
	}
	return u.Hostname(), port
}

// boundCredentialSnapshot finds what this pass held for the binding that names
// sourceName, or nil when the function does not bind that service at all.
//
// A binding that pins a database is held under its own derived name and carries
// that database; one that does not is held under the service's shared name. The
// binding decides, which is why this cannot be answered from the service name
// alone.
func boundCredentialSnapshot(fn *kipperv1.Function, sourceName, serviceType string, rendered renderedBindings) (map[string][]byte, error) {
	var found *kipperv1.ServiceBinding
	for i := range fn.Spec.ServiceBindings {
		b := &fn.Spec.ServiceBindings[i]
		if b.Name != sourceName {
			continue
		}
		if found != nil {
			// Two bindings to one service give two answers to "which database
			// does the poller watch", and picking one silently is how a poller
			// ends up on a table nobody expected.
			return nil, fmt.Errorf("service %q is bound more than once, so the event trigger's credentials are ambiguous: keep one binding for it", sourceName)
		}
		found = b
	}
	if found == nil || rendered == nil {
		return nil, nil
	}

	name := secretname.ServiceCredentials(sourceName)
	if bindingIsDerived(*found, serviceType, true) {
		name = secretname.Binding(found.Name, secretname.KindFunction, fn.Name)
	}
	if data, ok := rendered[name]; ok {
		return data, nil
	}
	return nil, nil
}

// buildPollSidecar produces the kipper-poll container that bridges
// event sources into HTTP invocations of the function. The trigger's
// Config map carries trigger-specific knobs (query, list, bucket, etc.)
// from the CR and gets translated into env vars the poller understands.
func (r *FunctionReconciler) buildPollSidecar(ctx context.Context, fn *kipperv1.Function, trigger *kipperv1.FunctionTrigger, fnPort int32, rendered renderedBindings) (corev1.Container, error) {
	source := trigger.Config["source"]
	conn, err := r.loadServiceConn(ctx, fn, fn.Namespace, source, rendered)
	if err != nil {
		return corev1.Container{}, err
	}

	sourceURL := conn.URL()
	if trigger.Type == "postgres" && !strings.Contains(sourceURL, "sslmode") {
		// Kipper services run with sslmode=disable in-cluster; the poller
		// needs the same so the connection succeeds.
		if strings.Contains(sourceURL, "?") {
			sourceURL += "&sslmode=disable"
		} else {
			sourceURL += "?sslmode=disable"
		}
	}

	env := []corev1.EnvVar{
		{Name: "KIPPER_TRIGGER", Value: trigger.Type},
		{Name: "KIPPER_TARGET_URL", Value: fmt.Sprintf("http://localhost:%d/event", fnPort)},
		{Name: "KIPPER_SOURCE_URL", Value: sourceURL},
	}

	switch trigger.Type {
	case "postgres", "mysql":
		if q := trigger.Config["query"]; q != "" {
			env = append(env, corev1.EnvVar{Name: "KIPPER_QUERY", Value: q})
		}
		if md := trigger.Config["markDone"]; md != "" {
			env = append(env, corev1.EnvVar{Name: "KIPPER_MARK_DONE", Value: md})
		}
	case "redis":
		if list := trigger.Config["list"]; list != "" {
			env = append(env, corev1.EnvVar{Name: "KIPPER_REDIS_LIST", Value: list})
		}
	case "minio":
		if bucket := trigger.Config["bucket"]; bucket != "" {
			env = append(env, corev1.EnvVar{Name: "KIPPER_MINIO_BUCKET", Value: bucket})
		}
	}

	return corev1.Container{
		Name:  "kipper-poll",
		Image: kipperPollImage,
		Env:   env,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("16Mi"),
				corev1.ResourceCPU:    resource.MustParse("10m"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
		},
	}, nil
}

// reconcileEventScaledObject creates the KEDA ScaledObject that scales
// the function's Deployment from 0 based on activity at the event
// source.
func (r *FunctionReconciler) reconcileEventScaledObject(ctx context.Context, fn *kipperv1.Function, trigger *kipperv1.FunctionTrigger) error {
	// KEDA's scaler reads the service's Secret itself and is not a pod Kipper
	// templates, so it sits outside the one-generation guarantee and reads the
	// live credentials here rather than this pass's snapshot.
	conn, err := r.loadServiceConn(ctx, fn, fn.Namespace, trigger.Config["source"], nil)
	if err != nil {
		// Without bound credentials we cannot wire up KEDA. Surface the
		// error but do not block — the Deployment still exists; it just
		// will not auto-scale on events.
		log.FromContext(ctx).Info("event ScaledObject skipped", "function", fn.Name, "reason", err.Error())
		return nil
	}

	triggers := buildKEDATriggers(fn, trigger, conn)
	if len(triggers) == 0 {
		return nil
	}

	spec := map[string]interface{}{
		"scaleTargetRef": map[string]interface{}{
			"name": fn.Name,
		},
		"minReplicaCount": int64(0),
		"maxReplicaCount": int64(10),
		"cooldownPeriod":  int64(300),
		"pollingInterval": int64(10),
		"triggers":        triggers,
	}

	desired := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "keda.sh/v1alpha1",
			"kind":       "ScaledObject",
			"metadata": map[string]interface{}{
				"name":      fn.Name,
				"namespace": fn.Namespace,
				"labels":    toInterfaceMap(functionLabels(fn)),
			},
			"spec": spec,
		},
	}

	var existing unstructured.Unstructured
	existing.SetGroupVersionKind(schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject"})
	getErr := r.Get(ctx, types.NamespacedName{Name: fn.Name, Namespace: fn.Namespace}, &existing)
	if errors.IsNotFound(getErr) {
		if err := controllerutil.SetControllerReference(fn, desired, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, desired)
	}
	if getErr != nil {
		return getErr
	}
	// Written wholesale rather than field by field, so the object that lands is
	// desired and not existing. Provenance is therefore asked of existing —
	// what is about to be overwritten — before anything is written over it.
	if ok, why := childProvenance(&existing, functionOwner(fn)); !ok {
		return fmt.Errorf("%s %q in %s %s; rename the function or remove that object",
			existing.GetKind(), existing.GetName(), existing.GetNamespace(), why)
	}
	desired.SetResourceVersion(existing.GetResourceVersion())
	if err := controllerutil.SetControllerReference(fn, desired, r.Scheme); err != nil {
		return err
	}
	return r.Update(ctx, desired)
}

// deleteOwnedKEDAObject removes a KEDA object this function controls. One under
// the same name that it does not control belongs to somebody else: a trigger
// being switched off must not destroy what switching it on would have refused
// to overwrite.
func (r *FunctionReconciler) deleteOwnedKEDAObject(ctx context.Context, fn *kipperv1.Function, kind, name string) error {
	var existing unstructured.Unstructured
	existing.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "keda.sh", Version: "v1alpha1", Kind: kind,
	})
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: fn.Namespace}, &existing); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !ownedByWorkload(&existing, functionOwner(fn)) {
		return nil
	}
	if err := r.Delete(ctx, &existing); err != nil && !errors.IsNotFound(err) {
		return err
	}
	return nil
}

// buildKEDATriggers translates a Kipper FunctionTrigger into the
// KEDA-shaped triggers array.
func buildKEDATriggers(fn *kipperv1.Function, trigger *kipperv1.FunctionTrigger, conn *serviceConn) []interface{} {
	switch trigger.Type {
	case "postgres":
		query := trigger.Config["query"]
		if query == "" {
			return nil
		}
		countQuery := fmt.Sprintf("SELECT COUNT(*) FROM (%s) AS kipper_count", query)
		return []interface{}{map[string]interface{}{
			"type": "postgresql",
			"metadata": map[string]interface{}{
				"host":             conn.Host,
				"port":             fmt.Sprintf("%d", conn.Port),
				"userName":         conn.Username,
				"dbName":           conn.Database,
				"sslmode":          "disable",
				"query":            countQuery,
				"targetQueryValue": "1",
			},
			"authenticationRef": map[string]interface{}{
				"name": fn.Name + "-trigger-auth",
			},
		}}
	case "mysql":
		query := trigger.Config["query"]
		if query == "" {
			return nil
		}
		countQuery := fmt.Sprintf("SELECT COUNT(*) FROM (%s) AS kipper_count", query)
		return []interface{}{map[string]interface{}{
			"type": "mysql",
			"metadata": map[string]interface{}{
				"query":      countQuery,
				"queryValue": "1",
			},
			"authenticationRef": map[string]interface{}{
				"name": fn.Name + "-trigger-auth",
			},
		}}
	case "redis":
		list := trigger.Config["list"]
		if list == "" {
			return nil
		}
		return []interface{}{map[string]interface{}{
			"type": "redis",
			"metadata": map[string]interface{}{
				"address":    fmt.Sprintf("%s:%d", conn.Host, conn.Port),
				"listName":   list,
				"listLength": "1",
			},
		}}
	case "minio":
		// MinIO uses webhook-based events; the kipper-poll sidecar
		// receives them. KEDA polls on a cron schedule so the function
		// gets a periodic poke when bucket activity is expected.
		return []interface{}{map[string]interface{}{
			"type": "cron",
			"metadata": map[string]interface{}{
				"timezone":        "UTC",
				"start":           "0 * * * *",
				"end":             "59 * * * *",
				"desiredReplicas": "1",
			},
		}}
	}
	return nil
}

// reconcileTriggerAuth creates the KEDA TriggerAuthentication that
// references the bound Service's existing credentials Secret. No
// duplicate Secret is created — KEDA reads the right key directly from
// the service-managed Secret (postgres uses PASSWORD, mysql uses the
// connection string).
func (r *FunctionReconciler) reconcileTriggerAuth(ctx context.Context, fn *kipperv1.Function, trigger *kipperv1.FunctionTrigger) error {
	source := trigger.Config["source"]
	if source == "" {
		return nil
	}

	var secretRefs []interface{}
	switch trigger.Type {
	case "postgres":
		secretRefs = []interface{}{map[string]interface{}{
			"parameter": "password",
			"name":      secretname.ServiceCredentials(source),
			"key":       "PASSWORD",
		}}
	case "mysql":
		// MySQL's KEDA scaler wants a single connectionString; we synth
		// it into the credentials Secret if not present, falling back to
		// the password parameter.
		secretRefs = []interface{}{map[string]interface{}{
			"parameter": "password",
			"name":      secretname.ServiceCredentials(source),
			"key":       "PASSWORD",
		}}
	default:
		return nil
	}

	desired := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "keda.sh/v1alpha1",
			"kind":       "TriggerAuthentication",
			"metadata": map[string]interface{}{
				"name":      fn.Name + "-trigger-auth",
				"namespace": fn.Namespace,
				// Labelled like every other child, so a controller reference
				// lost to a direct write or a restore can be re-asserted. An
				// unlabelled object with no owner is unrecognisable to both the
				// adopt and the delete path, and stays unowned for ever.
				"labels": toInterfaceMap(functionLabels(fn)),
			},
			"spec": map[string]interface{}{
				"secretTargetRef": secretRefs,
			},
		},
	}

	var existing unstructured.Unstructured
	existing.SetGroupVersionKind(schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "TriggerAuthentication"})
	getErr := r.Get(ctx, types.NamespacedName{Name: fn.Name + "-trigger-auth", Namespace: fn.Namespace}, &existing)
	if errors.IsNotFound(getErr) {
		if err := controllerutil.SetControllerReference(fn, desired, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, desired)
	}
	if getErr != nil {
		return getErr
	}
	// Written wholesale rather than field by field, so the object that lands is
	// desired and not existing. Provenance is therefore asked of existing —
	// what is about to be overwritten — before anything is written over it.
	if ok, why := childProvenance(&existing, functionOwner(fn)); !ok {
		return fmt.Errorf("%s %q in %s %s; rename the function or remove that object",
			existing.GetKind(), existing.GetName(), existing.GetNamespace(), why)
	}
	desired.SetResourceVersion(existing.GetResourceVersion())
	if err := controllerutil.SetControllerReference(fn, desired, r.Scheme); err != nil {
		return err
	}
	return r.Update(ctx, desired)
}

// reconcileEnvSecret materialises Function.Spec.Env into a Secret named
// function-<fn>-env, resolving each ${NAME} against the environment the
// function's pods will actually see. The HTTP Deployment, the cron CronJob and
// the test-run Job all reference this Secret in their container's EnvFrom (with
// Optional=true), so without this reconciler the plain env vars set on the CR
// were silently dropped on the way to the pod.
//
// One Secret serves all three, which is why the table it resolves against is
// the one both pod shapes agree on: a value resolved from KIPPER_MODE would be
// written once and then read by the shape it does not describe.
func (r *FunctionReconciler) reconcileEnvSecret(ctx context.Context, fn *kipperv1.Function, envSources []envSource) (string, error) {
	shared := withFunctionRuntime(fn, envSources, functionShared, "")
	table, err := effectiveEnv(ctx, r.Client, fn.Namespace, shared)
	if err != nil {
		return "", err
	}
	resolved, diag := renderEnv(fn.Spec.Env, table)
	// Recorded in memory; updateStatus persists it at the end of the pass.
	applyEnvResolvedCondition(&fn.Status.Conditions, fn.Generation, len(fn.Spec.Env), diag)

	labels := functionLabels(fn)

	// One generation for every pod shape. What differs between the serving pod,
	// a batch run and a test run is the mode and trigger, and those are set on
	// the container itself rather than read from a Secret, so the published
	// environment is the same object for all of them.
	published, err := publishedEnv(ctx, r.Client, fn.Namespace, shared, resolved)
	if err != nil {
		return "", err
	}
	generation, err := publishEnvGeneration(ctx, r.Client, r.Scheme, fn, secretname.KindFunction,
		published, labels)
	if err != nil {
		return "", err
	}

	if err := updateLegacyEnvSecret(ctx, r.Client, r.Scheme, fn,
		secretname.Env(secretname.KindFunction, fn.Name), resolved, labels); err != nil {
		return "", err
	}
	return generation, nil
}

func (r *FunctionReconciler) reconcileCodeConfigMap(ctx context.Context, fn *kipperv1.Function) error {
	filename, _ := runtimeHandler(fn.Spec.Runtime)

	data := map[string]string{
		filename: fn.Spec.Source.Code,
	}

	// If the user declared third-party dependencies, the controller
	// emits a runtime-specific manifest into the ConfigMap. The runtime
	// image's entrypoint installs them at container start.
	if manifestName, manifest := dependencyManifest(fn); manifestName != "" {
		data[manifestName] = manifest
	}

	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fn.Name + "-code",
			Namespace: fn.Namespace,
			Labels: map[string]string{
				"app":       fn.Name,
				kipperLabel: kipperValue,
			},
		},
		Data: data,
	}

	if err := controllerutil.SetControllerReference(fn, desired, r.Scheme); err != nil {
		return err
	}

	var existing corev1.ConfigMap
	err := r.Get(ctx, types.NamespacedName{Name: fn.Name + "-code", Namespace: fn.Namespace}, &existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	if err := adoptChild("ConfigMap", &existing, functionOwner(fn), r.Scheme); err != nil {
		return err
	}
	existing.Data = desired.Data
	return r.Update(ctx, &existing)
}

// dependencyManifest returns the filename and content of the
// runtime-specific dependency manifest derived from the function's
// declared dependencies. Returns ("", "") when no deps are declared.
func dependencyManifest(fn *kipperv1.Function) (string, string) {
	if fn.Spec.Source == nil || len(fn.Spec.Source.Dependencies) == 0 {
		return "", ""
	}
	switch fn.Spec.Runtime {
	case "node":
		// Build a deterministic package.json. Sorting the dependency
		// keys keeps the file stable across reconciles, which avoids
		// pointless ConfigMap churn.
		keys := make([]string, 0, len(fn.Spec.Source.Dependencies))
		for k := range fn.Spec.Source.Dependencies {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var buf strings.Builder
		buf.WriteString("{\n  \"name\": \"" + fn.Name + "\",\n  \"private\": true,\n  \"dependencies\": {\n")
		for i, k := range keys {
			fmt.Fprintf(&buf, "    %q: %q", k, fn.Spec.Source.Dependencies[k])
			if i < len(keys)-1 {
				buf.WriteString(",")
			}
			buf.WriteString("\n")
		}
		buf.WriteString("  }\n}\n")
		return "package.json", buf.String()
	case "python":
		keys := make([]string, 0, len(fn.Spec.Source.Dependencies))
		for k := range fn.Spec.Source.Dependencies {
			// Skip standard library modules. Pip can't install them
			// (some have an empty PyPI placeholder, others 404) and
			// the user's `import decimal` works without any install.
			// Filtering here covers manual entries, AI-assist
			// suggestions, and accidental copy-paste.
			if pythonStdlib[strings.ToLower(rootPackage(k))] {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var buf strings.Builder
		for _, k := range keys {
			v := fn.Spec.Source.Dependencies[k]
			// Empty / "latest" / "*" all mean "any version" — emit the
			// bare name. The form defaults to "*" when the user leaves
			// the version blank, and pip rejects "name==*" so we have
			// to translate it here, not pass it through.
			switch {
			case v == "" || v == "latest" || v == "*":
				buf.WriteString(k)
			case strings.ContainsAny(v, "=<>~!"):
				// Full PEP 440 specifier (e.g. ">=2.0,<3") passed verbatim.
				buf.WriteString(k + v)
			default:
				// Bare version like "1.2.3" or "2.9.*" — pin with ==.
				buf.WriteString(k + "==" + v)
			}
			buf.WriteString("\n")
		}
		return "requirements.txt", buf.String()
	}
	return "", ""
}

func (r *FunctionReconciler) reconcileService(ctx context.Context, fn *kipperv1.Function) error {
	labels := functionLabels(fn)
	port := fn.Spec.Port
	if port == 0 {
		port = 8080
	}

	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fn.Name,
			Namespace: fn.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": fn.Name},
			Ports:    []corev1.ServicePort{{Port: port, TargetPort: intstr.FromInt32(port)}},
		},
	}

	if err := controllerutil.SetControllerReference(fn, desired, r.Scheme); err != nil {
		return err
	}

	var existing corev1.Service
	err := r.Get(ctx, types.NamespacedName{Name: fn.Name, Namespace: fn.Namespace}, &existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	// The spec is deliberately left alone — a Service's clusterIP and
	// nodePorts are assigned by the API server and rewriting them fights it.
	// Ownership still has to be asserted, or a Service that already existed is
	// reconciled for ever and garbage-collected never.
	if err := adoptChild("Service", &existing, functionOwner(fn), r.Scheme); err != nil {
		return err
	}
	return r.Update(ctx, &existing)
}

func (r *FunctionReconciler) reconcileHTTPScaledObject(ctx context.Context, fn *kipperv1.Function) error {
	// Check if HTTP trigger is present
	hasHTTP := false
	for _, t := range fn.Spec.Triggers {
		if t.Type == "http" {
			hasHTTP = true
			break
		}
	}
	if !hasHTTP && len(fn.Spec.Triggers) == 0 {
		hasHTTP = true // default to HTTP trigger
	}
	if !hasHTTP {
		return nil
	}

	port := fn.Spec.Port
	if port == 0 {
		port = 8080
	}

	host := domain.SubdomainFor("fn-"+fn.Name, r.Domain)
	labels := functionLabels(fn)
	labels["kipper.run/fn-namespace"] = fn.Namespace

	hso := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "http.keda.sh/v1alpha1",
			"kind":       "HTTPScaledObject",
			"metadata": map[string]interface{}{
				"name":      fn.Name,
				"namespace": fn.Namespace,
				"labels":    toInterfaceMap(labels),
			},
			"spec": map[string]interface{}{
				"hosts": []interface{}{host},
				"scaleTargetRef": map[string]interface{}{
					"name":    fn.Name,
					"service": fn.Name,
					"port":    int64(port),
				},
				"replicas": map[string]interface{}{
					"min": int64(0),
					"max": int64(10),
				},
				"scalingMetric": map[string]interface{}{
					"requestRate": map[string]interface{}{
						"targetValue": int64(5),
						"window":      "1m",
					},
				},
				"scaledownPeriod": int64(300),
			},
		},
	}

	var existing unstructured.Unstructured
	existing.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "http.keda.sh",
		Version: "v1alpha1",
		Kind:    "HTTPScaledObject",
	})

	if err := controllerutil.SetControllerReference(fn, hso, r.Scheme); err != nil {
		return err
	}

	err := r.Get(ctx, types.NamespacedName{Name: fn.Name, Namespace: fn.Namespace}, &existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, hso)
	}
	if err != nil {
		return err
	}

	if err := adoptChild("HTTPScaledObject", &existing, functionOwner(fn), r.Scheme); err != nil {
		return err
	}
	existing.Object["spec"] = hso.Object["spec"]
	return r.Update(ctx, &existing)
}

// setFunctionRouteConflict marks the function's route unavailable so a refused
// host surfaces on status instead of silently dropping the Ingress.
func (r *FunctionReconciler) setFunctionRouteConflict(fn *kipperv1.Function, host, reason string) {
	apimeta.SetStatusCondition(&fn.Status.Conditions, metav1.Condition{
		Type:    kipperv1.ConditionRouteReady,
		Status:  metav1.ConditionFalse,
		Reason:  "HostUnavailable",
		Message: fmt.Sprintf("route host %q is %s", host, reason),
	})
}

// deleteFunctionIngress removes the function's Ingress (in the shared keda
// namespace) when its host is refused. All function ingresses share the name
// fn-<name>, so it only deletes the object when its source-namespace label
// matches — otherwise a refused function would delete a same-named function's
// live Ingress in another project.
func (r *FunctionReconciler) deleteFunctionIngress(ctx context.Context, name, namespace string) error {
	var existing networkingv1.Ingress
	err := r.Get(ctx, types.NamespacedName{Name: "fn-" + name, Namespace: "keda"}, &existing)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if existing.Labels[fnNamespaceLabel] != namespace {
		// Belongs to a same-named function in another namespace — leave it.
		return nil
	}
	return r.Delete(ctx, &existing)
}

// sharedNamespaceObjectBelongsTo answers the ownership question for the objects
// a Function keeps in the shared keda namespace.
//
// A controller reference cannot be used for them: Kubernetes forbids a
// namespaced object owning one in another namespace, so nothing here dies with
// its Function and the label is the only marker there is. Two functions of the
// same name in different projects derive the same fn-<name> object, which is
// precisely the collision this has to survive.
func sharedNamespaceObjectBelongsTo(obj client.Object, fn *kipperv1.Function) bool {
	return obj.GetLabels()[fnNamespaceLabel] == fn.Namespace
}

// deleteOwnedFunctionMiddleware removes a function's security middleware from
// the shared keda namespace, and leaves a same-named one belonging to another
// project alone.
func (r *FunctionReconciler) deleteOwnedFunctionMiddleware(ctx context.Context, fn *kipperv1.Function, name string) error {
	var existing unstructured.Unstructured
	existing.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "traefik.io", Version: "v1alpha1", Kind: "Middleware",
	})
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: "keda"}, &existing); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !sharedNamespaceObjectBelongsTo(&existing, fn) {
		return nil
	}
	if err := r.Delete(ctx, &existing); err != nil && !errors.IsNotFound(err) {
		return err
	}
	return nil
}

// deleteOwnedHTTPScaledObject removes the HTTPScaledObject this function owns.
// Deleting it by name would destroy an object reconcileHTTPScaledObject refuses
// to adopt, which is the asymmetry the App controller had.
func (r *FunctionReconciler) deleteOwnedHTTPScaledObject(ctx context.Context, fn *kipperv1.Function) error {
	var existing unstructured.Unstructured
	existing.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "http.keda.sh", Version: "v1alpha1", Kind: "HTTPScaledObject",
	})
	if err := r.Get(ctx, types.NamespacedName{Name: fn.Name, Namespace: fn.Namespace}, &existing); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !ownedByWorkload(&existing, functionOwner(fn)) {
		return nil
	}
	if err := r.Delete(ctx, &existing); err != nil && !errors.IsNotFound(err) {
		return err
	}
	return nil
}

// cleanupSharedNamespaceObjects removes what a deleted Function leaves in the
// keda namespace. Garbage collection cannot reach across namespaces, so these
// two outlive their Function unless they are deleted here.
func (r *FunctionReconciler) cleanupSharedNamespaceObjects(ctx context.Context, fn *kipperv1.Function) error {
	if err := r.deleteFunctionIngress(ctx, fn.Name, fn.Namespace); err != nil {
		return fmt.Errorf("deleting function ingress: %w", err)
	}
	if err := r.deleteOwnedFunctionMiddleware(ctx, fn, "fn-"+fn.Name+"-security"); err != nil {
		return fmt.Errorf("deleting function security middleware: %w", err)
	}
	return nil
}

// cleanupHTTPServing tears down the HTTP serving path for a function that no
// longer has an HTTP trigger and releases its route claim. The Ingress and the
// HTTPScaledObject are HTTP-only; the Deployment and Service are shared with the
// event path, so they are left to the normal reconcile.
func (r *FunctionReconciler) cleanupHTTPServing(ctx context.Context, fn *kipperv1.Function) error {
	if err := r.deleteFunctionIngress(ctx, fn.Name, fn.Namespace); err != nil {
		return fmt.Errorf("deleting function ingress: %w", err)
	}
	if err := r.deleteOwnedHTTPScaledObject(ctx, fn); err != nil {
		return fmt.Errorf("deleting http scaled object: %w", err)
	}
	// The middleware exists only to be referenced by the route, so it goes with
	// it rather than lingering in the shared namespace under a name a later
	// function would collide with.
	if err := r.deleteOwnedFunctionMiddleware(ctx, fn, "fn-"+fn.Name+"-security"); err != nil {
		return fmt.Errorf("deleting function security middleware: %w", err)
	}
	// The host claim is sticky and stays held by the project; a cron-only
	// function keeps its fn-<name> host reserved so a later HTTP edit reclaims it.
	return nil
}

func (r *FunctionReconciler) reconcileIngress(ctx context.Context, fn *kipperv1.Function) error {
	host := domain.SubdomainFor("fn-"+fn.Name, r.Domain)

	// Two functions of the same name in different projects derive the same
	// fn-<name> host, so claim it cluster-wide before creating the Ingress. A
	// reserved platform host, or one another project owns, is refused.
	reserved, err := hostReservedForPlatform(ctx, r.hostReader(), host, r.Domain)
	if err != nil {
		return err
	}
	if reserved {
		r.setFunctionRouteConflict(fn, host, "reserved for a platform service")
		return r.deleteFunctionIngress(ctx, fn.Name, fn.Namespace)
	}
	owned, err := reserveHost(ctx, r.hostReader(), r.Client, fn.Namespace, host)
	if err != nil {
		return err
	}
	if !owned {
		r.setFunctionRouteConflict(fn, host, "already claimed by another project")
		return r.deleteFunctionIngress(ctx, fn.Name, fn.Namespace)
	}
	apimeta.RemoveStatusCondition(&fn.Status.Conditions, kipperv1.ConditionRouteReady)

	// Behind the host claim, not before it. The middleware shares the keda
	// namespace under the same conventional name, so reconciling it earlier let
	// a project that is about to be refused the route create the object first
	// and leave the winner unable to replace what it now serves through.
	if err := r.reconcileFunctionSecurityMiddleware(ctx, fn); err != nil {
		return fmt.Errorf("reconciling function security middleware: %w", err)
	}

	labels := functionLabels(fn)
	labels["kipper.run/fn-namespace"] = fn.Namespace

	// Functions live under the cluster's r.Domain. When that's a
	// kipper.run subdomain, the gateway terminates TLS for the
	// `*.kipper.run` wildcard and proxies HTTPS to the cluster IP with
	// the original hostname as SNI. Drop the cert-manager annotation
	// (its HTTP-01 challenge would 404 at the gateway and the
	// Certificate would sit pending forever) but keep the spec.tls
	// hosts entry so Traefik still has a TLS router for the SNI.
	// Without secretName Traefik falls back to its default cert, which
	// is fine because the gateway hop uses InsecureSkipVerify.
	// See the matching note in app_controller.go reconcileIngress.
	gatewayTLS := strings.HasSuffix(host, ".kipper.run")

	annotations := map[string]string{}
	if !gatewayTLS {
		annotations["cert-manager.io/cluster-issuer"] = "letsencrypt-prod"
	}
	if !fn.Spec.NoSecurityHeaders {
		annotations["traefik.ingress.kubernetes.io/router.middlewares"] = "keda-fn-" + fn.Name + "-security@kubernetescrd,traefik-rate-limit@kubernetescrd"
	} else {
		annotations["traefik.ingress.kubernetes.io/router.middlewares"] = "traefik-rate-limit@kubernetescrd"
	}

	tlsEntry := networkingv1.IngressTLS{Hosts: []string{host}}
	if !gatewayTLS {
		tlsEntry.SecretName = "fn-" + fn.Name + "-tls"
	}

	pathType := networkingv1.PathTypePrefix
	desired := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "fn-" + fn.Name,
			Namespace:   "keda",
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: strPtr("traefik"),
			TLS:              []networkingv1.IngressTLS{tlsEntry},
			Rules: []networkingv1.IngressRule{
				{
					Host: host,
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{
								{
									Path:     "/",
									PathType: &pathType,
									Backend: networkingv1.IngressBackend{
										Service: &networkingv1.IngressServiceBackend{
											Name: "keda-add-ons-http-interceptor-proxy",
											Port: networkingv1.ServiceBackendPort{Number: 8080},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	// Cannot set owner reference across namespaces, so ingress in keda
	// namespace is managed by label-based lookup during cleanup

	var existing networkingv1.Ingress
	err = r.Get(ctx, types.NamespacedName{Name: "fn-" + fn.Name, Namespace: "keda"}, &existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	if !sharedNamespaceObjectBelongsTo(&existing, fn) {
		// reserveHost above should already have refused this function, so
		// reaching here means the reservation and the Ingress disagree. Writing
		// over another project's route on the strength of a shared name is not
		// the way to resolve that.
		return nil
	}

	existing.Spec = desired.Spec
	existing.Annotations = desired.Annotations
	return r.Update(ctx, &existing)
}

func (r *FunctionReconciler) buildFunctionCSP(allowlist []string) string {
	return buildWorkloadCSP(allowlist)
}

func (r *FunctionReconciler) reconcileFunctionSecurityMiddleware(ctx context.Context, fn *kipperv1.Function) error {
	mwName := "fn-" + fn.Name + "-security"

	if fn.Spec.NoSecurityHeaders {
		return r.deleteOwnedFunctionMiddleware(ctx, fn, mwName)
	}

	csp := r.buildFunctionCSP(fn.Spec.CSPAllowlist)
	labels := functionLabels(fn)
	labels["kipper.run/fn-namespace"] = fn.Namespace

	desired := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "traefik.io/v1alpha1",
			"kind":       "Middleware",
			"metadata": map[string]interface{}{
				"name":      mwName,
				"namespace": "keda",
				"labels":    toInterfaceMap(labels),
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

	var existing unstructured.Unstructured
	existing.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "traefik.io", Version: "v1alpha1", Kind: "Middleware",
	})

	err := r.Get(ctx, types.NamespacedName{Name: mwName, Namespace: "keda"}, &existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	// This runs before the host is claimed, so a same-named function in another
	// project reaches here even when it will be refused the route a moment
	// later. The label is the only ownership marker a shared namespace allows.
	if !sharedNamespaceObjectBelongsTo(&existing, fn) {
		return nil
	}

	existing.Object["spec"] = desired.Object["spec"]
	return r.Update(ctx, &existing)
}

func (r *FunctionReconciler) updateStatus(ctx context.Context, fn *kipperv1.Function) error {
	var deploy appsv1.Deployment
	err := r.Get(ctx, types.NamespacedName{Name: fn.Name, Namespace: fn.Namespace}, &deploy)
	if errors.IsNotFound(err) {
		fn.Status.Phase = "Idle"
		fn.Status.Replicas = 0
		return r.Status().Update(ctx, fn)
	}
	if err != nil {
		return err
	}

	fn.Status.Replicas = deploy.Status.Replicas
	fn.Status.Endpoint = "https://" + domain.SubdomainFor("fn-"+fn.Name, r.Domain)

	switch {
	case deploy.Status.AvailableReplicas > 0:
		fn.Status.Phase = "Running"
	case deploy.Status.Replicas > 0:
		fn.Status.Phase = "Scaling"
	default:
		fn.Status.Phase = "Idle"
	}

	return r.Status().Update(ctx, fn)
}

func (r *FunctionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kipperv1.Function{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		// The Secrets this controller derives, so a binding refused because its
		// credentials had not arrived yet is retried the moment they do rather
		// than waiting for the resync. The App controller has had this since it
		// began deriving them.
		Owns(&corev1.Secret{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.enqueueForRegistryCredentials)).
		// Same reason as the App controller: a rotated service password must reach
		// the bindings derived from it.
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.enqueueFunctionsForServiceCredentials)).
		Watches(&kipperv1.ClusterIdentity{}, handler.EnqueueRequestsFromMapFunc(r.enqueueFunctionsForClusterIdentity)).
		Complete(r)
}

func functionLabels(fn *kipperv1.Function) map[string]string {
	trigger := "http"
	if len(fn.Spec.Triggers) > 0 {
		trigger = fn.Spec.Triggers[0].Type
	}
	return map[string]string{
		"app":                      fn.Name,
		kipperLabel:                kipperValue,
		"kipper.run/resource-type": "function",
		"kipper.run/trigger":       trigger,
	}
}

func toInterfaceMap(m map[string]string) map[string]interface{} {
	result := make(map[string]interface{}, len(m))
	for k, v := range m {
		result[k] = v
	}
	return result
}

// BuildBatchPodSpec produces the pod spec used to run a Function in
// batch mode. It's shared between the cron reconciler (which wraps it
// in a CronJob) and the on-demand test handler in the handlers
// package (which wraps it in a one-off Job). The trigger string ends
// up as KIPPER_TRIGGER so the runtime image can dispatch on it —
// "cron" for scheduled runs, "test" for manual test invocations.
//
// Exposed as a package-level function so callers outside the
// reconciler (notably the handlers package) don't need to instantiate
// a FunctionReconciler just to build the spec.
//
// pullSecrets comes from the caller's staging call (ensureImagePullSecret /
// StageFunctionPullSecret), so the spec references exactly what that single
// credential read staged — the spec never re-reads the credential list and can
// never reach a different authorization decision than the staging did.
func BuildBatchPodSpec(ctx context.Context, c client.Client, fn *kipperv1.Function, trigger string, pullSecrets []corev1.LocalObjectReference) (corev1.PodSpec, error) {
	// No reconcile behind this call, so nothing has been resolved for it. The
	// bindings are derived here through the same function the reconciler uses,
	// which does two things a bare read would not: it writes the per-binding
	// projection a pinned binding needs, so a test run works before the first
	// controller pass rather than being refused, and it returns the snapshot it
	// took. Passing nil instead left effectiveEnv and publishedEnv each reading
	// the mutable shared Secret, which is the skew this whole change exists to
	// remove, reproduced inside the object that is supposed to be atomic.
	rendered, _, _, err := reconcileBindingSecrets(ctx, c, c.Scheme(), fn,
		secretname.KindFunction, fn.Spec.ServiceBindings)
	if err != nil {
		return corev1.PodSpec{}, fmt.Errorf("deriving service bindings: %w", err)
	}
	sources, refused, err := functionEnvSources(ctx, c, fn, rendered)
	if err != nil {
		return corev1.PodSpec{}, fmt.Errorf("resolving service bindings: %w", err)
	}
	if len(refused) > 0 {
		return corev1.PodSpec{}, fmt.Errorf("service bindings refused: %s", strings.Join(refused, "; "))
	}

	// Published here rather than left to the reconciler, because a test run is
	// callable the moment a Function is created and the pod's only env source is
	// not optional: pointing a handler-created Job at a generation no controller
	// pass has written yet gives a Job that can never start.
	shared := withFunctionRuntime(fn, sources, functionShared, "")
	table, err := effectiveEnv(ctx, c, fn.Namespace, shared)
	if err != nil {
		return corev1.PodSpec{}, err
	}
	resolved, _ := renderEnv(fn.Spec.Env, table)
	published, err := publishedEnv(ctx, c, fn.Namespace, shared, resolved)
	if err != nil {
		return corev1.PodSpec{}, err
	}
	generation, err := publishEnvGeneration(ctx, c, c.Scheme(), fn, secretname.KindFunction,
		published, functionLabels(fn))
	if err != nil {
		return corev1.PodSpec{}, fmt.Errorf("publishing environment: %w", err)
	}
	return batchPodSpec(fn, trigger, pullSecrets, sources, generation), nil
}

// batchPodSpec builds the batch shape from sources already resolved. It reads
// nothing, so it cannot reach a different answer than the pass that resolved
// them.
func batchPodSpec(fn *kipperv1.Function, trigger string, pullSecrets []corev1.LocalObjectReference, envSources []envSource, generation string) corev1.PodSpec {
	image := effectiveFunctionImage(fn)
	isInline := fn.Spec.Source != nil && fn.Spec.Source.Code != ""

	container := corev1.Container{
		Name:  fn.Name,
		Image: image,
	}

	if isInline {
		container.VolumeMounts = []corev1.VolumeMount{
			{Name: "function-code", MountPath: "/app/function"},
		}
	}

	container.Resources = functionResources(fn.Spec.Resources)

	// The same sources the serving pod reads, plus the mode and trigger a batch
	// run dispatches on.
	sources := withFunctionRuntime(fn, envSources, functionBatch, trigger)
	container.EnvFrom = envFrom(generation)
	container.Env = directEnv(sources)

	// Same shared-volume mounts as the HTTP Deployment so batch runs see
	// the same filesystem layout the HTTP function does.
	for _, vm := range fn.Spec.Volumes {
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      vm.Name,
			MountPath: vm.MountPath,
		})
	}

	podSpec := corev1.PodSpec{
		Containers:    []corev1.Container{container},
		RestartPolicy: corev1.RestartPolicyOnFailure,
	}
	if isInline {
		podSpec.Volumes = []corev1.Volume{{
			Name: "function-code",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: fn.Name + "-code"},
				},
			},
		}}
	}
	for _, vm := range fn.Spec.Volumes {
		podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
			Name: vm.Name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: "shared-" + vm.Name,
				},
			},
		})
	}
	podSpec.ImagePullSecrets = pullSecrets

	return podSpec
}

// rootPackage returns the top-level package name for a Python import
// path. Imports like "xml.etree.ElementTree" map to "xml", which is
// what we check against the stdlib set. For names without dots it
// returns the input unchanged.
func rootPackage(name string) string {
	if i := strings.IndexByte(name, '.'); i >= 0 {
		return name[:i]
	}
	return name
}

// pythonStdlib lists Python 3.12 stdlib modules so we can filter them
// out of pip install commands. Pip can't install stdlib (some return
// 404, others install empty placeholder packages that mask the real
// stdlib import). Keep the list lowercased — Python imports are
// case-sensitive, but PyPI is not, so lowercase comparison is safe
// and forgiving against typos.
var pythonStdlib = map[string]bool{
	"abc": true, "argparse": true, "array": true, "ast": true, "asyncio": true,
	"base64": true, "binascii": true, "bisect": true, "builtins": true, "bz2": true,
	"calendar": true, "codecs": true, "collections": true, "concurrent": true,
	"configparser": true, "contextlib": true, "contextvars": true, "copy": true,
	"copyreg": true, "csv": true, "ctypes": true, "curses": true,
	"dataclasses": true, "datetime": true, "decimal": true, "difflib": true,
	"dis": true, "doctest": true,
	"email": true, "encodings": true, "enum": true, "errno": true,
	"fcntl": true, "filecmp": true, "fileinput": true, "fnmatch": true,
	"fractions": true, "functools": true,
	"gc": true, "getopt": true, "getpass": true, "gettext": true, "glob": true,
	"graphlib": true, "grp": true, "gzip": true,
	"hashlib": true, "heapq": true, "hmac": true, "html": true, "http": true,
	"imaplib": true, "importlib": true, "inspect": true, "io": true,
	"ipaddress": true, "itertools": true,
	"json":      true,
	"keyword":   true,
	"linecache": true, "locale": true, "logging": true, "lzma": true,
	"math": true, "mimetypes": true, "mmap": true, "multiprocessing": true,
	"netrc": true, "numbers": true,
	"operator": true, "os": true,
	"pathlib": true, "pdb": true, "pickle": true, "pickletools": true, "pkgutil": true,
	"platform": true, "plistlib": true, "poplib": true, "posix": true, "posixpath": true,
	"pprint": true, "profile": true, "pstats": true, "pty": true, "pwd": true,
	"py_compile": true, "pyclbr": true, "pydoc": true,
	"queue": true, "quopri": true,
	"random": true, "re": true, "readline": true, "reprlib": true, "resource": true,
	"rlcompleter": true, "runpy": true,
	"sched": true, "secrets": true, "select": true, "selectors": true, "shelve": true,
	"shlex": true, "shutil": true, "signal": true, "site": true, "smtplib": true,
	"sndhdr": true, "socket": true, "socketserver": true, "sqlite3": true, "ssl": true,
	"stat": true, "statistics": true, "string": true, "stringprep": true, "struct": true,
	"subprocess": true, "symtable": true, "sys": true, "sysconfig": true, "syslog": true,
	"tabnanny": true, "tarfile": true, "telnetlib": true, "tempfile": true, "termios": true,
	"test": true, "textwrap": true, "threading": true, "time": true, "timeit": true,
	"tkinter": true, "token": true, "tokenize": true, "tomllib": true, "trace": true,
	"traceback": true, "tracemalloc": true, "tty": true, "turtle": true, "types": true,
	"typing":      true,
	"unicodedata": true, "unittest": true, "urllib": true, "uu": true, "uuid": true,
	"venv":     true,
	"warnings": true, "wave": true, "weakref": true, "webbrowser": true, "wsgiref": true,
	"xdrlib": true, "xml": true, "xmlrpc": true,
	"zipapp": true, "zipfile": true, "zipimport": true, "zlib": true, "zoneinfo": true,
}

func functionResources(res kipperv1.FunctionResources) corev1.ResourceRequirements {
	cpuReq, cpuLim := pairOrDefault(res.CPURequest, res.CPULimit, "50m")
	memReq, memLim := pairOrDefault(res.MemoryRequest, res.MemoryLimit, "64Mi")
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

// adoptWriterSecret sets the Function as controller of `function-<fn>-secrets`,
// the Secret kip and the console write function credentials into, so deleting
// the Function garbage-collects its plaintext secrets the same way deleting
// an App does.
func (r *FunctionReconciler) adoptWriterSecret(ctx context.Context, fn *kipperv1.Function) error {
	name := secretname.Secrets(secretname.KindFunction, fn.Name)
	var secret corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: fn.Namespace}, &secret)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if metav1.GetControllerOf(&secret) != nil {
		return nil
	}
	if secret.Labels[kipperLabel] != kipperValue || secret.Labels["app"] != fn.Name {
		return nil
	}
	if err := controllerutil.SetControllerReference(fn, &secret, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on %s: %w", name, err)
	}
	return r.Update(ctx, &secret)
}

// sweepWriterSecret removes the function's credential Secret during deletion.
// Adoption normally lets Kubernetes garbage-collect it with the Function, but
// a function deleted before its first successful reconcile has adopted
// nothing, and the plaintext Secret would outlive it silently.
func (r *FunctionReconciler) sweepWriterSecret(ctx context.Context, fn *kipperv1.Function) error {
	name := secretname.Secrets(secretname.KindFunction, fn.Name)
	var secret corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: fn.Namespace}, &secret)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if secret.Labels[kipperLabel] != kipperValue || secret.Labels["app"] != fn.Name {
		return nil
	}
	if owner := metav1.GetControllerOf(&secret); owner != nil && owner.UID != fn.UID {
		return nil
	}
	if err := r.Delete(ctx, &secret); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("sweeping %s: %w", name, err)
	}
	return nil
}
