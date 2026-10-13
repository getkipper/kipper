package cmd

import (
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
)

type logRequest struct {
	Follow bool
	Tail   int64
}

// addLogFlags defaults to finite output so scripts can wait for completion.
func addLogFlags(cmd *cobra.Command, defaultTail int) {
	cmd.Flags().BoolP("follow", "f", false, "keep streaming new log lines until every stream ends or Ctrl+C")
	cmd.Flags().Int("tail", defaultTail, "number of lines to show from the end of the logs")
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
