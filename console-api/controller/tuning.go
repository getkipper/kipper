package controller

import (
	"context"
	"fmt"
	"log"
	"maps"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/resourcebounds"
	quotapkg "github.com/getkipper/kipper/console-api/quota"
	"github.com/getkipper/kipper/controller/pkg/labels"
)

// decreaseCooldownAfterOOM blocks CPU and memory decreases after an OOM kill
// raised memory, so quiet checks right after the restart cannot undo it.
const decreaseCooldownAfterOOM = 24 * time.Hour

// workloadOwner is the App, Service or Function a Deployment or StatefulSet
// belongs to.
type workloadOwner struct {
	Kind string
	Name string
	UID  string
}

// ownerOf reads a workload's controlling App, Service or Function.
func ownerOf(obj metav1.Object) (workloadOwner, bool) {
	ref := metav1.GetControllerOf(obj)
	if ref == nil || !strings.HasPrefix(ref.APIVersion, kipperv1.GroupVersion.Group+"/") {
		return workloadOwner{}, false
	}
	switch ref.Kind {
	case "App", "Service", "Function":
		return workloadOwner{Kind: ref.Kind, Name: ref.Name, UID: string(ref.UID)}, true
	}
	return workloadOwner{}, false
}

func tuningName(owner workloadOwner) string {
	return resourcebounds.TuningName(owner.Kind, owner.Name)
}

// ownerSpec reads the owner's resource quantities and who set each.
func (rc *ResourceController) ownerSpec(ctx context.Context, namespace string, owner workloadOwner) (resourcebounds.Spec, error) {
	key := crclient.ObjectKey{Namespace: namespace, Name: owner.Name}
	switch owner.Kind {
	case "App":
		var app kipperv1.App
		if err := rc.crClient.Get(ctx, key, &app); err != nil {
			return resourcebounds.Spec{}, err
		}
		return resourcebounds.AppSpec(&app)
	case "Service":
		var svc kipperv1.Service
		if err := rc.crClient.Get(ctx, key, &svc); err != nil {
			return resourcebounds.Spec{}, err
		}
		r := svc.Spec.Resources
		return resourcebounds.OwnedSpec(r.CPURequest, r.CPULimit, r.MemoryRequest, r.MemoryLimit)
	default:
		var fn kipperv1.Function
		if err := rc.crClient.Get(ctx, key, &fn); err != nil {
			return resourcebounds.Spec{}, err
		}
		r := fn.Spec.Resources
		return resourcebounds.OwnedSpec(r.CPURequest, r.CPULimit, r.MemoryRequest, r.MemoryLimit)
	}
}

// loadTuning returns the owner's record or a new, unsaved record. It waits for
// garbage collection to remove a previous owner's record. Ownerless records
// are deleted and rebuilt because garbage collection will leave them behind.
func (rc *ResourceController) loadTuning(ctx context.Context, namespace string, owner workloadOwner) (*kipperv1.ResourceTuning, bool, error) {
	rt := &kipperv1.ResourceTuning{}
	err := rc.crClient.Get(ctx, crclient.ObjectKey{Namespace: namespace, Name: tuningName(owner)}, rt)
	switch {
	case err == nil && metav1.GetControllerOf(rt) == nil:
		if err := rc.crClient.Delete(ctx, rt, crclient.Preconditions{UID: &rt.UID}); err != nil && !apierrors.IsNotFound(err) {
			return nil, false, fmt.Errorf("removing ownerless %s: %w", rt.Name, err)
		}
	case err == nil && !resourcebounds.TuningBelongsTo(rt, types.UID(owner.UID)):
		return nil, false, fmt.Errorf("%s belongs to an earlier %s of the same name, waiting for it to be removed", rt.Name, owner.Kind)
	case err == nil:
		return rt, false, nil
	case !apierrors.IsNotFound(err):
		return nil, false, err
	}
	controller := true
	return &kipperv1.ResourceTuning{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tuningName(owner),
			Namespace: namespace,
			// Rebuild tuning state after a restore instead of reusing old recommendations
			// and OOM cooldowns.
			Labels: map[string]string{labels.ExcludeFromBackup: "true"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(),
				Kind:       owner.Kind,
				Name:       owner.Name,
				UID:        types.UID(owner.UID),
				Controller: &controller,
			}},
		},
		Spec: kipperv1.ResourceTuningSpec{Kind: owner.Kind, Name: owner.Name},
	}, true, nil
}

