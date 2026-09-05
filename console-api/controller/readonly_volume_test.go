package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
	batch, ok := rc.crashLoopAlert("shop-test/db-0/postgres", &pod.Status.ContainerStatuses[0], pod, at(0), "2026-09-05T00:00:00Z")

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
	batch, ok := rc.crashLoopAlert("shop-test/db-0/postgres", &pod.Status.ContainerStatuses[0], pod, at(0), "2026-09-05T00:00:00Z")

	assert.True(t, ok)
	assert.Equal(t, "CrashLoopBackOff", batch.entry.Action)
}

// Reading a log is a call to the API server. A controller that cannot read one
// still has to alert on the crash loop it can see.
func TestCrashLoopAlertWhenTheLogCannotBeRead(t *testing.T) {
	rc := NewResourceController(nil, nil)
	rc.readPreviousLog = func(string, string, string) string { return "" }

	pod := crashLoopingPodFor("shop-test", "db", "postgres")
	batch, ok := rc.crashLoopAlert("shop-test/db-0/postgres", &pod.Status.ContainerStatuses[0], pod, at(0), "2026-09-05T00:00:00Z")

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
