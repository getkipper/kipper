package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/k8s"
)

var longhornSettingGVR = schema.GroupVersionResource{Group: "longhorn.io", Version: "v1beta2", Resource: "settings"}

const (
	// longhornBackupTimeout bounds the wait for each backup taken before a Longhorn step.
	longhornBackupTimeout = 2 * time.Hour
	longhornBackupTTL     = "720h"
)

// longhornReport is what kip upgrade reads about a cluster's Longhorn before
// it changes anything. Problems keep --longhorn from starting, and include
// Held, the nodes under the RHEL 9.7 hold.
type longhornReport struct {
	Longhorn bool
	Profile  string
	Plan     installer.LonghornPlan
	Record   *installer.LonghornHopRecord
	Started  bool
	Problems []string
	Held     []string
}

// checkLonghornFlags refuses --longhorn with --skip-system, which skips the
// step that runs it.
func checkLonghornFlags(longhorn, skipSystem bool) error {
	if longhorn && skipSystem {
		return fmt.Errorf("--longhorn upgrades Longhorn in the system-component step, which --skip-system skips. Drop one of the two flags")
	}
	return nil
}

// readLonghornReport reads the cluster's Longhorn through the Kubernetes API:
// the storage mode, the hop record, the applied and running releases, the
// backup destination and the nodes. It changes nothing. A cluster on local
// storage returns a report with Longhorn false. With k3sFirst the Kubernetes
// floors are left to the hop checks, since --k3s moves the nodes first.
func readLonghornReport(ctx context.Context, cs kubernetes.Interface, dyn dynamic.Interface, localExternal, k3sFirst bool) (longhornReport, error) {
	var r longhornReport
	pc, err := dyn.Resource(platformConfigGVR).Get(ctx, "platform", metav1.GetOptions{})
	if err != nil {
		return r, fmt.Errorf("reading the PlatformConfig: %w", err)
	}
	storage, _, _ := unstructured.NestedString(pc.Object, "spec", "storage")
	if storage != "" && storage != installer.StorageLonghorn {
		return r, nil
	}
	r.Longhorn = true
	r.Profile, _, _ = unstructured.NestedString(pc.Object, "spec", "profile")
	backup, _, _ := unstructured.NestedString(pc.Object, "spec", "backup")

	markers, err := readLonghornMarkersViaAPI(ctx, cs, dyn)
	if err != nil {
		return r, err
	}
	r.Record, r.Started = markers.Record, markers.HopStarted()
	version, fresh, err := installer.LonghornVersionToApply(markers)
	switch {
	case err != nil:
		r.Problems = append(r.Problems, err.Error())
	case fresh:
		r.Problems = append(r.Problems, "Longhorn is not installed on this cluster, so there is nothing to upgrade")
	default:
		if r.Plan, err = installer.PlanLonghornUpgrade(version); err != nil {
			r.Problems = append(r.Problems, err.Error())
		}
	}

	problem, err := longhornBackupProblem(ctx, cs, dyn, backup, localExternal)
	if err != nil {
		return r, err
	}
	if problem != "" {
		r.Problems = append(r.Problems, problem)
	}
	if problem, err = unbackedVolumesProblem(ctx, cs); err != nil {
		return r, err
	}
	if problem != "" {
		r.Problems = append(r.Problems, problem)
	}

	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return r, fmt.Errorf("listing nodes: %w", err)
	}
	facts := nodeFactsOf(nodes.Items)
	r.Held = installer.RHEL97LevelProblems(facts)
	r.Problems = append(r.Problems, r.Held...)
	if !k3sFirst {
		r.Problems = append(r.Problems, r.Plan.KubernetesProblems(facts)...)
	}
	return r, nil
}

