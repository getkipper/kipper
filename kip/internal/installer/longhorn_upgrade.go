package installer

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// LonghornUpgradeCommand is the command that moves Longhorn to the target.
const LonghornUpgradeCommand = "kip upgrade --longhorn"

// LonghornUpgrade moves an existing Longhorn one minor at a time to the
// release this kip installs. Before every step it checks the cluster, takes a
// Velero backup through Backup, which must return only once the backup has
// completed, and writes Longhorn's volumes, replicas, engines and settings
// under ExportDir. A recorded version change resumes with its existing backup
// and export once either version marker shows the target. Prepared steps and
// engine catch-ups restart with fresh checks, a backup and an export.
// Backup must be non-nil when a new
// step is needed and return the completed backup name. ExportDir must be
// set for new steps.
// StepTimeout bounds each settling wait and Interval sets the polling interval.
// Zero values use 45 minutes and 10 seconds, respectively.
// A nil Out discards progress messages.
type LonghornUpgrade struct {
	Out       io.Writer
	Backup    func(to string) (string, error)
	ExportDir string

	StepTimeout time.Duration
	Interval    time.Duration
	now         func() time.Time
}

const (
	defaultLonghornStepTimeout = 45 * time.Minute
	defaultLonghornInterval    = 10 * time.Second
	engineUpgradeSetting       = "concurrent-automatic-engine-upgrade-per-node-limit"
)

// Apply runs the upgrade on the server reached through client. It refuses a
// host without Longhorn, and stops at the first step that does not pass its
// checks, naming what to fix. Completed steps remain applied. It installs
// host storage dependencies and changes Longhorn manifests and settings.
// It disables automatic engine upgrades before host preparation and attempts
// to disable them again on return. Cleanup failures are reported through Out.
func (u LonghornUpgrade) Apply(client CommandRunner) error {
	if u.StepTimeout == 0 {
		u.StepTimeout = defaultLonghornStepTimeout
	}
	if u.Interval == 0 {
		u.Interval = defaultLonghornInterval
	}
	if u.now == nil {
		u.now = time.Now
	}
	if u.Out == nil {
		u.Out = io.Discard
	}
	// An interrupted run can leave engine upgrades enabled. Disable them before
	// host preparation to keep upgrades within the backed-up engine phase,
	// and attempt cleanup on every return.
	defer u.stopEngineUpgrades(client)
	markers, err := ReadLonghornMarkers(client)
	if err != nil {
		return err
	}
	version, fresh, err := LonghornVersionToApply(markers)
	if err != nil {
		return err
	}
	if fresh {
		return fmt.Errorf("%s has nothing to upgrade, because Longhorn is not installed on this cluster", LonghornUpgradeCommand)
	}
	if err := u.setEngineUpgrades(client, "0"); err != nil {
		return err
	}
	host, err := DetectHostOS(client)
	if err != nil {
		return fmt.Errorf("installing longhorn dependencies: %w", err)
	}
	if err := installStorageDependencies(client, host, nil); err != nil {
		return err
	}

	steps := 0
	if rec := markers.Record; rec != nil {
		switch {
		case markers.HopStarted():
			say(u.Out, "       resuming the unfinished Longhorn step from %s to %s (backup %s)\n", rec.From, rec.To, orUnknown(rec.Backup))
			if err := u.resumeGate(client, rec.From, rec.To); err != nil {
				return err
			}
			if err := applyLonghornManifest(client, rec.To); err != nil {
				return err
			}
			if err := u.settle(client, rec.To); err != nil {
				return err
			}
			say(u.Out, "       Longhorn %s → %s: settled\n", rec.From, rec.To)
			steps++
		case rec.From == rec.To:
			say(u.Out, "       the engine catch-up on Longhorn %s did not finish. Starting it again with its checks and a new backup\n", rec.From)
		default:
			say(u.Out, "       the Longhorn step from %s to %s was recorded but never applied. Starting it again with its checks and a new backup\n", rec.From, rec.To)
		}
		if err := deleteLonghornHopRecord(client); err != nil {
			return err
		}
	}
	plan, err := PlanLonghornUpgrade(version)
	if err != nil {
		return err
	}
	snap, err := u.waitFor(client, version, "Longhorn "+version+"'s components", func(s longhornSnapshot) []string { return s.componentProblems(version) })
	if err != nil {
		return err
	}
	if lagging, _ := snap.engineLag(); len(lagging) > 0 {
		say(u.Out, "       %d %s not on Longhorn %s's engine yet. Upgrading them first\n", len(lagging), plural(len(lagging), "volume is", "volumes are"), version)
		if err := u.step(client, version, version); err != nil {
			return err
		}
		steps++
	}
	current := version
	for _, hop := range plan.Hops {
		if err := u.step(client, current, hop); err != nil {
			return err
		}
		current = hop
		steps++
	}
	if steps == 0 {
		if err := installLonghorn(client, host, nil); err != nil {
			return err
		}
		u.pruneEngineImages(client)
		return nil
	}
	if snap, err := readLonghornSnapshot(client); err == nil {
		if _, exempt := snap.engineLag(); len(exempt) > 0 {
			say(u.Out, "       %s %s more replicas than the cluster has nodes, so %s older engine, which Longhorn %s still supports. Longhorn moves a volume onto the current engine once it has a node for every replica: add nodes with kip node add, then run %s again\n",
				strings.Join(exempt, ", "), plural(len(exempt), "wants", "want"), plural(len(exempt), "it keeps its", "they keep their"), current, LonghornUpgradeCommand)
		}
	}
	if _, err := tagServerStorage(client, serverStorageWait); err != nil {
		return err
	}
	return ensureStorageClasses(client, serverStorageWait)
}

