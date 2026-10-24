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
	"github.com/getkipper/kipper/console-api/internal/nsowner"
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
// one step per reconcile. Reader must read uncached. Writer patches Longhorn's
// objects, the claim and the StatefulSet and writes the conversion Lease.
// Longhorn handles replica deletion. StatefulSet names the service's StatefulSet,
// which stops while the conversion needs the volume detached, and is empty for claims
// whose users Kipper does not stop. Holder names Kipper's backup holder pod for
// the claim, which stops while the claim carries a copy-change stop.
type storageConverter struct {
	Reader      client.Reader
	Writer      client.Client
	Now         func() time.Time
	Commands    storageCommands
	StatefulSet string
	Holder      string
}

// storageCommands are the commands a conversion's messages name for the
// object that owns the claim. Copies is the command that sets its copies,
// without the count. Empty commands are omitted.
type storageCommands struct {
	Copies string
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

// convert takes one step towards wanted copies on pvc's Longhorn volume.
// The caller must establish that its object owns pvc. Unbound claims, dump
// claims, local storage and platform namespaces are skipped. Otherwise,
// once the volume is no longer held, the count and data locality the hold
// changed are put back before anything else, as raiseUnheldCount describes. A
// reduction, or a change that needs Longhorn's revision counter switched on,
// first stops Kipper's users of the claim, as stopFirst describes. A counter
// switch begun under a stop finishes before the volume can restart. Once no
// step needs the stop, it is undone before the next step.
// After an uncached read or a write, pvc holds the current claim. It returns a
// StorageCopies condition for the step, or nil when there is no conversion to
// report, including unset copies, a missing or deleting volume, a volume
// belonging to another claim, placement-only updates, completed conversions
// and Longhorn's own rebuilds. The caller then reports live copies. Read and
// write failures are returned as errors.
func (s storageConverter) convert(ctx context.Context, pvc *corev1.PersistentVolumeClaim, wanted *int32) (*metav1.Condition, error) {
	if pvc.Spec.VolumeName == "" || pvc.Labels[dumpForLabel] != "" || platformNamespace(pvc.Namespace) ||
		(pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName == platform.LocalPathClass) {
		return nil, nil
	}
	if wanted == nil && pvc.Annotations[platform.CopyChangeStopAnnotation] == "" {
		return nil, nil
	}
	// The stop record decides what the pass undoes, so it is read past the
	// cache, which may not hold the last pass's write yet.
	if err := s.Reader.Get(ctx, types.NamespacedName{Namespace: pvc.Namespace, Name: pvc.Name}, pvc); err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading claim %s: %w", pvc.Name, err)
	}
	v, c, err := s.read(ctx, pvc)
	if err != nil {
		return nil, err
	}
	if raised, err := s.raiseUnheldCount(ctx, v); raised || err != nil {
		if err != nil {
			return nil, err
		}
		// Everything read before the raise is out of date.
		return s.convert(ctx, pvc, wanted)
	}
	if v != nil && v.Records.Stop != nil && !v.CounterOn && !v.CounterOff {
		// An engine started with the counter on for some copies and off for
		// others marks copies ERR, so a switch begun under this stop is
		// finished before anything starts the volume, whatever the target.
		return s.stopFirst(ctx, pvc, v, c, conversionStep{})
	}
	if v == nil || wanted == nil {
		_, err := s.restore(ctx, pvc, v)
		return nil, err
	}
	target := int64(*wanted)
	step := planConversion(target, v, c, s.now())
	// The hold is planned only after the room for a server copy is checked,
	// so a change that has no room stops nothing.
	if step.Lease == leaseTake || step.Kind == stepHold {
		if err := s.readCapacity(ctx, c); err != nil {
			return nil, err
		}
		step = planConversion(target, v, c, s.now())
	}
	switchCounter := !v.CounterOn && (step.Hold || step.Lease == leaseTake)
	stopped, running := false, false
	switch {
	case (step.Hold || switchCounter) && step.Kind == stepBlocked && !v.Held:
		// Stopping cannot help with the cause, so nothing more is stopped for
		// it. A stop already made stays until the change is set back.
		if v.Records.Stop != nil {
			if err := s.keepStopped(ctx, pvc.Namespace); err != nil {
				return nil, err
			}
			stopped = true
		} else {
			running = true
		}
	case step.Hold || switchCounter:
		if cond, err := s.stopFirst(ctx, pvc, v, c, step); cond != nil || err != nil {
			return cond, err
		}
		stopped = true
	default:
		patched, err := s.restore(ctx, pvc, v)
		if err != nil {
			return nil, err
		}
		if patched {
			// The step was planned for the count and marker restore changed.
			return s.convert(ctx, pvc, wanted)
		}
		if step.NeedsAttach {
			runs, err := s.runs(ctx, pvc.Namespace)
			if err != nil {
				return nil, err
			}
			if runs {
				step = conversionStep{Kind: stepWait, Reason: kipperv1.StorageCopiesConversionPending,
					Message: fmt.Sprintf("Waiting for %s to attach the volume. This Longhorn version builds copies only while the volume is attached.", s.StatefulSet)}
			}
		}
	}
	backoff := backoffOf(pvc)
	if backoff != nil && backoff.Target != target {
		if err := s.patchClaim(ctx, pvc, map[string]*string{platform.CopiesBackoffAnnotation: nil}); err != nil {
			return nil, err
		}
		backoff = nil
	}
	if until, err := time.Parse(time.RFC3339, backoffUntil(backoff)); step.Lease == leaseTake && err == nil && s.now().Before(until) {
		step = blocked(fmt.Sprintf("Longhorn refused the last request to build a copy: %s. Kipper asks again at %s. Setting the copies back stops the change.",
			backoff.Error, until.UTC().Format(time.RFC3339)))
	}

	switch step.Lease {
	case leaseTake:
		holder, err := s.takeLease(ctx, v.Name)
		if err != nil {
			return nil, err
		}
		if holder != v.Name {
			return &metav1.Condition{Type: kipperv1.ConditionStorageCopies, Status: metav1.ConditionFalse,
				Reason:  kipperv1.StorageCopiesConversionPending,
				Message: s.waitingFor(ctx, pvc, holder) + " This volume's conversion starts once that one is done."}, nil
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

	if step.Kind == stepPatch {
		if err := s.patchVolume(ctx, v, step.Patch); err != nil {
			if step.Lease == leaseTake {
				return s.refusedRebuild(ctx, pvc, v, step, target, backoff, err)
			}
			return nil, err
		}
		if step.Lease == leaseTake && backoff != nil {
			if err := s.patchClaim(ctx, pvc, map[string]*string{platform.CopiesBackoffAnnotation: nil}); err != nil {
				return nil, err
			}
		}
	}
	if step.Reason == "" {
		return nil, nil
	}
	if step.Hold && step.Reason != kipperv1.StorageCopiesConverting {
		step.Message += " " + s.setBack(true, stopped)
	}
	switch {
	case stopped:
		step.Message = s.stoppedFor() + step.Message
	case running:
		step.Message = s.keepsRunning() + step.Message
	}
	return &metav1.Condition{Type: kipperv1.ConditionStorageCopies, Status: metav1.ConditionFalse, Reason: step.Reason, Message: step.Message}, nil
}

// records reads the conversion records on pvc. An unreadable stop counts as
// a stop without a recorded replica count.
func (s storageConverter) records(pvc *corev1.PersistentVolumeClaim) claimRecords {
	var r claimRecords
	var stop platform.CopyChangeStop
	if found, err := platform.ReadRecord(pvc.Annotations, platform.CopyChangeStopAnnotation, &stop); found {
		if err != nil {
			stop = platform.CopyChangeStop{}
		}
		r.Stop = &stop
	}
	return r
}

// patchClaim updates pvc's annotations only if its resourceVersion still
// matches. Nil values remove annotations. On success, pvc holds the written claim.
func (s storageConverter) patchClaim(ctx context.Context, pvc *corev1.PersistentVolumeClaim, annotations map[string]*string) error {
	raw, err := json.Marshal(map[string]interface{}{"metadata": map[string]interface{}{
		"resourceVersion": pvc.ResourceVersion, "annotations": annotations,
	}})
	if err != nil {
		return err
	}
	if err := s.Writer.Patch(ctx, pvc, client.RawPatch(types.MergePatchType, raw)); err != nil {
		return fmt.Errorf("recording the conversion on claim %s: %w", pvc.Name, err)
	}
	return nil
}

// backoffOf is the backoff recorded on pvc, or nil when there is none or it
// does not decode.
func backoffOf(pvc *corev1.PersistentVolumeClaim) *platform.CopiesBackoff {
	var b platform.CopiesBackoff
	if found, err := platform.ReadRecord(pvc.Annotations, platform.CopiesBackoffAnnotation, &b); !found || err != nil {
		return nil
	}
	return &b
}

func backoffUntil(b *platform.CopiesBackoff) string {
	if b == nil {
		return ""
	}
	return b.Until
}

// refusedRebuild handles a rebuild patch that failed with err. Only a definite
// refusal, proven by a re-read showing that this pass's rebuild annotation
// did not land, frees the lease for other volumes. The volume then waits out
// a backoff for the same target, recorded on pvc, and reports Blocked. Any
// other failure keeps the lease, since the patch may still land, and is
// returned.
func (s storageConverter) refusedRebuild(ctx context.Context, pvc *corev1.PersistentVolumeClaim, v *lhVolume, step conversionStep, target int64, backoff *platform.CopiesBackoff, err error) (*metav1.Condition, error) {
	if !errors.IsForbidden(err) && !errors.IsInvalid(err) && !errors.IsBadRequest(err) && !errors.IsNotFound(err) {
		return nil, err
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(longhornVolumeKind)
	if rerr := s.Reader.Get(ctx, types.NamespacedName{Namespace: longhornNamespace, Name: v.Name}, obj); rerr != nil && !absentKind(rerr) {
		return nil, err
	}
	if step.Patch == nil || step.Patch.Metadata == nil {
		return nil, err
	}
	if started := step.Patch.Metadata.Annotations[copiesRebuildAnnotation]; started == nil || obj.GetAnnotations()[copiesRebuildAnnotation] == *started {
		return nil, err
	}
	if rerr := s.releaseLease(ctx, v.Name); rerr != nil {
		return nil, rerr
	}
	failures := 1
	if backoff != nil {
		failures = backoff.Failures + 1
	}
	until := s.now().Add(platform.BackoffDelay(failures)).UTC().Format(time.RFC3339)
	raw, merr := json.Marshal(platform.CopiesBackoff{Target: target, Until: until, Failures: failures, Error: err.Error()})
	if merr != nil {
		return nil, merr
	}
	value := string(raw)
	if perr := s.patchClaim(ctx, pvc, map[string]*string{platform.CopiesBackoffAnnotation: &value}); perr != nil {
		return nil, perr
	}
	return &metav1.Condition{Type: kipperv1.ConditionStorageCopies, Status: metav1.ConditionFalse, Reason: kipperv1.StorageCopiesBlocked,
		Message: fmt.Sprintf("Longhorn refused the request to build a copy: %v. Kipper asks again at %s. Setting the copies back stops the change.", err, until)}, nil
}

// waitingFor names the volume holding the conversion lease for a message on
// pvc's object. A volume of another project is not named, since the message
// is visible to pvc's project.
func (s storageConverter) waitingFor(ctx context.Context, pvc *corev1.PersistentVolumeClaim, holder string) string {
	unnamed := "Waiting for another volume's copy change to finish."
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(longhornVolumeKind)
	if err := s.Reader.Get(ctx, types.NamespacedName{Namespace: longhornNamespace, Name: holder}, obj); err != nil {
		return unnamed
	}
	ns, _, _ := unstructured.NestedString(obj.Object, "status", "kubernetesStatus", "namespace")
	claim, _, _ := unstructured.NestedString(obj.Object, "status", "kubernetesStatus", "pvcName")
	if ns == "" || claim == "" {
		return unnamed
	}
	if ns != pvc.Namespace {
		own, ok, err := nsowner.Of(ctx, s.Reader, pvc.Namespace)
		if err != nil || !ok {
			return unnamed
		}
		theirs, ok, err := nsowner.Of(ctx, s.Reader, ns)
		if err != nil || !ok || theirs != own {
			return "Waiting for the copy change of a volume in another project to finish."
		}
	}
	return fmt.Sprintf("Waiting for the copy change of %s in %s to finish.", claimWorkload(claim), ns)
}

// claimWorkload is the service or shared volume a Kipper claim belongs to,
// or the claim's own name.
func claimWorkload(claim string) string {
	if name, ok := strings.CutPrefix(claim, "shared-"); ok {
		return name
	}
	if name, ok := strings.CutPrefix(claim, "data-"); ok && strings.HasSuffix(name, "-0") {
		return strings.TrimSuffix(name, "-0")
	}
	return claim
}

// convertOrBlock calls convert and always returns a condition for a failed
// pass, so the status never keeps describing an earlier state. A conflict
// means the volume or claim changed since it was read, so the whole pass is
// run again, reading and planning afresh, up to conversionTries times.
// Conflicts that persist, timeouts, throttling and a failed admission webhook
// call report ConversionPending, the last with the API server's error. A
// webhook that denies the request reports Blocked like other errors.
// Other errors are logged and report Blocked, so users can reset the copy
// count while the failure persists, which a Converting condition would
// prevent in kip and admission policy.
func (s storageConverter) convertOrBlock(ctx context.Context, pvc *corev1.PersistentVolumeClaim, wanted *int32) (*metav1.Condition, error) {
	c, err := s.convert(ctx, pvc, wanted)
	for try := 1; try < conversionTries && errors.IsConflict(err); try++ {
		c, err = s.convert(ctx, pvc, wanted)
	}
	if err == nil {
		return c, nil
	}
	transient := errors.IsConflict(err) || errors.IsTimeout(err) || errors.IsServerTimeout(err) || errors.IsTooManyRequests(err)
	if !transient {
		log.FromContext(ctx).Error(err, "Could not convert storage copies.", "namespace", pvc.Namespace, "claim", pvc.Name)
	}
	if errors.IsInternalError(err) && strings.Contains(err.Error(), "failed calling webhook") {
		return &metav1.Condition{Type: kipperv1.ConditionStorageCopies, Status: metav1.ConditionFalse, Reason: kipperv1.StorageCopiesConversionPending,
			Message: fmt.Sprintf("A webhook call failed, so Kipper retries: %v", err)}, nil
	}
	if transient {
		return &metav1.Condition{Type: kipperv1.ConditionStorageCopies, Status: metav1.ConditionFalse, Reason: kipperv1.StorageCopiesConversionPending,
			Message: fmt.Sprintf("Kipper could not complete the next step of the copy change: %v. It reads the volume again and retries.", err)}, nil
	}
	return &metav1.Condition{Type: kipperv1.ConditionStorageCopies, Status: metav1.ConditionFalse, Reason: kipperv1.StorageCopiesBlocked,
		Message: fmt.Sprintf("Kipper could not take the next step of the copy change: %v. Kipper will retry. Setting the copies back stops the change.", err)}, nil
}

// conversionTries limits conversion attempts per pass, including the initial
// attempt. Only conflicts trigger another attempt within the same pass.
const conversionTries = 3

// read loads pvc's Longhorn volume, replicas, engines, attachment, node tags
// and availability, support for rebuilding detached volumes and the claim's
// conversion records. Replica modes are accepted only when the active engine
// is running and both its node and its status owner's node pass
// engineNodeProblem. Disk capacity is read separately. A nil volume means it
// is absent, being deleted or belongs to another claim.
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
	v.Records = s.records(pvc)

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
	var engine *unstructured.Unstructured
	for i, e := range engines.Items {
		disabled, _, _ := unstructured.NestedBool(e.Object, "spec", "revisionCounterDisabled")
		v.Engines = append(v.Engines, lhEngineRef{Name: e.GetName(), ResourceVersion: e.GetResourceVersion(), CounterDisabled: disabled})
		if active, _, _ := unstructured.NestedBool(e.Object, "spec", "active"); active {
			engine = &engines.Items[i]
			v.EngineFound = true
			v.Addresses, _, _ = unstructured.NestedStringMap(e.Object, "status", "currentReplicaAddressMap")
		}
	}
	v.CounterOn, v.CounterOff = counterState(v)
	attachment, err := s.readAttachment(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	v.Attachment = attachment

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
	markHomeEvicted(v.Replicas, nodes.Items)
	nodeNames := []string{}
	for _, r := range v.Replicas {
		nodeNames = append(nodeNames, r.Node)
	}
	if v.Attachment != nil {
		nodeNames = append(nodeNames, v.Attachment.TicketNode)
	}
	for _, node := range nodeNames {
		if _, seen := c.Available[node]; node != "" && !seen {
			c.Available[node] = readHolder(ctx, s.Reader, node) == holderAvailable
		}
	}
	if engine == nil && v.State == "attached" {
		v.EngineUntrusted = "Longhorn reports no active engine for the volume"
	}
	if engine != nil {
		// The engine's status is written by the longhorn-manager on ownerID.
		// The engine's node and that manager's node must both be able to
		// report, or the status may be the last one before one of them failed.
		state, _, _ := unstructured.NestedString(engine.Object, "status", "currentState")
		node, _, _ := unstructured.NestedString(engine.Object, "spec", "nodeID")
		owner, _, _ := unstructured.NestedString(engine.Object, "status", "ownerID")
		doubt := ""
		switch {
		case state == "":
			doubt = "Longhorn reports no state for the engine"
		case state != "running":
			doubt = "Longhorn reports the engine as " + state
		default:
			if doubt = s.engineNodeProblem(ctx, node, c); doubt == "" {
				doubt = s.engineNodeProblem(ctx, owner, c)
			}
		}
		if doubt == "" {
			v.Modes, _, _ = unstructured.NestedStringMap(engine.Object, "status", "replicaModeMap")
			v.EngineNode, v.EngineOwner = node, owner
		} else if v.State != "detached" {
			v.EngineUntrusted = doubt
		}
	}
	if a := v.Attachment; a != nil && a.TicketNode != "" {
		node := a.TicketNode
		v.Held = a.TicketSatisfied && v.State == "attached" && v.AttachedNode == node && v.FrontendDisabled &&
			v.EngineNode == node && v.EngineOwner == node && v.CounterOn
	}
	version, err := s.setting(ctx, "current-longhorn-version")
	if err != nil {
		return nil, nil, err
	}
	c.OfflineRebuilding = versionAtLeast(version, 1, 9)
	return v, c, nil
}

// counterState reports whether the volume, every engine and every replica not
// being deleted have Longhorn's revision counter on, and whether none has.
func counterState(v *lhVolume) (on, off bool) {
	on, off = !v.CounterDisabled, v.CounterDisabled
	for _, e := range v.Engines {
		on, off = on && !e.CounterDisabled, off && e.CounterDisabled
	}
	for _, r := range v.Replicas {
		if !r.Deleting {
			on, off = on && !r.CounterDisabled, off && r.CounterDisabled
		}
	}
	return on, off
}

// engineNodeProblem returns an empty string when node can report an engine's
// status, or the reason it cannot. The Kubernetes node must be Ready and
// unfenced, and the Longhorn node must be Ready, which reflects its manager's
// availability, including a down or missing manager pod. Read errors and
// missing nodes fail these checks.
func (s storageConverter) engineNodeProblem(ctx context.Context, node string, c *lhCluster) string {
	if node == "" {
		return "Longhorn names no node for the engine"
	}
	var kube corev1.Node
	if err := s.Reader.Get(ctx, types.NamespacedName{Name: node}, &kube); err != nil {
		return "node " + node + " could not be read"
	}
	if !platform.NodeAvailable(&kube) {
		return "node " + node + " is not Ready or is fenced"
	}
	i := slices.IndexFunc(c.nodes, func(n unstructured.Unstructured) bool { return n.GetName() == node })
	if i < 0 {
		return "Longhorn has no node " + node
	}
	if ready := longhornCondition(c.nodes[i], "Ready"); !ready.ok {
		if ready.reason != "" {
			return fmt.Sprintf("Longhorn reports node %s not Ready (%s)", node, ready.reason)
		}
		return "Longhorn reports node " + node + " not Ready"
	}
	return ""
}

func volumeFrom(obj *unstructured.Unstructured) *lhVolume {
	str := func(path ...string) string {
		s, _, _ := unstructured.NestedString(obj.Object, path...)
		return s
	}
	count, _, _ := unstructured.NestedInt64(obj.Object, "spec", "numberOfReplicas")
	selector, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "nodeSelector")
	v := &lhVolume{
		Name: obj.GetName(), UID: string(obj.GetUID()), ResourceVersion: obj.GetResourceVersion(), Count: count, Selector: selector,
		Robustness: str("status", "robustness"),
		Locality:   str("spec", "dataLocality"), AutoBalance: str("spec", "replicaAutoBalance"),
		OfflineRebuilding: str("spec", "offlineRebuilding"),
		Size:              numberField(obj.Object, "spec", "size"), Actual: numberField(obj.Object, "status", "actualSize"),
		State: str("status", "state"), AttachedNode: str("status", "currentNodeID"),
		CounterDisabled: boolField(obj, "spec", "revisionCounterDisabled"), FrontendDisabled: boolField(obj, "status", "frontendDisabled"),
		Image: str("spec", "image"), CurrentImage: str("status", "currentImage"),
		MigrationNodeID: str("spec", "migrationNodeID"), CurrentMigrationNodeID: str("status", "currentMigrationNodeID"),
		Seeded: platform.CopyVolumeOf(obj.Object).Seeded,
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

// markHomeEvicted sets HomeEvicted on each replica whose Longhorn node asks
// for eviction, or whose disk does, matched as Longhorn matches them, by the
// replica's diskID and the disk's status diskUUID.
func markHomeEvicted(replicas []lhReplica, nodes []unstructured.Unstructured) {
	evictedNodes, evictedDisks := map[string]bool{}, map[string]bool{}
	for _, n := range nodes {
		evictedNodes[n.GetName()] = boolField(&n, "spec", "evictionRequested")
		disks, _, _ := unstructured.NestedMap(n.Object, "spec", "disks")
		for disk := range disks {
			uuid, _, _ := unstructured.NestedString(n.Object, "status", "diskStatus", disk, "diskUUID")
			if uuid != "" && boolField(&n, "spec", "disks", disk, "evictionRequested") {
				evictedDisks[uuid] = true
			}
		}
	}
	for i := range replicas {
		r := &replicas[i]
		r.HomeEvicted = evictedNodes[r.Node] || (r.DiskID != "" && evictedDisks[r.DiskID])
	}
}

func replicaFrom(obj *unstructured.Unstructured) lhReplica {
	str := func(field string) string {
		s, _, _ := unstructured.NestedString(obj.Object, "spec", field)
		return s
	}
	evicting, _, _ := unstructured.NestedBool(obj.Object, "spec", "evictionRequested")
	state, _, _ := unstructured.NestedString(obj.Object, "status", "currentState")
	return lhReplica{
		Name: obj.GetName(), UID: string(obj.GetUID()), ResourceVersion: obj.GetResourceVersion(),
		Node: str("nodeID"), DiskID: str("diskID"), HealthyAt: str("healthyAt"), FailedAt: str("failedAt"), LastFailedAt: str("lastFailedAt"),
		LastHealthyAt: str("lastHealthyAt"), CurrentState: state,
		EvictionRequested: evicting, Deleting: obj.GetDeletionTimestamp() != nil,
		CounterDisabled: boolField(obj, "spec", "revisionCounterDisabled"),
	}
}

func boolField(obj *unstructured.Unstructured, path ...string) bool {
	b, _, _ := unstructured.NestedBool(obj.Object, path...)
	return b
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

// takeLease makes holder the conversion Lease's holder and returns the holder
// afterwards, which is another volume's name when holder had to wait. It
// returns an empty name after a creation race or update conflict. A free
// lease or holder's own is taken. Another holder's lease is taken over only
// once it has expired and that holder's volume has no Kipper rebuild within
// its time budget. An earlier rebuild may continue after its budget expires.
func (s storageConverter) takeLease(ctx context.Context, holder string) (string, error) {
	now := metav1.NewMicroTime(s.now())
	seconds := int32(conversionLeaseSeconds)
	var lease coordinationv1.Lease
	key := types.NamespacedName{Namespace: conversionLeaseNamespace, Name: conversionLeaseName}
	if err := s.Reader.Get(ctx, key, &lease); err != nil {
		if !errors.IsNotFound(err) {
			return "", fmt.Errorf("reading the conversion lease: %w", err)
		}
		lease = coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Namespace: conversionLeaseNamespace, Name: conversionLeaseName},
			Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &seconds, AcquireTime: &now, RenewTime: &now},
		}
		if err := s.Writer.Create(ctx, &lease); err != nil {
			if errors.IsAlreadyExists(err) {
				return "", nil
			}
			return "", fmt.Errorf("creating the conversion lease: %w", err)
		}
		return holder, nil
	}
	current := ""
	if lease.Spec.HolderIdentity != nil {
		current = *lease.Spec.HolderIdentity
	}
	if current != "" && current != holder {
		if !leaseExpired(&lease, now.Time) {
			return current, nil
		}
		busy, err := s.rebuildOutstanding(ctx, current)
		if err != nil {
			return "", err
		}
		if busy {
			return current, nil
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
			return "", nil
		}
		return "", fmt.Errorf("updating the conversion lease: %w", err)
	}
	return holder, nil
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
