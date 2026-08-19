package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/getkipper/kipper/kip/internal/credentials"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

var credentialsCmd = &cobra.Command{
	Use:   "credentials",
	Short: "Read git and registry credentials stored in the cluster",
	Long: `View the git and container-registry credentials configured in the
Kipper console settings. Useful for recovering a token to use in another
cluster or pipeline when you no longer have a copy elsewhere.

Access requires kubeconfig permission to read secrets in the kipper-system
namespace, so in practice this is a cluster-admin command.`,
}

var credentialsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured credentials with masked values",
	RunE:  runCredentialsList,
}

var credentialsGetCmd = &cobra.Command{
	Use:   "get [name]",
	Short: "Print the plaintext value of a single credential",
	Long: `Print the plaintext value of a single credential.

By name, this reads the global credential store. With --app, it reads the
git token an application uses to clone its source repository. That token
lives in a Secret in the app's own namespace and does not appear in the
global store or in 'credentials list'.

  kip credentials get github-com
  kip credentials get --app acme-website --project acme --environment test`,
	Args: func(cmd *cobra.Command, args []string) error {
		app, _ := cmd.Flags().GetString("app")
		if app != "" {
			if len(args) > 0 {
				return fmt.Errorf("provide either a credential name or --app, not both")
			}
			if cmd.Flags().Changed("type") {
				return fmt.Errorf("--type does not apply with --app: an app's git token is always a git credential")
			}
			return nil
		}
		return cobra.ExactArgs(1)(cmd, args)
	},
	RunE: runCredentialsGet,
}

func init() {
	credentialsListCmd.Flags().String("type", "", "filter by type: git or registry")
	credentialsGetCmd.Flags().String("type", "", "disambiguate when the same name exists in both stores: git or registry")
	credentialsGetCmd.Flags().String("app", "", "read the git token of this application instead of a named credential")
	credentialsGetCmd.Flags().String("project", "", "project name (used with --app)")
	credentialsGetCmd.Flags().String("environment", "", "target environment (used with --app)")

	credentialsAllowCmd.Flags().StringArray("project", nil, "project allowed to build with this credential (repeatable)")
	credentialsRevokeCmd.Flags().StringArray("project", nil, "project to stop from building with this credential (repeatable)")

	credentialsCmd.AddCommand(credentialsListCmd)
	credentialsCmd.AddCommand(credentialsGetCmd)
	credentialsCmd.AddCommand(credentialsAllowCmd)
	credentialsCmd.AddCommand(credentialsRevokeCmd)
	rootCmd.AddCommand(credentialsCmd)
}

func runCredentialsList(cmd *cobra.Command, _ []string) error {
	filter, err := parseCredentialType(cmd)
	if err != nil {
		return err
	}

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	creds, err := credentials.List(ctx, k8sClient.Clientset(), filter)
	if err != nil {
		return err
	}

	if len(creds) == 0 {
		fmt.Printf("\n  No credentials configured\n\n")
		return nil
	}

	fmt.Printf("\n  %-10s %-25s %-35s %-20s %-14s %s\n", "TYPE", "NAME", "SERVER", "USERNAME", "VALUE", "ALLOWED PROJECTS")
	for _, c := range creds {
		projects := strings.Join(c.AllowedProjects, ", ")
		if projects == "" {
			projects = "(none)"
		}
		fmt.Printf("  %-10s %-25s %-35s %-20s %-14s %s\n",
			c.Type, c.Name, c.Server, c.Username, credentials.Mask(c.Value), projects)
	}
	fmt.Println()
	return nil
}

func runCredentialsGet(cmd *cobra.Command, args []string) error {
	if app, _ := cmd.Flags().GetString("app"); app != "" {
		return runCredentialsGetApp(cmd, app)
	}

	preferred, err := parseCredentialType(cmd)
	if err != nil {
		return err
	}

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	cred, err := credentials.Get(ctx, k8sClient.Clientset(), args[0], preferred)
	if err != nil {
		return err
	}

	fmt.Println(cred.Value)
	return nil
}

func runCredentialsGetApp(cmd *cobra.Command, appName string) error {
	ns, k8sClient, err := resolveAppNamespace(cmd, appName)
	if err != nil {
		return err
	}

	dyn := k8sClient.Dynamic()
	if dyn == nil {
		return fmt.Errorf("dynamic client unavailable for this cluster")
	}

	ctx := context.Background()
	appCR, err := dyn.Resource(manifest.AppGVR).Namespace(ns).Get(ctx, appName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading app %q in namespace %q: %w", appName, ns, err)
	}

	secretName, _, err := unstructured.NestedString(appCR.Object, "spec", "git", "credentialsSecret")
	if err != nil {
		return fmt.Errorf("reading git source of %q: %w", appName, err)
	}
	if secretName == "" {
		return fmt.Errorf("app %q has no git credential configured", appName)
	}

	cred, err := credentials.GetForApp(ctx, k8sClient.Clientset(), ns, secretName, appName)
	if err != nil {
		return err
	}

	fmt.Println(cred.Value)
	return nil
}

func parseCredentialType(cmd *cobra.Command) (credentials.Type, error) {
	raw, _ := cmd.Flags().GetString("type")
	switch raw {
	case "":
		return "", nil
	case "git":
		return credentials.TypeGit, nil
	case "registry":
		return credentials.TypeRegistry, nil
	default:
		return "", fmt.Errorf("invalid --type %q: must be git or registry", raw)
	}
}
