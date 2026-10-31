package installer

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getkipper/kipper/controller/pkg/backupscope"
	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/kip/internal/service"
)

// KubectlMoveCluster runs a storage move with kubectl on the server, as the
// cluster admin, so the admission policies let its copy pod through.
type KubectlMoveCluster struct {
	Server CommandRunner
}

var _ MoveCluster = KubectlMoveCluster{}

const (
	moveCopyImage = "busybox:1.36"
	// veleroVolumesExcludesAnnotation lists pod volumes Velero's file-system
	// backup skips.
	veleroVolumesExcludesAnnotation = "backup.velero.io/backup-volumes-excludes"
	// moveLapsedAnnotation on a namespace lists claims that were under move
	// while the namespace lacked its move label.
	moveLapsedAnnotation = "kipper.run/storage-move-lapsed"
)

// moveLineBreakCheck refuses a volume holding a name with a line break,
// which find's output cannot carry.
const moveLineBreakCheck = `nl='
'
broken=$(find /src -name "*$nl*")
if [ -n "$broken" ]; then
  echo "a file or directory name in the volume contains a line break, which the copy cannot verify. Rename it, then run the move again"
  exit 1
fi
`

// moveCopyScript copies /src into /dst and flushes the copy to the volume
// with syncfs, which reports write-back errors, so a copy whose data never
// reached the volume fails here. The copy is verified by a separate pod.
const moveCopyScript = `set -eu -o pipefail
` + moveLineBreakCheck + `find /dst -mindepth 1 -maxdepth 1 ! -name lost+found -exec rm -rf {} +
echo "copying the data"
cp -a /src/. /dst/
chown "$(stat -c %u:%g /src)" /dst
chmod "$(stat -c %a /src)" /dst
echo "flushing the copy to the volume"
sync -f /dst
echo "kipper-move-copied"
`

// moveVerifyScript compares every entry of /src with its copy in /dst, both
// mounted read-only. Directories compare by type, mode and owner, other
// entries also by size, symlinks also by target, and regular files also by
// SHA-256. Each pass hands the source's paths to xargs in batches, and both
// sides describe each batch in the same order. Metadata and checksum processes
// handle up to 1000 entries each, while readlink runs once per symlink.
// Results stay in traversal order and memory holds at most one batch per side.
// Equal entry counts then rule out extra entries in the copy. A top-level
// lost+found is ignored on both sides. A difference prints the mismatch
// marker and exits 3. An unreadable source entry, or a batch that reads
// differently from its entries one by one, exits 1.
//
// Symlink targets are read with readlink, whose exit status shows a failed
// read, because BusyBox stat can omit an unreadable target and still succeed.
// A batch passes only when both reads succeed and match. Read failures remain
// distinct from target text. Targets are compared as one string per batch.
// The earlier size comparison preserves target boundaries and distinguishes
// trailing line breaks that command substitution strips.
const moveVerifyScript = `set -eu -o pipefail
` + moveLineBreakCheck + `compare='
set -eu
kind=$1
shift
describe() {
  case $kind in
  d) stat -c "%F:%a:%u:%g %n" "$@" ;;
  o) stat -c "%F:%a:%u:%g:%s %n" "$@" ;;
  l) for q; do readlink "$q" || exit 1; done ;;
  f) sha256sum "$@" ;;
  esac
}
if a=$(cd /src && describe "$@" 2>/dev/null) && b=$(cd /dst && describe "$@" 2>/dev/null) && [ "$a" = "$b" ]; then
  exit 0
fi
for p; do
  a=$(cd /src && describe "$p") || { echo "the source entry $p cannot be read"; exit 255; }
  if ! b=$(cd /dst && describe "$p" 2>/dev/null); then
    echo "kipper-move-mismatch: the copy differs from the source at $p: $a, and the copy has no readable entry there"
    exit 255
  fi
  if [ "$a" != "$b" ]; then
    echo "kipper-move-mismatch: the copy differs from the source at $p: $a, copied as $b"
    exit 255
  fi
done
echo "the entries from $1 could not be compared consistently, so the comparison stopped"
exit 255
'
pass() {
  kind=$1
  shift
  r=$(cd /src && find . -path ./lost+found -prune -o "$@" -print0 | xargs -0 -r -n 1000 sh -c "$compare" compare "$kind") && return 0
  echo "${r:-the comparison stopped before it finished}"
  case $r in kipper-move-mismatch*) exit 3 ;; esac
  exit 1
}
entries() {
  cd "$1"
  find . -path ./lost+found -prune -o -print
}
echo "comparing the copy with the source"
pass d -type d
pass o ! -type d
pass l -type l
pass f -type f
checked=$(($(entries /src | wc -l)))
copied=$(($(entries /dst | wc -l)))
if [ "$checked" -ne "$copied" ]; then
  echo "kipper-move-mismatch: the copy holds $copied entries and the source $checked"
  exit 3
fi
echo "kipper-move-verified $checked entries"
`

// notFound reports whether kubectl said the object does not exist.
func notFound(out string, err error) bool {
	text := out
	if err != nil {
		text += err.Error()
	}
	return strings.Contains(text, "(NotFound)")
}

// getJSON decodes one object into the destination. It returns false with no
// error for a missing object and an error for an unreadable response.
func (k KubectlMoveCluster) getJSON(what, cmd string, into any) (bool, error) {
	out, err := k.Server.Run(cmd + " -o json")
	if err != nil {
		if notFound(out, err) {
			return false, nil
		}
		return false, fmt.Errorf("reading %s: %w", what, err)
	}
	payload := kubectlPayload(out)
	if payload == "" {
		return false, fmt.Errorf("reading %s: kubectl printed no object", what)
	}
	if err := json.Unmarshal([]byte(payload), into); err != nil {
		return false, fmt.Errorf("reading %s: %w", what, err)
	}
	return true, nil
}

