package cmd

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/getkipper/kipper/controller/pkg/serving"
	"github.com/getkipper/kipper/kip/internal/clusteridentity"
	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
)

var clusterCmd = &cobra.Command{
	Use:   "cluster",
	Short: "Manage cluster connections",
}

var clusterListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all configured clusters",
	RunE:  runClusterList,
}

var clusterUseCmd = &cobra.Command{
	Use:   "use [name]",
	Short: "Switch to a different cluster",
	Long: `Set the active cluster for all kip commands.

Examples:
  kip cluster use staging
  kip cluster use production`,
	Args: cobra.ExactArgs(1),
	RunE: runClusterUse,
}

var clusterExportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export cluster credentials for sharing with team members",
	Long: `Exports the current cluster's connection details as a portable file
that team members can import with 'kip cluster add'.

Examples:
  kip cluster export > cluster.kip
  kip cluster export --cluster staging > staging.kip`,
	RunE: runClusterExport,
}

var clusterAddCmd = &cobra.Command{
	Use:   "add [file]",
	Short: "Add a cluster from an exported file",
	Long: `Import a cluster configuration shared by a team member.

Examples:
  kip cluster add cluster.kip
  kip cluster add staging.kip --set-current`,
	Args: cobra.ExactArgs(1),
	RunE: runClusterAdd,
}

var clusterRemoveCmd = &cobra.Command{
	Use:   "remove [name]",
	Short: "Remove a cluster from your configuration",
	Args:  cobra.ExactArgs(1),
	RunE:  runClusterRemove,
}

var clusterRenameCmd = &cobra.Command{
	Use:   "rename [old-name] [new-name]",
	Short: "Rename a cluster for easier switching",
	Long: `Give a cluster a short, memorable name.

Examples:
  kip cluster rename example.kipper.run example
  kip cluster rename 203-0-113-12.kipper.run dev`,
	Args: cobra.ExactArgs(2),
	RunE: runClusterRename,
}

var clusterEnvCmd = &cobra.Command{
	Use:   "env [component] [KEY=VALUE...]",
	Short: "Set environment variables on a cluster component",
	Long: `Sets environment variables on a Kipper cluster component and restarts
it to pick up the changes.

Components: console, console-api, dex, traefik

Examples:
  kip cluster env console-api LOG_LEVEL=debug
  kip cluster env console-api LOG_LEVEL=debug FEATURE_X=enabled`,
	Args: cobra.MinimumNArgs(2),
	RunE: runClusterEnv,
}

var clusterDomainCmd = &cobra.Command{
	Use:   "domain [domain]",
	Short: "Change the cluster's serving domain, or repair a drifted local config",
	Long: `Move the Kipper console, API, and login onto a custom domain
(e.g. kipper.example.com) or a *.kipper.run subdomain (kip claims it with the
gateway first).

The change runs as a no-lockout transition driven by the cluster: the new hosts
come up alongside the old ones, kip verifies from outside that they answer with
a valid certificate, and only then does it approve the one cutover that moves
the login issuer. The old hosts keep serving until the cutover completes. If
verification fails, nothing is cut over and the old hosts keep serving.

Point DNS for the new hosts at the server before running this.

If the cluster uses SSO connectors, the cutover moves the Dex issuer, so each
provider's callback URL must be updated first. kip stops and lists what to
change; re-run with --ack-sso-callbacks once every provider is updated.

Other modes:
  --sync      resume a change interrupted partway (e.g. a dropped connection),
              or report an already-converged cluster.
  --rollback  return to the previous serving identity, as a normal cutover in
              the opposite direction with the same checks.
  --repair    rewrite the local ~/.kip/config.yaml from the cluster's
              ClusterIdentity record, after a kip version change or local drift.

Examples:
  kip cluster domain kipper.example.com
  kip cluster domain kipper.example.com --ack-sso-callbacks --yes
  kip cluster domain --sync
  kip cluster domain --rollback
  kip cluster domain --repair`,
	Args: cobra.MaximumNArgs(1),
	RunE: runClusterDomain,
}

func init() {
	clusterExportCmd.Flags().String("cluster", "", "cluster name (defaults to current)")
	clusterAddCmd.Flags().Bool("set-current", false, "set as the active cluster after adding")
	clusterDomainCmd.Flags().Bool("repair", false, "rewrite local config from the cluster's ClusterIdentity record")
	clusterDomainCmd.Flags().Bool("sync", false, "resume an interrupted domain change, or report an already-converged cluster")
	clusterDomainCmd.Flags().Bool("rollback", false, "return to the previous serving identity recorded at the last change")
	clusterDomainCmd.Flags().Bool("ack-sso-callbacks", false, "confirm each SSO provider's callback URL has been updated for the new issuer")
	clusterDomainCmd.Flags().BoolP("yes", "y", false, "skip the confirmation prompt (non-interactive cutover)")

	clusterCmd.AddCommand(clusterListCmd)
	clusterCmd.AddCommand(clusterUseCmd)
	clusterCmd.AddCommand(clusterExportCmd)
	clusterCmd.AddCommand(clusterAddCmd)
	clusterCmd.AddCommand(clusterRemoveCmd)
	clusterCmd.AddCommand(clusterRenameCmd)
	clusterCmd.AddCommand(clusterDomainCmd)
	clusterCmd.AddCommand(clusterEnvCmd)
	rootCmd.AddCommand(clusterCmd)
}