// step moves Longhorn from one release to the next, or, with from equal to
// to, moves lagging volumes onto the applied release's engine.
func (u LonghornUpgrade) step(client CommandRunner, from, to string) error {
	what := fmt.Sprintf("Longhorn %s → %s", from, to)
	if from == to {
		what = fmt.Sprintf("the engine catch-up on Longhorn %s", from)
	}
	say(u.Out, "       %s: checking the cluster\n", what)
	if err := u.gate(client, from, to); err != nil {
		return err
	}
	say(u.Out, "       %s: taking a backup\n", what)
	backup, err := u.Backup(to)
	if err != nil {
		return fmt.Errorf("the backup before %s: %w. Nothing in Longhorn was changed", what, err)
	}
	say(u.Out, "       %s: backup %s completed\n", what, backup)
	// The cluster may change during a long backup, so check it again
	// before applying the manifest.
	if err := u.gate(client, from, to); err != nil {
		return err
	}
	dir, err := u.export(client, from, to)
	if err != nil {
		return err
	}
	say(u.Out, "       %s: Longhorn's volumes, replicas, engines and settings saved to %s\n", what, dir)
	if err := writeLonghornHopRecord(client, LonghornHopRecord{From: from, To: to, Started: u.now().UTC().Format(time.RFC3339), Backup: backup, Export: dir}); err != nil {
		return err
	}
	say(u.Out, "       %s: applying Longhorn %s\n", what, to)
	if err := applyLonghornManifest(client, to); err != nil {
		return fmt.Errorf("%w. Run %s again to finish it", err, LonghornUpgradeCommand)
	}
	if err := u.settle(client, to); err != nil {
		return err
	}
	if err := deleteLonghornHopRecord(client); err != nil {
		return err
	}
	say(u.Out, "       %s: settled\n", what)
	return nil
}

