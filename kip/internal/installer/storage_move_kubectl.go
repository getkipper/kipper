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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getkipper/kipper/controller/pkg/backupscope"
	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/platform"
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

// moveCopyScript copies /src into /dst and compares every entry of the
// source with its copy. Directories compare by mode and owner, regular files
// also by size and SHA-256, symlinks by owner and the SHA-256 of their target,
// and anything else by type, mode and owner. The comparison streams from find
// without sorting, so comparison memory stays independent of the file count.
// Equal entry counts then rule out extra entries in the copy. A top-level
// lost+found is ignored on both sides. A name with a line break is refused
// before anything is copied, because find's output cannot carry it.
const moveCopyScript = `set -eu -o pipefail
nl='
'
broken=$(find /src -name "*$nl*")
if [ -n "$broken" ]; then
  echo "a file or directory name in the volume contains a line break, which the copy cannot verify. Rename it, then run the move again"
  exit 1
fi
find /dst -mindepth 1 -maxdepth 1 ! -name lost+found -exec rm -rf {} +
echo "copying the data"
cp -a /src/. /dst/
chown "$(stat -c %u:%g /src)" /dst
chmod "$(stat -c %a /src)" /dst
entry() {
  if [ -L "$1" ]; then
    m=$(stat -c %u:%g "$1") && t=$(readlink "$1" | sha256sum) && printf 'l %s %s\n' "$m" "${t%% *}"
  elif [ -d "$1" ]; then
    m=$(stat -c %a:%u:%g "$1") && printf 'd %s\n' "$m"
  elif [ -f "$1" ]; then
    m=$(stat -c %a:%u:%g:%s "$1") && h=$(sha256sum "$1") && printf 'f %s %s\n' "$m" "${h%% *}"
  else
    m=$(stat -c %F:%a:%u:%g "$1") && printf 'o %s\n' "$m"
  fi
}
entries() {
  cd "$1"
  find . -path ./lost+found -prune -o -print
}
echo "comparing the copy with the source"
checked=$(entries /src | {
  n=0
  while IFS= read -r p; do
    a=$(entry "/src/$p") || { echo "the source entry $p cannot be read" >&2; exit 1; }
    b=$(entry "/dst/$p" 2>/dev/null) || b="missing"
    if [ "$a" != "$b" ]; then
      echo "the copy differs from the source at $p: $a, copied as $b" >&2
      exit 1
    fi
    n=$((n + 1))
  done
  echo "$n"
})
copied=$(($(entries /dst | wc -l)))
if [ "$checked" -ne "$copied" ]; then
  echo "the copy holds $copied entries and the source $checked"
  exit 1
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
	}, nil
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
		return 0, fmt.Errorf("the StatefulSet %s is gone", name)
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

func (k KubectlMoveCluster) StartStatefulSet(namespace, name string, replicas int32) (int32, error) {
	sts, err := k.readStatefulSet(namespace, name)
	if err != nil {
		return 0, err
	}
	if sts == nil {
		return 0, fmt.Errorf("the StatefulSet %s is gone", name)
	}
	if replicas < 0 {
		if sts.Annotations[platform.StorageMoveAnnotation] == "" {
			if sts.Spec.Replicas == nil {
				return 1, nil
			}
			return *sts.Spec.Replicas, nil
		}
		if replicas, err = savedReplicas(sts); err != nil {
			return 0, err
		}
	}
	return replicas, patchStatefulSet(k.Server, namespace, name, map[string]any{
		"metadata": map[string]any{"resourceVersion": sts.ResourceVersion, "annotations": map[string]any{
			platform.StorageMoveAnnotation:         nil,
			platform.StorageMoveReplicasAnnotation: nil,
		}},
		"spec": map[string]any{"replicas": replicas},
	})
}

func (k KubectlMoveCluster) ServiceReady(namespace, name string) (bool, error) {
	var pod corev1.Pod
	found, err := k.getJSON("pod "+name+"-0", fmt.Sprintf("kubectl -n %s get pod %s", shellQuote(namespace), shellQuote(name+"-0")), &pod)
	if err != nil || !found {
		return false, err
	}
	if pod.Status.Phase != corev1.PodRunning {
		return false, nil
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue, nil
		}
	}
	return false, nil
}

func (k KubectlMoveCluster) ReadClaim(namespace, name string) (*MoveClaim, error) {
	var pvc corev1.PersistentVolumeClaim
	found, err := k.getJSON("claim "+name, fmt.Sprintf("kubectl -n %s get pvc %s", shellQuote(namespace), shellQuote(name)), &pvc)
	if err != nil || !found {
		return nil, err
	}
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
	return c, nil
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

// moveDenial is how the API server words a request the pods policy denied.
// A Warn binding names the policy too, but only in a warning on a request
// that succeeds.
var moveDenial = regexp.MustCompile(`ValidatingAdmissionPolicy '` + regexp.QuoteMeta(platform.StorageMovePodsPolicy) + `' with binding '[^']*' denied request`)

