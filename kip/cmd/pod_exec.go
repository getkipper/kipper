package cmd

import (
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/getkipper/kipper/kip/internal/k8s"
)

// podExecFunc runs a command in a container, streaming stdin, stdout and stderr.
type podExecFunc func(ctx context.Context, command []string, stdin io.Reader, stdout, stderr io.Writer) error

// podExec binds command execution to a container. Each call opens a new SPDY
// stream and returns setup or streaming errors. A nil stdin disables input.
func podExec(clientset kubernetes.Interface, restConfig *rest.Config, namespace, pod, container string) podExecFunc {
	return func(ctx context.Context, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
		req := clientset.CoreV1().RESTClient().Post().
			Resource("pods").
			Name(pod).
			Namespace(namespace).
			SubResource("exec").
			VersionedParams(&corev1.PodExecOptions{
				Container: container,
				Command:   command,
				Stdin:     stdin != nil,
				Stdout:    true,
				Stderr:    true,
			}, scheme.ParameterCodec)

		executor, err := k8s.NewSPDYExecutor(restConfig, "POST", req.URL())
		if err != nil {
			return fmt.Errorf("creating executor: %w", err)
		}
		return executor.StreamWithContext(ctx, remotecommand.StreamOptions{
			Stdin:  stdin,
			Stdout: stdout,
			Stderr: stderr,
		})
	}
}
