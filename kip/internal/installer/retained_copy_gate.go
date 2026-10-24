package installer

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/kip/internal/velerolabel"
)

// RetainedCopyReadyFor is how long a moved service's pod must have been Ready
// before its local copy can be deleted.
const RetainedCopyReadyFor = time.Hour

// RetainedCopySoak is how long after a move a healthy service's local copy is
// kept when no backup holds its data from Longhorn.
const RetainedCopySoak = 24 * time.Hour

// RetainedDestinationAnnotation on a retained volume holds the UID of the
// Longhorn claim its move verified.
const RetainedDestinationAnnotation = retainedDestinationAnnotation

const (
	retainedDataVolume    = "data"
	veleroBackupNameLabel = "velero.io/backup-name"
	veleroBackupUIDLabel  = "velero.io/backup-uid"
	veleroPVCUIDLabel     = "velero.io/pvc-uid"
)

// RetainedService is what the prune decision reads from a Kipper service.
type RetainedService struct {
	MoveMark          string
	ConsistentBackups bool
	CopiesChange      string
}

// VeleroBackup holds the fields of a velero.io/v1 Backup the prune decision
// reads.
type VeleroBackup struct {
	Metadata metav1.ObjectMeta  `json:"metadata"`
	Status   VeleroBackupStatus `json:"status"`
}

type VeleroBackupStatus struct {
	Phase          string       `json:"phase"`
	StartTimestamp *metav1.Time `json:"startTimestamp,omitempty"`
	Expiration     *metav1.Time `json:"expiration,omitempty"`
}