// gate returns an error naming every reason the step must not start: the
// cluster not ready or settled at from, the hop's own requirements, and a
// host at the RHEL 9.7 level. Version changes also require enough unrequested
// CPU for another instance manager on every node.
func (u LonghornUpgrade) gate(client CommandRunner, from, to string) error {
	snap, err := readLonghornSnapshot(client)
	if err != nil {
		return err
	}
	var problems []string
	if from == to {
		problems = snap.readyProblems(from)
	} else {
		problems = snap.settledProblems(from)
	}
	if from != to {
		more, err := hopRequirementProblems(client, snap, to)
		if err != nil {
			return err
		}
		problems = append(problems, more...)
		nodes, pods, namespaces, err := readCPUFacts(client)
		if err != nil {
			return err
		}
		problems = append(problems, snap.hopCPUProblems(nodes, pods, namespaces)...)
	}
	problems = append(problems, snap.rhelHoldProblems()...)
	if len(problems) == 0 {
		return nil
	}
	step := fmt.Sprintf("Longhorn %s → %s", from, to)
	if from == to {
		step = fmt.Sprintf("the engine catch-up on Longhorn %s", from)
	}
	return fmt.Errorf("%s did not start, and nothing in Longhorn was changed:\n  - %s\nFix these, or wait for Longhorn to finish what it is doing, then run %s again", step, strings.Join(problems, "\n  - "), LonghornUpgradeCommand)
}

// hopCPUProblems reports nodes with insufficient unrequested CPU for another
// instance manager. Longhorn binds its pod to the node, where insufficient CPU
// prevents admission without evicting other pods, so the step times out. Unlike
// the installation check, this counts existing managers as used CPU until their
// engines and replicas stop. An attached volume's engine stays until it detaches.
func (s longhornSnapshot) hopCPUProblems(nodes []corev1.Node, pods []corev1.Pod, namespaces []corev1.Namespace) []string {
	var problems []string
	for _, r := range instanceManagerRooms(nodes, pods) {
		need := s.instanceManagerNeed(r)
		if r.free >= need {
			continue
		}
		problem := fmt.Sprintf("%s has %dm CPU that no pod requests%s, and during the upgrade Longhorn starts another instance manager there that requests %dm. The node would refuse it, so the step would time out with volumes still on the old engine.",
			r.node, max(r.free, 0), r.after(), need)
		if len(r.managers) > 1 {
			problem += fmt.Sprintf(" %d instance managers run there because an older one keeps the engines of volumes that stayed attached through an earlier step. Those engines stay until the volumes detach. Longhorn removes the older manager once everything using those volumes, including backup holders, has stopped.", len(r.managers))
		}
		problems = append(problems, problem+" "+cpuRemedy(r.node, need-r.free, pods, namespaces))
	}
	return problems
}

// instanceManagerNeed returns the CPU request in millicores of the instance
// manager Longhorn starts on r's node. Without a readable setting or override it
// uses the largest request among unfinished instance manager pods on that node,
// or the default percentage of allocatable CPU when there are none.
func (s longhornSnapshot) instanceManagerNeed(r instanceManagerRoom) int64 {
	if need, ok := s.nextInstanceManagerCPU(r.node, r.allocatable); ok {
		return need
	}
	if len(r.managers) > 0 {
		return slices.Max(r.managers)
	}
	return r.need
}

// instanceManagerCPUHint returns newline-prefixed CPU shortage hints for
// in-service Longhorn nodes still waiting for the default instance manager.
// It skips nodes with an admitted manager pod or enough CPU and returns an
// empty string if reads fail or no hints apply.
func instanceManagerCPUHint(client CommandRunner, snap longhornSnapshot) string {
	missing := snap.nodesWithoutInstanceManager()
	if len(missing) == 0 {
		return ""
	}
	nodes, pods, namespaces, err := readCPUFacts(client)
	if err != nil {
		return ""
	}
	pods, admitted := withoutNewInstanceManagers(pods, snap.Settings["default-instance-manager-image"])
	var hints []string
	for _, r := range instanceManagerRooms(nodes, pods) {
		need := snap.instanceManagerNeed(r)
		if !slices.Contains(missing, r.node) || admitted[r.node] || r.free >= need {
			continue
		}
		hints = append(hints, fmt.Sprintf("%s has %dm CPU that no pod requests%s, and Longhorn's new instance manager there requests %dm. The node refuses a pod bound to it without that CPU, which would keep the instance manager from starting. %s",
			r.node, max(r.free, 0), r.after(), need, cpuRemedy(r.node, need-r.free, pods, namespaces)))
	}
	if len(hints) == 0 {
		return ""
	}
	return "\n" + strings.Join(hints, "\n")
}

