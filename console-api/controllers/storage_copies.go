package controllers

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

// Longhorn objects are read uncached and unstructured, so no informer is ever
// registered for a kind a local-path cluster does not serve.
const longhornNamespace = "longhorn-system"

// Storage status is refreshed on a timer because nothing watches Longhorn.
const (
	storageRefreshConverting = 15 * time.Second
	storageRefreshUnsettled  = time.Minute
	storageRefreshSettled    = 5 * time.Minute
)

var (
	longhornNodeList    = schema.GroupVersionKind{Group: "longhorn.io", Version: "v1beta2", Kind: "NodeList"}
	longhornVolumeKind  = schema.GroupVersionKind{Group: "longhorn.io", Version: "v1beta2", Kind: "Volume"}
	longhornReplicaList = schema.GroupVersionKind{Group: "longhorn.io", Version: "v1beta2", Kind: "ReplicaList"}
)

// liveCopies combines Longhorn replica data with Kubernetes node availability.
type liveCopies struct {
	Copies  int32
	Healthy int32
	Unknown int32
	Nodes   []string
}

// absentKind reports a missing resource kind or object. This includes Longhorn
// kinds on a local-path cluster.
func absentKind(err error) bool {
	return errors.IsNotFound(err) || apimeta.IsNoMatchError(err)
}

// usableReplicaNodes counts the nodes that can take a copy now: Longhorn nodes
// tagged kipper-replica, schedulable and Ready in Longhorn, whose Kubernetes
// node is Ready and not fenced. A cluster without Longhorn has none.
func usableReplicaNodes(ctx context.Context, reader client.Reader) (int, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(longhornNodeList)
	if err := reader.List(ctx, list, client.InNamespace(longhornNamespace)); err != nil {
		if absentKind(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("listing Longhorn nodes: %w", err)
	}
	usable := 0
	for _, n := range list.Items {
		tags, _, _ := unstructured.NestedStringSlice(n.Object, "spec", "tags")
		scheduling, _, _ := unstructured.NestedBool(n.Object, "spec", "allowScheduling")
		if !slices.Contains(tags, platform.ReplicaStorageTag) || !scheduling ||
			!longhornConditionTrue(n, "Ready") || !longhornConditionTrue(n, "Schedulable") {
			continue
		}
		var node corev1.Node
		if err := reader.Get(ctx, types.NamespacedName{Name: n.GetName()}, &node); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return 0, fmt.Errorf("reading node %s: %w", n.GetName(), err)
		}
		if platform.NodeAvailable(&node) {
			usable++
		}
	}
	return usable, nil
}

// CopiesCapacity returns the maximum copy count offered by the administrator
// and the eligible node count used by the claim creation gate. The offer defaults
// to one if PlatformConfig or maxCopies is absent, or maxCopies is invalid.
// A cluster without Longhorn has no eligible nodes. Other read errors are returned.
func CopiesCapacity(ctx context.Context, reader client.Reader) (offered int32, nodes int, err error) {
	var pc kipperv1.PlatformConfig
	if err := reader.Get(ctx, types.NamespacedName{Name: PlatformConfigName}, &pc); err != nil && !errors.IsNotFound(err) {
		return 0, 0, fmt.Errorf("reading the copies offer: %w", err)
	}
	nodes, err = usableReplicaNodes(ctx, reader)
	if err != nil {
		return 0, 0, err
	}
	return platform.OfferedCopies(pc.Spec.MaxCopies), nodes, nil
}

func longhornConditionTrue(obj unstructured.Unstructured, kind string) bool {
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conditions {
		m, _ := c.(map[string]interface{})
		if m["type"] == kind {
			return m["status"] == "True"
		}
	}
	return false
}

// holderState is what readLiveCopies knows about a replica holder's node.
type holderState int

const (
	holderAvailable holderState = iota
	holderUnavailable
	holderUnreadable
)

