package installer

import (
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/getkipper/kipper/controller/pkg/datavolume"
	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/kip/internal/service"
)

// Steps of a storage move, in order. A record's Step is the last one that
// completed. The rb- steps belong to a rollback.
const (
	MoveRecorded        = "recorded"
	MoveStopped         = "stopped"
	MoveRetained        = "retained"
	MoveReleased        = "released"
	MoveClaimed         = "claimed"
	MoveCopied          = "copied"
	MoveDumpReleased    = "dump"
	MoveCommitted       = "committed"
	MoveRollbackCleaned = "rb-cleaned"
	MoveRollbackBound   = "rb-bound"
	MoveRollbackDone    = "rb-done"
)

const (
	MoveForward  = "forward"
	MoveRollback = "rollback"
)

const (
	// StorageMoveRecordNamespace holds the move records. Project owners can
	// write ConfigMaps in their own namespace, and a record names the
	// cluster-scoped volumes kip binds and deletes.
	StorageMoveRecordNamespace = "kipper-system"
	storageMoveRecordPrefix    = "kipper-storage-move-"
	// StorageMoveRollbackAnnotation marks the claim a rollback creates on the
	// retained local volume. Its value names the move record.
	StorageMoveRollbackAnnotation = "kipper.run/storage-move-rollback"
	localPathRoot                 = "/var/lib/rancher/k3s/storage/"
	// moveDrainWait outlasts the API server's 60-second request timeout, so a
	// pod create admitted before the policy saw the namespace is stored or has
	// failed by the time kip lists pods.
	moveDrainWait = 70 * time.Second
	movePoll      = 2 * time.Second
	// moveCopyPolls bounds the copy at about 24 hours of polling.
	moveCopyPolls = 43200
	dumpForLabel  = "kipper.run/dump-for"
)

// MoveRecord is the state of one storage move, kept as JSON in a ConfigMap
// so an interrupted move resumes or rolls back from where it stopped.
type MoveRecord struct {
	ServiceUID      string            `json:"serviceUID"`
	Namespace       string            `json:"namespace"`
	Service         string            `json:"service"`
	Direction       string            `json:"direction"`
	Step            string            `json:"step"`
	Replicas        int32             `json:"replicas"`
	ReplicasSaved   bool              `json:"replicasSaved,omitempty"`
	OriginalStorage string            `json:"originalStorage"`
	OldClaimUID     string            `json:"oldClaimUID"`
	OldClaimLabels  map[string]string `json:"oldClaimLabels,omitempty"`
	OldPV           string            `json:"oldPV"`
	OldPVUID        string            `json:"oldPVUID"`
	Path            string            `json:"path"`
	Node            string            `json:"node"`
	UsedBytes       int64             `json:"usedBytes"`
	TargetSize      string            `json:"targetSize"`
	DumpClaim       string            `json:"dumpClaim,omitempty"`
	DumpClaimUID    string            `json:"dumpClaimUID,omitempty"`
	NewClaimUID     string            `json:"newClaimUID,omitempty"`
	NewPV           string            `json:"newPV,omitempty"`
	RollbackUID     string            `json:"rollbackClaimUID,omitempty"`
	CopyPodUID      string            `json:"copyPodUID,omitempty"`
	CopyDone        bool              `json:"copyDone,omitempty"`
	VerifyPodUID    string            `json:"verifyPodUID,omitempty"`
	ExpandedTo      string            `json:"expandedTo,omitempty"`
	FenceAdded      []string          `json:"fenceAdded,omitempty"`
	StartedAt       string            `json:"startedAt"`
	Operator        string            `json:"operator,omitempty"`
}

// MoveService is the part of a Kipper Service a move reads.
type MoveService struct {
	UID               string
	Storage           string
	Mark              string
	ConsistentBackups bool
	CopiesChange      string
}

// MoveStatefulSet is the part of a service's StatefulSet a move reads.
// Replicas is the count a move restores: the saved count while the
// StatefulSet carries the mark, its current count otherwise.
type MoveStatefulSet struct {
	FenceStopped bool
	Mark         string
	Replicas     int32
}

// MoveClaim is a PersistentVolumeClaim as a move sees it. Size is the
// requested size and Capacity the size the volume reports. ResizePending
// means the volume grew and its filesystem grows at the next mount.
type MoveClaim struct {
	Namespace     string
	Name          string
	UID           string
	Class         string
	Phase         string
	VolumeName    string
	Size          string
	Capacity      string
	ResizePending bool
	Labels        map[string]string
	Annotations   map[string]string
	Deleting      bool
}

// MovePV is a PersistentVolume as a move sees it.
type MovePV struct {
	Name            string
	UID             string
	Class           string
	Phase           string
	Reclaim         string
	Capacity        string
	Path            string
	Node            string
	ClaimNamespace  string
	ClaimName       string
	ClaimUID        string
	Labels          map[string]string
	Annotations     map[string]string
	ResourceVersion string
}

// PVPatch changes a PersistentVolume. Empty Reclaim keeps the policy. A nil
// map value removes that label or annotation. RebindTo, when set, replaces
// the claim reference with the namespace and claim name and clears its UID
// and resource version, so a new claim of that name can bind the Released
// volume. A non-empty ResourceVersion rejects a patch if the volume has
// changed since the read.
type PVPatch struct {
	ResourceVersion string
	Reclaim         string
	Labels          map[string]*string
	Annotations     map[string]*string
	RebindTo        *ClaimRef
}

// ClaimRef names a claim.
type ClaimRef struct {
	Namespace string
	Name      string
}

// MoveFence records a namespace's claims protected by the storage move policy.
// Admission allows only admin-created copy and verify pods to mount protected claims.
// Enforced reflects the namespace's move label, not the policy's availability.
// Lapsed claims need their copies checked after a gap in enforcement.
// Each move clears its lapsed claims after checking its copy. Version guards
// updates against concurrent changes.
type MoveFence struct {
	Claims   []string
	Enforced bool
	Lapsed   []string
	Version  string
}

// NamespaceDeletion says whether a namespace is being deleted itself, and Why
// says why it or a Project holding it is being deleted, "" when neither is.
type NamespaceDeletion struct {
	Namespace bool
	Why       string
}

// MovePod is a pod that mounts one of the claims under move.
type MovePod struct {
	Name        string
	UID         string
	StatefulSet string
}

// SourceVolume is the read-only view of the retained local directory the copy
// pod reads: a hostPath volume pinned to the directory's node, pre-bound to
// one claim in the service namespace. The claim carries
// StorageMoveServiceLabel set to Service.
type SourceVolume struct {
	PV        string
	Claim     string
	Namespace string
	Service   string
	Path      string
	Node      string
	Capacity  string
}

// CopyPod copies Source into Target, or, as a verify pod, compares them.
type CopyPod struct {
	Namespace string
	Name      string
	Source    string
	Target    string
}

// CopyResult reports the progress of a copy or verify pod. Finished is false
// until the pod exits. Succeeded requires both a successful exit and the
// pod's success marker in Log. Differs reports a verify pod that found the
// copy different from its source. Failed pods include a termination reason,
// such as OOMKilled, when available.
type CopyResult struct {
	Finished  bool
	Succeeded bool
	Differs   bool
	UID       string
	Log       string
	Reason    string
}

// MoveBackupState is what a move needs to know about backups and transfers.
type MoveBackupState struct {
	Configured      bool
	Running         []string
	Transfers       []string
	LatestCompleted time.Time
}

// MoveCluster provides cluster operations for a storage move. Each call is an
// operation, which may make several requests. The engine handles polling.
type MoveCluster interface {
	// StorageMode returns the recorded mode, defaulting to Longhorn when unset.
	StorageMode() (string, error)
	// ServerNode returns the server hostname in lower case.
	ServerNode() (string, error)
	// PathExists reports whether path is a directory on the server.
	PathExists(path string) (bool, error)
	// UsedBytes returns the larger of allocated and apparent directory bytes
	// so capacity checks allow for sparse files becoming fully allocated.
	// Hard-linked files count once per name, as the copy writes them.
	UsedBytes(path string) (int64, error)
	// CountEntries estimates the number of filesystem entries for the downtime
	// calculation, including path itself and lost+found. Names containing line
	// breaks may inflate the count and are rejected later by the copy script.
	// An error means the traversal or count could not be read successfully.
	CountEntries(path string) (int64, error)
	// BackupState reports active backups and restores across the cluster and
	// active transfers in namespace. LatestCompleted is the newest completed
	// backup covering namespace, or zero if none has a valid completion time.
	// Missing backup or transfer resource types count as empty lists.
	BackupState(namespace string) (MoveBackupState, error)

	// ReadRecord returns nil with no error when the record is absent.
	// An unreadable record returns an error.
	ReadRecord(name string) (*MoveRecord, error)
	// SaveRecord creates or updates the JSON record in StorageMoveRecordNamespace.
	SaveRecord(name string, rec MoveRecord) error
	// DeleteRecord removes the saved record. An absent record is a success.
	DeleteRecord(name string) error

	// ReadService returns nil with no error when the service is absent.
	ReadService(namespace, name string) (*MoveService, error)
	// MarkService sets the storage move annotation, or removes it for an empty mark.
	MarkService(namespace, name, mark string) error
	// SetServiceStorage sets the requested storage size, or removes it for an empty size.
	SetServiceStorage(namespace, name, size string) error

	// ReadStatefulSet returns nil with no error when the StatefulSet is absent.
	// Replicas is the saved count for a marked StatefulSet, or its current count
	// otherwise. An invalid saved count returns an error.
	ReadStatefulSet(namespace, name string) (*MoveStatefulSet, error)
	// StopStatefulSet scales to 0. An unmarked StatefulSet saves mark and save,
	// using its current replica count if save is negative. A marked StatefulSet
	// keeps its annotations. The returned count is save if non-negative,
	// otherwise the saved count. A missing StatefulSet causes an error wrapping
	// errStatefulSetGone, and an invalid saved count needed for the return value
	// causes an error.
	StopStatefulSet(namespace, name, mark string, save int32) (int32, error)
	// StartStatefulSet removes the mark and scales a marked StatefulSet to
	// replicas, or to its saved count if replicas is negative. On success,
	// count is the resulting replica count and started reports whether the
	// StatefulSet was marked and patched, even when count is zero. An unmarked
	// StatefulSet keeps its current count and returns started false. A missing
	// StatefulSet causes an error wrapping errStatefulSetGone. An invalid saved
	// count needed for restoration also causes an error.
	StartStatefulSet(namespace, name string, replicas int32) (count int32, started bool, err error)
	// ServiceReady reports whether the service's ordinal-zero pod is Running
	// and Ready, with the scheduler's message while it cannot place the pod.
	// An absent pod returns false with no error.
	ServiceReady(namespace, name string) (ready bool, unschedulable string, err error)

	// ReadClaim returns nil with no error when the claim is absent.
	ReadClaim(namespace, name string) (*MoveClaim, error)
	// CreateClaim creates a ReadWriteOnce claim with the supplied class and
	// quantity. An invalid quantity or an existing claim returns an error.
	CreateClaim(claim MoveClaim) error
	// DeleteClaim requests deletion only while the claim's UID is uid.
	// An absent claim is a success. It returns without waiting for removal.
	DeleteClaim(namespace, name, uid string) error
	// ReadPV returns nil with no error when the volume is absent.
	ReadPV(name string) (*MovePV, error)
	// RetainedVolume returns the local volume retained by an unfinished move
	// for serviceUID, or nil if none exists. Multiple matching volumes cause
	// an error.
	RetainedVolume(serviceUID string) (*MovePV, error)
	// PatchPV applies the changes described by PVPatch to an existing volume.
	PatchPV(name string, patch PVPatch) error

	// ReadMoveFence returns the namespace's storage move fence. An absent
	// namespace is an error.
	ReadMoveFence(namespace string) (MoveFence, error)
	// WriteMoveFence sets the fence's claims and lapsed claims, guarded by
	// its version, and sets the move label while any claim remains. An empty
	// claim list removes the label and both lists.
	WriteMoveFence(namespace string, fence MoveFence) error
	// AdmissionRefuses reports whether a dry-run pod mounting claim is
	// refused by the storage move policy. A missing or non-enforcing binding
	// and failures unrelated to the policy's denial return errors.
	AdmissionRefuses(namespace, claim string) (bool, error)
	// NamespaceDeleteRefused reports whether a dry-run delete of the namespace
	// is refused by the namespace delete policy. A missing or non-enforcing
	// binding and failures unrelated to the policy's denial return errors.
	NamespaceDeleteRefused(namespace string) (bool, error)
	// NamespaceTerminating says whether the namespace, or a Project holding
	// it, is being deleted.
	NamespaceTerminating(namespace string) (NamespaceDeletion, error)
	// ClaimsLabeled lists the namespace's claims that match the label
	// selector.
	ClaimsLabeled(namespace, selector string) ([]MoveClaim, error)
	// ResourceState returns every node and pod in the cluster and the
	// namespace's ResourceQuotas and default priority class. An empty class
	// name means there is no global default.
	ResourceState(namespace string) (MoveResources, error)
	// PodsMounting lists pods referencing any supplied claim, excluding pods
	// that have succeeded or failed. Each pod appears once, in API list order.
	PodsMounting(namespace string, claims []string) ([]MovePod, error)

	// DeletePV requests deletion only while the volume's UID is uid.
	// An absent volume is a success. It returns without waiting for removal.
	DeletePV(name, uid string) error
	// CreateSourceVolume applies a Retain hostPath volume pinned to src.Node
	// and a pre-bound claim in src.Namespace. The copy pod mounts it read-only.
	// A failure may leave the volume created without its claim.
	CreateSourceVolume(src SourceVolume) error
	// StartCopy creates a pod that clears the target except lost+found, copies
	// the read-only source and flushes the copy to the volume. It returns the
	// pod's UID before completion and fails if the pod already exists.
	StartCopy(pod CopyPod) (string, error)
	// CopyResult returns an unfinished result while the pod is active and an
	// error if it is absent. Success requires a successful exit and the copy's
	// marker in the last 40 log lines. Unreadable logs count as an unsuccessful copy.
	CopyResult(namespace, name string) (CopyResult, error)
	// StartVerify creates a pod that mounts the source and the target
	// read-only and compares every entry. It returns the pod's UID before
	// completion and fails if the pod already exists.
	StartVerify(pod CopyPod) (string, error)
	// VerifyResult reads a verify pod as CopyResult reads a copy pod, with the
	// verification marker for success and the mismatch marker for Differs.
	VerifyResult(namespace, name string) (CopyResult, error)
	// VolumeDetached reports whether the Longhorn volume is detached. An
	// absent volume is an error.
	VolumeDetached(name string) (bool, error)
	// ExpandClaim patches the claim's requested size without waiting for the
	// resize. Invalid quantities and API rejections return errors.
	ExpandClaim(namespace, name, size string) error
	// PodExists reports whether the named pod exists, including terminal pods.
	PodExists(namespace, name string) (bool, error)
	// DeletePod deletes the pod and reports whether it is gone.
	DeletePod(namespace, name string) (bool, error)
}

