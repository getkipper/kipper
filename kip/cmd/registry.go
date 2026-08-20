package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"

	"github.com/getkipper/kipper/controller/pkg/registrycred"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

var registryCmd = &cobra.Command{
	Use:   "registry",
	Short: "Manage container registry credentials",
}

var registryAddCmd = &cobra.Command{
	Use:   "add",
	Short: "Add or update registry credentials",
	RunE:  runRegistryAdd,
}

var registryListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured registries",
	RunE:  runRegistryList,
}

var registryRemoveCmd = &cobra.Command{
	Use:   "remove [name]",
	Short: "Remove registry credentials",
	Args:  cobra.ExactArgs(1),
	RunE:  runRegistryRemove,
}

func init() {
	registryAddCmd.Flags().String("server", "", "registry server (e.g. ghcr.io, registry.example.com)")
	registryAddCmd.Flags().String("username", "", "registry username or token name (required for a new registry)")
	registryAddCmd.Flags().String("password", "", "registry password or access token (required for a new registry)")
	registryAddCmd.Flags().String("name", "", "credential name (auto-generated from server if omitted)")
	registryAddCmd.Flags().StringArray("allow-project", nil, "project allowed to pull with this credential (repeatable; replaces the allow-list)")
	_ = registryAddCmd.MarkFlagRequired("server")

	registryCmd.AddCommand(registryAddCmd)
	registryAllowCmd.Flags().StringArray("project", nil, "project allowed to pull with this credential (repeatable)")
	registryRevokeCmd.Flags().StringArray("project", nil, "project to stop from pulling with this credential (repeatable)")

	registryCmd.AddCommand(registryAllowCmd)
	registryCmd.AddCommand(registryRevokeCmd)
	registryCmd.AddCommand(registryListCmd)
	registryCmd.AddCommand(registryRemoveCmd)
	rootCmd.AddCommand(registryCmd)
}

func runRegistryAdd(cmd *cobra.Command, args []string) error {
	server, _ := cmd.Flags().GetString("server")
	username, _ := cmd.Flags().GetString("username")
	password, _ := cmd.Flags().GetString("password")
	name, _ := cmd.Flags().GetString("name")
	allowProjects, _ := cmd.Flags().GetStringArray("allow-project")

	server = registrycred.NormalizeServer(server)
	if name == "" {
		name = sanitizeName(server)
	}

	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	clientset := k8sClient.Clientset()

	// Resolved to the name the cluster knows a project by, which is what the
	// pull check compares against. An organisation prefix is part of that name,
	// so a bare project would otherwise be granted something that matches
	// nothing, and the command would report success.
	resolved := make([]string, 0, len(allowProjects))
	for _, project := range allowProjects {
		resolved = append(resolved, cluster.ResolveNamespace(project, ""))
	}
	warnUnknownProjects(ctx, k8sClient.Dynamic(), resolved)

	var allowed, removed []string
	if err := registrycred.Update(ctx, clientset, func(entries []registrycred.Entry) ([]registrycred.Entry, error) {
		var applyErr error
		entries, allowed, removed, applyErr = applyRegistryAdd(entries, registryAdd{
			Name:            name,
			Server:          server,
			Username:        username,
			Password:        password,
			AllowedProjects: resolved,
			ReplaceAllowed:  cmd.Flags().Changed("allow-project"),
		})
		return entries, applyErr
	}); err != nil {
		return fmt.Errorf("saving registry credentials: %w", err)
	}
	allowProjects = allowed

	fmt.Printf("\n  ✔  Registry %s added (%s)\n", name, server)
	if len(removed) > 0 {
		// --allow-project replaces the list, so an operator naming one project
		// takes away every other. It always did; now it says so.
		fmt.Printf("  Removed: %s\n", strings.Join(removed, ", "))
	}
	if len(allowProjects) > 0 {
		fmt.Printf("  Allowed projects: %s\n\n", strings.Join(allowProjects, ", "))
	} else {
		fmt.Printf("  No project is allowed yet: grant one with:\n")
		fmt.Printf("    kip registry allow %s --project <project>\n\n", name)
	}
	return nil
}

func runRegistryList(cmd *cobra.Command, args []string) error {
	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	entries, err := registrycred.Load(ctx, k8sClient.Clientset())
	if err != nil {
		return err
	}

	if len(entries) == 0 {
		fmt.Printf("\n  No registries configured\n\n")
		return nil
	}

	fmt.Printf("\n  %-25s %-40s %-20s %s\n", "NAME", "SERVER", "USERNAME", "ALLOWED PROJECTS")
	for _, e := range entries {
		projects := strings.Join(e.AllowedProjects, ", ")
		if projects == "" {
			projects = "(none)"
		}
		fmt.Printf("  %-25s %-40s %-20s %s\n", e.Name, e.Server, e.Username, projects)
	}
	fmt.Println()
	return nil
}

