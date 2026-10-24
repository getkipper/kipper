package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/backupscope"
	"github.com/getkipper/kipper/controller/pkg/longhorncap"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

// conversionLeaseName is the Lease that lets one Longhorn volume at a time
// build a copy Kipper asked for. Its holder is the Longhorn volume's name.
const (
	conversionLeaseName      = "kipper-storage-conversion"
	conversionLeaseNamespace = "kipper-system"
	conversionLeaseSeconds   = 600
)

var (
	longhornReplicaKind = schema.GroupVersionKind{Group: "longhorn.io", Version: "v1beta2", Kind: "Replica"}
	longhornEngineList  = schema.GroupVersionKind{Group: "longhorn.io", Version: "v1beta2", Kind: "EngineList"}
	longhornSettingKind = schema.GroupVersionKind{Group: "longhorn.io", Version: "v1beta2", Kind: "Setting"}
)

// storageConverter changes the copies of an existing claim's Longhorn volume,
// one step per reconcile. Reader must read uncached. Writer patches the
// volume, deletes replicas and writes the conversion Lease.
type storageConverter struct {
	Reader client.Reader
	Writer client.Client
	Now    func() time.Time
}

// platformNamespace reports a namespace whose claims are never converted:
// kipper-system and the namespaces backups skip, among them the restore
// drill's.
func platformNamespace(ns string) bool {
	return ns == conversionLeaseNamespace || slices.Contains(backupscope.ExcludedNamespaces(), ns)
}

func (s storageConverter) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// convert takes one step towards wanted copies on pvc's Longhorn volume. The
// caller has established that its object owns pvc. It returns the
// StorageCopies condition describing the conversion, or nil when the
// object's copies are unset, the claim is on local storage, not bound, a
// dump claim or in a platform namespace, or its Longhorn volume is missing,
// being deleted or belongs to another claim. It also returns nil when the
// step has no conversion reason, including placement-only updates, completed
// conversions and Longhorn's own rebuilds. The caller then reports live copies.
// Read and write failures are returned as errors.
func (s storageConverter) convert(ctx context.Context, pvc *corev1.PersistentVolumeClaim, wanted *int32) (*metav1.Condition, error) {
	if wanted == nil || pvc.Spec.VolumeName == "" || pvc.Labels[dumpForLabel] != "" || platformNamespace(pvc.Namespace) ||
		(pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName == platform.LocalPathClass) {
		return nil, nil
	}
	target := int64(*wanted)
	v, c, err := s.read(ctx, pvc)
	if err != nil || v == nil {
		return nil, err
	}
	step := planConversion(target, v, c, s.now())
	if step.Lease == leaseTake {
		if err := s.readCapacity(ctx, c); err != nil {
			return nil, err
		}
		step = planConversion(target, v, c, s.now())
	}

	switch step.Lease {
	case leaseTake:
		held, err := s.takeLease(ctx, v.Name)
		if err != nil {
			return nil, err
		}
		if !held {
			return &metav1.Condition{Type: kipperv1.ConditionStorageCopies, Status: metav1.ConditionFalse,
				Reason:  kipperv1.StorageCopiesConversionPending,
				Message: "Another volume's copies are being converted. This volume's conversion starts once that one is done."}, nil
		}
	case leaseHold:
		if _, err := s.takeLease(ctx, v.Name); err != nil {
			return nil, err
		}
	default:
		// A settled volume skips the read. A lease it still held expires and
		// may be taken over, since the volume has no rebuild outstanding.
		if step.Kind != stepDone {
			if err := s.releaseLease(ctx, v.Name); err != nil {
				return nil, err
			}
		}
	}

	switch step.Kind {
	case stepPatch:
		if err := s.patchVolume(ctx, v, step.Patch); err != nil {
			return nil, err
		}
	case stepDelete:
		if err := s.deleteSurplus(ctx, pvc, target, step); err != nil {
			return nil, err
		}
	}
	if step.Reason == "" {
		return nil, nil
	}
	return &metav1.Condition{Type: kipperv1.ConditionStorageCopies, Status: metav1.ConditionFalse, Reason: step.Reason, Message: step.Message}, nil
}