// MovePlan describes the data to move and estimated downtime shown for consent.
type MovePlan struct {
	Namespace  string
	Service    string
	UsedBytes  int64
	Entries    int64
	TargetSize string
	DumpClaim  string
	BackupAge  time.Duration
	NoBackup   bool
	Downtime   time.Duration
}

// StorageMove moves one service's data volume from local-path onto a
// single-copy Longhorn volume. The local directory supports rollback before
// commit and is kept afterwards for manual recovery until pruned.
// Callers must hold the node lifecycle lease throughout Run or Rollback.
// Nil Out discards progress. Nil Now uses time.Now, and nil Sleep uses time.Sleep.
type StorageMove struct {
	Cluster   MoveCluster
	Namespace string
	Service   string
	Operator  string
	NoBackup  bool
	Out       io.Writer
	Now       func() time.Time
	Sleep     func(time.Duration)
	Consent   func(MovePlan) (bool, error)
	CheckRoom func(dataBytes, dumpBytes int64) error
}

// ErrMoveCanceled is returned when consent is refused.
var ErrMoveCanceled = errors.New("the storage move was canceled")

var errStatefulSetGone = errors.New("the StatefulSet is gone")

// StorageMoveRecordName names the record of a move of the service with uid.
func StorageMoveRecordName(uid string) string { return storageMoveRecordPrefix + uid }

// ServiceDataClaim names the data claim of a Kipper service's StatefulSet.
func ServiceDataClaim(service string) string { return "data-" + service + "-0" }

func moveDumpClaim(service string) string { return service + "-dump" }
func moveCopyPodName(service string) string {
	return platform.StorageMoveCopyPodPrefix + service
}

func moveVerifyPodName(service string) string {
	return platform.StorageMoveVerifyPodPrefix + service
}

func moveSourceClaim(service string) string { return "kipper-move-src-" + service }

func moveSource(rec *MoveRecord) SourceVolume {
	short := rec.ServiceUID
	if len(short) > 8 {
		short = short[:8]
	}
	return SourceVolume{
		PV:        "kipper-move-src-" + short,
		Claim:     moveSourceClaim(rec.Service),
		Namespace: rec.Namespace,
		Service:   rec.Service,
		Path:      rec.Path,
		Node:      rec.Node,
	}
}

// moveClaimLabels copies labels and sets the move's service label, preserving
// the caller's map.
func moveClaimLabels(labels map[string]string, service string) map[string]string {
	out := map[string]string{platform.StorageMoveServiceLabel: service}
	for k, v := range labels {
		if k != platform.StorageMoveServiceLabel {
			out[k] = v
		}
	}
	return out
}

func (m *StorageMove) printf(format string, args ...any) {
	if m.Out != nil {
		_, _ = fmt.Fprintf(m.Out, format, args...)
	}
}

// Run starts a move, or continues an unfinished one from its record. A
// record whose direction is rollback continues the rollback. Before a fresh
// move, a non-nil CheckRoom checks data and dump capacity in bytes, and a
// non-nil Consent asks permission. Refusal returns ErrMoveCanceled. Errors
// leave completed checkpoints available for a later Run or Rollback.
// Success restores a marked StatefulSet's saved replica count, which may
// leave the service stopped. An unmarked StatefulSet keeps its current count.
// The controller recreates a missing StatefulSet with one pod after the move
// releases the service. A readiness timeout is reported to Out without returning
// an error.
func (m *StorageMove) Run() error {
	svc, rec, err := m.load()
	if err != nil {
		return err
	}
	switch {
	case rec == nil:
		if svc.Mark != "" {
			return fmt.Errorf("service %s carries the storage move mark %s, and its move record is missing. Run 'kip service storage longhorn %s --rollback' to undo the move using its remaining volumes", m.Service, svc.Mark, m.Service)
		}
		if rec, err = m.start(svc); err != nil {
			return err
		}
	case rec.Direction == MoveRollback:
		m.printf("A rollback of this move is unfinished, so kip continues the rollback.\n")
		return m.rollback(rec)
	default:
		m.printf("Continuing the move of %s after step %q.\n", m.Service, rec.Step)
	}
	return m.forward(rec)
}

// Rollback undoes an unfinished move onto the local volume and restores
// the saved replica count if the StatefulSet is still marked. An unmarked
// StatefulSet keeps its current count. The controller recreates a missing
// StatefulSet with one pod after the move releases the service.
// It returns an error for a missing or committed move.
// For a marked service with a missing record, it reconstructs the record from
// the remaining volumes. If no local volume can be recovered, it clears the
// move and restarts the service on its current data claim. Failed rollbacks
// can be resumed with Run or Rollback. A readiness timeout is reported to Out
// without returning an error.
func (m *StorageMove) Rollback() error {
	svc, rec, err := m.load()
	if err != nil {
		return err
	}
	if rec == nil && svc.Mark != "" {
		return m.rollbackWithoutRecord(svc)
	}
	if rec == nil {
		return fmt.Errorf("service %s has no unfinished storage move to roll back", m.Service)
	}
	if rec.Step == MoveCommitted {
		return fmt.Errorf("the move of %s is committed: the service's data is on Longhorn, and writes since then exist only there. Run 'kip service storage longhorn %s' to finish it. The local copy is old and 'kip cluster storage prune' deletes it", m.Service, m.Service)
	}
	if rec.Direction != MoveRollback {
		rec.Direction = MoveRollback
		if err := m.save(rec); err != nil {
			return err
		}
	}
	return m.rollback(rec)
}

// rollbackWithoutRecord rebuilds the record of a move whose record is gone
// and rolls it back. The local volume is either still bound to the data claim
// or kept with the move's annotations. With neither left, the move's fence
// and marks are cleared and the service starts on the data claim it has,
// because it may hold the only remaining data.
func (m *StorageMove) rollbackWithoutRecord(svc *MoveService) error {
	m.printf("The move record for %s is missing, so kip rebuilds it from the remaining volumes.\n", m.Service)
	sts, err := m.Cluster.ReadStatefulSet(m.Namespace, m.Service)
	if err != nil {
		return err
	}
	// The controller recreates a missing StatefulSet with one pod.
	replicas := int32(1)
	if sts != nil {
		replicas = sts.Replicas
	}
	rec := &MoveRecord{
		ServiceUID:      svc.UID,
		Namespace:       m.Namespace,
		Service:         m.Service,
		Direction:       MoveRollback,
		Step:            MoveStopped,
		Replicas:        replicas,
		ReplicasSaved:   true,
		OriginalStorage: svc.Storage,
		StartedAt:       m.now().UTC().Format(time.RFC3339),
		Operator:        m.Operator,
	}
	if err := m.findDump(rec); err != nil {
		return err
	}
	found, err := m.recoverVolume(rec)
	if err != nil {
		return err
	}
	if !found {
		m.printf("No local copy of the data for %s could be recovered, so kip clears the move and starts the service on its data claim %s. Check its data, and restore it from a backup if anything is missing.\n", m.Service, ServiceDataClaim(m.Service))
		if err := m.removeCopyPod(rec); err != nil {
			return err
		}
		return m.restart(rec)
	}
	if err := m.save(rec); err != nil {
		return fmt.Errorf("writing the move record: %w", err)
	}
	return m.rollback(rec)
}

// findDump fills in the dump claim of a move whose record is gone, found by
// the label the controller gives it, so the rollback fences it and the
// restart clears its fence entry. A dump the move already released is
// recorded by name for the same reason. A claim by that name without the
// label is not the service's dump and is left out.
func (m *StorageMove) findDump(rec *MoveRecord) error {
	name := moveDumpClaim(rec.Service)
	dumps, err := m.Cluster.ClaimsLabeled(rec.Namespace, dumpForLabel+"="+rec.Service)
	if err != nil {
		return err
	}
	for _, d := range dumps {
		if d.Name == name {
			rec.DumpClaim, rec.DumpClaimUID = d.Name, d.UID
			return nil
		}
	}
	existing, err := m.Cluster.ReadClaim(rec.Namespace, name)
	if err != nil {
		return err
	}
	if existing == nil {
		rec.DumpClaim = name
	}
	return nil
}

// recoverVolume fills in the local volume of a move whose record is gone:
// the data claim's own volume while it is still bound, or else the volume
// the move kept for this service. It reports false when neither exists.
func (m *StorageMove) recoverVolume(rec *MoveRecord) (bool, error) {
	c := m.Cluster
	claim, err := c.ReadClaim(rec.Namespace, ServiceDataClaim(rec.Service))
	if err != nil {
		return false, err
	}
	if claim != nil && claim.Class == platform.LocalPathClass && claim.Phase == "Bound" && !claim.Deleting {
		pv, err := c.ReadPV(claim.VolumeName)
		if err != nil {
			return false, err
		}
		if pv != nil && pv.ClaimUID == claim.UID {
			rec.OldClaimUID, rec.OldClaimLabels = claim.UID, claim.Labels
			rec.OldPV, rec.OldPVUID, rec.Path, rec.Node = pv.Name, pv.UID, pv.Path, pv.Node
			return true, nil
		}
	}
	pv, err := c.RetainedVolume(rec.ServiceUID)
	if err != nil || pv == nil {
		return false, err
	}
	if pv.Path != pv.Annotations[retainedPathAnnotation] || !strings.HasPrefix(pv.Path, localPathRoot) || strings.Contains(pv.Path, "..") {
		return false, fmt.Errorf("the kept local volume %s of %s keeps its data at %q, not at the path the move recorded, so kip stops", pv.Name, rec.Service, pv.Path)
	}
	rec.Step = MoveClaimed
	rec.OldClaimUID = pv.Annotations[retainedClaimUIDAnnotation]
	rec.OldPV, rec.OldPVUID, rec.Path, rec.Node = pv.Name, pv.UID, pv.Path, pv.Node
	rec.UsedBytes, _ = strconv.ParseInt(pv.Annotations[platform.RetainedLocalCopyUsedBytesAnnotation], 10, 64)
	// The claim the rollback creates needs the service label, by which
	// deleting the service with its data finds it. The move's Longhorn claim
	// carries the old claim's labels.
	rec.OldClaimLabels = map[string]string{}
	if claim != nil && (claim.Annotations[platform.StorageMoveAnnotation] == StorageMoveRecordName(rec.ServiceUID) || claim.Labels[platform.StorageMoveServiceLabel] == rec.Service) {
		rec.TargetSize = claim.Size
		for k, v := range claim.Labels {
			rec.OldClaimLabels[k] = v
		}
	}
	rec.OldClaimLabels[datavolume.LabelKey] = rec.Service
	return true, nil
}