func runRegistryRemove(cmd *cobra.Command, args []string) error {
	name := args[0]

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	clientset := k8sClient.Clientset()

	if err := registrycred.Update(ctx, clientset, func(entries []registrycred.Entry) ([]registrycred.Entry, error) {
		kept := make([]registrycred.Entry, 0, len(entries))
		for _, e := range entries {
			if e.Name != name {
				kept = append(kept, e)
			}
		}
		if len(kept) == len(entries) {
			return nil, &registrycred.UnknownRegistryError{Name: name}
		}
		return kept, nil
	}); err != nil {
		return err
	}

	fmt.Printf("\n  ✔  Registry %s removed\n\n", name)
	return nil
}

// loadRegEntries returns the stored credential list. A missing Secret means no
// registries are configured and returns (nil, nil); a read or parse failure is
// an error, so a mutation never rewrites the list from a state it could not
// read — that would silently destroy every other credential.

// sanitizeName derives a default credential name from a server's host. It must
// generate the same name as the console API's sanitizeRegistryName, so kip and
// the console address the same entry for the same registry.
func sanitizeName(server string) string {
	host := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(server, "https://"), "http://"), "/")
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	return strings.NewReplacer(".", "-", ":", "-").Replace(strings.ToLower(host))
}

// warnUnknownProjects says when a grant names a project the cluster does not
// have. It warns rather than refuses, because creating the credential before the
// project it is for is an ordinary bootstrap and the grant starts matching the
// moment that project exists. A cluster it cannot ask is not an answer either
// way, so it says nothing.
func warnUnknownProjects(ctx context.Context, dyn dynamic.Interface, projects []string) {
	if len(projects) == 0 {
		return
	}
	live, err := dyn.Resource(manifest.ProjectGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	known := make([]string, 0, len(live.Items))
	for i := range live.Items {
		known = append(known, live.Items[i].GetName())
	}
	for _, project := range projects {
		if !contains(known, project) {
			fmt.Fprintf(os.Stderr, "  !   This cluster has no project %q. The grant is stored and starts working if one is created.\n", project)
		}
	}
}

// projectsMissingFrom is what a replacing grant takes away.
func projectsMissingFrom(before, after []string) []string {
	var gone []string
	for _, p := range before {
		if !contains(after, p) {
			gone = append(gone, p)
		}
	}
	return gone
}

// registryAdd is what `kip registry add` was asked to change. An omitted flag
// keeps what is stored, so granting never requires re-entering the password.
type registryAdd struct {
	Name            string
	Server          string
	Username        string
	Password        string
	AllowedProjects []string
	// ReplaceAllowed is whether --allow-project was given at all. The flag
	// replaces the list, which is what it has always done, so its absence and an
	// empty value mean different things.
	ReplaceAllowed bool
}

// applyRegistryAdd is the whole decision `kip registry add` makes, kept apart
// from the command so it can be driven directly. It returns the list to store,
// the projects allowed afterwards, and the ones the change took away.
func applyRegistryAdd(entries []registrycred.Entry, want registryAdd) ([]registrycred.Entry, []string, []string, error) {
	live := registrycred.Find(entries, want.Name)
	if live == nil {
		if want.Username == "" || want.Password == "" {
			return nil, nil, nil, fmt.Errorf("--username and --password are required for a new registry")
		}
		return append(entries, registrycred.Entry{
			Name: want.Name, Server: want.Server, Username: want.Username, Password: want.Password,
			AllowedProjects: want.AllowedProjects,
		}), want.AllowedProjects, nil, nil
	}
	// A credential is addressed by name, so changing the host it points at
	// silently repoints the password too. That is a different registry with
	// somebody else's credential, so it takes a fresh one.
	if !registrycred.SameRegistry(live.Server, want.Server) && want.Password == "" {
		return nil, nil, nil, fmt.Errorf("%s currently points at %s. Pointing it at %s needs --password, because the stored one belongs to the old registry",
			want.Name, live.Server, want.Server)
	}
	live.Server = want.Server
	if want.Username != "" {
		live.Username = want.Username
	}
	if want.Password != "" {
		live.Password = want.Password
	}
	var removed []string
	if want.ReplaceAllowed {
		removed = projectsMissingFrom(live.AllowedProjects, want.AllowedProjects)
		live.AllowedProjects = want.AllowedProjects
	}
	return entries, live.AllowedProjects, removed, nil
}
