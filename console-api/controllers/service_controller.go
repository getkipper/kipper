package controllers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/serviceui"
	"github.com/getkipper/kipper/console-api/share"
	"github.com/getkipper/kipper/console-api/uisession"
	"github.com/getkipper/kipper/controller/pkg/secretname"
	"github.com/getkipper/kipper/controller/pkg/servicecatalog"
)

const serviceFinalizer = "kipper.run/service-cleanup"

// ServiceReconciler reconciles a Service CR.
type ServiceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Domain is the cluster's base domain (e.g. "example.com"). Used
	// to build per-service UI hostnames as <svc>-<namespace>.<Domain>.
	// Empty disables UI ingress reconciliation — services still come
	// up, they just aren't reachable from the browser.
	Domain string
	// ConsoleAuthCheckURL is the absolute URL of the console-api's
	// /auth/check forwardAuth endpoint, used by Traefik middlewares
	// to gate UI ingress traffic. Typically
	// https://console.<Domain>/api/v1/auth/check.
	ConsoleAuthCheckURL string
	// ShareGrants revokes a deleted service's share links during
	// finalization, before the UI Ingress teardown. Production always
	// sets it (a nil store is a fatal misconfiguration at startup). A nil
	// store here makes finalization fail closed — the finalizer is
	// retained and deletion errors — so only non-deleting unit fixtures
	// may leave it unset.
	ShareGrants *share.GrantStore
}

func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var svc kipperv1.Service
	if err := r.Get(ctx, req.NamespacedName, &svc); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !svc.DeletionTimestamp.IsZero() {
		logger.Info("cleaning up service resources", "service", svc.Name)
		// Revoke the service's share links before releasing the
		// finalizer — fail closed. A grant that outlives its service
		// could otherwise open a recreated namesake if the UID check
		// were ever bypassed. A missing store or a failed revoke keeps
		// the finalizer, so deletion retries rather than orphaning
		// grants.
		if r.ShareGrants == nil {
			return ctrl.Result{}, fmt.Errorf("share grant store not configured; refusing to finalize service %s/%s with its links intact", svc.Namespace, svc.Name)
		}
		if err := r.ShareGrants.RevokeAllForService(ctx, svc.Namespace, svc.Name); err != nil {
			return ctrl.Result{}, fmt.Errorf("revoking share links: %w", err)
		}
		// Unbind everything that names this service, for the same reason and in
		// the same way: fail closed, keep the finalizer, retry. A workload left
		// bound to a service that has gone fails its own reconcile outright and
		// cannot be unbound afterwards, so the service must not finish leaving
		// until nothing depends on it.
		if err := ClearBindingsToService(ctx, r.Client, svc.Name, svc.Namespace); err != nil {
			return ctrl.Result{}, fmt.Errorf("clearing bindings to %s: %w", svc.Name, err)
		}
		controllerutil.RemoveFinalizer(&svc, serviceFinalizer)
		return ctrl.Result{}, r.Update(ctx, &svc)
	}

	if !controllerutil.ContainsFinalizer(&svc, serviceFinalizer) {
		controllerutil.AddFinalizer(&svc, serviceFinalizer)
		if err := r.Update(ctx, &svc); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.reconcileCredentialsSecret(ctx, &svc); err != nil {
		// Said on the object, not only in the controller's log. A service whose
		// credentials Secret belongs to something else never reaches
		// updateStatus, so without this it sits at Pending with the reason
		// visible only to whoever thinks to read the log. That state is reached
		// by a name collision the create-time checks cannot see, a restore among
		// them, so it has to explain itself.
		r.reportCredentialsBlocked(ctx, &svc, err)
		return ctrl.Result{}, fmt.Errorf("reconciling credentials secret: %w", err)
	}

	if err := r.reconcileStatefulSet(ctx, &svc); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling statefulset: %w", err)
	}

	if err := r.reconcileHeadlessService(ctx, &svc); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling headless service: %w", err)
	}

	if err := r.reconcileUIIngress(ctx, &svc); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling ui ingress: %w", err)
	}

	if err := r.reconcileUINetworkPolicy(ctx, &svc); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling ui network policy: %w", err)
	}

	if err := r.updateStatus(ctx, &svc); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	return ctrl.Result{}, nil
}