func (k KubectlMoveCluster) apply(what string, obj any) error {
	body, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	if out, err := k.Server.RunStdin("kubectl apply -f - >/dev/null", strings.NewReader(string(body))); err != nil {
		return fmt.Errorf("writing %s: %w: %s", what, err, strings.TrimSpace(out))
	}
	return nil
}

func (k KubectlMoveCluster) create(what string, obj any) error {
	body, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	if out, err := k.Server.RunStdin("kubectl create -f - >/dev/null", strings.NewReader(string(body))); err != nil {
		return fmt.Errorf("creating %s: %w: %s", what, err, strings.TrimSpace(out))
	}
	return nil
}

func (k KubectlMoveCluster) mergePatch(what, target string, patch any) error {
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	if out, err := k.Server.RunStdin(fmt.Sprintf("kubectl %s --type merge --patch-file /dev/stdin >/dev/null", target), strings.NewReader(string(body))); err != nil {
		return fmt.Errorf("updating %s: %w: %s", what, err, strings.TrimSpace(out))
	}
	return nil
}

func (k KubectlMoveCluster) StorageMode() (string, error) { return ReadStorageMode(k.Server) }

func (k KubectlMoveCluster) ServerNode() (string, error) {
	out, err := k.Server.Run("hostname")
	if err != nil {
		return "", fmt.Errorf("reading the server's node name: %w", err)
	}
	return strings.ToLower(kubectlValue(out)), nil
}

var movePathPayload = regexp.MustCompile(`kipper-path\[(yes|no)\]`)

func (k KubectlMoveCluster) PathExists(path string) (bool, error) {
	out, err := k.Server.Run(fmt.Sprintf("if [ -d %s ]; then echo 'kipper-path[yes]'; else echo 'kipper-path[no]'; fi", shellQuote(path)))
	if err != nil {
		return false, fmt.Errorf("checking %s: %w", path, err)
	}
	m := movePathPayload.FindStringSubmatch(out)
	if m == nil {
		return false, fmt.Errorf("checking %s: unexpected output %q", path, strings.TrimSpace(out))
	}
	return m[1] == "yes", nil
}

var moveUsedPayload = regexp.MustCompile(`kipper-used\[(\d+) (\d+)\]`)

// UsedBytes returns the larger of the allocated and apparent size, since
// the copy fills sparse files. Hard-linked files count once per name,
// because the copy writes each name as its own file.
func (k KubectlMoveCluster) UsedBytes(path string) (int64, error) {
	q := shellQuote(path)
	out, err := k.Server.Run(fmt.Sprintf(`a=$(du -s -l -B1 -- %s) && b=$(du -s -l -B1 --apparent-size -- %s) && printf 'kipper-used[%%s %%s]\n' "${a%%%%[[:space:]]*}" "${b%%%%[[:space:]]*}"`, q, q))
	if err != nil {
		return 0, fmt.Errorf("measuring %s: %w", path, err)
	}
	m := moveUsedPayload.FindStringSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("measuring %s: unexpected output %q", path, strings.TrimSpace(out))
	}
	a, _ := strconv.ParseInt(m[1], 10, 64)
	b, _ := strconv.ParseInt(m[2], 10, 64)
	if b > a {
		a = b
	}
	return a, nil
}

var moveEntriesPayload = regexp.MustCompile(`kipper-entries\[\s*(\d+)\]`)

func (k KubectlMoveCluster) CountEntries(path string) (int64, error) {
	out, err := k.Server.Run(fmt.Sprintf(`set -o pipefail; n=$(find %s | wc -l) && printf 'kipper-entries[%%s]\n' "$n"`, shellQuote(path)))
	if err != nil {
		return 0, fmt.Errorf("counting the entries in %s: %w", path, err)
	}
	m := moveEntriesPayload.FindStringSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("counting the entries in %s: unexpected output %q", path, strings.TrimSpace(out))
	}
	return strconv.ParseInt(m[1], 10, 64)
}

// veleroTerminal lists backup and restore phases that allow a storage move,
// including deletion of a backup that has already stopped running.
var veleroTerminal = map[string]bool{
	"Completed": true, "PartiallyFailed": true, "Failed": true, "FailedValidation": true, "Deleting": true,
}

type moveList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			IncludedNamespaces []string `json:"includedNamespaces"`
			ExcludedNamespaces []string `json:"excludedNamespaces"`
		} `json:"spec"`
		Status struct {
			Phase               string `json:"phase"`
			CompletionTimestamp string `json:"completionTimestamp"`
		} `json:"status"`
	} `json:"items"`
}

// list reads a list of objects. A resource type the cluster does not have
// reads as an empty list.
func (k KubectlMoveCluster) list(what, cmd string) (moveList, error) {
	var l moveList
	out, err := k.Server.Run(cmd + " -o json 2>&1")
	if err != nil {
		if identityAbsentFromKubectl(out) {
			return l, nil
		}
		return l, fmt.Errorf("listing %s: %w", what, err)
	}
	if err := json.Unmarshal([]byte(kubectlPayload(out)), &l); err != nil {
		return l, fmt.Errorf("reading %s: %w", what, err)
	}
	return l, nil
}

