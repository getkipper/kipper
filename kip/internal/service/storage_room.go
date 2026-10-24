package service

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/getkipper/kipper/controller/pkg/longhorncap"
	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/kip/internal/k8s"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

var (
	longhornNodeGVR    = schema.GroupVersionResource{Group: "longhorn.io", Version: "v1beta2", Resource: "nodes"}
	longhornSettingGVR = schema.GroupVersionResource{Group: "longhorn.io", Version: "v1beta2", Resource: "settings"}
)

const longhornProvisioner = "driver.longhorn.io"

// NewVolume describes a proposed volume for a storage check. On Longhorn,
// counts of at least two use the replicated class. Other values use the
// single-copy class, including shared volumes. Local storage uses local-path.
// CheckStorageRoom skips volumes whose Claim already exists in Namespace.
type NewVolume struct {
	Name      string
	Shared    bool
	Bytes     int64
	Namespace string
	Claim     string
	Copies    int32
}

// class is the StorageClass a new claim for v gets in storage mode.
func (v NewVolume) class(mode string) string {
	if v.Copies == 0 {
		return platform.ClaimClassForCopies(mode, nil)
	}
	return platform.ClaimClassForCopies(mode, &v.Copies)
}

// DefaultStorage is the Service CRD's default for spec.storage, which the API
// server fills in for every type when a command leaves the field out.
const DefaultStorage = "1Gi"

// ServiceVolumes lists the volumes a new service creates: its data volume and,
// for Postgres with consistent backups, a dump volume of the same size. An
// empty storage selects the Service CRD's default size. Invalid Kubernetes
// storage quantities return an error. Sizes are converted to whole bytes,
// rounding fractional bytes away from zero.
func ServiceVolumes(namespace, name, serviceType, storage string, consistentBackups bool) ([]NewVolume, error) {
	if storage == "" {
		storage = DefaultStorage
	}
	size, err := resource.ParseQuantity(storage)
	if err != nil {
		return nil, fmt.Errorf("storage size %q: %w", storage, err)
	}
	volumes := []NewVolume{{Name: "data", Bytes: size.Value(), Namespace: namespace, Claim: "data-" + name + "-0"}}
	if consistentBackups && serviceType == "postgres" {
		volumes = append(volumes, NewVolume{Name: "dump", Bytes: size.Value(), Namespace: namespace, Claim: name + "-dump"})
	}
	return volumes, nil
}

// SharedVolume describes a shared volume backed by the claim "shared-" + name
// in namespace, with the requested size in bytes.
func SharedVolume(namespace, name string, bytes int64) NewVolume {
	return NewVolume{Name: name, Shared: true, Bytes: bytes, Namespace: namespace, Claim: "shared-" + name}
}

// DumpVolume describes the dump volume needed when enabling consistent backups.
// It returns nil when the service is missing, is not Postgres, or already has
// consistent backups enabled. Non-empty storage overrides the service's size
// to reflect a size change in the same command. Otherwise, it uses the service's
// storage size, falling back to DefaultStorage when empty. Read failures and
// invalid storage quantities return an error.
func (m *Manager) DumpVolume(ctx context.Context, namespace, name, storage string) ([]NewVolume, error) {
	svc, err := m.Dynamic.Resource(manifest.ServiceGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading service %s: %w", name, err)
	}
	serviceType, _, _ := unstructured.NestedString(svc.Object, "spec", "type")
	on, _, _ := unstructured.NestedBool(svc.Object, "spec", "consistentBackups")
	if serviceType != "postgres" || on {
		return nil, nil
	}
	if storage == "" {
		storage, _, _ = unstructured.NestedString(svc.Object, "spec", "storage")
	}
	volumes, err := ServiceVolumes(namespace, name, serviceType, storage, true)
	if err != nil {
		return nil, err
	}
	return volumes[1:], nil
}

