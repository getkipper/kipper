package controller

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/getkipper/kipper/controller/pkg/labels"
)

// How a crash loop is reported over time.
//
// The shape comes from an incident where a database crash-looped for three and
// a half days and produced the same hourly warning eighty-four times. Nobody
// reads the eighty-fourth copy of a message they ignored at the sixth, and the
// alert store caps at fifty, so the flood evicted the first alert of the
// incident — the one whose timestamp said when the volume went read-only.
const (
	// escalateAfter is how long an unresolved crash loop runs before it stops
	// being a warning.
	//
	// Six hours is past every slow operation this platform performs (a restore,
	// a migration, a node drain, an instance-manager restart, all minutes) and
	// past the point where waiting is reasonable: kubelet's backoff caps at
	// five minutes, and nothing recovers on the seventy-third attempt that
	// would not have on the tenth. It also keeps escalation inside waking hours
	// from either end. A failure at 07:00 escalates at 13:00 and one at 23:00
	// at 05:00, where four hours would make 03:00 the common escalation time
	// for an evening failure, and the second 3am alert for something unfixable
	// is the one that gets escalation switched off for good.
	escalateAfter = 6 * time.Hour

	// escalatedRepeat is how often it repeats once escalated. Daily still
	// reaches Monday morning for a Friday-evening failure, and three broken
	// services cost three alerts a day rather than twelve.
	escalatedRepeat = 24 * time.Hour

	// recoveredAfter is how long a container must run clean before the episode
	// is closed. Kubelet forgets its own backoff entry after twice its
	// five-minute cap, so this is Kubernetes' definition of recovered rather
	// than an invented one.
	recoveredAfter = 10 * time.Minute

	// crashLoopCooldown is the existing warning cadence, unchanged: at most one
	// crash-loop alert per container per hour, whatever else happens.
	crashLoopCooldown = 60 * time.Minute
)

const (
	stageWarning  = "warning"
	stageCritical = "critical"
)

// episode is one unresolved crash loop for a container.
//
// "Unresolved" rather than "continuously in CrashLoopBackOff" is the definition
// that survives contact with a real pod. A container between crash attempts is
// briefly Running and not Ready, and is neither crash-looping nor recovered.
// Ending the episode there would let those gaps stop escalation ever firing;
// counting them as crash-loop time would escalate a container that is merely
// slow to start. They do neither: they hold the episode open and restart the
// recovery run.
type episode struct {
	// firstSeen is when this episode began, and what escalation is measured
	// from. Deliberately not the container's RestartCount, which is cumulative
	// for the pod's lifetime: a StatefulSet pod carrying months of occasional
	// restarts would escalate on the first tick of a fresh loop.
	firstSeen time.Time
	// lastAlerted is when this episode last produced an alert.
	lastAlerted time.Time
	// escalatedAt is when it became critical, zero until then. Written only
	// after the escalation alert has been stored, because it decides whether
	// the alert is ever sent again.
	escalatedAt time.Time
	// readySince is when the container last started running clean, zero
	// whenever it is not.
	readySince time.Time
	// lastSeen is refreshed on every observation, and is what the state sweep
	// ages on. Sweeping on firstSeen would prune a live three-day episode at
	// the TTL, which is exactly the episode worth keeping.
	lastSeen time.Time
	// restarts is the workload's total, carried for the message where it is
	// worth reading and checkable against the pods.
	restarts int32
	// restartsByPod is what decides whether a container crashed again, kept per
	// pod rather than as a total: a replica being replaced drops the total, and
	// compared against that a different replica crashing afterwards reads as a
	// smaller number and the crash is missed.
	restartsByPod map[string]int32
}