// readHolder reads a replica holder's Kubernetes node. A missing node is
// unavailable. Any other read error leaves it unreadable.
func readHolder(ctx context.Context, reader client.Reader, name string) holderState {
	var node corev1.Node
	if err := reader.Get(ctx, types.NamespacedName{Name: name}, &node); err != nil {
		if errors.IsNotFound(err) {
			return holderUnavailable
		}
		return holderUnreadable
	}
	if platform.NodeAvailable(&node) {
		return holderAvailable
	}
	return holderUnavailable
}

// readLiveCopies reads the replica count configured on pvc's Longhorn volume
// and counts distinct Ready, unfenced nodes holding a current replica, as
// defined by platform.ReplicaCurrent. Missing nodes are unavailable. Other node
// read failures count as Unknown. Healthy holders are sorted by name.
// The volume name comes from the claim.
// It returns nil for an unbound claim, missing Longhorn resources, a mismatched
// claim reference or a replica count outside 0 to math.MaxInt32. Other volume
// and replica read errors are returned. A local-path claim is always reported
// as one healthy copy, including before it is bound.
func readLiveCopies(ctx context.Context, reader client.Reader, pvc *corev1.PersistentVolumeClaim) (*liveCopies, error) {
	if pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName == platform.LocalPathClass {
		return &liveCopies{Copies: 1, Healthy: 1}, nil
	}
	name := pvc.Spec.VolumeName
	if name == "" {
		return nil, nil
	}
	volume := &unstructured.Unstructured{}
	volume.SetGroupVersionKind(longhornVolumeKind)
	if err := reader.Get(ctx, types.NamespacedName{Namespace: longhornNamespace, Name: name}, volume); err != nil {
		if absentKind(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading Longhorn volume %s: %w", name, err)
	}
	ns, _, _ := unstructured.NestedString(volume.Object, "status", "kubernetesStatus", "namespace")
	claim, _, _ := unstructured.NestedString(volume.Object, "status", "kubernetesStatus", "pvcName")
	if ns != pvc.Namespace || claim != pvc.Name {
		return nil, nil
	}
	wanted, _, _ := unstructured.NestedInt64(volume.Object, "spec", "numberOfReplicas")
	if wanted < 0 || wanted > math.MaxInt32 {
		return nil, nil
	}

	replicas := &unstructured.UnstructuredList{}
	replicas.SetGroupVersionKind(longhornReplicaList)
	if err := reader.List(ctx, replicas, client.InNamespace(longhornNamespace), client.MatchingLabels{"longhornvolume": name}); err != nil {
		if absentKind(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing replicas of Longhorn volume %s: %w", name, err)
	}
	live := &liveCopies{Copies: int32(wanted)}
	holders := map[string]holderState{}
	for _, r := range replicas.Items {
		healthyAt, _, _ := unstructured.NestedString(r.Object, "spec", "healthyAt")
		failedAt, _, _ := unstructured.NestedString(r.Object, "spec", "failedAt")
		lastFailedAt, _, _ := unstructured.NestedString(r.Object, "spec", "lastFailedAt")
		node, _, _ := unstructured.NestedString(r.Object, "spec", "nodeID")
		if node == "" || !platform.ReplicaCurrent(healthyAt, failedAt, lastFailedAt) {
			continue
		}
		if _, seen := holders[node]; seen {
			continue
		}
		state := readHolder(ctx, reader, node)
		holders[node] = state
		switch state {
		case holderAvailable:
			live.Healthy++
			live.Nodes = append(live.Nodes, node)
		case holderUnreadable:
			live.Unknown++
		}
	}
	slices.Sort(live.Nodes)
	return live, nil
}

// copiesCondition describes a claim's copies against what its object asks
// for. setCommand is the command that sets copies on the object, shown while
// they are unset.
func copiesCondition(wanted *int32, live *liveCopies, setCommand string) metav1.Condition {
	c := metav1.Condition{Type: kipperv1.ConditionStorageCopies}
	switch {
	case wanted == nil && live == nil:
		c.Status, c.Reason = metav1.ConditionUnknown, kipperv1.StorageCopiesNotSet
		c.Message = fmt.Sprintf("Copies are not set, so Kipper leaves this volume as it is. Set them with '%s'.", setCommand)
	case wanted == nil && live.Copies > platform.MaxCopies:
		c.Status, c.Reason = metav1.ConditionUnknown, kipperv1.StorageCopiesNotSet
		c.Message = fmt.Sprintf("Copies are not set, so Kipper leaves this volume as it is: Longhorn keeps %d %s (%d healthy), more than Kipper sets. Setting copies with '%s' converts it to that count.", live.Copies, pluralCopies(live.Copies), live.Healthy, setCommand)
	case wanted == nil:
		c.Status, c.Reason = metav1.ConditionUnknown, kipperv1.StorageCopiesNotSet
		c.Message = fmt.Sprintf("Copies are not set, so Kipper leaves this volume as it is: Longhorn keeps %d %s (%d healthy). Set them with '%s'.", live.Copies, pluralCopies(live.Copies), live.Healthy, setCommand)
	case live == nil:
		c.Status, c.Reason = metav1.ConditionUnknown, kipperv1.StorageCopiesPending
		c.Message = "The volume is not provisioned yet."
	case live.Copies != *wanted:
		c.Status, c.Reason = metav1.ConditionFalse, kipperv1.StorageCopiesConversionPending
		c.Message = fmt.Sprintf("The copy count is set to %d, and the volume has %d. Kipper converts it once the volume can be read.", *wanted, live.Copies)
	case live.Healthy < *wanted && live.Unknown > 0:
		c.Status, c.Reason = metav1.ConditionUnknown, kipperv1.StorageCopiesUnknown
		c.Message = fmt.Sprintf("%d of %d copies are healthy, and Kipper could not read node information for %d %s.", live.Healthy, *wanted, live.Unknown, pluralCopies(live.Unknown))
	case live.Healthy < *wanted:
		c.Status, c.Reason = metav1.ConditionFalse, kipperv1.StorageCopiesDegraded
		c.Message = fmt.Sprintf("%d of %d copies are healthy. Longhorn rebuilds a missing copy when a node that can hold it is available.", live.Healthy, *wanted)
	default:
		c.Status, c.Reason = metav1.ConditionTrue, kipperv1.StorageCopiesHealthy
		c.Message = fmt.Sprintf("%d of %d copies are healthy.", live.Healthy, *wanted)
	}
	return c
}

// storageStatus is the status block for a claim's live copies, or nil when
// they are unknown.
func storageStatus(pvc *corev1.PersistentVolumeClaim, live *liveCopies) *kipperv1.StorageCopiesStatus {
	if live == nil {
		return nil
	}
	s := &kipperv1.StorageCopiesStatus{Copies: live.Copies, HealthyCopies: live.Healthy, Nodes: live.Nodes}
	if pvc.Spec.StorageClassName != nil {
		s.Class = *pvc.Spec.StorageClassName
	}
	return s
}

// storageRefresh polls healthy and unset storage less often to reduce API
// reads and a running conversion more often because it takes one step per
// reconcile.
func storageRefresh(c *metav1.Condition) time.Duration {
	switch {
	case c != nil && (c.Status == metav1.ConditionTrue || c.Reason == kipperv1.StorageCopiesNotSet):
		return storageRefreshSettled
	case c != nil && c.Reason == kipperv1.StorageCopiesConverting:
		return storageRefreshConverting
	}
	return storageRefreshUnsettled
}

// projectRequeue is how soon a project is reconciled again: the egress
// refresh interval, or sooner while it reports shared storage copies, since
// nothing watches Longhorn's objects.
func projectRequeue(conditions []metav1.Condition) time.Duration {
	c := apimeta.FindStatusCondition(conditions, kipperv1.ConditionStorageCopies)
	if c == nil {
		return egressRefreshInterval
	}
	return earliest(egressRefreshInterval, storageRefresh(c))
}

// earliest returns the shorter of two requeue delays, where zero means none.
func earliest(a, b time.Duration) time.Duration {
	if a == 0 || (b != 0 && b < a) {
		return b
	}
	return a
}

// creationGate decides whether a new claim that wants copies may be created.
// It returns "" when creation may go ahead, or the StorageCopies reason and
// message that hold it. Messages give counts only, never other nodes' names.
func creationGate(ctx context.Context, reader client.Reader, mode string, copies *int32, shared, restored bool) (reason, message string, err error) {
	if !platform.WantsTwoCopies(copies) {
		return "", "", nil
	}
	if platform.StorageMode(mode) == platform.StorageLocal {
		return kipperv1.StorageCopiesLocalStorage, "Two copies need Longhorn storage, and this cluster uses local storage, so no volume was created. Set copies to 1.", nil
	}
	if shared && !platform.RWXCopiesQualified {
		return kipperv1.StorageCopiesRWXNotQualified, "Shared volumes cannot have two copies in this Kipper release, so no volume was created. Set copies to 1.", nil
	}
	if restored {
		// A restore must not depend on how many nodes are back yet.
		// Longhorn creates the volume degraded and adds the copy later.
		return "", "", nil
	}
	nodes, err := usableReplicaNodes(ctx, reader)
	if err != nil {
		return "", "", err
	}
	if nodes < 2 {
		return kipperv1.StorageCopiesNotEnoughReplicaNodes, fmt.Sprintf("Two copies need two ready nodes that can hold copies, and the cluster has %d, so the volume is waiting for creation. An admin can enable copies on nodes with 'kip node storage <node> --copies'. The volume is created once two eligible nodes are ready.", nodes), nil
	}
	return "", "", nil
}

func setCopiesCondition(conditions *[]metav1.Condition, c metav1.Condition, generation int64) {
	c.ObservedGeneration = generation
	apimeta.SetStatusCondition(conditions, c)
}

// storageGateError holds a new claim back for the StorageCopies reason it
// names. It is a state to report, not a failure to retry at once.
type storageGateError struct {
	reason, message string
}

func (e *storageGateError) Error() string { return e.message }

// dataClaimName is the claim the StatefulSet's "data" template creates for
// its only pod.
func dataClaimName(service string) string { return "data-" + service + "-0" }

// gateNewDataClaim applies the copies creation gate to a service whose
// StatefulSet and data claim do not exist yet. An existing workload or claim,
// restored or not, is reconciled whatever copies says.
func (r *ServiceReconciler) gateNewDataClaim(ctx context.Context, svc *kipperv1.Service, mode string) error {
	if !platform.WantsTwoCopies(svc.Spec.Copies) {
		return nil
	}
	for _, obj := range []client.Object{&appsv1.StatefulSet{}, &corev1.PersistentVolumeClaim{}} {
		name := svc.Name
		if _, claim := obj.(*corev1.PersistentVolumeClaim); claim {
			name = dataClaimName(svc.Name)
		}
		err := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: name}, obj)
		if err == nil {
			return nil
		}
		if !errors.IsNotFound(err) {
			return err
		}
	}
	reason, message, err := creationGate(ctx, r.longhornReader(), mode, svc.Spec.Copies, false, svc.Labels[veleroRestoreNameLabel] != "")
	if err != nil {
		return err
	}
	if reason != "" {
		return &storageGateError{reason: reason, message: message}
	}
	return nil
}