// StorageRoom describes the capacity in bytes for new replicas on one node's
// disk, after applying Longhorn's reservation and scheduling settings.
type StorageRoom struct {
	Node  string
	Bytes int64
}

// CheckStorageRoom checks capacity without changing cluster state. It checks
// room for each replicated volume on two distinct nodes, then checks combined
// demand, counting both copies of a replicated volume. A definite shortage returns
// an error with sizeOption as the size flag or field in its hint. A Kubernetes
// Forbidden error skips the check with an empty warning and nil error. Other read
// failures, no eligible disks, or an inconclusive placement return a warning and
// nil error.
// Existing claims, local storage and classes using other provisioners are skipped.
// Pending Longhorn volumes count towards demand if they fit on an eligible disk,
// including service and shared volumes whose claims are still being created.
// An empty warning and nil error mean the check passed or was skipped.
// Capacity is a snapshot, so passing this check does not guarantee scheduling.
func (m *Manager) CheckStorageRoom(ctx context.Context, volumes []NewVolume, sizeOption string) (warning string, err error) {
	if len(volumes) == 0 {
		return "", nil
	}
	mode := k8s.ReadStorageMode(ctx, m.Dynamic)
	if mode == platform.StorageLocal {
		return "", nil
	}
	volumes, err = m.withoutClaims(ctx, volumes)
	if err != nil {
		return notChecked(err), nil
	}
	var nodes []unstructured.Unstructured
	var disks []longhorncap.Disk
	var read bool
	var wanted []placement
	for _, class := range []string{platform.SingleWriterClass(mode), platform.LonghornReplicatedClass} {
		var inClass []NewVolume
		for _, v := range volumes {
			if v.class(mode) == class {
				inClass = append(inClass, v)
			}
		}
		if len(inClass) == 0 {
			continue
		}
		selector, longhorn, err := m.classNodeSelector(ctx, class)
		if err != nil {
			return notChecked(err), nil
		}
		if !longhorn {
			continue
		}
		if !read {
			var settings longhorncap.Settings
			if nodes, settings, err = m.longhornState(ctx); err != nil {
				return notChecked(err), nil
			}
			disks, read = longhorncap.SchedulingDisks(nodes, settings), true
		}
		eligible := withTags(disks, selector)
		if len(eligible) == 0 {
			return notChecked(fmt.Errorf("no Longhorn node can currently accept %s volumes", class)), nil
		}
		for _, v := range inClass {
			wanted = append(wanted, placement{volume: v, class: class, disks: eligible})
		}
	}
	if len(wanted) == 0 {
		return "", nil
	}
	pending, err := m.unscheduledClaims(ctx, mode, nodes, disks)
	if err != nil {
		return notChecked(err), nil
	}
	return place(disks, wanted, placeable(disks, pending), sizeOption)
}