func runClusterList(_ *cobra.Command, _ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	if len(cfg.Clusters) == 0 {
		fmt.Print("\n  No clusters configured. Run 'kip install' to set up a cluster.\n\n")
		return nil
	}

	fmt.Println()
	for _, c := range cfg.Clusters {
		marker := "  "
		if c.Name == cfg.CurrentCluster {
			marker = "→ "
		}
		fmt.Printf("  %s%s\n", marker, c.Name)
		fmt.Printf("      Console: https://%s\n", c.ConsoleHost())
		fmt.Printf("      Server:  %s\n", c.Host)
		if c.CurrentProject != "" {
			if c.CurrentEnvironment != "" {
				fmt.Printf("      Project: %s/%s\n", c.CurrentProject, c.CurrentEnvironment)
			} else {
				fmt.Printf("      Project: %s\n", c.CurrentProject)
			}
		}
		fmt.Println()
	}

	return nil
}

func runClusterEnv(_ *cobra.Command, args []string) error {
	component := args[0]

	namespaces := map[string]string{
		"console":     "kipper-system",
		"console-api": "kipper-system",
		"dex":         "dex",
		"traefik":     "traefik",
	}

	ns, ok := namespaces[component]
	if !ok {
		return fmt.Errorf("unknown component %q: valid options: console, console-api, dex, traefik", component)
	}

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	deploy, err := k8sClient.Clientset().AppsV1().Deployments(ns).Get(ctx, component, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting deployment %s/%s: %w", ns, component, err)
	}

	container := &deploy.Spec.Template.Spec.Containers[0]

	for _, pair := range args[1:] {
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid env var %q: expected KEY=VALUE format", pair)
		}
		key, value := parts[0], parts[1]

		found := false
		for i, env := range container.Env {
			if env.Name == key {
				container.Env[i].Value = value
				found = true
				break
			}
		}
		if !found {
			container.Env = append(container.Env, corev1.EnvVar{Name: key, Value: value})
		}
		fmt.Printf("  ✔  %s=%s\n", key, value)

		if component == "console-api" && cascadesToAllApps(key) {
			fmt.Printf("\n  ⚠  %s is read by the App controller. When console-api restarts,\n", key)
			fmt.Printf("     every App in the cluster will roll its pods to pick up the new value.\n")
			fmt.Printf("     For JVM apps with low CPU limits this can cause prolonged JIT throttling -\n")
			fmt.Printf("     consider switching them to the 'jvm' profile (burstable CPU) first.\n\n")
		}
	}

	// Trigger restart to pick up changes
	if deploy.Spec.Template.Annotations == nil {
		deploy.Spec.Template.Annotations = make(map[string]string)
	}
	deploy.Spec.Template.Annotations["kipper.run/restartedAt"] = time.Now().Format(time.RFC3339)

	if _, err := k8sClient.Clientset().AppsV1().Deployments(ns).Update(ctx, deploy, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating %s: %w", component, err)
	}

	fmt.Printf("\n  ✔  %s restarting with updated environment\n\n", component)
	return nil
}

func runClusterUse(_ *cobra.Command, args []string) error {
	name := args[0]

	var chosenName, chosenDomain string
	if err := config.Update(func(cfg *config.Config) error {
		cluster := cfg.GetCluster(name)
		if cluster == nil {
			for i := range cfg.Clusters {
				if strings.Contains(cfg.Clusters[i].Name, name) {
					cluster = &cfg.Clusters[i]
					break
				}
			}
		}
		if cluster == nil {
			fmt.Printf("\n  Cluster %q not found. Available clusters:\n\n", name)
			for _, c := range cfg.Clusters {
				fmt.Printf("    %s\n", c.Name)
			}
			fmt.Println()
			return fmt.Errorf("cluster %q not found", name)
		}
		chosenName, chosenDomain = cluster.Name, cluster.Domain
		cfg.CurrentCluster = cluster.Name
		return nil
	}); err != nil {
		return err
	}
	cluster := &config.Cluster{Name: chosenName, Domain: chosenDomain}

	fmt.Printf("\n  ✔  Switched to %s (%s)\n\n", cluster.Name, cluster.Domain)
	return nil
}

func runClusterExport(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	clusterName, _ := cmd.Flags().GetString("cluster")
	if clusterName == "" {
		clusterName = cfg.CurrentCluster
	}

	cluster := cfg.GetCluster(clusterName)
	if cluster == nil {
		return fmt.Errorf("cluster %q not found", clusterName)
	}

	kubeconfig, err := os.ReadFile(cluster.Kubeconfig)
	if err != nil {
		return fmt.Errorf("reading kubeconfig: %w", err)
	}

	fmt.Printf("# Kipper cluster export: share this file with team members\n")
	fmt.Printf("# Import with: kip cluster add <file>\n")
	fmt.Printf("name: %s\n", cluster.Name)
	fmt.Printf("provider: %s\n", cluster.Provider)
	fmt.Printf("host: %s\n", cluster.Host)
	fmt.Printf("domain: %s\n", cluster.Domain)
	if cluster.Org != "" {
		fmt.Printf("org: %s\n", cluster.Org)
	}
	fmt.Printf("kubeconfig: |\n")
	encoded := base64.StdEncoding.EncodeToString(kubeconfig)
	for i := 0; i < len(encoded); i += 76 {
		end := i + 76
		if end > len(encoded) {
			end = len(encoded)
		}
		fmt.Printf("  %s\n", encoded[i:end])
	}

	return nil
}