// withoutNewInstanceManagers filters out unfinished instance manager pods using
// image whose data engine is not v2, so their requests do not inflate the CPU
// shortage estimate. It also returns nodes considered to have an admitted pod
// because the pod is Running or has container statuses.
func withoutNewInstanceManagers(pods []corev1.Pod, image string) ([]corev1.Pod, map[string]bool) {
	rest := make([]corev1.Pod, 0, len(pods))
	admitted := map[string]bool{}
	for i := range pods {
		p := &pods[i]
		finished := p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
		v1 := p.Labels["longhorn.io/data-engine"] != "v2"
		if p.Labels["longhorn.io/component"] != "instance-manager" || !v1 || finished || !runsImage(p, image) {
			rest = append(rest, *p)
			continue
		}
		if p.Status.Phase == corev1.PodRunning || len(p.Status.ContainerStatuses) > 0 {
			admitted[p.Spec.NodeName] = true
		}
	}
	return rest, admitted
}

// runsImage reports whether a regular container in p specifies image exactly.
// An empty image never matches.
func runsImage(p *corev1.Pod, image string) bool {
	for _, c := range p.Spec.Containers {
		if image != "" && c.Image == image {
			return true
		}
	}
	return false
}

// nextInstanceManagerCPU returns a new v1 instance manager's CPU request in
// millicores. A positive node override takes precedence over the rounded
// guaranteed-instance-manager-cpu percentage of allocatable CPU. The setting
// accepts a number or, starting with Longhorn 1.10, a per-data-engine object.
// Without an override, the result is false if the setting is missing,
// unreadable or negative.
func (s longhornSnapshot) nextInstanceManagerCPU(node string, allocatable int64) (int64, bool) {
	if override := s.IMCPUOverrides[node]; override > 0 {
		return override, true
	}
	percent, err := strconv.ParseFloat(v1SettingValue(s.Settings["guaranteed-instance-manager-cpu"]), 64)
	if err != nil || percent < 0 {
		return 0, false
	}
	return int64(math.Round(float64(allocatable) * percent / 100)), true
}

// resumeGate checks volume, engine, Kubernetes, CRD, node and host requirements
// before resuming a partly applied hop. It allows components to be between
// releases by skipping readiness checks at from. Read failures return immediately.
// Otherwise the error lists all failed requirements.
func (u LonghornUpgrade) resumeGate(client CommandRunner, from, to string) error {
	snap, err := readLonghornSnapshot(client)
	if err != nil {
		return err
	}
	problems := snap.carryProblems()
	more, err := hopRequirementProblems(client, snap, to)
	if err != nil {
		return err
	}
	problems = append(problems, more...)
	problems = append(problems, snap.outOfServiceProblems()...)
	problems = append(problems, snap.rhelHoldProblems()...)
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("the unfinished Longhorn %s → %s was not resumed, and nothing more in Longhorn was changed:\n  - %s\nFix these, then run %s again", from, to, strings.Join(problems, "\n  - "), LonghornUpgradeCommand)
}

// hopRequirementProblems lists what release to requires of the cluster before
// its manifest is applied: compatible engines, a supported Kubernetes version,
// and, for v1.10, CRDs whose stored versions exclude v1beta1.
func hopRequirementProblems(client CommandRunner, snap longhornSnapshot, to string) ([]string, error) {
	release, ok := longhornReleaseFor(to)
	if !ok {
		return nil, fmt.Errorf("kip has no upgrade path entry for Longhorn %s", to)
	}
	problems := snap.hopProblems(release)
	if v, _ := parseLonghornVersion(to); v.minor == 10 {
		stale, err := crdsStoringV1beta1(client)
		if err != nil {
			return nil, err
		}
		for _, crd := range stale {
			problems = append(problems, fmt.Sprintf("the CRD %s still lists v1beta1 in status.storedVersions. Longhorn %s removes v1beta1, so its objects must first be migrated as the Longhorn v1.10.0 release notes describe", crd, to))
		}
	}
	return problems, nil
}

