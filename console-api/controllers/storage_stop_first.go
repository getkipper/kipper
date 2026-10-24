package controllers

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/longhorncap"
	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/controller/pkg/stranded"
)

// conversionTicket is the ID of the Longhorn attachment ticket through which
// a copy change attaches a volume on the server with its frontend disabled.
// Its type is the one Longhorn gives attachments made through its API, which
// an attachment for a pod cannot interrupt.
const (
	conversionTicket     = "kipper-copy-change"
	conversionTicketType = "longhorn-api"
)

var (
	longhornAttachmentKind = schema.GroupVersionKind{Group: "longhorn.io", Version: "v1beta2", Kind: "VolumeAttachment"}
	longhornEngineKind     = schema.GroupVersionKind{Group: "longhorn.io", Version: "v1beta2", Kind: "Engine"}
)

// readAttachment reads the Longhorn VolumeAttachment of the volume name, or
// returns nil when Longhorn has none.
func (s storageConverter) readAttachment(ctx context.Context, name string) (*lhAttachment, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(longhornAttachmentKind)
	if err := s.Reader.Get(ctx, types.NamespacedName{Namespace: longhornNamespace, Name: name}, obj); err != nil {
		if absentKind(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading the Longhorn attachment of %s: %w", name, err)
	}
	a := &lhAttachment{ResourceVersion: obj.GetResourceVersion()}
	ticket, found, _ := unstructured.NestedMap(obj.Object, "spec", "attachmentTickets", conversionTicket)
	if !found {
		return a, nil
	}
	a.TicketNode, _ = ticket["nodeID"].(string)
	if status, found, _ := unstructured.NestedMap(obj.Object, "status", "attachmentTicketStatuses", conversionTicket); found {
		satisfied, _ := status["satisfied"].(bool)
		a.TicketSatisfied = satisfied && longhorncap.Int64Field(status, "generation") == longhorncap.Int64Field(ticket, "generation")
	}
	return a, nil
}

// stopFirst prepares pvc's volume for a reduction or a step that requires
// Longhorn's revision counter. It records a stop on pvc and stops the
// StatefulSet, waits for the volume to detach, switches the counter on while
// detached and, for a reduction, attaches the volume on the server through
// Kipper's ticket with the frontend disabled. Each pass takes the next of these
// steps from what Kubernetes and Longhorn record, so a restarted console-api
// resumes where the last one stopped. It returns the condition to report, or
// nil once the volume is held and step may run.
func (s storageConverter) stopFirst(ctx context.Context, pvc *corev1.PersistentVolumeClaim, v *lhVolume, c *lhCluster, step conversionStep) (*metav1.Condition, error) {
	if v.Records.Stop == nil {
		if cond, err := s.stop(ctx, pvc, step.Hold); cond != nil || err != nil {
			return cond, err
		}
	} else if err := s.keepStopped(ctx, pvc.Namespace); err != nil {
		return nil, err
	}
	ticket := ""
	if v.Attachment != nil {
		ticket = v.Attachment.TicketNode
	}
	switch {
	case v.Held && step.Hold:
		return nil, nil
	case ticket != "" && (!v.CounterOn || !step.Hold):
		// An engine started before the counter was on compared nothing, so
		// the volume detaches and the counter goes on first.
		if err := s.dropTicket(ctx, v); err != nil {
			return nil, err
		}
		return s.pending("Kipper released the volume and waits for it to detach.", step.Hold), nil
	case ticket != "":
		if !c.Available[ticket] {
			return s.blocked(fmt.Sprintf("Kipper attaches the volume on %s to compare its copies, and %s is not Ready or is fenced. The change continues once it is back.", ticket, ticket), step.Hold), nil
		}
		return s.pending(attachingMessage(ticket), step.Hold), nil
	case v.State != "detached":
		return s.waitForDetach(ctx, pvc, v, step.Hold)
	case !v.CounterOn:
		return s.switchCounter(ctx, v, step.Hold)
	case v.Attachment == nil:
		return s.blocked("Longhorn keeps no attachment record for this volume, so Kipper cannot attach it on the server to compare its copies.", step.Hold), nil
	}
	node := s.conversionNode(ctx, v, c)
	if node == "" {
		return s.blocked("Kipper attaches the volume on the server to compare its copies, and no server node is Ready. The change continues once one is.", step.Hold), nil
	}
	// Longhorn may drop API attachment tickets on cordoned nodes.
	cordoned, err := s.cordoned(ctx, node)
	if err != nil {
		return nil, err
	}
	if cordoned {
		return s.blocked(fmt.Sprintf("%s is cordoned, so Kipper does not attach the volume there to compare its copies. The change continues once %s is uncordoned.", node, node), step.Hold), nil
	}
	if err := s.addTicket(ctx, v, node); err != nil {
		return nil, err
	}
	return s.pending(attachingMessage(node), step.Hold), nil
}

func attachingMessage(node string) string {
	return fmt.Sprintf("Longhorn attaches the volume on %s with nothing able to write to it, so its engine compares the copies before Longhorn removes the extra ones.", node)
}

// pending is a ConversionPending condition for a stop-first step, which says
// that the service is stopped and how to stop the change. hold marks a
// reduction.
func (s storageConverter) pending(message string, hold bool) *metav1.Condition {
	return &metav1.Condition{Type: kipperv1.ConditionStorageCopies, Status: metav1.ConditionFalse, Reason: kipperv1.StorageCopiesConversionPending,
		Message: s.stoppedFor() + message + " " + s.setBack(hold, true)}
}

// blocked is a Blocked condition for a stop-first step, phrased as pending is.
func (s storageConverter) blocked(message string, hold bool) *metav1.Condition {
	return &metav1.Condition{Type: kipperv1.ConditionStorageCopies, Status: metav1.ConditionFalse, Reason: kipperv1.StorageCopiesBlocked,
		Message: s.stoppedFor() + message + " " + s.setBack(hold, true)}
}

// blockedBeforeStop is a Blocked condition for a change that stopped nothing.
func (s storageConverter) blockedBeforeStop(message string, hold bool) *metav1.Condition {
	return &metav1.Condition{Type: kipperv1.ConditionStorageCopies, Status: metav1.ConditionFalse, Reason: kipperv1.StorageCopiesBlocked,
		Message: s.keepsRunning() + message + " " + s.setBack(hold, false)}
}

// stoppedMarker and inUseMarker are in the messages of a change that keeps a
// service stopped or waits for pods that use the volume, which storageRefresh
// reads again soon.
const (
	stoppedMarker = "is stopped for the copy change"
	inUseMarker   = "The volume is in use by"
)

func (s storageConverter) stoppedFor() string {
	if s.StatefulSet == "" {
		return ""
	}
	return s.StatefulSet + " " + stoppedMarker + " and starts again once it is done. "
}

func (s storageConverter) keepsRunning() string {
	if s.StatefulSet == "" {
		return ""
	}
	return s.StatefulSet + " keeps running. "
}

// setBack explains how to cancel the change and whether the service restarts.
// Canceling a reduction requests two copies and requires an admin when the
// cluster offers one. Canceling an increase requests one copy. If Longhorn
// has built a server copy, a service keeps it and ends at two copies, while a
// shared volume keeps every copy.
func (s storageConverter) setBack(hold, stopped bool) string {
	way := "Setting the copies back"
	if s.Commands.Copies != "" {
		back := 1
		if hold {
			back = 2
		}
		way += fmt.Sprintf(" with '%s %d'", s.Commands.Copies, back)
	}
	switch {
	case !stopped || s.StatefulSet == "":
		way += " stops the change and keeps every copy."
	case hold:
		way += fmt.Sprintf(" starts %s again at two copies. If Longhorn has built the server copy for the change already, Kipper keeps it, and Longhorn removes one worker copy first.", s.StatefulSet)
	default:
		way += fmt.Sprintf(" starts %s again and keeps every copy.", s.StatefulSet)
	}
	if hold {
		way += " On a cluster that offers one copy, setting the copies back needs an admin."
	}
	return way
}

// stop records the stop on pvc before stopping the StatefulSet so the stop
// can be recovered after a restart. Active pods outside Kipper's control block
// the stop and are named in the returned condition. The record saves at least
// one replica, as kip node fence does, so a service left at zero by fencing
// can run when started again.
func (s storageConverter) stop(ctx context.Context, pvc *corev1.PersistentVolumeClaim, hold bool) (*metav1.Condition, error) {
	users, _, err := s.claimUsers(ctx, pvc)
	if err != nil {
		return nil, err
	}
	if len(users) > 0 {
		return s.blockedBeforeStop(inUse(users), hold), nil
	}
	rec := platform.CopyChangeStop{At: s.now().UTC().Format(time.RFC3339)}
	if s.StatefulSet != "" {
		sts, err := s.statefulSet(ctx, pvc.Namespace)
		if err != nil {
			return nil, err
		}
		replicas := int32(1)
		if sts.Spec.Replicas != nil {
			replicas = max(*sts.Spec.Replicas, 1)
		}
		rec.Replicas = &replicas
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	value := string(raw)
	if err := s.patchClaim(ctx, pvc, map[string]*string{platform.CopyChangeStopAnnotation: &value}); err != nil {
		return nil, err
	}
	return nil, s.keepStopped(ctx, pvc.Namespace)
}

// inUse explains a wait for pods that Kipper does not stop.
func inUse(users []string) string {
	return fmt.Sprintf("%s %s. Kipper stops only what it owns, so the change continues once nothing else uses the volume.", inUseMarker, podList(users))
}

func podList(pods []string) string {
	if len(pods) == 1 {
		return "pod " + pods[0]
	}
	return "pods " + strings.Join(pods, ", ")
}

// keepStopped scales the StatefulSet to zero unless it is there already, so
// a scale-up by anything else is undone while the stop lasts.
func (s storageConverter) keepStopped(ctx context.Context, namespace string) error {
	if s.StatefulSet == "" {
		return nil
	}
	sts, err := s.statefulSet(ctx, namespace)
	if err != nil {
		return err
	}
	if sts.Spec.Replicas != nil && *sts.Spec.Replicas == 0 {
		return nil
	}
	return s.scale(ctx, sts, 0)
}

// runs reports whether the claim's StatefulSet is set to run, so its pod
// attaches the volume. It is false without a StatefulSet.
func (s storageConverter) runs(ctx context.Context, namespace string) (bool, error) {
	if s.StatefulSet == "" {
		return false, nil
	}
	sts, err := s.statefulSet(ctx, namespace)
	if err != nil {
		return false, err
	}
	return sts.Spec.Replicas == nil || *sts.Spec.Replicas > 0, nil
}

func (s storageConverter) statefulSet(ctx context.Context, namespace string) (*appsv1.StatefulSet, error) {
	var sts appsv1.StatefulSet
	if err := s.Reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: s.StatefulSet}, &sts); err != nil {
		return nil, fmt.Errorf("reading the StatefulSet %s: %w", s.StatefulSet, err)
	}
	return &sts, nil
}

