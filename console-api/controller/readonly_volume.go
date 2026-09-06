package controller

import (
	"context"
	"fmt"
	"slices"
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
			if strings.Contains(lower, marker) {
				return boundedLine(line)
			}
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
func (rc *ResourceController) readPreviousContainerLog(ctx context.Context, namespace, pod, container string) string {
	if rc.client == nil {
		return ""
	}

	ctx, cancel := context.WithTimeout(ctx, previousLogTimeout)
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
func (rc *ResourceController) readOnlyVolumeEvidence(ctx context.Context, budget *evidenceBudget, obs workloadObservation) (string, *corev1.Pod) {
	if rc.readPreviousLog == nil {
		return "", nil
	}

	var candidates []failingContainer
	for _, f := range obs.failing {
		// Nothing a pod recreation would fix.
		if len(writableClaims(f.pod, f.status.Name, obs.kind)) > 0 {
			candidates = append(candidates, f)
		}
	}
	if len(candidates) == 0 {
		return "", nil
	}

	// Admission is per workload, not per read. The evidence can be in any of a
	// workload's failing replicas, and admitting one read at a time meant a
	// workload whose second replica held it spent its turn on the first and was
	// refused again — every tick, for good.
	if !budget.admit(obs.key) {
		// The tick has read as much as it is allowed to. This workload still
		// gets its ordinary crash-loop alert, and the next tick owes it, so the
		// diagnosis is delayed rather than lost.
		return "", nil
	}

	for _, f := range candidates {
		if line := readOnlyEvidenceLine(rc.readPreviousLog(ctx, f.pod.Namespace, f.pod.Name, f.status.Name)); line != "" {
			return line, f.pod
		}
	}
	return "", nil
}

// evidenceBudgetPerTick caps how long one scan may spend reading container logs.
//
// The reads happen while the controller holds its state lock, and one is
// bounded at five seconds. A cluster with fifty failing workloads would
// otherwise hold that lock for minutes on the first tick after a restart.
// Nothing contends for it today, because it is only ever taken from the tick,
// and this is what keeps that from becoming a trap for whoever adds a reader
// off the tick path.
// A variable so a test can prove the ceiling without waiting ten seconds for it.
var evidenceBudgetPerTick = 10 * time.Second

// evidenceBudget is one tick's allowance for reading logs.
//
// It also carries what earlier ticks could not afford, oldest first. The scan
// walks a stable order, so without a queue the same workloads at the front
// spend the budget every time and one further down is never read at all — its
// diagnosis lost rather than delayed.
type evidenceBudget struct {
	deadline time.Time
	// owed is the debt carried in, oldest first. Only its head may overrun the
	// deadline, which is what makes the queue advance instead of paying the
	// same workload every tick.
	owed []string
	paid map[string]bool
	// fresh is what this tick could not afford and did not already owe.
	fresh   []string
	overran bool
}

// evidenceBudget starts a fresh allowance, owing whatever earlier ticks skipped.
func (rc *ResourceController) evidenceBudget() *evidenceBudget {
	return &evidenceBudget{
		deadline: time.Now().Add(evidenceBudgetPerTick),
		owed:     rc.evidenceOwed,
		paid:     map[string]bool{},
	}
}

// admit reports whether this workload's logs may be read on this tick.
//
// Inside the budget, anything goes. Past it, the workload at the head of the
// debt queue is let through and the rest wait: that drains a backlog at a
// workload per tick while keeping the overrun to one workload, rather than
// paying off fifty of them at five seconds each on the tick after an outage,
// which is the lock-holding this budget exists to prevent.
func (b *evidenceBudget) admit(key string) bool {
	if b == nil {
		return false
	}
	if time.Now().Before(b.deadline) {
		b.paid[key] = true
		return true
	}
	if !b.overran && len(b.owed) > 0 && b.owed[0] == key {
		b.paid[key] = true
		b.overran = true
		return true
	}
	if !b.isOwed(key) {
		b.fresh = append(b.fresh, key)
	}
	return false
}

func (b *evidenceBudget) isOwed(key string) bool {
	for _, k := range b.owed {
		if k == key {
			return true
		}
	}
	return false
}

// carry is the debt to hand the next tick: what is still owed, in the order it
// was incurred, then whatever this tick added.
func (b *evidenceBudget) carry() []string {
	var next []string
	for _, k := range b.owed {
		if !b.paid[k] {
			next = append(next, k)
		}
	}
	return append(next, b.fresh...)
}

// writableClaims lists the persistent volumes a container can write to.
//
// This is the whole of the test. A log line says a filesystem stopped accepting
// writes and does not say which, and no amount of parsing can settle it: a
// pathname cannot establish its backing filesystem without resolving the
// container's mount namespace and its symlinks, and the ordinary Postgres layout
// puts /var/lib/postgresql/data/pg_wal on a different volume from the path it is
// written as.
//
// Four rounds of review were spent trying to attribute the failure from the
// line, and the attempt was wrong in both directions: naming a healthy volume
// with confidence, and suppressing a real incident because a path looked like it
// pointed elsewhere. So the alert reports what it knows. The container writes to
// these volumes, one of them or the node's disk stopped accepting writes, and
// here is the line it wrote. An operator reading that has everything the parser
// was trying to guess, and none of the guesses.
func writableClaims(pod *corev1.Pod, container string, kind containerKind) []string {
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

	var claims []string
	for _, c := range containers {
		if c.Name != container {
			continue
		}
		for _, m := range c.VolumeMounts {
			// A volume the workload asked for read-only did not remount; it was
			// mounted that way, and recreating the pod reproduces it.
			if m.ReadOnly || readOnlyVolume[m.Name] {
				continue
			}
			if claim, ok := claimForVolume[m.Name]; ok && !slices.Contains(claims, claim) {
				claims = append(claims, claim)
			}
		}
	}
	sort.Strings(claims)
	return claims
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
	reason := fmt.Sprintf("container %q wrote this before it died: %s. %s",
		containerName, evidence, describeClaims(writableClaims(pod, containerName, kind), recoveryCommand(pod.Labels)))

	return alertBatch{
		entry: ResourceLogEntry{
			Time:      nowStr,
			App:       pod.Labels["app"],
			Namespace: pod.Namespace,
			Action:    "ReadOnlyFilesystem",
			Severity:  stageCritical,
			Reason:    reason,
		},
		marks: []pendingMark{{dst: rc.crashLoopAlerted, key: key, at: now}},
		apply: []func(){func() { rc.crashLoopEpisode[key] = next }},
	}
}

// describeClaims names the volumes the container writes to, and stops there.
func describeClaims(claims []string, recovery string) string {
	// The command sits inside the condition, never after it as an imperative: a
	// container with a writable volume and a read-only ConfigMap that failed on
	// the ConfigMap produces this same message, and recreating that pod
	// interrupts the service and brings back the same configuration.
	act := "recreating the pod"
	if recovery != "" {
		act = "'" + recovery + "' recreates the pod and"
	}

	mounts := "The container mounts volume " + strings.Join(claims, " and ")
	which := "If that is the one, "
	if len(claims) > 1 {
		mounts = "The container mounts volumes " + strings.Join(claims, " and ")
		which = "If one of those is the one, "
	}

	return "It hit a read-only filesystem. " + mounts + ". " + which + act +
		" clears it, which a container restart cannot do because the mount belongs to the pod"
}
