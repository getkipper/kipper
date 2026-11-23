package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/controller/pkg/secretname"
	"github.com/getkipper/kipper/controller/pkg/servicecatalog"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

// Options describes a stateful service to deploy.
type Options struct {
	Name         string
	Namespace    string
	Type         string
	Storage      string
	MemoryLimit  string
	CPULimit     string
	ImageVersion string
	MajorUpgrade bool
}

// Status holds summary info for a running service.
type Status struct {
	Name    string
	Type    string
	Status  string
	Storage string
	Ready   string
	// BlockedReason and BlockedMessage carry whichever refusal the service is
	// standing on: credentials the reconciler would not use, a name that belongs
	// to something else, or a deletion that cannot finish. Empty on a service
	// that is fine, and on any cluster older than the conditions.
	BlockedReason  string
	BlockedMessage string
}

// ConnectionInfo holds the connection details for a service.
type ConnectionInfo struct {
	Type     string
	Host     string
	Port     int32
	Username string
	Password string
	Database string
	URL      string
}

// Manager creates and manages stateful services.
//
// Client is used to read workload state (StatefulSets, Secrets) and PVCs.
// Dynamic is used to read Service CRs (services.kipper.run), which are
// the source of truth for what services exist. The CR is what both the
// CLI and the console-api agree on; workload reads only enrich CR data
// with live status. Dynamic may be nil for callers that do not need
// CR-aware listing (e.g. Manager.Add in tests).
type Manager struct {
	Client  kubernetes.Interface
	Dynamic dynamic.Interface
}

// catalog maps service types to their configuration.
var catalog = map[string]serviceSpec{
	"postgres": { //nolint:gosec // env var names, not actual credentials
		Image:          "postgres:16-alpine",
		Port:           5432,
		DefaultStorage: "5Gi",
		EnvVars: map[string]string{
			"POSTGRES_DB": "app",
		},
		PasswordEnvVar: "POSTGRES_PASSWORD",
		UserEnvVar:     "POSTGRES_USER",
		DefaultUser:    "kipper",
		URLFormat:      "postgres://%s:%s@%s:%d/%s",
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{
					Command: []string{"pg_isready", "-U", "kipper"},
				},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
		},
	},
	"redis": {
		Image:          "redis:7-alpine",
		Port:           6379,
		DefaultStorage: "1Gi",
		EnvVars:        map[string]string{},
		URLFormat:      "redis://%s:%d",
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{
					Command: []string{"redis-cli", "ping"},
				},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
		},
	},
	"mysql": { //nolint:gosec // env var names, not actual credentials
		Image:          "mysql:8-oracle",
		Port:           3306,
		DefaultStorage: "5Gi",
		EnvVars: map[string]string{
			"MYSQL_DATABASE": "app",
		},
		PasswordEnvVar: "MYSQL_ROOT_PASSWORD",
		UserEnvVar:     "MYSQL_USER",
		DefaultUser:    "kipper",
		URLFormat:      "mysql://%s:%s@%s:%d/%s",
		MountPath:      "/var/lib/mysql",
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{
					Command: []string{"mysqladmin", "ping", "-h", "localhost"},
				},
			},
			InitialDelaySeconds: 10,
			PeriodSeconds:       10,
		},
	},
	"mongodb": { //nolint:gosec // env var names, not actual credentials
		Image:          "mongo:7",
		Port:           27017,
		DefaultStorage: "5Gi",
		EnvVars:        map[string]string{},
		PasswordEnvVar: "MONGO_INITDB_ROOT_PASSWORD",
		UserEnvVar:     "MONGO_INITDB_ROOT_USERNAME",
		DefaultUser:    "kipper",
		URLFormat:      "mongodb://%s:%s@%s:%d/%s?authSource=admin",
		MountPath:      "/data/db",
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{
					Command: []string{"mongosh", "--eval", "db.adminCommand('ping')"},
				},
			},
			InitialDelaySeconds: 10,
			PeriodSeconds:       10,
		},
	},
	"rabbitmq": {
		Image:          "rabbitmq:3-management-alpine",
		Port:           5672,
		DefaultStorage: "1Gi",
		EnvVars:        map[string]string{},
		PasswordEnvVar: "RABBITMQ_DEFAULT_PASS",
		UserEnvVar:     "RABBITMQ_DEFAULT_USER",
		DefaultUser:    "kipper",
		URLFormat:      "amqp://%s:%s@%s:%d/",
		MountPath:      "/var/lib/rabbitmq",
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{
					Command: []string{"rabbitmq-diagnostics", "-q", "ping"},
				},
			},
			InitialDelaySeconds: 15,
			PeriodSeconds:       10,
		},
	},
	"opensearch": {
		Image:          "opensearchproject/opensearch:2",
		Port:           9200,
		DefaultStorage: "5Gi",
		EnvVars: map[string]string{
			"discovery.type":          "single-node",
			"DISABLE_SECURITY_PLUGIN": "true",
			"OPENSEARCH_JAVA_OPTS":    "-Xms256m -Xmx256m",
		},
		URLFormat: "http://%s:%d",
		MountPath: "/usr/share/opensearch/data",
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/_cluster/health",
					Port: intstr.FromInt32(9200),
				},
			},
			InitialDelaySeconds: 30,
			PeriodSeconds:       10,
		},
	},
	"minio": { //nolint:gosec // env var names, not actual credentials
		// Keep the default aligned with the server release built in images/minio.
		Image:          servicecatalog.MinIOImage(servicecatalog.MinIORelease),
		Port:           9000,
		DefaultStorage: "10Gi",
		EnvVars:        map[string]string{},
		PasswordEnvVar: "MINIO_ROOT_PASSWORD",
		UserEnvVar:     "MINIO_ROOT_USER",
		DefaultUser:    "kipper",
		URLFormat:      "http://%s:%d",
		Command:        []string{"minio"},
		Args:           []string{"server", "/data", "--console-address", ":9001"},
		MountPath:      "/data",
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/minio/health/ready",
					Port: intstr.FromInt32(9000),
				},
			},
			InitialDelaySeconds: 10,
			PeriodSeconds:       10,
		},
	},
	"mailhog": {
		// Upstream is archived; v1.0.1 is the last release
		// (verified on Docker Hub 2026-05-20). amd64 only —
		// ARM clusters need a community fork.
		//
		// TODO: this CLI catalog deploys raw StatefulSet/Service/
		// Secret directly, so the UI Ingress + forwardAuth
		// Middleware are *not* created here. To get the UI
		// exposed at mailhog-<ns>.<cluster-domain>, add MailHog
		// through the console (or via `kip apply`) so a Service
		// CR exists and the controller reconciles the UI
		// resources. Aligning the CLI with the CR path is a
		// separate cleanup.
		Image:          "mailhog/mailhog:v1.0.1",
		Port:           1025,
		DefaultStorage: "1Gi",
		EnvVars:        map[string]string{},
		URLFormat:      "smtp://%s:%d",
		MountPath:      "/maildir",
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{
					Port: intstr.FromInt32(1025),
				},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
		},
	},
}

