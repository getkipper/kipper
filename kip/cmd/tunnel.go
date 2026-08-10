package cmd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

var tunnelCmd = &cobra.Command{
	Use:   "tunnel [name]",
	Short: "Create a local port forward to a pod",
	Long: `Opens a tunnel from your local machine to a service or app in the cluster.
Connect your database client, browser, or any tool to localhost.

The name must identify one workload. Where it matches several, kip says which
and stops, rather than choosing one for you.

Examples:
  kip tunnel mydb                    # Forward PostgreSQL on localhost:5432
  kip tunnel mydb --local-port 15432 # Custom local port
  kip tunnel frontend --port 3000    # Forward to app on port 3000
  kip tunnel cache                   # Forward Redis on localhost:6379
  kip tunnel api --project shop --environment prod
  kip tunnel api --kind service      # When an app and a service share a name`,
	Args: cobra.ExactArgs(1),
	RunE: runTunnel,
}

func init() {
	tunnelCmd.Flags().Int("local-port", 0, "local port (defaults to the remote port)")
	tunnelCmd.Flags().Int("port", 0, "remote container port (auto-detected if not set)")
	tunnelCmd.Flags().String("project", "", "project name")
	tunnelCmd.Flags().String("environment", "", "target environment")
	tunnelCmd.Flags().String("kind", "", "workload kind: app, function, or service")

	rootCmd.AddCommand(tunnelCmd)
}

func runTunnel(cmd *cobra.Command, args []string) error {
	name := args[0]
	localPort, _ := cmd.Flags().GetInt("local-port")
	remotePort, _ := cmd.Flags().GetInt("port")

	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	// A tunnel that lands on someone else's database is the failure this guards
	// against, so an unresolvable name is refused rather than resolved by list
	// order. tunnel wants a Ready pod: forwarding to one that cannot serve
	// produces a connection failure that reads as a broken app.
	request, err := workloadTargetFlags(cmd, cluster, name, preferReady)
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
	containerPort := target.containerPort()

	if remotePort > 0 {
		containerPort = int32(remotePort) //nolint:gosec // port range validated by flag type
	}
	if containerPort == 0 {
		containerPort = 8080
	}
	if localPort == 0 {
		localPort = int(containerPort)
	}

	// Build the port-forward
	kubeconfigPath := cluster.Kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return fmt.Errorf("loading kubeconfig: %w", err)
	}

	transport, upgrader, err := spdy.RoundTripperFor(config)
	if err != nil {
		return fmt.Errorf("creating transport: %w", err)
	}

	url := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(namespace).
		Name(podName).
		SubResource("portforward").
		URL()

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", url)

	ports := []string{fmt.Sprintf("%d:%d", localPort, containerPort)}
	stopChan := make(chan struct{}, 1)
	readyChan := make(chan struct{})

	fw, err := portforward.New(dialer, ports, stopChan, readyChan, os.Stdout, os.Stderr)
	if err != nil {
		return fmt.Errorf("creating port forward: %w", err)
	}

	// Handle Ctrl+C
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		close(stopChan)
	}()

	fmt.Printf("\n  Forwarding localhost:%d → %s/%s:%d\n", localPort, namespace, podName, containerPort)
	fmt.Printf("  Press Ctrl+C to stop\n\n")

	// Suggest connection string based on service type
	ssList, _ := clientset.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "kipper.run/service-type",
	})
	for _, ss := range ssList.Items {
		if ss.Name == name {
			svcType := ss.Labels["kipper.run/service-type"]
			printConnectionHint(svcType, localPort)
			break
		}
	}

	return fw.ForwardPorts()
}

func printConnectionHint(svcType string, port int) {
	switch svcType {
	case "postgres":
		fmt.Printf("  Connect: psql -h localhost -p %d -U kipper -d app\n\n", port)
	case "mysql":
		fmt.Printf("  Connect: mysql -h 127.0.0.1 -P %d -u kipper -p app\n\n", port)
	case "redis":
		fmt.Printf("  Connect: redis-cli -h localhost -p %d\n\n", port)
	case "mongodb":
		fmt.Printf("  Connect: mongosh mongodb://kipper@localhost:%d/app\n\n", port)
	case "minio":
		fmt.Printf("  Console: http://localhost:%d\n\n", port)
	default:
		hint := strings.ToUpper(svcType)
		fmt.Printf("  %s available at localhost:%d\n\n", hint, port)
	}
}