func (m *StorageMove) load() (*MoveService, *MoveRecord, error) {
	svc, err := m.Cluster.ReadService(m.Namespace, m.Service)
	if err != nil {
		return nil, nil, err
	}
	if svc == nil {
		return nil, nil, fmt.Errorf("service %s not found in %s", m.Service, m.Namespace)
	}
	rec, err := m.Cluster.ReadRecord(StorageMoveRecordName(svc.UID))
	if err != nil {
		return nil, nil, err
	}
	return svc, rec, nil
}

func (m *StorageMove) save(rec *MoveRecord) error {
	return m.Cluster.SaveRecord(StorageMoveRecordName(rec.ServiceUID), *rec)
}

func (m *StorageMove) checkpoint(rec *MoveRecord, step string) error {
	rec.Step = step
	if err := m.save(rec); err != nil {
		return fmt.Errorf("recording step %q: %w", step, err)
	}
	return nil
}

// start runs the preflight, asks consent and writes the record.
func (m *StorageMove) start(svc *MoveService) (*MoveRecord, error) {
	rec, plan, err := m.preflight(svc)
	if err != nil {
		return nil, err
	}
	if m.Consent != nil {
		ok, err := m.Consent(plan)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, ErrMoveCanceled
		}
	}
	rec.Step = MoveRecorded
	if err := m.save(rec); err != nil {
		return nil, fmt.Errorf("writing the move record: %w", err)
	}
	return rec, nil
}

func (m *StorageMove) preflight(svc *MoveService) (*MoveRecord, MovePlan, error) {
	c := m.Cluster
	mode, err := c.StorageMode()
	if err != nil {
		return nil, MovePlan{}, err
	}
	if platform.StorageMode(mode) != platform.StorageLonghorn {
		return nil, MovePlan{}, fmt.Errorf("this cluster uses %s storage. Run 'kip cluster storage longhorn' first", mode)
	}
	sts, err := c.ReadStatefulSet(m.Namespace, m.Service)
	if err != nil {
		return nil, MovePlan{}, err
	}
	switch {
	case sts == nil:
		return nil, MovePlan{}, fmt.Errorf("service %s has no StatefulSet yet. Wait until it is running and try again", m.Service)
	case sts.FenceStopped:
		return nil, MovePlan{}, fmt.Errorf("service %s was stopped by 'kip node fence'. Recover it with 'kip node fence' first", m.Service)
	case sts.Mark != "":
		return nil, MovePlan{}, fmt.Errorf("the StatefulSet of %s carries the storage move mark %s without a move record", m.Service, sts.Mark)
	}
	if d, err := c.NamespaceTerminating(m.Namespace); err != nil {
		return nil, MovePlan{}, err
	} else if d.Why != "" {
		return nil, MovePlan{}, fmt.Errorf("%s, so kip does not start moving %s", d.Why, m.Service)
	}

	claim, err := c.ReadClaim(m.Namespace, ServiceDataClaim(m.Service))
	if err != nil {
		return nil, MovePlan{}, err
	}
	if claim == nil {
		return nil, MovePlan{}, fmt.Errorf("service %s has no data volume %s", m.Service, ServiceDataClaim(m.Service))
	}
	if claim.Class != platform.LocalPathClass {
		return nil, MovePlan{}, fmt.Errorf("the data volume of %s uses %s, so there is nothing to move", m.Service, claim.Class)
	}
	if claim.Phase != "Bound" || claim.VolumeName == "" {
		return nil, MovePlan{}, fmt.Errorf("the data volume of %s is %s, not bound", m.Service, strings.ToLower(claim.Phase))
	}
	pv, err := c.ReadPV(claim.VolumeName)
	if err != nil {
		return nil, MovePlan{}, err
	}
	if pv == nil || pv.ClaimUID != claim.UID {
		return nil, MovePlan{}, fmt.Errorf("the volume %s is not bound to the data claim of %s", claim.VolumeName, m.Service)
	}
	if !strings.HasPrefix(pv.Path, localPathRoot) || strings.Contains(pv.Path, "..") {
		return nil, MovePlan{}, fmt.Errorf("the volume %s keeps its data at %q, outside %s", pv.Name, pv.Path, localPathRoot)
	}
	server, err := c.ServerNode()
	if err != nil {
		return nil, MovePlan{}, err
	}
	if pv.Node == "" || !strings.EqualFold(pv.Node, server) {
		return nil, MovePlan{}, fmt.Errorf("the volume %s lives on node %q, and kip moves only volumes on the server %q", pv.Name, pv.Node, server)
	}
	exists, err := c.PathExists(pv.Path)
	if err != nil {
		return nil, MovePlan{}, err
	}
	if !exists {
		return nil, MovePlan{}, fmt.Errorf("the directory %s of volume %s is missing on the server", pv.Path, pv.Name)
	}

	if src, err := c.ReadClaim(m.Namespace, moveSourceClaim(m.Service)); err != nil {
		return nil, MovePlan{}, err
	} else if src != nil {
		return nil, MovePlan{}, fmt.Errorf("a claim %s exists in %s, and the move needs that name for its read-only view of the data. Rename or remove it, then try again", src.Name, m.Namespace)
	}

	backups, err := c.BackupState(m.Namespace)
	if err != nil {
		return nil, MovePlan{}, err
	}
	if len(backups.Running) > 0 {
		return nil, MovePlan{}, fmt.Errorf("a backup or restore is running (%s). Try again once it finishes", strings.Join(backups.Running, ", "))
	}
	if len(backups.Transfers) > 0 {
		return nil, MovePlan{}, fmt.Errorf("a migration transfer exists in %s (%s). Try again once the migration finishes", m.Namespace, strings.Join(backups.Transfers, ", "))
	}
	plan := MovePlan{Namespace: m.Namespace, Service: m.Service, NoBackup: m.NoBackup}
	switch {
	case backups.Configured && backups.LatestCompleted.IsZero():
		return nil, MovePlan{}, fmt.Errorf("no completed backup covers %s. Run 'kip backup create' and try again", m.Namespace)
	case backups.Configured:
		plan.BackupAge = m.now().Sub(backups.LatestCompleted)
		plan.NoBackup = false
	case !m.NoBackup:
		return nil, MovePlan{}, fmt.Errorf("this cluster has no backups, so nothing could restore %s if the move went wrong. "+
			"Add backups by running kip install again with --backup-storage-bucket and its related flags, or pass --no-backup to move it anyway", m.Service)
	}

	used, err := c.UsedBytes(pv.Path)
	if err != nil {
		return nil, MovePlan{}, err
	}
	target, err := moveTargetSize(claim.Size, used)
	if err != nil {
		return nil, MovePlan{}, err
	}
	entries, err := c.CountEntries(pv.Path)
	if err != nil {
		return nil, MovePlan{}, err
	}
	dump, err := c.ReadClaim(m.Namespace, moveDumpClaim(m.Service))
	if err != nil {
		return nil, MovePlan{}, err
	}
	dumpName, dumpUID := "", ""
	if dump != nil && dump.Labels[dumpForLabel] == m.Service && dump.Class == platform.LocalPathClass {
		dumpName, dumpUID = dump.Name, dump.UID
	}
	if m.CheckRoom != nil {
		size := resource.MustParse(target)
		var dumpBytes int64
		if svc.ConsistentBackups {
			dumpBytes = size.Value()
		}
		if err := m.CheckRoom(size.Value(), dumpBytes); err != nil {
			return nil, MovePlan{}, err
		}
	}
	res, err := c.ResourceState(m.Namespace)
	if err != nil {
		return nil, MovePlan{}, err
	}
	if err := moveFitRefusal(res, pv.Node, m.Namespace, m.Service); err != nil {
		return nil, MovePlan{}, err
	}
	if err := moveQuotaRefusal(res, m.Namespace, m.Service); err != nil {
		return nil, MovePlan{}, err
	}
	plan.UsedBytes, plan.Entries, plan.TargetSize, plan.DumpClaim, plan.Downtime = used, entries, target, dumpName, moveDowntime(used, entries)

	return &MoveRecord{
		ServiceUID:      svc.UID,
		Namespace:       m.Namespace,
		Service:         m.Service,
		Direction:       MoveForward,
		OriginalStorage: svc.Storage,
		OldClaimUID:     claim.UID,
		OldClaimLabels:  claim.Labels,
		OldPV:           pv.Name,
		OldPVUID:        pv.UID,
		Path:            pv.Path,
		Node:            pv.Node,
		UsedBytes:       used,
		TargetSize:      target,
		DumpClaim:       dumpName,
		DumpClaimUID:    dumpUID,
		StartedAt:       m.now().UTC().Format(time.RFC3339),
		Operator:        m.Operator,
	}, plan, nil
}

// moveDowntime estimates downtime using 10 MiB/s for copying and verifying,
// 3 ms per entry and three minutes for draining, detaching and starting.
// It rounds up to whole minutes. These allowances are conservative:
// a 6.1 GiB live move achieved 12.6 MiB/s, and copying and verifying small files
// on Longhorn took 2.6 ms per file.
func moveDowntime(used, entries int64) time.Duration {
	const rate = 10 << 20
	const perEntry = 3 * time.Millisecond
	d := 3*time.Minute + time.Duration(float64(used)/rate*float64(time.Second)) + time.Duration(entries)*perEntry
	return (d + time.Minute - 1).Truncate(time.Minute)
}

// moveTargetSize adds 20% to used bytes and rounds up to at least one GiB
// so the copy has room to grow. A larger declared size is preserved exactly.
func moveTargetSize(declared string, used int64) (string, error) {
	const gib = int64(1) << 30
	want := int64(math.Ceil(float64(used) * 1.2 / float64(gib)))
	if want < 1 {
		want = 1
	}
	if declared != "" {
		q, err := resource.ParseQuantity(declared)
		if err != nil {
			return "", fmt.Errorf("reading the declared size %q: %w", declared, err)
		}
		if q.Value() >= want*gib {
			return declared, nil
		}
	}
	return strconv.FormatInt(want, 10) + "Gi", nil
}

func (m *StorageMove) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *StorageMove) sleep(d time.Duration) {
	if m.Sleep != nil {
		m.Sleep(d)
		return
	}
	time.Sleep(d)
}

// waitFor polls done every movePoll, up to polls times.
func (m *StorageMove) waitFor(what string, polls int, done func() (bool, error)) error {
	for i := 0; i < polls; i++ {
		ok, err := done()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		m.sleep(movePoll)
	}
	return fmt.Errorf("%w %s", errTimedOut, what)
}

// errTimedOut is the error waitFor wraps when its polls run out.
var errTimedOut = errors.New("timed out waiting for")

// claims lists the claims the admission fence covers. The source view is
// fenced before it exists, so only admin-created copy and verify pods can mount it.
func (rec *MoveRecord) claims() []string {
	claims := []string{ServiceDataClaim(rec.Service), moveSourceClaim(rec.Service)}
	if rec.DumpClaim != "" {
		claims = append(claims, rec.DumpClaim)
	}
	return claims
}

func stepIndex(step string) int {
	for i, s := range []string{MoveRecorded, MoveStopped, MoveRetained, MoveReleased, MoveClaimed, MoveCopied, MoveDumpReleased, MoveCommitted} {
		if s == step {
			return i
		}
	}
	return -1
}

