package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/kip/internal/deployer"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

var serviceRestartCmd = &cobra.Command{
	Use:   "restart [name]",
	Short: "Restart a stateful service by recreating its pod",
	Long: `Recreates the service's pod, which is what a container restart cannot do.

A filesystem that remounted read-only underneath a running service stays that
way however many times its container dies, because the mount belongs to the pod.
Recreating the pod makes kubelet detach and re-attach the volume, and a database
that was crash-looping on a read-only disk comes back cleanly.

The service's data is untouched: the volume is reattached, not recreated.`,
	Args: cobra.ExactArgs(1),
	RunE: runServiceRestart,
}

func runServiceRestart(cmd *cobra.Command, args []string) error {
	serviceName := args[0]

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	// An alert names a namespace rather than a project, so the operator running
	// the command it printed should not have to translate one into the other.
	// A named project still wins, which is how two services sharing a name are
	// told apart.
	namespace := resolveServiceNamespace(cmd)
	if !cmd.Flags().Changed("project") && !cmd.Flags().Changed("environment") {
		found, findErr := findServiceNamespace(context.Background(), k8sClient.Dynamic(), serviceName)
		if findErr != nil {
			return findErr
		}
		namespace = found
	}

	d := &deployer.Deployer{Client: k8sClient.Clientset(), Dynamic: k8sClient.Dynamic()}

	ctx := context.Background()

	fmt.Printf("\n  Restarting %s...\n", serviceName)
	stamp, err := d.RestartWorkload(ctx, manifest.ServiceGVR, "service", namespace, serviceName)
	if err != nil {
		return err
	}

	// Writing the stamp is not the restart. The reconciler that carries it onto
	// the pod template is what recreates the pod, and a cluster running an
	// older console-api ignores the annotation entirely: the write succeeds,
	// nothing happens, and reporting success here would be a false green at the
	// exact moment somebody is recovering from one.
	read := statefulSetStamp(k8sClient.Clientset(), namespace, serviceName)
	if err := confirmRestart(ctx, read, serviceName, stamp, confirmRestartWithin); err != nil {
		fmt.Printf("  ✗  %v\n\n", err)
		// "could not confirm", not "was not restarted": a read that failed
		// throughout the window, or a controller that acted a second later,
		// both land here and the pod may well have rolled.
		return fmt.Errorf("could not confirm %s was restarted", serviceName)
	}

	fmt.Printf("  ✔  Restart triggered\n")
	fmt.Printf("     The pod is recreated and its volume reattached. Watch it with: kip service list\n\n")

	return nil
}

// findServiceNamespace locates the one namespace holding a Service CR of this
// name. Two projects can each run a service called db, so an ambiguous name is
// an error naming both rather than a guess that restarts the wrong database.
func findServiceNamespace(ctx context.Context, dyn dynamic.Interface, name string) (string, error) {
	if dyn == nil {
		return "", fmt.Errorf("no cluster connection to look for service %q", name)
	}

	list, err := dyn.Resource(manifest.ServiceGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("looking for service %q: %w", name, err)
	}

	var found []string
	for i := range list.Items {
		if list.Items[i].GetName() == name {
			found = append(found, list.Items[i].GetNamespace())
		}
	}

	switch len(found) {
	case 0:
		return "", fmt.Errorf("no service %q on this cluster", name)
	case 1:
		return found[0], nil
	default:
		sort.Strings(found) // the same two namespaces must read the same way twice
		return "", fmt.Errorf("service %q exists in %s; name one with --project (and --environment)",
			name, strings.Join(found, " and "))
	}
}

// confirmRestartWithin is how long the reconciler is given. It watches the CR,
// so this is one reconcile plus the round trip, not a rollout: the pod takes
// longer to come back and that is not what is being waited for.
const confirmRestartWithin = 20 * time.Second

// templateStamp reads the restart stamp currently on a workload's pod template.
type templateStamp func(ctx context.Context) (string, error)

// statefulSetStamp reads it from the StatefulSet behind a service.
func statefulSetStamp(client kubernetes.Interface, namespace, name string) templateStamp {
	return func(ctx context.Context) (string, error) {
		sts, err := client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		return sts.Spec.Template.Annotations["kipper.run/restartedAt"], nil
	}
}

// confirmRestart waits for the restart stamp to reach the workload's pod
// template, which is the thing that actually recreates the pod.
//
// A cluster whose console-api predates this feature writes the annotation to
// the Service CR and does nothing with it, so without this the command reports
// a recovery that never happened.
//
// The deadline is on the context, not only on the loop: an API server that
// accepts the connection and then stops answering would otherwise hold this
// open for good, because the loop's own check is never reached.
func confirmRestart(parent context.Context, read templateStamp, name string, stamp string, within time.Duration) error {
	ctx, cancel := context.WithTimeout(parent, within)
	defer cancel()

	var lastErr error
	for {
		current, err := read(ctx)
		switch {
		case err != nil:
			lastErr = err
		case current == stamp:
			return nil
		default:
			lastErr = nil
		}

		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("could not read the workload behind %s to confirm the restart: %w", name, lastErr)
			}
			// What was observed, then the likely cause, and not the other way
			// round. A controller that is merely behind produces this same
			// timeout, and telling somebody mid-incident to upgrade a cluster
			// that is working would be worse advice than none.
			return fmt.Errorf("the restart did not reach %s within %s. Most often that is a console-api predating 'kip service restart', which 'kip upgrade' fixes. A busy controller looks the same, so check 'kip status' before upgrading", name, within)
		case <-time.After(500 * time.Millisecond):
		}
	}
}