func (m *Manager) withoutClaims(ctx context.Context, volumes []NewVolume) ([]NewVolume, error) {
	var out []NewVolume
	for _, v := range volumes {
		if v.Claim != "" {
			_, err := m.Client.CoreV1().PersistentVolumeClaims(v.Namespace).Get(ctx, v.Claim, metav1.GetOptions{})
			if err == nil {
				continue
			}
			if !errors.IsNotFound(err) {
				return nil, fmt.Errorf("reading the claim %s: %w", v.Claim, err)
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// ServerStorageRoom estimates the largest capacity for a new single-copy volume
// among eligible disks after placing pending volumes largest first on the
// roomiest eligible disks, with each copy on a distinct node. Volumes whose
// copies cannot all fit are skipped. Ties favour the first node name alphabetically.
// Longhorn may choose a different placement.
// It returns an error for local storage or a non-Longhorn single-writer class,
// read failures, or no eligible disks. Zero capacity with nil error means the
// estimated placement leaves no room for a new replica on eligible disks.
func (m *Manager) ServerStorageRoom(ctx context.Context) (StorageRoom, error) {
	mode := k8s.ReadStorageMode(ctx, m.Dynamic)
	class := platform.SingleWriterClass(mode)
	selector, longhorn, err := m.classNodeSelector(ctx, class)
	if err != nil {
		return StorageRoom{}, err
	}
	if mode == platform.StorageLocal || !longhorn {
		return StorageRoom{}, fmt.Errorf("the %s class does not use Longhorn", class)
	}
	nodes, settings, err := m.longhornState(ctx)
	if err != nil {
		return StorageRoom{}, err
	}
	disks := longhorncap.SchedulingDisks(nodes, settings)
	eligible := withTags(disks, selector)
	if len(eligible) == 0 {
		return StorageRoom{}, fmt.Errorf("no Longhorn node can currently accept %s volumes", class)
	}
	pending, err := m.unscheduledClaims(ctx, mode, nodes, disks)
	if err != nil {
		return StorageRoom{}, err
	}
	disks = holdRoom(disks, pending)
	best := disks[roomiest(disks, eligible)]
	return StorageRoom{Node: best.Node, Bytes: best.Room}, nil
}

// unscheduledClaims returns pending demand from Longhorn claims without a volume
// or without a replica in any disk's scheduledReplica, which Longhorn v1.7.3
// sums into storageScheduled. It also includes service and shared volumes whose
// claims are still being created. This accounts for recently requested volumes
// before their sizes appear in disk totals. Resources being deleted are excluded.
// Only positive sizes with an eligible Longhorn disk are included.
func (m *Manager) unscheduledClaims(ctx context.Context, mode string, nodes []unstructured.Unstructured, disks []longhorncap.Disk) ([]placement, error) {
	claims, err := m.Client.CoreV1().PersistentVolumeClaims(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing volume claims: %w", err)
	}
	pvs, err := m.Client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing persistent volumes: %w", err)
	}
	handles := map[string]string{}
	for _, pv := range pvs.Items {
		if pv.Spec.CSI != nil && pv.Spec.CSI.Driver == longhornProvisioner {
			handles[pv.Name] = pv.Spec.CSI.VolumeHandle
		}
	}
	counted := scheduledVolumes(nodes)
	type classDisks struct {
		disks    []int
		longhorn bool
	}
	classes := map[string]classDisks{}
	classOf := func(class string) (classDisks, error) {
		if known, ok := classes[class]; ok {
			return known, nil
		}
		selector, longhorn, err := m.classNodeSelector(ctx, class)
		if err != nil && !errors.IsNotFound(err) {
			return classDisks{}, err
		}
		classes[class] = classDisks{disks: withTags(disks, selector), longhorn: longhorn}
		return classes[class], nil
	}
	var pending []placement
	hold := func(v NewVolume, class string) error {
		known, err := classOf(class)
		if err != nil || !known.longhorn || len(known.disks) == 0 || v.Bytes <= 0 {
			return err
		}
		v.Name = v.Namespace + "/" + v.Claim
		pending = append(pending, placement{volume: v, class: class, disks: known.disks})
		return nil
	}
	existing := map[string]bool{}
	for _, c := range claims.Items {
		existing[c.Namespace+"/"+c.Name] = true
		if c.DeletionTimestamp != nil || c.Spec.StorageClassName == nil {
			continue
		}
		if volume := handles[c.Spec.VolumeName]; c.Spec.VolumeName != "" && (counted[volume] || counted[c.Spec.VolumeName]) {
			continue
		}
		size := c.Spec.Resources.Requests[corev1.ResourceStorage]
		if err := hold(NewVolume{Bytes: size.Value(), Namespace: c.Namespace, Claim: c.Name}, *c.Spec.StorageClassName); err != nil {
			return nil, err
		}
	}
	// kip service add waits for the credentials Secret, which the reconciler
	// writes before the StatefulSet, so the next command can run before claims exist.
	services, err := m.Dynamic.Resource(manifest.ServiceGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing services: %w", err)
	}
	for _, svc := range services.Items {
		if svc.GetDeletionTimestamp() != nil {
			continue
		}
		serviceType, _, _ := unstructured.NestedString(svc.Object, "spec", "type")
		storage, _, _ := unstructured.NestedString(svc.Object, "spec", "storage")
		dump, _, _ := unstructured.NestedBool(svc.Object, "spec", "consistentBackups")
		volumes, err := ServiceVolumes(svc.GetNamespace(), svc.GetName(), serviceType, storage, dump)
		if err != nil {
			continue
		}
		copies, _, _ := unstructured.NestedInt64(svc.Object, "spec", "copies")
		volumes[0].Copies = int32(copies) //nolint:gosec // G115: the CRD bounds copies to 1 or 2.
		for _, v := range volumes {
			if !existing[v.Namespace+"/"+v.Claim] {
				if err := hold(v, v.class(mode)); err != nil {
					return nil, err
				}
			}
		}
	}
	shared, err := m.Dynamic.Resource(manifest.VolumeGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing shared volumes: %w", err)
	}
	for _, vol := range shared.Items {
		size, _, _ := unstructured.NestedString(vol.Object, "spec", "size")
		q, err := resource.ParseQuantity(size)
		if vol.GetDeletionTimestamp() != nil || err != nil {
			continue
		}
		if v := SharedVolume(vol.GetNamespace(), vol.GetName(), q.Value()); !existing[v.Namespace+"/"+v.Claim] {
			if err := hold(v, v.class(mode)); err != nil {
				return nil, err
			}
		}
	}
	return pending, nil
}

// scheduledVolumes returns the Longhorn volumes with a replica in some disk's
// scheduledReplica. Longhorn names a replica <volume>-r-<id>.
func scheduledVolumes(nodes []unstructured.Unstructured) map[string]bool {
	volumes := map[string]bool{}
	for i := range nodes {
		statuses, _, _ := unstructured.NestedMap(nodes[i].Object, "status", "diskStatus")
		for name := range statuses {
			replicas, _, _ := unstructured.NestedMap(statuses, name, "scheduledReplica")
			for replica := range replicas {
				if cut := strings.LastIndex(replica, "-r-"); cut > 0 {
					volumes[replica[:cut]] = true
				}
			}
		}
	}
	return volumes
}

// placeable returns pending volumes that individually fit on an eligible disk.
// Volumes too large for every eligible disk consume no scheduling capacity
// because Longhorn cannot place them with the current capacity.
func placeable(disks []longhorncap.Disk, pending []placement) []placement {
	var out []placement
	for _, p := range pending {
		if disks[roomiest(disks, p.disks)].Room >= p.volume.Bytes {
			out = append(out, p)
		}
	}
	return out
}

// holdRoom estimates remaining capacity on a copy of disks by placing pending
// volumes largest first with hold. Volumes that no longer fit after earlier
// placements are skipped.
// Longhorn may place them differently, so the result serves as an estimate for
// kip status and never as proof of a shortage.
func holdRoom(disks []longhorncap.Disk, pending []placement) []longhorncap.Disk {
	disks = slices.Clone(disks)
	for _, p := range largestFirst(placeable(disks, pending)) {
		hold(disks, p)
	}
	return disks
}

func largestFirst(placements []placement) []placement {
	sorted := slices.Clone(placements)
	slices.SortStableFunc(sorted, func(a, b placement) int { return cmp.Compare(b.volume.Bytes, a.volume.Bytes) })
	return sorted
}

// hold reserves room in disks for p's first copy on its roomiest eligible disk
// and, for two copies, the second on the roomiest eligible disk of another node.
// It returns true when all reservations succeed, or false with disks unchanged
// when any copy cannot fit. p.disks must be non-empty.
func hold(disks []longhorncap.Disk, p placement) bool {
	first := roomiest(disks, p.disks)
	if disks[first].Room < p.volume.Bytes {
		return false
	}
	if p.copies() == 2 {
		second, ok := secondCopyDisk(disks, p.disks)
		if !ok || disks[second].Room < p.volume.Bytes {
			return false
		}
		disks[second].Room -= p.volume.Bytes
	}
	disks[first].Room -= p.volume.Bytes
	return true
}

// notChecked returns an empty string for Kubernetes Forbidden errors and a
// warning for other failures.
func notChecked(err error) string {
	if errors.IsForbidden(err) {
		return ""
	}
	return fmt.Sprintf("Longhorn's free space was not checked (%v). If the disk is full, the new volume stays unscheduled and its pod waits.", err)
}

// classNodeSelector returns the Longhorn node tags a class requires, and false
// when the class is not provisioned by Longhorn.
func (m *Manager) classNodeSelector(ctx context.Context, class string) ([]string, bool, error) {
	sc, err := m.Client.StorageV1().StorageClasses().Get(ctx, class, metav1.GetOptions{})
	if err != nil {
		return nil, false, fmt.Errorf("reading the %s StorageClass: %w", class, err)
	}
	if sc.Provisioner != longhornProvisioner {
		return nil, false, nil
	}
	return classTags(sc.Parameters), true, nil
}

// classTags returns the Longhorn node tags in a class's nodeSelector parameter.
func classTags(parameters map[string]string) []string {
	var tags []string
	for _, tag := range strings.Split(parameters["nodeSelector"], ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			tags = append(tags, tag)
		}
	}
	return tags
}

func (m *Manager) longhornState(ctx context.Context) ([]unstructured.Unstructured, longhorncap.Settings, error) {
	var settings longhorncap.Settings
	for _, s := range []struct {
		name string
		into *int64
	}{
		{"storage-over-provisioning-percentage", &settings.OverProvisioning},
		{"storage-minimal-available-percentage", &settings.MinimalAvailable},
	} {
		name, into := s.name, s.into
		setting, err := m.Dynamic.Resource(longhornSettingGVR).Namespace("longhorn-system").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, settings, fmt.Errorf("reading the Longhorn setting %s: %w", name, err)
		}
		value, _, _ := unstructured.NestedString(setting.Object, "value")
		if *into, err = strconv.ParseInt(value, 10, 64); err != nil {
			return nil, settings, fmt.Errorf("reading the Longhorn setting %s: %w", name, err)
		}
	}
	list, err := m.Dynamic.Resource(longhornNodeGVR).Namespace("longhorn-system").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, settings, fmt.Errorf("listing Longhorn nodes: %w", err)
	}
	return list.Items, settings, nil
}