// scale sets sts's replicas only if its resourceVersion still matches.
func (s storageConverter) scale(ctx context.Context, sts *appsv1.StatefulSet, replicas int32) error {
	raw, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{"resourceVersion": sts.ResourceVersion},
		"spec":     map[string]interface{}{"replicas": replicas},
	})
	if err != nil {
		return err
	}
	if err := s.Writer.Patch(ctx, sts, client.RawPatch(types.MergePatchType, raw)); err != nil {
		return fmt.Errorf("scaling the StatefulSet %s to %d: %w", sts.Name, replicas, err)
	}
	return nil
}

// waitForDetach reports the wait for v to detach. Pods that Kipper does not
// stop and an attachment node that cannot report block it.
func (s storageConverter) waitForDetach(ctx context.Context, pvc *corev1.PersistentVolumeClaim, v *lhVolume, hold bool) (*metav1.Condition, error) {
	users, terminating, err := s.claimUsers(ctx, pvc)
	if err != nil {
		return nil, err
	}
	if len(users) > 0 {
		return s.blocked(inUse(users), hold), nil
	}
	if node := v.AttachedNode; node != "" && readHolder(ctx, s.Reader, node) != holderAvailable {
		return s.blocked(fmt.Sprintf("The volume is still attached on %s, which is not Ready or is fenced, so it cannot detach. "+
			"The change continues once the node is back. If %s is gone for good, run 'kip node fence %s'.", node, node, node), hold), nil
	}
	if len(terminating) > 0 {
		return s.pending(fmt.Sprintf("Waiting for %s to finish terminating and the volume to detach.", podList(terminating)), hold), nil
	}
	return s.pending("Waiting for the volume to detach.", hold), nil
}

