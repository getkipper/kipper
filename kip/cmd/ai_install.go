package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/kip/internal/ai"
	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
)

var aiInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install a private LLM and chat UI in the cluster",
	Long: `Install the Kipper AI bundle: Ollama (model serving) and
LibreChat (chat UI). Exposes an OpenAI-compatible API your apps can call
at http://ollama.kipper-ai.svc.cluster.local:11434/v1 and a chat UI on
chat.<your-domain>.

Document RAG (chat-with-your-docs) lands in the Phase 2 RAG bundle, not
in Phase 1.

The cluster must have at least 8 GiB of free memory on a single node.
Larger memory and a GPU unlock larger models automatically.

Examples:
  kip ai install
  kip ai install --host chat.example.com
  kip ai install --model qwen2.5:7b-instruct-q4_K_M --yes`,
	RunE: runAIInstall,
}

func init() {
	aiInstallCmd.Flags().String("host", "", "external chat UI hostname (defaults to chat.<cluster-domain>)")
	aiInstallCmd.Flags().String("model", "", "Ollama model tag to preload (defaults to a tier-appropriate Qwen 2.5 model)")
	aiInstallCmd.Flags().String("pvc-size", "", "model cache PVC size (defaults to 10Gi/30Gi/60Gi by tier)")
	aiInstallCmd.Flags().BoolP("yes", "y", false, "skip the auto-configure prompt and accept all defaults")
	aiInstallCmd.Flags().Bool("skip-storage-check", false, "skip the MinIO storage preflight (only for evaluation installs that will not run kip ai backup)")
	aiCmd.AddCommand(aiInstallCmd)
}

func runAIInstall(cmd *cobra.Command, _ []string) error {
	cluster, client, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	host, _ := cmd.Flags().GetString("host")
	if host == "" {
		host, err = defaultChatHost(cluster)
		if err != nil {
			return err
		}
	}
	model, _ := cmd.Flags().GetString("model")
	pvcSize, _ := cmd.Flags().GetString("pvc-size")
	yes, _ := cmd.Flags().GetBool("yes")
	skipStorageCheck, _ := cmd.Flags().GetBool("skip-storage-check")

	ctx := context.Background()

	fmt.Printf("\n  Inspecting cluster capacity...\n")
	report, err := ai.Detect(ctx, client.Clientset())
	if err != nil {
		return fmt.Errorf("detecting cluster tier: %w", err)
	}
	tier := report.Tier()
	fmt.Printf("  ✔   Detected %s, best node %q has %s free (cluster total %s across %d node(s))\n",
		tier, report.BestNode.Name, humanBytes(report.BestNode.FreeMemory),
		humanBytes(report.TotalFree), len(report.Nodes))

	// If the bundle is already installed, skip the tier-refusal gate.
	// The bundle's own pods consume the memory budget we are checking
	// against, so a rerun (reconciliation) would otherwise fail
	// preflight on a perfectly healthy install.
	alreadyInstalled, err := bundleAlreadyInstalled(ctx, client.Clientset())
	if err != nil {
		return fmt.Errorf("checking existing bundle: %w", err)
	}

	if !alreadyInstalled && !tier.MeetsMinimum() {
		return fmt.Errorf("best node has %s free, AI bundle needs at least 8 GiB on a single node; "+
			"run 'kip ai configure --provider claude' to use a hosted provider instead",
			humanBytes(report.BestNode.FreeMemory))
	}
	if alreadyInstalled {
		fmt.Printf("  ✔   Bundle already installed; reconciling existing resources\n")
		// Use a non-None tier so default-model selection still works in
		// the reconcile path, even if the live cluster looks like
		// TierNone because the bundle's own pods consume the budget.
		if tier == ai.TierNone {
			tier = ai.TierOne
		}
	}

	// Storage preflight runs only on a fresh install. On reconcile the
	// bundle's PVCs already exist, and refusing reconcile because MinIO
	// is undersized would block legitimate config reapplies. Operators
	// who want to skip the check on a fresh install (evaluation boxes
	// that will never run `kip ai backup`) can pass --skip-storage-check.
	if !alreadyInstalled && !skipStorageCheck {
		if err := checkBackupStorage(ctx, client.Clientset(), pvcSize, tier); err != nil {
			return err
		}
	}

	fmt.Printf("\n  Installing AI bundle on %s\n\n", cluster.Name)

	installer := &ai.Installer{
		Clientset: client.Clientset(),
		Dynamic:   client.Dynamic(),
		Out:       os.Stdout,
	}
	if err := installer.Install(ctx, tier, ai.Options{
		Host:     host,
		NodeName: report.BestNode.Name,
		Model:    model,
		PVCSize:  pvcSize,
	}); err != nil {
		return err
	}

	chatURL := "https://" + host
	fmt.Printf("\n  ✔  AI bundle installed\n")
	fmt.Printf("  Chat UI:   %s\n", chatURL)
	fmt.Printf("  Cluster API: http://ollama.%s.svc.cluster.local:11434/v1\n\n", ai.Namespace)

	printChatBootstrapInstructions(os.Stdout)

	return offerAutoConfigure(cmd.InOrStdin(), os.Stdout, yes, model, tier)
}

