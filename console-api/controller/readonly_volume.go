package controller

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
)

// readOnlyMarkers are the ways a process says its disk stopped accepting
// writes. The errno text is the common thread: every language's strerror(30)
// produces it, so it catches Postgres, MySQL, MongoDB and anything using libc
// without a per-engine list. The kernel's own line is here because it names the
// moment the filesystem gave up, and it appears in the container log when the
// volume is the one the container is writing to.
var readOnlyMarkers = []string{
	"read-only file system",
	"remounting filesystem read-only",
	"read-only filesystem",
}

// evidenceLineLimit keeps a quoted log line to something an alert can carry.
// The alert goes to Slack and to email, and neither is a log viewer.
const evidenceLineLimit = 300

// readOnlyEvidence reports whether a log says the filesystem underneath it went
// read-only.
//
// It exists because that failure is invisible in every other signal: the volume
// is attached, the pod is scheduled, the metrics path cannot see the mount, and
// the crash loop it produces looks like any other. The container's own log is
// the only place it says so.
func readOnlyEvidence(log string) bool {
	return readOnlyEvidenceLine(log) != ""
}

// readOnlyEvidenceLine returns the line that says so, trimmed to something an
// alert can carry, or "" when nothing in the log does.
func readOnlyEvidenceLine(log string) string {
	for _, line := range strings.Split(log, "\n") {
		lower := strings.ToLower(line)
		for _, marker := range readOnlyMarkers {
			if !strings.Contains(lower, marker) {
				continue
			}
			return boundedLine(line)
		}
	}
	return ""
}

// boundedLine trims a log line to the limit, cutting on a rune boundary so a
// truncated multi-byte character cannot turn the alert into mojibake, and
// counting the ellipsis against the limit rather than past it.
func boundedLine(line string) string {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) <= evidenceLineLimit {
		return trimmed
	}

	const ellipsis = "…"
	cut := evidenceLineLimit - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(trimmed[cut]) {
		cut--
	}
	return trimmed[:cut] + ellipsis
}

// previousLogTail is how much of the dead container's log to read. The line
// that says the filesystem went read-only is written as the process gives up,
// so it is at the end.
const previousLogTail = 50

// previousLogTimeout bounds one log read. The scan runs over every managed pod
// on a tick, so a hung API server must not hold the tick open.
const previousLogTimeout = 5 * time.Second

// readPreviousContainerLog fetches the tail of the log the container wrote
// before it died.
//
// A crash loop means the interesting log belongs to the previous container, and
// the running one has usually not got far enough to say anything. Failures are
// answered with "": a log that cannot be read is not evidence of anything, and
// the crash loop still deserves its ordinary alert.
func (rc *ResourceController) readPreviousContainerLog(namespace, pod, container string) string {
	if rc.client == nil {
		return ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), previousLogTimeout)
	defer cancel()

	tail := int64(previousLogTail)
	req := rc.client.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container: container,
		TailLines: &tail,
		Previous:  true,
	})
	out, err := req.DoRaw(ctx)
	if err != nil {
		return ""
	}
	return string(out)
}

// readOnlyVolumeEvidence returns the log line saying this container's
// filesystem stopped accepting writes, or "" when its log says nothing of the
// kind or cannot be read.
func (rc *ResourceController) readOnlyVolumeEvidence(pod *corev1.Pod, cs *corev1.ContainerStatus) string {
	if rc.readPreviousLog == nil {
		return ""
	}
	return readOnlyEvidenceLine(rc.readPreviousLog(pod.Namespace, pod.Name, cs.Name))
}

// readOnlyVolumeAlert is the alert for a volume that went read-only underneath a
// running container.
//
// It is critical from the first sighting rather than starting as a warning and
// escalating: the filesystem does not come back on its own, so waiting six
// hours to say so only delays the recovery. It carries the log line it found,
// because an operator should see the evidence rather than trust a matcher.
func (rc *ResourceController) readOnlyVolumeAlert(key string, pod *corev1.Pod, next episode, now time.Time, nowStr, evidence string) alertBatch {
	reason := "the filesystem this container writes to has remounted read-only, which a container restart cannot clear: " + evidence
	if cmd := recoveryCommand(pod.Labels); cmd != "" {
		reason += ". Recover with: " + cmd
	}

	return alertBatch{
		entry: ResourceLogEntry{
			Time:      nowStr,
			App:       pod.Labels["app"],
			Namespace: pod.Namespace,
			Action:    "VolumeReadOnly",
			Severity:  stageCritical,
			Reason:    reason,
		},
		marks: []pendingMark{{dst: rc.crashLoopAlerted, key: key, at: now}},
		apply: []func(){func() { rc.crashLoopEpisode[key] = next }},
	}
}
