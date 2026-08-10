package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/kip/internal/ai"
	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/k8s"
)

var aiCmd = &cobra.Command{
	Use:   "ai",
	Short: "Configure AI assistant",
}

var aiConfigureCmd = &cobra.Command{
	Use:   "configure",
	Short: "Set up the AI provider for code assistance, log analysis, and diagnostics",
	Long: `Configure which AI provider Kipper uses. Supports Claude (Anthropic),
OpenAI, and Ollama (self-hosted). The configuration is saved locally and
synced to the cluster so the web console can use it too.

Examples:
  kip ai configure
  kip ai configure --provider claude --key sk-ant-...
  kip ai status`,
	RunE: runAIConfigure,
}

var aiStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show current AI configuration",
	RunE:  runAIStatus,
}

func init() {
	aiConfigureCmd.Flags().String("provider", "", "AI provider: claude, openai, ollama")
	aiConfigureCmd.Flags().String("key", "", "API key (not needed for Ollama)")
	aiConfigureCmd.Flags().String("model", "", "Model override (optional)")
	aiConfigureCmd.Flags().String("ollama-url", "", "Ollama server URL (default: http://localhost:11434)")

	aiCmd.AddCommand(aiConfigureCmd)
	aiCmd.AddCommand(aiStatusCmd)
	rootCmd.AddCommand(aiCmd)
}

func runAIConfigure(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	provider, _ := cmd.Flags().GetString("provider")
	key, _ := cmd.Flags().GetString("key")
	model, _ := cmd.Flags().GetString("model")
	ollamaURL, _ := cmd.Flags().GetString("ollama-url")

	reader := bufio.NewReader(os.Stdin)

	// Interactive mode if no flags provided
	if provider == "" {
		fmt.Println("\n  AI Provider Setup")
		fmt.Println()
		fmt.Println("  1) Claude (Anthropic)")
		fmt.Println("  2) OpenAI")
		fmt.Println("  3) Ollama (self-hosted)")
		fmt.Println("  4) None — disable AI features")
		fmt.Println()
		fmt.Print("  Choose provider [1-4]: ")

		choice, _ := reader.ReadString('\n')
		choice = strings.TrimSpace(choice)

		switch choice {
		case "1":
			provider = "claude"
		case "2":
			provider = "openai"
		case "3":
			provider = "ollama"
		case "4":
			// Persist Provider="none" so future install flows can tell
			// "user explicitly opted out" apart from "never configured."
			// An empty AIConfig would let kip ai install silently re-enable
			// AI for someone who deliberately turned it off.
			cfg.AI = config.AIConfig{Provider: "none"}
			if err := saveAISettings(cfg.AI); err != nil {
				return err
			}
			fmt.Print("\n  ✔  AI features disabled\n\n")
			return syncAIToCluster(cfg)
		default:
			return fmt.Errorf("invalid choice: %s", choice)
		}
	}

	if key == "" && (provider == "claude" || provider == "openai") {
		fmt.Printf("\n  Enter %s API key: ", provider)
		key, _ = reader.ReadString('\n')
		key = strings.TrimSpace(key)
		if key == "" {
			return fmt.Errorf("API key is required for %s", provider)
		}
	}

	if ollamaURL == "" && provider == "ollama" {
		fmt.Print("\n  Ollama URL [http://localhost:11434]: ")
		ollamaURL, _ = reader.ReadString('\n')
		ollamaURL = strings.TrimSpace(ollamaURL)
		if ollamaURL == "" {
			ollamaURL = "http://localhost:11434"
		}
	}

	cfg.AI = config.AIConfig{
		Provider:  provider,
		APIKey:    key,
		Model:     model,
		OllamaURL: ollamaURL,
	}

	if err := saveAISettings(cfg.AI); err != nil {
		return err
	}

	fmt.Printf("\n  ✔  AI configured: %s\n", provider)
	if model != "" {
		fmt.Printf("  Model: %s\n", model)
	}
	fmt.Println()

	return syncAIToCluster(cfg)
}

