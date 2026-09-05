package controller

import (
	"context"
	"fmt"
	"sort"
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
func (rc *ResourceController) readOnlyVolumeEvidence(obs workloadObservation) (string, *corev1.Pod) {
	if rc.readPreviousLog == nil {
		return "", nil
	}

	for _, f := range obs.failing {
		line := readOnlyEvidenceLine(rc.readPreviousLog(f.pod.Namespace, f.pod.Name, f.status.Name))
		if line == "" {
			continue
		}
		if claim := blamedClaim(f.pod, f.status.Name, obs.kind, line); claim != "" {
			return line, f.pod
		}
	}
	return "", nil
}

// containerMount is one place a container can write, and what is behind it.
type containerMount struct {
	path string
	// claim is the persistent volume claim behind this mount, or "" when the
	// mount is a ConfigMap, a Secret, an emptyDir or anything else.
	claim    string
	readOnly bool
}

// mountsOf lists what one container mounts, deepest path first, so the most
// specific mount containing a path is the first that matches it.
func mountsOf(pod *corev1.Pod, container string, kind containerKind) []containerMount {
	claimForVolume := map[string]string{}
	readOnlyVolume := map[string]bool{}
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		claimForVolume[v.Name] = v.PersistentVolumeClaim.ClaimName
		readOnlyVolume[v.Name] = v.PersistentVolumeClaim.ReadOnly
	}

	containers := pod.Spec.Containers
	if kind == initContainer {
		containers = pod.Spec.InitContainers
	}

	var mounts []containerMount
	for _, c := range containers {
		if c.Name != container {
			continue
		}
		for _, m := range c.VolumeMounts {
			mounts = append(mounts, containerMount{
				path:     m.MountPath,
				claim:    claimForVolume[m.Name],
				readOnly: m.ReadOnly || readOnlyVolume[m.Name],
			})
		}
	}

	sort.Slice(mounts, func(i, j int) bool { return len(mounts[i].path) > len(mounts[j].path) })
	return mounts
}

// blamedClaim names the persistent volume a read-only message is about, or ""
// when it cannot be pinned on one.
//
// The phrase on its own does not mean a persistent volume remounted. A container
// with a read-only root filesystem, or writing to a ConfigMap or Secret mount,
// logs the same errno text for a configuration reason, and recreating that pod
// reproduces the mount rather than clearing it.
//
// Where the message names a path, the mount it falls under decides. Where it
// names none, which is what the incident's own line did, the container's mounts
// decide: if everything it can write to is a persistent volume, a filesystem
// that stopped accepting writes is one of them.
func blamedClaim(pod *corev1.Pod, container string, kind containerKind, evidence string) string {
	mounts := mountsOf(pod, container, kind)

	if path := firstAbsolutePath(evidence); path != "" {
		for _, m := range mounts {
			if !underMount(path, m.path) {
				continue
			}
			if m.readOnly || m.claim == "" {
				// Mounted read-only on request, or not a volume at all.
				return ""
			}
			return m.claim
		}
		// Under nothing this container mounts: its image's own filesystem.
		return ""
	}

	var only string
	for _, m := range mounts {
		if m.readOnly {
			continue
		}
		if m.claim == "" {
			// It can write somewhere that is not a volume, so a message naming
			// no path cannot be pinned on one.
			return ""
		}
		only = m.claim
	}
	return only
}

// firstAbsolutePath picks the first absolute path out of a log line, trimming
// the punctuation a message tends to wrap it in.
func firstAbsolutePath(line string) string {
	for _, field := range strings.Fields(line) {
		trimmed := strings.Trim(field, "\"'`:,;()[]{}<>")
		if strings.HasPrefix(trimmed, "/") && len(trimmed) > 1 {
			return trimmed
		}
	}
	return ""
}

// underMount reports whether a path is inside a mount, matching whole segments
// so /var/lib/postgresqlx does not count as being under /var/lib/postgresql.
func underMount(path, mount string) bool {
	if mount == "" {
		return false
	}
	mount = strings.TrimSuffix(mount, "/")
	return path == mount || strings.HasPrefix(path, mount+"/")
}

// readOnlyVolumeAlert is the alert for a volume that went read-only underneath a
// running container.
//
// It is critical from the first sighting rather than starting as a warning and
// escalating: the filesystem does not come back on its own, so waiting six
// hours to say so only delays the recovery. It carries the log line it found,
// because an operator should see the evidence rather than trust a matcher.
func (rc *ResourceController) readOnlyVolumeAlert(key string, pod *corev1.Pod, next episode, now time.Time, nowStr, containerName string, kind containerKind, evidence string) alertBatch {
	// Stated as the evidence it is. The log line is what the container wrote;
	// that a volume remounted is the reading of it, and an operator seeing both
	// can judge for themselves.
	reason := fmt.Sprintf("container %q wrote this before it died: %s. That points at %s having remounted read-only, which a container restart cannot clear because the mount belongs to the pod",
		containerName, evidence, describeClaims(blamedClaim(pod, containerName, kind, evidence)))
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

// describeClaims names the volume, so an operator knows what to go and look at.
func describeClaims(claim string) string {
	if claim == "" {
		return "its volume"
	}
	return "volume " + claim
}