// stageAt reports how this episode should be reported now, and whether it is
// due to say anything at all.
func (e episode) stageAt(now time.Time) (stage string, alert bool) {
	// A zero firstSeen is a new episode, not an infinitely old one. Without
	// this guard now.Sub(zero) is some seventeen thousand years and every new
	// crash loop opens at critical.
	old := !e.firstSeen.IsZero() && now.Sub(e.firstSeen) >= escalateAfter
	escalated := !e.escalatedAt.IsZero() || old
	stage = stageWarning
	if escalated {
		stage = stageCritical
	}

	// A new episode speaks immediately; there is nothing to repeat yet.
	if e.lastAlerted.IsZero() {
		return stage, true
	}

	quiet := crashLoopCooldown
	if !e.escalatedAt.IsZero() {
		quiet = escalatedRepeat
	}
	// Crossing the threshold speaks at once rather than waiting out the
	// warning cooldown it was already inside.
	if escalated && e.escalatedAt.IsZero() {
		return stage, true
	}
	return stage, now.Sub(e.lastAlerted) >= quiet
}

// observeUnsettled records a container that is neither crash-looping nor
// running clean. The episode stands and its recovery run restarts.
func (e *episode) observeUnsettled(now time.Time) {
	e.lastSeen = now
	e.readySince = time.Time{}
}

// recoveredAt reports whether the container has run clean for long enough to
// close the episode. Measured from a timestamp rather than counted in ticks:
// ten one-minute observations span nine minutes, and a tick delayed by a busy
// controller would shorten it further.
func (e episode) recoveredAt(now time.Time) bool {
	if e.readySince.IsZero() {
		return false
	}
	return now.Sub(e.readySince) >= recoveredAfter
}

// deservesAllClear reports whether closing this episode is worth telling
// anyone. Only an episode that escalated is: somebody was woken for it. A brief
// crash loop that never got past a warning closes quietly, or every transient
// costs two alerts instead of one.
func (e episode) deservesAllClear() bool {
	return !e.escalatedAt.IsZero()
}

// crashLoopAlert builds one crash-loop alert and the state changes it earns.
//
// Every state change is deferred into the batch rather than written here.
// escalatedAt and the episode's deletion decide whether this alert is ever sent
// again, which makes them suppression state: written directly, a failed
// ConfigMap write would leave an episode believing it had escalated while
// nothing was ever delivered, suppressed for good.
//
// Caller holds rc.mu.
func (rc *ResourceController) crashLoopAlert(key string, obs workloadObservation, now time.Time, nowStr string) (alertBatch, bool) {
	ep := rc.crashLoopEpisode[key]
	if ep.firstSeen.IsZero() {
		ep.firstSeen = now
	}
	ep.lastSeen = now
	ep.readySince = time.Time{}
	ep.restarts = obs.restarts
	ep.restartsByPod = obs.counts

	stage, due := ep.stageAt(now)
	if !due {
		rc.crashLoopEpisode[key] = ep
		return alertBatch{}, false
	}

	next := ep
	next.lastAlerted = now
	if stage == stageCritical && next.escalatedAt.IsZero() {
		next.escalatedAt = now
	}

	// A volume that remounted read-only produces a crash loop like any other,
	// and the only place it says so is the log the container wrote as it gave
	// up. It gets its own alert because the remedy differs: the pod has to be
	// recreated, and the restarts Kubernetes is already doing cannot clear a
	// mount.
	// Read across every failing replica, not the one that happened to be listed
	// first: the evidence is in whichever replica's disk went, and which that
	// is must not depend on the order the API returned the pods.
	if evidence, pod := rc.readOnlyVolumeEvidence(obs); evidence != "" {
		// Critical from the first sighting, so the episode is escalated here
		// rather than at six hours. Left at warning stage the critical would
		// repeat hourly, and an operator who recreated the pod inside those six
		// hours would never be told it was over.
		if next.escalatedAt.IsZero() {
			next.escalatedAt = now
		}
		return rc.readOnlyVolumeAlert(key, pod, next, now, nowStr, obs.status.Name, obs.kind, evidence), true
	}

	pod, cs := obs.pod, obs.status
	action := "CrashLoopBackOff"
	reason := fmt.Sprintf("container %q is crash-looping", cs.Name)
	if term := cs.LastTerminationState.Terminated; term != nil {
		reason += fmt.Sprintf(" (last exit code %d)", term.ExitCode)
	}
	if stage == stageCritical {
		action = "CrashLoopBackOff persisting"
		reason = fmt.Sprintf(
			"container %q in namespace %q has been crash-looping for %s (%d restarts). It is not recovering on its own.",
			cs.Name, pod.Namespace, roundedFor(now.Sub(ep.firstSeen)), obs.restarts)
		if cmd := recoveryCommand(pod.Labels); cmd != "" {
			reason += " Recover with: " + cmd
		}
	}

	return alertBatch{
		entry: ResourceLogEntry{
			Time:      nowStr,
			App:       pod.Labels["app"],
			Namespace: pod.Namespace,
			Action:    action,
			Severity:  stage,
			Reason:    reason,
		},
		marks: []pendingMark{{dst: rc.crashLoopAlerted, key: key, at: now}},
		apply: []func(){func() { rc.crashLoopEpisode[key] = next }},
	}, true
}