// printChatBootstrapInstructions tells the user how to seed their first
// admin account. Open registration is disabled in the LibreChat values,
// so nobody can sign up through the public Ingress until the operator
// runs this one-time command.
func printChatBootstrapInstructions(out io.Writer) {
	_, _ = fmt.Fprintln(out, "  Create your admin account before opening the chat URL:")
	_, _ = fmt.Fprintln(out, "    kip ai admin create \\")
	_, _ = fmt.Fprintln(out, "      --email you@example.com \\")
	_, _ = fmt.Fprintln(out, "      --name 'Your Name' \\")
	_, _ = fmt.Fprintln(out, "      --password 'pick-a-strong-password'")
	_, _ = fmt.Fprintln(out, "  Open registration is disabled, so nobody else can sign up first.")
	_, _ = fmt.Fprintln(out)
}

// defaultChatHost derives the chat UI hostname from the cluster's domain
// via the SubdomainFor convention (chat--<cluster>.kipper.run on free
// subdomains, chat.<domain> on custom domains).
func defaultChatHost(cluster *config.Cluster) (string, error) {
	if cluster.Domain == "" {
		return "", fmt.Errorf("--host is required (cluster has no domain configured)")
	}
	return installer.SubdomainFor("chat", cluster.Domain), nil
}

// offerAutoConfigure prompts the user to repoint kip's AI client at the
// in-cluster Ollama. It runs when AI is unset entirely, or when AI is set
// to the Ollama provider with no URL or the loopback default. A custom
// non-loopback Ollama URL or any other provider is left untouched.
func offerAutoConfigure(in io.Reader, out io.Writer, assumeYes bool, model string, tier ai.Tier) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if !shouldRewireAIClient(&cfg.AI) {
		_, _ = fmt.Fprintf(out, "  AI client already configured (provider: %s), leaving alone.\n\n", cfg.AI.Provider)
		return nil
	}

	if !assumeYes {
		_, _ = fmt.Fprint(out, "  Use this Ollama for kip's own AI features (log analysis, Dockerfile generation)? [Y/n]: ")
		reader := bufio.NewReader(in)
		ans, _ := reader.ReadString('\n')
		ans = strings.ToLower(strings.TrimSpace(ans))
		if ans == "n" || ans == "no" {
			_, _ = fmt.Fprintln(out)
			return nil
		}
	}

	configuredModel := model
	if configuredModel == "" {
		configuredModel = tier.DefaultModel()
	}
	const localOllamaURL = "http://localhost:11434"
	clusterOllamaURL := fmt.Sprintf("http://ollama.%s.svc.cluster.local:11434", ai.Namespace)

	// Local kip CLI runs on the operator's laptop and cannot resolve the
	// cluster-internal DNS name. Default the local URL to localhost so a
	// `kubectl port-forward` works, and surface the port-forward command
	// so the user knows what to do.
	cfg.AI = config.AIConfig{
		Provider:  "ollama",
		Model:     configuredModel,
		OllamaURL: localOllamaURL,
	}
	if err := saveAISettings(cfg.AI); err != nil {
		return err
	}

	// In-cluster apps (the console-api, etc.) reach Ollama at the
	// cluster Service, so the cluster Secret carries the cluster URL.
	clusterCfg := *cfg
	clusterCfg.AI.OllamaURL = clusterOllamaURL
	if err := syncAIToCluster(&clusterCfg); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "  ✔   kip AI client wired to Ollama (model: %s)\n", configuredModel)
	_, _ = fmt.Fprintf(out, "      In-cluster apps reach it at %s\n", clusterOllamaURL)
	_, _ = fmt.Fprintln(out, "      Local kip CLI features need a port-forward:")
	_, _ = fmt.Fprintf(out, "        kubectl port-forward -n %s svc/ollama 11434:11434\n\n", ai.Namespace)
	return nil
}