func runAIStatus(_ *cobra.Command, _ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	fmt.Println()
	if !cfg.AI.AIEnabled() {
		fmt.Println("  AI client: disabled")
		fmt.Println("  Run 'kip ai configure' to set up an AI provider for kip's own AI features.")
		fmt.Println()
	} else {
		fmt.Printf("  AI client: enabled\n")
		fmt.Printf("  Provider: %s\n", cfg.AI.Provider)
		if cfg.AI.APIKey != "" {
			masked := cfg.AI.APIKey
			if len(masked) > 12 {
				masked = masked[:8] + "..." + masked[len(masked)-4:]
			} else {
				masked = "••••••••"
			}
			fmt.Printf("  API key: %s\n", masked)
		}
		if cfg.AI.Model != "" {
			fmt.Printf("  Model: %s\n", cfg.AI.Model)
		}
		if cfg.AI.OllamaURL != "" {
			fmt.Printf("  Ollama URL: %s\n", cfg.AI.OllamaURL)
		}
		fmt.Println()
	}

	// Bundle status runs regardless of AI client config. A user who
	// installed the bundle but declined to repoint kip's own AI features
	// still needs to see chat URL and workload health from this command.
	printBundleStatus(cfg)
	return nil
}

// printBundleStatus prints the in-cluster AI bundle health to stdout.
// Best-effort: silent when no cluster is reachable so the existing client-
// only status remains useful when running 'kip ai status' offline.
func printBundleStatus(cfg *config.Config) {
	cluster := cfg.GetCluster(cfg.CurrentCluster)
	if cluster == nil {
		return
	}
	client, err := k8s.NewFromCluster(cluster)
	if err != nil {
		return
	}

	ctx := context.Background()
	clientset := client.Clientset()

	_, err = clientset.CoreV1().Namespaces().Get(ctx, ai.Namespace, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		fmt.Println("  In-cluster bundle: not installed")
		fmt.Println("  Run 'kip ai install' to install Ollama and LibreChat.")
		fmt.Println()
		return
	}
	if err != nil {
		return
	}

	fmt.Println("  In-cluster bundle:")
	fmt.Printf("    %s\n", bundleDeploymentState(ctx, clientset, "ollama"))
	fmt.Printf("    %s\n", bundleDeploymentState(ctx, clientset, ai.LibreChatDeploymentName))
	// The installed model is read from the Kipper-owned bundle-state
	// ConfigMap, not from the local AI client. A user with provider:
	// claude or who declined auto-config still gets the right answer.
	installedModel := readBundleStateModel(ctx, clientset)
	fmt.Printf("    %s\n", bundleModelState(ctx, clientset, installedModel))

	host := bundleChatHost(ctx, clientset)
	if host != "" {
		fmt.Printf("  Chat URL: https://%s\n", host)
	}
	fmt.Println()

	printRAGBundleStatus(os.Stdout, ctx, clientset)
}

// printRAGBundleStatus prints the Phase 2 RAG bundle's health when
// it is installed. Quiet when the AnythingLLM Deployment is absent so
// Phase-1-only users do not see noise about a bundle they have not
// opted into. Takes an io.Writer so tests can capture the output
// without driving os.Stdout.
func printRAGBundleStatus(out io.Writer, ctx context.Context, clientset kubernetes.Interface) {
	if _, err := clientset.AppsV1().Deployments(ai.Namespace).Get(ctx, ai.AnythingLLMDeploymentName, metav1.GetOptions{}); err != nil {
		return
	}

	_, _ = fmt.Fprintln(out, "  RAG bundle:")
	_, _ = fmt.Fprintf(out, "    %s\n", bundleStatefulSetState(ctx, clientset, ai.QdrantServiceName))
	_, _ = fmt.Fprintf(out, "    %s\n", bundleDeploymentState(ctx, clientset, ai.AnythingLLMDeploymentName))
	embeddingModel := readRAGBundleStateEmbeddingModel(ctx, clientset)
	_, _ = fmt.Fprintf(out, "    %s\n", bundleModelState(ctx, clientset, embeddingModel))

	if askHost := bundleAskHost(ctx, clientset); askHost != "" {
		_, _ = fmt.Fprintf(out, "  Ask URL: https://%s\n", askHost)
	}
	_, _ = fmt.Fprintln(out)
}

