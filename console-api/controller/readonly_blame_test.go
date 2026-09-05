package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
)

// A log line saying "read-only file system" does not say which filesystem. The
// container has several: its persistent volumes, anything mounted from a
// ConfigMap or Secret, and its own image, which lives on the node's disk and can
// go read-only too.
//
// So the answer is one of three, and pretending otherwise is what makes an alert
// send somebody to the wrong place:
//
//   - a path that lands on a writable volume: that volume, named;
//   - a path that lands somewhere else: not a volume problem, so nothing;
//   - nothing placeable: say what the container mounts and leave the
//     attribution open.

func pgPod(rootReadOnly bool, mounts ...corev1.VolumeMount) *corev1.Pod {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{Name: "data", VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-db-0"}}},
				{Name: "wal", VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "wal-db-0"}}},
				{Name: "config", VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{}}},
			},
			Containers: []corev1.Container{{Name: "postgres", VolumeMounts: mounts}},
		},
	}
	if rootReadOnly {
		yes := true
		pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{ReadOnlyRootFilesystem: &yes}
	}
	return pod
}

var pgMounts = []corev1.VolumeMount{
	{Name: "data", MountPath: "/var/lib/postgresql"},
	{Name: "wal", MountPath: "/var/lib/wal"},
	{Name: "config", MountPath: "/etc/postgresql", ReadOnly: true},
}

// A command with two paths is the common shape: mv, cp, tar. The source is often
// on the image's own filesystem, and taking only the first path would silence
// the alert for a destination that really is a broken volume.
func TestBlameReadsEveryPathInTheLine(t *testing.T) {
	pod := pgPod(false, pgMounts...)

	blame := blameFor(pod, "postgres", mainContainer,
		"mv /tmp/pg_wal_backup /var/lib/wal/000001: Read-only file system")

	assert.Equal(t, "wal-db-0", blame.claim,
		"the destination is the volume that refused the write")
	assert.True(t, blame.certain)
}

// A path prefixed without a space is normal in a Java or Node stack trace.
func TestBlameFindsAPathBehindAPrefix(t *testing.T) {
	pod := pgPod(false, pgMounts...)

	blame := blameFor(pod, "postgres", mainContainer,
		"java.io.FileNotFoundException:/var/lib/postgresql/global/pg_control (Read-only file system)")

	assert.Equal(t, "data-db-0", blame.claim)
	assert.True(t, blame.certain)
}

// A path under a mount that is read-only by request is a configuration error.
// Recreating the pod reproduces it.
func TestBlameStaysQuietForADeliberatelyReadOnlyPath(t *testing.T) {
	pod := pgPod(false, pgMounts...)

	blame := blameFor(pod, "postgres", mainContainer,
		"cannot write /etc/postgresql/postgresql.conf: read-only file system")

	assert.False(t, blame.alert, "that mount was asked for read-only")
}

// The line from the incident names a lock file and no path. Nothing can be
// placed, so the alert still goes out (the container writes to volumes and a pod
// recreation is the remedy) but it does not claim to know which filesystem.
func TestBlameIsHonestWhenNothingCanBePlaced(t *testing.T) {
	pod := pgPod(false, pgMounts...)

	blame := blameFor(pod, "postgres", mainContainer,
		`FATAL:  could not remove old lock file "postmaster.pid": Read-only file system`)

	assert.True(t, blame.alert, "the container writes to persistent volumes; this is worth saying")
	assert.False(t, blame.certain, "but which filesystem went is not knowable from that line")
	assert.Equal(t, []string{"data-db-0", "wal-db-0"}, blame.candidates,
		"so the alert names what it mounts and lets the operator look")
}

// A container whose root filesystem is read-only by configuration will log this
// message for perfectly ordinary reasons, and none of them is a broken volume.
func TestBlameStaysQuietWhenTheRootIsReadOnlyByDesign(t *testing.T) {
	pod := pgPod(true, pgMounts...)

	blame := blameFor(pod, "postgres", mainContainer,
		`could not remove old lock file "postmaster.pid": Read-only file system`)

	assert.False(t, blame.alert,
		"a read-only root makes an unplaceable message ambiguous in a way that is usually not the volume")
}

// A path landing on the image's own filesystem, with nothing else to go on, is
// the node's disk rather than a volume. It is still worth an alert, because the
// container cannot write and a pod recreation may move it, but it names nothing.
func TestBlameOnAPathUnderNoMount(t *testing.T) {
	pod := pgPod(false, pgMounts...)

	blame := blameFor(pod, "postgres", mainContainer,
		"cannot write /var/run/postgresql/.s.PGSQL.5432: read-only file system")

	assert.True(t, blame.alert)
	assert.False(t, blame.certain)
}

// A container that mounts no volume at all has nothing this alert can help with.
func TestBlameStaysQuietWithoutAVolume(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		Containers: []corev1.Container{{Name: "web"}},
	}}

	blame := blameFor(pod, "web", mainContainer, "cannot write: read-only file system")
	assert.False(t, blame.alert)
}