// withTags returns indexes of disks whose node carries every selector tag.
// An empty selector includes all disks.
func withTags(disks []longhorncap.Disk, selector []string) []int {
	var out []int
	for i, d := range disks {
		if longhorncap.HasAllTags(d.Tags, selector) {
			out = append(out, i)
		}
	}
	return out
}

// placement is a new volume and the disks Longhorn may put a copy of it on.
type placement struct {
	volume NewVolume
	class  string
	disks  []int
}

// copies returns the requested replica count for p's class: two for the
// replicated class and one otherwise. Each copy requires a distinct node.
func (p placement) copies() int {
	if p.class == platform.LonghornReplicatedClass {
		return 2
	}
	return 1
}

// place refuses when a shortage is certain for any new volume: it is larger
// than any eligible disk has room for, a replicated volume lacks room on a
// second node, or a node set needs more room for new and pending copies than
// it has, counting both copies of a replicated volume. Otherwise it estimates
// placement for every volume, largest first, with hold, so
// volumes of both classes draw on the same disks. Longhorn makes its own
// placement choices, so failure to place a volume here produces only a warning.
func place(disks []longhorncap.Disk, wanted, pending []placement, sizeOption string) (string, error) {
	all := append(slices.Clone(pending), wanted...)
	for _, p := range wanted {
		if disks[roomiest(disks, p.disks)].Room < p.volume.Bytes || secondCopyShort(disks, p) {
			return "", shortage(disks, wanted, pending, p, sizeOption)
		}
	}
	demands := make([]longhorncap.Demand, len(all))
	for j, p := range all {
		demands[j] = longhorncap.Demand{Bytes: p.volume.Bytes, Copies: p.copies(), Disks: p.disks}
	}
	for _, s := range longhorncap.Shortfalls(disks, demands) {
		if slices.ContainsFunc(s.Copies[len(pending):], func(n int) bool { return n > 0 }) {
			return "", nodeShortage(s, all, len(pending), sizeOption)
		}
	}
	room := slices.Clone(disks)
	for _, p := range largestFirst(all) {
		if hold(room, p) {
			continue
		}
		need, free := total(all, disks, allDisks(all))
		return fmt.Sprintf("Longhorn has %s free for scheduling on eligible disks, and these volumes need %s. The check could not find a placement. If Longhorn also cannot place them, a volume stays unscheduled and its pod waits.",
			FormatSize(free), FormatSize(need)), nil
	}
	return "", nil
}