// claimUsers returns sorted names of active pods in pvc's namespace that
// mount the claim, excluding the service's StatefulSet pods and backup holder.
// Pods being deleted are returned separately as terminating because they
// keep the volume attached until termination finishes.
func (s storageConverter) claimUsers(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (users, terminating []string, err error) {
	var pods corev1.PodList
	if err := s.Reader.List(ctx, &pods, client.InNamespace(pvc.Namespace)); err != nil {
		return nil, nil, fmt.Errorf("listing the pods in %s: %w", pvc.Namespace, err)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		ended := p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
		if ended || (s.Holder != "" && p.Name == s.Holder) || s.ownPod(p) || !mountsClaim(p, pvc.Name) {
			continue
		}
		if p.DeletionTimestamp != nil {
			terminating = append(terminating, p.Name)
			continue
		}
		users = append(users, p.Name)
	}
	slices.Sort(users)
	slices.Sort(terminating)
	return users, terminating, nil
}

func (s storageConverter) ownPod(p *corev1.Pod) bool {
	ref := metav1.GetControllerOf(p)
	return s.StatefulSet != "" && ref != nil && ref.Kind == "StatefulSet" && ref.Name == s.StatefulSet
}

func mountsClaim(p *corev1.Pod, claim string) bool {
	return slices.ContainsFunc(p.Spec.Volumes, func(v corev1.Volume) bool {
		return v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim
	})
}

// switchCounter turns Longhorn's revision counter on for the detached volume
// v: on every replica not being deleted, every engine and the volume itself.
// A volume with the counter off everywhere is switched only when Longhorn's
// records show no failed copy and the volume is not faulted, since the first
// engine start afterward has no counters to compare. A volume switched in
// part by an earlier pass is always completed.
func (s storageConverter) switchCounter(ctx context.Context, v *lhVolume, hold bool) (*metav1.Condition, error) {
	if doubt := counterDoubt(v); v.CounterOff && doubt != "" {
		return s.blocked(fmt.Sprintf("This volume was created without Longhorn's revision counter, which lets Longhorn tell which copy is current. "+
			"Kipper switches it on before the copy change while the volume is detached, and only when Longhorn's records show every copy healthy, "+
			"but %s, so Kipper keeps every copy. Once Longhorn has rebuilt that copy, the change can be made again.", doubt), hold), nil
	}
	on := map[string]interface{}{"revisionCounterDisabled": false}
	for _, r := range v.Replicas {
		if r.CounterDisabled && !r.Deleting {
			if err := s.patchLonghorn(ctx, longhornReplicaKind, r.Name, r.ResourceVersion, on); err != nil {
				return nil, err
			}
		}
	}
	for _, e := range v.Engines {
		if e.CounterDisabled {
			if err := s.patchLonghorn(ctx, longhornEngineKind, e.Name, e.ResourceVersion, on); err != nil {
				return nil, err
			}
		}
	}
	if v.CounterDisabled {
		if err := s.patchLonghorn(ctx, longhornVolumeKind, v.Name, v.ResourceVersion, on); err != nil {
			return nil, err
		}
	}
	return s.pending("Kipper switched on Longhorn's revision counter for this volume while it was detached, so Longhorn can tell which copy is current from now on.", hold), nil
}

// counterDoubt is why Longhorn's records do not show every copy of v healthy,
// or an empty string when they do.
func counterDoubt(v *lhVolume) string {
	if v.Robustness == "faulted" {
		return "Longhorn reports the volume as faulted"
	}
	for _, r := range v.Replicas {
		node := cmp.Or(r.Node, "an unknown node")
		switch {
		case r.Deleting:
		case r.FailedAt != "":
			return "Longhorn recorded a failure of the copy on " + node
		case r.CurrentState == "error":
			return "Longhorn reports the copy on " + node + " in error"
		}
	}
	return ""
}

// patchLonghorn merges spec into a Longhorn object's spec only if its
// resourceVersion still matches.
func (s storageConverter) patchLonghorn(ctx context.Context, kind schema.GroupVersionKind, name, resourceVersion string, spec map[string]interface{}) error {
	raw, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{"resourceVersion": resourceVersion},
		"spec":     spec,
	})
	if err != nil {
		return err
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(kind)
	obj.SetNamespace(longhornNamespace)
	obj.SetName(name)
	if err := s.Writer.Patch(ctx, obj, client.RawPatch(types.MergePatchType, raw)); err != nil {
		return fmt.Errorf("patching Longhorn %s %s: %w", strings.ToLower(kind.Kind), name, err)
	}
	return nil
}