type serviceSpec struct {
	Image          string
	Port           int32
	DefaultStorage string
	EnvVars        map[string]string
	PasswordEnvVar string
	UserEnvVar     string
	DefaultUser    string
	URLFormat      string
	ReadinessProbe *corev1.Probe
	Command        []string
	Args           []string
	MountPath      string
}

// SupportedTypes returns the list of available service types.
func SupportedTypes() []string {
	types := make([]string, 0, len(catalog))
	for t := range catalog {
		types = append(types, t)
	}
	return types
}

// IsSupported returns true if the given service type is in the catalog.
func IsSupported(serviceType string) bool {
	_, ok := catalog[serviceType]
	return ok
}

// Add deploys a stateful service.
func (m *Manager) Add(ctx context.Context, opts Options) (*ConnectionInfo, error) {
	spec, ok := catalog[opts.Type]
	if !ok {
		return nil, fmt.Errorf("unsupported service type %q (supported: %v)", opts.Type, SupportedTypes())
	}

	if opts.Storage == "" {
		opts.Storage = spec.DefaultStorage
	}

	// Override image version if specified (e.g. --version 15 → postgres:15-alpine)
	if opts.ImageVersion != "" {
		spec.Image = imageWithVersion(spec.Image, opts.ImageVersion)
	}

	// Generate credentials
	password, err := generatePassword(24)
	if err != nil {
		return nil, fmt.Errorf("generating password: %w", err)
	}

	username := spec.DefaultUser

	// Create credentials secret
	if err := m.createCredentialsSecret(ctx, opts, spec, username, password); err != nil {
		return nil, fmt.Errorf("creating credentials: %w", err)
	}

	// Create StatefulSet
	if err := m.createStatefulSet(ctx, opts, spec); err != nil {
		return nil, fmt.Errorf("creating statefulset: %w", err)
	}

	// Create Service
	if err := m.createService(ctx, opts, spec); err != nil {
		return nil, fmt.Errorf("creating service: %w", err)
	}

	svcHost := fmt.Sprintf("%s.%s.svc.cluster.local", opts.Name, opts.Namespace)

	conn := &ConnectionInfo{
		Host:     svcHost,
		Port:     spec.Port,
		Username: username,
		Password: password,
		Database: "app",
	}
	conn.URL = conn.formatURL(spec.URLFormat, opts.Type)
	return conn, nil
}

// Delete removes a stateful service. Requires --delete-data flag.
func (m *Manager) Delete(ctx context.Context, namespace, name string, deleteData bool) error {
	if !deleteData {
		return fmt.Errorf("refusing to delete service %q without --delete-data flag (this permanently destroys all data)", name)
	}

	// There is no CR on this path and so no owner reference to go on, which
	// leaves the management label as the only thing saying the workload is
	// Kipper's. A StatefulSet called db with a claim called data-db-0 is what
	// anybody's database looks like, and Kipper's rule is that a resource
	// without the label is not its to touch.
	workload, err := m.workloadIsKippers(ctx, namespace, name)
	if err != nil {
		return err
	}

	// Read before deleting, because the name is free the moment the workload
	// goes and what gets destroyed has to be the volumes of the service that was
	// asked about.
	volumes, err := m.DataVolumes(ctx, namespace, name)
	if err != nil {
		return err
	}

	// Everything is read before anything is deleted. Checking as it went would
	// leave a service half removed the moment one object turned out to be
	// somebody else's, and every rerun would stop in the same place, so the
	// volume that made the operator run this could never be cleared.
	c := m.Client
	address, err := m.ourObject("address", namespace, name, func() (metav1.Object, error) {
		return c.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
	})
	if err != nil {
		return err
	}
	credentials := secretname.ServiceCredentials(name)
	secret, err := m.ourObject("credentials", namespace, credentials, func() (metav1.Object, error) {
		return c.CoreV1().Secrets(namespace).Get(ctx, credentials, metav1.GetOptions{})
	})
	if err != nil {
		return err
	}

	// Pinned to the workload whose provenance was read, because the name is free
	// the moment it goes and what stands there next may be nobody's business of
	// this command's.
	if workload != nil {
		uid := workload.GetUID()
		err := c.AppsV1().StatefulSets(namespace).Delete(ctx, name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &uid},
		})
		if err != nil && !errors.IsNotFound(err) && !errors.IsConflict(err) {
			return fmt.Errorf("deleting the workload of %s: %w", name, err)
		}
	}
	if address != nil {
		uid := address.GetUID()
		err := c.CoreV1().Services(namespace).Delete(ctx, name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &uid},
		})
		if err != nil && !errors.IsNotFound(err) && !errors.IsConflict(err) {
			return fmt.Errorf("deleting the cluster address of %s: %w", name, err)
		}
	}
	if secret != nil {
		uid := secret.GetUID()
		err := c.CoreV1().Secrets(namespace).Delete(ctx, credentials, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &uid},
		})
		if err != nil && !errors.IsNotFound(err) && !errors.IsConflict(err) {
			return fmt.Errorf("deleting %s: %w", credentials, err)
		}
	}

	// Through the same destroy the CR path uses, so both agree on which volumes
	// belong to a service, and neither deletes a claim while the StatefulSet is
	// still there to write it back.
	return m.DestroyVolumes(ctx, namespace, name, volumes)
}