// clusterNamePattern is what a cluster name may be. An import takes its name
// from a file someone sent you, and that name becomes a path under
// ~/.kip/clusters, so a name carrying a separator or a parent reference would
// write wherever its author chose.
var clusterNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

// windowsDeviceNames cannot be file names on Windows even with an extension, so
// a cluster named for one imports on Linux and fails on a colleague's laptop.
var windowsDeviceNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// validateClusterName refuses a name that cannot safely become a file name.
func validateClusterName(name string) error {
	if !clusterNamePattern.MatchString(name) || strings.Contains(name, "..") {
		return fmt.Errorf("cluster name %q is not usable: names may hold letters, digits, dots, dashes and underscores, must start with a letter or digit, and cannot contain a path", name)
	}
	stem, _, _ := strings.Cut(strings.ToLower(name), ".")
	if windowsDeviceNames[stem] {
		return fmt.Errorf("cluster name %q is a reserved device name on Windows, where the kubeconfig could not be written. Pick another name", name)
	}
	return nil
}

// refuseAStolenKubeconfigPath stops an import from writing over the kubeconfig
// another cluster entry already points at. The path comes from the name, and on
// the case-folding filesystems macOS and Windows use by default, "Shop" and
// "shop" are two config entries over one file: one cluster's credentials then
// answer for the other.
func refuseAStolenKubeconfigPath(cfg *config.Config, name, path string) error {
	for i := range cfg.Clusters {
		other := &cfg.Clusters[i]
		if other.Name == name || other.Kubeconfig == "" {
			continue
		}
		if strings.EqualFold(filepath.Clean(other.Kubeconfig), filepath.Clean(path)) {
			return fmt.Errorf("refusing to import: %s is already the kubeconfig for cluster %q. Two clusters cannot share one file, and these names differ only in ways some filesystems ignore. Rename the existing cluster first with 'kip cluster rename'", path, other.Name)
		}
	}
	return nil
}

// refuseToReplaceACredential stops an import from overwriting a kubeconfig that
// holds something this import cannot put back. Three files can be at that path:
// none, which is the ordinary first import; one kip wrote, which carries no
// credential and is replaced freely, so re-importing an updated export keeps
// working; or one holding the cluster's admin certificate or another tool's
// credential plugin, which is the operator's way in and is refused.
//
// A file that cannot be parsed is refused as well. It may be any of the three,
// and guessing wrong costs an operator their access to the cluster.
func refuseToReplaceACredential(path string) error {
	// The path is <clusters dir>/<name>.yaml and the name has been through
	// validateClusterName, which admits no separator and no parent reference.
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) { //nolint:gosec // G703: name is validated above; the taint analysis cannot see it
		return nil
	}
	existing, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return fmt.Errorf("refusing to import: %s is already there and could not be read (%v), so kip cannot tell whether replacing it would cost you access to this cluster. Move it aside and run this again", path, err)
	}
	// Every entry, not only the active one: this replaces the whole file, so a
	// certificate parked in a context nobody is using is lost just as
	// completely as the one in front.
	for name, authInfo := range existing.AuthInfos {
		if authInfo == nil {
			continue
		}
		// A credential of any form is refused even when kip's own plugin sits
		// in the same entry: the plugin can be re-rendered, the credential
		// beside it cannot. Another tool's plugin is refused outright, since
		// kip never issued it and cannot put it back.
		if carriesCredential(authInfo) || (authInfo.Exec != nil && !installer.IsExactlyKipExec(authInfo)) {
			// A plugin that looks like kip's but is not exactly what this build
			// writes gets its own sentence. Calling an older kip's file "a
			// credential an export cannot reissue" sends the operator looking
			// for a secret that was never in it.
			if installer.IsKipExecAuthInfo(authInfo) {
				return fmt.Errorf("refusing to import: %s holds a kip credential plugin for %q that this build did not write, so kip cannot tell whether replacing it costs you access. It may come from another kip, or carry settings added by hand. Move it aside and run this again to replace it", path, name)
			}
			return fmt.Errorf("refusing to import: %s already holds a credential for %q that an export cannot reissue. It may be how this machine reaches the cluster. Move it aside and run this again to replace it", path, name)
		}
	}
	return nil
}

// carriesCredential reports whether an entry holds something that authenticates
// on its own, as opposed to a plugin invocation or nothing at all.
//
// Every form kubeconfig supports counts, not only the obvious ones. A refresh
// token sitting in auth-provider config, or a basic-auth password, is as much a
// credential as a client certificate, and missing one means both callers get it
// wrong in opposite directions: an export carrying it is accepted as
// credential-free, and a local file holding it is replaced as if empty.
func carriesCredential(authInfo *clientcmdapi.AuthInfo) bool {
	return len(authInfo.ClientCertificateData) > 0 || authInfo.ClientCertificate != "" ||
		len(authInfo.ClientKeyData) > 0 || authInfo.ClientKey != "" ||
		authInfo.Token != "" || authInfo.TokenFile != "" ||
		authInfo.Username != "" || authInfo.Password != "" ||
		authInfo.AuthProvider != nil
}

