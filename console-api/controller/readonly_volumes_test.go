package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
)

// A log line says a filesystem stopped accepting writes. It does not say which,
// and no parser can settle it: a pathname cannot establish its backing
// filesystem without resolving the container's mount namespace and its symlinks,
// and Postgres puts /var/lib/postgresql/data/pg_wal on a different volume from
// the path it is written as often enough for that to matter.
//
// Four review rounds went into attributing the failure from the line, and the
// attempt was wrong in both directions: naming a healthy volume with confidence,
// and suppressing a real incident because a path looked like it pointed
// somewhere else. So the alert reports what it knows.

func containerWith(volumes []corev1.Volume, mounts []corev1.VolumeMount) *corev1.Pod {
	return &corev1.Pod{
		Spec: corev1.PodSpec{
			Volumes:    volumes,
			Containers: []corev1.Container{{Name: "postgres", VolumeMounts: mounts}},
		},
	}
}

func claimVolume(name, claim string, readOnly bool) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: claim, ReadOnly: readOnly,
		},
	}}
}

func TestWritableClaims(t *testing.T) {
	tests := []struct {
		name    string
		volumes []corev1.Volume
		mounts  []corev1.VolumeMount
		want    []string
	}{
		{
			name:    "one volume",
			volumes: []corev1.Volume{claimVolume("data", "data-db-0", false)},
			mounts:  []corev1.VolumeMount{{Name: "data", MountPath: "/var/lib/postgresql"}},
			want:    []string{"data-db-0"},
		},
		{
			name: "two volumes, named in a stable order",
			volumes: []corev1.Volume{
				claimVolume("wal", "wal-db-0", false),
				claimVolume("data", "data-db-0", false),
			},
			mounts: []corev1.VolumeMount{
				{Name: "wal", MountPath: "/var/lib/wal"},
				{Name: "data", MountPath: "/var/lib/postgresql"},
			},
			want: []string{"data-db-0", "wal-db-0"},
		},
		{
			name:    "a mount asked for read-only did not remount; it was mounted that way",
			volumes: []corev1.Volume{claimVolume("config", "config-db-0", false)},
			mounts:  []corev1.VolumeMount{{Name: "config", MountPath: "/etc/postgresql", ReadOnly: true}},
			want:    nil,
		},
		{
			name:    "and neither did a claim declared read-only",
			volumes: []corev1.Volume{claimVolume("config", "config-db-0", true)},
			mounts:  []corev1.VolumeMount{{Name: "config", MountPath: "/etc/postgresql"}},
			want:    nil,
		},
		{
			name: "a ConfigMap is not a volume this can help with",
			volumes: []corev1.Volume{{
				Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}},
			}},
			mounts: []corev1.VolumeMount{{Name: "config", MountPath: "/etc/postgresql"}},
			want:   nil,
		},
		{
			name:    "nor is an emptyDir",
			volumes: []corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
			mounts:  []corev1.VolumeMount{{Name: "scratch", MountPath: "/scratch"}},
			want:    nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, writableClaims(containerWith(tc.volumes, tc.mounts), "postgres", mainContainer))
		})
	}
}

// An init container and a main container can share a name, and their mounts must
// not run together.
func TestWritableClaimsAreScopedToTheContainerKind(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		Volumes: []corev1.Volume{
			claimVolume("init-data", "scratch", false),
			claimVolume("app-data", "data", false),
		},
		InitContainers: []corev1.Container{{
			Name: "db", VolumeMounts: []corev1.VolumeMount{{Name: "init-data", MountPath: "/init"}},
		}},
		Containers: []corev1.Container{{
			Name: "db", VolumeMounts: []corev1.VolumeMount{{Name: "app-data", MountPath: "/data"}},
		}},
	}}

	assert.Equal(t, []string{"data"}, writableClaims(pod, "db", mainContainer))
	assert.Equal(t, []string{"scratch"}, writableClaims(pod, "db", initContainer))
}

// The message says what the container mounts and leaves the attribution alone.
func TestDescribeClaims(t *testing.T) {
	assert.Contains(t, describeClaims([]string{"data-db-0"}), "volume data-db-0")
	assert.Contains(t, describeClaims([]string{"data-db-0"}), "the node's own disk",
		"the volume is not the only filesystem that can go read-only")
	assert.Contains(t, describeClaims([]string{"data-db-0", "wal-db-0"}), "data-db-0 and wal-db-0")
	assert.NotContains(t, describeClaims([]string{"data-db-0"}), "points at",
		"nothing here establishes which filesystem failed")
}