func (r *ServiceReconciler) reconcileCredentialsSecret(ctx context.Context, svc *kipperv1.Service) error {
	secretName := secretname.ServiceCredentials(svc.Name)

	var existing corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: svc.Namespace}, &existing)
	if err == nil {
		// Ownership is the whole of the service's claim on these credentials,
		// and it is what admits them into a bound workload. A Secret this
		// Service does not own is refused there, so carrying on here would
		// start the engine against a password no binding can use and leave the
		// failure to surface inside somebody's application. Nothing is adopted
		// on the strength of a name, a label or a set of keys: whoever put the
		// object here has to say so by setting the controller reference.
		if owner := metav1.GetControllerOf(&existing); owner == nil || owner.UID != svc.UID {
			return fmt.Errorf("secret %s is not owned by this service, so its credentials cannot be injected into anything bound to it; set its controller reference to this Service, or remove it if the service holds no data yet", secretName)
		}
		return r.ensureCredentialDefaults(ctx, &existing, svc.Spec.Type)
	}
	if !errors.IsNotFound(err) {
		return err
	}

	if err := r.refuseToMintOverExistingData(ctx, svc); err != nil {
		return err
	}

	catalog := serviceCatalog(svc.Spec.Type)
	password := generatePassword()
	host := fmt.Sprintf("%s.%s.svc.cluster.local", svc.Name, svc.Namespace)

	var data map[string][]byte
	if svc.Spec.Type == "minio" {
		// MinIO is S3: bound apps want a single endpoint URL plus an
		// access key / secret key, not host/port/user/pass. The
		// endpoint is composed here because a bound app receives its
		// environment through envFrom, and the kubelet passes those
		// values straight into the container without expanding
		// anything in them. $(VAR) is expanded only in a container's
		// own env, command and args, so an app cannot assemble the
		// endpoint from HOST and PORT wherever Kipper puts them.
		//
		// A template resolves it now — ENDPOINT=http://${S3_HOST}:${S3_PORT}
		// is rendered before the pod sees it — so this is the default
		// rather than the only route.
		//
		// The rule the other way round still holds for anything added
		// to directEnv: link addresses and runtime variables are
		// container env, so the kubelet does expand $(VAR) in them,
		// against every envFrom value as well as their own.
		data = map[string][]byte{
			"ENDPOINT":   []byte(fmt.Sprintf("http://%s:%d", host, catalog.port)),
			"ACCESS_KEY": []byte("kipper"),
			"SECRET_KEY": []byte(password),
		}
	} else {
		data = map[string][]byte{
			"HOST": []byte(host),
			"PORT": []byte(fmt.Sprintf("%d", catalog.port)),
		}
		// A service started without authentication gets no credentials.
		// Minting them anyway put a generated password into every bound
		// workload for a server that never asks for one, and redis does
		// worse than ignore it: it answers AUTH with an error when no
		// password is set, so redis://:${REDIS_PASSWORD}@host fails to
		// connect and names the wrong cause.
		if servicecatalog.HasAuth(svc.Spec.Type) {
			data["USERNAME"] = []byte("kipper")
			data["PASSWORD"] = []byte(password)
		}
		// Type-specific defaults: NAME=app for databases, VHOST=/ for
		// rabbitmq, nothing for minio (buckets are per-binding, not a
		// single logical namespace).
		for k, v := range kipperv1.CredentialDefaults(svc.Spec.Type) {
			data[k] = []byte(v)
		}
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: svc.Namespace,
			Labels:    serviceLabels(svc),
		},
		Data: data,
	}

	if err := controllerutil.SetControllerReference(svc, secret, r.Scheme); err != nil {
		return err
	}

	return r.Create(ctx, secret)
}

// refuseToMintOverExistingData stops the reconciler generating a password for a
// service that already has a database, which would come back holding a
// credential its own data does not know.
//
// A service's data outlives its credentials Secret. The Secret can go while the
// volume stays: garbage collection deletes a dependent whose owner UID no longer
// resolves, which is what a Velero restore leaves behind when the Service CR
// comes back with a new UID, and an operator can delete one by hand just as
// easily. Minting a replacement is silent and irreversible from the engine's
// side, because postgres, mysql, mongodb and rabbitmq only read the password
// when they initialise, so the database keeps the old one and every bound
// workload starts failing to authenticate with no indication of why.
//
// The claim rests on the volume rather than on any metadata: a data volume for
// this service means an engine has already initialised and made up its mind. A
// genuinely new service has none, and a deleted one has its volumes removed
// alongside it, so this only refuses where the two have come apart.
func (r *ServiceReconciler) refuseToMintOverExistingData(ctx context.Context, svc *kipperv1.Service) error {
	// The StatefulSet's volume claim template is named "data" and it runs a
	// single replica, so its claim is data-<service>-0.
	claim := fmt.Sprintf("data-%s-0", svc.Name)
	var pvc corev1.PersistentVolumeClaim
	err := r.Get(ctx, types.NamespacedName{Name: claim, Namespace: svc.Namespace}, &pvc)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("service %s has data in %s but no credentials secret, and generating a new password would lock it out of that data; restore %s from a backup, or delete the volume to start the service empty",
		svc.Name, claim, secretname.ServiceCredentials(svc.Name))
}

// ensureCredentialDefaults brings an existing credentials Secret into
// the current shape: adds any missing type-specific defaults (e.g.
// VHOST=/ on a rabbitmq secret that pre-dates the rabbitmq fix) and
// prunes type-specific keys that no longer belong (e.g. NAME=app on a
// rabbitmq or minio secret — a leftover from when every authed
// service inherited the database-shaped credentials). Whatever base
// keys a service type carries are preserved: HOST/PORT/USERNAME/PASSWORD
// for most, ENDPOINT/ACCESS_KEY/SECRET_KEY for minio (S3).
//
// A type that starts without authentication keeps HOST and PORT alone,
// so a Secret minted before that rule loses the credentials it carried.
// Pruning them is safe in the direction that matters: redis, opensearch
// and mailhog never read the password when they started, so no server
// is locked out of anything by its removal.
func (r *ServiceReconciler) ensureCredentialDefaults(ctx context.Context, secret *corev1.Secret, svcType string) error {
	desired := kipperv1.CredentialDefaults(svcType)
	// A pre-existing Secret with no data (created out of band, or
	// drained of keys) has Data == nil; assigning to a nil map
	// panics. Initialise before writing.
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	updated := false
	for k, v := range desired {
		if _, ok := secret.Data[k]; !ok {
			secret.Data[k] = []byte(v)
			updated = true
		}
	}
	// Stale type-specific keys to prune. NAME belongs to the database
	// services; VHOST to rabbitmq. If the current service type doesn't
	// want a key, remove it so EnvFrom stops injecting a meaningless
	// env var (e.g. AMQP_NAME=app).
	stale := []string{"NAME", "VHOST"}
	// MinIO authenticates under ACCESS_KEY/SECRET_KEY and has neither of
	// these, so this only reaches a type that carries them and does not
	// use them.
	if !servicecatalog.HasAuth(svcType) {
		stale = append(stale, "USERNAME", "PASSWORD")
	}
	for _, k := range stale {
		if _, wanted := desired[k]; wanted {
			continue
		}
		if _, present := secret.Data[k]; present {
			delete(secret.Data, k)
			updated = true
		}
	}
	if !updated {
		return nil
	}
	return r.Update(ctx, secret)
}

