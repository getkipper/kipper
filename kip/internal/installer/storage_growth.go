package installer

import (
	"fmt"
	"io"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/kip/internal/service"
)

// StorageGrowthHardenAnnotation on PlatformConfig keeps the hardening
// decision of an unfinished 'kip cluster storage longhorn', so a rerun does
// not mistake services the interrupted package install enabled for the
// operator's own.
const StorageGrowthHardenAnnotation = "kipper.run/storage-growth-harden"

// StorageGrowth switches a local-storage cluster to Longhorn in place. The
// existing volumes stay on local-path. Each service moves with
// 'kip service storage longhorn'.
type StorageGrowth struct {
	Server          CommandRunner
	Out             io.Writer
	SkipMemoryFloor bool
}

func (g StorageGrowth) printf(format string, args ...any) {
	if g.Out != nil {
		_, _ = fmt.Fprintf(g.Out, format, args...)
	}
}

// Run installs Longhorn on the server and records the new storage mode last,
// so a run that stops early repeats the idempotent install. It refuses on a
// cluster that already uses Longhorn.
func (g StorageGrowth) Run() error {
	mode, err := ReadStorageMode(g.Server)
	if err != nil {
		return err
	}
	switch platform.StorageMode(mode) {
	case platform.StorageLonghorn:
		return fmt.Errorf("this cluster already uses Longhorn storage. Move each service still on local storage with 'kip service storage longhorn <name>'")
	case platform.StorageLocal:
	default:
		return fmt.Errorf("this cluster's storage mode %q is unknown", mode)
	}
	if err := requireStorageMoveSupport(g.Server); err != nil {
		return err
	}
	if err := checkLonghornCPU(g.Server); err != nil {
		return err
	}

	sys, err := GatherSystemInfo(g.Server)
	if err != nil {
		return fmt.Errorf("gathering system info: %w", err)
	}
	floorWarning, err := growthPreflight(sys, g.SkipMemoryFloor)
	if err != nil {
		return err
	}
	g.printf("  ✔  RAM: %d MiB, Longhorn needs at least %d MiB\n", sys.RAMMB, LonghornMinRAMMB)
	if floorWarning != "" {
		g.printf("  ⚠  %s\n", sentence(floorWarning))
	}
	volumes, err := LocalServiceVolumes(g.Server)
	if err != nil {
		g.printf("  ⚠  Service volumes on local storage were not measured: %v\n", err)
	} else if len(volumes) > 0 {
		var need int64
		for _, v := range volumes {
			need += int64(math.Ceil(float64(v.UsedBytes) * 1.2))
		}
		g.printf("  ℹ  Moving all %d service volumes needs about %.1f GiB free on top of their local copies, which stay until 'kip cluster storage prune'.\n",
			len(volumes), float64(need)/float64(int64(1)<<30))
	}

	host, err := DetectHostOS(g.Server)
	if err != nil {
		return err
	}
	harden, err := g.hardenDecision()
	if err != nil {
		return err
	}
	g.printf("  Preparing the host for Longhorn...\n")
	if err := ensureKernelModules(g.Server, host, true, true); err != nil {
		return err
	}
	if err := DisableNFSDirectoryDelegations(g.Server); err != nil {
		return err
	}
	if host.UsesNeedrestart() {
		if err := ConfigureStorageRestarts(g.Server); err != nil {
			return err
		}
	}
	g.printf("  Installing Longhorn...\n")
	if err := InstallLonghornThen(g.Server, hardenAfterStoragePackages(harden, g.Server)); err != nil {
		return err
	}
	if host.UsesNeedrestart() {
		g.stampStorageRestarts()
	}
	if floorWarning != "" {
		if err := RecordMemoryFloorOverride(g.Server, sys.RAMMB); err != nil {
			return err
		}
	}
	patch := `{"spec":{"storage":"longhorn"},"metadata":{"annotations":{"` + StorageGrowthHardenAnnotation + `":null}}}`
	if _, err := g.Server.RunStdin("kubectl patch platformconfig platform --type merge --patch-file /dev/stdin >/dev/null", strings.NewReader(patch)); err != nil {
		return fmt.Errorf("recording Longhorn as the storage mode: %w", err)
	}
	g.printf("  ✔  The cluster now uses Longhorn. New volumes get one Longhorn copy, and the platform's own volumes stay on local storage.\n")
	if len(volumes) > 0 {
		g.printf("\n  These service volumes are still on local storage. Move each one when it suits you:\n")
		for _, v := range volumes {
			g.printf("    %s/%s (%s used): kip service storage longhorn %s%s\n", v.Namespace, v.Service, formatBytes(v.UsedBytes), v.Service, v.Flags)
		}
	}
	return nil
}

