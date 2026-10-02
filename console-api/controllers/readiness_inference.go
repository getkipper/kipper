package controllers

import (
	"context"
	"errors"
	"net"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

const (
	inferenceDialTimeout = 2 * time.Second
	inferenceMaxPods     = 3
	// Treat a refused connection as evidence of a closed port only after
	// the app container has had this long to start.
	inferenceSettledAge = 10 * time.Minute
)

// TCPDial opens a TCP connection to addr and closes it again.
func TCPDial(ctx context.Context, addr string) error {
	d := net.Dialer{Timeout: inferenceDialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

func connectionRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}

// inferReadiness samples up to three running pods. A ready pod accepting
// a connection selects TCP. Otherwise, a refusal from a container running
// for at least ten minutes selects no check. Other results leave it pending.
func (r *AppReconciler) inferReadiness(ctx context.Context, app *kipperv1.App, deploy *appsv1.Deployment, now time.Time) inference {
	if r.Dial == nil {
		return inferencePending
	}
	pods, err := ownedPods(ctx, r.hostReader(), deploy)
	if err != nil {
		log.FromContext(ctx).Error(err, "listing pods to learn whether the app port accepts connections", "app", app.Name)
		return inferencePending
	}
	candidates := inferenceCandidates(app, pods)

	type answer struct {
		accepted, settledRefusal bool
	}
	answers := make([]answer, len(candidates))
	var wg sync.WaitGroup
	for i := range candidates {
		wg.Add(1)
		go func(i int, p *corev1.Pod) {
			defer wg.Done()
			addr := net.JoinHostPort(p.Status.PodIP, strconv.Itoa(int(app.Spec.Port)))
			err := r.Dial(ctx, addr)
			switch {
			case err == nil:
				answers[i].accepted = podReady(p)
			case connectionRefused(err):
				since := runningSince(p, app.Name)
				answers[i].settledRefusal = !since.IsZero() && now.Sub(since) >= inferenceSettledAge
			}
		}(i, candidates[i])
	}
	wg.Wait()

	result := inferencePending
	for _, a := range answers {
		if a.accepted {
			return inferredTCP
		}
		if a.settledRefusal {
			result = inferredNone
		}
	}
	return result
}

// inferenceCandidates excludes terminating pods, placeholders, and pods
// without an IP address. It prioritizes ready pods, then the oldest running
// app containers, and returns at most three candidates.
func inferenceCandidates(app *kipperv1.App, pods []corev1.Pod) []*corev1.Pod {
	var out []*corev1.Pod
	for i := range pods {
		p := &pods[i]
		if p.Status.Phase != corev1.PodRunning || p.DeletionTimestamp != nil || p.Status.PodIP == "" {
			continue
		}
		if app.Spec.Git != nil && len(p.Spec.Containers) > 0 && p.Spec.Containers[0].Image == "busybox:latest" {
			continue
		}
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if podReady(out[i]) != podReady(out[j]) {
			return podReady(out[i])
		}
		a, b := runningSince(out[i], app.Name), runningSince(out[j], app.Name)
		if a.IsZero() != b.IsZero() {
			return b.IsZero()
		}
		return a.Before(b)
	})
	if len(out) > inferenceMaxPods {
		out = out[:inferenceMaxPods]
	}
	return out
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// runningSince is when the app container last started running, or the zero
// time when it is not running.
func runningSince(p *corev1.Pod, container string) time.Time {
	for _, s := range p.Status.ContainerStatuses {
		if s.Name == container && s.State.Running != nil {
			return s.State.Running.StartedAt.Time
		}
	}
	return time.Time{}
}