// saveTuning writes the record's status, creating the record first when it
// is new. The auto-sizer is the record's only writer.
func (rc *ResourceController) saveTuning(ctx context.Context, rt *kipperv1.ResourceTuning, isNew bool) error {
	if isNew {
		status := rt.Status
		if err := rc.crClient.Create(ctx, rt); err != nil {
			return err
		}
		rt.Status = status
	}
	return rc.crClient.Status().Update(ctx, rt)
}

// tuneWorkload records a recommendation for the owner's reconciler to apply.
// It respects user bounds and held values. OOM kills on resources with user
// or held memory values produce an alert without raising the limit.
func (rc *ResourceController) tuneWorkload(
	ctx context.Context,
	obj metav1.Object,
	appName string,
	live *corev1.Container,
	replicas, surgePods int32,
	podSpec *corev1.PodSpec,
	podEntries []podMetricsEntry,
	sizing appSizing,
	sustainSaturationFor time.Duration,
) []ResourceLogEntry {
	namespace := obj.GetNamespace()
	owner, ok := ownerOf(obj)
	if !ok || rc.crClient == nil {
		rc.noteSkipped(namespace, obj.GetName())
		return nil
	}
	spec, err := rc.ownerSpec(ctx, namespace, owner)
	if err != nil {
		log.Printf("resource controller: reading %s %s/%s: %v", owner.Kind, namespace, owner.Name, err)
		return nil
	}
	rt, isNew, err := rc.loadTuning(ctx, namespace, owner)
	if err != nil {
		log.Printf("resource controller: reading tuning record for %s/%s: %v", namespace, owner.Name, err)
		return nil
	}
	before := rt.Status.DeepCopy()
	now := time.Now()

	oomKey := namespace + "/" + appName + "/" + live.Name
	if last := rt.Status.LastOOM; last != nil {
		at := handledOOMTime(last)
		rc.mu.Lock()
		if at.After(rc.oomHandledAt[oomKey]) {
			rc.oomHandledAt[oomKey] = at
		}
		rc.mu.Unlock()
	}

	var entries []ResourceLogEntry
	// Keep the OOM alert even if saving the record fails; acknowledge it only
	// after the alert is stored.
	var oomAlert []ResourceLogEntry
	memMode := resourcebounds.ModeOf(spec.MemoryRequest, spec.MemoryLimit)
	if memMode != resourcebounds.ModeAutomatic {
		if entry, at, fresh := rc.oomAtUserLimit(namespace, appName, oomKey, live, podEntries, now); fresh {
			entries = append(entries, entry)
			oomAlert = append(oomAlert, entry)
			// Alerting is the only response here, so the OOM counts as handled
			// only once its alert is stored; see commitStagedAcks.
			rc.stageAck(func(ctx context.Context) {
				rc.acknowledgeOOM(ctx, namespace, owner, oomKey, oomIdentity(live.Name, at), at)
			})
			// Evaluation must not double memory for this OOM either.
			oomSeen := at
			podEntries = withoutOOMAt(podEntries, oomSeen)
		}
	}
	blockDecrease := sizing.ScaledOut
	cooldown := false
	if until := rt.Status.DecreaseBlockedUntil; until != nil && now.Before(until.Time) {
		blockDecrease = true
		cooldown = true
	}
	rc.startHistoryOverIfResized(namespace, appName, live.Resources)

	workloadLabels := obj.GetLabels()
	if workloadLabels[labels.ResourceProfile] == "" && owner.Kind == "Function" {
		// Functions run on the lightweight profile, which their Deployments do
		// not label.
		workloadLabels = maps.Clone(workloadLabels)
		if workloadLabels == nil {
			workloadLabels = map[string]string{}
		}
		workloadLabels[labels.ResourceProfile] = "lightweight"
	}
	work := live.DeepCopy()
	cpuMode := resourcebounds.ModeOf(spec.CPURequest, spec.CPULimit)
	proposed, oomMark := rc.evaluate(namespace, appName, work, podEntries, workloadLabels, replicas, blockDecrease, sustainSaturationFor,
		cpuMode == resourcebounds.ModeAutomatic && !sizing.Tracked.CPU)
	// An autoscaler reads usage as a share of the request, so a metric it
	// tracks keeps its live size. The raise after an OOM kill is the exception,
	// because more pods do not save a pod that runs out of memory.
	if sizing.Tracked.CPU {
		keepLive(work, live, corev1.ResourceCPU)
	}
	if sizing.Tracked.Memory && oomMark == nil {
		keepLive(work, live, corev1.ResourceMemory)
	}

	profile := profileDefaults(workloadLabels[labels.ResourceProfile])
	oomCap := rc.oomCapBytes
	if oomCap == 0 {
		oomCap = defaultOOMCapBytes
	}
	pendingCPU, pendingMem := pendingPairs(sizing.Tracked.Recommendation(rt.Status))
	cpuEff, cpuChanged := effective(spec.CPURequest, spec.CPULimit, corev1.ResourceCPU, live, work, pendingCPU, cooldown,
		resourcebounds.AutoRange{Floor: resource.MustParse(profile.cpu), LimitFloor: resource.MustParse(profile.cpuLimit())})
	memEff, memChanged := effective(spec.MemoryRequest, spec.MemoryLimit, corev1.ResourceMemory, live, work, pendingMem, cooldown,
		resourcebounds.AutoRange{Floor: resource.MustParse(profile.memory), Ceiling: *resource.NewQuantity(oomCap, resource.BinarySI)})

	target := live.DeepCopy()
	setPair(target, corev1.ResourceCPU, cpuEff)
	setPair(target, corev1.ResourceMemory, memEff)
	kept := keepChangedEntries(proposed, live, cpuEff, cpuChanged, memEff, memChanged)
	kept, quotaBlocked := rc.applyQuotaCeiling(ctx, namespace, appName, target, live.Resources, replicas, surgePods,
		quotapkg.WithContainerResources(podSpec, target.Resources), kept)
	entries = append(entries, kept...)

	// Preserve the pending recommendation while quota blocks this proposal.
	if !quotaBlocked {
		next := kipperv1.ResourceTuningStatus{
			Recommendation: recommendation(cpuMode, cpuEff, memMode, memEff),
			MemoryCause:    rt.Status.MemoryCause,
		}
		switch {
		case oomMark != nil && memChanged:
			next.MemoryCause = kipperv1.MemoryCauseOOMKill
		case !sameMemory(next.Recommendation, rt.Status.Recommendation):
			next.MemoryCause = ""
		case pairsEqual(memEff, liveMemory(live)):
			// The raise is running, so the live size keeps it from here on.
			next.MemoryCause = ""
		}
		rt.Status.Recommendation = sizing.Tracked.Recommendation(next)
		rt.Status.MemoryCause = next.MemoryCause
	}
	if oomMark != nil && !quotaBlocked {
		rt.Status.LastOOM = &kipperv1.OOMRecord{Identity: oomIdentity(live.Name, oomMark.at), At: metav1.NewTime(oomMark.at), Alerted: true}
		if memChanged {
			until := metav1.NewTime(now.Add(decreaseCooldownAfterOOM))
			rt.Status.DecreaseBlockedUntil = &until
		}
	}
	if rt.Status.Recommendation != before.Recommendation {
		at := metav1.NewTime(now)
		rt.Status.RecommendedAt = &at
	}

	if isNew || !statusEqual(before, &rt.Status) {
		if err := rc.saveTuning(ctx, rt, isNew); err != nil {
			log.Printf("resource controller: saving tuning record for %s/%s: %v", namespace, owner.Name, err)
			return append(oomAlert, rc.workloadUpdateFailed(namespace, appName, err)...)
		}
	}
	// A blocked increase leaves the OOM unhandled, so it is tried again once
	// the quota allows it.
	if !quotaBlocked {
		rc.commitOOMMark(oomMark)
	}
	if len(kept) > 0 && !quotaBlocked {
		rc.recordChange(namespace, appName)
	}
	return entries
}