func (g StorageGrowth) stampStorageRestarts() {
	machineID, err := ReadMachineID(g.Server)
	if err == nil {
		var node string
		if node, err = NodeNameForMachine(g.Server, machineID); err == nil {
			err = StampStorageRestarts(g.Server, node, machineID)
		}
	}
	if err != nil {
		g.printf("       (could not record host configuration: %v)\n", err)
	}
}

// requireStorageMoveSupport refuses a cluster whose platform predates the
// storage move: its PlatformConfig would refuse the new mode, and nothing
// would keep pods off a volume during a move.
func requireStorageMoveSupport(server CommandRunner) error {
	out, err := server.Run("kubectl get validatingadmissionpolicy " + platform.StorageMovePodsPolicy + " --ignore-not-found -o name")
	if err != nil {
		return fmt.Errorf("checking the cluster's storage move support: %w", err)
	}
	if !strings.Contains(out, platform.StorageMovePodsPolicy) {
		return fmt.Errorf("this cluster's platform is older than this kip. Run 'kip upgrade' first, then run this command again")
	}
	return nil
}

// longhornInstanceManagerCPUPercent is Longhorn's default
// guaranteed-instance-manager-cpu: the share of each node's allocatable CPU
// that its instance manager requests.
const longhornInstanceManagerCPUPercent = 12

// checkLonghornCPU refuses a cluster where a node has no room for Longhorn's
// instance manager.
func checkLonghornCPU(server CommandRunner) error {
	nodes, pods, namespaces, err := readCPUFacts(server)
	if err != nil {
		return err
	}
	return longhornCPURefusal(nodes, pods, namespaces)
}

func readCPUFacts(server CommandRunner) ([]corev1.Node, []corev1.Pod, []corev1.Namespace, error) {
	var nodes corev1.NodeList
	var pods corev1.PodList
	var namespaces corev1.NamespaceList
	for _, l := range []struct {
		what, command string
		into          any
	}{
		{"the nodes", kubeNodeQuery, &nodes},
		{"the pods", "kubectl get pods -A --request-timeout=30s -o json", &pods},
		{"the namespaces", "kubectl get namespaces --request-timeout=30s -o json", &namespaces},
	} {
		if err := kubectlJSON(server, l.what, l.command, l.into); err != nil {
			return nil, nil, nil, fmt.Errorf("measuring CPU for Longhorn: %w", err)
		}
	}
	return nodes.Items, pods.Items, namespaces.Items, nil
}

// longhornCPURefusal returns an error for the first node whose unrequested CPU
// is below the default instance manager request. Pods in longhorn-system are
// excluded to avoid counting an existing reservation twice after a partial
// install. Namespace labels identify each service's project and environment
// for the suggested commands.
func longhornCPURefusal(nodes []corev1.Node, pods []corev1.Pod, namespaces []corev1.Namespace) error {
	others := make([]corev1.Pod, 0, len(pods))
	for _, p := range pods {
		if p.Namespace != "longhorn-system" {
			others = append(others, p)
		}
	}
	for _, r := range instanceManagerRooms(nodes, others) {
		if r.free < r.need {
			return fmt.Errorf("%s has %dm CPU that no pod requests%s, and Longhorn's storage engine requests %dm there (%d%% of the node's %dm). Services moved onto Longhorn could not start again. %s, then run this command again",
				r.node, max(r.free, 0), r.after(), r.need, longhornInstanceManagerCPUPercent, r.allocatable, cpuRemedy(r.node, r.need-r.free, others, namespaces))
		}
	}
	return nil
}

// instanceManagerRoom holds a node's unrequested CPU, default instance manager
// request, and existing instance manager requests, all in millicores.
type instanceManagerRoom struct {
	node                    string
	free, need, allocatable int64
	pending                 []string
	managers                []int64
}

// after returns a sentence fragment naming pending pods counted against the
// available CPU, or an empty string when there are none.
func (r instanceManagerRoom) after() string {
	if len(r.pending) == 0 {
		return ""
	}
	return " once the pending pods that could start there (" + strings.Join(r.pending, ", ") + ") take their share"
}