func (m *StorageMove) forward(rec *MoveRecord) error {
	steps := []struct {
		name string
		run  func(*MoveRecord) error
	}{
		{MoveStopped, m.stop},
		{MoveRetained, m.retain},
		{MoveReleased, m.release},
		{MoveClaimed, m.claim},
		{MoveCopied, m.copyData},
		{MoveDumpReleased, m.afterCopy},
		{MoveCommitted, m.commit},
	}
	done := stepIndex(rec.Step)
	if done < 0 {
		return fmt.Errorf("the move record of %s names an unknown step %q", m.Service, rec.Step)
	}
	if done >= stepIndex(MoveStopped) && done < stepIndex(MoveCommitted) {
		// The stop and the second verification may send the move back to copy again.
		if err := m.stop(rec); err != nil {
			return err
		}
		if err := m.reverify(rec); err != nil {
			return err
		}
		done = stepIndex(rec.Step)
	}
	for _, s := range steps {
		if stepIndex(s.name) <= done {
			continue
		}
		if err := s.run(rec); err != nil {
			return err
		}
		if err := m.checkpoint(rec, s.name); err != nil {
			return err
		}
	}
	return m.finish(rec)
}

// stopped reports whether the replica count has been saved for restoration.
// The stop may still be waiting for service pods to exit.
func (rec *MoveRecord) stopped() bool {
	return rec.ReplicasSaved
}

// restoreCount returns the recorded replica count, or -1 to use the count
// saved on the StatefulSet.
func (rec *MoveRecord) restoreCount() int32 {
	if rec.ReplicasSaved {
		return rec.Replicas
	}
	return -1
}

// stop fences the claims at admission, drains requests already admitted,
// marks the service and stops its StatefulSet. A resumed move or rollback
// runs it again, because the fence may have been lifted while kip was not
// running. It then keeps the replica count saved by the first stop.
func (m *StorageMove) stop(rec *MoveRecord) error {
	resumed := rec.stopped()
	if resumed {
		m.printf("Checking that %s is stopped and every pod is kept off its volume...\n", rec.Service)
		if err := m.recheckCopy(rec); err != nil {
			return err
		}
	} else {
		m.printf("Stopping %s and keeping every pod off its volume...\n", rec.Service)
		if err := m.noteFenceAdded(rec); err != nil {
			return err
		}
	}
	if err := m.addMoveClaims(rec.Namespace, rec.claims()); err != nil {
		return err
	}
	data := ServiceDataClaim(rec.Service)
	if err := m.waitFor("the admission policy to refuse pods on "+data, 30, func() (bool, error) {
		return m.Cluster.AdmissionRefuses(rec.Namespace, data)
	}); err != nil {
		return fmt.Errorf("%w. Check that the %s admission policy is installed ('kip upgrade' installs it)", err, platform.StorageMovePodsPolicy)
	}
	if err := m.guardNamespace(rec, resumed); err != nil {
		return err
	}
	m.sleep(moveDrainWait)
	// The policy reserves helper pod names for admins. Before the first stop,
	// any existing helper needs operator review because this move has created
	// none. A resumed move keeps only helpers with its recorded UIDs.
	if !resumed {
		for _, helper := range []string{moveCopyPodName(rec.Service), moveVerifyPodName(rec.Service)} {
			if exists, err := m.Cluster.PodExists(rec.Namespace, helper); err != nil {
				return err
			} else if exists {
				return fmt.Errorf("a pod %s exists in %s, and the move needs that name for its own pod. Remove it, then run the command again", helper, rec.Namespace)
			}
		}
	} else if err := m.dropStrangeHelperPods(rec); err != nil {
		return err
	}
	if err := m.Cluster.MarkService(rec.Namespace, rec.Service, StorageMoveRecordName(rec.ServiceUID)); err != nil {
		return fmt.Errorf("marking %s: %w", rec.Service, err)
	}
	save := int32(-1)
	if resumed {
		save = rec.Replicas
	}
	replicas, err := m.Cluster.StopStatefulSet(rec.Namespace, rec.Service, StorageMoveRecordName(rec.ServiceUID), save)
	switch {
	case resumed && errors.Is(err, errStatefulSetGone):
		// The saved count lets the move resume. The service marker prevents
		// the controller from recreating the StatefulSet while existing pods drain.
	case errors.Is(err, errStatefulSetGone):
		return fmt.Errorf("stopping %s: %w. Run 'kip service storage longhorn %s --rollback' to end the move", rec.Service, err, rec.Service)
	case err != nil:
		return fmt.Errorf("stopping %s: %w", rec.Service, err)
	default:
		rec.Replicas, rec.ReplicasSaved = replicas, true
	}
	if err := m.waitFor("the pods of "+rec.Service+" to stop", 150, func() (bool, error) {
		pods, err := m.foreignPods(rec, true)
		if err != nil {
			return false, err
		}
		return len(pods) == 0, nil
	}); err != nil {
		return err
	}
	if rec.Direction == MoveForward && stepIndex(rec.Step) < stepIndex(MoveReleased) {
		return m.measureStopped(rec)
	}
	return nil
}

// noteFenceAdded records which of the move's claims the fence does not hold
// yet, before the move adds them. A rerun of a move that never stopped keeps
// what an earlier run recorded.
func (m *StorageMove) noteFenceAdded(rec *MoveRecord) error {
	fence, err := m.Cluster.ReadMoveFence(rec.Namespace)
	if err != nil {
		return err
	}
	added := append([]string(nil), rec.FenceAdded...)
	for _, c := range rec.claims() {
		if !containsString(fence.Claims, c) && !containsString(added, c) {
			added = append(added, c)
		}
	}
	if sortedEqual(added, rec.FenceAdded) {
		return nil
	}
	rec.FenceAdded = added
	return m.save(rec)
}

// guardNamespace reads the namespace and its Project again once the fence
// is up. A move whose namespace is being deleted steps aside if it has not
// stopped the service yet, and otherwise carries on while the deletion waits
// for it. A namespace that is not itself being deleted must refuse deletion
// before the move marks or stops the service, even while its Project is deleted.
func (m *StorageMove) guardNamespace(rec *MoveRecord, resumed bool) error {
	d, err := m.Cluster.NamespaceTerminating(rec.Namespace)
	if err != nil {
		return err
	}
	if d.Why != "" && !resumed && rec.Direction == MoveForward {
		sts, err := m.Cluster.ReadStatefulSet(rec.Namespace, rec.Service)
		if err != nil {
			return err
		}
		if sts == nil || sts.Mark == "" {
			return m.stepAside(rec, d.Why)
		}
	}
	if !d.Namespace {
		if err := m.waitFor("the admission policy to refuse deleting namespace "+rec.Namespace, 30, func() (bool, error) {
			return m.Cluster.NamespaceDeleteRefused(rec.Namespace)
		}); err != nil {
			return fmt.Errorf("%w. Check that the %s admission policy is installed ('kip upgrade' installs it)", err, platform.StorageMoveNamespacesPolicy)
		}
	}
	if d.Why != "" {
		m.printf("%s. The move of %s has already stopped it, so the move goes on and the deletion waits until it finishes or rolls back.\n", d.Why, rec.Service)
	}
	return nil
}

// stepAside ends a move that has not stopped its service: the claims this
// move fenced leave the fence, which keeps its label while another move
// holds claims, and the record goes.
func (m *StorageMove) stepAside(rec *MoveRecord, why string) error {
	if err := m.Cluster.MarkService(rec.Namespace, rec.Service, ""); err != nil {
		return fmt.Errorf("unmarking %s: %w", rec.Service, err)
	}
	if err := m.removeMoveClaims(rec.Namespace, rec.FenceAdded); err != nil {
		return err
	}
	if err := m.Cluster.DeleteRecord(StorageMoveRecordName(rec.ServiceUID)); err != nil {
		return fmt.Errorf("deleting the move record: %w", err)
	}
	return fmt.Errorf("%s, so kip stopped the move of %s before changing its storage. The service keeps running on its local volume", why, rec.Service)
}

// measureStopped measures data after writers have stopped and increases the
// planned Longhorn volume size if data grew since confirmation. It runs
// before the Longhorn claim exists.
func (m *StorageMove) measureStopped(rec *MoveRecord) error {
	used, err := m.Cluster.UsedBytes(rec.Path)
	if err != nil {
		return err
	}
	if used <= rec.UsedBytes {
		return nil
	}
	rec.UsedBytes = used
	target, err := moveTargetSize(rec.TargetSize, used)
	if err != nil || target == rec.TargetSize {
		return err
	}
	if m.CheckRoom != nil {
		svc, err := m.Cluster.ReadService(rec.Namespace, rec.Service)
		if err != nil {
			return err
		}
		size := resource.MustParse(target)
		var dumpBytes int64
		if svc != nil && svc.ConsistentBackups {
			dumpBytes = size.Value()
		}
		if err := m.CheckRoom(size.Value(), dumpBytes); err != nil {
			return fmt.Errorf("%s holds %s now that it has stopped, which needs a %s volume: %w. Run the command with --rollback to start it again on its local volume", rec.Service, formatBytes(used), target, err)
		}
	}
	m.printf("%s holds %s now that it has stopped, so its Longhorn volume will be %s.\n", rec.Service, formatBytes(used), target)
	rec.TargetSize = target
	return nil
}

// recheckCopy schedules another copy if the fence lapsed or another pod
// mounts the destination, because either may have allowed writes after
// verification. The copy may have verified after its pod was recorded but
// before the copied checkpoint. This check runs before restoring the fence
// or stopping pods, while evidence of possible writes is still available.
func (m *StorageMove) recheckCopy(rec *MoveRecord) error {
	step := stepIndex(rec.Step)
	started := step == stepIndex(MoveClaimed) && (rec.CopyPodUID != "" || rec.CopyDone)
	if rec.Direction != MoveForward || step >= stepIndex(MoveCommitted) || (step < stepIndex(MoveCopied) && !started) {
		return nil
	}
	data := ServiceDataClaim(rec.Service)
	fence, err := m.Cluster.ReadMoveFence(rec.Namespace)
	if err != nil {
		return err
	}
	why := ""
	if !fence.Enforced || !containsString(fence.Claims, data) || containsString(fence.Lapsed, data) {
		why = "the fence that keeps pods off " + data + " was down"
	} else {
		pods, err := m.Cluster.PodsMounting(rec.Namespace, []string{data})
		if err != nil {
			return err
		}
		for _, p := range pods {
			if !rec.ownsHelperPod(p) {
				why = "the pod " + p.Name + " mounts " + data
				break
			}
		}
	}
	if why == "" {
		return nil
	}
	m.printf("Since the copy started, %s, so kip copies the data again.\n", why)
	rec.forgetCopy()
	return m.checkpoint(rec, MoveClaimed)
}

// ownsHelperPod reports whether p is the move's own copy or verify pod, by
// name and recorded UID.
func (rec *MoveRecord) ownsHelperPod(p MovePod) bool {
	switch p.Name {
	case moveCopyPodName(rec.Service):
		return rec.CopyPodUID != "" && p.UID == rec.CopyPodUID
	case moveVerifyPodName(rec.Service):
		return rec.VerifyPodUID != "" && p.UID == rec.VerifyPodUID
	}
	return false
}

// forgetCopy drops the copy and its verification, so the next copy step
// copies again.
func (rec *MoveRecord) forgetCopy() {
	rec.CopyPodUID, rec.CopyDone, rec.VerifyPodUID = "", false, ""
}

// reverify compares a forward move's verified copy with its source again
// when the move resumes past its copy, because the fence may have been down
// while kip was away, and a writer that has left leaves only its changes.
// A move that released its dump volume removed its source view, so the view
// is made again and removed afterwards. A mismatch sends the move back to
// copy again. A committed move is never rewound.
func (m *StorageMove) reverify(rec *MoveRecord) error {
	step := stepIndex(rec.Step)
	if rec.Direction != MoveForward || step < stepIndex(MoveCopied) || step >= stepIndex(MoveCommitted) {
		return nil
	}
	if err := m.requireNoPods(rec); err != nil {
		return err
	}
	if err := m.requireDestination(rec); err != nil {
		return err
	}
	pv, err := m.retainedSource(rec)
	if err != nil {
		return err
	}
	m.printf("The move of %s was interrupted after its copy was verified, so kip compares the copy with its source again...\n", rec.Service)
	if err := m.verify(rec, pv); err != nil {
		if !errors.Is(err, errCopyDiffers) {
			return err
		}
		m.printf("The copy no longer matches its source, so kip copies the data again. %s\n", strings.TrimPrefix(err.Error(), errCopyDiffers.Error()+". "))
		return m.checkpoint(rec, MoveClaimed)
	}
	if step >= stepIndex(MoveDumpReleased) {
		return m.removeSource(rec)
	}
	return nil
}