// effective resolves a proposal against the bounds and reports whether it
// changes an automatically sized or bounded resource. With no new proposal,
// it preserves the pending recommendation. During the OOM cooldown, proposals
// computed from the old live size can raise that recommendation but cannot
// reduce it.
func effective(request, limit resourcebounds.Quantity, name corev1.ResourceName, live, proposed *corev1.Container, pending *resourcebounds.Pair, cooldown bool, auto resourcebounds.AutoRange) (resourcebounds.Pair, bool) {
	livePair, _ := resourcebounds.PairOf(&live.Resources, name)
	proposedPair, ok := resourcebounds.PairOf(&proposed.Resources, name)
	if !ok || pairsEqual(proposedPair, livePair) {
		p, _ := resourcebounds.Resolve(request, limit, pending, livePair, resourcebounds.AutoRange{})
		return p, false
	}
	if cooldown && pending != nil {
		proposedPair = resourcebounds.Pair{
			Request: maxQuantity(proposedPair.Request, pending.Request),
			Limit:   maxQuantity(proposedPair.Limit, pending.Limit),
		}
	}
	p, mode := resourcebounds.Resolve(request, limit, &proposedPair, livePair, auto)
	changed := (mode == resourcebounds.ModeAutomatic || mode == resourcebounds.ModeBounded) && !pairsEqual(p, livePair)
	return p, changed
}