func (k KubectlMoveCluster) BackupState(namespace string) (MoveBackupState, error) {
	var state MoveBackupState
	out, err := k.Server.Run("kubectl get platformconfig platform --ignore-not-found -o jsonpath='{\"backup[\"}{.spec.backup}{\"]\"}'")
	if err != nil {
		return state, fmt.Errorf("reading the backup setting: %w", err)
	}
	backup := ""
	if m := regexp.MustCompile(`backup\[([^\]]*)\]`).FindStringSubmatch(out); m != nil {
		backup = m[1]
	}
	state.Configured = platform.BackupsConfigured(backup)
	backups, err := k.list("backups", "kubectl -n velero get backups.velero.io")
	if err != nil {
		return state, err
	}
	for _, b := range backups.Items {
		if !veleroTerminal[b.Status.Phase] {
			state.Running = append(state.Running, "backup/"+b.Metadata.Name)
			continue
		}
		if b.Status.Phase != "Completed" || !backupscope.Covers(b.Spec.IncludedNamespaces, b.Spec.ExcludedNamespaces, namespace) {
			continue
		}
		if t, err := time.Parse(time.RFC3339, b.Status.CompletionTimestamp); err == nil && t.After(state.LatestCompleted) {
			state.LatestCompleted = t
		}
	}
	restores, err := k.list("restores", "kubectl -n velero get restores.velero.io")
	if err != nil {
		return state, err
	}
	for _, r := range restores.Items {
		if !veleroTerminal[r.Status.Phase] {
			state.Running = append(state.Running, "restore/"+r.Metadata.Name)
		}
	}
	transfers, err := k.list("transfers", "kubectl -n "+shellQuote(namespace)+" get datatransfers.kipper.run")
	if err != nil {
		return state, err
	}
	for _, d := range transfers.Items {
		if d.Status.Phase != "Completed" && d.Status.Phase != "Failed" {
			state.Transfers = append(state.Transfers, d.Metadata.Name)
		}
	}
	return state, nil
}

func (k KubectlMoveCluster) ReadRecord(name string) (*MoveRecord, error) {
	var cm corev1.ConfigMap
	found, err := k.getJSON("the move record", fmt.Sprintf("kubectl -n %s get configmap %s", StorageMoveRecordNamespace, shellQuote(name)), &cm)
	if err != nil || !found {
		return nil, err
	}
	var rec MoveRecord
	if err := json.Unmarshal([]byte(cm.Data["record"]), &rec); err != nil {
		return nil, fmt.Errorf("reading the move record %s: %w", name, err)
	}
	return &rec, nil
}

func (k KubectlMoveCluster) SaveRecord(name string, rec MoveRecord) error {
	body, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return k.apply("the move record", corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: StorageMoveRecordNamespace, Labels: map[string]string{
			"app.kubernetes.io/managed-by":      "kipper",
			"kipper.run/storage-move-service":   rec.Service,
			"kipper.run/storage-move-namespace": rec.Namespace,
		}},
		Data: map[string]string{"record": string(body)},
	})
}

func (k KubectlMoveCluster) DeleteRecord(name string) error {
	if _, err := k.Server.Run(fmt.Sprintf("kubectl -n %s delete configmap %s --ignore-not-found >/dev/null", StorageMoveRecordNamespace, shellQuote(name))); err != nil {
		return err
	}
	return nil
}

func serviceTarget(namespace, name string) string {
	return fmt.Sprintf("-n %s services.kipper.run %s", shellQuote(namespace), shellQuote(name))
}

