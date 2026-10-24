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

	"github.com/getkipper/kipper/controller/pkg/platform"
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
			g.printf("    %s/%s (%s used): kip service storage longhorn %s --project %s\n", v.Namespace, v.Service, formatBytes(v.UsedBytes), v.Service, v.Namespace)
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
	var out []LocalServiceVolume
	for _, s := range services.Items {
		claim := ServiceDataClaim(s.Metadata.Name)
		key := s.Metadata.Namespace + "/" + claim
		if local[key] {
			out = append(out, LocalServiceVolume{Namespace: s.Metadata.Namespace, Service: s.Metadata.Name, Claim: claim, UsedBytes: used[key]})
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
type RetainedCopy struct {
	PV        string
	Namespace string
	Service   string
	State     string
	UsedBytes int64
	MovedAt   time.Time
	Prunable  bool
	Note      string
}

// RetainedCopies lists retained local volumes sorted by namespace and service.
// Prunable requires a finished move with the verified Longhorn claim still
// bound, or both the service and its data claim to be gone. Note explains why
// a moved copy is kept or records that its service was deleted. Sizes and
// timestamps come from move annotations, with zero values for invalid data.
func RetainedCopies(server CommandRunner) ([]RetainedCopy, error) {
	var pvs corev1.PersistentVolumeList
	if err := kubectlJSON(server, "retained volumes", "kubectl get pv -l "+platform.RetainedLocalCopyLabel+"=true -o json", &pvs); err != nil {
		return nil, err
	}
	cluster := KubectlMoveCluster{Server: server}
	var out []RetainedCopy
	for _, pv := range pvs.Items {
		a := pv.Annotations
		ns, svc, _ := strings.Cut(a[platform.RetainedLocalCopyServiceAnnotation], "/")
		c := RetainedCopy{PV: pv.Name, Namespace: ns, Service: svc, State: a[platform.RetainedLocalCopyStateAnnotation]}
		c.UsedBytes, _ = strconv.ParseInt(a[platform.RetainedLocalCopyUsedBytesAnnotation], 10, 64)
		c.MovedAt, _ = time.Parse(time.RFC3339, a[platform.RetainedLocalCopyMovedAtAnnotation])
		if c.State == platform.RetainedLocalCopyMoved {
			var err error
			if c.Prunable, c.Note, err = retainedCopyVerdict(cluster, ns, svc, a[retainedDestinationAnnotation]); err != nil {
				return nil, err
			}
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Namespace+"/"+out[i].Service < out[j].Namespace+"/"+out[j].Service })
	return out, nil
}

// retainedCopyVerdict decides from live objects whether a moved copy can be
// deleted. The service's data claim must still be the bound Longhorn claim
// the move verified, by UID, and the move must have finished, or the service
// and its claim must both be gone.
// Otherwise the copy may be the only data left, and the note says why it is
// kept.
func retainedCopyVerdict(cluster KubectlMoveCluster, namespace, service, destination string) (bool, string, error) {
	if namespace == "" || service == "" {
		return false, "kept: the copy names no service", nil
	}
	claim, err := cluster.ReadClaim(namespace, ServiceDataClaim(service))
	if err != nil {
		return false, "", err
	}
	switch {
	case claim == nil:
		svc, err := cluster.ReadService(namespace, service)
		if err != nil {
			return false, "", err
		}
		if svc != nil {
			return false, "kept: the service has no data volume, so this copy may be the only one left", nil
		}
		return true, "its service was deleted", nil
	case claim.Class == platform.LocalPathClass:
		return false, "kept: the service's data volume is on local storage", nil
	case !platform.IsLonghornClass(claim.Class) || claim.Phase != "Bound" || claim.Deleting:
		return false, "kept: the service's data volume is not a bound Longhorn volume", nil
	case destination == "":
		return false, "kept: the move recorded no verified data volume to compare with", nil
	case claim.UID != destination:
		return false, "kept: the service's data volume was replaced after the move, so this copy may hold the only original data", nil
	}
	svc, err := cluster.ReadService(namespace, service)
	if err != nil {
		return false, "", err
	}
	if svc != nil && svc.Mark != "" {
		return false, fmt.Sprintf("kept: its move is unfinished. Finish it with 'kip service storage longhorn %s --project %s'", service, namespace), nil
	}
	return true, "", nil
}

// PruneRetainedCopy hands a moved copy back to the local-path provisioner,
// which deletes its directory and volume. It decides again from live objects
// immediately before deletion and refuses a copy that is no longer safe to
// delete. It polls up to polls times, calling sleep(interval) after each
// unsuccessful check. A true result with no error means the volume is gone,
// including when it was already absent. A false result with no error means
// deletion is still pending.
func PruneRetainedCopy(server CommandRunner, pv string, polls int, interval time.Duration, sleep func(time.Duration)) (bool, error) {
	cluster := KubectlMoveCluster{Server: server}
	current, err := cluster.ReadPV(pv)
	if err != nil || current == nil {
		return current == nil, err
	}
	if current.Annotations[platform.RetainedLocalCopyStateAnnotation] != platform.RetainedLocalCopyMoved {
		return false, fmt.Errorf("%s belongs to an unfinished move and is kept", pv)
	}
	ns, svc, _ := strings.Cut(current.Annotations[platform.RetainedLocalCopyServiceAnnotation], "/")
	prunable, note, err := retainedCopyVerdict(cluster, ns, svc, current.Annotations[retainedDestinationAnnotation])
	if err != nil {
		return false, err
	}
	if !prunable {
		return false, fmt.Errorf("%s is %s", pv, note)
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