// instanceManagerRooms measures available CPU on every supplied node, including
// cordoned nodes, and rounds the default instance manager request to millicores.
// It subtracts assigned, unfinished pods and unscheduled pending pods that could
// run on the node and individually fit its available CPU and memory. Pending
// pods count even if they cannot all fit together.
func instanceManagerRooms(nodes []corev1.Node, pods []corev1.Pod) []instanceManagerRoom {
	rooms := make([]instanceManagerRoom, 0, len(nodes))
	for i := range nodes {
		n := service.Headroom(nodes[i:i+1], pods)[0]
		pending, cpu, _ := pendingThatFit(pods, &nodes[i], n, func(*corev1.Pod) bool { return false })
		rooms = append(rooms, instanceManagerRoom{
			node:        n.Node,
			free:        n.CPUMilli - cpu,
			need:        int64(math.Round(float64(n.AllocatableCPUMilli) * longhornInstanceManagerCPUPercent / 100)),
			allocatable: n.AllocatableCPUMilli,
			pending:     pending,
			managers:    instanceManagerRequests(n.Node, pods),
		})
	}
	return rooms
}

// instanceManagerRequests returns CPU requests in millicores for unfinished
// instance manager pods assigned to node, in input order.
func instanceManagerRequests(node string, pods []corev1.Pod) []int64 {
	var requests []int64
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName != node || p.Labels["longhorn.io/component"] != "instance-manager" || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		cpu, _ := platform.PodRequests(&p.Spec)
		requests = append(requests, cpu)
	}
	return requests
}

// minSuggestedServiceCPUMilli is the lowest CPU cpuRemedy suggests for a service.
const minSuggestedServiceCPUMilli = 100

// cpuRemedy suggests lowering the CPU of Kipper services on the node, largest
// first and none below minSuggestedServiceCPUMilli, until short millicores
// are freed. Only services whose namespace names its project are suggested,
// with the --project and --environment flags that reach them. Without such a
// plan it falls back to the general advice.
func cpuRemedy(node string, short int64, pods []corev1.Pod, namespaces []corev1.Namespace) string {
	type candidate struct {
		namespace, name, flags string
		cpu                    int64
	}
	flags := serviceFlags(namespaces)
	var services []candidate
	for i := range pods {
		p := &pods[i]
		name := p.Labels["app"]
		if p.Spec.NodeName != node || name == "" || p.Labels[labels.ServiceType] == "" || flags[p.Namespace] == "" || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, c := range p.Spec.Containers {
			if c.Name == name {
				services = append(services, candidate{p.Namespace, name, flags[p.Namespace], c.Resources.Requests.Cpu().MilliValue()})
			}
		}
	}
	sort.Slice(services, func(i, j int) bool {
		if services[i].cpu != services[j].cpu {
			return services[i].cpu > services[j].cpu
		}
		return services[i].namespace+"/"+services[i].name < services[j].namespace+"/"+services[j].name
	})
	var steps []string
	for _, s := range services {
		if short <= 0 {
			break
		}
		cut := min(short, s.cpu-minSuggestedServiceCPUMilli)
		if cut <= 0 {
			continue
		}
		short -= cut
		steps = append(steps, fmt.Sprintf("%s in %s from %dm to %dm with 'kip service update %s --cpu %dm%s'", s.name, s.namespace, s.cpu, s.cpu-cut, s.name, s.cpu-cut, s.flags))
	}
	if short > 0 || len(steps) == 0 {
		return "Lower a service's CPU with 'kip service update <name> --cpu <cpu> --project <project>', or give the node more CPU"
	}
	return "Lower " + strings.Join(steps, " and ") + ", or give the node more CPU"
}

// serviceFlags maps each namespace that names its project to the
// --project and --environment flags that reach its services.
func serviceFlags(namespaces []corev1.Namespace) map[string]string {
	flags := map[string]string{}
	for _, ns := range namespaces {
		if f := projectFlags(ns.Labels); f != "" {
			flags[ns.Name] = f
		}
	}
	return flags
}

// projectFlags returns the --project and --environment flags from a
// namespace's project labels, or "" when it names no project.
func projectFlags(nsLabels map[string]string) string {
	project := nsLabels[labels.Project]
	if project == "" {
		return ""
	}
	flags := " --project " + project
	if env := nsLabels[labels.Environment]; env != "" {
		flags += " --environment " + env
	}
	return flags
}

// ServiceFlags returns space-prefixed kip flags for services in namespace.
// It uses the project label and optional environment label. Without a project
// label, it uses the namespace name as --project, which kip also accepts.
func ServiceFlags(namespace string, nsLabels map[string]string) string {
	if f := projectFlags(nsLabels); f != "" {
		return f
	}
	return " --project " + namespace
}