// dropStrangeHelperPods deletes a pod by the copy pod's name that is not this
// move's own copy pod still in use, and any pod by the verify pod's name,
// since a resumed move never reuses an earlier verification. A pod the move
// did not start may have written the volume while the fence was down, so a
// forward move whose copy had finished records that it goes back to copy
// again before the pod is deleted.
func (m *StorageMove) dropStrangeHelperPods(rec *MoveRecord) error {
	for _, h := range []struct {
		name, uid string
		result    func(namespace, name string) (CopyResult, error)
	}{
		{moveCopyPodName(rec.Service), rec.CopyPodUID, m.Cluster.CopyResult},
		{moveVerifyPodName(rec.Service), rec.VerifyPodUID, m.Cluster.VerifyResult},
	} {
		exists, err := m.Cluster.PodExists(rec.Namespace, h.name)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		r, err := h.result(rec.Namespace, h.name)
		if err != nil {
			return err
		}
		own := h.uid != "" && r.UID == h.uid
		if own && h.name == moveCopyPodName(rec.Service) && stepIndex(rec.Step) < stepIndex(MoveDumpReleased) {
			continue
		}
		copied := stepIndex(rec.Step) >= stepIndex(MoveCopied) || rec.CopyDone
		if !own && rec.Direction == MoveForward && copied {
			m.printf("Pod %s was started outside this move and may have written to its volume, so kip copies the data again.\n", h.name)
			rec.forgetCopy()
			if err := m.checkpoint(rec, MoveClaimed); err != nil {
				return err
			}
		}
		if err := m.deletePod(rec.Namespace, h.name); err != nil {
			return err
		}
	}
	return nil
}

func (m *StorageMove) deletePod(namespace, name string) error {
	return m.waitFor("the pod "+name+" to go", 90, func() (bool, error) {
		return m.Cluster.DeletePod(namespace, name)
	})
}

// foreignPods returns the pods that mount the move's claims. StatefulSet
// pods are returned only when withService is set. Any other pod except the
// move's own copy pod, known by the UID the record holds, is an error.
func (m *StorageMove) foreignPods(rec *MoveRecord, withService bool) ([]MovePod, error) {
	pods, err := m.Cluster.PodsMounting(rec.Namespace, rec.claims())
	if err != nil {
		return nil, err
	}
	var service, other []string
	var found []MovePod
	for _, p := range pods {
		switch {
		case rec.ownsHelperPod(p):
		case p.StatefulSet == rec.Service:
			service = append(service, p.Name)
			found = append(found, p)
		default:
			other = append(other, p.Name)
		}
	}
	if len(other) > 0 {
		sort.Strings(other)
		return nil, fmt.Errorf("pods outside %s mount its volume: %s. Stop them, then run the command again", rec.Service, strings.Join(other, ", "))
	}
	if len(service) > 0 && !withService {
		sort.Strings(service)
		return nil, fmt.Errorf("pods of %s are running again during the move: %s", rec.Service, strings.Join(service, ", "))
	}
	return found, nil
}

func (m *StorageMove) requireNoPods(rec *MoveRecord) error {
	_, err := m.foreignPods(rec, false)
	return err
}

// addMoveClaims puts claims under the namespace's fence. The fence is
// enforced afterwards, and the claims are no longer listed as lapsed: a
// resumed move checks its copy before it fences its claims again.
func (m *StorageMove) addMoveClaims(namespace string, add []string) error {
	return m.editMoveFence(namespace, func(f *MoveFence) {
		for _, c := range add {
			if !containsString(f.Claims, c) {
				f.Claims = append(f.Claims, c)
			}
		}
		f.Lapsed = withoutStrings(f.Lapsed, add)
	})
}

func (m *StorageMove) removeMoveClaims(namespace string, remove []string) error {
	return m.editMoveFence(namespace, func(f *MoveFence) {
		f.Claims = withoutStrings(f.Claims, remove)
		f.Lapsed = withoutStrings(f.Lapsed, remove)
	})
}

func withoutStrings(list, remove []string) []string {
	var kept []string
	for _, s := range list {
		if !containsString(remove, s) {
			kept = append(kept, s)
		}
	}
	return kept
}

// editMoveFence retries failed writes up to five times, rereading the fence
// to preserve concurrent changes. If the namespace label is missing, existing
// claims are marked as lapsed before edit runs. Remaining claims restore the
// label, and other moves can detect the gap when checking their copies.
func (m *StorageMove) editMoveFence(namespace string, edit func(*MoveFence)) error {
	var last error
	for i := 0; i < 5; i++ {
		read, err := m.Cluster.ReadMoveFence(namespace)
		if err != nil {
			return err
		}
		next := MoveFence{
			Claims:  append([]string(nil), read.Claims...),
			Lapsed:  append([]string(nil), read.Lapsed...),
			Version: read.Version,
		}
		if !read.Enforced {
			for _, c := range read.Claims {
				if !containsString(next.Lapsed, c) {
					next.Lapsed = append(next.Lapsed, c)
				}
			}
		}
		edit(&next)
		next.Lapsed = withoutStrings(next.Lapsed, withoutStrings(next.Lapsed, next.Claims))
		sort.Strings(next.Claims)
		sort.Strings(next.Lapsed)
		same := sortedEqual(read.Claims, next.Claims) && sortedEqual(read.Lapsed, next.Lapsed)
		if same && (read.Enforced || len(next.Claims) == 0) {
			return nil
		}
		if last = m.Cluster.WriteMoveFence(namespace, next); last == nil {
			return nil
		}
		m.sleep(movePoll)
	}
	return fmt.Errorf("updating the claims under move in %s: %w", namespace, last)
}

func sortedEqual(a, b []string) bool {
	a = append([]string(nil), a...)
	sort.Strings(a)
	return strings.Join(a, ",") == strings.Join(b, ",")
}

func strPtr(s string) *string { return &s }

// retain keeps the old volume and its directory when its claim goes.
func (m *StorageMove) retain(rec *MoveRecord) error {
	pv, err := m.Cluster.ReadPV(rec.OldPV)
	if err != nil {
		return err
	}
	if pv == nil || pv.UID != rec.OldPVUID {
		return fmt.Errorf("the local volume %s of %s is gone or was replaced, so kip stops before touching the claim", rec.OldPV, rec.Service)
	}
	if pv.ClaimUID != rec.OldClaimUID {
		return fmt.Errorf("the local volume %s is no longer bound to the claim the move recorded", rec.OldPV)
	}
	return m.Cluster.PatchPV(rec.OldPV, PVPatch{
		ResourceVersion: pv.ResourceVersion,
		Reclaim:         "Retain",
		Labels:          map[string]*string{platform.RetainedLocalCopyLabel: strPtr("true")},
		Annotations: map[string]*string{
			platform.RetainedLocalCopyStateAnnotation:     strPtr(platform.RetainedLocalCopyMoving),
			platform.RetainedLocalCopyServiceAnnotation:   strPtr(rec.Namespace + "/" + rec.Service),
			retainedServiceUIDAnnotation:                  strPtr(rec.ServiceUID),
			retainedClaimUIDAnnotation:                    strPtr(rec.OldClaimUID),
			retainedPathAnnotation:                        strPtr(rec.Path),
			platform.RetainedLocalCopyUsedBytesAnnotation: strPtr(strconv.FormatInt(rec.UsedBytes, 10)),
		},
	})
}

// Annotations a retained local volume carries besides the shared keys in
// the platform package.
const (
	retainedServiceUIDAnnotation = "kipper.run/retained-local-copy-service-uid"
	retainedClaimUIDAnnotation   = "kipper.run/retained-local-copy-claim-uid"
	retainedPathAnnotation       = "kipper.run/retained-local-copy-path"
	// retainedDestinationAnnotation holds the UID of the Longhorn claim the
	// move verified, so prune can tell it from a later replacement.
	retainedDestinationAnnotation = "kipper.run/retained-local-copy-destination-uid"
)

func retainedAnnotationKeys() []string {
	return []string{
		platform.RetainedLocalCopyStateAnnotation,
		platform.RetainedLocalCopyServiceAnnotation,
		platform.RetainedLocalCopyMovedAtAnnotation,
		retainedServiceUIDAnnotation,
		retainedClaimUIDAnnotation,
		retainedPathAnnotation,
		retainedDestinationAnnotation,
		platform.RetainedLocalCopyUsedBytesAnnotation,
	}
}

// release deletes the old claim. Its volume stays, Released, with the data.
// Retain is set again and read back first, because the checkpoint of the
// retain step says nothing about the volume today.
func (m *StorageMove) release(rec *MoveRecord) error {
	if err := m.requireNoPods(rec); err != nil {
		return err
	}
	if err := m.retain(rec); err != nil {
		return err
	}
	pv, err := m.Cluster.ReadPV(rec.OldPV)
	if err != nil {
		return err
	}
	if pv == nil || pv.UID != rec.OldPVUID || pv.Reclaim != "Retain" {
		return fmt.Errorf("the local volume %s of %s is not kept on Retain, so kip stops before releasing its claim", rec.OldPV, rec.Service)
	}
	name := ServiceDataClaim(rec.Service)
	return m.waitFor("the old claim "+name+" to go", 150, func() (bool, error) {
		claim, err := m.Cluster.ReadClaim(rec.Namespace, name)
		if err != nil {
			return false, err
		}
		if claim == nil || claim.UID != rec.OldClaimUID {
			return true, nil
		}
		return false, m.Cluster.DeleteClaim(rec.Namespace, name, rec.OldClaimUID)
	})
}

// isMoveClaim reports whether claim is the Longhorn claim this move created,
// recognized by its annotation, class and planned or grown size when its UID
// was not recorded.
func isMoveClaim(rec *MoveRecord, claim *MoveClaim) bool {
	return claim.Annotations[platform.StorageMoveAnnotation] == StorageMoveRecordName(rec.ServiceUID) &&
		claim.Class == platform.LonghornSingleClass &&
		(sameQuantity(claim.Size, rec.TargetSize) || (rec.ExpandedTo != "" && sameQuantity(claim.Size, rec.ExpandedTo)))
}

// isStrayClaim reports whether claim is one the StatefulSet controller made
// from its local-path template while the old claim was gone. Such a claim
// names no volume and stays Pending, because local-path waits for a pod and
// admission keeps every pod that would mount it away. A claim that names a
// volume may be about to bind to data, so it never counts as a stray.
func isStrayClaim(claim *MoveClaim) bool {
	return claim.Annotations[platform.StorageMoveAnnotation] == "" && claim.Annotations[StorageMoveRollbackAnnotation] == "" &&
		claim.Class == platform.LocalPathClass && claim.Phase == "Pending" && claim.VolumeName == ""
}

func sameQuantity(a, b string) bool {
	qa, errA := resource.ParseQuantity(a)
	qb, errB := resource.ParseQuantity(b)
	return errA == nil && errB == nil && qa.Cmp(qb) == 0
}

