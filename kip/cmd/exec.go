package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

// ttySizeQueue feeds remote terminal-size updates to the exec stream.
// Initial size is sent immediately so the remote shell renders its
// prompt at the right width. SIGWINCH on the local TTY pushes new
// sizes onto the channel as the user resizes their window. Without
// this the shell's prompt-redraw queries (e.g. ESC[6n "where's the
// cursor?") get printed as text instead of being interpreted by the
// terminal, giving "^[[47;8R"-style garbage.
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

The name must identify one workload. Where it matches several, kip says which
and stops, rather than choosing one for you.

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

	// Everything after -- is the command to run
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

	// A shell that opens somewhere other than where the operator meant is the
	// failure this guards against, so an unresolvable name is refused rather
	// than resolved by list order. exec accepts a running-but-unready pod:
	// debugging one is a legitimate reason to want a shell in it.
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

	// Build the exec request
	kubeconfigPath := cluster.Kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return fmt.Errorf("loading kubeconfig: %w", err)
	}

	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   command,
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
			TTY:       true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("creating executor: %w", err)
	}

	fmt.Printf("  Connecting to %s/%s...\n\n", namespace, podName)

	// Put the local terminal into raw mode so keys go through to the
	// remote shell without local echo or line-buffering, and so escape
	// sequences (cursor queries, control keys) don't get processed
	// twice. Restored on exit. If stdin isn't a TTY (piped input,
	// non-interactive), skip raw mode and just stream.
	stdinFd := int(os.Stdin.Fd())
	var sizeQueue remotecommand.TerminalSizeQueue
	if term.IsTerminal(stdinFd) {
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
		Tty:               true,
		TerminalSizeQueue: sizeQueue,
	})
}