func (r *ServiceReconciler) longhornReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// reportStorageCopies sets the service's storage status and StorageCopies
// condition from its data claim. A missing claim leaves both as they are.
func (r *ServiceReconciler) reportStorageCopies(ctx context.Context, svc *kipperv1.Service) error {
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: dataClaimName(svc.Name)}, &pvc); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	var converted *metav1.Condition
	if r.ownsDataClaim(ctx, svc, &pvc) {
		var err error
		if converted, err = (storageConverter{Reader: r.longhornReader(), Writer: r.Client}).convertOrBlock(ctx, &pvc, svc.Spec.Copies); err != nil {
			return err
		}
	}
	live, err := readLiveCopies(ctx, r.longhornReader(), &pvc)
	if err != nil {
		return reportConversionOnly(ctx, &svc.Status.Conditions, converted, svc.Generation, err)
	}
	svc.Status.Storage = storageStatus(&pvc, live)
	setCopiesCondition(&svc.Status.Conditions, orConverted(converted, copiesCondition(svc.Spec.Copies, live, "kip service update "+svc.Name+" --copies 1|2")), svc.Generation)
	return nil
}

// ownsDataClaim reports whether pvc is svc's data claim: the StatefulSet named
// after svc is controlled by svc, and the claim is controlled by that
// StatefulSet or has no controller and carries an app label matching svc's
// name. Only owned claims are converted. Other claims are only reported.
func (r *ServiceReconciler) ownsDataClaim(ctx context.Context, svc *kipperv1.Service, pvc *corev1.PersistentVolumeClaim) bool {
	var sts appsv1.StatefulSet
	if err := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}, &sts); err != nil {
		return false
	}
	ref := metav1.GetControllerOf(&sts)
	if ref == nil {
		return false
	}
	if owned, _ := controllerRefNames(ref, svc, "Service"); !owned {
		return false
	}
	if claimRef := metav1.GetControllerOf(pvc); claimRef != nil {
		return claimRef.Kind == "StatefulSet" && claimRef.Name == sts.Name && claimRef.UID == sts.UID
	}
	return pvc.Labels["app"] == svc.Name
}

