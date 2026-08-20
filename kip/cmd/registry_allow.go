package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/controller/pkg/registrycred"
)

var registryAllowCmd = &cobra.Command{
	Use:   "allow [name]",
	Short: "Let a project pull with a registry credential",
	Long: `Allows a project to pull images with a registry credential.

A credential is usable only by the projects named on it, so a new one pulls
nothing until a project is allowed. The password is never asked for: this
changes who may use the credential, not what it is.

  kip registry allow ghcr-io --project shop
  kip registry allow ghcr-io --project shop --project blog

This adds to the allow-list. 'kip registry add --allow-project' replaces it.`,
	Args: cobra.ExactArgs(1),
	RunE: runRegistryAllow,
}

var registryRevokeCmd = &cobra.Command{
	Use:   "revoke [name]",
	Short: "Stop a project pulling with a registry credential",
	Long: `Stops a project pulling with a registry credential.

Workloads already running keep the image they have. The next pull for that
project is refused, which for most workloads means the next restart.

  kip registry revoke ghcr-io --project shop`,
	Args: cobra.ExactArgs(1),
	RunE: runRegistryRevoke,
}

func runRegistryAllow(cmd *cobra.Command, args []string) error {
	projects, cluster, k8sClient, err := credentialGrantArgs(cmd)
	if err != nil {
		return err
	}
	// Stored under the name the cluster knows the project by, which is what the
	// pull check compares against.
	resolved := make([]string, 0, len(projects))
	for _, project := range projects {
		resolved = append(resolved, cluster.ResolveNamespace(project, ""))
	}
	return grantRegistryCredential(context.Background(), k8sClient.Clientset(), k8sClient.Dynamic(),
		args[0], resolved, cmd.OutOrStdout())
}

func runRegistryRevoke(cmd *cobra.Command, args []string) error {
	projects, cluster, k8sClient, err := credentialGrantArgs(cmd)
	if err != nil {
		return err
	}
	return revokeRegistryCredential(context.Background(), k8sClient.Clientset(),
		args[0], projects, revocableForms(cluster, projects), cmd.OutOrStdout())
}

// grantRegistryCredential allows projects to pull with the named credential and
// reports the allow-list it leaves behind.
//
// Only a grant checks the project. Nothing reports at pull time which project a
// credential was granted to, so a typo is stored as a success and met as a
// refused pull much later. A revoke is the way out of a mistake, including one
// naming a project that has since been deleted.
func grantRegistryCredential(ctx context.Context, clientset kubernetes.Interface, dyn dynamic.Interface,
	name string, projects []string, out io.Writer) error {
	if err := projectsMustExist(ctx, dyn, projects); err != nil {
		return err
	}
	return changeRegistryCredential(ctx, clientset, name, projects, projects, registrycred.Grant, "may now pull with", out)
}

// revokeRegistryCredential stops projects pulling with the named credential.
// named is what the operator asked for and forms is every spelling of it that
// may be stored, so the message says one project where two were removed.
func revokeRegistryCredential(ctx context.Context, clientset kubernetes.Interface,
	name string, named, forms []string, out io.Writer) error {
	return changeRegistryCredential(ctx, clientset, name, named, forms, registrycred.Revoke, "can no longer pull with", out)
}

func changeRegistryCredential(ctx context.Context, clientset kubernetes.Interface, name string, named, projects []string,
	change func([]registrycred.Entry, string, []string) ([]registrycred.Entry, error), verb string, out io.Writer) error {
	var allowed []string
	err := registrycred.Update(ctx, clientset, func(entries []registrycred.Entry) ([]registrycred.Entry, error) {
		updated, changeErr := change(entries, name, projects)
		if changeErr != nil {
			return nil, changeErr
		}
		if entry := registrycred.Find(updated, name); entry != nil {
			allowed = entry.AllowedProjects
		}
		return updated, nil
	})
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "\n  ✔  %s %s %s\n", strings.Join(named, ", "), verb, name)
	if len(allowed) == 0 {
		_, _ = fmt.Fprintf(out, "     No project may pull with it now.\n\n")
		return nil
	}
	_, _ = fmt.Fprintf(out, "     Allowed projects: %s\n\n", strings.Join(allowed, ", "))
	return nil
}