// ourObject reads one of the objects a service's name derives and refuses it if
// it is not Kipper's. Nothing there is not a refusal, and comes back nil.
func (m *Manager) ourObject(kind, namespace, name string, read func() (metav1.Object, error)) (metav1.Object, error) {
	object, err := read()
	if errors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the %s of %s: %w", kind, name, err)
	}
	if object.GetLabels()[labels.ManagedBy] != labels.Kipper {
		return nil, fmt.Errorf("the %s named %s in %s is not Kipper's", kind, name, namespace)
	}
	// An owner means something else is keeping this object, and this path clears
	// what a deleted service left behind. Whether it has stood there all along or
	// appeared while this was reading, it is not the leftover.
	if owner := metav1.GetControllerOf(object); owner != nil {
		return nil, fmt.Errorf("the %s named %s in %s belongs to %s %s, so it is not the leftover of a deleted service",
			kind, name, namespace, owner.Kind, owner.Name)
	}
	return object, nil
}

// workloadIsKippers refuses a workload that carries no sign of being Kipper's,
// and returns the one it read so the delete can be pinned to it.
//
// Nothing to refuse is not a refusal: a service whose workload has already gone
// leaves only its volume, and that is the state this path exists to clear.
func (m *Manager) workloadIsKippers(ctx context.Context, namespace, name string) (metav1.Object, error) {
	workload, err := m.Client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the workload of %s: %w", name, err)
	}
	if workload.Labels[labels.ManagedBy] != labels.Kipper {
		return nil, fmt.Errorf("the workload named %s in %s is not Kipper's, so the data under that name is not this service's to destroy", name, namespace)
	}
	// A workload with a controller belongs to a service record, and this path
	// clears what a service that has gone left behind. Whether the record has
	// stood there all along or appeared while this was reading, this workload is
	// not the leftover.
	if owner := metav1.GetControllerOf(workload); owner != nil {
		return nil, fmt.Errorf("the workload named %s in %s belongs to %s %s, so it is not the leftover of a deleted service", name, namespace, owner.Kind, owner.Name)
	}
	return workload, nil
}

// UpdateResult describes what changed during an update. Version is the resolved
// image tag when a version was requested, even if ImageChanged is false. It is
// empty when no version was requested.
type UpdateResult struct {
	StorageExpanded  bool
	StorageDeclared  bool
	ResourcesChanged bool
	ImageChanged     bool
	NeedsRestart     bool
	Version          string
}

// Update modifies a running service's resources, storage, or image version.
// Storage changes update the declared size and expand claims other than local-path
// claims without restarting the pod. StorageDeclared reports local-path claims,
// whose requested size is left unchanged. Resource and image changes are saved to
// the Service CR for its reconciler to apply and restart the pod.
// Numeric versions preserve the running image variant. Other tags are used as
// given. Version changes must pass servicecatalog.CheckNewVersion and
// servicecatalog.CheckVersionChange. Options.MajorUpgrade confirms higher major
// versions or unparseable tags under that policy.
// Invalid storage requests fail before any writes. Later failures may leave partial changes.
func (m *Manager) Update(ctx context.Context, namespace, name string, opts Options) (*UpdateResult, error) {
	result := &UpdateResult{}

	// Validate storage first so an invalid request leaves the whole update unapplied.
	var storage *storagePlan
	if opts.Storage != "" {
		plan, err := m.planStorage(ctx, namespace, name, opts.Storage)
		if err != nil {
			return nil, err
		}
		storage = plan
	}

	ss, err := m.Client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, fmt.Errorf("service %q not found", name)
		}
		return nil, fmt.Errorf("getting statefulset: %w", err)
	}

	if len(ss.Spec.Template.Spec.Containers) == 0 {
		return nil, fmt.Errorf("service %q has no containers", name)
	}

	container := &ss.Spec.Template.Spec.Containers[0]

	if opts.ImageVersion != "" {
		if err := servicecatalog.CheckNewVersion(ss.Labels[labels.ServiceType], opts.ImageVersion); err != nil {
			return nil, err
		}
		running := imageTag(container.Image)
		tag := opts.ImageVersion
		if bareVersion.MatchString(tag) {
			// Preserve the running image's variant, such as -alpine, to keep the same base distribution.
			tag = imageTag(imageWithVersion(container.Image, tag))
		}
		if err := servicecatalog.CheckVersionChange(ss.Labels[labels.ServiceType], running, tag, opts.MajorUpgrade); err != nil {
			return nil, err
		}
		changed, err := m.setServiceVersion(ctx, namespace, name, tag, running)
		if err != nil {
			return nil, err
		}
		result.ImageChanged = changed
		result.NeedsRestart = changed
		result.Version = tag
	}

	// Save resource bounds on the Service CR for its reconciler to apply.
	if opts.MemoryLimit != "" || opts.CPULimit != "" {
		if err := m.setServiceResources(ctx, namespace, name, opts.MemoryLimit, opts.CPULimit); err != nil {
			return nil, err
		}
		result.ResourcesChanged = true
		result.NeedsRestart = true
	}

	if storage != nil {
		if err := m.applyStorage(ctx, namespace, name, storage, result); err != nil {
			return nil, err
		}
	}

	return result, nil
}

