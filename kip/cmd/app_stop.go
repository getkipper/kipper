package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/kip/internal/deployer"
)

var appStopCmd = &cobra.Command{
	Use:   "stop [app-name]",
	Short: "Stop an application until it is started again",
	Long: `Stop an application. Its pods shut down and give their CPU and memory back,
and its route answers with a "this app is stopped" page. Everything else stays:
the replica count, autoscaling, environment, secrets, volumes, bound services and
the route itself. Services and databases the app uses keep running.

Start it again with kip app start.`,
	Example: `  kip app stop shop --reason "not used until the next campaign"`,
	Args:    cobra.ExactArgs(1),
	RunE:    runAppStop,
}

var appStartCmd = &cobra.Command{
	Use:   "start [app-name]",
	Short: "Start a stopped application",
	Long: `Start a stopped application with the replica count and autoscaling it had.
The start is a rollout like any other, so new pods take traffic once they pass
their health check.`,
	Args: cobra.ExactArgs(1),
	RunE: runAppStart,
}

func init() {
	for _, c := range []*cobra.Command{appStopCmd, appStartCmd} {
		c.Flags().String("project", "", "project name")
		c.Flags().String("environment", "", "target environment")
		appCmd.AddCommand(c)
	}
	appStopCmd.Flags().String("reason", "", "why the app is stopped, shown wherever it is listed")
	appStopCmd.Flags().Bool("for-migration", false, "stop the app as the write freeze before a migration; the migration does not carry this stop to the target")
}

func runAppStop(cmd *cobra.Command, args []string) error {
	appName := args[0]
	reason, _ := cmd.Flags().GetString("reason")
	forMigration, _ := cmd.Flags().GetBool("for-migration")
	if len(reason) > 500 {
		return fmt.Errorf("the reason must be at most 500 characters")
	}

	ns, k8sClient, err := resolveAppNamespace(cmd, appName)
	if err != nil {
		return err
	}
	d := &deployer.Deployer{Client: k8sClient.Clientset(), Dynamic: k8sClient.Dynamic()}
	ctx := context.Background()

	fmt.Printf("\n  Stopping %s...\n", appName)
	var reasonOpt *string
	if cmd.Flags().Changed("reason") {
		reasonOpt = &reason
	}
	res, err := d.Stop(ctx, ns, appName, deployer.StopOptions{
		Reason:       reasonOpt,
		By:           stopIdentity(ctx, k8sClient.Clientset(), "kip"),
		At:           time.Now(),
		ForMigration: forMigration,
	})
	if err != nil {
		return err
	}
	if res.Already {
		switch {
		case forMigration && res.Freeze:
			fmt.Printf("  ✔  %s is already stopped for the migration\n\n", appName)
		case forMigration:
			fmt.Printf("  ✔  %s was already stopped, and keeps that stop. A migration carries it to the target, so the app arrives stopped there too.\n\n", appName)
		case reasonOpt != nil:
			fmt.Printf("  ✔  %s was already stopped; the reason is updated\n\n", appName)
		default:
			fmt.Printf("  ✔  %s was already stopped\n\n", appName)
		}
		return nil
	}
	fmt.Printf("  ✔  Stopped. Its pods shut down, and its route answers with the stopped page.\n")
	fmt.Printf("     Bound services and volumes keep running. Start it again with `kip app start %s`.\n\n", appName)
	return nil
}

func runAppStart(cmd *cobra.Command, args []string) error {
	appName := args[0]

	ns, k8sClient, err := resolveAppNamespace(cmd, appName)
	if err != nil {
		return err
	}
	d := &deployer.Deployer{Client: k8sClient.Clientset(), Dynamic: k8sClient.Dynamic()}

	fmt.Printf("\n  Starting %s...\n", appName)
	res, err := d.Start(context.Background(), ns, appName)
	if err != nil {
		return err
	}
	switch {
	case !res.WasStopped:
		fmt.Printf("  ✔  %s is not stopped; nothing to do\n\n", appName)
	case res.RunsNoPods:
		fmt.Printf("  ✔  Started, but %s is scaled to zero replicas, so it runs no pods until `kip app scale %s --replicas N`\n\n", appName, appName)
	default:
		fmt.Printf("  ✔  Started. `kip app list` shows the rollout until the new pods are ready.\n\n")
	}
	return nil
}

// stopIdentity combines the tool name with the authenticated cluster user,
// falling back to the tool name when the user lookup fails.
func stopIdentity(ctx context.Context, cs kubernetes.Interface, tool string) string {
	review, err := cs.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authnv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil || review == nil || review.Status.UserInfo.Username == "" {
		return tool
	}
	return tool + " (" + review.Status.UserInfo.Username + ")"
}

// stoppedNote is the line kip app list prints under the table for a stopped
// app.
func stoppedNote(appName string, info deployer.StoppedInfo) string {
	note := appName + " is stopped"
	if at, err := time.Parse(time.RFC3339, info.At); err == nil {
		note += " since " + at.UTC().Format("2006-01-02 15:04 UTC")
	}
	// Anyone who can update the app writes these, so they are printed
	// without control characters.
	if info.By != "" {
		note += " by " + printable(info.By)
	}
	if info.Reason != "" {
		note += ": " + printable(info.Reason)
	}
	return note
}