// readLonghornMarkersViaAPI reads version markers through the Kubernetes
// API, using the volumes CRD to detect an existing Longhorn installation.
func readLonghornMarkersViaAPI(ctx context.Context, cs kubernetes.Interface, dyn dynamic.Interface) (installer.LonghornMarkers, error) {
	var m installer.LonghornMarkers
	cm, err := cs.CoreV1().ConfigMaps(installer.NodeRecordsNamespace).Get(ctx, installer.LonghornHopRecordName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return m, fmt.Errorf("reading the Longhorn hop record: %w", err)
	default:
		m.Record = &installer.LonghornHopRecord{From: cm.Data["from"], To: cm.Data["to"], Started: cm.Data["started"], Backup: cm.Data["backup"], Export: cm.Data["export"]}
	}

	ds, err := cs.AppsV1().DaemonSets("longhorn-system").Get(ctx, "longhorn-manager", metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return m, fmt.Errorf("reading the longhorn-manager DaemonSet: %w", err)
	default:
		for _, c := range ds.Spec.Template.Spec.Containers {
			if c.Name == "longhorn-manager" {
				if m.Applied, err = installer.LonghornManagerVersion(c.Image); err != nil {
					return m, err
				}
			}
		}
	}

	setting, err := dyn.Resource(longhornSettingGVR).Namespace("longhorn-system").Get(ctx, "current-longhorn-version", metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return m, fmt.Errorf("reading Longhorn's current-longhorn-version setting: %w", err)
	default:
		m.Running, _, _ = unstructured.NestedString(setting.Object, "value")
	}

	_, err = dyn.Resource(crdGVR).Get(ctx, "volumes.longhorn.io", metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return m, fmt.Errorf("checking for Longhorn's CRDs: %w", err)
	default:
		m.CRDs = true
	}
	return m, nil
}

const longhornBackupFix = "Configure external backup storage first: run kip install again with --backup-storage-bucket and its related flags (read \"Re-running install\" in the installation docs before you do), then run " + installer.LonghornUpgradeCommand

// longhornBackupProblem returns why the cluster's backups cannot protect a
// Longhorn upgrade, or "" when they live off the cluster and the local kip
// configuration can keep them there. mode is the PlatformConfig's backup mode.
func longhornBackupProblem(ctx context.Context, cs kubernetes.Interface, dyn dynamic.Interface, mode string, localExternal bool) (string, error) {
	if mode == installer.BackupNone {
		return "Longhorn upgrades need a backup that lives off this cluster, and this cluster takes no backups. " + longhornBackupFix, nil
	}
	if problem, err := backupLocationProblem(ctx, cs, dyn); problem != "" || err != nil {
		return problem, err
	}
	if !localExternal {
		return "this cluster uses external backup storage, and its connection settings are missing from the local kip configuration, so the upgrade would point Velero back at in-cluster MinIO. " +
			"Run kip install again with --backup-storage-bucket and its related flags, then run " + installer.LonghornUpgradeCommand, nil
	}
	return "", nil
}

// unbackedVolumesProblem lists claims in the user-workload backup scope that
// lack a regular-container mount in a running pod. An empty string means all
// claims in that scope have mounts. API read failures return an error.
func unbackedVolumesProblem(ctx context.Context, cs kubernetes.Interface) (string, error) {
	pvcs, err := cs.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("listing volume claims: %w", err)
	}
	pods, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("listing pods: %w", err)
	}
	outside := volumesOutsideBackups(pvcs.Items, pods.Items)
	if len(outside) == 0 {
		return "", nil
	}
	return fmt.Sprintf("Longhorn upgrades need a backup of every volume, and no regular container in a running pod mounts %s, so the backup would omit %s files. Start the app that uses %s with kip app start <app>, or remove a volume you no longer need, then run %s",
		strings.Join(outside, ", "), plural(len(outside), "its", "their"), plural(len(outside), "it", "each"), installer.LonghornUpgradeCommand), nil
}