// Snapshot holds a service's type, credential or name blockage, and TLS state
// from one CR read, so callers make decisions from a consistent snapshot.
// TLSServing requires a Serving condition for the current generation.
// TLSWaiting contains the condition message when TLSReady is false.
type Snapshot struct {
	Type           string
	BlockedReason  string
	BlockedMessage string
	TLS            string
	TLSServing     bool
	TLSWaiting     string
	Copies         Copies
	DumpFault      string
}

// Blocked reports whether the reconciler has refused this service anything.
func (s Snapshot) Blocked() bool { return s.BlockedReason != "" }

// Read returns the service type, blockage and TLS state from one Service CR
// read. Credential blockage takes precedence over name blockage. It returns
// an error if the dynamic client is missing or the CR cannot be read.
func (m *Manager) Read(ctx context.Context, namespace, name string) (Snapshot, error) {
	if m.Dynamic == nil {
		return Snapshot{}, fmt.Errorf("service manager is not configured with a dynamic client")
	}
	cr, err := m.Dynamic.Resource(manifest.ServiceGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return Snapshot{}, fmt.Errorf("service %q not found", name)
		}
		return Snapshot{}, fmt.Errorf("getting service CR: %w", err)
	}
	svcType, _, _ := unstructured.NestedString(cr.Object, "spec", "type")
	reason, message := blockage(cr.Object, servicecatalog.ConditionCredentialsReady)
	if reason == "" {
		reason, message = blockage(cr.Object, servicecatalog.ConditionNameFree)
	}
	tls, _, _ := unstructured.NestedString(cr.Object, "spec", "tls")
	_, waiting := blockage(cr.Object, servicecatalog.ConditionTLSReady)
	_, dumpFault := blockage(cr.Object, servicecatalog.ConditionDumpVolumeReady)
	return Snapshot{Type: svcType, BlockedReason: reason, BlockedMessage: message,
		TLS: tls, TLSServing: ServesTLS(cr), TLSWaiting: waiting, Copies: CopiesOf(cr.Object), DumpFault: dumpFault}, nil
}

// credentialsBlockage reads the reason and remedy off a service that the
// reconciler has refused, and answers empty for one it has not.
//
// Everything here is read defensively. The condition is absent on every cluster
// older than it, absent on a healthy service, and the object comes off the wire
// as whatever the API server holds, so a status that is not a map or a
// conditions entry that is not one has to answer "nothing wrong" rather than
// stop the listing.
func blockage(obj map[string]interface{}, wanted string) (string, string) {
	conditions, found, err := unstructured.NestedSlice(obj, "status", "conditions")
	if err != nil || !found {
		return "", ""
	}
	for _, entry := range conditions {
		condition, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := condition["type"].(string)
		if name != wanted {
			continue
		}
		// Only a condition that is false is a blockage: the reconciler takes the
		// condition off entirely once the cause clears, so a true one says
		// nothing to act on.
		//
		// Every entry is read rather than the first, because this CRD puts no
		// uniqueness on the type. A restore or an edit can leave two, and
		// stopping at a stale true one would hide a live refusal. A warning is
		// the safe thing to get wrong in the direction of showing it.
		if status, _ := condition["status"].(string); status != "False" {
			continue
		}
		reason, _ := condition["reason"].(string)
		message, _ := condition["message"].(string)
		return reason, message
	}
	return "", ""
}

// ServesTLS reports whether the controller has confirmed that a service's pod
// serves TLS in the mode requested by its current spec. It requires a true
// TLSReady condition with reason Serving for the current generation and returns
// false when that confirmation is missing or stale.
func ServesTLS(cr *unstructured.Unstructured) bool {
	conditions, _, _ := unstructured.NestedSlice(cr.Object, "status", "conditions")
	for _, entry := range conditions {
		condition, ok := entry.(map[string]interface{})
		if !ok || condition["type"] != servicecatalog.ConditionTLSReady {
			continue
		}
		observed, _, _ := unstructured.NestedInt64(condition, "observedGeneration")
		return condition["status"] == "True" && condition["reason"] == "Serving" && observed == cr.GetGeneration()
	}
	return false
}