// nodeShortage explains a shortfall to which a new volume contributes. The
// first pending entries of all are volumes awaiting scheduling, and the rest
// are new.
func nodeShortage(s longhorncap.Shortfall, all []placement, pending int, sizeOption string) error {
	var needs []string
	var need, held int64
	var waiting []string
	for j, p := range all {
		if s.Copies[j] == 0 {
			continue
		}
		bytes := int64(s.Copies[j]) * p.volume.Bytes
		part := p.volume.Name + " " + FormatSize(p.volume.Bytes)
		if s.Copies[j] > 1 {
			part += fmt.Sprintf(" × %d", s.Copies[j])
		}
		if j < pending {
			held += bytes
			waiting = append(waiting, part)
			continue
		}
		need += bytes
		needs = append(needs, part)
	}
	where := strings.Join(s.Nodes, ", ")
	message := fmt.Sprintf("not enough storage on %s: Longhorn can schedule at most %s more on these nodes. The copies required on these nodes need %s (%s)",
		where, FormatSize(s.Room), FormatSize(need), strings.Join(needs, ", "))
	if len(waiting) > 0 {
		message += fmt.Sprintf(" in addition to %s for volumes awaiting Longhorn scheduling (%s)", FormatSize(held), strings.Join(waiting, ", "))
	}
	return fmt.Errorf("%s. Free space with kip service delete <name> --delete-data or kip volume delete <name> --delete-data, grow a disk, or request less storage with %s",
		message, sizeOption)
}