// claim creates the Longhorn data claim, or adopts the one an earlier run
// created, and waits until it is bound.
func (m *StorageMove) claim(rec *MoveRecord) error {
	name := ServiceDataClaim(rec.Service)
	existing, err := m.Cluster.ReadClaim(rec.Namespace, name)
	if err != nil {
		return err
	}
	if existing != nil && !ownDestination(rec, existing) {
		if !isStrayClaim(existing) {
			return fmt.Errorf("a claim %s that the move did not create exists (class %s, %s). Remove it or run with --rollback", name, existing.Class, strings.ToLower(existing.Phase))
		}
		if err := m.requireNoPods(rec); err != nil {
			return err
		}
		m.printf("Deleting the empty claim %s the StatefulSet created while the old one was gone.\n", name)
		if err := m.Cluster.DeleteClaim(rec.Namespace, name, existing.UID); err != nil {
			return err
		}
		if err := m.waitGone(rec.Namespace, name, existing.UID); err != nil {
			return err
		}
		existing = nil
	}
	if existing == nil {
		if err := m.Cluster.CreateClaim(MoveClaim{
			Namespace:   rec.Namespace,
			Name:        name,
			Class:       platform.LonghornSingleClass,
			Size:        rec.TargetSize,
			Labels:      moveClaimLabels(rec.OldClaimLabels, rec.Service),
			Annotations: map[string]string{platform.StorageMoveAnnotation: StorageMoveRecordName(rec.ServiceUID)},
		}); err != nil {
			return fmt.Errorf("creating the Longhorn claim %s: %w", name, err)
		}
	}
	var bound *MoveClaim
	if err := m.waitFor("the Longhorn claim "+name+" to bind", 150, func() (bool, error) {
		c, err := m.Cluster.ReadClaim(rec.Namespace, name)
		if err != nil || c == nil {
			return false, err
		}
		if !ownDestination(rec, c) {
			return false, fmt.Errorf("the claim %s changed while kip waited for it to bind", name)
		}
		bound = c
		return c.Phase == "Bound", nil
	}); err != nil {
		return err
	}
	rec.NewClaimUID, rec.NewPV = bound.UID, bound.VolumeName
	if raise, err := smallerThan(rec.OriginalStorage, rec.TargetSize); err != nil {
		return err
	} else if raise {
		if err := m.Cluster.SetServiceStorage(rec.Namespace, rec.Service, rec.TargetSize); err != nil {
			return fmt.Errorf("recording the new size of %s: %w", rec.Service, err)
		}
		m.printf("The data volume of %s is now %s, so its storage setting was raised to match.\n", rec.Service, rec.TargetSize)
	}
	return nil
}

// smallerThan reports whether declared is unset or below target.
func smallerThan(declared, target string) (bool, error) {
	if declared == "" {
		return true, nil
	}
	d, err := resource.ParseQuantity(declared)
	if err != nil {
		return false, fmt.Errorf("reading the storage setting %q: %w", declared, err)
	}
	t, err := resource.ParseQuantity(target)
	if err != nil {
		return false, err
	}
	return d.Cmp(t) < 0, nil
}

func (m *StorageMove) waitGone(namespace, name, uid string) error {
	return m.waitFor("the claim "+name+" to go", 150, func() (bool, error) {
		c, err := m.Cluster.ReadClaim(namespace, name)
		if err != nil {
			return false, err
		}
		return c == nil || c.UID != uid, nil
	})
}

// copyData copies the retained directory into the Longhorn claim through a
// read-only source view, then has a separate pod compare the two trees once
// the Longhorn volume has detached, so the comparison reads what Longhorn
// stored rather than the copy's page cache. The copy pod the record names is
// the move's own: a rerun waits for it or takes its result instead of copying
// again. A finished copy is recorded, so a rerun verifies it again without
// copying. Only a successful verification completes the step.
func (m *StorageMove) copyData(rec *MoveRecord) error {
	if err := m.requireNoPods(rec); err != nil {
		return err
	}
	if err := m.requireDestination(rec); err != nil {
		return err
	}
	pv, err := m.retainedSource(rec)
	if err != nil {
		return err
	}
	if !rec.CopyDone {
		if err := m.copyGrowingOnce(rec, pv); err != nil {
			return err
		}
	}
	if err := m.verify(rec, pv); err != nil {
		if errors.Is(err, errCopyDiffers) {
			return fmt.Errorf("the copy of %s does not match its source when read back from the Longhorn volume, so the service stays stopped with its local copy intact. "+
				"Run 'kip service storage longhorn %s' to copy again, or add --rollback to return to the local volume. Verification found that %w", rec.Service, rec.Service, err)
		}
		return err
	}
	return m.requireDestination(rec)
}

// retainedSource returns the retained local volume the copy reads.
func (m *StorageMove) retainedSource(rec *MoveRecord) (*MovePV, error) {
	pv, err := m.Cluster.ReadPV(rec.OldPV)
	if err != nil {
		return nil, err
	}
	if pv == nil || pv.UID != rec.OldPVUID {
		return nil, fmt.Errorf("the retained local volume %s is gone, so there is nothing to copy. Restore %s from a backup", rec.OldPV, rec.Service)
	}
	return pv, nil
}

// copyGrowingOnce copies, and when the copy runs out of room on a volume the
// move has not grown yet, grows it and copies again. A copy that runs out of
// room on the grown volume fails closed.
func (m *StorageMove) copyGrowingOnce(rec *MoveRecord, pv *MovePV) error {
	if err := m.ensureExpanded(rec); err != nil {
		return err
	}
	err := m.copyOnce(rec, pv)
	var full copyOutOfRoom
	if errors.As(err, &full) && rec.ExpandedTo == "" {
		size := moveExpansionSize(rec.TargetSize, rec.UsedBytes)
		m.printf("The copy of %s ran out of room on its %s Longhorn volume, so kip grows the volume once, to %s, and copies again.\n", rec.Service, rec.TargetSize, size)
		rec.ExpandedTo = size
		if err := m.save(rec); err != nil {
			return fmt.Errorf("recording the larger volume: %w", err)
		}
		if err := m.ensureExpanded(rec); err != nil {
			return err
		}
		err = m.copyOnce(rec, pv)
	}
	if errors.As(err, &full) {
		return fmt.Errorf("the copy of %s ran out of room again on its volume, which kip already grew to %s, so the service stays stopped with its local copy intact. "+
			"Run 'kip service storage longhorn %s --rollback' to return to the local volume. %s", rec.Service, rec.ExpandedTo, rec.Service, full.detail)
	}
	return err
}

// copyOutOfRoom is a copy that failed with No space left on device. detail
// says how the copy pod ended.
type copyOutOfRoom struct {
	error
	detail string
}

// moveExpansionSize is the size a move grows its claim to after the copy
// ran out of room: 125% of the planned size or the used bytes plus 30%,
// whichever is larger, in whole GiB.
func moveExpansionSize(target string, used int64) string {
	const gib = 1 << 30
	planned := resource.MustParse(target)
	want := max(math.Ceil(float64(planned.Value())*1.25/gib), math.Ceil(float64(used)*1.3/gib))
	return strconv.FormatInt(int64(want), 10) + "Gi"
}

// ensureExpanded grows the destination claim to the size the record names,
// once Longhorn's room and the namespace's quotas admit it, and waits until
// the volume shows the new size, either as capacity or as a filesystem resize
// pending until the next mount. The service's storage setting follows.
func (m *StorageMove) ensureExpanded(rec *MoveRecord) error {
	if rec.ExpandedTo == "" {
		return nil
	}
	if err := m.requireDestination(rec); err != nil {
		return err
	}
	name := ServiceDataClaim(rec.Service)
	want, err := resource.ParseQuantity(rec.ExpandedTo)
	if err != nil {
		return fmt.Errorf("reading the recorded size %q: %w", rec.ExpandedTo, err)
	}
	claim, err := m.Cluster.ReadClaim(rec.Namespace, name)
	if err != nil {
		return err
	}
	if claim == nil {
		return m.requireDestination(rec)
	}
	current, err := resource.ParseQuantity(claim.Size)
	if err != nil {
		return fmt.Errorf("reading the size of %s: %w", name, err)
	}
	if current.Cmp(want) < 0 {
		if err := m.checkExpansion(rec, want.Value()-current.Value(), want.Value()); err != nil {
			return fmt.Errorf("the copy of %s needs its Longhorn volume grown to %s, and %w. Run the command again once there is room, or add --rollback to return to the local volume", rec.Service, rec.ExpandedTo, err)
		}
		if err := m.Cluster.ExpandClaim(rec.Namespace, name, rec.ExpandedTo); err != nil {
			return fmt.Errorf("growing %s to %s: %w", name, rec.ExpandedTo, err)
		}
	}
	if err := m.waitFor(name+" to show its new size", 150, func() (bool, error) {
		c, err := m.Cluster.ReadClaim(rec.Namespace, name)
		if err != nil || c == nil {
			return false, err
		}
		if c.ResizePending {
			return true, nil
		}
		capacity, err := resource.ParseQuantity(c.Capacity)
		return err == nil && capacity.Cmp(want) >= 0, nil
	}); err != nil {
		return fmt.Errorf("%w. Run the command again to copy once it has grown", err)
	}
	if raise, err := smallerThan(rec.OriginalStorage, rec.ExpandedTo); err != nil {
		return err
	} else if raise {
		svc, err := m.Cluster.ReadService(rec.Namespace, rec.Service)
		if err != nil {
			return err
		}
		if svc != nil && svc.Storage != rec.ExpandedTo {
			if err := m.Cluster.SetServiceStorage(rec.Namespace, rec.Service, rec.ExpandedTo); err != nil {
				return fmt.Errorf("recording the new size of %s: %w", rec.Service, err)
			}
		}
	}
	return nil
}

// checkExpansion checks Longhorn's room for the added bytes and the dump
// volume at the new size, and the namespace's quotas for the storage the
// service ends up with and the next copy pod. A service with consistent
// backups ends up with a dump claim of the new size on Longhorn in place of
// its local one.
func (m *StorageMove) checkExpansion(rec *MoveRecord, added, size int64) error {
	svc, err := m.Cluster.ReadService(rec.Namespace, rec.Service)
	if err != nil {
		return err
	}
	var dump, localDump int64
	if svc != nil && svc.ConsistentBackups {
		dump = size
		if localDump, err = m.localDumpSize(rec); err != nil {
			return err
		}
	}
	if m.CheckRoom != nil {
		if err := m.CheckRoom(added, dump); err != nil {
			return err
		}
	}
	res, err := m.Cluster.ResourceState(rec.Namespace)
	if err != nil {
		return err
	}
	longhorn := *resource.NewQuantity(added+dump, resource.BinarySI)
	add := corev1.ResourceList{
		corev1.ResourceRequestsStorage: longhorn,
		corev1.ResourceName(platform.LonghornSingleClass + ".storageclass.storage.k8s.io/requests.storage"): longhorn,
	}
	free := corev1.ResourceList{corev1.ResourceRequestsStorage: *resource.NewQuantity(min(localDump, dump), resource.BinarySI)}
	if err := quotaRefusal(res.Quotas, "growing the volume of "+rec.Service, func(q corev1.ResourceQuota) (corev1.ResourceList, corev1.ResourceList, error) {
		covered, err := quotaCovers(q, func(r corev1.ScopedResourceSelectorRequirement) (bool, error) { return claimInScope(r, "") })
		if err != nil || !covered {
			return nil, nil, err
		}
		return add, free, nil
	}); err != nil {
		return err
	}
	return moveQuotaRefusal(res, rec.Namespace, rec.Service)
}

// localDumpSize returns the recorded local dump claim's requested size in bytes.
// It returns zero for an absent or replaced claim, or an invalid size.
func (m *StorageMove) localDumpSize(rec *MoveRecord) (int64, error) {
	if rec.DumpClaim == "" {
		return 0, nil
	}
	claim, err := m.Cluster.ReadClaim(rec.Namespace, rec.DumpClaim)
	if err != nil || claim == nil || claim.UID != rec.DumpClaimUID {
		return 0, err
	}
	if size, err := resource.ParseQuantity(claim.Size); err == nil {
		return size.Value(), nil
	}
	return 0, nil
}

// copyOnce runs or adopts the copy pod, records a successful copy and
// deletes the pod.
func (m *StorageMove) copyOnce(rec *MoveRecord, pv *MovePV) error {
	pod := CopyPod{Namespace: rec.Namespace, Name: moveCopyPodName(rec.Service), Source: moveSourceClaim(rec.Service), Target: ServiceDataClaim(rec.Service)}
	own, err := m.ownCopy(rec)
	if err != nil {
		return err
	}
	if !own {
		if err := m.startCopy(rec, pv, pod); err != nil {
			return err
		}
	} else {
		m.printf("Waiting for the copy kip started earlier to finish...\n")
	}
	var result CopyResult
	if err := m.waitFor("the copy to finish", moveCopyPolls, func() (bool, error) {
		r, err := m.Cluster.CopyResult(pod.Namespace, pod.Name)
		result = r
		return r.Finished, err
	}); err != nil {
		return err
	}
	if !result.Succeeded {
		if err := m.removeCopyPod(rec); err != nil {
			return err
		}
		detail := copyFailure(result, "copy")
		err := fmt.Errorf("the copy of %s failed, so the service stays stopped with its local copy intact. "+
			"Run 'kip service storage longhorn %s' to copy again, or add --rollback to return to the local volume. %s",
			rec.Service, rec.Service, detail)
		if strings.Contains(result.Log, "No space left on device") {
			return copyOutOfRoom{err, detail}
		}
		return err
	}
	rec.CopyDone = true
	if err := m.save(rec); err != nil {
		return fmt.Errorf("recording the finished copy: %w", err)
	}
	return m.deletePod(rec.Namespace, pod.Name)
}

