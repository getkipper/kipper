package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/getkipper/kipper/kip/internal/k8s"
)

// ttySizeQueue feeds remote terminal-size updates to the exec stream.
// It queues the initial stdout terminal size so the remote shell can render
// its prompt at the right width. On Unix, SIGWINCH queues updated sizes.
// Unavailable sizes and updates received while the queue is full are skipped.
type ttySizeQueue struct {
	ch chan remotecommand.TerminalSize
}

func newTTYSizeQueue() *ttySizeQueue {
	q := &ttySizeQueue{ch: make(chan remotecommand.TerminalSize, 4)}
	q.push()
	notifyOnResize(q.push)
	return q
}

func (q *ttySizeQueue) push() {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return
	}
	select {
	case q.ch <- remotecommand.TerminalSize{Width: uint16(w), Height: uint16(h)}: //nolint:gosec // terminal sizes never exceed uint16
	default:
	}
}

func (q *ttySizeQueue) Next() *remotecommand.TerminalSize {
	size, ok := <-q.ch
	if !ok {
		return nil
	}
	return &size
}

var execCmd = &cobra.Command{
	Use:   "exec [name] [-- command]",
	Short: "Open a shell or run a command in a pod",
	Long: `Opens an interactive shell in a running app, function, or service pod.
Optionally run a specific command instead of an interactive shell.

The name must identify one workload. If it matches several, kip lists the
matches and stops so you can choose the intended workload.

Examples:
  kip exec myapp                          # Open shell in app pod
  kip exec mydb                           # Open shell in database pod
  kip exec myapp -- cat /app/config.php   # View a file
  kip exec myapp -- ls /var/www/html      # List directory
  kip exec api --project shop --environment prod
  kip exec api --kind service             # When an app and a service share a name`,
	Args: cobra.MinimumNArgs(1),
	RunE: runExec,
}

func init() {
	execCmd.Flags().String("project", "", "project name")
	execCmd.Flags().String("environment", "", "target environment")
	execCmd.Flags().String("kind", "", "workload kind: app, function, or service")

	rootCmd.AddCommand(execCmd)
}

func runExec(cmd *cobra.Command, args []string) error {
	name := args[0]

	var command []string
	if cmd.ArgsLenAtDash() > 0 {
		command = args[cmd.ArgsLenAtDash():]
	}
	if len(command) == 0 {
		command = []string{"/bin/sh"}
	}

	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	// Require a unique workload to avoid opening a shell in the wrong target.
	// Allow running pods that are unready so they can be debugged.
	request, err := workloadTargetFlags(cmd, cluster, name, acceptUnready)
	if err != nil {
		return err
	}

	ctx := context.Background()
	clientset := k8sClient.Clientset()

	target, err := request.resolve(ctx, clientset, k8sClient.Dynamic(), cluster)
	if err != nil {
		return err
	}
	namespace := target.candidate.namespace
	podName := target.pod.Name
	containerName := workloadContainerName(target.pod, name)

	config, err := k8s.RESTConfig(cluster)
	if err != nil {
		return err
	}

	stdinFd := int(os.Stdin.Fd())
	interactive := term.IsTerminal(stdinFd)
	options := execOptions(containerName, command, interactive)
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&options, scheme.ParameterCodec)

	executor, err := k8s.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("creating executor: %w", err)
	}

	// Keep connection status out of captured command output.
	if interactive {
		fmt.Fprintf(os.Stderr, "  Connecting to %s/%s...\n\n", namespace, podName)
	}

	// Raw mode lets the remote shell handle echo, line buffering and escape
	// sequences (cursor queries and control keys) directly. Restore the local
	// terminal on return. Non-terminal input can stream without raw mode.
	var sizeQueue remotecommand.TerminalSizeQueue
	if interactive {
		oldState, err := term.MakeRaw(stdinFd)
		if err != nil {
			return fmt.Errorf("putting terminal into raw mode: %w", err)
		}
		defer func() { _ = term.Restore(stdinFd, oldState) }()
		sizeQueue = newTTYSizeQueue()
	}

	return executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:             os.Stdin,
		Stdout:            os.Stdout,
		Stderr:            os.Stderr,
		Tty:               options.TTY,
		TerminalSizeQueue: sizeQueue,
	})
}

// execOptions enables stdin, stdout and stderr and requests a TTY for
// interactive sessions. The caller determines whether stdin is a terminal.
// Non-interactive sessions use streams so piped or empty stdin can close
// independently of the command's output.
func execOptions(container string, command []string, interactive bool) corev1.PodExecOptions {
	return corev1.PodExecOptions{
		Container: container,
		Command:   command,
		Stdin:     true,
		Stdout:    true,
		Stderr:    true,
		TTY:       interactive,
	}
}