// List reads Service CRs and enriches them with live workload status.
// Services awaiting their first StatefulSet remain visible.
func (m *Manager) List(ctx context.Context, namespace string) ([]Status, error) {
	if m.Dynamic == nil {
		return nil, fmt.Errorf("service manager is not configured with a dynamic client")
	}

	crList, err := m.Dynamic.Resource(manifest.ServiceGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing service CRs: %w", err)
	}

	crashLoops := CrashLoopingServices(ctx, m.Client, namespace)
	unschedulable := UnschedulableServices(ctx, m.Client, namespace)

	var services []Status
	for _, cr := range crList.Items {
		name := cr.GetName()
		svcType, _, _ := unstructured.NestedString(cr.Object, "spec", "type")
		storage, _, _ := unstructured.NestedString(cr.Object, "spec", "storage")
		phase, _, _ := unstructured.NestedString(cr.Object, "status", "phase")
		if phase == "" {
			phase = "pending"
		} else {
			phase = strings.ToLower(phase)
		}
		// A service keeps the phase it had until it goes, and one whose cleanup
		// has stopped keeps it for good. Left as running it reads as a delete
		// that did nothing.
		reason, message := blockage(cr.Object, servicecatalog.ConditionCredentialsReady)
		if reason == "" {
			reason, message = blockage(cr.Object, servicecatalog.ConditionNameFree)
		}
		if cr.GetDeletionTimestamp() != nil {
			phase = "deleting"
			if stuck, why := blockage(cr.Object, servicecatalog.ConditionCleanupComplete); stuck != "" {
				reason, message = stuck, why
			}
		}

		ready := "0/0"
		if ss, ssErr := m.Client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{}); ssErr == nil {
			replicas := int32(0)
			if ss.Spec.Replicas != nil {
				replicas = *ss.Spec.Replicas
			}
			ready = fmt.Sprintf("%d/%d", ss.Status.ReadyReplicas, replicas)
			if storage == "" && len(ss.Spec.VolumeClaimTemplates) > 0 {
				storage = ss.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests.Storage().String()
			}
		}

		// A crash loop is invisible in the fields above: phase comes from the
		// CR, which is fine, and readiness says 0/1, which a service that is
		// merely starting also says. One service read "running 0/1" for three
		// and a half days while its container died 996 times.
		if why, looping := crashLoops[name]; looping && phase != "deleting" && reason == "" {
			phase = "crash-looping"
			reason, message = "CrashLoopBackOff", why
		}
		// Readiness alone cannot distinguish scheduling failures from startup.
		if why, stuck := unschedulable[name]; stuck && phase != "deleting" && reason == "" {
			reason, message = "FailedScheduling", why
		}

		services = append(services, Status{
			Name:           name,
			Type:           svcType,
			Status:         phase,
			Storage:        storage,
			Ready:          ready,
			BlockedReason:  reason,
			BlockedMessage: message,
		})
	}

	return services, nil
}

// Info returns connection details for a service. The Service CR is the
// authoritative source for the service type; the credentials Secret carries
// the connection-string components.
func (m *Manager) Info(ctx context.Context, namespace, name string) (*ConnectionInfo, error) {
	svcType := ""
	if m.Dynamic != nil {
		snapshot, err := m.Read(ctx, namespace, name)
		if err != nil {
			return nil, err
		}
		svcType = snapshot.Type
	} else if ss, ssErr := m.Client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{}); ssErr == nil {
		svcType = ss.Labels[labels.ServiceType]
	}

	return m.Connection(ctx, namespace, name, svcType)
}

// Connection reads the credentials Secret and shapes what a client needs to
// reach the service. The type comes from the caller, so a caller that has
// already read the service does not read it again and cannot disagree with
// itself about what it found.
func (m *Manager) Connection(ctx context.Context, namespace, name, svcType string) (*ConnectionInfo, error) {
	secret, err := m.Client.CoreV1().Secrets(namespace).Get(ctx, secretname.ServiceCredentials(name), metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, fmt.Errorf("credentials for service %q not found", name)
		}
		return nil, fmt.Errorf("getting credentials: %w", err)
	}

	conn := &ConnectionInfo{
		Host:     string(secret.Data["HOST"]),
		Port:     5432,
		Username: string(secret.Data["USERNAME"]),
		Password: string(secret.Data["PASSWORD"]),
		Database: string(secret.Data["NAME"]),
	}
	conn.Type = svcType
	if svcType == "minio" {
		// S3 credentials: host comes out of the endpoint URL, and the
		// access/secret key stand in for username/password.
		if u, err := url.Parse(string(secret.Data["ENDPOINT"])); err == nil {
			conn.Host = u.Hostname()
		}
		conn.Username = string(secret.Data["ACCESS_KEY"])
		conn.Password = string(secret.Data["SECRET_KEY"])
	}
	if spec, ok := catalog[svcType]; ok {
		conn.Port = spec.Port
		conn.URL = conn.formatURL(spec.URLFormat, svcType)
	}
	return conn, nil
}

// SecretMask stands in for a password or secret key that is not shown.
const SecretMask = "********"

// Masked returns a copy of c with a non-empty Password replaced by SecretMask.
// For known service types, it rebuilds the URL with the masked password where
// the URL format includes credentials. For unknown types, the URL is unchanged
// and may still contain credentials. An empty Password leaves the copy unchanged.
func (c ConnectionInfo) Masked() ConnectionInfo {
	if c.Password == "" {
		return c
	}
	c.Password = SecretMask
	if spec, ok := catalog[c.Type]; ok {
		c.URL = c.formatURL(spec.URLFormat, c.Type)
	}
	return c
}