// secondCopyDisk returns the index of the roomiest eligible disk on a different
// node from the roomiest disk. It returns false if all eligible disks share one
// node. indexes must be non-empty. Ties follow roomiest.
func secondCopyDisk(disks []longhorncap.Disk, indexes []int) (int, bool) {
	first := disks[roomiest(disks, indexes)].Node
	var others []int
	for _, i := range indexes {
		if disks[i].Node != first {
			others = append(others, i)
		}
	}
	if len(others) == 0 {
		return 0, false
	}
	return roomiest(disks, others), true
}

// secondCopyShort reports whether a replicated volume lacks room for a copy
// outside the roomiest node, including when no other node has an eligible disk.
func secondCopyShort(disks []longhorncap.Disk, p placement) bool {
	if p.class != platform.LonghornReplicatedClass {
		return false
	}
	i, ok := secondCopyDisk(disks, p.disks)
	return !ok || disks[i].Room < p.volume.Bytes
}

// total returns the combined size in bytes of all requested copies and the
// sum of Room for the listed disks.
func total(wanted []placement, disks []longhorncap.Disk, indexes []int) (need, free int64) {
	for _, w := range wanted {
		need += int64(w.copies()) * w.volume.Bytes
	}
	for _, i := range indexes {
		free += disks[i].Room
	}
	return need, free
}