// reportConversionOnly records converted, logs readErr and returns nil so a
// Blocked condition can be saved even when live reads fail. The caller retains
// the previous storage status. If converted is nil, it returns readErr.
func reportConversionOnly(ctx context.Context, conditions *[]metav1.Condition, converted *metav1.Condition, generation int64, readErr error) error {
	if converted == nil {
		return readErr
	}
	log.FromContext(ctx).Error(readErr, "Could not read the live storage copies.")
	setCopiesCondition(conditions, *converted, generation)
	return nil
}

// orConverted returns the conversion condition when present, otherwise the
// live copies condition.
func orConverted(converted *metav1.Condition, live metav1.Condition) metav1.Condition {
	if converted != nil {
		return *converted
	}
	return live
}

// reportStorageGate records why the service's data claim is held back.
func (r *ServiceReconciler) reportStorageGate(ctx context.Context, svc *kipperv1.Service, gate *storageGateError) error {
	before := svc.Status.DeepCopy()
	svc.Status.Phase = "Pending"
	setCopiesCondition(&svc.Status.Conditions, metav1.Condition{
		Type: kipperv1.ConditionStorageCopies, Status: metav1.ConditionFalse, Reason: gate.reason, Message: gate.message,
	}, svc.Generation)
	return r.writeStatusIfChanged(ctx, svc, before)
}