// formatURL builds a display URL from the connection components and the
// catalog's URLFormat for CLI display and internal use. Kubernetes Secrets
// store the components so frameworks can use their own URL schemes, such as
// postgres:// or jdbc:postgresql://. Unknown service types return an empty string.
func (c *ConnectionInfo) formatURL(urlFormat, svcType string) string {
	switch svcType {
	case "postgres", "mysql":
		return fmt.Sprintf(urlFormat, c.Username, c.Password, c.Host, c.Port, c.Database)
	case "mongodb":
		return fmt.Sprintf(urlFormat, c.Username, c.Password, c.Host, c.Port, c.Database)
	case "rabbitmq":
		return fmt.Sprintf(urlFormat, c.Username, c.Password, c.Host, c.Port)
	case "redis", "opensearch", "mailhog":
		return fmt.Sprintf(urlFormat, c.Host, c.Port)
	case "minio":
		// S3 clients use a plain endpoint URL and separate credentials.
		return fmt.Sprintf(urlFormat, c.Host, c.Port)
	default:
		return ""
	}
}

func (m *Manager) createCredentialsSecret(ctx context.Context, opts Options, spec serviceSpec, username, password string) error {
	svcHost := fmt.Sprintf("%s.%s.svc.cluster.local", opts.Name, opts.Namespace)

	data := map[string][]byte{
		"HOST": []byte(svcHost),
		"PORT": []byte(fmt.Sprintf("%d", spec.Port)),
		"NAME": []byte("app"),
	}
	// A server started without authentication gets no credentials, so a
	// bound workload is not handed a password the server will refuse. The
	// console reconciler applies the same rule from its own catalog.
	if servicecatalog.HasAuth(opts.Type) {
		data["USERNAME"] = []byte(username)
		data["PASSWORD"] = []byte(password)
	}

	switch opts.Type {
	case "rabbitmq":
		data["management"] = []byte(fmt.Sprintf("http://%s:15672", svcHost))
		delete(data, "NAME")
	case "redis", "opensearch", "mailhog":
		delete(data, "NAME")
	case "minio":
		// S3 service: replace the generic host/port/user/pass baseline
		// with the endpoint URL + access key / secret key that S3
		// clients read. Matches the console reconciler's shape so a
		// CLI-created and a console-created MinIO bind identically.
		data = map[string][]byte{
			"ENDPOINT":   []byte(fmt.Sprintf("http://%s:%d", svcHost, spec.Port)),
			"ACCESS_KEY": []byte(username),
			"SECRET_KEY": []byte(password),
		}
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretname.ServiceCredentials(opts.Name),
			Namespace: opts.Namespace,
			Labels:    serviceLabels(opts.Name, opts.Type),
		},
		Data: data,
	}

	_, err := m.Client.CoreV1().Secrets(opts.Namespace).Create(ctx, secret, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (m *Manager) createStatefulSet(ctx context.Context, opts Options, spec serviceSpec) error {
	replicas := int32(1)

	envVars := make([]corev1.EnvVar, 0)
	for k, v := range spec.EnvVars {
		envVars = append(envVars, corev1.EnvVar{Name: k, Value: v})
	}

	// MinIO is S3: its root credentials live under the access-key /
	// secret-key names, not the generic user/pass keys.
	passwordKey, userKey := "PASSWORD", "USERNAME"
	if opts.Type == "minio" {
		passwordKey, userKey = "SECRET_KEY", "ACCESS_KEY"
	}

	if spec.PasswordEnvVar != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name: spec.PasswordEnvVar,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretname.ServiceCredentials(opts.Name)},
					Key:                  passwordKey,
				},
			},
		})
	}

	if spec.UserEnvVar != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name: spec.UserEnvVar,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretname.ServiceCredentials(opts.Name)},
					Key:                  userKey,
				},
			},
		})
	}

	// MySQL needs MYSQL_PASSWORD in addition to MYSQL_ROOT_PASSWORD
	if opts.Type == "mysql" {
		envVars = append(envVars, corev1.EnvVar{
			Name: "MYSQL_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretname.ServiceCredentials(opts.Name)},
					Key:                  "PASSWORD",
				},
			},
		})
	}

	// Determine volume mount based on service type
	mountPath := spec.MountPath
	subPath := ""
	if opts.Type == "postgres" {
		mountPath = "/var/lib/postgresql/data"
		subPath = "pgdata"
	} else if mountPath == "" {
		mountPath = "/data"
	}

	container := corev1.Container{
		Name:  opts.Type,
		Image: spec.Image,
		Ports: []corev1.ContainerPort{
			{ContainerPort: spec.Port, Name: opts.Type},
		},
		Env:     envVars,
		Command: spec.Command,
		Args:    spec.Args,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: mountPath, SubPath: subPath},
		},
		ReadinessProbe: spec.ReadinessProbe,
	}

	// Apply resource limits if specified
	if opts.MemoryLimit != "" || opts.CPULimit != "" {
		limits := corev1.ResourceList{}
		requests := corev1.ResourceList{}
		if opts.MemoryLimit != "" {
			limits[corev1.ResourceMemory] = resource.MustParse(opts.MemoryLimit)
			requests[corev1.ResourceMemory] = resource.MustParse(opts.MemoryLimit)
		}
		if opts.CPULimit != "" {
			limits[corev1.ResourceCPU] = resource.MustParse(opts.CPULimit)
			requests[corev1.ResourceCPU] = resource.MustParse(opts.CPULimit)
		}
		container.Resources = corev1.ResourceRequirements{
			Limits:   limits,
			Requests: requests,
		}
	}

	// Additional ports for services with management UIs
	if opts.Type == "minio" {
		container.Ports = append(container.Ports, corev1.ContainerPort{
			ContainerPort: 9001, Name: "console",
		})
	}
	if opts.Type == "rabbitmq" {
		container.Ports = append(container.Ports, corev1.ContainerPort{
			ContainerPort: 15672, Name: "management",
		})
	}

	ss := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.Name,
			Namespace: opts.Namespace,
			Labels:    serviceLabels(opts.Name, opts.Type),
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: opts.Name,
			Replicas:    &replicas,
			Selector:    &metav1.LabelSelector{MatchLabels: map[string]string{"app": opts.Name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: serviceLabels(opts.Name, opts.Type)},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{container},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "data",
						Labels: map[string]string{"app": opts.Name},
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						StorageClassName: strPtr("longhorn-single"),
						AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse(opts.Storage),
							},
						},
					},
				},
			},
		},
	}

	_, err := m.Client.AppsV1().StatefulSets(opts.Namespace).Create(ctx, ss, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (m *Manager) createService(ctx context.Context, opts Options, spec serviceSpec) error {
	ports := []corev1.ServicePort{
		{Port: spec.Port, TargetPort: intstr.FromInt32(spec.Port), Name: opts.Type},
	}
	if opts.Type == "minio" {
		ports = append(ports, corev1.ServicePort{
			Port: 9001, TargetPort: intstr.FromInt32(9001), Name: "console",
		})
	}
	if opts.Type == "rabbitmq" {
		ports = append(ports, corev1.ServicePort{
			Port: 15672, TargetPort: intstr.FromInt32(15672), Name: "management",
		})
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.Name,
			Namespace: opts.Namespace,
			Labels:    serviceLabels(opts.Name, opts.Type),
		},
		Spec: corev1.ServiceSpec{
			Selector:  map[string]string{"app": opts.Name},
			Ports:     ports,
			ClusterIP: "None",
		},
	}

	_, err := m.Client.CoreV1().Services(opts.Namespace).Create(ctx, svc, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func serviceLabels(name, serviceType string) map[string]string {
	return map[string]string{
		"app":              name,
		labels.ManagedBy:   labels.Kipper,
		labels.ServiceType: serviceType,
	}
}

func strPtr(s string) *string { return &s }

// bareVersion matches a version without a variant, such as 16 or 8.0.
var bareVersion = regexp.MustCompile(`^\d+(\.\d+)*$`)

// versionLedTag identifies numeric version prefixes so the remaining suffix can
// be preserved as an image variant.
var versionLedTag = regexp.MustCompile(`^\d+(\.\d+)*-`)

// imageWithVersion applies a user's --version to an image, preserving its variant.
//
// Most catalog entries include a variant, so --version 15 changes postgres:16-alpine
// to postgres:15-alpine to retain the Alpine image. Only tags starting with a
// numeric version have their variant preserved. MinIO release tags such as
// RELEASE.2025-09-07T16-13-09Z contain hyphens in the timestamp, so splitting at
// the first hyphen would append part of the old date to the new tag.
func imageWithVersion(image, version string) string {
	base, tag, hasTag := strings.Cut(image, ":")
	if !hasTag || !versionLedTag.MatchString(tag) {
		return base + ":" + version
	}
	_, variant, _ := strings.Cut(tag, "-")
	return base + ":" + version + "-" + variant
}

func generatePassword(length int) (string, error) {
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b)[:length], nil
}

// storagePlan holds a validated size and the claims to update. Local-path claims
// are excluded because their provisioner does not support expansion.
type storagePlan struct {
	size  resource.Quantity
	grow  []*corev1.PersistentVolumeClaim
	local bool
}

// planStorage validates a positive size for non-local-path claims without saving
// changes. An unfinished expansion can be retried with a smaller request above
// the reported capacity. Server-side dry runs check each planned update before
// any writes, so admission errors such as Longhorn disk capacity limits surface
// early. If a webhook rejects dry runs as unsupported, remaining checks are skipped.
func (m *Manager) planStorage(ctx context.Context, namespace, name, storage string) (*storagePlan, error) {
	size, err := resource.ParseQuantity(storage)
	if err != nil || size.Sign() <= 0 {
		return nil, fmt.Errorf("invalid --storage %q: use a positive size such as 10Gi", storage)
	}
	pvcs, err := m.Client.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", name),
	})
	if err != nil {
		return nil, fmt.Errorf("reading the volumes of %s: %w", name, err)
	}
	plan := &storagePlan{size: size}
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		if class := pvc.Spec.StorageClassName; class != nil && *class == platform.LocalPathClass {
			plan.local = true
			continue
		}
		if err := allowedSize(pvc, size); err != nil {
			return nil, err
		}
		plan.grow = append(plan.grow, pvc)
	}
	for _, pvc := range plan.grow {
		trial := pvc.DeepCopy()
		trial.Spec.Resources.Requests[corev1.ResourceStorage] = size
		_, err := m.Client.CoreV1().PersistentVolumeClaims(namespace).Update(ctx, trial, metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}})
		switch {
		case errors.IsBadRequest(err) && strings.Contains(err.Error(), "does not support dry run"):
			return plan, nil
		case errors.IsForbidden(err):
			return nil, fmt.Errorf("volume %s cannot grow to %s: %w%s", pvc.Name, size.String(), err, longhornRoomHint(err))
		case err != nil:
			return nil, fmt.Errorf("checking whether volume %s can grow to %s: %w", pvc.Name, size.String(), err)
		}
	}
	return plan, nil
}