// observeCrashLoopRecovery watches a container that is no longer waiting, and
// closes its episode once it has run clean for long enough.
//
// Caller holds rc.mu.
func (rc *ResourceController) observeCrashLoopRecovery(key string, obs workloadObservation, now time.Time, nowStr string) (alertBatch, bool) {
	ep, tracked := rc.crashLoopEpisode[key]
	if !tracked {
		return alertBatch{}, false
	}

	// Every replica, not the one that happened to be listed first: a replica
	// running but never becoming ready, or sitting on a bad exit code, is not
	// waiting for anything and is not recovered either.
	running := obs.allClean && ep.stillClean(obs.counts)
	if !running {
		// Running but not Ready, restarting again, or terminated: neither
		// crash-looping nor recovered. The episode stands and the clean run
		// starts over.
		//
		// The counts are taken even so. A container can crash and be back up
		// between two observations, so this is how an unseen crash arrives:
		// the clean run is void, and the count it crashed to is what the next
		// ten minutes are measured from. Left at the old baseline, every later
		// healthy observation would compare against a number it can never
		// reach again and restart the run forever.
		ep.observeUnsettled(now)
		ep.restarts = obs.restarts
		ep.restartsByPod = obs.counts
		rc.crashLoopEpisode[key] = ep
		return alertBatch{}, false
	}

	ep.observeClean(now, obs.counts, obs.restarts)
	rc.crashLoopEpisode[key] = ep

	if !ep.recoveredAt(now) {
		return alertBatch{}, false
	}

	// Closing the episode is deferred like any other alert-driving change: if
	// the all-clear never reaches the store, the episode has to survive so the
	// next tick can send it.
	if !ep.deservesAllClear() {
		delete(rc.crashLoopEpisode, key)
		return alertBatch{}, false
	}

	return alertBatch{
		entry: ResourceLogEntry{
			Time:      nowStr,
			App:       obs.pod.Labels["app"],
			Namespace: obs.pod.Namespace,
			Action:    "CrashLoopBackOff resolved",
			Severity:  "info",
			Reason: fmt.Sprintf("container %q recovered after %s and %d restarts",
				obs.status.Name, roundedFor(now.Sub(ep.firstSeen)), ep.restarts),
		},
		// The cooldown mark is deliberately left alone. Deleting it here would
		// re-arm a first-tick alert, so a container running twelve minutes
		// between crashes would cycle every half hour.
		apply: []func(){func() { delete(rc.crashLoopEpisode, key) }},
	}, true
}

// touchEpisode keeps an episode alive while its alert is inside the cooldown,
// so the six-hour clock keeps running through the quiet hours.
//
// Caller holds rc.mu.
func (rc *ResourceController) touchEpisode(key string, obs workloadObservation, now time.Time) {
	ep, tracked := rc.crashLoopEpisode[key]
	if !tracked {
		ep.firstSeen = now
	}
	ep.lastSeen = now
	ep.readySince = time.Time{}
	ep.restarts = obs.restarts
	ep.restartsByPod = obs.counts
	rc.crashLoopEpisode[key] = ep
}

// roundedFor prints a duration an operator reads rather than one a machine
// emits: 8h12m rather than 8h12m4.019s.
func roundedFor(d time.Duration) string {
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	return d.Round(time.Minute).String()
}