// lookupBackupHost resolves a backup endpoint's host name to its addresses.
var lookupBackupHost = func(ctx context.Context, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

// backupLocationProblem returns a blocking reason for a missing backup
// location, an unreadable or unresolvable endpoint, or a name or address
// associated with the cluster. Loopback, link-local and unspecified addresses
// also block the upgrade. An empty endpoint means AWS S3.
// An empty result means these checks pass. API read failures return an error.
func backupLocationProblem(ctx context.Context, cs kubernetes.Interface, dyn dynamic.Interface) (string, error) {
	const need = "Longhorn upgrades need a backup that lives off this cluster, and "
	inside := need + "this cluster's backups stay inside it (in-cluster MinIO by default), on the Longhorn storage the upgrade changes. " + longhornBackupFix
	bsl, err := dyn.Resource(backupStorageLocationGVR).Namespace("velero").Get(ctx, "default", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return inside, nil
	}
	if err != nil {
		return "", fmt.Errorf("reading Velero's backup location: %w", err)
	}
	endpoint, _, err := unstructured.NestedString(bsl.Object, "spec", "config", "s3Url")
	if err != nil {
		return fmt.Sprintf("%skip cannot read the endpoint of Velero's backup location (%v). %s", need, err, longhornBackupFix), nil
	}
	if endpoint == "" {
		return "", nil
	}
	host := ""
	if u, err := url.Parse(endpoint); err == nil {
		host = u.Hostname()
	}
	if host == "" {
		return fmt.Sprintf("%skip cannot read the endpoint %q of Velero's backup location. %s", need, endpoint, longhornBackupFix), nil
	}
	nsList, err := cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("listing namespaces: %w", err)
	}
	namespaces := map[string]bool{}
	for _, ns := range nsList.Items {
		namespaces[ns.Name] = true
	}
	if inClusterEndpoint(endpoint, namespaces) {
		return inside, nil
	}
	ips := []net.IP{net.ParseIP(host)}
	if ips[0] == nil {
		if ips, err = lookupBackupHost(ctx, host); err != nil || len(ips) == 0 {
			if err == nil {
				err = errors.New("no address")
			}
			return fmt.Sprintf("%skip cannot resolve %s, the endpoint of Velero's backup location (%v), so it cannot tell whether the backups leave the cluster. %s", need, host, err, longhornBackupFix), nil
		}
	}
	addrs, err := readClusterAddresses(ctx, cs)
	if err != nil {
		return "", err
	}
	for _, ip := range ips {
		if what := addrs.owner(ip); what != "" {
			location := "Velero's backup location " + host
			if host == ip.String() {
				location = "Velero's backup location"
			}
			return fmt.Sprintf("%s%s points at %s, %s. %s", need, location, ip, what, longhornBackupFix), nil
		}
	}
	return "", nil
}

// clusterAddresses are the addresses that lead into the cluster: node
// addresses, Service addresses and endpoints, and the pod and service ranges.
type clusterAddresses struct {
	ips  map[string]string
	nets []clusterNet
}

type clusterNet struct {
	cidr *net.IPNet
	what string
}

// readClusterAddresses collects node addresses and pod ranges, Service
// addresses, EndpointSlice addresses and ServiceCIDR ranges. When the API
// does not support ServiceCIDRs, only individual Service addresses are checked.
// Each address keeps its first owner, so a host-network pod's endpoint
// retains the node's name.
func readClusterAddresses(ctx context.Context, cs kubernetes.Interface) (clusterAddresses, error) {
	a := clusterAddresses{ips: map[string]string{}}
	addRange := func(cidr, what string) {
		if _, n, err := net.ParseCIDR(cidr); err == nil {
			a.nets = append(a.nets, clusterNet{cidr: n, what: what})
		}
	}
	addIP := func(addr, what string) {
		if ip := net.ParseIP(addr); ip != nil {
			if _, seen := a.ips[ip.String()]; !seen {
				a.ips[ip.String()] = what
			}
		}
	}
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return a, fmt.Errorf("listing nodes: %w", err)
	}
	for _, n := range nodes.Items {
		for _, cidr := range append([]string{n.Spec.PodCIDR}, n.Spec.PodCIDRs...) {
			addRange(cidr, fmt.Sprintf("inside node %s's pod range %s", n.Name, cidr))
		}
		for _, addr := range n.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP || addr.Type == corev1.NodeExternalIP {
				addIP(addr.Address, "an address of node "+n.Name)
			}
		}
	}
	cidrs, err := cs.NetworkingV1().ServiceCIDRs().List(ctx, metav1.ListOptions{})
	switch {
	case apierrors.IsNotFound(err) || apierrors.IsMethodNotSupported(err):
	case err != nil:
		return a, fmt.Errorf("listing ServiceCIDRs: %w", err)
	default:
		for _, sc := range cidrs.Items {
			for _, cidr := range sc.Spec.CIDRs {
				addRange(cidr, "inside the cluster's service range "+cidr)
			}
		}
	}
	services, err := cs.CoreV1().Services("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return a, fmt.Errorf("listing services: %w", err)
	}
	for _, svc := range services.Items {
		what := fmt.Sprintf("an address of service %s/%s", svc.Namespace, svc.Name)
		for _, addr := range append(append([]string{svc.Spec.ClusterIP}, svc.Spec.ClusterIPs...), svc.Spec.ExternalIPs...) {
			addIP(addr, what)
		}
		for _, ing := range svc.Status.LoadBalancer.Ingress {
			addIP(ing.IP, what)
		}
	}
	slices, err := cs.DiscoveryV1().EndpointSlices("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return a, fmt.Errorf("listing endpoint slices: %w", err)
	}
	for _, sl := range slices.Items {
		what := fmt.Sprintf("an endpoint of service %s/%s", sl.Namespace, sl.Labels[discoveryv1.LabelServiceName])
		for _, ep := range sl.Endpoints {
			for _, addr := range ep.Addresses {
				addIP(addr, what)
			}
		}
	}
	return a, nil
}