// conversionNode is the node a reduction holds the volume on: the node of the
// first server copy that is not failed, or else the first Ready server node by
// name. It is empty when there is none.
func (s storageConverter) conversionNode(ctx context.Context, v *lhVolume, c *lhCluster) string {
	for _, r := range v.Replicas {
		if c.Servers[r.Node] && r.FailedAt == "" && !r.Deleting {
			return r.Node
		}
	}
	var servers []string
	for node, server := range c.Servers {
		if server {
			servers = append(servers, node)
		}
	}
	slices.Sort(servers)
	for _, node := range servers {
		if readHolder(ctx, s.Reader, node) == holderAvailable {
			return node
		}
	}
	return ""
}

// cordoned reports whether the Kubernetes node is marked unschedulable. A
// missing node is not.
func (s storageConverter) cordoned(ctx context.Context, name string) (bool, error) {
	var node corev1.Node
	if err := s.Reader.Get(ctx, types.NamespacedName{Name: name}, &node); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading the node %s: %w", name, err)
	}
	return node.Spec.Unschedulable, nil
}

// addTicket adds Kipper's attachment ticket for node to v's attachment, with
// the frontend disabled, only if the attachment is unchanged since read.
func (s storageConverter) addTicket(ctx context.Context, v *lhVolume, node string) error {
	return s.patchAttachment(ctx, v, map[string]interface{}{
		"id": conversionTicket, "type": conversionTicketType, "nodeID": node,
		"parameters": map[string]interface{}{"disableFrontend": "true", "lastAttachedBy": "kipper"},
	})
}

