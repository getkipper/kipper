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

	"github.com/getkipper/kipper/controller/pkg/labels"
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
}

// Status holds summary info for a running service.
type Status struct {
	Name    string
	Type    string
	Status  string
	Storage string
	Ready   string
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
		// MinIO tags a release rather than a version line, so there is no
		// patch-floating tag to sit on the way postgres:16-alpine does.
		// Upstream has published nothing since this release.
		Image:          "minio/minio:RELEASE.2025-09-07T16-13-09Z",
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

	c := m.Client
	_ = c.AppsV1().StatefulSets(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	_ = c.CoreV1().Services(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	_ = c.CoreV1().Secrets(namespace).Delete(ctx, secretname.ServiceCredentials(name), metav1.DeleteOptions{})

	// Delete PVCs created by the StatefulSet
	pvcs, err := c.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", name),
	})
	if err == nil {
		for _, pvc := range pvcs.Items {
			_ = c.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, pvc.Name, metav1.DeleteOptions{})
		}
	}

	return nil
}

// UpdateResult describes what changed during an update.
type UpdateResult struct {
	StorageExpanded  bool
	ResourcesChanged bool
	ImageChanged     bool
	NeedsRestart     bool
}

// Update modifies a running service's resources, storage, or image version.
// Storage expansion is live (no restart). Resource and image changes restart the pod.
func (m *Manager) Update(ctx context.Context, namespace, name string, opts Options) (*UpdateResult, error) {
	result := &UpdateResult{}

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

	// Update image version
	if opts.ImageVersion != "" {
		serviceType := ss.Labels[labels.ServiceType]
		spec, ok := catalog[serviceType]
		if ok {
			newImage := imageWithVersion(spec.Image, opts.ImageVersion)
			if container.Image != newImage {
				container.Image = newImage
				result.ImageChanged = true
				result.NeedsRestart = true
			}
		}
	}

	// Update resource limits
	if opts.MemoryLimit != "" || opts.CPULimit != "" {
		limits := container.Resources.Limits
		requests := container.Resources.Requests
		if limits == nil {
			limits = corev1.ResourceList{}
		}
		if requests == nil {
			requests = corev1.ResourceList{}
		}
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
		result.ResourcesChanged = true
		result.NeedsRestart = true
	}

	// Apply StatefulSet changes (image + resources)
	if result.ImageChanged || result.ResourcesChanged {
		if _, err := m.Client.AppsV1().StatefulSets(namespace).Update(ctx, ss, metav1.UpdateOptions{}); err != nil {
			return nil, fmt.Errorf("updating statefulset: %w", err)
		}
	}

	// Expand storage (PVC patch — live, no restart needed)
	if opts.Storage != "" {
		pvcs, err := m.Client.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app=%s", name),
		})
		if err == nil {
			for i := range pvcs.Items {
				pvcs.Items[i].Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse(opts.Storage)
				if _, err := m.Client.CoreV1().PersistentVolumeClaims(namespace).Update(ctx, &pvcs.Items[i], metav1.UpdateOptions{}); err != nil {
					return nil, fmt.Errorf("expanding storage: %w", err)
				}
				result.StorageExpanded = true
			}
		}
	}

	return result, nil
}

// List returns all stateful services in a namespace by reading Service CRs.
//
// The CR is the source of truth for what services exist; the StatefulSet is
// only consulted to enrich each entry with live workload status (READY
// count, storage). A CR with no StatefulSet yet (controller has not
// reconciled) still appears in the list with phase from the CR.
func (m *Manager) List(ctx context.Context, namespace string) ([]Status, error) {
	if m.Dynamic == nil {
		return nil, fmt.Errorf("service manager is not configured with a dynamic client")
	}

	crList, err := m.Dynamic.Resource(manifest.ServiceGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing service CRs: %w", err)
	}

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

		services = append(services, Status{
			Name:    name,
			Type:    svcType,
			Status:  phase,
			Storage: storage,
			Ready:   ready,
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
		cr, err := m.Dynamic.Resource(manifest.ServiceGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				return nil, fmt.Errorf("service %q not found", name)
			}
			return nil, fmt.Errorf("getting service CR: %w", err)
		}
		svcType, _, _ = unstructured.NestedString(cr.Object, "spec", "type")
	} else if ss, ssErr := m.Client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{}); ssErr == nil {
		svcType = ss.Labels[labels.ServiceType]
	}

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

// formatURL builds a display URL from the connection components and the
// catalog's URLFormat. The URL is for CLI display and internal use only —
// it is not stored in Kubernetes secrets because different frameworks need
// different URL schemes (e.g. postgres:// vs jdbc:postgresql://).
func (c *ConnectionInfo) formatURL(urlFormat, svcType string) string {
	switch svcType {
	case "postgres", "mysql":
		return fmt.Sprintf(urlFormat, c.Username, c.Password, c.Host, c.Port, c.Database)
	case "mongodb":
		return fmt.Sprintf(urlFormat, c.Username, c.Password, c.Host, c.Port, c.Database)
	case "rabbitmq":
		return fmt.Sprintf(urlFormat, c.Username, c.Password, c.Host, c.Port)
	case "redis", "opensearch":
		return fmt.Sprintf(urlFormat, c.Host, c.Port)
	case "minio":
		// S3 has no user:pass@host URL form; show the plain endpoint and
		// keep the secret key out of the connection URL.
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

// versionLedTag matches a default tag whose leading token is a bare version,
// which is what makes the rest of it a variant rather than part of the version.
var versionLedTag = regexp.MustCompile(`^\d+(\.\d+)*-`)

// imageWithVersion applies a user's --version to a catalog image, keeping the
// variant the catalog chose.
//
// Most catalog entries name a version and a variant, so `--version 15` against
// postgres:16-alpine has to produce postgres:15-alpine rather than postgres:15,
// which is a different image. That only holds where the default tag leads with
// a version: MinIO tags releases as RELEASE.2025-09-07T16-13-09Z, where the
// hyphens are inside the timestamp, and splitting on the first one would append
// half of the old date to the new tag.
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