// requireEnforcingBinding requires a binding to the expected pods policy
// with the Deny action, so a successful probe establishes enforcement.
func (k KubectlMoveCluster) requireEnforcingBinding() error {
	var binding struct {
		Spec struct {
			PolicyName        string   `json:"policyName"`
			ValidationActions []string `json:"validationActions"`
		} `json:"spec"`
	}
	name := platform.StorageMovePodsPolicy
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
	if err := k.requireEnforcingBinding(); err != nil {
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
	pvc := corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{Name: src.Claim, Namespace: src.Namespace, Labels: labels},
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
	root := int64(0)
	pod := corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: c.Name, Namespace: c.Namespace, Labels: moveObjectLabels(),
			Annotations: map[string]string{veleroVolumesExcludesAnnotation: "src,dst"}},
		Spec: corev1.PodSpec{
			RestartPolicy:   corev1.RestartPolicyNever,
			SecurityContext: &corev1.PodSecurityContext{RunAsUser: &root, RunAsGroup: &root},
			Containers: []corev1.Container{{
				Name: "copy", Image: moveCopyImage, Command: []string{"sh", "-c", moveCopyScript}, Resources: copyPodResources(),
				VolumeMounts: []corev1.VolumeMount{{Name: "src", MountPath: "/src", ReadOnly: true}, {Name: "dst", MountPath: "/dst"}},
			}},
			Volumes: []corev1.Volume{claimVolume("src", c.Source, true), claimVolume("dst", c.Target, false)},
		},
	}
	body, err := json.Marshal(pod)
	if err != nil {
		return "", err
	}
	out, err := k.Server.RunStdin(`kubectl create -f - -o jsonpath='kipper-uid[{.metadata.uid}]'`, strings.NewReader(string(body)))
	if err != nil {
		return "", fmt.Errorf("creating the copy pod: %w: %s", err, strings.TrimSpace(out))
	}
	m := moveCreatedUID.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("creating the copy pod: unexpected output %q", strings.TrimSpace(out))
	}
	return m[1], nil
}

func (k KubectlMoveCluster) CopyResult(namespace, name string) (CopyResult, error) {
	var pod corev1.Pod
	found, err := k.getJSON("the copy pod", fmt.Sprintf("kubectl -n %s get pod %s", shellQuote(namespace), shellQuote(name)), &pod)
	if err != nil {
		return CopyResult{}, err
	}
	if !found {
		return CopyResult{}, fmt.Errorf("the copy pod %s disappeared", name)
	}
	uid := string(pod.UID)
	if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
		return CopyResult{UID: uid}, nil
	}
	log, err := k.Server.Run(fmt.Sprintf("kubectl -n %s logs %s --tail=40 2>&1", shellQuote(namespace), shellQuote(name)))
	if err != nil {
		log = fmt.Sprintf("(the copy log could not be read: %v)", err)
	}
	succeeded := pod.Status.Phase == corev1.PodSucceeded && strings.Contains(log, "kipper-move-verified")
	return CopyResult{Finished: true, Succeeded: succeeded, UID: uid, Log: log, Reason: copyPodEnding(&pod)}, nil
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