// readRAGBundleStateEmbeddingModel pulls the recorded embedding model
// from the Kipper-owned RAG bundle-state ConfigMap. Returns the empty
// string when missing.
func readRAGBundleStateEmbeddingModel(ctx context.Context, clientset kubernetes.Interface) string {
	cm, err := clientset.CoreV1().ConfigMaps(ai.Namespace).Get(ctx, ai.RAGBundleStateConfigMapName, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return cm.Data["embedding_model"]
}

// bundleStatefulSetState returns a one-line health summary for a
// bundle StatefulSet, parallel to bundleDeploymentState.
func bundleStatefulSetState(ctx context.Context, clientset kubernetes.Interface, name string) string {
	ss, err := clientset.AppsV1().StatefulSets(ai.Namespace).Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return fmt.Sprintf("✗  %s: not deployed", name)
	}
	if err != nil {
		return fmt.Sprintf("?  %s: %v", name, err)
	}
	desired := int32(1)
	if ss.Spec.Replicas != nil {
		desired = *ss.Spec.Replicas
	}
	if ss.Status.ReadyReplicas == desired && ss.Status.CurrentRevision == ss.Status.UpdateRevision {
		return fmt.Sprintf("✔  %s: %d/%d ready", name, ss.Status.ReadyReplicas, desired)
	}
	return fmt.Sprintf("…  %s: %d/%d ready (rolling out)", name, ss.Status.ReadyReplicas, desired)
}

// bundleAskHost reads the AnythingLLM Ingress and returns its host,
// or empty string when not yet present. Parallel to bundleChatHost.
func bundleAskHost(ctx context.Context, clientset kubernetes.Interface) string {
	ing, err := clientset.NetworkingV1().Ingresses(ai.Namespace).Get(ctx, "anythingllm", metav1.GetOptions{})
	if err != nil || ing == nil {
		return ""
	}
	for _, rule := range ing.Spec.Rules {
		if rule.Host != "" {
			return rule.Host
		}
	}
	return ""
}

// readBundleStateModel pulls the installed model from the Kipper-owned
// bundle-state ConfigMap. Returns the empty string when missing or
// unreachable; bundleModelState handles that as "not configured".
func readBundleStateModel(ctx context.Context, clientset kubernetes.Interface) string {
	cm, err := clientset.CoreV1().ConfigMaps(ai.Namespace).Get(ctx, ai.BundleStateConfigMapName, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return cm.Data["model"]
}

// bundleModelState reaches the in-cluster Ollama Service through the
// apiserver's service-proxy subresource and confirms the configured model
// is present in /api/tags. Without this check, status could report a
// healthy bundle even when the model cache is missing or the API does
// not serve the expected model.
//
// The probe is bounded by a short context timeout so `kip ai status`
// stays fast. Any error short-circuits to a "?" line rather than failing
// the status command, since users running status on a partially-installed
// or unreachable cluster still want the rest of the output.
func bundleModelState(ctx context.Context, clientset kubernetes.Interface, expectedModel string) string {
	if expectedModel == "" {
		return "?  model: not configured (set --model on 'kip ai install')"
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	data, err := clientset.CoreV1().RESTClient().Get().
		Namespace(ai.Namespace).
		Resource("services").
		Name("ollama:11434").
		SubResource("proxy").
		Suffix("/api/tags").
		DoRaw(probeCtx)
	if err != nil {
		return fmt.Sprintf("?  model %q: could not query Ollama (%v)", expectedModel, err)
	}
	loaded, err := parseLoadedModels(data)
	if err != nil {
		return fmt.Sprintf("?  model %q: malformed Ollama /api/tags response", expectedModel)
	}
	for _, name := range loaded {
		if name == expectedModel {
			return fmt.Sprintf("✔  model %q loaded", expectedModel)
		}
	}
	return fmt.Sprintf("✗  model %q not loaded; run 'kubectl logs -n %s deployment/ollama' for pull progress", expectedModel, ai.Namespace)
}

// parseLoadedModels decodes the Ollama /api/tags response shape and
// returns the names of every loaded model. The response shape is stable:
//
//	{"models":[{"name":"qwen2.5:3b-instruct-q4_K_M",...}, ...]}
func parseLoadedModels(data []byte) ([]string, error) {
	var resp struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(resp.Models))
	for _, m := range resp.Models {
		names = append(names, m.Name)
	}
	return names, nil
}

// bundleDeploymentState returns a one-line health summary for a bundle
// Deployment.
func bundleDeploymentState(ctx context.Context, clientset kubernetes.Interface, name string) string {
	dep, err := clientset.AppsV1().Deployments(ai.Namespace).Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return fmt.Sprintf("✗  %s: not deployed", name)
	}
	if err != nil {
		return fmt.Sprintf("?  %s: %v", name, err)
	}
	for _, cond := range dep.Status.Conditions {
		if cond.Type == appsv1.DeploymentAvailable && cond.Status == corev1.ConditionTrue {
			return fmt.Sprintf("✔  %s: %d/%d ready", name, dep.Status.ReadyReplicas, dep.Status.Replicas)
		}
	}
	return fmt.Sprintf("…  %s: %d/%d ready (rolling out)", name, dep.Status.ReadyReplicas, dep.Status.Replicas)
}

