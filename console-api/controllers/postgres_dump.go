package controllers

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

const (
	dumpVolume    = "kipper-dump"
	dumpMountPath = "/var/lib/kipper-dump"
	// dumpForLabel marks a service's dump claim, so it can be told apart from
	// the data claim that shares the app label.
	dumpForLabel = "kipper.run/dump-for"

	preHookContainer  = "pre.hook.backup.velero.io/container"
	preHookCommand    = "pre.hook.backup.velero.io/command"
	preHookOnError    = "pre.hook.backup.velero.io/on-error"
	preHookTimeout    = "pre.hook.backup.velero.io/timeout"
	postHookContainer = "post.hook.backup.velero.io/container"
	postHookCommand   = "post.hook.backup.velero.io/command"
	postHookOnError   = "post.hook.backup.velero.io/on-error"
	postHookTimeout   = "post.hook.backup.velero.io/timeout"
)

var dumpHookAnnotations = []string{preHookContainer, preHookCommand, preHookOnError, preHookTimeout, postHookContainer, postHookCommand, postHookOnError, postHookTimeout}

func dumpClaimName(service string) string { return service + "-dump" }

func wantsConsistentBackups(svc *kipperv1.Service) bool {
	return svc.Spec.Type == "postgres" && svc.Spec.ConsistentBackups
}

// dumpHookTimeout allows 30 minutes plus one minute per GiB of requested data
// storage, rounded up to the next GiB, instead of the default 30 seconds.
func dumpHookTimeout(storage resource.Quantity) string {
	gib := (storage.Value() + (1<<30 - 1)) >> 30
	return fmt.Sprintf("%dm", 30+gib)
}

// ensureDumpClaim creates the dump volume, sized like the data volume, and
// expands non-local claims when the requested data size grows. Existing claims
// retain at least their current size.
func (r *ServiceReconciler) ensureDumpClaim(ctx context.Context, svc *kipperv1.Service, storageClass string, storage resource.Quantity) error {
	var existing corev1.PersistentVolumeClaim
	err := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: dumpClaimName(svc.Name)}, &existing)
	if errors.IsNotFound(err) {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name: dumpClaimName(svc.Name), Namespace: svc.Namespace,
				Labels: map[string]string{kipperLabel: kipperValue, "app": svc.Name, dumpForLabel: svc.Name},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				StorageClassName: &storageClass,
				Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: storage}},
			},
		}
		if err := controllerutil.SetControllerReference(svc, pvc, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, pvc)
	}
	if err != nil {
		return err
	}
	// The service's dump label allows adoption after a restore strips owner references,
	// matching the StatefulSet adoption policy.
	owner := metav1.GetControllerOf(&existing)
	if (owner != nil && owner.UID != svc.UID) || (owner == nil && existing.Labels[dumpForLabel] != svc.Name) {
		return &nameTakenError{Kind: "PersistentVolumeClaim", Name: existing.Name, Namespace: svc.Namespace}
	}
	changed := false
	if owner == nil {
		if err := adopt(&existing, svc, r.Scheme); err != nil {
			return err
		}
		changed = true
	}
	// Local-path volumes use the disk's free space and their provisioner rejects expansion.
	current := existing.Spec.Resources.Requests[corev1.ResourceStorage]
	local := existing.Spec.StorageClassName != nil && *existing.Spec.StorageClassName == "local-path"
	if current.Cmp(storage) < 0 && !local {
		existing.Spec.Resources.Requests[corev1.ResourceStorage] = storage
		changed = true
	}
	if !changed {
		return nil
	}
	return r.Update(ctx, &existing)
}

// removeDumpClaim deletes the dump volume of a service whose consistent
// backups are off. Call it after updating the StatefulSet template to remove
// the mount. PVC protection defers deletion while a pod still uses the claim.
func (r *ServiceReconciler) removeDumpClaim(ctx context.Context, svc *kipperv1.Service) error {
	if svc.Spec.Type != "postgres" || svc.Spec.ConsistentBackups {
		return nil
	}
	var existing corev1.PersistentVolumeClaim
	err := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: dumpClaimName(svc.Name)}, &existing)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if owner := metav1.GetControllerOf(&existing); owner == nil || owner.UID != svc.UID {
		return nil
	}
	if err := r.Delete(ctx, &existing); err != nil && !errors.IsNotFound(err) {
		return err
	}
	return nil
}

// applyConsistentBackups mounts the dump volume into the Postgres container
// and adds the Velero hooks that write a dump before the volumes are copied
// and remove it afterwards.
func applyConsistentBackups(tpl *corev1.PodTemplateSpec, svcName string, storage resource.Quantity) {
	c := &tpl.Spec.Containers[0]
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: dumpVolume, MountPath: dumpMountPath})
	tpl.Spec.Volumes = append(tpl.Spec.Volumes, dumpPodVolume(svcName))
	if tpl.Annotations == nil {
		tpl.Annotations = map[string]string{}
	}
	tpl.Annotations[preHookContainer] = svcName
	tpl.Annotations[preHookCommand] = shellCommandAnnotation(postgresDumpScript)
	tpl.Annotations[preHookOnError] = "Continue"
	tpl.Annotations[preHookTimeout] = dumpHookTimeout(storage)
	tpl.Annotations[postHookContainer] = svcName
	tpl.Annotations[postHookCommand] = shellCommandAnnotation(`rm -rf ` + dumpMountPath + `/current`)
	// The next pre-hook removes a leftover dump anyway, so a slow or failed
	// cleanup must not count against a backup whose dump and copy succeeded.
	tpl.Annotations[postHookOnError] = "Continue"
	tpl.Annotations[postHookTimeout] = "10m"
}