func allDisks(wanted []placement) []int {
	var all []int
	for _, w := range wanted {
		for _, i := range w.disks {
			if !slices.Contains(all, i) {
				all = append(all, i)
			}
		}
	}
	return all
}

// roomiest returns the index of the disk with the most room, choosing the first
// node name alphabetically on a tie. indexes must be non-empty.
func roomiest(disks []longhorncap.Disk, indexes []int) int {
	best := indexes[0]
	for _, i := range indexes[1:] {
		if disks[i].Room > disks[best].Room || (disks[i].Room == disks[best].Room && disks[i].Node < disks[best].Node) {
			best = i
		}
	}
	return best
}

func shortage(disks []longhorncap.Disk, wanted, pending []placement, p placement, sizeOption string) error {
	need, _ := total(wanted, disks, nil)
	best := disks[roomiest(disks, p.disks)]
	var sharing []placement
	for _, q := range pending {
		if slices.ContainsFunc(q.disks, func(i int) bool { return slices.Contains(p.disks, i) }) {
			sharing = append(sharing, q)
		}
	}
	needs := FormatSize(need) + " (" + describe(wanted) + ")"
	if len(sharing) > 0 {
		held, _ := total(sharing, disks, nil)
		needs += fmt.Sprintf(" in addition to %s for volumes awaiting Longhorn scheduling (%s)", FormatSize(held), describe(sharing))
	}
	hint := fmt.Sprintf("Free space with kip service delete <name> --delete-data or kip volume delete <name> --delete-data, grow the server's disk, or request less storage with %s.", sizeOption)
	if disks[roomiest(disks, p.disks)].Room >= p.volume.Bytes && secondCopyShort(disks, p) {
		second, ok := secondCopyDisk(disks, p.disks)
		if !ok {
			return fmt.Errorf("no room for the second copy of %s: it needs %s on another node, and no other node that can hold copies has a disk that Longhorn can schedule on now. 'kip cluster storage' shows the nodes that can hold copies",
				p.volume.Name, FormatSize(p.volume.Bytes))
		}
		return fmt.Errorf("not enough storage for the second copy of %s: it needs %s on another node that can hold copies, and Longhorn can schedule at most %s more on %s. %s",
			p.volume.Name, FormatSize(p.volume.Bytes), FormatSize(disks[second].Room), disks[second].Node, hint)
	}
	if p.class == platform.LonghornReplicatedClass {
		return fmt.Errorf("not enough storage on the nodes that can hold copies: Longhorn can schedule at most %s more, on %s, and this needs %s. %s",
			FormatSize(best.Room), best.Node, needs, hint)
	}
	return fmt.Errorf("not enough storage on the server: Longhorn can schedule %s more on %s, and this needs %s. %s",
		FormatSize(best.Room), best.Node, needs, hint)
}

// describe lists volume names and sizes in input order, separated by commas.
func describe(volumes []placement) string {
	parts := make([]string, len(volumes))
	for i, v := range volumes {
		parts[i] = v.volume.Name + " " + FormatSize(v.volume.Bytes)
	}
	return strings.Join(parts, ", ")
}

// FormatSize formats bytes as binary units. Whole Gi values are exact, other
// values of at least 1Gi round down to tenths of a Gi, and smaller values use
// whole Mi truncated towards zero.
func FormatSize(bytes int64) string {
	const gib = 1 << 30
	switch {
	case bytes >= gib && bytes%gib == 0:
		return fmt.Sprintf("%dGi", bytes/gib)
	case bytes >= gib:
		return fmt.Sprintf("%.1fGi", math.Floor(float64(bytes)*10/gib)/10)
	}
	return fmt.Sprintf("%dMi", bytes/(1<<20))
}
