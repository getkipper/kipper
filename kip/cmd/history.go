package cmd

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/webhook"
)

var appHistoryCmd = &cobra.Command{
	Use:   "history [app-name]",
	Short: "Show deployment history for an app",
	Args:  cobra.ExactArgs(1),
	RunE:  runAppHistory,
}

var appRollbackCmd = &cobra.Command{
	Use:   "rollback [app-name]",
	Short: "Rollback an app to a previous version",
	Long: `Rollback an app to a previous version. Without --revision, rolls back
to the immediately previous version. With --revision, rolls back to a
specific revision number from the deploy history.`,
	Args: cobra.ExactArgs(1),
	RunE: runAppRollback,
}

func init() {
	appHistoryCmd.Flags().String("project", "", "project name")
	appHistoryCmd.Flags().String("environment", "", "target environment")

	appRollbackCmd.Flags().String("project", "", "project name")
	appRollbackCmd.Flags().String("environment", "", "target environment")
	appRollbackCmd.Flags().Int("revision", 0, "target revision number (default: previous)")

	appCmd.AddCommand(appHistoryCmd)
	appCmd.AddCommand(appRollbackCmd)
}

func runAppHistory(cmd *cobra.Command, args []string) error {
	appName := args[0]

	ns, k8sClient, err := resolveAppNamespace(cmd, appName)
	if err != nil {
		return err
	}

	ctx := context.Background()
	history, err := webhook.GetHistory(ctx, k8sClient.Dynamic(), ns, appName)
	if err != nil {
		return err
	}

	if len(history) == 0 {
		fmt.Printf("\n  No deploy history for %s\n\n", appName)
		return nil
	}

	fmt.Printf("\n  %-6s %-50s %-10s %-12s %s\n", "#", "IMAGE", "COMMIT", "TRIGGER", "WHEN")
	for i, entry := range history {
		when := formatTimeAgo(entry.Timestamp)
		commit := entry.Commit
		if len(commit) > 8 {
			commit = commit[:8]
		}
		if commit == "" {
			commit = "-"
		}

		marker := ""
		if i == 0 {
			marker = " (current)"
		}

		image := entry.Image
		if len(image) > 48 {
			image = "..." + image[len(image)-45:]
		}

		fmt.Printf("  %-6s %-50s %-10s %-12s %s%s\n",
			strconv.Itoa(entry.Revision), image, commit, entry.Trigger, when, marker)
	}
	fmt.Println()

	return nil
}

func runAppRollback(cmd *cobra.Command, args []string) error {
	appName := args[0]
	revision, _ := cmd.Flags().GetInt("revision")

	ns, k8sClient, err := resolveAppNamespace(cmd, appName)
	if err != nil {
		return err
	}

	ctx := context.Background()
	target, err := webhook.Rollback(ctx, k8sClient.Dynamic(), ns, appName, revision)
	if err != nil {
		return err
	}

	fmt.Printf("\n  ✔  Rolled back %s to revision #%d (%s)\n\n", appName, target.Revision, target.Image)
	return nil
}

func formatTimeAgo(timestamp string) string {
	t, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return timestamp
	}

	d := time.Since(t).Truncate(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}