func (r *ServiceReconciler) reconcileStatefulSet(ctx context.Context, svc *kipperv1.Service) error {
	catalog := serviceCatalog(svc.Spec.Type)
	labels := serviceLabels(svc)

	storage := svc.Spec.Storage
	if storage == "" {
		storage = catalog.defaultStorage
	}

	version := svc.Spec.Version
	if version == "" {
		version = catalog.defaultVersion
	}

	image := fmt.Sprintf("%s:%s", catalog.image, version)

	containerPorts := []corev1.ContainerPort{{Name: "main", ContainerPort: catalog.port}}
	if catalog.ui != nil {
		containerPorts = append(containerPorts, corev1.ContainerPort{Name: "ui", ContainerPort: catalog.ui.port})
	}
	container := corev1.Container{
		Name:    svc.Name,
		Image:   image,
		Command: catalog.command,
		Args:    catalog.args,
		Ports:   containerPorts,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: catalog.dataPath},
		},
	}

	// Start from the type's default profile, then honour any explicit CR
	// fields, so every service pod always declares requests and limits.
	pCPUReq, pCPULim, pMemReq, pMemLim := profileResources(catalog.resourceProfile)
	cpuReq, cpuLim := ResolveResourcePair(svc.Spec.Resources.CPURequest, svc.Spec.Resources.CPULimit, pCPUReq, pCPULim)
	memReq, memLim := ResolveResourcePair(svc.Spec.Resources.MemoryRequest, svc.Spec.Resources.MemoryLimit, pMemReq, pMemLim)
	container.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpuReq),
			corev1.ResourceMemory: resource.MustParse(memReq),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpuLim),
			corev1.ResourceMemory: resource.MustParse(memLim),
		},
	}

	// Set env vars from catalog
	container.Env = catalog.envVars(svc.Name)

	replicas := int32(1)
	storageClass := "longhorn-single"

	// Init container removes lost+found created by ext4 formatting on Longhorn volumes.
	// MySQL refuses to initialise if the data directory contains any files.
	initContainers := []corev1.Container{
		{
			Name:    "remove-lost-found",
			Image:   "busybox:1.36",
			Command: []string{"sh", "-c", fmt.Sprintf("rm -rf %s/lost+found", catalog.dataPath)},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "data", MountPath: catalog.dataPath},
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("10m"),
					corev1.ResourceMemory: resource.MustParse("16Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("64Mi"),
				},
			},
		},
	}

	desired := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svc.Name,
			Namespace: svc.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: svc.Name,
			Selector:    &metav1.LabelSelector{MatchLabels: map[string]string{"app": svc.Name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					InitContainers: initContainers,
					Containers:     []corev1.Container{container},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "data"},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						StorageClassName: &storageClass,
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse(storage),
							},
						},
					},
				},
			},
		},
	}

	if err := controllerutil.SetControllerReference(svc, desired, r.Scheme); err != nil {
		return err
	}

	var existing appsv1.StatefulSet
	err := r.Get(ctx, types.NamespacedName{Name: svc.Name, Namespace: svc.Namespace}, &existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	// Adjustments made directly on the StatefulSet (a VPA, an operator, a
	// manual edit) live on the workload, not the CR. Keep them for any
	// resource type the CR does not pin, so pinning CPU alone doesn't reset a
	// raised memory limit back to the profile baseline.
	cpuPinned := svc.Spec.Resources.CPURequest != "" || svc.Spec.Resources.CPULimit != ""
	memPinned := svc.Spec.Resources.MemoryRequest != "" || svc.Spec.Resources.MemoryLimit != ""
	if (!cpuPinned || !memPinned) && len(existing.Spec.Template.Spec.Containers) > 0 {
		preserveUnpinnedResources(
			&desired.Spec.Template.Spec.Containers[0].Resources,
			existing.Spec.Template.Spec.Containers[0].Resources,
			cpuPinned, memPinned,
		)
	}
	existing.Spec.Template.Spec.Containers = desired.Spec.Template.Spec.Containers
	existing.Labels = desired.Labels
	return r.Update(ctx, &existing)
}