// convertOrBlock calls convert and returns conflicts, timeouts and throttling
// errors for retry. It logs other errors and returns a Blocked condition with
// a nil error so users can reset the copy count while the failure persists.
// A stale Converting condition would prevent this in kip and admission policy.
func (s storageConverter) convertOrBlock(ctx context.Context, pvc *corev1.PersistentVolumeClaim, wanted *int32) (*metav1.Condition, error) {
	c, err := s.convert(ctx, pvc, wanted)
	if err == nil || errors.IsConflict(err) || errors.IsTimeout(err) || errors.IsServerTimeout(err) || errors.IsTooManyRequests(err) {
		return c, err
	}
	log.FromContext(ctx).Error(err, "Could not convert storage copies.", "namespace", pvc.Namespace, "claim", pvc.Name)
	return &metav1.Condition{Type: kipperv1.ConditionStorageCopies, Status: metav1.ConditionFalse, Reason: kipperv1.StorageCopiesBlocked,
		Message: fmt.Sprintf("Kipper could not take the next step of the copy change: %v. Kipper will retry. Setting the copies back stops the change.", err)}, nil
}

// read loads pvc's Longhorn volume, its replicas and active engine, the
// Longhorn node tags, the availability of every replica's node and whether
// Longhorn rebuilds detached volumes. Disk capacity is left unread. A nil
// volume means there is nothing to convert.
func (s storageConverter) read(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (*lhVolume, *lhCluster, error) {
	name := pvc.Spec.VolumeName
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(longhornVolumeKind)
	if err := s.Reader.Get(ctx, types.NamespacedName{Namespace: longhornNamespace, Name: name}, obj); err != nil {
		if absentKind(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("reading Longhorn volume %s: %w", name, err)
	}
	ns, _, _ := unstructured.NestedString(obj.Object, "status", "kubernetesStatus", "namespace")
	claim, _, _ := unstructured.NestedString(obj.Object, "status", "kubernetesStatus", "pvcName")
	if ns != pvc.Namespace || claim != pvc.Name || obj.GetDeletionTimestamp() != nil {
		return nil, nil, nil
	}
	v := volumeFrom(obj)

	replicas := &unstructured.UnstructuredList{}
	replicas.SetGroupVersionKind(longhornReplicaList)
	if err := s.Reader.List(ctx, replicas, client.InNamespace(longhornNamespace), client.MatchingLabels{"longhornvolume": name}); err != nil {
		return nil, nil, fmt.Errorf("listing replicas of Longhorn volume %s: %w", name, err)
	}
	for i := range replicas.Items {
		v.Replicas = append(v.Replicas, replicaFrom(&replicas.Items[i]))
	}
	engines := &unstructured.UnstructuredList{}
	engines.SetGroupVersionKind(longhornEngineList)
	if err := s.Reader.List(ctx, engines, client.InNamespace(longhornNamespace), client.MatchingLabels{"longhornvolume": name}); err != nil {
		return nil, nil, fmt.Errorf("listing engines of Longhorn volume %s: %w", name, err)
	}
	for _, e := range engines.Items {
		if active, _, _ := unstructured.NestedBool(e.Object, "spec", "active"); active {
			v.Modes, _, _ = unstructured.NestedStringMap(e.Object, "status", "replicaModeMap")
		}
	}

	nodes := &unstructured.UnstructuredList{}
	nodes.SetGroupVersionKind(longhornNodeList)
	if err := s.Reader.List(ctx, nodes, client.InNamespace(longhornNamespace)); err != nil {
		return nil, nil, fmt.Errorf("listing Longhorn nodes: %w", err)
	}
	c := &lhCluster{Servers: map[string]bool{}, Eligible: map[string]bool{}, Available: map[string]bool{}, nodes: nodes.Items}
	for _, n := range nodes.Items {
		tags, _, _ := unstructured.NestedStringSlice(n.Object, "spec", "tags")
		c.Servers[n.GetName()] = longhorncap.HasAllTags(tags, []string{platform.ServerStorageTag})
		c.Eligible[n.GetName()] = longhorncap.HasAllTags(tags, []string{platform.ReplicaStorageTag})
	}
	for _, r := range v.Replicas {
		if _, seen := c.Available[r.Node]; r.Node != "" && !seen {
			c.Available[r.Node] = readHolder(ctx, s.Reader, r.Node) == holderAvailable
		}
	}
	version, err := s.setting(ctx, "current-longhorn-version")
	if err != nil {
		return nil, nil, err
	}
	c.OfflineRebuilding = versionAtLeast(version, 1, 9)
	return v, c, nil
}

func volumeFrom(obj *unstructured.Unstructured) *lhVolume {
	str := func(path ...string) string {
		s, _, _ := unstructured.NestedString(obj.Object, path...)
		return s
	}
	count, _, _ := unstructured.NestedInt64(obj.Object, "spec", "numberOfReplicas")
	selector, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "nodeSelector")
	v := &lhVolume{
		Name: obj.GetName(), ResourceVersion: obj.GetResourceVersion(), Count: count, Selector: selector,
		Locality: str("spec", "dataLocality"), AutoBalance: str("spec", "replicaAutoBalance"),
		OfflineRebuilding: str("spec", "offlineRebuilding"),
		Size:              numberField(obj.Object, "spec", "size"), Actual: numberField(obj.Object, "status", "actualSize"),
		State: str("status", "state"), Image: str("spec", "image"), CurrentImage: str("status", "currentImage"),
		MigrationNodeID: str("spec", "migrationNodeID"), CurrentMigrationNodeID: str("status", "currentMigrationNodeID"),
	}
	if started, err := time.Parse(time.RFC3339, obj.GetAnnotations()[copiesRebuildAnnotation]); err == nil {
		v.RebuildStarted = started
	} else if obj.GetAnnotations()[copiesRebuildAnnotation] != "" {
		// An unreadable start time still marks Kipper's rebuild. Its budget
		// counts as spent so it cannot hold the lease forever.
		v.RebuildStarted = time.Unix(1, 0)
	}
	return v
}

func replicaFrom(obj *unstructured.Unstructured) lhReplica {
	str := func(field string) string {
		s, _, _ := unstructured.NestedString(obj.Object, "spec", field)
		return s
	}
	evicting, _, _ := unstructured.NestedBool(obj.Object, "spec", "evictionRequested")
	return lhReplica{
		Name: obj.GetName(), UID: string(obj.GetUID()), ResourceVersion: obj.GetResourceVersion(),
		Node: str("nodeID"), HealthyAt: str("healthyAt"), FailedAt: str("failedAt"), LastFailedAt: str("lastFailedAt"),
		EvictionRequested: evicting, Deleting: obj.GetDeletionTimestamp() != nil,
	}
}

// numberField reads an int64 that Longhorn stores as a number or, like
// spec.size, as a decimal string. Anything else is zero.
func numberField(obj map[string]interface{}, path ...string) int64 {
	value, found, _ := unstructured.NestedFieldNoCopy(obj, path...)
	if !found {
		return 0
	}
	if s, ok := value.(string); ok {
		n, _ := strconv.ParseInt(s, 10, 64)
		return n
	}
	return longhorncap.Int64Field(map[string]interface{}{"n": value}, "n")
}

// versionAtLeast reports whether a Longhorn version such as v1.9.2 is at
// least major.minor. An unreadable version is treated as older.
func versionAtLeast(version string, major, minor int) bool {
	parts := strings.SplitN(strings.TrimPrefix(version, "v"), ".", 3)
	if len(parts) < 2 {
		return false
	}
	gotMajor, err1 := strconv.Atoi(parts[0])
	gotMinor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return gotMajor > major || (gotMajor == major && gotMinor >= minor)
}

func (s storageConverter) setting(ctx context.Context, name string) (string, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(longhornSettingKind)
	if err := s.Reader.Get(ctx, types.NamespacedName{Namespace: longhornNamespace, Name: name}, obj); err != nil {
		if errors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("reading Longhorn setting %s: %w", name, err)
	}
	value, _, _ := unstructured.NestedString(obj.Object, "value")
	return value, nil
}

// readCapacity fills c's disks from the Longhorn nodes read with it and the
// server reserve of every dump claim: its Longhorn volume's nominal size less
// what it already uses, because a dump grows towards its size on each backup.
func (s storageConverter) readCapacity(ctx context.Context, c *lhCluster) error {
	var settings longhorncap.Settings
	for _, field := range []struct {
		name  string
		value *int64
	}{
		{"storage-over-provisioning-percentage", &settings.OverProvisioning},
		{"storage-minimal-available-percentage", &settings.MinimalAvailable},
	} {
		raw, err := s.setting(ctx, field.name)
		if err != nil {
			return err
		}
		if *field.value, err = strconv.ParseInt(raw, 10, 64); err != nil {
			return fmt.Errorf("reading Longhorn setting %s: %q is not a number", field.name, raw)
		}
	}
	c.Disks = longhorncap.SchedulingDisks(c.nodes, settings)

	dumps := &corev1.PersistentVolumeClaimList{}
	if err := s.Reader.List(ctx, dumps, client.HasLabels{dumpForLabel}); err != nil {
		return fmt.Errorf("listing dump claims: %w", err)
	}
	c.ServerReserve = 0
	for _, d := range dumps.Items {
		if d.Spec.VolumeName == "" {
			continue
		}
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(longhornVolumeKind)
		if err := s.Reader.Get(ctx, types.NamespacedName{Namespace: longhornNamespace, Name: d.Spec.VolumeName}, obj); err != nil {
			if absentKind(err) {
				continue
			}
			return fmt.Errorf("reading Longhorn volume %s: %w", d.Spec.VolumeName, err)
		}
		c.ServerReserve += max(numberField(obj.Object, "spec", "size")-numberField(obj.Object, "status", "actualSize"), 0)
	}
	c.CapacityRead = true
	return nil
}

func (s storageConverter) patchVolume(ctx context.Context, v *lhVolume, patch *volumePatch) error {
	body := *patch
	meta := volumePatchMeta{ResourceVersion: v.ResourceVersion}
	if body.Metadata != nil {
		meta.Annotations = body.Metadata.Annotations
	}
	body.Metadata = &meta
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(longhornVolumeKind)
	obj.SetNamespace(longhornNamespace)
	obj.SetName(v.Name)
	if err := s.Writer.Patch(ctx, obj, client.RawPatch(types.MergePatchType, raw)); err != nil {
		return fmt.Errorf("patching Longhorn volume %s: %w", v.Name, err)
	}
	return nil
}

// deleteSurplus deletes the replica step chose after reading the volume
// again and planning once more, so the replicas it keeps are checked as they
// are now. The delete carries the replica's UID and resourceVersion from that
// read, so a replica that changed since is left alone.
func (s storageConverter) deleteSurplus(ctx context.Context, pvc *corev1.PersistentVolumeClaim, target int64, step conversionStep) error {
	v, c, err := s.read(ctx, pvc)
	if err != nil || v == nil {
		return err
	}
	fresh := planConversion(target, v, c, s.now())
	if fresh.Kind != stepDelete || fresh.Replica.Name != step.Replica.Name || !sameSelector(fresh.Keep, step.Keep) {
		return nil
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(longhornReplicaKind)
	obj.SetNamespace(longhornNamespace)
	obj.SetName(fresh.Replica.Name)
	uid := types.UID(fresh.Replica.UID)
	rv := fresh.Replica.ResourceVersion
	err = s.Writer.Delete(ctx, obj, client.Preconditions{UID: &uid, ResourceVersion: &rv})
	if err != nil && !errors.IsNotFound(err) && !errors.IsConflict(err) {
		return fmt.Errorf("deleting Longhorn replica %s: %w", fresh.Replica.Name, err)
	}
	return nil
}

// takeLease makes holder the conversion Lease's holder and reports whether it
// is. A free lease or holder's own is taken. Another holder's lease is taken
// over only once it has expired and that holder's volume has no Kipper
// rebuild within its time budget. An earlier rebuild may continue after its
// budget expires.
func (s storageConverter) takeLease(ctx context.Context, holder string) (bool, error) {
	now := metav1.NewMicroTime(s.now())
	seconds := int32(conversionLeaseSeconds)
	var lease coordinationv1.Lease
	key := types.NamespacedName{Namespace: conversionLeaseNamespace, Name: conversionLeaseName}
	if err := s.Reader.Get(ctx, key, &lease); err != nil {
		if !errors.IsNotFound(err) {
			return false, fmt.Errorf("reading the conversion lease: %w", err)
		}
		lease = coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Namespace: conversionLeaseNamespace, Name: conversionLeaseName},
			Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &seconds, AcquireTime: &now, RenewTime: &now},
		}
		if err := s.Writer.Create(ctx, &lease); err != nil {
			if errors.IsAlreadyExists(err) {
				return false, nil
			}
			return false, fmt.Errorf("creating the conversion lease: %w", err)
		}
		return true, nil
	}
	current := ""
	if lease.Spec.HolderIdentity != nil {
		current = *lease.Spec.HolderIdentity
	}
	if current != "" && current != holder {
		if !leaseExpired(&lease, now.Time) {
			return false, nil
		}
		busy, err := s.rebuildOutstanding(ctx, current)
		if err != nil || busy {
			return false, err
		}
	}
	if current != holder {
		lease.Spec.AcquireTime = &now
	}
	lease.Spec.HolderIdentity = &holder
	lease.Spec.LeaseDurationSeconds = &seconds
	lease.Spec.RenewTime = &now
	if err := s.Writer.Update(ctx, &lease); err != nil {
		if errors.IsConflict(err) {
			return false, nil
		}
		return false, fmt.Errorf("updating the conversion lease: %w", err)
	}
	return true, nil
}

