package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A volume that remounts read-only produces a crash loop like any other, and the
// alert said so: "container is crash-looping". The cause was in the container's
// own log the whole time, in the line the database wrote before it gave up.
//
// The distinction matters because the remedy differs. An ordinary crash loop is
// usually the image or the config. This one needs the pod recreated, and no
// number of container restarts will do it.

func TestReadOnlyEvidence(t *testing.T) {
	tests := []struct {
		name string
		log  string
		want bool
	}{
		{
			name: "postgres, which is the line from the incident",
			log:  `FATAL:  could not remove old lock file "postmaster.pid": Read-only file system`,
			want: true,
		},
		{
			name: "the kernel's own wording when ext4 gives up",
			log:  "EXT4-fs (sdb): Remounting filesystem read-only",
			want: true,
		},
		{
			name: "mysql",
			log:  "[ERROR] [MY-012574] [InnoDB] Unable to lock ./ibdata1 error: 30",
			want: false,
		},
		{
			name: "a generic write failure with the errno spelled out",
			log:  "write /var/lib/data/000001.log: read-only file system",
			want: true,
		},
		{
			name: "mongodb",
			log:  "Failed to write to file: errno:30 Read-only file system",
			want: true,
		},
		{
			name: "an ordinary crash has nothing to do with the disk",
			log:  "FATAL: password authentication failed for user \"app\"",
			want: false,
		},
		{
			name: "an application logging about a read-only replica is not this",
			log:  "connected to read-only replica, routing writes to primary",
			want: false,
		},
		{
			name: "an empty log proves nothing",
			log:  "",
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, readOnlyEvidence(tc.log))
		})
	}
}

// The alert has to carry the line it found, because an operator reading
// "read-only filesystem" needs to see the evidence rather than trust a matcher.
func TestReadOnlyEvidenceLineIsQuoted(t *testing.T) {
	log := "starting up\nchecking data directory\n" +
		`FATAL:  could not remove old lock file "postmaster.pid": Read-only file system` + "\n"

	line := readOnlyEvidenceLine(log)
	assert.Contains(t, line, "postmaster.pid")
	assert.NotContains(t, line, "starting up", "only the line that matched, not the whole log")
}

// A long line gets cut, because an alert goes to Slack and to email and neither
// wants a megabyte of log.
func TestReadOnlyEvidenceLineIsBounded(t *testing.T) {
	long := "Read-only file system: " + string(make([]byte, 4000))
	line := readOnlyEvidenceLine(long)
	assert.LessOrEqual(t, len(line), 300)
}

// The read-only case gets its own alert rather than another generic crash loop,
// because the remedy differs: the pod has to be recreated, and the restarts
// Kubernetes is already doing cannot help.
func TestCrashLoopAlertNamesAReadOnlyVolume(t *testing.T) {
	rc := NewResourceController(nil, nil)
	rc.readPreviousLog = func(string, string, string) string {
		return `FATAL:  could not remove old lock file "postmaster.pid": Read-only file system`
	}

	pod := crashLoopingPodFor("shop-test", "db", "postgres")
	batch, ok := rc.crashLoopAlert(soleObservation(pod).key, soleObservation(pod), at(0), "2026-09-05T00:00:00Z")

	assert.True(t, ok)
	assert.Equal(t, "VolumeReadOnly", batch.entry.Action,
		"a read-only volume is a different failure from a container that keeps dying")
	assert.Equal(t, stageCritical, batch.entry.Severity,
		"it will not recover on its own, so it does not start as a warning")
	assert.Contains(t, batch.entry.Reason, "postmaster.pid",
		"the alert has to carry the line it found")
	assert.Contains(t, batch.entry.Reason, "kip service restart db",
		"and the command that actually recovers it")
}