// errCopyDiffers marks a verification that found the copy different from
// its source.
var errCopyDiffers = errors.New("the copy differs from its source")

// verifyDetachWait limits detach polling to 150 attempts at movePoll intervals
// once no pod mounts the Longhorn volume.
const verifyDetachWait = 150

// verify waits for the Longhorn volume to detach, then runs a fresh verify
// pod against a fresh source view. Any earlier verify pod is deleted first,
// because a verification is never reused. A mismatch forgets the copy and
// returns errCopyDiffers. Other failures keep the copy for another verification.
// Completed pods are deleted, and timeouts attempt deletion. Other errors may
// leave a pod for the next run to remove.
func (m *StorageMove) verify(rec *MoveRecord, pv *MovePV) error {
	name := moveVerifyPodName(rec.Service)
	if err := m.deletePod(rec.Namespace, name); err != nil {
		return err
	}
	if err := m.waitFor("the Longhorn volume "+rec.NewPV+" to detach", verifyDetachWait, func() (bool, error) {
		return m.Cluster.VolumeDetached(rec.NewPV)
	}); err != nil {
		return fmt.Errorf("%w, so the copy cannot be read back from a fresh mount yet. Run 'kip service storage longhorn %s' again to verify it", err, rec.Service)
	}
	if err := m.freshSourceView(rec, pv); err != nil {
		return err
	}
	m.printf("Comparing the copy with its source on a fresh mount of the Longhorn volume...\n")
	uid, err := m.Cluster.StartVerify(CopyPod{Namespace: rec.Namespace, Name: name, Source: moveSourceClaim(rec.Service), Target: ServiceDataClaim(rec.Service)})
	if err != nil {
		return fmt.Errorf("starting the verification: %w", err)
	}
	rec.VerifyPodUID = uid
	if err := m.save(rec); err != nil {
		return fmt.Errorf("recording the verify pod: %w", err)
	}
	var result CopyResult
	if err := m.waitFor("the verification to finish", moveCopyPolls, func() (bool, error) {
		r, err := m.Cluster.VerifyResult(rec.Namespace, name)
		result = r
		return r.Finished, err
	}); err != nil {
		if !errors.Is(err, errTimedOut) {
			return err
		}
		if derr := m.deletePod(rec.Namespace, name); derr != nil {
			m.printf("Deleting the verify pod %s failed: %v\n", name, derr)
		}
		return fmt.Errorf("%w. The copy is kept: run 'kip service storage longhorn %s' to verify it again, or add --rollback to return to the local volume", err, rec.Service)
	}
	if err := m.deletePod(rec.Namespace, name); err != nil {
		return err
	}
	rec.VerifyPodUID = ""
	switch {
	case result.Succeeded:
		m.printf("%s\n", verifiedSummary(result.Log))
		return m.save(rec)
	case result.Differs:
		rec.forgetCopy()
		if err := m.save(rec); err != nil {
			return err
		}
		return fmt.Errorf("%w. %s", errCopyDiffers, copyFailure(result, "verification"))
	}
	if err := m.save(rec); err != nil {
		return err
	}
	return fmt.Errorf("the verification of the copy of %s did not finish, so the service stays stopped with its local copy intact. "+
		"Run 'kip service storage longhorn %s' to verify again, or add --rollback to return to the local volume. %s",
		rec.Service, rec.Service, copyFailure(result, "verification"))
}

// ownCopy matches the pod against the recorded UID and accepts an active pod
// or a successfully flushed copy. Otherwise, the next copy replaces the pod and source
// view.
func (m *StorageMove) ownCopy(rec *MoveRecord) (bool, error) {
	if rec.CopyPodUID == "" {
		return false, nil
	}
	name := moveCopyPodName(rec.Service)
	exists, err := m.Cluster.PodExists(rec.Namespace, name)
	if err != nil || !exists {
		return false, err
	}
	r, err := m.Cluster.CopyResult(rec.Namespace, name)
	if err != nil {
		return false, err
	}
	return r.UID == rec.CopyPodUID && (!r.Finished || r.Succeeded), nil
}

// startCopy replaces any copy pod and source view with fresh ones and
// records the new pod's UID.
func (m *StorageMove) startCopy(rec *MoveRecord, pv *MovePV, pod CopyPod) error {
	if err := m.removeCopyPod(rec); err != nil {
		return err
	}
	if err := m.freshSourceView(rec, pv); err != nil {
		return err
	}
	m.printf("Copying %s of data into the Longhorn volume...\n", formatBytes(rec.UsedBytes))
	uid, err := m.Cluster.StartCopy(pod)
	if err != nil {
		return fmt.Errorf("starting the copy: %w", err)
	}
	rec.CopyPodUID = uid
	if err := m.save(rec); err != nil {
		return fmt.Errorf("recording the copy pod: %w", err)
	}
	return nil
}

// freshSourceView replaces the read-only view of the retained directory
// with a new one and waits for its claim to bind. No pod may mount the
// view when it is called.
func (m *StorageMove) freshSourceView(rec *MoveRecord, pv *MovePV) error {
	src := moveSource(rec)
	src.Capacity = pv.Capacity
	if err := m.removeSource(rec); err != nil {
		return err
	}
	if existing, err := m.Cluster.ReadClaim(src.Namespace, src.Claim); err != nil {
		return err
	} else if existing != nil {
		return fmt.Errorf("a claim %s that the move did not create exists in %s. Remove it, then run the command again, or add --rollback", src.Claim, src.Namespace)
	}
	if existing, err := m.Cluster.ReadPV(src.PV); err != nil {
		return err
	} else if existing != nil {
		return fmt.Errorf("a volume %s that the move did not create exists. Remove it, then run the command again, or add --rollback", src.PV)
	}
	if err := m.Cluster.CreateSourceVolume(src); err != nil {
		return fmt.Errorf("creating the read-only view of %s: %w", rec.Path, err)
	}
	return m.waitFor("the source claim "+src.Claim+" to bind", 60, func() (bool, error) {
		c, err := m.Cluster.ReadClaim(src.Namespace, src.Claim)
		return err == nil && c != nil && c.Phase == "Bound", err
	})
}

// copyFailure combines the pod's termination reason, when available, with
// the last 20 log lines or a message indicating an empty log. what names the
// pod's job, such as copy or verification.
func copyFailure(r CopyResult, what string) string {
	var parts []string
	if r.Reason != "" {
		parts = append(parts, "The "+what+" pod ended with "+r.Reason+".")
	}
	if log := strings.TrimSpace(r.Log); log != "" {
		parts = append(parts, "The "+what+" said:\n"+lastLines(log, 20))
	} else {
		parts = append(parts, "The "+what+" printed nothing.")
	}
	return strings.Join(parts, " ")
}

// afterCopy removes the move's helper pods and its source view, then the
// local dump claim.
func (m *StorageMove) afterCopy(rec *MoveRecord) error {
	if err := m.removeCopyPod(rec); err != nil {
		return err
	}
	return m.releaseDump(rec)
}

// requireDestination checks that the data claim is still the Longhorn claim
// the move created and bound, by UID, so a claim replaced between runs is
// never copied onto or started on.
func (m *StorageMove) requireDestination(rec *MoveRecord) error {
	name := ServiceDataClaim(rec.Service)
	c, err := m.Cluster.ReadClaim(rec.Namespace, name)
	if err != nil {
		return err
	}
	if c != nil && c.UID == rec.NewClaimUID && c.VolumeName == rec.NewPV && c.Phase == "Bound" && !c.Deleting {
		return nil
	}
	if stepIndex(rec.Step) >= stepIndex(MoveCommitted) {
		return fmt.Errorf("the claim %s is not the Longhorn claim the move created, so %s stays stopped. The copied data is in volume %s", name, rec.Service, rec.NewPV)
	}
	return fmt.Errorf("the claim %s is not the Longhorn claim the move created, so kip stops before using it. Run the command with --rollback to return to the local volume", name)
}

// removeCopyPod deletes the copy and verify pods and, once they are gone, the
// source view. Only admins can create pods with these names while the fence
// is up, and stop refused any that existed before it, so the pods are
// deleted only once stop has finished. Before that, a pod by either name is
// not kip's.
func (m *StorageMove) removeCopyPod(rec *MoveRecord) error {
	if rec.stopped() || stepIndex(rec.Step) >= stepIndex(MoveStopped) {
		for _, name := range []string{moveCopyPodName(rec.Service), moveVerifyPodName(rec.Service)} {
			if err := m.deletePod(rec.Namespace, name); err != nil {
				return err
			}
		}
	}
	return m.removeSource(rec)
}

// removeSource deletes the source view after checking its path and Retain
// policy, which protects the data when its bound claim is deleted. Matching
// names alone are insufficient to delete either the claim or the volume.
func (m *StorageMove) removeSource(rec *MoveRecord) error {
	src := moveSource(rec)
	pv, err := m.Cluster.ReadPV(src.PV)
	if err != nil {
		return err
	}
	view := pv != nil && pv.Path == src.Path && pv.Reclaim == "Retain"
	claim, err := m.Cluster.ReadClaim(src.Namespace, src.Claim)
	if err != nil {
		return err
	}
	if view && claim != nil && claim.VolumeName == src.PV && (pv.ClaimUID == "" || pv.ClaimUID == claim.UID) {
		if err := m.Cluster.DeleteClaim(src.Namespace, src.Claim, claim.UID); err != nil {
			return fmt.Errorf("deleting the source claim %s: %w", src.Claim, err)
		}
		if err := m.waitGone(src.Namespace, src.Claim, claim.UID); err != nil {
			return err
		}
	}
	if !view {
		return nil
	}
	if err := m.Cluster.DeletePV(src.PV, pv.UID); err != nil {
		return fmt.Errorf("deleting the source volume %s: %w", src.PV, err)
	}
	// The volume lingers until its protection finalizer is removed, and the
	// next copy refuses any volume by that name it finds.
	return m.waitFor("the source volume "+src.PV+" to go", 60, func() (bool, error) {
		p, err := m.Cluster.ReadPV(src.PV)
		return err == nil && (p == nil || p.UID != pv.UID), err
	})
}