// rejectEmbeddedCredential refuses to import a kubeconfig that carries a
// long-lived credential (a client certificate/key, a static token, basic auth,
// or an auth-provider). The per-operator model keeps no shared admin credential
// on disk; importing one verbatim would put the shared system:masters
// certificate back on a machine and let it be re-exported. A credential-free
// exec kubeconfig (the install default) has none of these and imports fine.
// Operators authenticate as themselves with `kip auth login`.
func rejectEmbeddedCredential(kubeconfig []byte) error {
	cfg, err := clientcmd.Load(kubeconfig)
	if err != nil {
		return fmt.Errorf("parsing imported kubeconfig: %w", err)
	}
	for name, ai := range cfg.AuthInfos {
		if ai == nil {
			continue
		}
		if carriesCredential(ai) {
			return fmt.Errorf("refusing to import kubeconfig: user %q carries an embedded credential (client certificate/key or token). Kipper uses per-operator OIDC login. Import a credential-free export, then run `kip auth login` against the cluster", name)
		}
	}
	return nil
}

// rejectMismatchedClusterPin refuses an export whose kubeconfig asks kip for a
// different cluster's session than the export claims to be. The credential
// plugin serves whichever session the exec args name, so a file that says it
// is one cluster while pinning another would have kubectl hand that other
// cluster's token to this one's API server — a stranger's export naming a
// cluster of yours is the one way that argument arrives without you writing
// it. The two agreeing is the whole check.
func rejectMismatchedClusterPin(kubeconfig []byte, domain string) error {
	cfg, err := clientcmd.Load(kubeconfig)
	if err != nil {
		return fmt.Errorf("parsing imported kubeconfig: %w", err)
	}
	for name, ai := range cfg.AuthInfos {
		if ai == nil || ai.Exec == nil {
			continue
		}
		pinned := pinnedClusterDomains(ai.Exec.Args)
		if len(pinned) > 1 {
			return fmt.Errorf("refusing to import kubeconfig: user %q names a cluster more than once (%s), so what it asks for is not what it appears to ask for. Re-export the cluster from a machine where it works", name, strings.Join(pinned, ", "))
		}
		if len(pinned) == 0 || pinned[0] == domain {
			continue
		}
		return fmt.Errorf("refusing to import kubeconfig: user %q asks for the session of cluster %q while the export declares %q. Re-export the cluster from a machine where it works", name, pinned[0], domain)
	}
	return nil
}

// pinnedClusterDomains returns every domain an exec kubeconfig pins, in the
// order they appear.
//
// Every one of them matters, because pflag resolves a repeated flag to the
// last occurrence: reading only the first would let a file pin the domain it
// declares, then pin a second the plugin actually serves. A file kip writes
// carries exactly one, so more than one is refused rather than resolved.
func pinnedClusterDomains(args []string) []string {
	var pinned []string
	for i, arg := range args {
		if value, ok := strings.CutPrefix(arg, "--cluster-domain="); ok {
			pinned = append(pinned, value)
			continue
		}
		if arg == "--cluster-domain" && i+1 < len(args) {
			pinned = append(pinned, args[i+1])
		}
	}
	return pinned
}