// owner describes a matching cluster address or range, or a loopback,
// link-local or unspecified address. An empty string means no match.
func (a clusterAddresses) owner(ip net.IP) string {
	switch {
	case ip.IsLoopback():
		return "a loopback address"
	case ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast():
		return "a link-local address"
	case ip.IsUnspecified():
		return "an unspecified address"
	}
	if what, ok := a.ips[ip.String()]; ok {
		return what
	}
	for _, n := range a.nets {
		if n.cidr.Contains(ip) {
			return n.what
		}
	}
	return ""
}

// inClusterEndpoint reports whether an S3 endpoint names a service inside the
// cluster, such as the bundled MinIO under any of its DNS names: a bare name,
// <service>.<namespace> for one of the cluster's namespaces, or a name under
// .svc or .cluster.local. An empty endpoint is AWS S3, and one kip cannot
// parse counts as in the cluster. An address is left to backupLocationProblem.
func inClusterEndpoint(s3URL string, namespaces map[string]bool) bool {
	if s3URL == "" {
		return false
	}
	u, err := url.Parse(s3URL)
	if err != nil || u.Hostname() == "" {
		return true
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if net.ParseIP(host) != nil {
		return false
	}
	labels := strings.Split(host, ".")
	return len(labels) == 1 || (len(labels) == 2 && namespaces[labels[1]]) ||
		strings.HasSuffix(host, ".svc") || strings.HasSuffix(host, ".cluster.local")
}

func nodeFactsOf(nodes []corev1.Node) []installer.NodeFacts {
	facts := make([]installer.NodeFacts, 0, len(nodes))
	for _, n := range nodes {
		ni := n.Status.NodeInfo
		facts = append(facts, installer.NodeFacts{Name: n.Name, KubeletVersion: ni.KubeletVersion, OSImage: ni.OSImage, Kernel: ni.KernelVersion})
	}
	return facts
}

// print writes the Longhorn lines of kip upgrade --check. With strict, the
// operator asked for --longhorn, so problems are failures and the error says
// a decision is needed. Otherwise they are warnings.
func (r longhornReport) print(out io.Writer, strict bool) error {
	if !r.Longhorn {
		if strict {
			_, _ = fmt.Fprintf(out, "  ✗  Longhorn: this cluster uses local storage, so there is no Longhorn to upgrade\n")
			return fmt.Errorf("--longhorn has nothing to upgrade on a cluster with local storage")
		}
		return nil
	}
	switch {
	case r.Started:
		_, _ = fmt.Fprintf(out, "  ⚠  Longhorn: the step from %s to %s did not finish, and %s resumes it\n", r.Record.From, r.Record.To, installer.LonghornUpgradeCommand)
	case r.Record != nil && r.Record.From == r.Record.To:
		_, _ = fmt.Fprintf(out, "  ⚠  Longhorn: the engine catch-up on %s did not finish, and %s starts it again with a new backup\n", r.Record.From, installer.LonghornUpgradeCommand)
	case r.Record != nil:
		_, _ = fmt.Fprintf(out, "  ⚠  Longhorn: the step from %s to %s was recorded but never applied, and %s starts it again with a new backup\n", r.Record.From, r.Record.To, installer.LonghornUpgradeCommand)
	}
	switch {
	case r.Plan.From == "":
	case len(r.Plan.Hops) == 0:
		_, _ = fmt.Fprintf(out, "  ✔  Longhorn: runs %s, the release this kip installs\n", r.Plan.From)
	default:
		_, _ = fmt.Fprintf(out, "  ✔  Longhorn: %s moves it %s, with a backup before each step\n", installer.LonghornUpgradeCommand, r.Plan.Describe())
	}
	mark := "⚠"
	if strict {
		mark = "✗"
	}
	for _, p := range r.Problems {
		_, _ = fmt.Fprintf(out, "  %s  Longhorn: %s\n", mark, p)
	}
	if strict && len(r.Problems) > 0 {
		return fmt.Errorf("the cluster's Longhorn needs a decision before '%s' can run", installer.LonghornUpgradeCommand)
	}
	return nil
}

// checkLonghornUpgrade prints the Longhorn lines of kip upgrade --check. The
// caller counts its error only with --longhorn.
func checkLonghornUpgrade(ctx context.Context, cs kubernetes.Interface, dyn dynamic.Interface, out io.Writer, cluster *config.Cluster, k3sFirst, strict bool) error {
	r, err := readLonghornReport(ctx, cs, dyn, installer.BackupStorageFromRef(cluster.BackupStorage) != nil, k3sFirst)
	if err != nil {
		mark := "⚠"
		if strict {
			mark = "✗"
		}
		_, _ = fmt.Fprintf(out, "  %s  Longhorn: %v\n", mark, err)
		return err
	}
	return r.print(out, strict)
}

// preflightLonghorn reads the Longhorn report before kip upgrade changes
// anything. With strict it refuses on any problem. Otherwise it warns about
// held nodes and carries on, also when the report cannot be read.
func preflightLonghorn(ctx context.Context, cs kubernetes.Interface, dyn dynamic.Interface, out io.Writer, cluster *config.Cluster, k3sFirst, strict bool) (longhornReport, error) {
	r, err := readLonghornReport(ctx, cs, dyn, installer.BackupStorageFromRef(cluster.BackupStorage) != nil, k3sFirst)
	if err != nil {
		if strict {
			return r, err
		}
		_, _ = fmt.Fprintf(out, "  ⚠  could not read Longhorn's state (%v). The upgrade continues\n", err)
		return longhornReport{}, nil
	}
	if strict {
		if err := r.print(out, true); err != nil {
			return r, fmt.Errorf("%w. Longhorn was not changed", err)
		}
		return r, nil
	}
	for _, h := range r.Held {
		_, _ = fmt.Fprintf(out, "  ⚠  Longhorn host: %s\n", h)
	}
	return r, nil
}

// longhornStep is how the system-component step treats Longhorn: Upgrade is
// set with --longhorn and replaces the plain re-apply.
type longhornStep struct {
	Report  longhornReport
	Upgrade *installer.LonghornUpgrade
}

// apply wraps the Longhorn component in the node lifecycle lease, using
// Upgrade when set. It updates components in place and returns the same slice.
func (s longhornStep) apply(components []installer.SystemComponent) []installer.SystemComponent {
	for i := range components {
		if components[i].Name != "longhorn" {
			continue
		}
		run, purpose := components[i].Apply, "kip upgrade"
		if s.Upgrade != nil {
			run, purpose = s.Upgrade.Apply, installer.LonghornUpgradeCommand
		}
		components[i].Apply = func(client installer.CommandRunner) error {
			return withNodeLease(client, purpose, run)
		}
	}
	return components
}

// withNodeLease calls run with a lease-guarded client to serialize node,
// cluster access and Longhorn changes made through this lease. It returns
// an error if client cannot hold the lease or another run holds it.
func withNodeLease(client installer.CommandRunner, purpose string, run func(installer.CommandRunner) error) error {
	server, ok := client.(interface {
		installer.LeaseServer
		installer.FirewallRunner
	})
	if !ok {
		return fmt.Errorf("kip cannot take the node lifecycle lease over this connection, so Longhorn was not changed")
	}
	lease, err := installer.TakeNodeLease(server, purpose)
	if err != nil {
		return fmt.Errorf("%w. Longhorn was not changed", err)
	}
	defer lease.Release()
	return run(lease.Server(server))
}

// preview writes what the system-component step does to Longhorn, and with
// --longhorn the recovery material to hold before the first step.
func (s longhornStep) preview(out io.Writer) {
	r := s.Report
	if !r.Longhorn || r.Plan.From == "" {
		return
	}
	if s.Upgrade == nil {
		switch {
		case r.Started:
			_, _ = fmt.Fprintf(out, "  Longhorn re-applies %s, where its last upgrade step stopped. Run %s to finish that step.\n\n", r.Record.To, installer.LonghornUpgradeCommand)
		case len(r.Plan.Hops) > 0:
			_, _ = fmt.Fprintf(out, "  Longhorn stays at %s. %s moves it to %s one minor at a time, with a backup before each step.\n\n", r.Plan.From, installer.LonghornUpgradeCommand, installer.LonghornTarget())
		}
		return
	}
	if len(r.Plan.Hops) == 0 && r.Record == nil {
		_, _ = fmt.Fprintf(out, "  Longhorn already runs %s. kip re-applies it, and any volume still on an older engine\n", r.Plan.From)
		_, _ = fmt.Fprintf(out, "  moves onto the current one after kip takes a Velero backup.\n\n")
	} else {
		_, _ = fmt.Fprintf(out, "  Longhorn moves %s, one minor at a time. Before each step kip checks every volume,\n", r.Plan.Describe())
		_, _ = fmt.Fprintf(out, "  takes a Velero backup and waits for it to complete, and saves Longhorn's objects to\n")
		_, _ = fmt.Fprintf(out, "  %s. Each step then waits for every volume to settle,\n", s.Upgrade.ExportDir)
		_, _ = fmt.Fprintf(out, "  which can take an hour or more on a busy cluster. Longhorn cannot be downgraded: a\n")
		_, _ = fmt.Fprintf(out, "  step that fails is recovered by restoring the backups onto a fresh cluster.\n\n")
	}
	_, _ = fmt.Fprintf(out, "  Before you continue, have these off this cluster:\n")
	_, _ = fmt.Fprintf(out, "    - the backup repository key: kip backup repository-key export --to <file>\n")
	_, _ = fmt.Fprintf(out, "    - the SSE-C key, if backups are encrypted with one: kip backup sse-key export --to <file>\n")
	_, _ = fmt.Fprintf(out, "    - the credentials of the backup bucket\n")
	if r.Profile == installer.ProfileNano {
		_, _ = fmt.Fprintf(out, "    - a restore you have verified on a separate recovery cluster, since kip backup drill does not run on nano\n\n")
	} else {
		_, _ = fmt.Fprintf(out, "    - a restore you have verified: kip backup drill <namespace> for each namespace with data\n\n")
	}
}

var unsafePathChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// longhornExportDir is where a cluster's Longhorn objects are saved before
// each upgrade step: ~/.kip/longhorn-upgrades/<cluster>.
func longhornExportDir(cluster *config.Cluster) (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	name := unsafePathChars.ReplaceAllString(cluster.Name, "-")
	if strings.Trim(name, ".-") == "" {
		return "", errors.New("the cluster has no name kip can use for a directory")
	}
	return filepath.Join(dir, "longhorn-upgrades", name), nil
}

// newLonghornUpgrade builds the Longhorn upgrade with a Velero backup before
// every step, in the scope of 'kip backup create', kept for 30 days.
func newLonghornUpgrade(cluster *config.Cluster, client *k8s.Client) (*installer.LonghornUpgrade, error) {
	dir, err := longhornExportDir(cluster)
	if err != nil {
		return nil, err
	}
	take := func(ctx context.Context, name string) error {
		return takeBackup(ctx, cluster, client, name, longhornBackupTTL, 10*time.Second)
	}
	return &installer.LonghornUpgrade{Out: os.Stdout, ExportDir: dir, Backup: longhornBackup(client.Clientset(), client.Dynamic(), take)}, nil
}

// longhornBackup returns a callback that rechecks Velero's location and
// volume mounts before taking a backup named for the target release.
// Each call shares a two-hour timeout across the checks and backup.
// Failed checks return an empty name. Otherwise it returns the generated
// name and the error, if any, from take.
func longhornBackup(cs kubernetes.Interface, dyn dynamic.Interface, take func(ctx context.Context, name string) error) func(to string) (string, error) {
	return func(to string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), longhornBackupTimeout)
		defer cancel()
		for _, check := range []func() (string, error){
			func() (string, error) { return backupLocationProblem(ctx, cs, dyn) },
			func() (string, error) { return unbackedVolumesProblem(ctx, cs) },
		} {
			problem, err := check()
			if err != nil {
				return "", err
			}
			if problem != "" {
				return "", errors.New(problem)
			}
		}
		name := "before-longhorn-" + strings.ReplaceAll(to, ".", "-") + "-" + time.Now().UTC().Format("20060102-150405")
		return name, take(ctx, name)
	}
}