// settle waits for Longhorn's components at version, lets Longhorn move every
// volume it can onto version's engine, and waits for the whole cluster to settle.
func (u LonghornUpgrade) settle(client CommandRunner, version string) error {
	if _, err := u.waitFor(client, version, "Longhorn "+version+"'s components", func(s longhornSnapshot) []string { return s.componentProblems(version) }); err != nil {
		return err
	}
	if err := u.setEngineUpgrades(client, "1"); err != nil {
		return err
	}
	if _, err := u.waitFor(client, version, "every volume on Longhorn "+version, func(s longhornSnapshot) []string { return s.settledProblems(version) }); err != nil {
		return err
	}
	if err := u.setEngineUpgrades(client, "0"); err != nil {
		return err
	}
	u.pruneEngineImages(client)
	return nil
}

// pruneEngineImages deletes images older than the running release with no
// volume references and a zero reference count, keeping the default image.
// Longhorn deploys each image on every node. Cleanup requires Longhorn v1.10
// or later in major version 1, whose admission webhook rejects deletion of
// images in use to guard against a new reference racing the delete.
// Failures are reported through Out.
func (u LonghornUpgrade) pruneEngineImages(client CommandRunner) {
	snap, err := readLonghornSnapshot(client)
	if err != nil {
		say(u.Out, "  ⚠  could not read Longhorn's engine images to remove unused ones (%v)\n", err)
		return
	}
	running, err := parseLonghornVersion(snap.Settings["current-longhorn-version"])
	if err != nil || running.major != 1 || running.minor < 10 {
		return
	}
	used := map[string]bool{snap.Settings["default-engine-image"]: true}
	for _, v := range snap.Volumes {
		used[v.Image], used[v.CurrentImage] = true, true
	}
	for _, ei := range snap.EngineImages {
		_, tag, _ := strings.Cut(ei.Image, ":")
		v, err := parseLonghornVersion(tag)
		if used[ei.Image] || ei.RefCount != 0 || err != nil || !v.less(running) {
			continue
		}
		if _, err := client.Run(fmt.Sprintf("kubectl -n longhorn-system delete engineimages.longhorn.io %s --request-timeout=60s >/dev/null", ei.Name)); err != nil {
			say(u.Out, "  ⚠  could not remove the unused engine image %s (%v)\n", ei.Image, err)
			continue
		}
		say(u.Out, "       removed the unused engine image %s\n", ei.Image)
	}
}

// waitFor polls the snapshot until problems returns none or StepTimeout passes.
// A read error counts as not settled until the time is up.
func (u LonghornUpgrade) waitFor(client CommandRunner, version, what string, problems func(longhornSnapshot) []string) (longhornSnapshot, error) {
	deadline := u.now().Add(u.StepTimeout)
	last := ""
	for {
		snap, err := readLonghornSnapshot(client)
		var left []string
		if err != nil {
			left = []string{err.Error()}
		} else {
			left = problems(snap)
		}
		if len(left) == 0 {
			return snap, nil
		}
		if summary := strings.Join(left, "; "); summary != last {
			say(u.Out, "       waiting for %s: %s\n", what, left[0])
			last = summary
		}
		if !u.now().Before(deadline) {
			return snap, fmt.Errorf("%s did not settle within %s:\n  - %s%s\nRun %s again to keep waiting. It resumes where this run stopped",
				what, u.StepTimeout, strings.Join(left, "\n  - "), managerLogTail(client, version, left)+instanceManagerCPUHint(client, snap), LonghornUpgradeCommand)
		}
		time.Sleep(u.Interval)
	}
}

// managerLogTail adds the last 20 log lines when a problem mentions
// longhorn-manager, to help explain why the upgrade has stalled. Failed or
// empty log reads return an empty string.
func managerLogTail(client CommandRunner, version string, problems []string) string {
	for _, p := range problems {
		if strings.Contains(p, "longhorn-manager") {
			out, err := client.Run("kubectl -n longhorn-system logs daemonset/longhorn-manager -c longhorn-manager --tail=20 --request-timeout=30s 2>&1")
			if err != nil || strings.TrimSpace(out) == "" {
				return ""
			}
			return fmt.Sprintf("\nlonghorn-manager's last log lines (it logs why it refuses %s):\n%s", version, strings.TrimRight(out, "\n"))
		}
	}
	return ""
}