// growthPreflight checks the host for Longhorn: a supported OS outside the
// RHEL 9.7 hold, the RAM floor and Longhorn's minimum free disk.
func growthPreflight(sys SystemInfo, skipFloor bool) (string, error) {
	if err := checkOS(sys); err != nil {
		return "", err
	}
	if RHEL97LevelHost(sys.OS, sys.OSVersion, sys.Kernel) {
		return "", fmt.Errorf("%s %s cannot hold Longhorn volumes. %s", osDisplayName[strings.ToLower(sys.OS)], sys.OSVersion, longhornRHEL97Hold)
	}
	warning, err := CheckLonghornMemory(sys.RAMMB, skipFloor)
	if err != nil {
		return "", err
	}
	if minimalFree := sys.DiskSizeMB * longhornMinimalAvailablePercent / 100; sys.DiskMB <= minimalFree {
		return "", fmt.Errorf("%d MiB are free on the filesystem holding /var/lib/longhorn, and Longhorn keeps more than %d%% of it (%d MiB) free. Free up space or grow the disk, then run this command again",
			sys.DiskMB, longhornMinimalAvailablePercent, minimalFree)
	}
	return warning, nil
}

var growthHardenPayload = regexp.MustCompile(`harden\[(true|false|)\]`)

// hardenDecision reuses the decision of an unfinished run, or audits the
// host and records whether it was clean before any package is installed.
func (g StorageGrowth) hardenDecision() (bool, error) {
	out, err := g.Server.Run(`kubectl get platformconfig platform -o jsonpath='{"harden["}{.metadata.annotations.kipper\.run/storage-growth-harden}{"]"}'`)
	if err != nil {
		return false, fmt.Errorf("reading the hardening decision: %w", err)
	}
	if m := growthHardenPayload.FindStringSubmatch(out); len(m) > 1 && m[1] != "" {
		return m[1] == "true", nil
	}
	audit, err := AuditHost(g.Server)
	if err != nil {
		return false, fmt.Errorf("auditing host: %w", err)
	}
	harden := len(audit.Findings()) == 0
	if !harden {
		g.printf("  ℹ  The host already runs services kip would disable (%s), so it is left as it is.\n", strings.Join(audit.Findings(), "; "))
	}
	if _, err := g.Server.Run(fmt.Sprintf("kubectl annotate --overwrite platformconfig platform %s=%t >/dev/null", StorageGrowthHardenAnnotation, harden)); err != nil {
		return false, fmt.Errorf("recording the hardening decision: %w", err)
	}
	return harden, nil
}

// LocalServiceVolume is a service's data volume still on local storage.
type LocalServiceVolume struct {
	Namespace string
	Service   string
	Claim     string
	UsedBytes int64
	Flags     string
}

// LocalServiceVolumes lists the data claims of Kipper services that use
// local-path, sorted by namespace and service, with their used bytes from
// the server's disk. Missing measurements are reported as zero. Only service
// data claims are included, so platform volumes are excluded.
func LocalServiceVolumes(server CommandRunner) ([]LocalServiceVolume, error) {
	var services struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := kubectlJSON(server, "services", "kubectl get services.kipper.run -A -o json", &services); err != nil {
		return nil, err
	}
	var claims corev1.PersistentVolumeClaimList
	if err := kubectlJSON(server, "claims", "kubectl get pvc -A -o json", &claims); err != nil {
		return nil, err
	}
	local := map[string]bool{}
	for _, c := range claims.Items {
		if c.Spec.StorageClassName != nil && *c.Spec.StorageClassName == platform.LocalPathClass {
			local[c.Namespace+"/"+c.Name] = true
		}
	}
	used, err := localPathUsage(server)
	if err != nil {
		return nil, err
	}
	var namespaces corev1.NamespaceList
	if err := kubectlJSON(server, "namespaces", "kubectl get namespaces -o json", &namespaces); err != nil {
		return nil, err
	}
	nsLabels := make(map[string]map[string]string, len(namespaces.Items))
	for _, ns := range namespaces.Items {
		nsLabels[ns.Name] = ns.Labels
	}
	var out []LocalServiceVolume
	for _, s := range services.Items {
		claim := ServiceDataClaim(s.Metadata.Name)
		key := s.Metadata.Namespace + "/" + claim
		if local[key] {
			f := ServiceFlags(s.Metadata.Namespace, nsLabels[s.Metadata.Namespace])
			out = append(out, LocalServiceVolume{Namespace: s.Metadata.Namespace, Service: s.Metadata.Name, Claim: claim, UsedBytes: used[key], Flags: f})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Service < out[j].Service
	})
	return out, nil
}