func runClusterAdd(cmd *cobra.Command, args []string) error {
	filePath := args[0]
	setCurrent, _ := cmd.Flags().GetBool("set-current")

	data, err := os.ReadFile(filePath) //nolint:gosec // user-provided file path is intentional
	if err != nil {
		return fmt.Errorf("reading file: %w", err)
	}

	var name, provider, host, domain, org string
	// Whether the export carried an org at all, which is not the same as
	// carrying an empty one: the export omits the key when a cluster has no org,
	// so a merge that wrote org unconditionally would strip it from an entry
	// that has one — and the org prefixes every namespace kip resolves.
	orgCarried := false
	inKubeconfig := false
	var kubeconfigLines []string

	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || trimmed == "" {
			continue
		}

		if inKubeconfig {
			if len(line) > 0 && line[0] != ' ' && strings.Contains(line, ":") {
				inKubeconfig = false
			} else {
				kubeconfigLines = append(kubeconfigLines, strings.TrimSpace(line))
				continue
			}
		}

		parts := strings.SplitN(trimmed, ": ", 2)
		if len(parts) != 2 {
			if strings.HasSuffix(trimmed, "|") {
				key := strings.TrimSuffix(strings.TrimSpace(trimmed), ": |")
				if key == "kubeconfig" {
					inKubeconfig = true
				}
			}
			continue
		}
		key := parts[0]
		val := strings.TrimSpace(parts[1])
		if val == "|" {
			if key == "kubeconfig" {
				inKubeconfig = true
			}
			continue
		}

		switch key {
		case "name":
			name = val
		case "provider":
			provider = val
		case "host":
			host = val
		case "domain":
			domain = val
		case "org":
			org = val
			orgCarried = true
		}
	}

	if name == "" || host == "" {
		return fmt.Errorf("invalid export file: missing name or host")
	}

	kubeconfigB64 := strings.Join(kubeconfigLines, "")
	kubeconfig, err := base64.StdEncoding.DecodeString(kubeconfigB64)
	if err != nil {
		return fmt.Errorf("decoding kubeconfig: %w", err)
	}
	if err := validateClusterName(name); err != nil {
		return err
	}
	if err := rejectEmbeddedCredential(kubeconfig); err != nil {
		return err
	}
	if err := rejectMismatchedClusterPin(kubeconfig, domain); err != nil {
		return err
	}

	// The bundle's own kubeconfig is never written. Only its server address and
	// cluster authority survive, re-rendered around kip's credential plugin, so
	// an exec stanza someone put in the file they sent you has nothing to run.
	renderedKubeconfig, err := installer.RenderImportedKubeconfig(domain, string(kubeconfig))
	if err != nil {
		return err
	}
	rendered := []byte(renderedKubeconfig)

	dir, err := config.Dir()
	if err != nil {
		return err
	}
	clustersDir := dir + "/clusters"
	if err := os.MkdirAll(clustersDir, 0o700); err != nil {
		return fmt.Errorf("creating clusters directory: %w", err)
	}

	kubeconfigPath := fmt.Sprintf("%s/%s.yaml", clustersDir, name)

	becameCurrent := false
	if err := config.Update(func(cfg *config.Config) error {
		// The kubeconfig is written here rather than before the lock. Its path
		// is derived from the cluster name, so two imports of the same name
		// share it: publishing outside the lock lets one write the file and the
		// other write the entry, leaving a config that describes one cluster
		// beside credentials that reach another.
		// Atomic: writing in place truncates first, so a failure partway leaves
		// a live cluster's own kubeconfig corrupt while its entry still names it.
		// An export carries no credential, so writing it over one is a
		// one-way trade: the file at this path may be how this machine
		// reaches the cluster, and nothing in the import can reissue it.
		if err := refuseAStolenKubeconfigPath(cfg, name, kubeconfigPath); err != nil {
			return err
		}
		if err := refuseToReplaceACredential(kubeconfigPath); err != nil {
			return err
		}
		if err := installer.WriteFileAtomic(kubeconfigPath, rendered, 0o600); err != nil {
			return fmt.Errorf("writing kubeconfig: %w", err)
		}
		// Merge rather than replace. An export carries the connection details and
		// nothing else, so replacing the whole entry silently drops everything a
		// re-import cannot know: the gateway credential mirror that releases a
		// name when the cluster is gone, the backup storage the next upgrade
		// renders Velero against, the SSH key, the project context. Re-importing
		// an updated export of a cluster you already use is the ordinary way to
		// pick up a changed domain, and it must not cost any of those.
		if entry := cfg.GetCluster(name); entry != nil {
			entry.Provider = provider
			entry.Host = host
			entry.Domain = domain
			entry.Kubeconfig = kubeconfigPath
			if orgCarried {
				// The display name only becomes wrong when the org itself
				// changes. Clearing it every time costs an operator the name of
				// the org they are still in, on the routine re-import of their
				// own cluster, because an export cannot carry it back.
				if entry.Org != org {
					entry.OrgDisplayName = ""
				}
				entry.Org = org
			}
			// An export asserts a cluster that answers, which is the direct
			// contradiction of a marker saying its host was wiped. Leaving it
			// set would send the next uninstall of a freshly rebuilt cluster
			// past the wipe and into a release it cannot make, with no way out
			// but hand-editing the config.
			entry.HostWiped = false
		} else {
			cfg.Clusters = append(cfg.Clusters, config.Cluster{
				Name:       name,
				Provider:   provider,
				Host:       host,
				Domain:     domain,
				Kubeconfig: kubeconfigPath,
				Org:        org,
			})
		}
		if setCurrent || cfg.CurrentCluster == "" {
			cfg.CurrentCluster = name
		}
		// Including the case where this cluster was already the current one,
		// which re-importing an updated export of your active cluster hits.
		becameCurrent = cfg.CurrentCluster == name
		return nil
	}); err != nil {
		return err
	}

	fmt.Printf("\n  ✔  Cluster %s added\n", name)
	fmt.Printf("  Host: %s\n", host)
	fmt.Printf("  Domain: %s\n", domain)
	if becameCurrent {
		fmt.Printf("  Active: yes\n")
	}
	fmt.Print("\n")

	return nil
}

func runClusterRemove(_ *cobra.Command, args []string) error {
	name := args[0]

	if err := config.Update(func(cfg *config.Config) error {
		found := false
		var remaining []config.Cluster
		for _, c := range cfg.Clusters {
			if c.Name == name {
				found = true
				_ = os.Remove(c.Kubeconfig)
				continue
			}
			remaining = append(remaining, c)
		}
		if !found {
			return fmt.Errorf("cluster %q not found", name)
		}

		cfg.Clusters = remaining
		if cfg.CurrentCluster == name {
			if len(cfg.Clusters) > 0 {
				cfg.CurrentCluster = cfg.Clusters[0].Name
			} else {
				cfg.CurrentCluster = ""
			}
		}
		return nil
	}); err != nil {
		return err
	}

	fmt.Printf("\n  ✔  Cluster %s removed\n\n", name)
	return nil
}