// allowedSize accepts sizes at or above the current request. A smaller request
// is allowed during an unfinished expansion if it exceeds the reported capacity.
func allowedSize(pvc *corev1.PersistentVolumeClaim, size resource.Quantity) error {
	current := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if size.Cmp(current) >= 0 {
		return nil
	}
	capacity, known := pvc.Status.Capacity[corev1.ResourceStorage]
	switch {
	case !known || capacity.Cmp(current) >= 0:
		return fmt.Errorf("volume %s is %s and can only grow. Use at least %s", pvc.Name, current.String(), current.String())
	case size.Cmp(capacity) <= 0:
		return fmt.Errorf("volume %s is %s and its expansion to %s has not finished. Use more than %s", pvc.Name, capacity.String(), current.String(), capacity.String())
	}
	return nil
}

// longhornRoomHint suggests making room for expansion because Longhorn can reject
// growth beyond disk capacity. It matches the exact admission webhook name
// "validator.longhorn.io" in the error text and returns an empty string otherwise.
func longhornRoomHint(err error) string {
	if !strings.Contains(err.Error(), `admission webhook "validator.longhorn.io"`) {
		return ""
	}
	return ". Make room on the Longhorn disks first, for example with 'kip cluster storage --over-provisioning <percent>'"
}

// applyStorage updates the planned claims, then records the declared size on the
// Service CR. If a claim update fails, earlier writes remain applied and the error
// names the claims whose size requests were saved. The dry runs in planStorage
// check each claim separately, so claims sharing a disk can still be refused here.
func (m *Manager) applyStorage(ctx context.Context, namespace, name string, plan *storagePlan, result *UpdateResult) error {
	var raised []string
	for _, pvc := range plan.grow {
		pvc.Spec.Resources.Requests[corev1.ResourceStorage] = plan.size
		if _, err := m.Client.CoreV1().PersistentVolumeClaims(namespace).Update(ctx, pvc, metav1.UpdateOptions{}); err != nil {
			partial := ""
			if len(raised) > 0 {
				partial = fmt.Sprintf(". %s, and the service's declared size is unchanged. Run the command again once there is room", strings.Join(raised, ", "))
			}
			return fmt.Errorf("volume %s cannot grow to %s: %w%s%s", pvc.Name, plan.size.String(), err, longhornRoomHint(err), partial)
		}
		raised = append(raised, fmt.Sprintf("%s already requests %s", pvc.Name, plan.size.String()))
		result.StorageExpanded = true
	}
	result.StorageDeclared = plan.local
	if err := m.setServiceStorage(ctx, namespace, name, plan.size.String()); err != nil {
		return fmt.Errorf("recording the declared size (run the command again): %w", err)
	}
	return nil
}

