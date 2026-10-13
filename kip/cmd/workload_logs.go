package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"sync"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type logRequest struct {
	Follow bool
	Tail   int64
}

// addLogFlags defaults to finite output so scripts can wait for completion.
func addLogFlags(cmd *cobra.Command, defaultTail int) {
	cmd.Flags().BoolP("follow", "f", false, "keep streaming new log lines until every stream ends or Ctrl+C")
	cmd.Flags().Int("tail", defaultTail, "number of lines to show from the end of each pod's log")
}

func readLogFlags(cmd *cobra.Command) logRequest {
	follow, _ := cmd.Flags().GetBool("follow")
	tail, _ := cmd.Flags().GetInt("tail")
	return logRequest{Follow: follow, Tail: int64(tail)}
}

func podLogOptions(pod *corev1.Pod, workload string, req logRequest) *corev1.PodLogOptions {
	tail := req.Tail
	return &corev1.PodLogOptions{
		Container: workloadContainerName(pod, workload),
		Follow:    req.Follow,
		TailLines: &tail,
	}
}

// podLogOpener lets tests supply distinct log streams for each pod.
type podLogOpener func(ctx context.Context, podName string, opts *corev1.PodLogOptions) (io.ReadCloser, error)

func clientsetLogOpener(client kubernetes.Interface, namespace string) podLogOpener {
	return func(ctx context.Context, podName string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
		return client.CoreV1().Pods(namespace).GetLogs(podName, opts).Stream(ctx)
	}
}

func listWorkloadPods(ctx context.Context, client kubernetes.Interface, namespace, workload string) ([]corev1.Pod, error) {
	list, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", workload),
	})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// podsOldestFirst puts older pods before their replacements during a rollout.
func podsOldestFirst(pods []corev1.Pod) []corev1.Pod {
	sorted := slices.Clone(pods)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i].CreationTimestamp, sorted[j].CreationTimestamp
		if !a.Equal(&b) {
			return a.Before(&b)
		}
		return sorted[i].Name < sorted[j].Name
	})
	return sorted
}

func appLogs(ctx context.Context, client kubernetes.Interface, namespace, app string, req logRequest, out, errOut io.Writer) error {
	pods, err := listWorkloadPods(ctx, client, namespace, app)
	if err != nil {
		return fmt.Errorf("listing pods: %w", err)
	}
	if len(pods) == 0 {
		return fmt.Errorf("no pods found for app %q", app)
	}
	return printPodLogs(ctx, clientsetLogOpener(client, namespace), pods, app, req, out, errOut)
}

func functionLogs(ctx context.Context, client kubernetes.Interface, namespace, function string, req logRequest, out, errOut io.Writer) error {
	pods, err := listWorkloadPods(ctx, client, namespace, function)
	if err != nil {
		return fmt.Errorf("listing pods: %w", err)
	}
	if len(pods) == 0 {
		return fmt.Errorf("no running pods for function %q: it may be scaled to zero", function)
	}
	return printPodLogs(ctx, clientsetLogOpener(client, namespace), pods, function, req, out, errOut)
}

// printPodLogs groups output by pod, oldest first, or reads concurrently when
// following. Multiple pods get a prefix on each line to identify its source.
// Read failures go to errOut; the call fails if all pods fail or out fails.
// Only the supplied pods are followed, including during rollouts.
func printPodLogs(ctx context.Context, open podLogOpener, pods []corev1.Pod, workload string, req logRequest, out, errOut io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	pods = podsOldestFirst(pods)
	w := &podLogWriter{out: out, errOut: errOut, prefixed: len(pods) > 1}
	failures := make([]error, len(pods))
	read := func(i int) {
		pod := &pods[i]
		opts := podLogOptions(pod, workload, req)
		failures[i] = w.copyPod(ctx, open, pod.Name, opts)
		if w.writeFailed() {
			cancel()
		}
	}

	if req.Follow {
		var wg sync.WaitGroup
		for i := range pods {
			wg.Add(1)
			go func() {
				defer wg.Done()
				read(i)
			}()
		}
		wg.Wait()
	} else {
		for i := range pods {
			read(i)
			if w.writeFailed() {
				break
			}
		}
	}

	if err := w.writeErr; err != nil {
		return err
	}
	var failed []error
	for _, err := range failures {
		if err != nil {
			failed = append(failed, err)
		}
	}
	if len(failed) == len(pods) {
		return fmt.Errorf("no logs could be read for %q: %w", workload, errors.Join(failed...))
	}
	return nil
}

// podLogWriter keeps concurrent writes on separate lines and records the first
// output error so printPodLogs can cancel the streams.
type podLogWriter struct {
	mu       sync.Mutex
	out      io.Writer
	errOut   io.Writer
	prefixed bool
	writeErr error
}

func (w *podLogWriter) writeFailed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writeErr != nil
}

// copyPod reports stream errors with the pod name. Output errors are recorded
// separately so cancellation preserves the original write error.
func (w *podLogWriter) copyPod(ctx context.Context, open podLogOpener, podName string, opts *corev1.PodLogOptions) error {
	stream, err := open(ctx, podName, opts)
	if err == nil {
		defer func() { _ = stream.Close() }()
		err = w.copyLines(stream, fmt.Sprintf("[pod/%s/%s] ", podName, opts.Container))
	}
	if err == nil || w.writeFailed() {
		return nil
	}
	err = fmt.Errorf("pod/%s: %w", podName, err)
	w.mu.Lock()
	_, _ = fmt.Fprintf(w.errOut, "warning: failed to read logs from %v\n", err)
	w.mu.Unlock()
	return err
}

func (w *podLogWriter) copyLines(stream io.Reader, prefix string) error {
	reader := bufio.NewReader(stream)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			if err := w.writeLine(line, prefix); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func (w *podLogWriter) writeLine(line []byte, prefix string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.writeErr != nil {
		return w.writeErr
	}
	var err error
	if w.prefixed {
		_, err = io.WriteString(w.out, prefix)
		if err == nil {
			_, err = w.out.Write(line)
		}
		// The next pod's prefix must start a line of its own.
		if err == nil && line[len(line)-1] != '\n' {
			_, err = io.WriteString(w.out, "\n")
		}
	} else {
		_, err = w.out.Write(line)
	}
	if err != nil {
		w.writeErr = err
	}
	return err
}