func runClusterRename(_ *cobra.Command, args []string) error {
	oldName := args[0]
	newName := args[1]

	// The new name becomes a file name under ~/.kip/clusters, exactly as an
	// import's does, so it is checked the same way. Without this, a name
	// carrying a parent reference moves the kubeconfig out of the managed
	// directory to wherever the name points.
	if err := validateClusterName(newName); err != nil {
		return err
	}

	var actualOldName string
	if err := config.Update(func(cfg *config.Config) error {
		cluster := findCluster(cfg, oldName)
		if cluster == nil {
			return fmt.Errorf("cluster %q not found", oldName)
		}
		if cfg.GetCluster(newName) != nil {
			return fmt.Errorf("cluster %q already exists", newName)
		}
		actualOldName = cluster.Name

		// Move the kubeconfig file to match the new name when it lives in
		// the kip-managed clusters directory. We don't touch user-supplied
		// kubeconfigs that point elsewhere — those are the user's to manage.
		if cluster.Kubeconfig != "" {
			newPath, renameErr := renameKubeconfigFile(cluster.Kubeconfig, newName, cluster.Domain)
			if renameErr != nil {
				return fmt.Errorf("renaming kubeconfig: %w", renameErr)
			}
			cluster.Kubeconfig = newPath
		}

		cluster.Name = newName
		if cfg.CurrentCluster == actualOldName {
			cfg.CurrentCluster = newName
		}
		return nil
	}); err != nil {
		return err
	}

	fmt.Printf("\n  ✔  Renamed %s → %s\n\n", actualOldName, newName)
	return nil
}

// renameKubeconfigFile moves the cluster's kubeconfig to a filename
// derived from the new cluster name, but only when it lives in the
// kip-managed ~/.kip/clusters/ directory. Files in other locations
// keep their original path (the user is presumably managing them).
func renameKubeconfigFile(oldPath, newName, domain string) (string, error) {
	dir := filepath.Dir(oldPath)
	home, err := os.UserHomeDir()
	if err != nil {
		//nolint:nilerr // best-effort: if we cannot resolve $HOME, leave kubeconfig where it is rather than failing the rename
		return oldPath, nil
	}
	managedDir := filepath.Join(home, ".kip", "clusters")
	absDir, _ := filepath.Abs(dir)
	absManaged, _ := filepath.Abs(managedDir)
	if absDir != absManaged {
		return oldPath, nil
	}
	newPath := filepath.Join(dir, newName+".yaml")
	// Belt and braces behind validateClusterName: this function moves a file,
	// and the check that it lands inside the managed directory belongs next to
	// the move rather than only at the command's edge.
	if filepath.Dir(newPath) != dir {
		return "", fmt.Errorf("refusing to move the kubeconfig outside %s", dir)
	}
	if newPath == oldPath {
		return oldPath, nil
	}
	if _, err := os.Stat(oldPath); errors.Is(err, os.ErrNotExist) {
		// The file the entry names is gone. A file already at the destination
		// may be what an earlier attempt moved there before failing to record
		// it — in which case adopting it is the only way a re-run repairs the
		// entry — or it may be an unrelated leftover, and adopting that would
		// point kubectl at somebody else's cluster. Nothing about the two
		// states differs on disk, so ask the file: the one this rename moved
		// pins the domain of the cluster being renamed.
		if _, destErr := os.Stat(newPath); errors.Is(destErr, os.ErrNotExist) {
			// Neither file is there. Nothing to rename and nothing ambiguous
			// about it — the entry names a kubeconfig that was already gone, and
			// the rename says so by leaving that path alone.
			return oldPath, nil //nolint:nilerr // destErr is the absence being tested for, not a failure
		}
		if pinsDomain(newPath, domain) {
			return newPath, nil
		}
		// A file is sitting at the destination and nothing about it says it is
		// this cluster's. That is not the same as having nothing to rename: it
		// is either a collision or a move some earlier attempt made and could
		// not record, and committing the rename either way leaves the entry
		// naming a file that is not there. Say so and change nothing.
		return oldPath, fmt.Errorf("%s is missing and %s already exists but does not identify itself as %s. "+
			"If %s is this cluster's: a kubeconfig from before the exec pin, or one an interrupted rename moved, "+
			"move it back to %s and re-run. If it belongs to something else, move it out of the way first",
			oldPath, newPath, domain, newPath, oldPath)
	}
	// A case-only rename is the one case where the destination "already exists"
	// and is the very file being renamed: macOS and Windows fold case, so
	// Stat finds Shop.yaml when asked for shop.yaml. Refusing there would make
	// `kip cluster rename Shop shop` impossible on the machines people use.
	if !strings.EqualFold(newPath, oldPath) {
		if _, err := os.Stat(newPath); err == nil {
			return oldPath, fmt.Errorf("destination %s already exists", newPath)
		}
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		return oldPath, err
	}
	return newPath, nil
}

func runClusterDomain(cmd *cobra.Command, args []string) error {
	repair, _ := cmd.Flags().GetBool("repair")
	sync, _ := cmd.Flags().GetBool("sync")
	rollback, _ := cmd.Flags().GetBool("rollback")

	// The modes are mutually exclusive, and only the forward flow takes a domain.
	modes := 0
	for _, on := range []bool{repair, sync, rollback} {
		if on {
			modes++
		}
	}
	if modes > 1 {
		return fmt.Errorf("--repair, --sync, and --rollback cannot be combined")
	}
	if (repair || sync || rollback) && len(args) > 0 {
		return fmt.Errorf("--repair, --sync, and --rollback take no domain argument")
	}
	if ack, _ := cmd.Flags().GetBool("ack-sso-callbacks"); ack && repair {
		return fmt.Errorf("--ack-sso-callbacks applies to a cutover, not --repair")
	}

	switch {
	case repair:
		return runClusterDomainRepair()
	case sync:
		return runClusterDomainSync(cmd)
	case rollback:
		return runClusterDomainRollback(cmd)
	}

	if len(args) != 1 {
		return fmt.Errorf("requires a domain argument, or one of --sync, --rollback, --repair")
	}
	// Canonicalise once, here, so every step of the move sees the same spelling:
	// the gateway suffix test, the label claimed from the gateway, and the spec
	// patch, which the CRD accepts in lowercase only. Normalising further in
	// would leave the patch carrying whatever was typed.
	return runClusterDomainForward(cmd, installer.NormaliseDomain(args[0]))
}