// Everything else stays as it was. Most crash loops are the image or the config.
func TestCrashLoopAlertWithoutReadOnlyEvidence(t *testing.T) {
	rc := NewResourceController(nil, nil)
	rc.readPreviousLog = func(string, string, string) string {
		return `FATAL: password authentication failed for user "app"`
	}

	pod := crashLoopingPodFor("shop-test", "db", "postgres")
	batch, ok := rc.crashLoopAlert(soleObservation(pod).key, soleObservation(pod), at(0), "2026-09-05T00:00:00Z")

	assert.True(t, ok)
	assert.Equal(t, "CrashLoopBackOff", batch.entry.Action)
}

// Reading a log is a call to the API server. A controller that cannot read one
// still has to alert on the crash loop it can see.
func TestCrashLoopAlertWhenTheLogCannotBeRead(t *testing.T) {
	rc := NewResourceController(nil, nil)
	rc.readPreviousLog = func(string, string, string) string { return "" }

	pod := crashLoopingPodFor("shop-test", "db", "postgres")
	batch, ok := rc.crashLoopAlert(soleObservation(pod).key, soleObservation(pod), at(0), "2026-09-05T00:00:00Z")

	assert.True(t, ok)
	assert.Equal(t, "CrashLoopBackOff", batch.entry.Action)
}

func crashLoopingPodFor(namespace, service, container string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      service + "-0",
			Namespace: namespace,
			Labels: map[string]string{
				"app":                     service,
				"kipper.run/service-type": "postgres",
			},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: "data-" + service + "-0",
					},
				},
			}},
			Containers: []corev1.Container{{
				Name:         container,
				VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/var/lib/data"}},
			}},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         container,
				RestartCount: 996,
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
				},
				LastTerminationState: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: 1},
				},
			}},
		},
	}
}

// A read-only volume alert is critical from the first sighting, and the episode
// has to agree. Leaving it at warning stage means the critical repeats hourly
// instead of daily, and an operator who recreates the pod within six hours gets
// no all-clear for an alert that woke them.
func TestReadOnlyAlertEscalatesTheEpisode(t *testing.T) {
	rc := NewResourceController(nil, nil)
	rc.readPreviousLog = func(string, string, string) string {
		return `FATAL:  could not remove old lock file "postmaster.pid": Read-only file system`
	}

	key := "shop-test/db-0/postgres"
	pod := crashLoopingPodFor("shop-test", "db", "postgres")
	batch, ok := rc.crashLoopAlert(key, soleObservation(pod), at(0), "2026-09-05T00:00:00Z")
	require.True(t, ok)

	rc.commitBatches([]alertBatch{batch})

	ep := rc.crashLoopEpisode[key]
	assert.False(t, ep.escalatedAt.IsZero(),
		"a critical alert has to leave the episode escalated, or it repeats on the warning cadence")
	assert.True(t, ep.deservesAllClear(),
		"somebody was told this was critical; they are owed the news it is over")

	// And the cadence that follows is the daily one, not the hourly one.
	_, due := ep.stageAt(at(2))
	assert.False(t, due, "two hours after a critical, there is nothing new to say")
	stage, due := ep.stageAt(at(25))
	assert.True(t, due)
	assert.Equal(t, stageCritical, stage)
}

// The phrase alone does not mean a persistent volume remounted. A pod with a
// read-only root filesystem, a ConfigMap mount, or a Secret mount can log the
// same errno text for a configuration reason, and recreating that pod would
// reproduce the same intentionally read-only mount.
//
// So the alert is scoped to pods that actually have a persistent volume: the
// only workloads where the diagnosis can be true and where the recovery does
// anything.
func TestReadOnlyAlertNeedsAPersistentVolume(t *testing.T) {
	rc := NewResourceController(nil, nil)
	rc.readPreviousLog = func(string, string, string) string {
		return "config error: cannot write /etc/app/settings: read-only file system"
	}

	pod := crashLoopingPodFor("blog-prod", "web", "nginx")
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "config",
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}},
	}}

	batch, ok := rc.crashLoopAlert(soleObservation(pod).key, soleObservation(pod), at(0), "2026-09-05T00:00:00Z")

	require.True(t, ok)
	assert.Equal(t, "CrashLoopBackOff", batch.entry.Action,
		"a pod with no persistent volume cannot have had one remount read-only")
}