func leaseExpired(lease *coordinationv1.Lease, now time.Time) bool {
	if lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
		return true
	}
	return now.After(lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second))
}

// rebuildOutstanding reports whether the Longhorn volume name still carries a
// Kipper rebuild inside its budget.
func (s storageConverter) rebuildOutstanding(ctx context.Context, name string) (bool, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(longhornVolumeKind)
	if err := s.Reader.Get(ctx, types.NamespacedName{Namespace: longhornNamespace, Name: name}, obj); err != nil {
		if absentKind(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading Longhorn volume %s: %w", name, err)
	}
	v := volumeFrom(obj)
	return !v.RebuildStarted.IsZero() && s.now().Before(v.RebuildStarted.Add(rebuildBudget(v.Size))), nil
}

// releaseLease frees the conversion Lease when holder holds it.
func (s storageConverter) releaseLease(ctx context.Context, holder string) error {
	var lease coordinationv1.Lease
	key := types.NamespacedName{Namespace: conversionLeaseNamespace, Name: conversionLeaseName}
	if err := s.Reader.Get(ctx, key, &lease); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("reading the conversion lease: %w", err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != holder {
		return nil
	}
	lease.Spec.HolderIdentity = nil
	if err := s.Writer.Update(ctx, &lease); err != nil && !errors.IsConflict(err) {
		return fmt.Errorf("releasing the conversion lease: %w", err)
	}
	return nil
}