// stillClean reports whether an observed restart count means no new crash since
// the episode last looked.
//
// A count that has not moved is the same container instance, still up. A count
// that has dropped is a different pod under the same name, which is what
// `kip service restart` produces and therefore the shape of the recovery this
// alert asks for. Only an increase is another crash.
// Only the recovery path consults this, and that path runs when no replica of
// the workload is waiting. A container that has just crashed is not Running and
// Ready, so a total that drops because a replica was replaced cannot be read as
// a recovery for one that is still failing.
func (e episode) stillClean(counts map[string]int32) bool {
	for pod, count := range counts {
		baseline, known := e.restartsByPod[pod]
		if !known {
			// A pod nobody has seen before brings its own history. None of
			// those restarts happened since the last look.
			continue
		}
		if count > baseline {
			return false
		}
	}
	return true
}

// observeClean records a healthy observation, starting the recovery run if this
// is the first one and re-baselining the restart count so a later crash on a
// replacement pod is still seen as a crash.
func (e *episode) observeClean(now time.Time, counts map[string]int32, total int32) {
	if e.readySince.IsZero() {
		e.readySince = now
	}
	e.lastSeen = now
	e.restarts = total
	e.restartsByPod = counts
}

// holdEpisode records that a tracked container was observed in some state other
// than running clean: pulling an image, initialising, or crash-looping for a
// reason another path alerts on.
//
// It restarts the recovery run and keeps the episode out of the state sweep,
// without opening one for a container that has never crash-looped. Recovery is
// ten minutes of clean running, and a container that spent part of that window
// waiting has not had them.
//
// Caller holds rc.mu.
func (rc *ResourceController) holdEpisode(key string, now time.Time) {
	ep, tracked := rc.crashLoopEpisode[key]
	if !tracked {
		return
	}
	ep.observeUnsettled(now)
	rc.crashLoopEpisode[key] = ep
}

// recoveryCommand names the command that recreates this workload's pod, which
// is what recovers a volume that remounted read-only: a container restart
// cannot, because the mount belongs to the pod.
//
// The pod scan covers every Kipper-managed workload, and only the service
// controller sets a service type, so that label is what tells a database from
// an application. A pod naming no workload gets no command rather than a guess.
func recoveryCommand(podLabels map[string]string) string {
	name := podLabels["app"]
	if name == "" {
		return ""
	}
	if podLabels[labels.ServiceType] != "" {
		return "kip service restart " + name
	}
	return "kip app restart " + name
}

// containerKind separates an init container from a main one. Kubernetes allows
// them to share a name, and everything keyed on the name alone then merges the
// two: a completed init container would mask a main container that never
// becomes ready, and their volume mounts would run together.
type containerKind string

const (
	initContainer containerKind = "init"
	mainContainer containerKind = "app"
)

// episodeKey identifies a crash-loop episode by the workload rather than by the
// pod.
//
// The recovery these alerts prescribe replaces the pod. A StatefulSet reuses the
// name, so keying by pod name worked for the services in the incident, but a
// Deployment generates a new one: `kip app restart api` turns api-6d8f into
// api-7b9c, and an episode keyed by the old name would never see the healthy
// replacement, never send its all-clear, and quietly age out.
//
// A pod carrying no app label has no workload identity to use, and falls back to
// its own name so two of them cannot collide.
func episodeKey(namespace, podName, appLabel, container string, kind containerKind) string {
	workload := appLabel
	if workload == "" {
		workload = podName
	}
	return namespace + "/" + workload + "/" + string(kind) + ":" + container
}

// failingContainer is one replica's crash-looping container, kept so the
// decisions that are per replica stay per replica.
type failingContainer struct {
	pod    *corev1.Pod
	status *corev1.ContainerStatus
}