func (u LonghornUpgrade) setEngineUpgrades(client CommandRunner, limit string) error {
	cmd := fmt.Sprintf(`kubectl -n longhorn-system patch settings.longhorn.io %s --request-timeout=30s --type merge -p '{"value":%q}' >/dev/null`, engineUpgradeSetting, limit)
	deadline := u.now().Add(u.StepTimeout)
	for {
		_, err := client.Run(cmd)
		if err == nil {
			return nil
		}
		// Longhorn's admission webhook is unavailable while its manager restarts.
		if !u.now().Before(deadline) {
			return fmt.Errorf("setting Longhorn's %s to %s: %w. Run %s again", engineUpgradeSetting, limit, err, LonghornUpgradeCommand)
		}
		time.Sleep(u.Interval)
	}
}

func (u LonghornUpgrade) stopEngineUpgrades(client CommandRunner) {
	cmd := fmt.Sprintf(`kubectl -n longhorn-system patch settings.longhorn.io %s --request-timeout=30s --type merge -p '{"value":"0"}' >/dev/null`, engineUpgradeSetting)
	if _, err := client.Run(cmd); err != nil {
		say(u.Out, "  ⚠  could not set Longhorn's %s back to 0 (%v). Longhorn may keep moving engines onto its default engine until %s runs again and gets past its checks\n", engineUpgradeSetting, err, LonghornUpgradeCommand)
	}
}

// longhornExportKinds are the Longhorn objects saved before every step.
var longhornExportKinds = []string{"volumes", "replicas", "engines", "settings"}

// export saves Longhorn's objects as YAML under ExportDir and returns the
// timestamped directory. New directories and files are owner-only. A retry
// within the same second can reuse the directory and overwrite its files.
func (u LonghornUpgrade) export(client CommandRunner, from, to string) (string, error) {
	if u.ExportDir == "" {
		return "", fmt.Errorf("no directory to save Longhorn's objects to before the upgrade")
	}
	dir := filepath.Join(u.ExportDir, fmt.Sprintf("%s-%s-to-%s", u.now().UTC().Format("20060102T150405Z"), from, to))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	for _, kind := range longhornExportKinds {
		out, err := client.Run(fmt.Sprintf("kubectl -n longhorn-system get %s.longhorn.io --request-timeout=60s -o yaml", kind))
		if err != nil {
			return "", fmt.Errorf("saving Longhorn's %s before the upgrade: %w", kind, err)
		}
		path := filepath.Join(dir, kind+".yaml")
		if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
			return "", fmt.Errorf("writing %s: %w", path, err)
		}
	}
	return dir, nil
}

// hopProblems lists what keeps a manifest hop to release from starting: a
// volume with a missing or incompatible EngineImage, or a node with an
// unreadable Kubernetes version or one below the release's minimum.
func (s longhornSnapshot) hopProblems(release longhornRelease) []string {
	var problems []string
	for _, v := range s.Volumes {
		seen := map[string]bool{}
		for _, image := range []string{v.CurrentImage, v.Image} {
			if image == "" || seen[image] {
				continue
			}
			seen[image] = true
			ei := s.engineImage(image)
			switch {
			case ei == nil:
				problems = append(problems, fmt.Sprintf("%s runs the engine image %s, which has no EngineImage, so kip cannot check it against Longhorn %s", v.label(), image, release.Version))
			case ei.CLIAPI < release.EngineCLIAPIMin || ei.Controller < release.EngineControllerAPIMin:
				problems = append(problems, fmt.Sprintf("%s runs the engine image %s (API %d/%d), older than Longhorn %s accepts (%d/%d). Detach it by stopping what uses it, then run %s again, which moves it onto the current engine before the next step",
					v.label(), image, ei.CLIAPI, ei.Controller, release.Version, release.EngineCLIAPIMin, release.EngineControllerAPIMin, LonghornUpgradeCommand))
			}
		}
	}
	problems = append(problems, kubernetesFloorProblems(s.Nodes, release)...)
	return problems
}