func TestReadOnlyAlertFiresForAPodWithAClaim(t *testing.T) {
	rc := NewResourceController(nil, nil)
	rc.readPreviousLog = func(string, string, string) string {
		return `FATAL:  could not remove old lock file "postmaster.pid": Read-only file system`
	}

	pod := crashLoopingPodFor("shop-test", "db", "postgres")

	batch, ok := rc.crashLoopAlert(soleObservation(pod).key, soleObservation(pod), at(0), "2026-09-05T00:00:00Z")

	require.True(t, ok)
	assert.Equal(t, "VolumeReadOnly", batch.entry.Action)
	assert.Contains(t, batch.entry.Reason, "data-db-0",
		"the alert should name the claim, since that is what an operator goes and looks at")
}

// The claim has to belong to the container that failed. A pod can run a database
// mounting a volume alongside a sidecar that mounts none, and a sidecar dying on
// its own read-only config path would otherwise raise a critical alert naming
// the database's perfectly healthy volume.
func TestReadOnlyAlertNeedsTheFailingContainerToMountTheClaim(t *testing.T) {
	rc := NewResourceController(nil, nil)
	rc.readPreviousLog = func(string, string, string) string {
		return "cannot write /etc/sidecar/state: read-only file system"
	}

	pod := crashLoopingPodFor("shop-test", "db", "metrics")
	pod.Spec.Containers = []corev1.Container{
		{Name: "postgres", VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/var/lib/postgresql"}}},
		{Name: "metrics"},
	}

	batch, ok := rc.crashLoopAlert(soleObservation(pod).key, soleObservation(pod), at(0), "2026-09-06T00:00:00Z")

	require.True(t, ok)
	assert.Equal(t, "CrashLoopBackOff", batch.entry.Action,
		"the container that failed mounts no claim, so no volume of its can have remounted")
}

func TestReadOnlyAlertNamesOnlyTheClaimsTheFailingContainerMounts(t *testing.T) {
	rc := NewResourceController(nil, nil)
	rc.readPreviousLog = func(string, string, string) string {
		return `FATAL:  could not remove old lock file "postmaster.pid": Read-only file system`
	}

	pod := crashLoopingPodFor("shop-test", "db", "postgres")
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: "backups",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "backups-db-0"},
		},
	})
	pod.Spec.Containers = []corev1.Container{
		{Name: "postgres", VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/var/lib/postgresql"}}},
		{Name: "backup-agent", VolumeMounts: []corev1.VolumeMount{{Name: "backups", MountPath: "/backups"}}},
	}

	batch, ok := rc.crashLoopAlert(soleObservation(pod).key, soleObservation(pod), at(0), "2026-09-06T00:00:00Z")

	require.True(t, ok)
	assert.Equal(t, "VolumeReadOnly", batch.entry.Action)
	assert.Contains(t, batch.entry.Reason, "data-db-0")
	assert.NotContains(t, batch.entry.Reason, "backups-db-0",
		"the failing container does not mount that claim, so it is not the one to look at")
}

// A volume the workload asked for read-only did not remount; it was mounted that
// way. Recreating the pod reproduces it, so the alert would give a wrong
// diagnosis and a recovery that cannot work.
func TestReadOnlyAlertIgnoresAVolumeMountedReadOnlyOnPurpose(t *testing.T) {
	for _, tc := range []struct {
		name string
		pod  func(*corev1.Pod)
	}{
		{
			name: "the mount asks for read-only",
			pod: func(p *corev1.Pod) {
				p.Spec.Containers[0].VolumeMounts[0].ReadOnly = true
			},
		},
		{
			name: "the claim itself is read-only",
			pod: func(p *corev1.Pod) {
				p.Spec.Volumes[0].PersistentVolumeClaim.ReadOnly = true
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := NewResourceController(nil, nil)
			rc.readPreviousLog = func(string, string, string) string {
				return "cannot write /var/lib/data/x: read-only file system"
			}

			pod := crashLoopingPodFor("shop-test", "db", "postgres")
			tc.pod(pod)

			batch, ok := rc.crashLoopAlert(soleObservation(pod).key, soleObservation(pod), at(0), "2026-09-06T00:00:00Z")

			require.True(t, ok)
			assert.Equal(t, "CrashLoopBackOff", batch.entry.Action,
				"that volume was mounted read-only on request, so nothing remounted")
		})
	}
}

