// Package registrycred stores the admin-managed container-registry credentials
// and resolves which one a workload image needs. The whole list lives in one
// Secret in kipper-system and is read by both the settings handlers (which
// manage it) and the reconcilers (which stage a scoped pull Secret for a
// workload that runs a private third-party image), so a registry credential is
// never fanned out into every tenant namespace.
package registrycred

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// ConfigSecretName is the Secret holding the registry-credential list.
	ConfigSecretName = "kipper-registries" //nolint:gosec // k8s Secret object name, not a credential value
	// Namespace is where the list Secret lives; tenants cannot read it.
	Namespace = "kipper-system"
	// ClusterRegistryHost is the in-cluster Zot registry endpoint. Workload
	// images under it are namespaced <namespace>/<app>, and the nodes pull
	// them through the k3s registries mirror without a pod-level secret.
	ClusterRegistryHost = "zot.kipper-system.svc.cluster.local:5000"
	// dockerHubKey is the canonical Docker Hub auth key and match key.
	dockerHubKey   = "https://index.docker.io/v1/"
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "kipper"
	dataKey        = "registries"
)

// Entry is one container-registry credential. AllowedProjects lists the
// project names whose workloads may pull with it; an empty list denies every
// project (fail closed), so a credential is never staged into a tenant
// namespace until an admin explicitly grants a project.
type Entry struct {
	Name            string   `json:"name"`
	Server          string   `json:"server"`
	Username        string   `json:"username"`
	Password        string   `json:"password,omitempty"`
	AllowedProjects []string `json:"allowedProjects,omitempty"`
}

// AllowsProject reports whether project is on the entry's allow-list.
func (e Entry) AllowsProject(project string) bool {
	if project == "" {
		return false
	}
	for _, p := range e.AllowedProjects {
		if p == project {
			return true
		}
	}
	return false
}

// Load returns the registry-credential list. A missing list Secret (or a Secret
// with no registries key) returns (nil, nil); a read or parse failure returns an
// error so a caller enforcing on the list can fail closed.
func Load(ctx context.Context, client kubernetes.Interface) ([]Entry, error) {
	secret, err := client.CoreV1().Secrets(Namespace).Get(ctx, ConfigSecretName, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading registry credentials: %w", err)
	}
	return parse(secret.Data[dataKey])
}

// LoadCR is Load for a controller-runtime client, so a reconciler can resolve a
// workload's registry credential through its cached client.
func LoadCR(ctx context.Context, c crclient.Client) ([]Entry, error) {
	var secret corev1.Secret
	err := c.Get(ctx, crclient.ObjectKey{Namespace: Namespace, Name: ConfigSecretName}, &secret)
	if k8serrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading registry credentials: %w", err)
	}
	return parse(secret.Data[dataKey])
}

func parse(data []byte) ([]Entry, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parsing registry credentials: %w", err)
	}
	return entries, nil
}

// Save writes the registry-credential list back to its Secret.
func Save(ctx context.Context, client kubernetes.Interface, entries []Entry) error {
	data, err := json.Marshal(entries) //nolint:gosec // passwords are intentionally stored in a K8s Secret
	if err != nil {
		return err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ConfigSecretName,
			Namespace: Namespace,
			Labels:    map[string]string{managedByLabel: managedByValue},
		},
		Data: map[string][]byte{dataKey: data},
	}
	_, err = client.CoreV1().Secrets(Namespace).Update(ctx, secret, metav1.UpdateOptions{})
	if k8serrors.IsNotFound(err) {
		_, err = client.CoreV1().Secrets(Namespace).Create(ctx, secret, metav1.CreateOptions{})
	}
	return err
}

// NormalizeServer maps the common Docker Hub aliases to the exact key the
// container runtime matches on (`https://index.docker.io/v1/`), leaving any
// other server unchanged. Docker Hub pulls only resolve against that key, so an
// operator who enters `docker.io` would otherwise store a credential that is
// never used.
func NormalizeServer(server string) string {
	s := strings.TrimSpace(server)
	host := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://"), "/")
	switch strings.ToLower(host) {
	case "docker.io", "index.docker.io", "index.docker.io/v1", "registry-1.docker.io", "registry.hub.docker.com":
		return dockerHubKey
	}
	return s
}

// matchKey reduces a registry server to a comparable host key: Docker Hub in any
// spelling becomes the canonical Docker Hub key, and any other server is
// stripped of scheme and path so it compares by host[:port].
func matchKey(server string) string {
	if NormalizeServer(server) == dockerHubKey {
		return dockerHubKey
	}
	host := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(server), "https://"), "http://"), "/")
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	return strings.ToLower(host)
}

// RegistryHostForImage returns the match key of the registry an image ref pulls
// from. An image with no registry component (or a Docker Hub host) maps to the
// canonical Docker Hub key; an explicit host (a first path component containing
// "." or ":", or "localhost") maps to that host[:port]. This is compared against
// matchKey(entry.Server) to find the credential a workload image needs.
func RegistryHostForImage(image string) string {
	image = strings.TrimSpace(image)
	if image == "" {
		return ""
	}
	first, _, ok := strings.Cut(image, "/")
	if !ok || (!strings.ContainsAny(first, ".:") && first != "localhost") {
		return dockerHubKey // implicit Docker Hub (library/nginx, myorg/app, nginx:tag)
	}
	return matchKey(first)
}

// IsClusterRegistryImage reports whether an image ref pulls from the cluster
// registry.
func IsClusterRegistryImage(image string) bool {
	return RegistryHostForImage(image) == matchKey(ClusterRegistryHost)
}

// ClusterImageNamespace returns the namespace component of a cluster-registry
// image ref (builds push to <namespace>/<app>), or "" when the ref carries
// none.
func ClusterImageNamespace(image string) string {
	first, rest, ok := strings.Cut(strings.TrimSpace(image), "/")
	if !ok || matchKey(first) != matchKey(ClusterRegistryHost) {
		return ""
	}
	ns, _, ok := strings.Cut(rest, "/")
	if !ok {
		return ""
	}
	return ns
}

// FindForProject returns the first entry that matches the image's registry AND
// allows the project, or nil when none does (or the image or project is
// empty). Selection and authorization are one decision, so two same-host
// credentials granted to different projects each serve their own projects
// instead of list order deciding for every project.
func FindForProject(entries []Entry, image, project string) *Entry {
	want := RegistryHostForImage(image)
	if want == "" {
		return nil
	}
	for i := range entries {
		if matchKey(entries[i].Server) == want && entries[i].AllowsProject(project) {
			return &entries[i]
		}
	}
	return nil
}

// DockerConfigJSON renders a single-registry .dockerconfigjson for an entry,
// keyed on its server, for use as a workload's imagePullSecret. The auth field
// carries the base64 username:password pair — the form every container runtime
// resolves, and the same shape `kubectl create secret docker-registry` writes.
func (e Entry) DockerConfigJSON() ([]byte, error) {
	cfg := map[string]any{
		"auths": map[string]any{
			e.Server: map[string]string{
				"username": e.Username,
				"password": e.Password,
				"auth":     base64.StdEncoding.EncodeToString([]byte(e.Username + ":" + e.Password)),
			},
		},
	}
	return json.Marshal(cfg) //nolint:gosec // password is intentionally stored in a K8s Secret
}
