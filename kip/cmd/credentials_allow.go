package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/controller/pkg/sharedcred"
	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/credentials"
	"github.com/getkipper/kipper/kip/internal/k8s"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

var credentialsAllowCmd = &cobra.Command{
	Use:   "allow [name]",
	Short: "Let a project build with a shared git credential",
	Long: `Allows a project to build with a shared git credential.

A shared credential is usable only by the projects named on it, so a new
credential builds nothing until a project is allowed. The token itself is never
asked for: this changes who may use it, not what it is.

  kip credentials allow forge --project shop
  kip credentials allow forge --project shop --project blog

Container registry credentials are granted with 'kip registry add --allow-project'.`,
	Args: cobra.ExactArgs(1),
	RunE: runCredentialsAllow,
}

var credentialsRevokeCmd = &cobra.Command{
	Use:   "revoke [name]",
	Short: "Stop a project building with a shared git credential",
	Long: `Stops a project building with a shared git credential.

Apps already built keep running the image they have. The next build for that
project is refused.

  kip credentials revoke forge --project shop`,
	Args: cobra.ExactArgs(1),
	RunE: runCredentialsRevoke,
}

func runCredentialsAllow(cmd *cobra.Command, args []string) error {
	projects, cluster, k8sClient, err := credentialGrantArgs(cmd)
	if err != nil {
		return err
	}
	// Stored under the name the cluster knows the project by, which is what the
	// build-time check compares against. An org prefix is part of that name, so
	// a bare project would otherwise be granted something that matches nothing.
	resolved := make([]string, 0, len(projects))
	for _, project := range projects {
		resolved = append(resolved, cluster.ResolveNamespace(project, ""))
	}
	return grantSharedCredential(context.Background(), k8sClient.Clientset(), k8sClient.Dynamic(),
		args[0], resolved, cmd.OutOrStdout())
}

func runCredentialsRevoke(cmd *cobra.Command, args []string) error {
	projects, cluster, k8sClient, err := credentialGrantArgs(cmd)
	if err != nil {
		return err
	}
	return revokeSharedCredential(context.Background(), k8sClient.Clientset(), k8sClient.Dynamic(),
		args[0], projects, revocableForms(cluster, projects), cmd.OutOrStdout())
}

func credentialGrantArgs(cmd *cobra.Command) ([]string, *config.Cluster, *k8s.Client, error) {
	projects, _ := cmd.Flags().GetStringArray("project")
	if len(projects) == 0 {
		return nil, nil, nil, fmt.Errorf("--project is required")
	}
	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return nil, nil, nil, err
	}
	return projects, cluster, k8sClient, nil
}

// revocableForms is what a revoke has to remove: the project as typed and the
// name the cluster knows it by.
//
// A grant is stored resolved, so an org cluster needs the resolved form. What is
// stored is not always what this version would have written, though: an older
// kip, a hand-edited list or a restore can leave a bare name on an org cluster,
// and revoking is the way out of exactly those. Resolving alone would refuse to
// remove them while reporting success.
func revocableForms(cluster *config.Cluster, projects []string) []string {
	forms := make([]string, 0, len(projects)*2)
	for _, project := range projects {
		forms = append(forms, project)
		if resolved := cluster.ResolveNamespace(project, ""); resolved != project {
			forms = append(forms, resolved)
		}
	}
	return forms
}

// grantSharedCredential allows projects to build with the named shared git
// credential and reports the allow-list it leaves behind.
func grantSharedCredential(ctx context.Context, clientset kubernetes.Interface, dyn dynamic.Interface,
	name string, projects []string, out io.Writer) error {
	// Only a grant checks the project. Nothing reports at build time which
	// project a token was granted to, so a typo is stored as a success and met
	// as a refused build much later. A revoke is the way out of a mistake,
	// including one naming a project that has since been deleted, so requiring
	// the project there would shut the door on the way out.
	if err := projectsMustExist(ctx, dyn, projects); err != nil {
		return err
	}
	return changeSharedCredential(ctx, clientset, name, projects, projects, sharedcred.Grant, "may now build with", out)
}

// revokeSharedCredential stops projects building with the named shared git
// credential and reports the allow-list it leaves behind.
// named is what the operator asked for and forms is every spelling of it that
// may be stored, so the message says one project where two were removed.
func revokeSharedCredential(ctx context.Context, clientset kubernetes.Interface, _ dynamic.Interface,
	name string, named, forms []string, out io.Writer) error {
	return changeSharedCredential(ctx, clientset, name, named, forms, sharedcred.Revoke, "can no longer build with", out)
}

func changeSharedCredential(ctx context.Context, clientset kubernetes.Interface, name string, named, projects []string,
	change func([]sharedcred.Entry, string, []string) ([]sharedcred.Entry, error), verb string, out io.Writer) error {
	var allowed []string
	err := sharedcred.Update(ctx, clientset, func(entries []sharedcred.Entry) ([]sharedcred.Entry, error) {
		updated, changeErr := change(entries, name, projects)
		if changeErr != nil {
			return nil, changeErr
		}
		if entry := sharedcred.Find(updated, name); entry != nil {
			allowed = entry.AllowedProjects
		}
		return updated, nil
	})
	if err != nil {
		return sharedCredentialError(ctx, clientset, name, err)
	}

	_, _ = fmt.Fprintf(out, "\n  ✔  %s %s %s\n", strings.Join(named, ", "), verb, name)
	if len(allowed) == 0 {
		_, _ = fmt.Fprintf(out, "     No project may build with it now.\n\n")
		return nil
	}
	_, _ = fmt.Fprintf(out, "     Allowed projects: %s\n\n", strings.Join(allowed, ", "))
	return nil
}

// projectsMustExist refuses a grant naming a project the cluster does not have,
// and says which it does. A project name is compared exactly at build time, so a
// typo or the wrong case is a grant that can never match.
func projectsMustExist(ctx context.Context, dyn dynamic.Interface, projects []string) error {
	live, err := dyn.Resource(manifest.ProjectGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing projects: %w", err)
	}
	known := make([]string, 0, len(live.Items))
	for i := range live.Items {
		known = append(known, live.Items[i].GetName())
	}
	for _, project := range projects {
		if !contains(known, project) {
			return fmt.Errorf("this cluster has no project %q. It has: %s", project, strings.Join(known, ", "))
		}
	}
	return nil
}

// sharedCredentialError says which command grants a registry credential when
// that is what the name turns out to be. The two stores are separate and share a
// namespace of names, so "no shared git credential named ghcr" is true and
// unhelpful when ghcr is sitting in the other one.
//
// It stands in for that one error only. Substituting it for a failure to read or
// write would send an operator whose name exists in both stores to the wrong one
// whenever the cluster refused the write.
func sharedCredentialError(ctx context.Context, clientset kubernetes.Interface, name string, err error) error {
	var unknown *sharedcred.UnknownCredentialError
	if !errors.As(err, &unknown) {
		return err
	}
	regs, listErr := credentials.List(ctx, clientset, credentials.TypeRegistry)
	if listErr != nil {
		return err
	}
	for _, reg := range regs {
		if reg.Name == name {
			return fmt.Errorf("%s is a container registry credential, so grant it with 'kip registry add --name %s --server %s --allow-project <project>'. That flag replaces the allow-list, so name every project that should keep access", name, reg.Name, reg.Server)
		}
	}
	return err
}
