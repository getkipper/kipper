package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/deployer"
)

var appGitCmd = &cobra.Command{
	Use:   "git",
	Short: "Manage an app's git source",
}

var appGitRemoveCmd = &cobra.Command{
	Use:   "remove [app-name]",
	Short: "Stop building an app from git",
	Long: `Detaches the git repository an app builds from, so it deploys prebuilt
images instead.

Use this when a pipeline builds and pushes the image itself. While a git source
is attached, a deploy webhook builds from the repository and ignores the image
the pipeline named, so the two ways of deploying cannot both be in play.

The image the app is running now keeps running. The stored access token and the
last build's status go with the source.`,
	Args: cobra.ExactArgs(1),
	RunE: runAppGitRemove,
}

func init() {
	appGitRemoveCmd.Flags().String("project", "", "project name")
	appGitRemoveCmd.Flags().String("environment", "", "target environment")

	appGitCmd.AddCommand(appGitRemoveCmd)
	appCmd.AddCommand(appGitCmd)
}

func runAppGitRemove(cmd *cobra.Command, args []string) error {
	appName := args[0]

	ns, k8sClient, err := resolveAppNamespace(cmd, appName)
	if err != nil {
		return err
	}

	d := &deployer.Deployer{Client: k8sClient.Clientset(), Dynamic: k8sClient.Dynamic()}
	removed, err := d.RemoveGitSource(context.Background(), ns, appName)
	if err != nil {
		return err
	}

	if !removed {
		fmt.Printf("\n  %s builds from an image already, so there was nothing to remove\n\n", appName)
		return nil
	}

	fmt.Printf("\n  ✔  %s no longer builds from git\n", appName)
	fmt.Printf("     It keeps running the image it has. Deploy a new one with\n")
	fmt.Printf("     'kip app update %s --image <image>' or from your pipeline.\n\n", appName)
	return nil
}