func dumpPodVolume(svcName string) corev1.Volume {
	return corev1.Volume{Name: dumpVolume, VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: dumpClaimName(svcName)},
	}}
}

// shellCommandAnnotation renders script as the JSON array Velero reads from
// a hook command annotation.
func shellCommandAnnotation(script string) string {
	b, _ := json.Marshal([]string{"/bin/sh", "-c", script})
	return string(b)
}

// syncDumpTemplate carries the dump volume and hook annotations of desired
// onto a live template, or strips them when desired has none. A service that
// has neither a live nor a desired pre-hook keeps its template unchanged.
func syncDumpTemplate(live, desired *corev1.PodTemplateSpec, svcName string) {
	_, on := desired.Annotations[preHookCommand]
	_, wasOn := live.Annotations[preHookCommand]
	if !on && !wasOn {
		return
	}
	if on {
		live.Spec.Volumes = withVolume(live.Spec.Volumes, dumpPodVolume(svcName))
		if live.Annotations == nil {
			live.Annotations = map[string]string{}
		}
		for _, k := range dumpHookAnnotations {
			live.Annotations[k] = desired.Annotations[k]
		}
		return
	}
	var volumes []corev1.Volume
	for _, v := range live.Spec.Volumes {
		if v.Name != dumpVolume {
			volumes = append(volumes, v)
		}
	}
	live.Spec.Volumes = volumes
	for _, k := range dumpHookAnnotations {
		delete(live.Annotations, k)
	}
}

// postgresDumpScript runs before Velero copies the pod's volumes. The space
// check requires room for the data directory excluding WAL. On a shared
// filesystem, it also reserves the larger of 2 GiB and 10% of the filesystem.
// Each database's dump and table row counts use the same exported snapshot.
// PGDATABASE passes database names literally to both tools. Names containing
// line breaks or tabs are rejected because the manifest cannot represent them.
// The manifest records completion after the dump and count sessions succeed.
// Explicit failure paths exit with status 1 for Velero's hooksFailed count.
const postgresDumpScript = `set -u
D=` + dumpMountPath + `/current
umask 077
rm -rf "$D" && mkdir -p "$D" || exit 1
fail() { printf 'complete\tfalse\nerror\t%s\n' "$1" >> "$D/manifest"; exit 1; }
printf 'format\tkipper-dump/1\nstarted\t%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$D/manifest"
need=$(du -sk "$PGDATA" | cut -f1)
wal=$(du -sk "$PGDATA/pg_wal" 2>/dev/null | cut -f1)
[ -n "$need" ] || fail "measuring the data directory failed"
need=$((need - ${wal:-0}))
total=$(df -Pk ` + dumpMountPath + ` | awk 'NR==2 {print $2}')
free=$(df -Pk ` + dumpMountPath + ` | awk 'NR==2 {print $4}')
reserve=0
if [ "$(df -P "$PGDATA" | awk 'NR==2 {print $1}')" = "$(df -P ` + dumpMountPath + ` | awk 'NR==2 {print $1}')" ]; then
  reserve=$((total / 10)); [ "$reserve" -gt 2097152 ] || reserve=2097152
fi
[ "$free" -gt $((need + reserve)) ] || fail "the dump volume has ${free} KiB free, and the data uses ${need} KiB plus ${reserve} KiB kept free"
export U="${POSTGRES_USER:-postgres}"
odd=$(psql -U "$U" -d postgres -XAtc "SELECT count(*) FROM pg_database WHERE datallowconn AND NOT datistemplate AND datname ~ E'[\\n\\r\\t]'") || fail "listing databases failed"
[ "$odd" = 0 ] || fail "a database name contains a line break or tab, which the dump manifest cannot record"
psql -U "$U" -d postgres -XAtc "SELECT datname FROM pg_database WHERE datallowconn AND NOT datistemplate ORDER BY 1" > "$D/databases" || fail "listing databases failed"
pg_dumpall -U "$U" --globals-only > "$D/globals.sql" || fail "dumping roles failed"
ok=1
i=0
while IFS= read -r db; do
  i=$((i + 1))
  PGDATABASE="$db" psql -U "$U" -X -q -v ON_ERROR_STOP=1 -v out="$D/$i" -v counts="$D/$i.counts" <<'SQL' || ok=0
BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;
SELECT pg_export_snapshot() AS snap \gset
\setenv KIPPER_SNAPSHOT :snap
\setenv KIPPER_OUT :out
\! pg_dump -U "$U" -Fc --snapshot="$KIPPER_SNAPSHOT" -f "$KIPPER_OUT.dump"; echo $? > "$KIPPER_OUT.rc"
\pset tuples_only on
\pset format unaligned
\o :counts
SELECT format('SELECT %L || ''|'' || count(*) FROM %I.%I', schemaname || '.' || tablename, schemaname, tablename) FROM pg_tables WHERE schemaname NOT IN ('pg_catalog', 'information_schema') ORDER BY 1 \gexec
\o
COMMIT;
SQL
  rc=$(cat "$D/$i.rc" 2>/dev/null || echo missing)
  [ "$rc" = 0 ] || ok=0
  size=0
  [ -f "$D/$i.dump" ] && size=$(wc -c < "$D/$i.dump" | tr -d " ")
  sum=$(sha256sum "$D/$i.dump" 2>/dev/null | cut -d' ' -f1)
  printf 'database\t%s\t%s.dump\t%s\t%s\t%s\n' "$db" "$i" "$rc" "$size" "$sum" >> "$D/manifest"
done < "$D/databases"
printf 'finished\t%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$D/manifest"
[ "$ok" = 1 ] || fail "a database dump failed"
printf 'complete\ttrue\n' >> "$D/manifest"
`