// verifiedSummary formats the verify pod's last log line for the operator.
// Call only after verification succeeds. If the entry count cannot be parsed,
// the summary reports success without a count.
func verifiedSummary(log string) string {
	var entries int
	if _, err := fmt.Sscanf(lastLines(log, 1), "kipper-move-verified %d entries", &entries); err != nil {
		return "The copy matches its source."
	}
	return fmt.Sprintf("The copy matches its source: %d files and directories compared.", entries)
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func formatBytes(b int64) string {
	const mib = 1 << 20
	if b < 1<<30 {
		return fmt.Sprintf("%.0f MiB", math.Ceil(float64(b)/mib))
	}
	return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
}

// releaseDump deletes the local dump claim the move recorded. The controller
// recreates it on Longhorn once the service is unmarked. A claim that took
// its name since then is left alone.
func (m *StorageMove) releaseDump(rec *MoveRecord) error {
	if rec.DumpClaim == "" {
		return nil
	}
	claim, err := m.Cluster.ReadClaim(rec.Namespace, rec.DumpClaim)
	if err != nil {
		return err
	}
	if claim == nil {
		return nil
	}
	if claim.UID != rec.DumpClaimUID {
		m.printf("The dump volume %s was replaced during the move, so kip leaves it as it is.\n", rec.DumpClaim)
		return nil
	}
	if err := m.Cluster.DeleteClaim(rec.Namespace, rec.DumpClaim, claim.UID); err != nil {
		return fmt.Errorf("deleting the local dump volume %s: %w", rec.DumpClaim, err)
	}
	return m.waitGone(rec.Namespace, rec.DumpClaim, claim.UID)
}

// commit checks the destination and pod fence before the irreversible
// checkpoint. The subsequent restart directs writes to Longhorn.
func (m *StorageMove) commit(rec *MoveRecord) error {
	if err := m.requireDestination(rec); err != nil {
		return err
	}
	return m.requireNoPods(rec)
}

// finish ends a committed move: the retained copy is marked moved, the
// service started again and unmarked, and the record deleted.
func (m *StorageMove) finish(rec *MoveRecord) error {
	if err := m.requireDestination(rec); err != nil {
		return err
	}
	// The data is on the verified Longhorn claim by now, so a retained copy
	// that is already gone does not keep the service from starting.
	pv, err := m.Cluster.ReadPV(rec.OldPV)
	if err != nil {
		return err
	}
	if pv != nil && pv.UID == rec.OldPVUID {
		if err := m.Cluster.PatchPV(rec.OldPV, PVPatch{ResourceVersion: pv.ResourceVersion, Annotations: map[string]*string{
			platform.RetainedLocalCopyStateAnnotation:   strPtr(platform.RetainedLocalCopyMoved),
			platform.RetainedLocalCopyMovedAtAnnotation: strPtr(m.now().UTC().Format(time.RFC3339)),
			retainedDestinationAnnotation:               strPtr(rec.NewClaimUID),
		}}); err != nil {
			return fmt.Errorf("marking the local copy %s as moved: %w", rec.OldPV, err)
		}
	}
	if err := m.restart(rec); err != nil {
		return err
	}
	if pv == nil || pv.UID != rec.OldPVUID {
		m.printf("Moved %s onto Longhorn. Its old local directory is already gone.\n", rec.Service)
		return nil
	}
	m.printf("Moved %s onto Longhorn. Its old local directory stays until 'kip cluster storage prune' deletes it.\n", rec.Service)
	return nil
}

// restart lifts the admission fence, restores a marked StatefulSet's saved
// replicas, unmarks the service and deletes the record. Keeping the service
// marked through restoration holds off new copy changes until the move releases
// the StatefulSet. Restoration also removes the StatefulSet marker, so an
// unmarked StatefulSet was either restored earlier or never stopped by the move.
// It keeps its current replicas because a copy change may have stopped it since.
// A StatefulSet deleted during the move is left to the controller, which
// recreates it with one pod once the service is unmarked.
func (m *StorageMove) restart(rec *MoveRecord) error {
	if err := m.removeMoveClaims(rec.Namespace, rec.claims()); err != nil {
		return err
	}
	replicas, started, err := m.Cluster.StartStatefulSet(rec.Namespace, rec.Service, rec.restoreCount())
	gone := errors.Is(err, errStatefulSetGone)
	if err != nil && !gone {
		return fmt.Errorf("starting %s: %w", rec.Service, err)
	}
	if err := m.Cluster.MarkService(rec.Namespace, rec.Service, ""); err != nil {
		return fmt.Errorf("unmarking %s: %w", rec.Service, err)
	}
	if err := m.Cluster.DeleteRecord(StorageMoveRecordName(rec.ServiceUID)); err != nil {
		return fmt.Errorf("deleting the move record: %w", err)
	}
	if gone {
		m.printf("The StatefulSet of %s was deleted during the move, so the controller recreates it with one pod.\n", rec.Service)
		if rec.ReplicasSaved && rec.Replicas == 0 {
			m.printf("%s was stopped before the move and will start again when the controller recreates its StatefulSet.\n", rec.Service)
		}
		return nil
	}
	if !started {
		return nil
	}
	if replicas < 1 {
		m.printf("%s was stopped before, so it stays stopped. Start it with 'kip service start %s'.\n", rec.Service, rec.Service)
		return nil
	}
	m.printf("Starting %s...\n", rec.Service)
	var unschedulable string
	if err := m.waitFor(rec.Service+" to be ready", 150, func() (bool, error) {
		ready, reason, err := m.Cluster.ServiceReady(rec.Namespace, rec.Service)
		unschedulable = reason
		return ready, err
	}); err != nil {
		if unschedulable != "" {
			m.printf("Warning: %s is not ready yet. Its data is in place, but %s\n", rec.Service, service.UnschedulableMessage(rec.Service, unschedulable))
			return nil
		}
		m.printf("Warning: %s is not ready yet. Its data is in place. Watch it with 'kip service info %s'.\n", rec.Service, rec.Service)
	}
	return nil
}

func rollbackIndex(step string) int {
	for i, s := range []string{MoveRollbackCleaned, MoveRollbackBound, MoveRollbackDone} {
		if s == step {
			return i
		}
	}
	return -1
}

// rollback returns an unfinished move to the retained local volume. Each step
// decides from the live objects, since a forward step may have half-run.
func (m *StorageMove) rollback(rec *MoveRecord) error {
	m.printf("Rolling back the move of %s to its local volume...\n", rec.Service)
	// Rebinding needs the fence until the restart lifts it, which is all
	// that remains once the rollback is done.
	if rec.stopped() && rollbackIndex(rec.Step) < rollbackIndex(MoveRollbackDone) {
		if err := m.stop(rec); err != nil {
			return err
		}
	}
	if rollbackIndex(rec.Step) < 0 {
		if err := m.rollbackClean(rec); err != nil {
			return err
		}
		if err := m.checkpoint(rec, MoveRollbackCleaned); err != nil {
			return err
		}
	}
	if rollbackIndex(rec.Step) < 1 {
		if err := m.rollbackBind(rec); err != nil {
			return err
		}
		if err := m.checkpoint(rec, MoveRollbackBound); err != nil {
			return err
		}
	}
	if rollbackIndex(rec.Step) < 2 {
		if err := m.rollbackRestore(rec); err != nil {
			return err
		}
		if err := m.checkpoint(rec, MoveRollbackDone); err != nil {
			return err
		}
	}
	if err := m.restart(rec); err != nil {
		return err
	}
	m.printf("Rolled back %s. It runs on its local volume again.\n", rec.Service)
	return nil
}

func boundToRetained(claim *MoveClaim, pv *MovePV) bool {
	return claim != nil && pv != nil && claim.VolumeName == pv.Name && pv.ClaimUID == claim.UID
}

// rollbackClean removes the copy and verify pods and source view, then the move's
// Longhorn claim or a stray one. A claim bound to the retained volume is
// never deleted.
func (m *StorageMove) rollbackClean(rec *MoveRecord) error {
	if err := m.removeCopyPod(rec); err != nil {
		return err
	}
	name := ServiceDataClaim(rec.Service)
	claim, err := m.Cluster.ReadClaim(rec.Namespace, name)
	if err != nil {
		return err
	}
	pv, err := m.Cluster.ReadPV(rec.OldPV)
	if err != nil {
		return err
	}
	if pv == nil || pv.UID != rec.OldPVUID {
		return fmt.Errorf("the retained local volume %s is gone, so there is nothing to roll back to. Restore %s from a backup", rec.OldPV, rec.Service)
	}
	switch {
	case claim == nil, boundToRetained(claim, pv):
		return nil
	case claim.Annotations[StorageMoveRollbackAnnotation] == StorageMoveRecordName(rec.ServiceUID):
		return nil
	case ownDestination(rec, claim), isStrayClaim(claim):
		if err := m.requireNoPods(rec); err != nil {
			return err
		}
		if err := m.Cluster.DeleteClaim(rec.Namespace, name, claim.UID); err != nil {
			return fmt.Errorf("deleting the Longhorn claim %s: %w", name, err)
		}
		return m.waitGone(rec.Namespace, name, claim.UID)
	default:
		return fmt.Errorf("a claim %s that the move did not create exists (class %s), so kip stops before deleting anything", name, claim.Class)
	}
}

// ownDestination reports whether claim is the Longhorn claim this move
// created: by the UID the record holds, or, when a lost checkpoint kept the
// UID out of the record, by the move's annotation, class and size.
func ownDestination(rec *MoveRecord, claim *MoveClaim) bool {
	if rec.NewClaimUID != "" {
		return claim.UID == rec.NewClaimUID
	}
	return isMoveClaim(rec, claim)
}

// rollbackBind binds data-<service>-0 to the retained volume again.
func (m *StorageMove) rollbackBind(rec *MoveRecord) error {
	name := ServiceDataClaim(rec.Service)
	pv, err := m.Cluster.ReadPV(rec.OldPV)
	if err != nil {
		return err
	}
	if pv == nil || pv.UID != rec.OldPVUID {
		return fmt.Errorf("the retained local volume %s is gone", rec.OldPV)
	}
	claim, err := m.Cluster.ReadClaim(rec.Namespace, name)
	if err != nil {
		return err
	}
	if !boundToRetained(claim, pv) {
		if claim == nil {
			if err := m.Cluster.PatchPV(rec.OldPV, PVPatch{ResourceVersion: pv.ResourceVersion, RebindTo: &ClaimRef{Namespace: rec.Namespace, Name: name}}); err != nil {
				return fmt.Errorf("freeing %s for its claim: %w", rec.OldPV, err)
			}
			if err := m.Cluster.CreateClaim(MoveClaim{
				Namespace:   rec.Namespace,
				Name:        name,
				Class:       platform.LocalPathClass,
				VolumeName:  rec.OldPV,
				Size:        pv.Capacity,
				Labels:      moveClaimLabels(rec.OldClaimLabels, rec.Service),
				Annotations: map[string]string{StorageMoveRollbackAnnotation: StorageMoveRecordName(rec.ServiceUID)},
			}); err != nil {
				return fmt.Errorf("creating the claim on %s: %w", rec.OldPV, err)
			}
		} else if claim.Annotations[StorageMoveRollbackAnnotation] != StorageMoveRecordName(rec.ServiceUID) {
			return fmt.Errorf("a claim %s that the rollback did not create exists", name)
		}
		if err := m.waitFor(name+" to bind to "+rec.OldPV, 60, func() (bool, error) {
			c, err := m.Cluster.ReadClaim(rec.Namespace, name)
			if err != nil || c == nil {
				return false, err
			}
			p, err := m.Cluster.ReadPV(rec.OldPV)
			if err != nil {
				return false, err
			}
			if p == nil || p.UID != rec.OldPVUID {
				return false, fmt.Errorf("the local volume %s was replaced while its claim was binding", rec.OldPV)
			}
			claim, pv = c, p
			return boundToRetained(c, p), nil
		}); err != nil {
			return err
		}
	}
	if claim.UID != rec.OldClaimUID {
		rec.RollbackUID = claim.UID
	}
	return nil
}

// rollbackRestore gives the volume back its delete policy and the service
// its original storage setting. Delete hands the volume to the provisioner
// once its claim goes, so the binding is checked again first. A saved
// checkpoint cannot establish the claim's current state.
func (m *StorageMove) rollbackRestore(rec *MoveRecord) error {
	if err := m.rollbackBind(rec); err != nil {
		return err
	}
	pv, err := m.Cluster.ReadPV(rec.OldPV)
	if err != nil {
		return err
	}
	claim, err := m.Cluster.ReadClaim(rec.Namespace, ServiceDataClaim(rec.Service))
	if err != nil {
		return err
	}
	if pv == nil || pv.UID != rec.OldPVUID || !boundToRetained(claim, pv) || claim.Deleting {
		return fmt.Errorf("the local volume %s is no longer bound to the claim of %s, so kip keeps it on Retain. Run the command with --rollback again", rec.OldPV, rec.Service)
	}
	annotations := map[string]*string{}
	for _, k := range retainedAnnotationKeys() {
		annotations[k] = nil
	}
	if err := m.Cluster.PatchPV(rec.OldPV, PVPatch{
		ResourceVersion: pv.ResourceVersion,
		Reclaim:         "Delete",
		Labels:          map[string]*string{platform.RetainedLocalCopyLabel: nil},
		Annotations:     annotations,
	}); err != nil {
		return fmt.Errorf("restoring %s: %w", rec.OldPV, err)
	}
	svc, err := m.Cluster.ReadService(rec.Namespace, rec.Service)
	if err != nil {
		return err
	}
	if svc != nil && svc.Storage != rec.OriginalStorage {
		if err := m.Cluster.SetServiceStorage(rec.Namespace, rec.Service, rec.OriginalStorage); err != nil {
			return fmt.Errorf("restoring the storage setting of %s: %w", rec.Service, err)
		}
	}
	return nil
}