// setServiceStorage records the declared volume size, retrying update conflicts.
func (m *Manager) setServiceStorage(ctx context.Context, namespace, name, size string) error {
	services := m.Dynamic.Resource(manifest.ServiceGVR).Namespace(namespace)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		svc, err := services.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("service %q not found", name)
			}
			return fmt.Errorf("getting service: %w", err)
		}
		if err := unstructured.SetNestedField(svc.Object, size, "spec", "storage"); err != nil {
			return fmt.Errorf("setting storage: %w", err)
		}
		_, err = services.Update(ctx, svc, metav1.UpdateOptions{})
		return err
	})
}

// setServiceVersion records the image tag on the Service CR for its reconciler
// to apply to the StatefulSet, retrying update conflicts. It reports whether the
// CR changed. An empty CR version is treated as running for this comparison.
func (m *Manager) setServiceVersion(ctx context.Context, namespace, name, version, running string) (bool, error) {
	services := m.Dynamic.Resource(manifest.ServiceGVR).Namespace(namespace)
	changed := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		svc, err := services.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("service %q not found", name)
			}
			return fmt.Errorf("getting service: %w", err)
		}
		// Compare with the CR so a request for the running version can cancel
		// a pending change before the reconciler applies it.
		current, _, _ := unstructured.NestedString(svc.Object, "spec", "version")
		if current == "" {
			current = running
		}
		if current == version {
			changed = false
			return nil
		}
		if err := unstructured.SetNestedField(svc.Object, version, "spec", "version"); err != nil {
			return fmt.Errorf("setting version: %w", err)
		}
		if _, err := services.Update(ctx, svc, metav1.UpdateOptions{}); err != nil {
			return err
		}
		changed = true
		return nil
	})
	return changed, err
}

// imageTag returns the suffix after a colon in the final image path segment,
// or "" if there is no colon. Callers must supply tag references rather than digests.
func imageTag(image string) string {
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		return image[i+1:]
	}
	return ""
}

// setServiceResources records user-specified memory and CPU sizes on the Service
// CR, setting each supplied size as both its request and limit.
func (m *Manager) setServiceResources(ctx context.Context, namespace, name, memory, cpu string) error {
	services := m.Dynamic.Resource(manifest.ServiceGVR).Namespace(namespace)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		svc, err := services.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("service %q not found", name)
			}
			return fmt.Errorf("getting service: %w", err)
		}
		resources, _, _ := unstructured.NestedMap(svc.Object, "spec", "resources")
		if resources == nil {
			resources = map[string]interface{}{}
		}
		if memory != "" {
			resources["memoryRequest"], resources["memoryLimit"] = memory, memory
		}
		if cpu != "" {
			resources["cpuRequest"], resources["cpuLimit"] = cpu, cpu
		}
		if err := unstructured.SetNestedMap(svc.Object, resources, "spec", "resources"); err != nil {
			return fmt.Errorf("setting resources: %w", err)
		}
		_, err = services.Update(ctx, svc, metav1.UpdateOptions{})
		return err
	})
}
