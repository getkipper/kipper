package cmd

import (
	"context"
	"encoding/json"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/manifest"
	"github.com/getkipper/kipper/kip/internal/service"
)

// reportLocalVolumes lists what local storage leaves behind on a cluster
// that moved to Longhorn: moves that did not finish, service volumes still
// on local-path, and local copies kept after a move with the reason each is
// kept at now. It prints nothing when none are left. A failed service or
// claim lookup suppresses all output. A failed volume lookup suppresses only
// the retained copies, and a failed lookup for one copy only its reason.
func reportLocalVolumes(ctx context.Context, out io.Writer, client kubernetes.Interface, dyn dynamic.Interface, now time.Time) {
	services, err := dyn.Resource(manifest.ServiceGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	claims, err := client.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	local := map[string]bool{}
	for _, c := range claims.Items {
		if c.Spec.StorageClassName != nil && *c.Spec.StorageClassName == platform.LocalPathClass {
			local[c.Namespace+"/"+c.Name] = true
		}
	}
	var unfinished, onLocal []string
	byName := map[string]*installer.RetainedService{}
	for _, s := range services.Items {
		name := s.GetNamespace() + "/" + s.GetName()
		consistent, _, _ := unstructured.NestedBool(s.Object, "spec", "consistentBackups")
		byName[name] = &installer.RetainedService{
			MoveMark:          s.GetAnnotations()[platform.StorageMoveAnnotation],
			ConsistentBackups: consistent,
			CopiesChange:      installer.CopiesChangeReason(service.CopiesOf(s.Object).Reason),
		}
		switch {
		case s.GetAnnotations()[platform.StorageMoveAnnotation] != "":
			unfinished = append(unfinished, name)
		case local[s.GetNamespace()+"/"+installer.ServiceDataClaim(s.GetName())]:
			onLocal = append(onLocal, name)
		}
	}
	sort.Strings(unfinished)
	sort.Strings(onLocal)

	var kept []string
	var keptBytes int64
	pvs, err := client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{LabelSelector: platform.RetainedLocalCopyLabel + "=true"})
	if err == nil {
		velero := &statusVelero{dyn: dyn}
		for _, pv := range pvs.Items {
			if pv.Annotations[platform.RetainedLocalCopyStateAnnotation] != platform.RetainedLocalCopyMoved {
				continue
			}
			used, _ := strconv.ParseInt(pv.Annotations[platform.RetainedLocalCopyUsedBytesAnnotation], 10, 64)
			keptBytes += used
			line := pv.Annotations[platform.RetainedLocalCopyServiceAnnotation]
			if f, ok := retainedCopyFacts(ctx, client, byName, velero, pv.Annotations); ok {
				d := installer.DecideRetainedCopy(f, now, installer.PruneWaivers{})
				switch {
				case !d.Prunable:
					line += ": " + d.Note
				case d.Note != "":
					line += ": can be deleted: " + d.Note
				default:
					line += ": can be deleted"
				}
			}
			kept = append(kept, line)
		}
		sort.Strings(kept)
	}

	if len(unfinished) > 0 {
		say(out, "  Moves to Longhorn not finished:\n")
		say(out, "    ⚠  %s\n", strings.Join(unfinished, "\n    ⚠  "))
		say(out, "       These services stay stopped. Finish each move with 'kip service storage longhorn <name>', or undo it with --rollback.\n\n")
	}
	if len(onLocal) > 0 {
		say(out, "  Service volumes still on local storage:\n")
		say(out, "    ·  %s\n", strings.Join(onLocal, "\n    ·  "))
		say(out, "       Move each one with 'kip service storage longhorn <name>'. The service is stopped while its data is copied.\n\n")
	}
	if len(kept) > 0 {
		say(out, "  Local copies kept after a move: %d (%s)\n", len(kept), humanGiB(keptBytes))
		say(out, "    ·  %s\n", strings.Join(kept, "\n    ·  "))
		say(out, "       'kip cluster storage prune' deletes a copy once its service has been Ready on Longhorn for an hour and either a backup made after the move holds its data or a day has passed since the move.\n\n")
	}
}

// retainedCopyFacts reads what installer.DecideRetainedCopy needs for the
// retained volume with annotations a, with services keyed by namespace/name.
// The pod and backups are read only when the checks on the data claim pass.
// Claim or pod read failures return false. Failed backup reads count as no
// backup evidence.
func retainedCopyFacts(ctx context.Context, client kubernetes.Interface, services map[string]*installer.RetainedService, velero *statusVelero, a map[string]string) (installer.RetainedCopyFacts, bool) {
	ns, svc, _ := strings.Cut(a[platform.RetainedLocalCopyServiceAnnotation], "/")
	f := installer.RetainedCopyFacts{Namespace: ns, Service: svc, Destination: a[installer.RetainedDestinationAnnotation], ServiceState: services[ns+"/"+svc]}
	f.MovedAt, _ = time.Parse(time.RFC3339, a[platform.RetainedLocalCopyMovedAtAnnotation])
	if ns == "" || svc == "" {
		return f, true
	}
	claim, err := client.CoreV1().PersistentVolumeClaims(ns).Get(ctx, installer.ServiceDataClaim(svc), metav1.GetOptions{})
	switch {
	case err == nil:
		f.Claim = claim
	case !apierrors.IsNotFound(err):
		return f, false
	}
	if !f.NeedsServiceHealth() {
		return f, true
	}
	pod, err := client.CoreV1().Pods(ns).Get(ctx, svc+"-0", metav1.GetOptions{})
	switch {
	case err == nil:
		f.Pod = pod
	case !apierrors.IsNotFound(err):
		return f, false
	}
	f.Backups, f.PodVolumeBackups = velero.read(ctx)
	return f, true
}

// statusVelero lists Velero's backups and pod volume backups at most once. A
// failed list reads as no backups, which keeps every copy that needs one.
type statusVelero struct {
	dyn     dynamic.Interface
	done    bool
	backups []installer.VeleroBackup
	pvbs    []installer.VeleroPodVolumeBackup
}

func (v *statusVelero) read(ctx context.Context) ([]installer.VeleroBackup, []installer.VeleroPodVolumeBackup) {
	if !v.done {
		v.done = true
		decodeVeleroList(ctx, v.dyn, backupGVR, &v.backups)
		decodeVeleroList(ctx, v.dyn, podVolumeBackupGVR, &v.pvbs)
	}
	return v.backups, v.pvbs
}

func decodeVeleroList(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, into any) {
	list, err := dyn.Resource(gvr).Namespace("velero").List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	items := make([]map[string]any, 0, len(list.Items))
	for _, item := range list.Items {
		items = append(items, item.Object)
	}
	if body, err := json.Marshal(items); err == nil {
		_ = json.Unmarshal(body, into)
	}
}