// VeleroPodVolumeBackup holds the fields of a velero.io/v1 PodVolumeBackup
// the prune decision reads.
type VeleroPodVolumeBackup struct {
	Metadata metav1.ObjectMeta `json:"metadata"`
	Spec     struct {
		Pod    corev1.ObjectReference `json:"pod"`
		Volume string                 `json:"volume"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

// RetainedCopyFacts is the live state behind the decision on one moved copy.
// A nil Claim, ServiceState or Pod means the object does not exist. A zero
// MovedAt means the move recorded no valid time.
type RetainedCopyFacts struct {
	Namespace        string
	Service          string
	Destination      string
	MovedAt          time.Time
	Claim            *corev1.PersistentVolumeClaim
	ServiceState     *RetainedService
	Pod              *corev1.Pod
	Backups          []VeleroBackup
	PodVolumeBackups []VeleroPodVolumeBackup
}

// PruneWaivers are the operator's explicit waivers. SkipSoak (--now) skips the
// wait for a backup or a day. DeleteUnproven (--delete-unproven) skips the
// health gate. Callers must restrict it to a copy the operator named.
type PruneWaivers struct {
	SkipSoak       bool
	DeleteUnproven bool
}

// RetainedCopyDecision says whether a moved copy can be deleted, and Note says
// why it is kept or what released it. Gone marks a copy whose service and data
// claim are both gone. Unproven marks a service that does not pass the health
// gate, and FallbackWaived a copy that is prunable only because of --now.
// ConsistentBackups marks a FallbackWaived copy whose service uses consistent
// backups, which cannot establish backup evidence for this decision.
type RetainedCopyDecision struct {
	Prunable          bool
	Note              string
	Gone              bool
	Unproven          bool
	FallbackWaived    bool
	ConsistentBackups bool
}

// DecideRetainedCopy decides whether a moved local copy can be deleted.
// The service's data claim must be the bound Longhorn claim the move verified
// and any remaining service must have no move mark, or the service and claim
// must both be gone. With a remaining claim, the service's pod must be Ready
// on it for RetainedCopyReadyFor, and neither the service's status nor the
// claim's stop record may indicate an unfinished copy change.
// A qualifying backup from Longhorn or RetainedCopySoak since the move is
// also required. Waivers skip only the
// health or backup wait checks they name. This function reads no external state.
func DecideRetainedCopy(f RetainedCopyFacts, now time.Time, w PruneWaivers) RetainedCopyDecision {
	prunable, gone, note := retainedCopyVerdict(f)
	if !prunable || gone {
		return RetainedCopyDecision{Prunable: prunable, Gone: gone, Note: note}
	}
	var d RetainedCopyDecision
	if note := retainedHealthNote(f, now); note != "" {
		if !w.DeleteUnproven {
			return RetainedCopyDecision{Note: note, Unproven: true}
		}
		d.Unproven = true
		d.Note = strings.TrimPrefix(note, "kept: ") + ", and --delete-unproven waives the health check"
	}
	if backup := retainedBackupEvidence(f, now); backup != "" {
		d.Prunable = true
		if !d.Unproven {
			d.Note = "backup " + backup + " holds its data from Longhorn"
		}
		return d
	}
	if !f.MovedAt.IsZero() && !now.Before(f.MovedAt.Add(RetainedCopySoak)) {
		d.Prunable = true
		return d
	}
	if w.SkipSoak {
		d.Prunable, d.FallbackWaived, d.ConsistentBackups = true, true, consistentBackups(f)
		switch {
		case d.Unproven:
		case d.ConsistentBackups:
			d.Note = f.Service + " takes consistent backups, which the prune check cannot use as proof that its data was backed up. Using --now skips the day's wait"
		default:
			d.Note = "no backup holds its data from Longhorn yet, and --now gives up this fallback"
		}
		return d
	}
	if f.MovedAt.IsZero() {
		return RetainedCopyDecision{Unproven: d.Unproven, Note: "kept: the move recorded no time, so neither a later backup nor a day since the move can be checked"}
	}
	if consistentBackups(f) {
		return RetainedCopyDecision{Unproven: d.Unproven, Note: fmt.Sprintf("kept for %s more: %s takes consistent backups, and Velero records hook results per backup, not per pod", roundUp(f.MovedAt.Add(RetainedCopySoak).Sub(now)), f.Service)}
	}
	return RetainedCopyDecision{Unproven: d.Unproven, Note: fmt.Sprintf("kept until a backup of %s completes or %s from now", f.Service, roundUp(f.MovedAt.Add(RetainedCopySoak).Sub(now)))}
}

// NeedsServiceHealth reports whether the data claim passes its checks for a
// service that still exists, so the decision turns on the pod and backups.
// Readers use it to skip reading those when they cannot matter.
func (f RetainedCopyFacts) NeedsServiceHealth() bool {
	prunable, gone, _ := retainedCopyVerdict(f)
	return prunable && !gone
}

// retainedCopyVerdict applies the checks on the data claim. The service's
// data claim must still be the bound Longhorn claim the move verified, by
// UID, and the move must have finished, or the service and its claim must
// both be gone.
// Otherwise the copy may be the only data left, and the note says why it is
// kept.
func retainedCopyVerdict(f RetainedCopyFacts) (prunable, gone bool, note string) {
	if f.Namespace == "" || f.Service == "" {
		return false, false, "kept: the copy names no service"
	}
	claim := f.Claim
	class := ""
	if claim != nil && claim.Spec.StorageClassName != nil {
		class = *claim.Spec.StorageClassName
	}
	switch {
	case claim == nil:
		if f.ServiceState != nil {
			return false, false, "kept: the service has no data volume, so this copy may be the only one left"
		}
		return true, true, "its service was deleted"
	case class == platform.LocalPathClass:
		return false, false, "kept: the service's data volume is on local storage"
	case !platform.IsLonghornClass(class) || claim.Status.Phase != corev1.ClaimBound || claim.DeletionTimestamp != nil:
		return false, false, "kept: the service's data volume is not a bound Longhorn volume"
	case f.Destination == "":
		return false, false, "kept: the move recorded no verified data volume to compare with"
	case string(claim.UID) != f.Destination:
		return false, false, "kept: the service's data volume was replaced after the move, so this copy may hold the only original data"
	}
	if f.ServiceState != nil && f.ServiceState.MoveMark != "" {
		return false, false, fmt.Sprintf("kept: its move is unfinished. Finish it with 'kip service storage longhorn %s --project %s'", f.Service, f.Namespace)
	}
	if note := copiesChangeNote(f); note != "" {
		return false, false, note
	}
	return true, false, ""
}

// copiesChangeNote explains a copy change reported by the service or recorded
// on the destination claim, or returns "" when neither indicates a change.
// The claim must be non-nil. Its stop record persists across retries, so it
// protects the local copy even when the service's status is stale.
func copiesChangeNote(f RetainedCopyFacts) string {
	if f.ServiceState != nil && f.ServiceState.CopiesChange != "" {
		return fmt.Sprintf("kept: the copies of %s's data volume are changing (%s)", f.Service, f.ServiceState.CopiesChange)
	}
	if _, ok := f.Claim.Annotations[platform.CopyChangeStopAnnotation]; ok {
		return fmt.Sprintf("kept: the copies of %s's data volume are changing", f.Service)
	}
	return ""
}

// retainedHealthNote returns why the service's pod does not pass the health
// gate, or "" when it has been Ready on the verified claim long enough. The
// claim's UID is checked by retainedCopyVerdict, so the pod only needs to
// mount the claim by name.
func retainedHealthNote(f RetainedCopyFacts, now time.Time) string {
	pod := f.Pod
	if pod == nil {
		return fmt.Sprintf("kept: %s has not run on Longhorn yet", f.Service)
	}
	mounts := false
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == ServiceDataClaim(f.Service) {
			mounts = true
		}
	}
	if !mounts {
		return fmt.Sprintf("kept: %s's pod does not use the Longhorn volume the move verified", f.Service)
	}
	var ready *corev1.PodCondition
	for i, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			ready = &pod.Status.Conditions[i]
		}
	}
	if pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil || ready == nil || ready.Status != corev1.ConditionTrue {
		return fmt.Sprintf("kept: %s is not Ready on Longhorn", f.Service)
	}
	if ready.LastTransitionTime.IsZero() || now.Sub(ready.LastTransitionTime.Time) < RetainedCopyReadyFor {
		return fmt.Sprintf("kept: %s has not been Ready on Longhorn for an hour yet", f.Service)
	}
	return ""
}

// consistentBackups reports whether the service backs up a Postgres dump.
func consistentBackups(f RetainedCopyFacts) bool {
	return f.ServiceState != nil && f.ServiceState.ConsistentBackups
}

// retainedBackupEvidence returns the name of a backup that holds the
// service's data from Longhorn, or "" when none does. The backup must have
// completed, started after the move, have a future expiry and no deletion
// timestamp, and
// its completed PodVolumeBackup of the service's pod volume data must carry
// the backup's UID and be bound to the verified claim. Any missing field
// counts as no evidence. Consistent backups cannot qualify because Velero
// counts hooks per backup rather than per pod, and the dump's manifest is
// only inside the backup. These fields cannot prove that this service's dump succeeded.
func retainedBackupEvidence(f RetainedCopyFacts, now time.Time) string {
	if f.MovedAt.IsZero() || consistentBackups(f) {
		return ""
	}
	pod := f.Service + "-0"
	for _, b := range f.Backups {
		s := b.Status
		switch {
		case s.Phase != "Completed" || b.Metadata.DeletionTimestamp != nil:
			continue
		case s.StartTimestamp == nil || !s.StartTimestamp.After(f.MovedAt):
			continue
		case s.Expiration == nil || !s.Expiration.After(now):
			continue
		case b.Metadata.UID == "":
			continue
		}
		label := velerolabel.ValidName(b.Metadata.Name)
		for _, p := range f.PodVolumeBackups {
			l := p.Metadata.Labels
			if l[veleroBackupNameLabel] == label && l[veleroBackupUIDLabel] == string(b.Metadata.UID) &&
				p.Status.Phase == "Completed" && p.Spec.Pod.Namespace == f.Namespace && p.Spec.Pod.Name == pod &&
				p.Spec.Volume == retainedDataVolume && l[veleroPVCUIDLabel] == f.Destination {
				return b.Metadata.Name
			}
		}
	}
	return ""
}

// roundUp prints d in whole hours, or whole minutes under an hour, rounding up.
func roundUp(d time.Duration) string {
	if d >= time.Hour {
		return fmt.Sprintf("%dh", (d+time.Hour-1)/time.Hour)
	}
	return fmt.Sprintf("%dm", max((d+time.Minute-1)/time.Minute, 1))
}

// retainedCopyReader reads the live state behind prune decisions over
// kubectl. It lists Velero's backups at most once.
type retainedCopyReader struct {
	cluster KubectlMoveCluster
	velero  *struct {
		backups []VeleroBackup
		pvbs    []VeleroPodVolumeBackup
	}
}

// facts reads the state for the retained volume with annotations a. The pod
// and backups are read only when the checks on the data claim pass, since the
// decision needs them only then.
func (r *retainedCopyReader) facts(a map[string]string) (RetainedCopyFacts, error) {
	ns, svc, _ := strings.Cut(a[platform.RetainedLocalCopyServiceAnnotation], "/")
	f := RetainedCopyFacts{Namespace: ns, Service: svc, Destination: a[retainedDestinationAnnotation]}
	f.MovedAt, _ = time.Parse(time.RFC3339, a[platform.RetainedLocalCopyMovedAtAnnotation])
	if ns == "" || svc == "" {
		return f, nil
	}
	var claim corev1.PersistentVolumeClaim
	found, err := r.cluster.getJSON("claim "+ServiceDataClaim(svc), fmt.Sprintf("kubectl -n %s get pvc %s", shellQuote(ns), shellQuote(ServiceDataClaim(svc))), &claim)
	if err != nil {
		return f, err
	}
	if found {
		f.Claim = &claim
	}
	s, err := r.cluster.ReadService(ns, svc)
	if err != nil {
		return f, err
	}
	if s != nil {
		f.ServiceState = &RetainedService{MoveMark: s.Mark, ConsistentBackups: s.ConsistentBackups, CopiesChange: s.CopiesChange}
	}
	if !f.NeedsServiceHealth() {
		return f, nil
	}
	var pod corev1.Pod
	found, err = r.cluster.getJSON("pod "+svc+"-0", fmt.Sprintf("kubectl -n %s get pod %s", shellQuote(ns), shellQuote(svc+"-0")), &pod)
	if err != nil {
		return f, err
	}
	if found {
		f.Pod = &pod
	}
	if r.velero == nil {
		r.velero = &struct {
			backups []VeleroBackup
			pvbs    []VeleroPodVolumeBackup
		}{}
		if err := r.veleroList("backups", "kubectl -n velero get backups.velero.io", &r.velero.backups); err != nil {
			r.velero = nil
			return f, err
		}
		if err := r.veleroList("pod volume backups", "kubectl -n velero get podvolumebackups.velero.io", &r.velero.pvbs); err != nil {
			r.velero = nil
			return f, err
		}
	}
	f.Backups, f.PodVolumeBackups = r.velero.backups, r.velero.pvbs
	return f, nil
}

// veleroList reads a Velero list. A cluster without Velero reads as an empty
// list.
func (r *retainedCopyReader) veleroList(what, cmd string, into any) error {
	out, err := r.cluster.Server.Run(cmd + " -o json 2>&1")
	if err != nil {
		if identityAbsentFromKubectl(out) {
			return nil
		}
		return fmt.Errorf("listing %s: %w", what, err)
	}
	list := struct {
		Items json.RawMessage `json:"items"`
	}{}
	if err := json.Unmarshal([]byte(kubectlPayload(out)), &list); err != nil {
		return fmt.Errorf("reading %s: %w", what, err)
	}
	if len(list.Items) == 0 {
		return nil
	}
	if err := json.Unmarshal(list.Items, into); err != nil {
		return fmt.Errorf("reading %s: %w", what, err)
	}
	return nil
}