// bundleAlreadyInstalled returns true when the AI bundle's namespace
// already exists and is Kipper-owned. Reruns of `kip ai install`
// against this state are reconciliation, not a fresh install, so the
// tier-refusal gate must not fire (the bundle's own pods would push
// free memory below the threshold and falsely fail preflight).
func bundleAlreadyInstalled(ctx context.Context, clientset kubernetes.Interface) (bool, error) {
	ns, err := clientset.CoreV1().Namespaces().Get(ctx, ai.Namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return ns.Labels["app.kubernetes.io/managed-by"] == "kipper", nil
}

// shouldRewireAIClient returns true when the install flow should offer to
// point kip's AI client at the in-cluster Ollama. Eligible cases:
//   - AI is genuinely unset (empty Provider)
//   - AI is set to Ollama with no URL or the loopback default
//
// An explicit "none" provider means the user opted out of AI features.
// We respect that and never auto-rewire to Ollama on their behalf, even
// with --yes. Any other configured provider is also left alone.
func shouldRewireAIClient(cfg *config.AIConfig) bool {
	if cfg.Provider == "" {
		return true
	}
	if cfg.Provider == "none" {
		return false
	}
	if cfg.Provider != "ollama" {
		return false
	}
	url := strings.TrimRight(cfg.OllamaURL, "/")
	return url == "" || url == "http://localhost:11434" || url == "http://127.0.0.1:11434"
}

// humanBytes formats a byte count in GiB / MiB for status messages.
func humanBytes(b int64) string {
	const gib = 1024 * 1024 * 1024
	const mib = 1024 * 1024
	if b >= gib {
		return fmt.Sprintf("%.1f GiB", float64(b)/float64(gib))
	}
	return fmt.Sprintf("%d MiB", b/mib)
}

// checkBackupStorage runs the MinIO storage preflight before a fresh
// install. The bundle's backup story (`kip ai backup` via Velero) needs
// the velero/minio-storage PVC to be at least 3x the model cache size,
// which covers a snapshot at parity with the source plus headroom for
// incremental layers and Kopia's working overhead. Refuses install when
// MinIO is undersized so users hit the issue before they have data they
// care about, not the first time they reach for `kip ai backup`. Skips
// silently when Velero is not installed yet (e.g. older clusters), so
// the AI bundle remains installable without backup support.
func checkBackupStorage(ctx context.Context, clientset kubernetes.Interface, pvcSize string, tier ai.Tier) error {
	resolved := pvcSize
	if resolved == "" {
		resolved = ai.DefaultPVCFor(tier)
	}
	report, err := ai.CheckBackupStorage(ctx, clientset, resolved)
	if errors.Is(err, ai.ErrMinIONotFound) {
		fmt.Printf("  ⚠   Velero/MinIO not detected; skipping backup storage check (kip ai backup will not work until Velero is installed)\n")
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking backup storage: %w", err)
	}
	if !report.Sufficient {
		return fmt.Errorf(
			"MinIO has %s capacity, AI bundle backups need ~%s for the %s model cache. "+
				"Resize the velero/minio-storage PVC, or pass --skip-storage-check to install without backup support",
			humanBytes(report.MinIOCapacity), humanBytes(report.Projected), resolved,
		)
	}
	fmt.Printf("  ✔   MinIO has %s capacity, projected backup needs %s\n",
		humanBytes(report.MinIOCapacity), humanBytes(report.Projected))
	return nil
}