// soleObservation reduces one pod the way the scan does, so a test can drive the
// alert path with the same value the controller builds.
func soleObservation(pod *corev1.Pod) workloadObservation {
	obs := observeWorkloads([]corev1.Pod{*pod})
	if len(obs) != 1 {
		panic("test pod must have exactly one container status")
	}
	return obs[0]
}

// A container can mount a healthy PVC and also write to something that is
// read-only on purpose: its own root filesystem, a ConfigMap, a Secret. Writing
// to the wrong one produces the same errno text, and blaming the PVC sends the
// operator to a volume that is fine and prescribes a recreation that reproduces
// the problem.
//
// Where the log names a path, that decides it.
func TestReadOnlyEvidenceFollowsThePathTheLogNames(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{Name: "data", VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-db-0"}}},
				{Name: "config", VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{}}},
			},
			Containers: []corev1.Container{{
				Name: "postgres",
				VolumeMounts: []corev1.VolumeMount{
					{Name: "data", MountPath: "/var/lib/postgresql"},
					{Name: "config", MountPath: "/etc/postgresql", ReadOnly: true},
				},
			}},
		},
	}

	tests := []struct {
		name  string
		line  string
		blame string
	}{
		{
			name:  "the path is under the volume",
			line:  `could not write /var/lib/postgresql/data/pg_wal/000001: Read-only file system`,
			blame: "data-db-0",
		},
		{
			name:  "the path is under the config mount, which is read-only on purpose",
			line:  "cannot write /etc/postgresql/postgresql.conf: read-only file system",
			blame: "",
		},
		{
			name:  "the path is somewhere else entirely, so it is not the volume",
			line:  "cannot write /tmp/scratch: read-only file system",
			blame: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.blame, blamedClaim(pod, "postgres", mainContainer, tc.line))
		})
	}
}

// The line from the incident names a lock file and no path at all, which is the
// case this whole feature exists for. With nothing to correlate, the container's
// mounts decide: every writable mount it has is a persistent volume, so a
// read-only filesystem underneath it is one of them.
func TestReadOnlyEvidenceWithNoPathInTheLine(t *testing.T) {
	incident := `FATAL:  could not remove old lock file "postmaster.pid": Read-only file system`

	onlyAVolume := &corev1.Pod{
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-db-0"}}}},
			Containers: []corev1.Container{{
				Name:         "postgres",
				VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/var/lib/postgresql"}},
			}},
		},
	}
	assert.Equal(t, "data-db-0", blamedClaim(onlyAVolume, "postgres", mainContainer, incident),
		"the only writable mount it has is the volume")

	alsoWritesElsewhere := onlyAVolume.DeepCopy()
	alsoWritesElsewhere.Spec.Volumes = append(alsoWritesElsewhere.Spec.Volumes, corev1.Volume{
		Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})
	alsoWritesElsewhere.Spec.Containers[0].VolumeMounts = append(
		alsoWritesElsewhere.Spec.Containers[0].VolumeMounts,
		corev1.VolumeMount{Name: "scratch", MountPath: "/scratch"})

	assert.Empty(t, blamedClaim(alsoWritesElsewhere, "postgres", mainContainer, incident),
		"it writes somewhere that is not a volume, so an unplaced message cannot be pinned on one")
}