func (r *ServiceReconciler) reconcileHeadlessService(ctx context.Context, svc *kipperv1.Service) error {
	catalog := serviceCatalog(svc.Spec.Type)
	labels := serviceLabels(svc)

	ports := []corev1.ServicePort{
		{Name: "main", Port: catalog.port, TargetPort: intstr.FromInt32(catalog.port)},
	}
	if catalog.ui != nil {
		ports = append(ports, corev1.ServicePort{
			Name: "ui", Port: catalog.ui.port, TargetPort: intstr.FromInt32(catalog.ui.port),
		})
	}

	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svc.Name,
			Namespace: svc.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: "None",
			Selector:  map[string]string{"app": svc.Name},
			Ports:     ports,
		},
	}

	if err := controllerutil.SetControllerReference(svc, desired, r.Scheme); err != nil {
		return err
	}

	var existing corev1.Service
	err := r.Get(ctx, types.NamespacedName{Name: svc.Name, Namespace: svc.Namespace}, &existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	// Update if ports drifted — catalog changes (e.g. adding a UI
	// port to a service type that didn't have one) must propagate
	// to already-running services.
	if !servicePortsEqual(existing.Spec.Ports, desired.Spec.Ports) {
		existing.Spec.Ports = desired.Spec.Ports
		existing.Labels = desired.Labels
		return r.Update(ctx, &existing)
	}
	return nil
}

// servicePortsEqual compares two Service port lists by name + port +
// target port. Order matters: the existing object's ports are
// authoritative for ordering once it exists, but a name/port mismatch
// is treated as drift even if both sides carry the same set.
func servicePortsEqual(a, b []corev1.ServicePort) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Port != b[i].Port || a[i].TargetPort != b[i].TargetPort {
			return false
		}
	}
	return true
}

// uiHostname returns the per-service UI hostname Kipper exposes.
// For custom-domain clusters: <svc>-<namespace>.<cluster-domain>
// (one label under the wildcard cert). For free *.kipper.run
// clusters the cluster domain is itself a subdomain, so
// domain.SubdomainFor flattens the prefix with a hyphen to keep
// the result a single label under *.kipper.run — otherwise
// wildcard DNS and TLS would not cover the host.
func (r *ServiceReconciler) uiHostname(svc *kipperv1.Service) string {
	return serviceui.Hostname(svc.Name, svc.Namespace, r.Domain)
}

// reconcileUIIngress materialises a TLS Ingress + Traefik
// forwardAuth Middleware for services whose catalog ships a
// browseable UI (MailHog, future RabbitMQ Management, etc.). The
// Middleware points at the console-api /auth/check endpoint so
// only Dex-authenticated users reach the backend.
//
// When the catalog has no UI block, or the cluster has no Domain
// configured, any previously-created Ingress + Middleware are
// deleted so a flag flip is reversible without orphans.
func (r *ServiceReconciler) reconcileUIIngress(ctx context.Context, svc *kipperv1.Service) error {
	catalog := serviceCatalog(svc.Spec.Type)
	ingressName := svc.Name + "-ui"
	middlewareName := svc.Name + "-forward-auth"
	stripName := svc.Name + "-cookie-strip"

	if catalog.ui == nil || r.Domain == "" || r.ConsoleAuthCheckURL == "" {
		return r.deleteUIRoutingResources(ctx, svc.Namespace, ingressName, middlewareName, stripName)
	}

	if err := r.reconcileForwardAuthMiddleware(ctx, svc, middlewareName); err != nil {
		return err
	}
	if err := r.reconcileCookieStripMiddleware(ctx, svc, stripName); err != nil {
		return err
	}

	host := r.uiHostname(svc)
	pathType := networkingv1.PathTypePrefix
	path := "/"
	if catalog.ui.path != "" {
		path = catalog.ui.path
	}

	// On a kipper.run host the gateway terminates the public TLS and this
	// Ingress must fall through to Traefik's default store (the pinned hop
	// certificate): no cert-manager annotation (its HTTP-01 challenge would
	// 404 at the gateway) and no secretName. See the matching note in
	// app_controller.go reconcileIngress.
	gatewayTLS := strings.HasSuffix(host, ".kipper.run")

	// Middleware order matters and is applied left to right:
	//   rate-limit  — throttle the public gate (DoS protection);
	//   forward-auth — validate the Dex/share credential, which needs
	//                  to read the request's cookies;
	//   cookie-strip — blank the Cookie header before the request
	//                  reaches the backend UI, so a share credential
	//                  never lands in a third-party container's logs.
	annotations := map[string]string{
		"traefik.ingress.kubernetes.io/router.middlewares": fmt.Sprintf(
			"traefik-rate-limit@kubernetescrd,%s-%s@kubernetescrd,%s-%s@kubernetescrd",
			svc.Namespace, middlewareName, svc.Namespace, stripName,
		),
	}
	if !gatewayTLS {
		annotations["cert-manager.io/cluster-issuer"] = "letsencrypt-prod"
	}

	tlsEntry := networkingv1.IngressTLS{Hosts: []string{host}}
	if !gatewayTLS {
		tlsEntry.SecretName = ingressName + "-tls"
	}

	desired := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ingressName,
			Namespace: svc.Namespace,
			Labels: map[string]string{
				"app":             svc.Name,
				kipperLabel:       kipperValue,
				"kipper.run/role": "service-ui",
			},
			Annotations: annotations,
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: strPtr("traefik"),
			TLS:              []networkingv1.IngressTLS{tlsEntry},
			Rules: []networkingv1.IngressRule{{
				Host: host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path:     path,
							PathType: &pathType,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: svc.Name,
									Port: networkingv1.ServiceBackendPort{Name: "ui"},
								},
							},
						}},
					},
				},
			}},
		},
	}

	if err := controllerutil.SetControllerReference(svc, desired, r.Scheme); err != nil {
		return err
	}

	var existing networkingv1.Ingress
	err := r.Get(ctx, types.NamespacedName{Name: ingressName, Namespace: svc.Namespace}, &existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Spec = desired.Spec
	existing.Annotations = desired.Annotations
	existing.Labels = desired.Labels
	return r.Update(ctx, &existing)
}