func (k KubectlMoveCluster) ReadService(namespace, name string) (*MoveService, error) {
	var svc struct {
		Metadata struct {
			UID         string            `json:"uid"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Spec struct {
			Storage           string `json:"storage"`
			ConsistentBackups bool   `json:"consistentBackups"`
		} `json:"spec"`
		Status struct {
			Conditions []metav1.Condition `json:"conditions"`
		} `json:"status"`
	}
	found, err := k.getJSON("service "+name, fmt.Sprintf("kubectl -n %s get services.kipper.run %s", shellQuote(namespace), shellQuote(name)), &svc)
	if err != nil || !found {
		return nil, err
	}
	return &MoveService{
		UID:               svc.Metadata.UID,
		Storage:           svc.Spec.Storage,
		Mark:              svc.Metadata.Annotations[platform.StorageMoveAnnotation],
		ConsistentBackups: svc.Spec.ConsistentBackups,
		CopiesChange:      copiesChange(svc.Status.Conditions),
	}, nil
}

// copiesChange returns the service's StorageCopies reason if CopiesChangeReason
// recognises it as an unfinished change, or "" otherwise.
func copiesChange(conditions []metav1.Condition) string {
	c := meta.FindStatusCondition(conditions, "StorageCopies")
	if c == nil {
		return ""
	}
	return CopiesChangeReason(c.Reason)
}

// CopiesChangeReason returns reason for Converting, ConversionPending or
// Blocked, which indicate an unfinished copy change. It returns "" for all
// other reasons, including an empty or unrecognised reason.
func CopiesChangeReason(reason string) string {
	switch reason {
	case "Converting", "ConversionPending", "Blocked":
		return reason
	}
	return ""
}

func (k KubectlMoveCluster) MarkService(namespace, name, mark string) error {
	var value any
	if mark != "" {
		value = mark
	}
	return k.mergePatch("service "+name, "patch "+serviceTarget(namespace, name), map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{platform.StorageMoveAnnotation: value}},
	})
}

func (k KubectlMoveCluster) SetServiceStorage(namespace, name, size string) error {
	var value any
	if size != "" {
		value = size
	}
	return k.mergePatch("service "+name, "patch "+serviceTarget(namespace, name), map[string]any{
		"spec": map[string]any{"storage": value},
	})
}

func (k KubectlMoveCluster) readStatefulSet(namespace, name string) (*appsv1.StatefulSet, error) {
	var sts appsv1.StatefulSet
	found, err := k.getJSON("StatefulSet "+name, fmt.Sprintf("kubectl -n %s get statefulset %s", shellQuote(namespace), shellQuote(name)), &sts)
	if err != nil || !found {
		return nil, err
	}
	return &sts, nil
}

func (k KubectlMoveCluster) ReadStatefulSet(namespace, name string) (*MoveStatefulSet, error) {
	sts, err := k.readStatefulSet(namespace, name)
	if err != nil || sts == nil {
		return nil, err
	}
	_, fenced := sts.Annotations[FenceStopAnnotation]
	out := &MoveStatefulSet{FenceStopped: fenced, Mark: sts.Annotations[platform.StorageMoveAnnotation], Replicas: 1}
	if out.Mark != "" {
		if out.Replicas, err = savedReplicas(sts); err != nil {
			return nil, err
		}
	} else if sts.Spec.Replicas != nil {
		out.Replicas = *sts.Spec.Replicas
	}
	return out, nil
}

func (k KubectlMoveCluster) StopStatefulSet(namespace, name, mark string, save int32) (int32, error) {
	sts, err := k.readStatefulSet(namespace, name)
	if err != nil {
		return 0, err
	}
	if sts == nil {
		return 0, fmt.Errorf("%w: %s", errStatefulSetGone, name)
	}
	current := int32(1)
	if sts.Spec.Replicas != nil {
		current = *sts.Spec.Replicas
	}
	if sts.Annotations[platform.StorageMoveAnnotation] != "" {
		saved := save
		if saved < 0 {
			if saved, err = savedReplicas(sts); err != nil {
				return 0, err
			}
		}
		if current == 0 {
			return saved, nil
		}
		return saved, patchStatefulSet(k.Server, namespace, name, map[string]any{
			"metadata": map[string]any{"resourceVersion": sts.ResourceVersion},
			"spec":     map[string]any{"replicas": 0},
		})
	}
	replicas := current
	if save >= 0 {
		replicas = save
	}
	return replicas, patchStatefulSet(k.Server, namespace, name, map[string]any{
		"metadata": map[string]any{"resourceVersion": sts.ResourceVersion, "annotations": map[string]string{
			platform.StorageMoveAnnotation:         mark,
			platform.StorageMoveReplicasAnnotation: strconv.Itoa(int(replicas)),
		}},
		"spec": map[string]any{"replicas": 0},
	})
}

func savedReplicas(sts *appsv1.StatefulSet) (int32, error) {
	n, err := strconv.ParseInt(sts.Annotations[platform.StorageMoveReplicasAnnotation], 10, 32)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("reading the replicas saved on %s: %q", sts.Name, sts.Annotations[platform.StorageMoveReplicasAnnotation])
	}
	return int32(n), nil
}

func (k KubectlMoveCluster) StartStatefulSet(namespace, name string, replicas int32) (int32, bool, error) {
	sts, err := k.readStatefulSet(namespace, name)
	if err != nil {
		return 0, false, err
	}
	if sts == nil {
		return 0, false, fmt.Errorf("%w: %s", errStatefulSetGone, name)
	}
	// Checking the marker and guarding the patch with the same resourceVersion
	// preserves the replica count if another run has already released the StatefulSet.
	if sts.Annotations[platform.StorageMoveAnnotation] == "" {
		if sts.Spec.Replicas == nil {
			return 1, false, nil
		}
		return *sts.Spec.Replicas, false, nil
	}
	if replicas < 0 {
		if replicas, err = savedReplicas(sts); err != nil {
			return 0, false, err
		}
	}
	return replicas, true, patchStatefulSet(k.Server, namespace, name, map[string]any{
		"metadata": map[string]any{"resourceVersion": sts.ResourceVersion, "annotations": map[string]any{
			platform.StorageMoveAnnotation:         nil,
			platform.StorageMoveReplicasAnnotation: nil,
		}},
		"spec": map[string]any{"replicas": replicas},
	})
}

func (k KubectlMoveCluster) ServiceReady(namespace, name string) (bool, string, error) {
	var pod corev1.Pod
	found, err := k.getJSON("pod "+name+"-0", fmt.Sprintf("kubectl -n %s get pod %s", shellQuote(namespace), shellQuote(name+"-0")), &pod)
	if err != nil || !found {
		return false, "", err
	}
	if pod.Status.Phase != corev1.PodRunning {
		return false, service.UnschedulableReason(&pod), nil
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue, "", nil
		}
	}
	return false, "", nil
}

func (k KubectlMoveCluster) ReadClaim(namespace, name string) (*MoveClaim, error) {
	var pvc corev1.PersistentVolumeClaim
	found, err := k.getJSON("claim "+name, fmt.Sprintf("kubectl -n %s get pvc %s", shellQuote(namespace), shellQuote(name)), &pvc)
	if err != nil || !found {
		return nil, err
	}
	return moveClaim(&pvc), nil
}

func moveClaim(pvc *corev1.PersistentVolumeClaim) *MoveClaim {
	c := &MoveClaim{
		Namespace:   pvc.Namespace,
		Name:        pvc.Name,
		UID:         string(pvc.UID),
		Phase:       string(pvc.Status.Phase),
		VolumeName:  pvc.Spec.VolumeName,
		Labels:      pvc.Labels,
		Annotations: pvc.Annotations,
		Deleting:    pvc.DeletionTimestamp != nil,
	}
	if pvc.Spec.StorageClassName != nil {
		c.Class = *pvc.Spec.StorageClassName
	}
	if q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
		c.Size = q.String()
	}
	if q, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
		c.Capacity = q.String()
	}
	for _, cond := range pvc.Status.Conditions {
		if cond.Type == corev1.PersistentVolumeClaimFileSystemResizePending && cond.Status == corev1.ConditionTrue {
			c.ResizePending = true
		}
	}
	return c
}

func (k KubectlMoveCluster) CreateClaim(c MoveClaim) error {
	size, err := resource.ParseQuantity(c.Size)
	if err != nil {
		return fmt.Errorf("the size %q: %w", c.Size, err)
	}
	class := c.Class
	return k.create("claim "+c.Name, corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{Name: c.Name, Namespace: c.Namespace, Labels: c.Labels, Annotations: c.Annotations},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &class,
			VolumeName:       c.VolumeName,
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
		},
	})
}

// deleteWithUID deletes an object only while its UID is uid. A missing
// object counts as deleted.
func (k KubectlMoveCluster) deleteWithUID(what, path, uid string) error {
	body := fmt.Sprintf(`{"kind":"DeleteOptions","apiVersion":"v1","preconditions":{"uid":%q}}`, uid)
	out, err := k.Server.RunStdin(fmt.Sprintf("kubectl delete --raw %s -f - >/dev/null", shellQuote(path)), strings.NewReader(body))
	if err != nil && !notFound(out, err) {
		return fmt.Errorf("deleting %s: %w: %s", what, err, strings.TrimSpace(out))
	}
	return nil
}

func (k KubectlMoveCluster) DeleteClaim(namespace, name, uid string) error {
	return k.deleteWithUID("claim "+name, fmt.Sprintf("/api/v1/namespaces/%s/persistentvolumeclaims/%s", namespace, name), uid)
}

func (k KubectlMoveCluster) DeletePV(name, uid string) error {
	return k.deleteWithUID("volume "+name, "/api/v1/persistentvolumes/"+name, uid)
}

func (k KubectlMoveCluster) ReadPV(name string) (*MovePV, error) {
	var pv corev1.PersistentVolume
	found, err := k.getJSON("volume "+name, "kubectl get pv "+shellQuote(name), &pv)
	if err != nil || !found {
		return nil, err
	}
	return movePV(&pv), nil
}

func (k KubectlMoveCluster) RetainedVolume(serviceUID string) (*MovePV, error) {
	var pvs corev1.PersistentVolumeList
	if err := kubectlJSON(k.Server, "the kept local volumes", "kubectl get pv -l "+platform.RetainedLocalCopyLabel+"=true -o json", &pvs); err != nil {
		return nil, err
	}
	var found *MovePV
	for i := range pvs.Items {
		pv := movePV(&pvs.Items[i])
		if pv.Annotations[retainedServiceUIDAnnotation] != serviceUID || pv.Annotations[platform.RetainedLocalCopyStateAnnotation] != platform.RetainedLocalCopyMoving {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("the local volumes %s and %s both belong to an unfinished move of the same service", found.Name, pv.Name)
		}
		found = pv
	}
	return found, nil
}

func movePV(pv *corev1.PersistentVolume) *MovePV {
	out := &MovePV{
		Name:            pv.Name,
		UID:             string(pv.UID),
		Class:           pv.Spec.StorageClassName,
		Phase:           string(pv.Status.Phase),
		Reclaim:         string(pv.Spec.PersistentVolumeReclaimPolicy),
		Labels:          pv.Labels,
		Annotations:     pv.Annotations,
		ResourceVersion: pv.ResourceVersion,
	}
	if q, ok := pv.Spec.Capacity[corev1.ResourceStorage]; ok {
		out.Capacity = q.String()
	}
	switch {
	case pv.Spec.Local != nil:
		out.Path = pv.Spec.Local.Path
	case pv.Spec.HostPath != nil:
		out.Path = pv.Spec.HostPath.Path
	}
	out.Node = pvNode(pv)
	if ref := pv.Spec.ClaimRef; ref != nil {
		out.ClaimNamespace, out.ClaimName, out.ClaimUID = ref.Namespace, ref.Name, string(ref.UID)
	}
	return out
}

// pvNode returns the single hostname a volume's node affinity names, or ""
// when it names none or several.
func pvNode(pv *corev1.PersistentVolume) string {
	if pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil {
		return ""
	}
	var nodes []string
	for _, term := range pv.Spec.NodeAffinity.Required.NodeSelectorTerms {
		for _, e := range term.MatchExpressions {
			if e.Key == corev1.LabelHostname && e.Operator == corev1.NodeSelectorOpIn {
				nodes = append(nodes, e.Values...)
			}
		}
	}
	if len(nodes) != 1 {
		return ""
	}
	return nodes[0]
}

func (k KubectlMoveCluster) PatchPV(name string, p PVPatch) error {
	metadata := map[string]any{}
	if p.ResourceVersion != "" {
		metadata["resourceVersion"] = p.ResourceVersion
	}
	if len(p.Labels) > 0 {
		metadata["labels"] = p.Labels
	}
	if len(p.Annotations) > 0 {
		metadata["annotations"] = p.Annotations
	}
	spec := map[string]any{}
	if p.Reclaim != "" {
		spec["persistentVolumeReclaimPolicy"] = p.Reclaim
	}
	if p.RebindTo != nil {
		spec["claimRef"] = map[string]any{"namespace": p.RebindTo.Namespace, "name": p.RebindTo.Name, "uid": nil, "resourceVersion": nil}
	}
	return k.mergePatch("volume "+name, "patch pv "+shellQuote(name), map[string]any{"metadata": metadata, "spec": spec})
}

func (k KubectlMoveCluster) ReadMoveFence(namespace string) (MoveFence, error) {
	var ns corev1.Namespace
	found, err := k.getJSON("namespace "+namespace, "kubectl get namespace "+shellQuote(namespace), &ns)
	if err != nil {
		return MoveFence{}, err
	}
	if !found {
		return MoveFence{}, fmt.Errorf("namespace %s not found", namespace)
	}
	return MoveFence{
		Claims:   splitList(ns.Annotations[platform.StorageMoveClaimsAnnotation]),
		Enforced: ns.Labels[platform.StorageMoveNamespaceLabel] == "true",
		Lapsed:   splitList(ns.Annotations[moveLapsedAnnotation]),
		Version:  ns.ResourceVersion,
	}, nil
}

func splitList(value string) []string {
	var out []string
	for _, c := range strings.Split(value, ",") {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}

func (k KubectlMoveCluster) WriteMoveFence(namespace string, fence MoveFence) error {
	var label, claims, lapsed any
	if len(fence.Claims) > 0 {
		label, claims = "true", joinSorted(fence.Claims)
		if len(fence.Lapsed) > 0 {
			lapsed = joinSorted(fence.Lapsed)
		}
	}
	return k.mergePatch("namespace "+namespace, "patch namespace "+shellQuote(namespace), map[string]any{
		"metadata": map[string]any{
			"resourceVersion": fence.Version,
			"labels":          map[string]any{platform.StorageMoveNamespaceLabel: label},
			"annotations": map[string]any{
				platform.StorageMoveClaimsAnnotation: claims,
				moveLapsedAnnotation:                 lapsed,
			},
		},
	})
}

func joinSorted(list []string) string {
	sorted := append([]string(nil), list...)
	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}

func movePodResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("25m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
	}
}

// copyPodResources allows room for the page cache used by cp. Dirty pages
// awaiting writes to Longhorn count towards container memory, so a small
// limit can cause a large copy to be killed.
func copyPodResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("25m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("1Gi")},
	}
}

func claimVolume(name, claim string, readOnly bool) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim, ReadOnly: readOnly},
	}}
}

// policyDenial matches how the API server words a request the named policy
// denied. A Warn binding names the policy too, but only in a warning on a
// request that succeeds.
func policyDenial(policy string) *regexp.Regexp {
	return regexp.MustCompile(`ValidatingAdmissionPolicy '` + regexp.QuoteMeta(policy) + `' with binding '[^']*' denied request`)
}

var (
	moveDenial      = policyDenial(platform.StorageMovePodsPolicy)
	namespaceDenial = policyDenial(platform.StorageMoveNamespacesPolicy)
)

// requireEnforcingBinding checks that the binding named after the policy
// targets that policy with the Deny action, so a refused probe proves enforcement.
func (k KubectlMoveCluster) requireEnforcingBinding(name string) error {
	var binding struct {
		Spec struct {
			PolicyName        string   `json:"policyName"`
			ValidationActions []string `json:"validationActions"`
		} `json:"spec"`
	}
	found, err := k.getJSON("the admission binding "+name, "kubectl get validatingadmissionpolicybinding "+name, &binding)
	if err != nil {
		return err
	}
	switch {
	case !found:
		return fmt.Errorf("the admission binding %s is missing. Run 'kip upgrade' to install it", name)
	case binding.Spec.PolicyName != name || !containsString(binding.Spec.ValidationActions, "Deny"):
		return fmt.Errorf("the admission binding %s does not deny requests for its policy (policy %q, actions %v). Run 'kip upgrade' to restore it", name, binding.Spec.PolicyName, binding.Spec.ValidationActions)
	}
	return nil
}

func (k KubectlMoveCluster) AdmissionRefuses(namespace, claim string) (bool, error) {
	if err := k.requireEnforcingBinding(platform.StorageMovePodsPolicy); err != nil {
		return false, err
	}
	pod := corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-move-probe", Namespace: namespace},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "probe", Image: moveCopyImage, Command: []string{"true"}, Resources: movePodResources(),
				VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}}},
			Volumes: []corev1.Volume{claimVolume("data", claim, true)},
		},
	}
	body, err := json.Marshal(pod)
	if err != nil {
		return false, err
	}
	out, err := k.Server.RunStdin("kubectl create --dry-run=server -f - 2>&1", strings.NewReader(string(body)))
	switch {
	case err == nil:
		return false, nil
	case moveDenial.MatchString(out):
		return true, nil
	default:
		return false, fmt.Errorf("probing admission in %s: %w: %s", namespace, err, strings.TrimSpace(out))
	}
}