// keepChangedEntries drops the evaluation's change entries for a resource
// whose effective values did not change, and rewrites the rest to show the
// live and effective values.
func keepChangedEntries(proposed []ResourceLogEntry, live *corev1.Container, cpu resourcebounds.Pair, cpuChanged bool, mem resourcebounds.Pair, memChanged bool) []ResourceLogEntry {
	var kept []ResourceLogEntry
	for _, e := range proposed {
		switch {
		case e.Action == "applied default resources":
			if cpuChanged || memChanged {
				kept = append(kept, e)
			}
		case isChange(e.Action) && strings.Contains(e.Action, "memory"):
			if memChanged {
				e.From, e.To = shownChange(live, corev1.ResourceMemory, mem)
				kept = append(kept, e)
			}
		case isChange(e.Action) && strings.Contains(e.Action, "CPU"):
			if cpuChanged {
				e.From, e.To = shownChange(live, corev1.ResourceCPU, cpu)
				kept = append(kept, e)
			}
		default:
			kept = append(kept, e)
		}
	}
	return kept
}

func isChange(action string) bool {
	return strings.HasPrefix(action, "increased") || strings.HasPrefix(action, "decreased") || strings.HasPrefix(action, "doubled")
}

// shownChange picks the request for an alert's from/to, or the limit when
// only the limit moved.
func shownChange(live *corev1.Container, name corev1.ResourceName, to resourcebounds.Pair) (string, string) {
	from, _ := resourcebounds.PairOf(&live.Resources, name)
	if from.Request.Cmp(to.Request) == 0 {
		return from.Limit.String(), to.Limit.String()
	}
	return from.Request.String(), to.Request.String()
}

// recommendation records the effective values of each resource the
// auto-sizer sizes; a fixed or held resource gets none.
func recommendation(cpuMode resourcebounds.Mode, cpu resourcebounds.Pair, memMode resourcebounds.Mode, mem resourcebounds.Pair) kipperv1.TunedResources {
	var r kipperv1.TunedResources
	if cpuMode == resourcebounds.ModeAutomatic || cpuMode == resourcebounds.ModeBounded {
		r.CPURequest, r.CPULimit = cpu.Request.String(), cpu.Limit.String()
	}
	if memMode == resourcebounds.ModeAutomatic || memMode == resourcebounds.ModeBounded {
		r.MemoryRequest, r.MemoryLimit = mem.Request.String(), mem.Limit.String()
	}
	return r
}