// dropTicket removes Kipper's attachment ticket from v's attachment.
func (s storageConverter) dropTicket(ctx context.Context, v *lhVolume) error {
	return s.patchAttachment(ctx, v, nil)
}

func (s storageConverter) patchAttachment(ctx context.Context, v *lhVolume, ticket map[string]interface{}) error {
	raw, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{"resourceVersion": v.Attachment.ResourceVersion},
		"spec":     map[string]interface{}{"attachmentTickets": map[string]interface{}{conversionTicket: ticket}},
	})
	if err != nil {
		return err
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(longhornAttachmentKind)
	obj.SetNamespace(longhornNamespace)
	obj.SetName(v.Name)
	if err := s.Writer.Patch(ctx, obj, client.RawPatch(types.MergePatchType, raw)); err != nil {
		return fmt.Errorf("changing the Longhorn attachment of %s: %w", v.Name, err)
	}
	return nil
}

// restore undoes a copy-change stop on pvc. While the ticket and the stop
// still hold the volume, it first ends what the hold changed on the volume, as
// endHeldBuild describes, since a set-back ends the reduction. It then drops
// Kipper's attachment ticket, gives the StatefulSet back its recorded replicas
// and only then removes the stop record, so an interrupted restore is
// repeated. It reports whether it patched the volume, which makes a step
// planned from v out of date. v is nil when the volume is gone. A stop whose
// replica count could not be read starts one replica, as every Kipper service
// runs. A StatefulSet that kip node fence marked stays at zero, since only
// kip's checks start it.
func (s storageConverter) restore(ctx context.Context, pvc *corev1.PersistentVolumeClaim, v *lhVolume) (patched bool, err error) {
	stop := s.records(pvc).Stop
	ticket := v != nil && v.Attachment != nil && v.Attachment.TicketNode != ""
	if v != nil && (ticket || stop != nil) {
		if patched, err = s.endHeldBuild(ctx, v); err != nil {
			return false, err
		}
	}
	if ticket {
		if err := s.dropTicket(ctx, v); err != nil {
			return patched, err
		}
	}
	if stop == nil {
		return patched, nil
	}
	if s.StatefulSet != "" {
		replicas := int32(1)
		if stop.Replicas != nil {
			replicas = *stop.Replicas
		}
		var sts appsv1.StatefulSet
		err := s.Reader.Get(ctx, types.NamespacedName{Namespace: pvc.Namespace, Name: s.StatefulSet}, &sts)
		switch {
		case errors.IsNotFound(err):
		case err != nil:
			return patched, fmt.Errorf("reading the StatefulSet %s: %w", s.StatefulSet, err)
		case sts.Annotations[stranded.FenceStopAnnotation] != "":
		case sts.Spec.Replicas == nil || *sts.Spec.Replicas != replicas:
			if err := s.scale(ctx, &sts, replicas); err != nil {
				return patched, err
			}
		}
	}
	return patched, s.patchClaim(ctx, pvc, map[string]*string{platform.CopyChangeStopAnnotation: nil})
}

