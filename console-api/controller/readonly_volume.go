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
		if b := blameFor(f.pod, f.status.Name, obs.kind, line); b.alert {
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

// blame is what can be established about a read-only message, which is usually
// less than it looks.
type blame struct {
	// alert is whether this is worth raising at all.
	alert bool
	// certain is whether the message could be placed on one volume. When it is
	// false the alert says what the container mounts and leaves the attribution
	// open, because naming a healthy volume sends an operator to the wrong
	// place and prescribes a recreation that changes nothing.
	certain bool
	// claim is the volume the message was placed on, set only when certain.
	claim string
	// candidates are the writable volumes the container mounts, for the
	// uncertain case.
	candidates []string
}

// blameFor works out what a read-only message can be pinned on.
//
// The line says a filesystem stopped accepting writes; it does not say which.
// The container has several: its persistent volumes, whatever it mounts from a
// ConfigMap or Secret, and its own image, which lives on the node's disk and can
// go read-only too. Where the message names a path that lands on a writable
// volume, that settles it. Where it names a path that lands anywhere else, this
// is a configuration error and recreating the pod would reproduce it. Where it
// names nothing placeable, the alert still goes out, because the container does
// write to persistent volumes and a pod recreation is the remedy, but it says
// what it mounts rather than choosing one.
func blameFor(pod *corev1.Pod, container string, kind containerKind, evidence string) blame {
	mounts := mountsOf(pod, container, kind)

	var writable []string
	for _, m := range mounts {
		if !m.readOnly && m.claim != "" {
			writable = append(writable, m.claim)
		}
	}
	if len(writable) == 0 {
		// Nothing here a pod recreation would fix.
		return blame{}
	}
	sort.Strings(writable)

	// Every path, not the first. A command with two of them is the common
	// shape — mv, cp, tar — and the source is often on the image's own
	// filesystem while the destination is the volume that refused the write.
	var placed bool
	for _, path := range absolutePaths(evidence) {
		for _, m := range mounts {
			if !underMount(path, m.path) {
				continue
			}
			placed = true
			if !m.readOnly && m.claim != "" {
				return blame{alert: true, certain: true, claim: m.claim, candidates: writable}
			}
			break
		}
	}
	if placed {
		// Every path this line names lands on something mounted read-only by
		// request, or on something that is not a volume at all.
		return blame{}
	}

	// Nothing placeable. A container whose root filesystem is read-only by
	// configuration produces this message for ordinary reasons, and none of them
	// is a broken volume.
	if rootIsReadOnly(pod, container, kind) {
		return blame{}
	}
	return blame{alert: true, candidates: writable}
}

// rootIsReadOnly reports whether the container asked for a read-only root.
func rootIsReadOnly(pod *corev1.Pod, container string, kind containerKind) bool {
	containers := pod.Spec.Containers
	if kind == initContainer {
		containers = pod.Spec.InitContainers
	}
	for _, c := range containers {
		if c.Name != container {
			continue
		}
		return c.SecurityContext != nil &&
			c.SecurityContext.ReadOnlyRootFilesystem != nil &&
			*c.SecurityContext.ReadOnlyRootFilesystem
	}
	return false
}

// absolutePaths pulls every absolute path out of a log line.
//
// A path can arrive glued to a prefix with no space, which is how Java and Node
// print one, so each field is searched from its first slash rather than only
// tested at its start.
func absolutePaths(line string) []string {
	var paths []string
	for _, field := range strings.Fields(line) {
		slash := strings.Index(field, "/")
		if slash < 0 {
			continue
		}
		path := strings.Trim(field[slash:], "\"'`:,;()[]{}<>")
		if len(path) > 1 && strings.HasPrefix(path, "/") {
			paths = append(paths, path)
		}
	}
	return paths
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
	// what it means is the reading of it, and an operator seeing both can judge
	// for themselves.
	reason := fmt.Sprintf("container %q wrote this before it died: %s. %s, which a container restart cannot clear because the mount belongs to the pod",
		containerName, evidence, describeBlame(blameFor(pod, containerName, kind, evidence)))
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

// describeBlame says what was established, and no more. A message that could
// not be placed names the volumes the container mounts rather than choosing one,
// because the filesystem that stopped accepting writes might equally be the
// node's own disk under the container's image.
func describeBlame(b blame) string {
	switch {
	case b.certain:
		return "That points at volume " + b.claim + " having remounted read-only"
	case len(b.candidates) == 1:
		return "The container mounts volume " + b.candidates[0] + ". Either that volume or the node's own disk has stopped accepting writes"
	case len(b.candidates) > 1:
		return "The container mounts volumes " + strings.Join(b.candidates, " and ") + ". One of those, or the node's own disk, has stopped accepting writes"
	default:
		return "A filesystem it writes to has stopped accepting writes"
	}
}