// reconcileForwardAuthMiddleware creates the Traefik Middleware
// resource the Ingress references. It points at the console-api
// /auth/check endpoint and forwards X-Auth-User on success so any
// downstream UI that grows its own user concept can auto-login.
func (r *ServiceReconciler) reconcileForwardAuthMiddleware(ctx context.Context, svc *kipperv1.Service, name string) error {
	gvk := schema.GroupVersionKind{Group: "traefik.io", Version: "v1alpha1", Kind: "Middleware"}
	// The gate re-mints the per-host session cookie on the /auth/check
	// response when it is close to expiry; addAuthCookiesToResponse tells
	// Traefik to copy exactly that cookie back to the browser (Traefik
	// otherwise drops Set-Cookie headers from a forwardAuth response).
	// Traefik also suppresses a backend Set-Cookie of the same name, which
	// is moot today because reconcileCookieStripMiddleware blanks the
	// backend's cookies anyway, but keeps the session cookie ours alone.
	sessionCookie := uisession.CookieName(r.uiHostname(svc))
	desired := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "traefik.io/v1alpha1",
			"kind":       "Middleware",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": svc.Namespace,
				"labels": map[string]interface{}{
					"app":       svc.Name,
					kipperLabel: kipperValue,
				},
			},
			"spec": map[string]interface{}{
				"forwardAuth": map[string]interface{}{
					"address":                  r.ConsoleAuthCheckURL,
					"trustForwardHeader":       true,
					"authResponseHeaders":      []interface{}{"X-Auth-User"},
					"addAuthCookiesToResponse": []interface{}{sessionCookie},
				},
			},
		},
	}
	desired.SetGroupVersionKind(gvk)
	if err := controllerutil.SetControllerReference(svc, desired, r.Scheme); err != nil {
		return err
	}

	var existing unstructured.Unstructured
	existing.SetGroupVersionKind(gvk)
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: svc.Namespace}, &existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Object["spec"] = desired.Object["spec"]
	return r.Update(ctx, &existing)
}

// reconcileCookieStripMiddleware manages the Traefik headers Middleware
// that blanks the Cookie request header before the request reaches the
// service UI backend. It runs after forwardAuth (which needs to read
// the cookie), so a validated share or Dex cookie authorises the
// request but is never forwarded to the third-party UI container. Every
// current browseable UI (MailHog) is cookieless, so blanking all
// cookies is safe; a UI that needs its own cookies is a v1 concern.
func (r *ServiceReconciler) reconcileCookieStripMiddleware(ctx context.Context, svc *kipperv1.Service, name string) error {
	gvk := schema.GroupVersionKind{Group: "traefik.io", Version: "v1alpha1", Kind: "Middleware"}
	desired := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "traefik.io/v1alpha1",
			"kind":       "Middleware",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": svc.Namespace,
				"labels": map[string]interface{}{
					"app":       svc.Name,
					kipperLabel: kipperValue,
				},
			},
			"spec": map[string]interface{}{
				"headers": map[string]interface{}{
					"customRequestHeaders": map[string]interface{}{
						"Cookie": "",
					},
				},
			},
		},
	}
	desired.SetGroupVersionKind(gvk)
	if err := controllerutil.SetControllerReference(svc, desired, r.Scheme); err != nil {
		return err
	}

	var existing unstructured.Unstructured
	existing.SetGroupVersionKind(gvk)
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: svc.Namespace}, &existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Object["spec"] = desired.Object["spec"]
	return r.Update(ctx, &existing)
}