// workloadObservation is what one workload's containers looked like on one tick,
// gathered across every replica.
//
// Everything a decision needs is aggregated here rather than read back off a
// representative replica, because which replica the API lists first is not
// something an alert should depend on.
type workloadObservation struct {
	key  string
	kind containerKind
	// pod and status are a representative, used for the message: the failing
	// replica where there is one.
	pod    *corev1.Pod
	status *corev1.ContainerStatus
	// waiting is the most serious Waiting reason across the replicas, or "" when
	// none is waiting.
	waiting string
	// allClean is true when every replica is Running and Ready, or is an init
	// container that completed. Nothing waiting is not the same thing: a replica
	// can be running and never become ready, or sit Terminated on a bad exit
	// code, and neither is a recovery.
	allClean bool
	// restarts is the workload's total, for the message.
	restarts int32
	// counts is the per-pod restart count, which is what tells a new crash from
	// a total that fell because a replica was replaced.
	counts map[string]int32
	// failing is every crash-looping replica, so the OOM classification and the
	// read-only evidence are read across all of them.
	failing []failingContainer
	// pulling is every replica that cannot pull its image. A crash loop and a
	// pull failure are independent problems with independent remedies, and one
	// workload can have both: a replica OOM-looping while its replacement
	// cannot pull a new image. Folded into one waiting reason, whichever came
	// first would hide the other.
	pulling []failingContainer
}

// oomOnly reports whether every crash-looping replica was OOM-killed, which is
// the memory path's business rather than this one's. One replica failing for its
// own reason has to be alerted.
func (o workloadObservation) oomOnly() bool {
	if len(o.failing) == 0 {
		return false
	}
	for _, f := range o.failing {
		term := f.status.LastTerminationState.Terminated
		if term == nil || term.Reason != "OOMKilled" {
			return false
		}
	}
	return true
}

// observeWorkloads reduces every container status across every pod to one
// observation per workload and container, in a stable order.
//
// The episode is keyed by workload, so this is what keeps replicas from writing
// over each other: two failing replicas are one incident and raise one alert,
// and two healthy replicas with different lifetime restart counts no longer take
// turns resetting the recovery clock.
func observeWorkloads(pods []corev1.Pod) []workloadObservation {
	byKey := map[string]*workloadObservation{}
	var order []string

	for i := range pods {
		pod := &pods[i]
		for _, group := range []struct {
			kind     containerKind
			statuses []corev1.ContainerStatus
		}{
			{initContainer, pod.Status.InitContainerStatuses},
			{mainContainer, pod.Status.ContainerStatuses},
		} {
			for j := range group.statuses {
				cs := &group.statuses[j]
				key := episodeKey(pod.Namespace, pod.Name, pod.Labels["app"], cs.Name, group.kind)

				obs, seen := byKey[key]
				if !seen {
					obs = &workloadObservation{
						key: key, kind: group.kind, pod: pod, status: cs,
						allClean: true,
						counts:   map[string]int32{},
					}
					byKey[key] = obs
					order = append(order, key)
				}
				obs.restarts += cs.RestartCount
				obs.counts[pod.Name] = cs.RestartCount
				if !containerIsClean(cs, group.kind) {
					obs.allClean = false
				}

				if cs.State.Waiting == nil {
					continue
				}
				reason := cs.State.Waiting.Reason
				switch reason {
				case "CrashLoopBackOff":
					obs.failing = append(obs.failing, failingContainer{pod: pod, status: cs})
				case "ImagePullBackOff", "ErrImagePull":
					obs.pulling = append(obs.pulling, failingContainer{pod: pod, status: cs})
				}
				// A waiting replica is the one worth describing. A crash loop
				// outranks the other reasons, because it is the one with the
				// escalation ladder and a recovery command behind it; among
				// equals the first found wins, so the choice does not flap
				// between ticks.
				if obs.waiting == "" || (reason == "CrashLoopBackOff" && obs.waiting != "CrashLoopBackOff") {
					obs.waiting = reason
					obs.pod, obs.status = pod, cs
				}
			}
		}
	}

	observations := make([]workloadObservation, 0, len(order))
	for _, key := range order {
		observations = append(observations, *byKey[key])
	}
	return observations
}

// containerIsClean reports whether one container is doing what it should.
//
// An init container that has done its job is Terminated and never Running, and
// succeeding is everything it is ever going to do. Anything else, for a main
// container, means Running and Ready.
func containerIsClean(cs *corev1.ContainerStatus, kind containerKind) bool {
	if kind == initContainer {
		if term := cs.State.Terminated; term != nil && term.ExitCode == 0 {
			return true
		}
	}
	return cs.State.Running != nil && cs.Ready
}