// bundleChatHost reads the LibreChat Ingress and returns its host, or
// empty string when not yet present.
func bundleChatHost(ctx context.Context, clientset kubernetes.Interface) string {
	ing, err := clientset.NetworkingV1().Ingresses(ai.Namespace).Get(ctx, "librechat", metav1.GetOptions{})
	if err != nil || ing == nil {
		return ""
	}
	for _, rule := range ing.Spec.Rules {
		if rule.Host != "" {
			return rule.Host
		}
	}
	return ""
}

// syncAIToCluster writes the AI config to the cluster's K8s Secret
// so the web console can use the same settings.
func syncAIToCluster(cfg *config.Config) error {
	cluster := cfg.GetCluster(cfg.CurrentCluster)
	if cluster == nil {
		return nil // no cluster connected, skip sync
	}

	client, err := k8s.NewFromCluster(cluster)
	if err != nil {
		fmt.Printf("  ⚠  Could not connect to cluster: %v\n", err)
		return nil //nolint:nilerr // non-fatal, sync is best-effort
	}

	ctx := context.Background()
	clientset := client.Clientset()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kipper-ai-config",
			Namespace: "kipper-system",
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kipper",
				// Marker for `kip ai backup`: lets the AI backup scope
				// catch this Secret without sweeping in unrelated
				// kipper-system resources.
				"app.kubernetes.io/part-of": "kipper-ai",
			},
		},
		Data: map[string][]byte{
			"provider":   []byte(cfg.AI.Provider),
			"api_key":    []byte(cfg.AI.APIKey),
			"model":      []byte(cfg.AI.Model),
			"ollama_url": []byte(cfg.AI.OllamaURL),
		},
	}

	_, err = clientset.CoreV1().Secrets("kipper-system").Update(ctx, secret, metav1.UpdateOptions{})
	if errors.IsNotFound(err) {
		_, err = clientset.CoreV1().Secrets("kipper-system").Create(ctx, secret, metav1.CreateOptions{})
	}
	if err != nil {
		fmt.Printf("  ⚠  Could not sync to cluster: %v\n", err)
		return nil // non-fatal
	}

	fmt.Println("  ✔  Synced to cluster")
	return nil
}

// saveAISettings writes just the AI block, re-reading everything else under the
// lock.
//
// This command asks the operator questions, and it read the config before doing
// so. Saving the whole snapshot afterwards would put back every other field as
// it looked before the questions were asked — including a gateway credential a
// concurrent uninstall had recorded meanwhile, which would be the last copy of
// it. How long that window is depends on how long someone takes to answer.
func saveAISettings(ai config.AIConfig) error {
	if err := config.Update(func(cfg *config.Config) error {
		cfg.AI = ai
		return nil
	}); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}
	return nil
}