// deleteUIRoutingResources tears down the Ingress + Middlewares that
// reconcileUIIngress would have written. Called on the no-UI path
// so a catalog change that removes a `ui` block (or a Domain
// unset) reverses cleanly.
func (r *ServiceReconciler) deleteUIRoutingResources(ctx context.Context, namespace, ingressName string, middlewareNames ...string) error {
	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: ingressName, Namespace: namespace}}
	if err := r.Delete(ctx, ing); err != nil && !errors.IsNotFound(err) {
		return err
	}
	for _, name := range middlewareNames {
		mw := &unstructured.Unstructured{}
		mw.SetGroupVersionKind(schema.GroupVersionKind{Group: "traefik.io", Version: "v1alpha1", Kind: "Middleware"})
		mw.SetName(name)
		mw.SetNamespace(namespace)
		if err := r.Delete(ctx, mw); err != nil && !errors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// ingressControllerSelector is the resolved identity of the cluster's
// ingress controller pods — the only peers allowed to reach a
// service UI port. Sourced from the kipper-system/ingress-controller
// ConfigMap, with built-in defaults that match a stock Traefik
// Helm-chart install. Operators on non-standard setups (custom
// labels, an Nginx ingress controller, a different namespace
// restriction) override by editing the ConfigMap; no code change
// or restart needed.
type ingressControllerSelector struct {
	// LabelKey + LabelValue identify the ingress controller pods.
	// Defaults: "app.kubernetes.io/name" + "traefik".
	LabelKey   string
	LabelValue string
	// Namespace, when set, restricts the policy to ingress
	// controller pods in that namespace. Empty means "any
	// namespace that happens to run a pod with the matching
	// label" — the safer default on multi-tenant clusters where
	// the operator can't fully predict where ingress controllers
	// land.
	Namespace string
}

const (
	ingressControllerConfigMapName      = "ingress-controller"
	ingressControllerConfigMapNamespace = "kipper-system"
	defaultIngressControllerLabelKey    = "app.kubernetes.io/name"
	defaultIngressControllerLabelValue  = "traefik"
)

// resolveIngressController reads the cluster's ingress-controller
// config from kipper-system/ingress-controller and returns the
// selector the NetworkPolicy template should use. Missing
// ConfigMap, missing keys, or read errors all fall through to the
// Traefik-Helm defaults — the same behaviour every Kipper install
// has shipped with, so a clean cluster needs no extra
// configuration.
func (r *ServiceReconciler) resolveIngressController(ctx context.Context) ingressControllerSelector {
	sel := ingressControllerSelector{
		LabelKey:   defaultIngressControllerLabelKey,
		LabelValue: defaultIngressControllerLabelValue,
	}
	var cm corev1.ConfigMap
	err := r.Get(ctx, types.NamespacedName{
		Name:      ingressControllerConfigMapName,
		Namespace: ingressControllerConfigMapNamespace,
	}, &cm)
	if err != nil {
		return sel
	}
	if v := cm.Data["labelKey"]; v != "" {
		sel.LabelKey = v
	}
	if v := cm.Data["labelValue"]; v != "" {
		sel.LabelValue = v
	}
	if v := cm.Data["namespace"]; v != "" {
		sel.Namespace = v
	}
	return sel
}

// reconcileUINetworkPolicy locks the UI port down to the cluster's
// ingress controller. SMTP / binding traffic on the main port is
// untouched — that's the path bound apps use, which already lives
// inside the cluster network.
//
// Without this policy, any pod in the same cluster (including
// other tenants' workloads on multi-tenant deployments) could
// reach the MailHog UI directly on its ClusterIP, bypassing the
// forwardAuth gate. The policy is best-effort: clusters without a
// running NetworkPolicy controller (e.g. raw k3s without flannel
// network policy) will accept the resource but not enforce it.
//
// Selector is sourced from resolveIngressController so operators
// can adapt to non-standard ingress setups (custom labels, Nginx,
// stricter namespace restrictions) without changing Kipper code.
// Defaults match a stock Traefik Helm install.
func (r *ServiceReconciler) reconcileUINetworkPolicy(ctx context.Context, svc *kipperv1.Service) error {
	catalog := serviceCatalog(svc.Spec.Type)
	name := svc.Name + "-ui-traffic"

	if catalog.ui == nil {
		existing := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: svc.Namespace}}
		if err := r.Delete(ctx, existing); err != nil && !errors.IsNotFound(err) {
			return err
		}
		return nil
	}

	uiPort := intstr.FromInt32(catalog.ui.port)
	mainPort := intstr.FromInt32(catalog.port)
	tcp := corev1.ProtocolTCP

	ingress := r.resolveIngressController(ctx)
	nsSelector := &metav1.LabelSelector{}
	if ingress.Namespace != "" {
		nsSelector = &metav1.LabelSelector{
			MatchLabels: map[string]string{"kubernetes.io/metadata.name": ingress.Namespace},
		}
	}

	desired := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: svc.Namespace,
			Labels: map[string]string{
				"app":             svc.Name,
				kipperLabel:       kipperValue,
				"kipper.run/role": "service-ui",
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": svc.Name}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				// UI port: only ingress-controller pods so the
				// forwardAuth gate isn't bypassed. Selector +
				// namespace come from the configurable
				// ingressControllerSelector so a chart upgrade
				// that renames labels, or a swap to a different
				// ingress controller, is a ConfigMap edit
				// rather than a Kipper release.
				{
					From: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: nsSelector,
						PodSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{ingress.LabelKey: ingress.LabelValue},
						},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{Port: &uiPort, Protocol: &tcp}},
				},
				// Main service port (SMTP for MailHog): open to
				// every pod. Without this, the NetworkPolicy's
				// default-deny on selected pods would block the
				// binding traffic apps depend on.
				{
					Ports: []networkingv1.NetworkPolicyPort{{Port: &mainPort, Protocol: &tcp}},
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(svc, desired, r.Scheme); err != nil {
		return err
	}

	var existing networkingv1.NetworkPolicy
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: svc.Namespace}, &existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	return r.Update(ctx, &existing)
}

func (r *ServiceReconciler) updateStatus(ctx context.Context, svc *kipperv1.Service) error {
	var sts appsv1.StatefulSet
	err := r.Get(ctx, types.NamespacedName{Name: svc.Name, Namespace: svc.Namespace}, &sts)
	if errors.IsNotFound(err) {
		svc.Status.Phase = "Pending"
		return r.Status().Update(ctx, svc)
	}
	if err != nil {
		return err
	}

	catalog := serviceCatalog(svc.Spec.Type)
	svc.Status.Host = fmt.Sprintf("%s.%s.svc.cluster.local", svc.Name, svc.Namespace)
	svc.Status.Port = catalog.port
	svc.Status.CredentialsSecret = secretname.ServiceCredentials(svc.Name)

	if sts.Status.ReadyReplicas > 0 {
		svc.Status.Phase = "Running"
	} else {
		svc.Status.Phase = "Pending"
	}

	return r.Status().Update(ctx, svc)
}

