package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/controller/pkg/secretname"
	"github.com/getkipper/kipper/kip/internal/webhook"
)

var webhookCmd = &cobra.Command{
	Use:   "webhook",
	Short: "Manage deployment webhooks",
}

var webhookEnableCmd = &cobra.Command{
	Use:   "enable [app-name]",
	Short: "Enable webhook-triggered deploys for an app",
	Args:  cobra.ExactArgs(1),
	RunE:  runWebhookEnable,
}

var webhookDisableCmd = &cobra.Command{
	Use:   "disable [app-name]",
	Short: "Disable webhook-triggered deploys for an app",
	Args:  cobra.ExactArgs(1),
	RunE:  runWebhookDisable,
}

var webhookStatusCmd = &cobra.Command{
	Use:   "status [app-name]",
	Short: "Show webhook URL and token for an app",
	Args:  cobra.ExactArgs(1),
	RunE:  runWebhookStatus,
}

func init() {
	webhookEnableCmd.Flags().String("project", "", "project name")
	webhookEnableCmd.Flags().String("environment", "", "target environment")

	webhookDisableCmd.Flags().String("project", "", "project name")
	webhookDisableCmd.Flags().String("environment", "", "target environment")

	webhookStatusCmd.Flags().String("project", "", "project name")
	webhookStatusCmd.Flags().String("environment", "", "target environment")

	webhookCmd.AddCommand(webhookEnableCmd)
	webhookCmd.AddCommand(webhookDisableCmd)
	webhookCmd.AddCommand(webhookStatusCmd)
	appCmd.AddCommand(webhookCmd)
}

func runWebhookEnable(cmd *cobra.Command, args []string) error {
	appName := args[0]

	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	project, _ := cmd.Flags().GetString("project")
	environment, _ := cmd.Flags().GetString("environment")

	var ns string
	if project != "" {
		ns = cluster.ResolveNamespace(project, environment)
	} else {
		ctx := context.Background()
		ns, err = findWorkloadNamespace(ctx, k8sClient.Clientset(), k8sClient.Dynamic(), secretname.KindApp, appName)
		if err != nil {
			return err
		}
	}

	ctx := context.Background()
	token, err := webhook.Enable(ctx, k8sClient.Clientset(), ns, appName)
	if err != nil {
		return err
	}

	webhookURL := fmt.Sprintf("https://%s/api/v1/webhook/%s/%s", cluster.ConsoleHost(), ns, appName)

	fmt.Printf("\n  ✔  Webhook enabled for %s\n\n", appName)
	fmt.Printf("  Webhook URL:    %s\n", webhookURL)
	fmt.Printf("  Secret token:   %s\n\n", token)
	fmt.Printf("  GitLab CI snippet (.gitlab-ci.yml):\n\n")
	fmt.Printf("    deploy:\n")
	fmt.Printf("      stage: deploy\n")
	fmt.Printf("      script:\n")
	fmt.Printf("        - |\n")
	fmt.Printf("          curl -s -X POST %s \\\n", webhookURL)
	fmt.Printf("            -H \"X-Kipper-Token: $KIPPER_WEBHOOK_TOKEN\" \\\n")
	fmt.Printf("            -H \"Content-Type: application/json\" \\\n")
	fmt.Printf("            -d '{\"image\": \"'$CI_REGISTRY_IMAGE:$CI_COMMIT_SHORT_SHA'\", \"commit\": \"'$CI_COMMIT_SHORT_SHA'\"}'\n\n")

	fmt.Printf("  GitHub Actions snippet:\n\n")
	fmt.Printf("    - name: Deploy to Kipper\n")
	fmt.Printf("      run: |\n")
	fmt.Printf("        curl -s -X POST %s \\\n", webhookURL)
	fmt.Printf("          -H \"X-Kipper-Token: ${{ secrets.KIPPER_WEBHOOK_TOKEN }}\" \\\n")
	fmt.Printf("          -H \"Content-Type: application/json\" \\\n")
	fmt.Printf("          -d '{\"image\": \"ghcr.io/${{ github.repository }}:${{ github.sha }}\", \"commit\": \"${{ github.sha }}\"}'\n\n")

	fmt.Printf("  Add the secret token as KIPPER_WEBHOOK_TOKEN in your CI/CD settings.\n\n")

	return nil
}

func runWebhookDisable(cmd *cobra.Command, args []string) error {
	appName := args[0]

	ns, k8sClient, err := resolveAppNamespace(cmd, appName)
	if err != nil {
		return err
	}

	ctx := context.Background()
	if err := webhook.Disable(ctx, k8sClient.Clientset(), ns, appName); err != nil {
		return err
	}

	fmt.Printf("  ✔  Webhook disabled for %s\n", appName)
	return nil
}

func runWebhookStatus(cmd *cobra.Command, args []string) error {
	appName := args[0]

	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	project, _ := cmd.Flags().GetString("project")
	environment, _ := cmd.Flags().GetString("environment")

	var ns string
	if project != "" {
		ns = cluster.ResolveNamespace(project, environment)
	} else {
		ctx := context.Background()
		ns, err = findWorkloadNamespace(ctx, k8sClient.Clientset(), k8sClient.Dynamic(), secretname.KindApp, appName)
		if err != nil {
			return err
		}
	}

	ctx := context.Background()
	token, err := webhook.GetToken(ctx, k8sClient.Clientset(), ns, appName)
	if err != nil {
		return err
	}

	webhookURL := fmt.Sprintf("https://%s/api/v1/webhook/%s/%s", cluster.ConsoleHost(), ns, appName)

	fmt.Printf("\n  Webhook URL:    %s\n", webhookURL)
	fmt.Printf("  Secret token:   %s\n\n", token)

	return nil
}