func (r *VolumeReconciler) longhornReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// reportStorageCopies sets the volume's storage status and StorageCopies
// condition from its shared claim.
func (r *VolumeReconciler) reportStorageCopies(ctx context.Context, vol *kipperv1.Volume, pvc *corev1.PersistentVolumeClaim) error {
	var converted *metav1.Condition
	if owned, _ := claimProvenance(pvc, vol); owned {
		var err error
		if converted, err = convertShared(ctx, storageConverter{Reader: r.longhornReader(), Writer: r.Client}, pvc, vol.Spec.Copies); err != nil {
			return err
		}
	}
	live, err := readLiveCopies(ctx, r.longhornReader(), pvc)
	if err != nil {
		return reportConversionOnly(ctx, &vol.Status.Conditions, converted, vol.Generation, err)
	}
	vol.Status.Storage = storageStatus(pvc, live)
	setCopiesCondition(&vol.Status.Conditions, orConverted(converted, sharedCopiesCondition(vol.Spec.Copies, live, "kip volume update "+vol.Name+" --copies 1|2")), vol.Generation)
	return nil
}

// convertShared returns nil without converting when two copies are requested
// and RWXCopiesQualified is false. Other requests use the normal conversion.
func convertShared(ctx context.Context, sc storageConverter, pvc *corev1.PersistentVolumeClaim, wanted *int32) (*metav1.Condition, error) {
	if platform.WantsTwoCopies(wanted) && !platform.RWXCopiesQualified {
		return nil, nil
	}
	return sc.convertOrBlock(ctx, pvc, wanted)
}