func (k KubectlMoveCluster) NamespaceDeleteRefused(namespace string) (bool, error) {
	if err := k.requireEnforcingBinding(platform.StorageMoveNamespacesPolicy); err != nil {
		return false, err
	}
	out, err := k.Server.Run(fmt.Sprintf("kubectl delete namespace %s --dry-run=server 2>&1", shellQuote(namespace)))
	switch {
	case err == nil:
		return false, nil
	case namespaceDenial.MatchString(out):
		return true, nil
	default:
		return false, fmt.Errorf("probing whether namespace %s can be deleted: %w: %s", namespace, err, strings.TrimSpace(out))
	}
}

// NamespaceTerminating checks namespace deletion, then deleting Projects
// with a matching namespace UID claim or a name in the legacy status list.
// An absent namespace or a failed read returns an error.
func (k KubectlMoveCluster) NamespaceTerminating(namespace string) (NamespaceDeletion, error) {
	var ns corev1.Namespace
	found, err := k.getJSON("namespace "+namespace, "kubectl get namespace "+shellQuote(namespace), &ns)
	if err != nil {
		return NamespaceDeletion{}, err
	}
	if !found {
		return NamespaceDeletion{}, fmt.Errorf("namespace %s not found", namespace)
	}
	if ns.DeletionTimestamp != nil {
		return NamespaceDeletion{Namespace: true, Why: "namespace " + namespace + " is being deleted"}, nil
	}
	var projects struct {
		Items []struct {
			Metadata struct {
				Name              string       `json:"name"`
				DeletionTimestamp *metav1.Time `json:"deletionTimestamp"`
			} `json:"metadata"`
			Status struct {
				Namespaces      []string `json:"namespaces"`
				NamespaceClaims []struct {
					Name string `json:"name"`
					UID  string `json:"uid"`
				} `json:"namespaceClaims"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := kubectlJSON(k.Server, "the projects", "kubectl get projects.kipper.run -o json", &projects); err != nil {
		return NamespaceDeletion{}, err
	}
	for _, p := range projects.Items {
		if p.Metadata.DeletionTimestamp == nil {
			continue
		}
		holds := containsString(p.Status.Namespaces, namespace)
		for _, c := range p.Status.NamespaceClaims {
			holds = holds || (c.Name == namespace && c.UID == string(ns.UID))
		}
		if holds {
			return NamespaceDeletion{Why: "project " + p.Metadata.Name + ", which holds namespace " + namespace + ", is being deleted"}, nil
		}
	}
	return NamespaceDeletion{}, nil
}

func (k KubectlMoveCluster) ResourceState(namespace string) (MoveResources, error) {
	var nodes corev1.NodeList
	var pods corev1.PodList
	var quotas corev1.ResourceQuotaList
	var classes schedulingv1.PriorityClassList
	for _, l := range []struct {
		what, cmd string
		into      any
	}{
		{"the nodes", "kubectl get nodes -o json", &nodes},
		{"the pods", "kubectl get pods -A -o json", &pods},
		{"the quotas in " + namespace, fmt.Sprintf("kubectl -n %s get resourcequota -o json", shellQuote(namespace)), &quotas},
		{"the priority classes", "kubectl get priorityclasses -o json", &classes},
	} {
		if err := kubectlJSON(k.Server, l.what, l.cmd, l.into); err != nil {
			return MoveResources{}, err
		}
	}
	return MoveResources{Nodes: nodes.Items, Pods: pods.Items, Quotas: quotas.Items, DefaultPriorityClass: defaultPriorityClass(classes.Items)}, nil
}

// defaultPriorityClass returns the class the API server gives a pod that
// names none: the global default with the lowest value, or "" for none.
func defaultPriorityClass(classes []schedulingv1.PriorityClass) string {
	var found *schedulingv1.PriorityClass
	for i := range classes {
		c := &classes[i]
		if c.GlobalDefault && (found == nil || c.Value < found.Value) {
			found = c
		}
	}
	if found == nil {
		return ""
	}
	return found.Name
}

func (k KubectlMoveCluster) ClaimsLabeled(namespace, selector string) ([]MoveClaim, error) {
	var pvcs corev1.PersistentVolumeClaimList
	if err := kubectlJSON(k.Server, "the claims labeled "+selector, fmt.Sprintf("kubectl -n %s get pvc -l %s -o json", shellQuote(namespace), shellQuote(selector)), &pvcs); err != nil {
		return nil, err
	}
	out := make([]MoveClaim, 0, len(pvcs.Items))
	for i := range pvcs.Items {
		out = append(out, *moveClaim(&pvcs.Items[i]))
	}
	return out, nil
}

func (k KubectlMoveCluster) PodsMounting(namespace string, claims []string) ([]MovePod, error) {
	var pods corev1.PodList
	if err := kubectlJSON(k.Server, "the pods in "+namespace, fmt.Sprintf("kubectl -n %s get pods -o json", shellQuote(namespace)), &pods); err != nil {
		return nil, err
	}
	var out []MovePod
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim == nil || !containsString(claims, v.PersistentVolumeClaim.ClaimName) {
				continue
			}
			pod := MovePod{Name: p.Name, UID: string(p.UID)}
			if owner := metav1.GetControllerOf(&p); owner != nil && owner.Kind == "StatefulSet" {
				pod.StatefulSet = owner.Name
			}
			out = append(out, pod)
			break
		}
	}
	return out, nil
}

func (k KubectlMoveCluster) CreateSourceVolume(src SourceVolume) error {
	capacity, err := resource.ParseQuantity(src.Capacity)
	if err != nil {
		return fmt.Errorf("the capacity %q: %w", src.Capacity, err)
	}
	directory := corev1.HostPathDirectory
	empty := ""
	labels := moveObjectLabels()
	pv := corev1.PersistentVolume{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolume"},
		ObjectMeta: metav1.ObjectMeta{Name: src.PV, Labels: labels},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: capacity},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			StorageClassName:              "",
			PersistentVolumeSource:        corev1.PersistentVolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: src.Path, Type: &directory}},
			ClaimRef:                      &corev1.ObjectReference{Namespace: src.Namespace, Name: src.Claim},
			NodeAffinity: &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpIn, Values: []string{src.Node}}},
			}}}},
		},
	}
	claimLabels := moveClaimLabels(labels, src.Service)
	pvc := corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{Name: src.Claim, Namespace: src.Namespace, Labels: claimLabels},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &empty,
			VolumeName:       src.PV,
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: capacity}},
		},
	}
	if err := k.apply("the source volume", pv); err != nil {
		return err
	}
	return k.apply("the source claim", pvc)
}

// moveObjectLabels excludes the move's objects from Velero backups to prevent
// a restore from restarting a copy over the service's live data.
func moveObjectLabels() map[string]string {
	return map[string]string{"app.kubernetes.io/managed-by": "kipper", labels.ExcludeFromBackup: "true"}
}

var moveCreatedUID = regexp.MustCompile(`kipper-uid\[([^\]]+)\]`)

func (k KubectlMoveCluster) StartCopy(c CopyPod) (string, error) {
	return k.startHelper("copy", c, moveCopyScript, copyPodResources(), false)
}

// StartVerify runs the verify pod with the probe pod's resources: it only
// reads, and the clean pages it reads can be reclaimed under its limit.
func (k KubectlMoveCluster) StartVerify(c CopyPod) (string, error) {
	return k.startHelper("verify", c, moveVerifyScript, movePodResources(), true)
}

// startHelper creates a move pod that runs script against the source at /src
// and the target at /dst, and returns its UID.
func (k KubectlMoveCluster) startHelper(container string, c CopyPod, script string, resources corev1.ResourceRequirements, targetReadOnly bool) (string, error) {
	root := int64(0)
	pod := corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: c.Name, Namespace: c.Namespace, Labels: moveObjectLabels(),
			Annotations: map[string]string{veleroVolumesExcludesAnnotation: "src,dst"}},
		Spec: corev1.PodSpec{
			RestartPolicy:   corev1.RestartPolicyNever,
			SecurityContext: &corev1.PodSecurityContext{RunAsUser: &root, RunAsGroup: &root},
			Containers: []corev1.Container{{
				Name: container, Image: moveCopyImage, Command: []string{"sh", "-c", script}, Resources: resources,
				VolumeMounts: []corev1.VolumeMount{{Name: "src", MountPath: "/src", ReadOnly: true}, {Name: "dst", MountPath: "/dst", ReadOnly: targetReadOnly}},
			}},
			Volumes: []corev1.Volume{claimVolume("src", c.Source, true), claimVolume("dst", c.Target, targetReadOnly)},
		},
	}
	body, err := json.Marshal(pod)
	if err != nil {
		return "", err
	}
	out, err := k.Server.RunStdin(`kubectl create -f - -o jsonpath='kipper-uid[{.metadata.uid}]'`, strings.NewReader(string(body)))
	if err != nil {
		return "", fmt.Errorf("creating the %s pod: %w: %s", container, err, strings.TrimSpace(out))
	}
	m := moveCreatedUID.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("creating the %s pod: unexpected output %q", container, strings.TrimSpace(out))
	}
	return m[1], nil
}

func (k KubectlMoveCluster) CopyResult(namespace, name string) (CopyResult, error) {
	return k.helperResult("copy", namespace, name, "kipper-move-copied")
}

func (k KubectlMoveCluster) VerifyResult(namespace, name string) (CopyResult, error) {
	r, err := k.helperResult("verify", namespace, name, "kipper-move-verified")
	r.Differs = r.Finished && !r.Succeeded && strings.Contains(r.Log, "kipper-move-mismatch")
	return r, err
}

// helperResult reads a move pod's phase and, once it has ended, the last 40
// lines of its log. Success needs both a Succeeded pod and marker in the log.
func (k KubectlMoveCluster) helperResult(what, namespace, name, marker string) (CopyResult, error) {
	var pod corev1.Pod
	found, err := k.getJSON("the "+what+" pod", fmt.Sprintf("kubectl -n %s get pod %s", shellQuote(namespace), shellQuote(name)), &pod)
	if err != nil {
		return CopyResult{}, err
	}
	if !found {
		return CopyResult{}, fmt.Errorf("the %s pod %s disappeared", what, name)
	}
	uid := string(pod.UID)
	if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
		return CopyResult{UID: uid}, nil
	}
	log, err := k.Server.Run(fmt.Sprintf("kubectl -n %s logs %s --tail=40 2>&1", shellQuote(namespace), shellQuote(name)))
	if err != nil {
		log = fmt.Sprintf("(the %s log could not be read: %v)", what, err)
	}
	succeeded := pod.Status.Phase == corev1.PodSucceeded && strings.Contains(log, marker)
	return CopyResult{Finished: true, Succeeded: succeeded, UID: uid, Log: log, Reason: copyPodEnding(&pod)}, nil
}

func (k KubectlMoveCluster) VolumeDetached(name string) (bool, error) {
	var volume struct {
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	found, err := k.getJSON("Longhorn volume "+name, "kubectl -n longhorn-system get volumes.longhorn.io "+shellQuote(name), &volume)
	if err != nil {
		return false, err
	}
	if !found {
		return false, fmt.Errorf("the Longhorn volume %s is gone", name)
	}
	return volume.Status.State == "detached", nil
}

func (k KubectlMoveCluster) ExpandClaim(namespace, name, size string) error {
	if _, err := resource.ParseQuantity(size); err != nil {
		return fmt.Errorf("the size %q: %w", size, err)
	}
	return k.mergePatch("claim "+name, fmt.Sprintf("-n %s patch pvc %s", shellQuote(namespace), shellQuote(name)), map[string]any{
		"spec": map[string]any{"resources": map[string]any{"requests": map[string]any{"storage": size}}},
	})
}

// copyPodEnding names how a failed copy pod ended: its container's
// termination reason and exit code, or the pod's own reason for a pod the
// kubelet evicted or never ran.
func copyPodEnding(pod *corev1.Pod) string {
	if pod.Status.Phase != corev1.PodFailed {
		return ""
	}
	for _, c := range pod.Status.ContainerStatuses {
		if t := c.State.Terminated; t != nil && t.Reason != "" {
			return fmt.Sprintf("%s (exit %d)", t.Reason, t.ExitCode)
		}
	}
	switch {
	case pod.Status.Reason != "" && pod.Status.Message != "":
		return pod.Status.Reason + ": " + pod.Status.Message
	case pod.Status.Reason != "":
		return pod.Status.Reason
	}
	return ""
}

func (k KubectlMoveCluster) PodExists(namespace, name string) (bool, error) {
	var pod corev1.Pod
	return k.getJSON("pod "+name, fmt.Sprintf("kubectl -n %s get pod %s", shellQuote(namespace), shellQuote(name)), &pod)
}

func (k KubectlMoveCluster) DeletePod(namespace, name string) (bool, error) {
	if out, err := k.Server.Run(fmt.Sprintf("kubectl -n %s delete pod %s --ignore-not-found --wait=false >/dev/null", shellQuote(namespace), shellQuote(name))); err != nil {
		return false, fmt.Errorf("deleting pod %s: %w: %s", name, err, strings.TrimSpace(out))
	}
	var pod corev1.Pod
	found, err := k.getJSON("pod "+name, fmt.Sprintf("kubectl -n %s get pod %s", shellQuote(namespace), shellQuote(name)), &pod)
	return !found, err
}