// localPathUsage measures each local-path directory, keyed by
// namespace/claim. Directory names are pvc-<uid>_<namespace>_<claim>.
func localPathUsage(server CommandRunner) (map[string]int64, error) {
	out, err := server.Run("timeout 60 du -sk " + localPathRoot + "*/ 2>/dev/null; true")
	if err != nil {
		return nil, fmt.Errorf("measuring local volumes: %w", err)
	}
	used := map[string]int64{}
	for _, line := range strings.Split(out, "\n") {
		size, path, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		kib, err := strconv.ParseInt(size, 10, 64)
		if err != nil {
			continue
		}
		parts := strings.Split(filepath.Base(strings.TrimRight(path, "/")), "_")
		if len(parts) == 3 && strings.HasPrefix(parts[0], "pvc-") {
			used[parts[1]+"/"+parts[2]] += kib * 1024
		}
	}
	return used, nil
}

// RetainedCopy is a local volume kept after its service moved to Longhorn.
// Gone, Unproven, FallbackWaived and ConsistentBackups mirror RetainedCopyDecision.
type RetainedCopy struct {
	PV                string
	Namespace         string
	Service           string
	State             string
	UsedBytes         int64
	MovedAt           time.Time
	Prunable          bool
	Note              string
	Gone              bool
	Unproven          bool
	FallbackWaived    bool
	ConsistentBackups bool
}

// RetainedCopies lists retained local volumes sorted by namespace and service,
// with each moved copy evaluated by DecideRetainedCopy using now and w. Note
// explains why a moved copy is kept or what released it. Sizes and timestamps
// come from move annotations, with zero values for invalid data.
func RetainedCopies(server CommandRunner, now time.Time, w PruneWaivers) ([]RetainedCopy, error) {
	var pvs corev1.PersistentVolumeList
	if err := kubectlJSON(server, "retained volumes", "kubectl get pv -l "+platform.RetainedLocalCopyLabel+"=true -o json", &pvs); err != nil {
		return nil, err
	}
	reader := &retainedCopyReader{cluster: KubectlMoveCluster{Server: server}}
	var out []RetainedCopy
	for _, pv := range pvs.Items {
		a := pv.Annotations
		ns, svc, _ := strings.Cut(a[platform.RetainedLocalCopyServiceAnnotation], "/")
		c := RetainedCopy{PV: pv.Name, Namespace: ns, Service: svc, State: a[platform.RetainedLocalCopyStateAnnotation]}
		c.UsedBytes, _ = strconv.ParseInt(a[platform.RetainedLocalCopyUsedBytesAnnotation], 10, 64)
		c.MovedAt, _ = time.Parse(time.RFC3339, a[platform.RetainedLocalCopyMovedAtAnnotation])
		if c.State == platform.RetainedLocalCopyMoved {
			f, err := reader.facts(a)
			if err != nil {
				return nil, err
			}
			d := DecideRetainedCopy(f, now, w)
			c.Prunable, c.Note, c.Gone, c.Unproven, c.FallbackWaived, c.ConsistentBackups = d.Prunable, d.Note, d.Gone, d.Unproven, d.FallbackWaived, d.ConsistentBackups
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Namespace+"/"+out[i].Service < out[j].Namespace+"/"+out[j].Service })
	return out, nil
}

// PruneRetainedCopy hands a moved copy back to the local-path provisioner,
// which deletes its directory and volume. It decides again from live objects
// immediately before deletion, using DecideRetainedCopy with now and w, and
// refuses a copy that is no longer safe to delete. It polls up to polls times
// and calls sleep(interval) after each
// unsuccessful check. A true result with no error means the volume is gone,
// including when it was already absent. A false result with no error means
// deletion is still pending.
func PruneRetainedCopy(server CommandRunner, pv string, now time.Time, w PruneWaivers, polls int, interval time.Duration, sleep func(time.Duration)) (bool, error) {
	cluster := KubectlMoveCluster{Server: server}
	current, err := cluster.ReadPV(pv)
	if err != nil || current == nil {
		return current == nil, err
	}
	if current.Annotations[platform.RetainedLocalCopyStateAnnotation] != platform.RetainedLocalCopyMoved {
		return false, fmt.Errorf("%s belongs to an unfinished move and is kept", pv)
	}
	f, err := (&retainedCopyReader{cluster: cluster}).facts(current.Annotations)
	if err != nil {
		return false, err
	}
	if d := DecideRetainedCopy(f, now, w); !d.Prunable {
		return false, fmt.Errorf("%s is %s", pv, d.Note)
	}
	if err := cluster.PatchPV(pv, PVPatch{ResourceVersion: current.ResourceVersion, Reclaim: "Delete"}); err != nil {
		return false, err
	}
	for i := 0; i < polls; i++ {
		v, err := cluster.ReadPV(pv)
		if err != nil {
			return false, err
		}
		if v == nil {
			return true, nil
		}
		sleep(interval)
	}
	return false, nil
}