// cascadesToAllApps reports whether the given env var on console-api will
// cause the App controller to re-sync every App when console-api restarts —
// rolling all pods at once. The list is intentionally explicit so we don't
// silently warn for unrelated user-set env vars.
func cascadesToAllApps(key string) bool {
	switch key {
	case "SIDECAR_IMAGE", "DOMAIN", "DEFAULT_REGISTRY":
		return true
	}
	return false
}

// runClusterDomainRepair rewrites the local ~/.kip/config.yaml entry for the
// current cluster from the ClusterIdentity CR, the authoritative record of the
// serving identity. Used when the local state drifts (an interrupted `kip
// cluster domain`, a config copied from another machine).
//
// The CR is read as one coherent snapshot; live Ingress rules are never
// consulted, because mid-transition they intentionally carry both the old and
// the new host sets and mixing the two is exactly the corruption repair exists
// to fix. A CR that cannot be read is an error, never a silent fallback to
// weaker inference.
//
// We never modify cluster state from this command: only ~/.kip/config.yaml.
func runClusterDomainRepair() error {
	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ci, err := clusteridentity.New(k8sClient.Dynamic()).Get(ctx)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("this cluster has no ClusterIdentity yet; run 'kip upgrade' to deploy the serving-identity reconciler, then retry")
		}
		return fmt.Errorf("reading serving identity: %w", err)
	}
	identity := repairIdentity(ci)
	if identity == nil || identity.Domain == "" {
		return fmt.Errorf("the ClusterIdentity carries no usable identity yet; let the reconciler settle ('kip cluster domain --sync'), then retry")
	}
	if ci.Status.Transition != nil {
		fmt.Printf("\n  A change is in flight (phase %s); the config is repaired to the identity\n", ci.Phase())
		fmt.Printf("  login currently uses. Re-run --repair once the change settles.\n")
	}

	hosts := serving.ResolveHosts(identity.Domain, overridesOf(identity.Hosts))
	repaired := repairedFields{
		Domain:           identity.Domain,
		ConsoleDomain:    overrideIfDifferent("console", identity.Domain, hosts.Console),
		ConsoleAPIDomain: overrideIfDifferent("console-api", identity.Domain, hosts.ConsoleAPI),
		DexDomain:        overrideIfDifferent("dex", identity.Domain, hosts.Dex),
		Kubeconfig:       cluster.Kubeconfig,
	}
	// The local gateway token is the disaster-recovery mirror of the cluster's
	// gateway-credentials Secret, so repair refreshes it too — an interrupted
	// token rotation otherwise leaves the mirror stale forever, because a
	// same-IP re-registration never re-discloses the token.
	repaired.GatewayToken = clusterGatewayToken(k8sClient.Clientset())

	repinned, err := persistRepairedCluster(cluster.Name, repaired)
	if err != nil {
		return err
	}

	shown := config.Cluster{
		Domain: repaired.Domain, ConsoleDomain: repaired.ConsoleDomain,
		ConsoleAPIDomain: repaired.ConsoleAPIDomain, DexDomain: repaired.DexDomain,
	}
	fmt.Printf("\n  Repaired local config for %s:\n", cluster.Name)
	fmt.Printf("    Domain           = %s\n", shown.Domain)
	fmt.Printf("    ConsoleDomain    = %s\n", displayOrConvention(shown.ConsoleDomain, shown.ConsoleHost()))
	fmt.Printf("    ConsoleAPIDomain = %s\n", displayOrConvention(shown.ConsoleAPIDomain, shown.ConsoleAPIHost()))
	fmt.Printf("    DexDomain        = %s\n", displayOrConvention(shown.DexDomain, shown.DexHost()))
	if repinned {
		fmt.Printf("    kubectl now signs in against %s\n", repaired.Domain)
	}
	fmt.Println()
	return nil
}

// persistRepairedCluster writes a repaired cluster entry to disk: the config
// itself, and the pin in its kubeconfig that has to name the same domain.
//
// The kubeconfig goes first, because the credential plugin asks for a session
// by domain and a failure part-way must leave the file that still works alone.
// Two files cannot be written as one transaction, so the remaining case is
// reported rather than papered over: a repin that lands while the config save
// fails leaves kubectl asking for a domain the config does not list yet, which
// refuses rather than serving the wrong session, and re-running the repair
// settles it.
// repairedFields is what a repair owns: the serving identity read off the
// cluster, and the gateway credential when the cluster still had one to give.
//
// It is a field list rather than a whole config.Cluster because a repair takes
// seconds of network round trips, and writing back an entry captured before them
// would restore everything else as it looked then — a token a concurrent
// uninstall mirrored in the meantime included, which by then can be its only
// copy. Repair's job is to rewrite this cluster's identity from the CR, not to
// have an opinion about the rest of its entry.
type repairedFields struct {
	Domain           string
	ConsoleDomain    string
	ConsoleAPIDomain string
	DexDomain        string
	Kubeconfig       string
	// GatewayToken is empty when the cluster could not be read for one, which
	// leaves whatever is recorded alone rather than clearing it.
	GatewayToken string
}

