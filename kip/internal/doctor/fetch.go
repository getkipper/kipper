package doctor

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/getkipper/kipper/controller/pkg/hopca"
)

// Runner runs a shell command on the cluster's server and returns its output.
type Runner interface {
	Run(command string) (string, error)
}

// Part names one piece of a Snapshot, so a check can say which read it is missing.
type Part string

const (
	PartNodes       Part = "nodes"
	PartWorkloads   Part = "core workloads"
	PartHopCert     Part = "hop certificate"
	PartTLSStores   Part = "TLS stores"
	PartClusterHost Part = "cluster identity"
	PartProfile     Part = "platform profile"
	PartHeartbeat   Part = "gateway heartbeat log"
	PartTraefikCopy Part = "Traefik's copy of the hop certificate"
)

// Workload identifies a Deployment to check for availability.
type Workload struct {
	Namespace string
	Name      string
	Label     string
}

// Key returns the workload's "namespace/name".
func (w Workload) Key() string { return w.Namespace + "/" + w.Name }

// CoreWorkloads lists the Deployments checked for cluster health, in display order.
var CoreWorkloads = []Workload{
	{"traefik", "traefik", "Traefik"},
	{"kube-system", "coredns", "CoreDNS"},
	{"cert-manager", "cert-manager-webhook", "cert-manager"},
	{"dex", "dex", "Dex"},
	{"kipper-system", "console-api", "Console API"},
}

// TLSStore is a Traefik TLSStore and the Secret it serves by default.
type TLSStore struct {
	Namespace   string
	Name        string
	DefaultCert string
}

// Snapshot holds cluster observations. Missing records read failures by part.
// A failed part may contain partial data, which checks must treat as incomplete.
type Snapshot struct {
	Nodes           []corev1.Node
	Workloads       map[string]*appsv1.Deployment
	HopCert         string
	HopCandidate    string
	TraefikCopyCert string
	TLSStores       []TLSStore
	ClusterHost     string
	Profile         string
	Heartbeat       []string
	Backups         BackupState
	Missing         map[Part]error
}

const (
	hopNamespace  = "kipper-system"
	hopCertName   = "kipper-hop-cert"
	heartbeatTail = 20
	// requestTimeout limits each kubectl server request so stalled requests
	// can be reported as read failures.
	requestTimeout = "--request-timeout=20s"
)