func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kipperv1.Service{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}

// Service catalog

type serviceCatalogEntry struct {
	image          string
	defaultVersion string
	// port is the binding port — what gets written into the
	// credentials Secret's PORT field and what bound apps reach via
	// the k8s Service. Always exposed on the pod and the Service.
	port int32
	// ui, if set, declares a second port on the service pod that
	// hosts a browseable web UI (MailHog's inbox, RabbitMQ
	// Management, etc.). The service reconciler exposes the port
	// on the headless Service and creates a TLS Ingress at
	// <svc>-<namespace>.<cluster-domain> gated by Traefik
	// forwardAuth so only Dex-authenticated users reach it.
	ui             *serviceCatalogUI
	dataPath       string
	defaultStorage string
	// resourceProfile names the profileResources() preset applied when a
	// service pins no explicit resources, so every service pod declares
	// requests and limits (databases need more than a plain app).
	resourceProfile string
	envVars         func(name string) []corev1.EnvVar
	command         []string
	args            []string
}

// serviceCatalogUI describes the browseable UI a service ships.
// Kept minimal: port + the path the backend serves under. Auth,
// TLS, and security headers are added by the reconciler from a
// fixed policy so individual catalog entries can't accidentally
// opt out of them.
type serviceCatalogUI struct {
	port int32
	path string // typically "/"
}

func serviceCatalog(svcType string) serviceCatalogEntry {
	switch svcType {
	case "postgres":
		return serviceCatalogEntry{
			image: "postgres", defaultVersion: "16-alpine", port: 5432, resourceProfile: "database",
			dataPath: "/var/lib/postgresql/data", defaultStorage: "5Gi",
			envVars: func(name string) []corev1.EnvVar {
				return []corev1.EnvVar{
					{Name: "POSTGRES_USER", Value: "kipper"},
					{Name: "POSTGRES_DB", Value: "app"},
					{Name: "PGDATA", Value: "/var/lib/postgresql/data/pgdata"},
					secretEnvPassword("POSTGRES_PASSWORD", secretname.ServiceCredentials(name)),
				}
			},
		}
	case "mysql":
		return serviceCatalogEntry{
			image: "mysql", defaultVersion: "8-oracle", port: 3306, resourceProfile: "database",
			dataPath: "/var/lib/mysql", defaultStorage: "5Gi",
			envVars: func(name string) []corev1.EnvVar {
				return []corev1.EnvVar{
					{Name: "MYSQL_USER", Value: "kipper"},
					{Name: "MYSQL_DATABASE", Value: "app"},
					secretEnvPassword("MYSQL_PASSWORD", secretname.ServiceCredentials(name)),
					secretEnvPassword("MYSQL_ROOT_PASSWORD", secretname.ServiceCredentials(name)),
				}
			},
		}
	case "redis":
		return serviceCatalogEntry{
			image: "redis", defaultVersion: "7-alpine", port: 6379, resourceProfile: "standard",
			dataPath: "/data", defaultStorage: "1Gi",
			envVars: func(_ string) []corev1.EnvVar { return nil },
		}
	case "mongodb":
		return serviceCatalogEntry{
			image: "mongo", defaultVersion: "7", port: 27017, resourceProfile: "database",
			dataPath: "/data/db", defaultStorage: "5Gi",
			envVars: func(name string) []corev1.EnvVar {
				return []corev1.EnvVar{
					{Name: "MONGO_INITDB_ROOT_USERNAME", Value: "kipper"},
					secretEnvPassword("MONGO_INITDB_ROOT_PASSWORD", secretname.ServiceCredentials(name)),
				}
			},
		}
	case "rabbitmq":
		return serviceCatalogEntry{
			image: "rabbitmq", defaultVersion: "3-management-alpine", port: 5672, resourceProfile: "standard",
			dataPath: "/var/lib/rabbitmq", defaultStorage: "1Gi",
			envVars: func(name string) []corev1.EnvVar {
				return []corev1.EnvVar{
					{Name: "RABBITMQ_DEFAULT_USER", Value: "kipper"},
					secretEnvPassword("RABBITMQ_DEFAULT_PASS", secretname.ServiceCredentials(name)),
				}
			},
		}
	case "opensearch":
		return serviceCatalogEntry{
			image: "opensearchproject/opensearch", defaultVersion: "2", port: 9200, resourceProfile: "jvm",
			dataPath: "/usr/share/opensearch/data", defaultStorage: "5Gi",
			envVars: func(_ string) []corev1.EnvVar {
				return []corev1.EnvVar{
					{Name: "discovery.type", Value: "single-node"},
					{Name: "DISABLE_SECURITY_PLUGIN", Value: "true"},
				}
			},
		}
	case "minio":
		return serviceCatalogEntry{
			// MinIO tags a release rather than a version line, so there is no
			// patch-floating tag to sit on the way postgres:16-alpine does.
			// Upstream has published nothing since this release.
			image: "minio/minio", defaultVersion: "RELEASE.2025-09-07T16-13-09Z", port: 9000, resourceProfile: "standard",
			dataPath: "/data", defaultStorage: "10Gi",
			command: []string{"minio"},
			args:    []string{"server", "/data", "--console-address", ":9001"},
			envVars: func(name string) []corev1.EnvVar {
				return []corev1.EnvVar{
					{Name: "MINIO_ROOT_USER", Value: "kipper"},
					secretEnv("MINIO_ROOT_PASSWORD", secretname.ServiceCredentials(name), "SECRET_KEY"),
				}
			},
		}
	case "mailhog":
		// MailHog catches outgoing SMTP traffic for dev / test
		// environments and serves a browseable inbox on port 8025.
		//
		// Image pin: the upstream `mailhog/mailhog` repo is archived;
		// v1.0.1 is the last published release (verified against
		// Docker Hub on 2026-05-20). It is amd64-only — ARM clusters
		// need a community fork (e.g. cd2/mailhog) until upstream is
		// revived or replaced. Storage default is empty (in-memory
		// inbox); users wanting persistence pass --storage on
		// `kip service add`.
		port := int32(8025)
		return serviceCatalogEntry{
			image: "mailhog/mailhog", defaultVersion: "v1.0.1", port: 1025, resourceProfile: "standard",
			ui:             &serviceCatalogUI{port: port, path: "/"},
			dataPath:       "/maildir",
			defaultStorage: "1Gi",
			envVars:        func(_ string) []corev1.EnvVar { return nil },
		}
	default:
		return serviceCatalogEntry{
			// 3.24 is the current stable line, and floats on its patches the
			// way the database entries above do.
			image: "alpine", defaultVersion: "3.24", port: 80, resourceProfile: "standard",
			dataPath: "/data", defaultStorage: "1Gi",
			envVars: func(_ string) []corev1.EnvVar { return nil },
		}
	}
}