// oomAtUserLimit reports a new OOM for user-set or held memory. Raising the
// request cannot help a container killed at its limit. The caller acknowledges
// the OOM after storing the alert.
func (rc *ResourceController) oomAtUserLimit(namespace, appName, oomKey string, live *corev1.Container, podEntries []podMetricsEntry, now time.Time) (ResourceLogEntry, time.Time, bool) {
	var oomAt time.Time
	for _, pe := range podEntries {
		if pe.OOMAt.After(oomAt) {
			oomAt = pe.OOMAt
		}
	}
	rc.mu.Lock()
	fresh := !oomAt.IsZero() && oomAt.After(rc.oomHandledAt[oomKey]) && now.Sub(oomAt) < oomActionableWindow
	rc.mu.Unlock()
	if !fresh {
		return ResourceLogEntry{}, time.Time{}, false
	}
	limit := live.Resources.Limits[corev1.ResourceMemory]
	return ResourceLogEntry{
		Time:      now.UTC().Format(time.RFC3339),
		App:       appName,
		Namespace: namespace,
		Action:    "OOMKilled at your limit",
		From:      limit.String(),
		To:        limit.String(),
		Reason:    fmt.Sprintf("container was OOMKilled at the memory limit of %s you set. Raise it to allow more", limit.String()),
	}, oomAt, true
}

func oomIdentity(container string, at time.Time) string {
	return container + "@" + at.UTC().Format(time.RFC3339Nano)
}

// handledOOMTime is the exact time of a recorded OOM. The identity keeps
// nanoseconds, which the At field loses when it is stored.
func handledOOMTime(rec *kipperv1.OOMRecord) time.Time {
	if i := strings.LastIndex(rec.Identity, "@"); i >= 0 {
		if at, err := time.Parse(time.RFC3339Nano, rec.Identity[i+1:]); err == nil {
			return at
		}
	}
	return rec.At.Time
}

// startHistoryOverIfResized clears usage samples after any resource change,
// so tuning uses observations taken at the current size.
func (rc *ResourceController) startHistoryOverIfResized(namespace, appName string, res corev1.ResourceRequirements) {
	wk := workloadKey{Namespace: namespace, Name: appName}
	sig := res.String()
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if prev, ok := rc.historySize[wk]; ok && prev != sig {
		delete(rc.history, wk)
	}
	rc.historySize[wk] = sig
}

// noteSkipped logs once per workload that the auto-sizer leaves it alone
// because it has no App, Service or Function owner.
func (rc *ResourceController) noteSkipped(namespace, name string) {
	key := namespace + "/" + name
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.skippedLogged[key] {
		return
	}
	rc.skippedLogged[key] = true
	log.Printf("resource controller: leaving %s alone, it has no App, Service or Function owner", key)
}

func setPair(c *corev1.Container, name corev1.ResourceName, p resourcebounds.Pair) {
	if c.Resources.Requests == nil {
		c.Resources.Requests = corev1.ResourceList{}
	}
	if c.Resources.Limits == nil {
		c.Resources.Limits = corev1.ResourceList{}
	}
	c.Resources.Requests[name] = p.Request
	c.Resources.Limits[name] = p.Limit
}

// keepLive sets one resource of c back to the live container's values.
func keepLive(c, live *corev1.Container, name corev1.ResourceName) {
	restore := func(dst *corev1.ResourceList, src corev1.ResourceList) {
		v, ok := src[name]
		switch {
		case !ok:
			delete(*dst, name)
		case *dst == nil:
			*dst = corev1.ResourceList{name: v}
		default:
			(*dst)[name] = v
		}
	}
	restore(&c.Resources.Requests, live.Resources.Requests)
	restore(&c.Resources.Limits, live.Resources.Limits)
}

func sameMemory(a, b kipperv1.TunedResources) bool {
	return a.MemoryRequest == b.MemoryRequest && a.MemoryLimit == b.MemoryLimit
}