func persistRepairedCluster(name string, repaired repairedFields) (repinned bool, err error) {
	if repaired.Kubeconfig != "" {
		repinned, err = installer.RepinExecKubeconfig(repaired.Domain, repaired.Kubeconfig)
		if err != nil {
			return false, fmt.Errorf("repointing kubeconfig at %s: %w", repaired.Domain, err)
		}
	}

	if err := config.Update(func(cfg *config.Config) error {
		entry := cfg.GetCluster(name)
		if entry == nil {
			return fmt.Errorf("cluster %q is no longer in the local config", name)
		}
		entry.Domain = repaired.Domain
		entry.ConsoleDomain = repaired.ConsoleDomain
		entry.ConsoleAPIDomain = repaired.ConsoleAPIDomain
		entry.DexDomain = repaired.DexDomain
		if repaired.GatewayToken != "" {
			entry.GatewayToken = repaired.GatewayToken
		}
		return nil
	}); err != nil {
		if repinned {
			return repinned, fmt.Errorf("saving config: %w (the kubeconfig now points at %s; re-run 'kip cluster domain --repair')", err, repaired.Domain)
		}
		return repinned, fmt.Errorf("saving config: %w", err)
	}
	return repinned, nil
}

// repairIdentity picks the coherent identity snapshot local-config repair
// writes: the spec identity when steady, the transition's target once the
// issuer has flipped, and the outgoing steady identity before the flip and
// while reverting — whichever identity login is actually using at that
// moment. CuttingOver spans the flip itself, so within it the persisted
// cutoverStartedAt marker (stamped the moment the new Dex config is durably
// written) decides which side of the flip the cluster is on.
func repairIdentity(ci *clusteridentity.ClusterIdentity) *clusteridentity.SteadyIdentity {
	t := ci.Status.Transition
	if t == nil {
		return &clusteridentity.SteadyIdentity{Domain: ci.Spec.Domain, Hosts: ci.Spec.Hosts}
	}
	switch t.Phase {
	case clusteridentity.PhaseCuttingOver:
		if t.CutoverStartedAt == nil {
			return ci.Status.Steady
		}
		return t.ToIdentity
	case clusteridentity.PhaseVerifying, clusteridentity.PhaseContracting:
		return t.ToIdentity
	default:
		// DualServe, AwaitingApproval, Reverting, Degraded: the outgoing
		// identity still authenticates.
		return ci.Status.Steady
	}
}

// overrideIfDifferent returns the override host when it differs from
// the SubdomainFor convention for the prefix+bareDomain pair. Returns
// empty (meaning "use convention") when they match.
func overrideIfDifferent(prefix, bareDomain, host string) string {
	conventional := installer.SubdomainFor(prefix, bareDomain)
	if host == conventional {
		return ""
	}
	return host
}

func displayOrConvention(override, resolved string) string {
	if override == "" {
		return resolved + " (default)"
	}
	return resolved + " (override)"
}

// pinsDomain reports that the exec kubeconfig at path asks kip for the session
// of exactly this cluster and no other.
//
// Two things have to hold. No user may name a different cluster — the same test
// rejectMismatchedClusterPin applies to an import, because a file where one user
// names this cluster and another names a different one reaches whichever the
// context selects. And the user the live context selects must name this one: a
// matching pin sitting in a user nothing uses proves only that the file has been
// near this cluster, while kubectl follows the context.
//
// "Live" is installer.ActiveContext's answer — the current context, or the only
// one when the file names none, because a single-context file is not ambiguous
// about which entry is live and the rewriter has always read it that way.
//
// Unreadable, unparsable, no resolvable context, a context selecting no user or
// one with no exec stanza, or a pin naming anything else all answer no. The only
// question being asked is whether adopting the file is safe, and every one of
// those is a reason it is not.
func pinsDomain(path, domain string) bool {
	if domain == "" {
		return false
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is inside the kip-managed clusters directory
	if err != nil {
		return false
	}
	if err := rejectMismatchedClusterPin(data, domain); err != nil {
		return false
	}
	cfg, err := clientcmd.Load(data)
	if err != nil {
		return false
	}
	// The same function that decides which context a rewrite keeps, rather than
	// a copy of its rule. Two answers to "which context is live" is the
	// confusion this function was fixed for, and a duplicated rule is how the
	// second answer comes back.
	kubeContext, _ := installer.ActiveContext(cfg)
	if kubeContext.AuthInfo == "" {
		// Nothing resolved, or a context selecting no user. The empty name is a
		// real key in this map — clientcmd builds it from the file without
		// validating names — so a user declared with an empty name would
		// otherwise answer for a context that resolves to nothing.
		return false
	}
	ai := cfg.AuthInfos[kubeContext.AuthInfo]
	if ai == nil || ai.Exec == nil {
		return false
	}
	pinned := pinnedClusterDomains(ai.Exec.Args)
	return len(pinned) == 1 && pinned[0] == domain
}