func secretEnvPassword(envName, secretName string) corev1.EnvVar {
	return secretEnv(envName, secretName, "PASSWORD")
}

func secretEnv(envName, secretName, key string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: envName,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  key,
			},
		},
	}
}

func serviceLabels(svc *kipperv1.Service) map[string]string {
	return map[string]string{
		"app":                     svc.Name,
		kipperLabel:               kipperValue,
		"kipper.run/service-type": svc.Spec.Type,
		// Record the same profile reconcileStatefulSet defaults from, so the
		// resource controller's floor and nil-defaults stay in lockstep with
		// the resources the reconciler applies.
		"kipper.run/resource-profile": serviceCatalog(svc.Spec.Type).resourceProfile,
	}
}

// ResolveResourcePair applies the request/limit contract shared by
// AppResources and ServiceResources on top of a profile baseline: an explicit
// request and limit are honoured as given; a one-sided value mirrors to the
// other side (Guaranteed QoS) so request never exceeds limit; and when neither
// is set the profile default is used.
func ResolveResourcePair(userReq, userLim, profileReq, profileLim string) (req, lim string) {
	switch {
	case userReq != "" && userLim != "":
		return userReq, userLim
	case userReq != "":
		return userReq, userReq
	case userLim != "":
		return userLim, userLim
	default:
		return profileReq, profileLim
	}
}

// preserveUnpinnedResources keeps the live workload's values for any resource
// type (cpu or memory) the CR does not pin, so adjustments made directly on the
// Deployment or StatefulSet (a VPA, an operator, a manual edit) survive a
// reconcile. A type the CR pins on either its request or limit is left as the
// reconciler computed it from the CR. Working per type means pinning one type
// no longer resets the other to its profile baseline.
func preserveUnpinnedResources(desired *corev1.ResourceRequirements, live corev1.ResourceRequirements, cpuPinned, memPinned bool) {
	keep := func(name corev1.ResourceName) {
		liveReq, hasReq := live.Requests[name]
		liveLim, hasLim := live.Limits[name]
		if !hasReq && !hasLim {
			return
		}
		if desired.Requests == nil {
			desired.Requests = corev1.ResourceList{}
		}
		if desired.Limits == nil {
			desired.Limits = corev1.ResourceList{}
		}
		// Copy the pair coherently. Copying a live request without its limit
		// onto a desired that still carries a smaller limit would make
		// request > limit, which the API server rejects, wedging the reconcile.
		// When the live side only pins one of the pair, mirror it so request
		// never exceeds limit.
		switch {
		case hasReq && hasLim:
			desired.Requests[name] = liveReq
			desired.Limits[name] = liveLim
		case hasReq:
			desired.Requests[name] = liveReq
			desired.Limits[name] = liveReq
		default:
			desired.Requests[name] = liveLim
			desired.Limits[name] = liveLim
		}
	}
	if !cpuPinned {
		keep(corev1.ResourceCPU)
	}
	if !memPinned {
		keep(corev1.ResourceMemory)
	}
}

func generatePassword() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// reportCredentialsBlocked writes onto the Service why it cannot proceed.
//
// Best effort: the reconcile is failing already, and losing the explanation is
// better than losing the error that caused it.
func (r *ServiceReconciler) reportCredentialsBlocked(ctx context.Context, svc *kipperv1.Service, cause error) {
	svc.Status.Phase = "Failed"
	meta.SetStatusCondition(&svc.Status.Conditions, metav1.Condition{
		Type:               "CredentialsReady",
		Status:             metav1.ConditionFalse,
		Reason:             "SecretNotOwned",
		Message:            cause.Error(),
		ObservedGeneration: svc.Generation,
	})
	if err := r.Status().Update(ctx, svc); err != nil {
		log.FromContext(ctx).Error(err, "recording why the credentials secret is blocked")
	}
}
