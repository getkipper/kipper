package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreClient "k8s.io/client-go/kubernetes/typed/core/v1"
)

const (
	registryConfigName = "kipper-registries"
	registryConfigNS   = "kipper-system"
	// dockerHubAuthKey is the canonical Docker Hub auth key — the only key the
	// container runtime resolves Docker Hub pulls against.
	dockerHubAuthKey = "https://index.docker.io/v1/"
)

// registryEntry mirrors console-api's registrycred.Entry. Every field must
// round-trip here, because saving rewrites the whole list — a field missing
// from this struct would be silently dropped from entries the console wrote.
type registryEntry struct {
	Name            string   `json:"name"`
	Server          string   `json:"server"`
	Username        string   `json:"username"`
	Password        string   `json:"password,omitempty"`
	AllowedProjects []string `json:"allowedProjects,omitempty"`
}

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

	server = normalizeRegistryServer(server)
	if name == "" {
		name = sanitizeName(server)
	}

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	clientset := k8sClient.Clientset()

	entries, err := loadRegEntries(ctx, clientset)
	if err != nil {
		return err
	}

	// Update or append. Omitted flags keep an existing entry's values, so a
	// grant (`--allow-project`) never requires re-entering the password.
	found := false
	for i := range entries {
		if entries[i].Name != name {
			continue
		}
		entries[i].Server = server
		if username != "" {
			entries[i].Username = username
		}
		if password != "" {
			entries[i].Password = password
		}
		if cmd.Flags().Changed("allow-project") {
			entries[i].AllowedProjects = allowProjects
		}
		allowProjects = entries[i].AllowedProjects
		found = true
		break
	}
	if !found {
		if username == "" || password == "" {
			return fmt.Errorf("--username and --password are required for a new registry")
		}
		entries = append(entries, registryEntry{
			Name: name, Server: server, Username: username, Password: password,
			AllowedProjects: allowProjects,
		})
	}

	if err := saveRegEntries(ctx, clientset, entries); err != nil {
		return fmt.Errorf("saving registry credentials: %w", err)
	}

	fmt.Printf("\n  ✔  Registry %s added (%s)\n", name, server)
	if len(allowProjects) > 0 {
		fmt.Printf("  Allowed projects: %s\n\n", strings.Join(allowProjects, ", "))
	} else {
		fmt.Printf("  No project is allowed yet: grant one with:\n")
		fmt.Printf("    kip registry add --server %s --allow-project <project>\n\n", server)
	}
	return nil
}

func runRegistryList(cmd *cobra.Command, args []string) error {
	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	entries, err := loadRegEntries(ctx, k8sClient.Clientset())
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

	entries, err := loadRegEntries(ctx, clientset)
	if err != nil {
		return err
	}
	filtered := make([]registryEntry, 0, len(entries))
	for _, e := range entries {
		if e.Name != name {
			filtered = append(filtered, e)
		}
	}

	if len(filtered) == len(entries) {
		return fmt.Errorf("registry %q not found", name)
	}

	if err := saveRegEntries(ctx, clientset, filtered); err != nil {
		return fmt.Errorf("saving registry credentials: %w", err)
	}

	fmt.Printf("\n  ✔  Registry %s removed\n\n", name)
	return nil
}

// loadRegEntries returns the stored credential list. A missing Secret means no
// registries are configured and returns (nil, nil); a read or parse failure is
// an error, so a mutation never rewrites the list from a state it could not
// read — that would silently destroy every other credential.
func loadRegEntries(ctx context.Context, clientset interface {
	CoreV1() coreClient.CoreV1Interface
}) ([]registryEntry, error) {
	secret, err := clientset.CoreV1().Secrets(registryConfigNS).Get(ctx, registryConfigName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading registry credentials: %w", err)
	}
	data := secret.Data["registries"]
	if len(data) == 0 {
		return nil, nil
	}
	var entries []registryEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parsing registry credentials: %w", err)
	}
	return entries, nil
}

func saveRegEntries(ctx context.Context, clientset interface {
	CoreV1() coreClient.CoreV1Interface
}, entries []registryEntry) error {
	data, err := json.Marshal(entries) //nolint:gosec // password is intentionally stored in a K8s Secret
	if err != nil {
		return err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      registryConfigName,
			Namespace: registryConfigNS,
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "kipper"},
		},
		Data: map[string][]byte{"registries": data},
	}

	_, err = clientset.CoreV1().Secrets(registryConfigNS).Update(ctx, secret, metav1.UpdateOptions{})
	if errors.IsNotFound(err) {
		_, err = clientset.CoreV1().Secrets(registryConfigNS).Create(ctx, secret, metav1.CreateOptions{})
	}
	return err
}

// normalizeRegistryServer maps the common Docker Hub aliases to the canonical
// auth key, mirroring console-api's registrycred.NormalizeServer so both
// writers store the same server value for the same registry.
func normalizeRegistryServer(server string) string {
	s := strings.TrimSpace(server)
	host := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://"), "/")
	switch strings.ToLower(host) {
	case "docker.io", "index.docker.io", "index.docker.io/v1", "registry-1.docker.io", "registry.hub.docker.com":
		return dockerHubAuthKey
	}
	return s
}

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