// endHeldBuild releases v's conversion lease and restores the volume settings
// in one patch. It disables data locality and auto-balance and raises a lowered
// count to the replicas Longhorn counts, preserving them when the hold ends.
// For a server-copy build, it clears the rebuild marker and restores placement
// for two copies when at least two remain. The requested count excludes a new
// copy that Longhorn has yet to place, whether created or not, so it cannot
// land on a worker under the restored placement. It reports whether it patched v.
func (s storageConverter) endHeldBuild(ctx context.Context, v *lhVolume) (bool, error) {
	if err := s.releaseLease(ctx, v.Name); err != nil {
		return false, err
	}
	counted := countedCopies(v)
	marked := !v.RebuildStarted.IsZero()
	if v.Count >= counted && keepsExtraCopies(v) && !marked {
		return false, nil
	}
	if !marked {
		count := max(v.Count, counted)
		return true, s.patchVolume(ctx, v, trimPatch(v, &count))
	}
	var placed int64
	for _, r := range v.Replicas {
		if r.Node != "" && !r.Deleting {
			placed++
		}
	}
	count := max(min(v.Count, placed), counted, 1)
	patch := trimPatch(v, &count)
	if count >= 2 {
		patch.Spec.NodeSelector = targetSelector(2)
	}
	patch.Metadata = &volumePatchMeta{Annotations: map[string]*string{copiesRebuildAnnotation: nil}}
	return true, s.patchVolume(ctx, v, patch)
}

// raiseUnheldCount puts back what the hold changed on v when a copy-change
// stop is recorded and the volume is not held, as a held reduction leaves it
// when Longhorn drops the hold, for example on a cordon. In one patch it turns
// data locality and auto-balance off and raises a count below the copies
// Longhorn counts to them. Outside the hold Longhorn then removes no healthy
// copy except one it evicts, and only while another healthy copy exists. The
// held reduction lowers the count again once it holds the volume. Without a
// stop the count is left alone. It reports whether it patched v.
func (s storageConverter) raiseUnheldCount(ctx context.Context, v *lhVolume) (bool, error) {
	if v == nil || v.Held || v.Records.Stop == nil {
		return false, nil
	}
	counted := countedCopies(v)
	if v.Count >= counted && keepsExtraCopies(v) {
		return false, nil
	}
	count := max(v.Count, counted)
	return true, s.patchVolume(ctx, v, trimPatch(v, &count))
}

// countedCopies counts v's replicas that Longhorn counts against its copy
// count.
func countedCopies(v *lhVolume) int64 {
	var n int64
	for _, r := range v.Replicas {
		if r.counted() {
			n++
		}
	}
	return n
}