var kubeletPattern = regexp.MustCompile(`^v(\d+)\.(\d+)`)

// kubernetesFloorProblems lists nodes whose kubelet is older than the
// release's Kubernetes floor, or whose version kip cannot read.
func kubernetesFloorProblems(nodes []NodeFacts, release longhornRelease) []string {
	floor := kubeletPattern.FindStringSubmatch(release.MinKubernetes)
	if floor == nil {
		return []string{fmt.Sprintf("kip cannot read Longhorn %s's Kubernetes floor %q", release.Version, release.MinKubernetes)}
	}
	fMajor, _ := strconv.Atoi(floor[1])
	fMinor, _ := strconv.Atoi(floor[2])
	var problems []string
	for _, n := range nodes {
		m := kubeletPattern.FindStringSubmatch(n.KubeletVersion)
		if m == nil {
			problems = append(problems, fmt.Sprintf("kip cannot read node %s's Kubernetes version %q", n.Name, n.KubeletVersion))
			continue
		}
		major, _ := strconv.Atoi(m[1])
		minor, _ := strconv.Atoi(m[2])
		if major < fMajor || (major == fMajor && minor < fMinor) {
			problems = append(problems, fmt.Sprintf("node %s runs Kubernetes %s, and Longhorn %s needs %s or later. Run 'kip upgrade --k3s' first", n.Name, n.KubeletVersion, release.Version, release.MinKubernetes))
		}
	}
	return problems
}

// KubernetesProblems names the nodes whose Kubernetes is older than a hop of
// the plan accepts, stopping at the first hop that has any. Unreadable node
// versions and unknown hops also produce problems. Nil means all planned
// hops pass these checks, including when the plan has no hops.
func (p LonghornPlan) KubernetesProblems(nodes []NodeFacts) []string {
	for _, hop := range p.Hops {
		release, ok := longhornReleaseFor(hop)
		if !ok {
			return []string{fmt.Sprintf("kip has no upgrade path entry for Longhorn %s", hop)}
		}
		if problems := kubernetesFloorProblems(nodes, release); len(problems) > 0 {
			return problems
		}
	}
	return nil
}

// rhelHoldProblems names nodes at the RHEL 9.7 package level or later,
// where Longhorn's v1 volumes may stop attaching (Longhorn issue 13606).
func (s longhornSnapshot) rhelHoldProblems() []string {
	return RHEL97LevelProblems(s.Nodes)
}

// RHEL97LevelProblems names each node at the RHEL 9.7 package level or later,
// where Longhorn's v1 volumes may stop attaching (Longhorn issue 13606).
// Results follow the input order. Nil means no nodes match the hold.
func RHEL97LevelProblems(nodes []NodeFacts) []string {
	var problems []string
	for _, n := range nodes {
		if RHEL97LevelNode(n) {
			problems = append(problems, fmt.Sprintf("node %s runs %s (kernel %s). %s", n.Name, n.OSImage, n.Kernel, longhornRHEL97Hold))
		}
	}
	return problems
}

// crdsStoringV1beta1 lists Longhorn CRDs whose status.storedVersions still
// holds v1beta1.
func crdsStoringV1beta1(client CommandRunner) ([]string, error) {
	out, err := client.Run("kubectl get crd --request-timeout=30s -o json")
	if err != nil {
		return nil, fmt.Errorf("reading the Longhorn CRDs' stored versions: %w", err)
	}
	return parseCRDsStoringV1beta1(jsonPayload(out))
}

func parseCRDsStoringV1beta1(data []byte) ([]string, error) {
	var list struct {
		Items []struct {
			Metadata kubeMeta `json:"metadata"`
			Status   struct {
				StoredVersions []string `json:"storedVersions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("reading the Longhorn CRDs' stored versions: %w", err)
	}
	var stale []string
	for _, it := range list.Items {
		if strings.HasSuffix(it.Metadata.Name, ".longhorn.io") && slices.Contains(it.Status.StoredVersions, "v1beta1") {
			stale = append(stale, it.Metadata.Name)
		}
	}
	return stale, nil
}

func say(out io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(out, format, args...)
}