func liveMemory(live *corev1.Container) resourcebounds.Pair {
	p, _ := resourcebounds.PairOf(&live.Resources, corev1.ResourceMemory)
	return p
}

func pairsEqual(a, b resourcebounds.Pair) bool {
	return a.Request.Cmp(b.Request) == 0 && a.Limit.Cmp(b.Limit) == 0
}

func statusEqual(a, b *kipperv1.ResourceTuningStatus) bool {
	if a.Recommendation != b.Recommendation || a.MemoryCause != b.MemoryCause {
		return false
	}
	if !timesEqual(a.DecreaseBlockedUntil, b.DecreaseBlockedUntil) {
		return false
	}
	if (a.LastOOM == nil) != (b.LastOOM == nil) {
		return false
	}
	return a.LastOOM == nil || a.LastOOM.Identity == b.LastOOM.Identity
}

func timesEqual(a, b *metav1.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(b)
}

// pendingPairs reads a record's recommendation per resource, nil where there
// is none.
func pendingPairs(rec kipperv1.TunedResources) (cpu, memory *resourcebounds.Pair) {
	return pairOfStrings(rec.CPURequest, rec.CPULimit), pairOfStrings(rec.MemoryRequest, rec.MemoryLimit)
}

func pairOfStrings(request, limit string) *resourcebounds.Pair {
	if request == "" || limit == "" {
		return nil
	}
	req, err := resource.ParseQuantity(request)
	if err != nil {
		return nil
	}
	lim, err := resource.ParseQuantity(limit)
	if err != nil {
		return nil
	}
	return &resourcebounds.Pair{Request: req, Limit: lim}
}

// withoutOOMAt drops the OOM an alert-only path already reported, so the
// evaluation does not also act on it.
func withoutOOMAt(entries []podMetricsEntry, at time.Time) []podMetricsEntry {
	out := make([]podMetricsEntry, 0, len(entries))
	for _, pe := range entries {
		if !pe.OOMAt.IsZero() && !pe.OOMAt.After(at) {
			pe.OOMAt = time.Time{}
			pe.OOMKilled = false
			if pe.Synthetic {
				continue
			}
		}
		out = append(out, pe)
	}
	return out
}

// stageAck queues work that may run only once this tick's alerts are stored.
func (rc *ResourceController) stageAck(fn func(context.Context)) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.stagedAcks = append(rc.stagedAcks, fn)
}

// commitStagedAcks runs the queued acknowledgements after the alerts they
// belong to were stored. With stored false it drops them, so the next tick
// raises those alerts again.
func (rc *ResourceController) commitStagedAcks(ctx context.Context, stored bool) {
	rc.mu.Lock()
	acks := rc.stagedAcks
	rc.stagedAcks = nil
	rc.mu.Unlock()
	if !stored {
		return
	}
	for _, ack := range acks {
		ack(ctx)
	}
}

// acknowledgeOOM records an alerted OOM in memory and in the tuning record
// to suppress duplicate alerts. If persistence fails, a restart can repeat it.
func (rc *ResourceController) acknowledgeOOM(ctx context.Context, namespace string, owner workloadOwner, oomKey, identity string, at time.Time) {
	rc.mu.Lock()
	if at.After(rc.oomHandledAt[oomKey]) {
		rc.oomHandledAt[oomKey] = at
	}
	rc.mu.Unlock()
	rt, isNew, err := rc.loadTuning(ctx, namespace, owner)
	if err != nil {
		log.Printf("resource controller: recording the alerted OOM for %s/%s: %v", namespace, owner.Name, err)
		return
	}
	rt.Status.LastOOM = &kipperv1.OOMRecord{Identity: identity, At: metav1.NewTime(at), Alerted: true}
	if err := rc.saveTuning(ctx, rt, isNew); err != nil {
		log.Printf("resource controller: recording the alerted OOM for %s/%s: %v", namespace, owner.Name, err)
	}
}

func maxQuantity(a, b resource.Quantity) resource.Quantity {
	if a.Cmp(b) >= 0 {
		return a
	}
	return b
}