// sharedCopiesCondition is copiesCondition for a shared claim, which cannot
// have two copies while RWXCopiesQualified is false.
func sharedCopiesCondition(wanted *int32, live *liveCopies, setCommand string) metav1.Condition {
	if platform.WantsTwoCopies(wanted) && !platform.RWXCopiesQualified {
		kept := "its current copies"
		if live != nil {
			kept = fmt.Sprintf("%d %s", live.Copies, pluralCopies(live.Copies))
		}
		return metav1.Condition{
			Type: kipperv1.ConditionStorageCopies, Status: metav1.ConditionFalse, Reason: kipperv1.StorageCopiesRWXNotQualified,
			Message: fmt.Sprintf("Shared volumes cannot have two copies in this Kipper release, so the volume keeps %s.", kept),
		}
	}
	return copiesCondition(wanted, live, setCommand)
}

func pluralCopies(n int32) string {
	if n == 1 {
		return "copy"
	}
	return "copies"
}

// projectSharedClaim reports whether pvc, read from a namespace the project
// reconciler resolved as the project's, is the project's shared storage
// claim: nothing controls it and Kipper labelled it a shared volume, without
// the volume-name label a Volume's claim carries.
func projectSharedClaim(pvc *corev1.PersistentVolumeClaim) bool {
	l := pvc.Labels
	_, volumeClaim := l[kipperlabels.VolumeName]
	return metav1.GetControllerOf(pvc) == nil && !volumeClaim && l[kipperLabel] == kipperValue &&
		l[kipperlabels.ResourceType] == kipperlabels.ResourceTypeSharedVolume
}

func (r *ProjectReconciler) longhornReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// projectCopiesCommand is how an admin records copies on a project's shared
// storage. No kip command sets them yet.
func projectCopiesCommand(project string, copies int32) string {
	// Messages wrap the command in single quotes, so the patch uses double ones.
	return fmt.Sprintf(`kubectl patch project %s --type merge -p "{\"spec\":{\"sharedStorage\":{\"copies\":%d}}}"`, project, copies)
}

// envCopies is one environment's shared storage StorageCopies condition.
type envCopies struct {
	env  string
	cond metav1.Condition
}

// projectCopiesCondition combines a non-empty list of environment conditions.
// It joins every environment's message in input order and selects the first
// running conversion so admission can refuse another copy count change.
// Otherwise, it selects the first condition with the least healthy status.
func projectCopiesCondition(envs []envCopies) metav1.Condition {
	rank := map[metav1.ConditionStatus]int{metav1.ConditionFalse: 0, metav1.ConditionUnknown: 1, metav1.ConditionTrue: 2}
	worst := envs[0].cond
	var messages []string
	for _, e := range envs {
		converting := e.cond.Reason == kipperv1.StorageCopiesConverting
		if worst.Reason != kipperv1.StorageCopiesConverting && (converting || rank[e.cond.Status] < rank[worst.Status]) {
			worst = e.cond
		}
		messages = append(messages, e.env+": "+e.cond.Message)
	}
	worst.Message = strings.Join(messages, " ")
	return worst
}
