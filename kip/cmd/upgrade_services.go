package cmd

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/getkipper/kipper/controller/pkg/labels"
)

const serviceSnapshotTimeout = 30 * time.Second

const consoleAPILeaseName = "kipper-console-api-controllers"

// serviceWait waits for the services that were ready before an upgrade to be
// updated by the new console-api and ready again. Writer returns the rollout's
// controller Lease identity, or "" if no matching pod holds it.
type serviceWait struct {
	List     func(context.Context) ([]appsv1.StatefulSet, error)
	Writer   func(context.Context) (string, error)
	Now      func() time.Time
	Sleep    func(context.Context, time.Duration)
	Interval time.Duration
	Timeout  time.Duration
}

// newServiceWait configures a wait for the console-api rollout with pod template
// hash rolled. It polls every 10 seconds for up to five minutes.
func newServiceWait(clientset kubernetes.Interface, rolled string) serviceWait {
	return serviceWait{
		List: func(ctx context.Context) ([]appsv1.StatefulSet, error) {
			list, err := clientset.AppsV1().StatefulSets("").List(ctx, metav1.ListOptions{LabelSelector: serviceTypeLabel})
			if err != nil {
				return nil, err
			}
			return list.Items, nil
		},
		Writer: func(ctx context.Context) (string, error) {
			return consoleAPIWriter(ctx, clientset, rolled)
		},
		Now: time.Now,
		Sleep: func(ctx context.Context, d time.Duration) {
			select {
			case <-ctx.Done():
			case <-time.After(d):
			}
		},
		Interval: 10 * time.Second,
		Timeout:  5 * time.Minute,
	}
}

// Ready returns namespace/name keys for services with at least one desired
// replica whose pods are all ready and up to date. It limits the listing to
// 30 seconds and returns listing errors. An empty map means no services qualify.
func (w serviceWait) Ready(ctx context.Context) (map[string]bool, error) {
	ctx, cancel := context.WithTimeout(ctx, serviceSnapshotTimeout)
	defer cancel()
	sets, err := w.List(ctx)
	if err != nil {
		return nil, err
	}
	ready := map[string]bool{}
	for _, sts := range sets {
		if serviceRolledOut(sts) {
			ready[sts.Namespace+"/"+sts.Name] = true
		}
	}
	return ready, nil
}

// Wait reports when services marked true in before have been updated by the
// rollout's current controller Lease holder and are ready again. It skips
// deleted services and services scaled to zero, and writes nothing if before
// is empty. It polls until completion, Timeout, or context cancellation.
// Failures are reported without failing the upgrade because the console and
// its data have already been upgraded.
func (w serviceWait) Wait(ctx context.Context, out io.Writer, before map[string]bool) {
	if len(before) == 0 {
		return
	}
	_, _ = fmt.Fprintf(out, "\n  ...  Services\n")
	ctx, cancel := context.WithTimeout(ctx, w.Timeout)
	defer cancel()
	deadline := w.Now().Add(w.Timeout)
	for {
		writer, err := w.Writer(ctx)
		var sets []appsv1.StatefulSet
		if err == nil && writer != "" {
			sets, err = w.List(ctx)
		}
		var waiting []string
		ready := 0
		if err == nil && writer != "" {
			for _, sts := range sets {
				name := sts.Namespace + "/" + sts.Name
				// A service stopped or fenced during the wait is down on purpose.
				if !before[name] || ptr.Deref(sts.Spec.Replicas, 1) == 0 {
					continue
				}
				if sts.Annotations[labels.AnnoWrittenBy] == writer && serviceRolledOut(sts) {
					ready++
				} else {
					waiting = append(waiting, name)
				}
			}
			if len(waiting) == 0 {
				_, _ = fmt.Fprintf(out, "  ✔  %d %s ready\n", ready, plural(ready, "service", "services"))
				return
			}
		}
		if ctx.Err() != nil || !w.Now().Before(deadline) {
			switch {
			case err != nil:
				_, _ = fmt.Fprintf(out, "  ⚠  Services not checked (%v)\n", err)
			case writer == "":
				_, _ = fmt.Fprintf(out, "  ⚠  Services not checked: the new console-api was not running its controllers after %s\n", w.Timeout)
				_, _ = fmt.Fprintf(out, "     Check them with 'kip status'\n")
			default:
				sort.Strings(waiting)
				_, _ = fmt.Fprintf(out, "  ⚠  Not updated and ready %s after the upgrade: %s\n", w.Timeout, strings.Join(waiting, ", "))
				_, _ = fmt.Fprintf(out, "     Check them with 'kip status'\n")
			}
			return
		}
		w.Sleep(ctx, w.Interval)
	}
}

// consoleAPIWriter returns the controller Lease holder's identity when its pod
// UID matches the holder identity, its template hash matches rolled, and the
// pod is not terminating. It returns an empty string for a missing Lease or
// pod or a mismatched holder, and propagates other API errors. It does not
// check Lease expiration.
func consoleAPIWriter(ctx context.Context, clientset kubernetes.Interface, rolled string) (string, error) {
	lease, err := clientset.CoordinationV1().Leases(kipperSystemNS).Get(ctx, consoleAPILeaseName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	holder := ptr.Deref(lease.Spec.HolderIdentity, "")
	podName, podUID, _ := strings.Cut(holder, "_")
	if podName == "" {
		return "", nil
	}
	pod, err := clientset.CoreV1().Pods(kipperSystemNS).Get(ctx, podName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if string(pod.UID) != podUID || pod.DeletionTimestamp != nil || pod.Labels[podTemplateHashLabel] != rolled {
		return "", nil
	}
	return holder, nil
}

// serviceRolledOut reports whether every replica of sts runs its latest
// template and is ready. UpdatedReplicas counts pods on the latest revision
// under both update strategies.
func serviceRolledOut(sts appsv1.StatefulSet) bool {
	want := ptr.Deref(sts.Spec.Replicas, 1)
	s := sts.Status
	return want > 0 &&
		s.ObservedGeneration >= sts.Generation &&
		s.UpdatedReplicas >= want && s.ReadyReplicas >= want
}