// FetchOverSSH reads a Snapshot by running kubectl on the server, which works
// even when the Kubernetes API rejects the operator's own login. It reads the
// public certificate fields individually, keeping private keys out of the output.
// Read failures are recorded in Missing while the remaining reads continue.
// Heartbeat contains at most 20 lines from the last 48 hours, oldest first.
func FetchOverSSH(r Runner) Snapshot {
	snap := Snapshot{Workloads: map[string]*appsv1.Deployment{}, Missing: map[Part]error{}}

	var nodes corev1.NodeList
	if err := runJSON(r, "kubectl get nodes -o json "+requestTimeout, &nodes); err != nil {
		snap.Missing[PartNodes] = err
	} else {
		snap.Nodes = nodes.Items
	}

	var deployments appsv1.DeploymentList
	if err := runJSON(r, "kubectl get deployments -A -o json "+requestTimeout, &deployments); err != nil {
		snap.Missing[PartWorkloads] = err
	} else {
		core := map[string]bool{}
		for _, w := range CoreWorkloads {
			core[w.Key()] = true
		}
		for i := range deployments.Items {
			d := &deployments.Items[i]
			if key := d.Namespace + "/" + d.Name; core[key] {
				snap.Workloads[key] = d
			}
		}
	}

	if cert, err := readSecretKey(r, hopNamespace, hopCertName, "tls.crt"); err != nil {
		snap.Missing[PartHopCert] = err
	} else {
		snap.HopCert = cert
		// A candidate exists only during key rotation, so its absence is normal.
		snap.HopCandidate, _ = readSecretKey(r, hopNamespace, hopCertName, "candidate.crt")
	}
	if cert, err := readSecretKey(r, hopca.TraefikNamespace, hopca.TraefikCopyName, "tls.crt"); err != nil {
		snap.Missing[PartTraefikCopy] = err
	} else {
		snap.TraefikCopyCert = cert
	}

	var stores struct {
		Items []struct {
			Metadata struct{ Namespace, Name string } `json:"metadata"`
			Spec     struct {
				DefaultCertificate struct {
					SecretName string `json:"secretName"`
				} `json:"defaultCertificate"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := runJSON(r, "kubectl get tlsstores.traefik.io -A -o json "+requestTimeout, &stores); err != nil {
		snap.Missing[PartTLSStores] = err
	} else {
		for _, s := range stores.Items {
			snap.TLSStores = append(snap.TLSStores, TLSStore{
				Namespace: s.Metadata.Namespace, Name: s.Metadata.Name, DefaultCert: s.Spec.DefaultCertificate.SecretName,
			})
		}
	}

	if out, err := r.Run("kubectl get clusteridentities.kipper.run cluster --ignore-not-found -o jsonpath='{.spec.gateway.clusterHost}' " + requestTimeout); err != nil {
		snap.Missing[PartClusterHost] = commandError(err)
	} else {
		snap.ClusterHost = withoutWarnings(out)
	}

	if out, err := r.Run("kubectl get platformconfigs.kipper.run platform --ignore-not-found -o jsonpath='{.spec.profile}' " + requestTimeout); err != nil {
		snap.Missing[PartProfile] = commandError(err)
	} else {
		snap.Profile = withoutWarnings(out)
	}

	// Filter locally so a failed kubectl command remains distinguishable from an empty log.
	if out, err := r.Run(fmt.Sprintf("kubectl -n %s logs deploy/console-api --since=48h --timestamps %s", hopNamespace, requestTimeout)); err != nil {
		snap.Missing[PartHeartbeat] = commandError(err)
	} else {
		var lines []string
		for _, line := range nonEmptyLines(out) {
			if strings.Contains(line, "gateway heartbeat") {
				lines = append(lines, line)
			}
		}
		snap.Heartbeat = newestHeartbeatLines(lines)
	}

	if state, err := fetchBackupsOverSSH(r); err != nil {
		snap.Missing[PartBackups] = err
	} else {
		snap.Backups = state
	}
	return snap
}

func runJSON(r Runner, command string, into any) error {
	out, err := r.Run(command)
	if err != nil {
		return commandError(err)
	}
	// kubectl can print warnings ahead of the document, and the runner merges them into the output.
	if i := strings.IndexByte(out, '{'); i > 0 {
		out = out[i:]
	}
	if err := json.Unmarshal([]byte(out), into); err != nil {
		return fmt.Errorf("reading the output of %q: %w", command, err)
	}
	return nil
}

// readSecretKey returns one base64-decoded field of a Secret. An absent Secret
// or field returns an empty string. Errors omit command output because it could
// contain the decoded value.
func readSecretKey(r Runner, namespace, name, key string) (string, error) {
	cmd := fmt.Sprintf("set -o pipefail; kubectl -n %s get secret %s --ignore-not-found -o jsonpath='{.data.%s}' %s 2>/dev/null | base64 -d",
		namespace, name, strings.ReplaceAll(key, ".", `\.`), requestTimeout)
	out, err := r.Run(cmd)
	if err != nil {
		return "", fmt.Errorf("reading %s/%s %s failed", namespace, name, key)
	}
	return strings.TrimSpace(out), nil
}

func commandError(err error) error {
	return fmt.Errorf("%s", strings.TrimSpace(err.Error()))
}

// withoutWarnings drops the warning lines kubectl prints ahead of a value.
func withoutWarnings(out string) string {
	var kept []string
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "Warning:") {
			kept = append(kept, line)
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// newestHeartbeatLines orders heartbeat lines by their timestamp, oldest
// first, and keeps the newest heartbeatTail of them. Lines from several pods
// arrive in pod order, which says nothing about which event is newest.
func newestHeartbeatLines(lines []string) []string {
	sorted := slices.Clone(lines)
	slices.SortStableFunc(sorted, func(a, b string) int {
		ta, _ := lineTime(a)
		tb, _ := lineTime(b)
		return ta.Compare(tb)
	})
	if len(sorted) > heartbeatTail {
		sorted = sorted[len(sorted)-heartbeatTail:]
	}
	return sorted
}

func nonEmptyLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
