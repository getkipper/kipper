package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/getkipper/kipper/controller/pkg/secretname"
)

const previousSuffix = ".__previous"

var secretCmd = &cobra.Command{
	Use:   "secret",
	Short: "Manage application secrets",
}

var secretSetCmd = &cobra.Command{
	Use:   "set [app-name] [KEY=VALUE | KEY]",
	Short: "Set a secret for an application",
	Long: `Set a secret value for an application. If only a key is provided
(without =VALUE), you will be prompted to enter the value with hidden input
so it does not appear in your shell history.`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runSecretSet,
}

var secretListCmd = &cobra.Command{
	Use:   "list [app-name]",
	Short: "List secret keys for an application (values masked)",
	Args:  cobra.ExactArgs(1),
	RunE:  runSecretList,
}

var secretRevealCmd = &cobra.Command{
	Use:   "reveal [app-name] [KEY]",
	Short: "Reveal a single secret value",
	Args:  cobra.ExactArgs(2),
	RunE:  runSecretReveal,
}

var secretDeleteCmd = &cobra.Command{
	Use:   "delete [app-name] [KEY]",
	Short: "Delete a secret from an application",
	Args:  cobra.ExactArgs(2),
	RunE:  runSecretDelete,
}

var secretRollbackCmd = &cobra.Command{
	Use:   "rollback [app-name] [KEY]",
	Short: "Restore a secret to its previous value",
	Args:  cobra.ExactArgs(2),
	RunE:  runSecretRollback,
}

func init() {
	secretSetCmd.Flags().String("from-file", "", "load secrets from a file (KEY=VALUE per line)")
	secretSetCmd.Flags().String("project", "", "project name")
	secretSetCmd.Flags().String("environment", "", "target environment (e.g. test, acc, prod)")

	secretListCmd.Flags().String("project", "", "project name")
	secretListCmd.Flags().String("environment", "", "target environment")

	secretRevealCmd.Flags().String("project", "", "project name")
	secretRevealCmd.Flags().String("environment", "", "target environment")

	secretDeleteCmd.Flags().String("project", "", "project name")
	secretDeleteCmd.Flags().String("environment", "", "target environment")

	secretRollbackCmd.Flags().String("project", "", "project name")
	secretRollbackCmd.Flags().String("environment", "", "target environment")

	secretCmd.AddCommand(secretSetCmd)
	secretCmd.AddCommand(secretListCmd)
	secretCmd.AddCommand(secretRevealCmd)
	secretCmd.AddCommand(secretDeleteCmd)
	secretCmd.AddCommand(secretRollbackCmd)
	appCmd.AddCommand(secretCmd)
}

func runSecretSet(cmd *cobra.Command, args []string) error {
	appName := args[0]
	fromFile, _ := cmd.Flags().GetString("from-file")

	ns, clientset, dyn, err := workloadClients(cmd, appName)
	if err != nil {
		return err
	}

	ctx := context.Background()

	// Before anything is written: an explicit --project resolves a namespace
	// without looking anything up, so a mistyped name would otherwise leave a
	// credential in a Secret no workload owns.
	if err := requireWorkload(ctx, clientset, dyn, workloadKind(cmd), ns, appName); err != nil {
		return err
	}

	// Load existing secrets
	existing := getOrCreateSecret(ctx, clientset, workloadKind(cmd), ns, appName)

	switch {
	case fromFile != "":
		pairs, err := readEnvFile(fromFile)
		if err != nil {
			return err
		}
		for k, v := range pairs {
			existing[k] = []byte(v)
		}
	case len(args) == 2:
		// KEY=VALUE or just KEY
		kv := args[1]
		if strings.Contains(kv, "=") {
			parts := strings.SplitN(kv, "=", 2)
			existing[parts[0]] = []byte(parts[1])
			// Registered under both `kip app` and `kip function`, so the
			// grandparent names the command the user actually ran.
			fmt.Fprintf(os.Stderr, "  Warning: secret value is visible in shell history. Use 'kip %s secret set %s %s' without =VALUE for hidden input.\n", cmd.Parent().Parent().Name(), appName, parts[0])
		} else {
			// Interactive hidden prompt
			value, err := promptHidden(fmt.Sprintf("  Enter value for %s: ", kv))
			if err != nil {
				return fmt.Errorf("reading secret value: %w", err)
			}
			existing[kv] = []byte(value)
		}
	default:
		return fmt.Errorf("provide KEY=VALUE, KEY (for interactive prompt), or --from-file")
	}

	if err := saveSecret(ctx, clientset, workloadKind(cmd), ns, appName, existing); err != nil {
		return err
	}

	rolled, err := checkDirectEnvConflicts(ctx, clientset, workloadKind(cmd), ns, appName, slices.Collect(maps.Keys(existing)))
	if err != nil {
		return err
	}

	fmt.Printf("  ✔  Secret updated for %s\n", appName)
	if rolled {
		fmt.Printf("      Removing the direct entries restarted %s, so the new values are live.\n", appName)
		return nil
	}
	return applyConfigChange(cmd, ctx, clientset, dyn, workloadKind(cmd), ns, appName)
}

func runSecretList(cmd *cobra.Command, args []string) error {
	appName := args[0]

	ns, clientset, _, err := workloadClients(cmd, appName)
	if err != nil {
		return err
	}

	ctx := context.Background()

	secret, err := clientset.CoreV1().Secrets(ns).Get(ctx, secretname.Secrets(workloadKind(cmd), appName), metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			fmt.Printf("\n  No secrets configured for %s\n\n", appName)
			return nil
		}
		return fmt.Errorf("getting secrets: %w", err)
	}

	if len(secret.Data) == 0 {
		fmt.Printf("\n  No secrets configured for %s\n\n", appName)
		return nil
	}

	fmt.Printf("\n  %-30s %-12s %s\n", "KEY", "PREVIOUS", "VALUE")
	for key := range secret.Data {
		if strings.HasSuffix(key, previousSuffix) {
			continue
		}
		hasPrevious := ""
		if _, ok := secret.Data[key+previousSuffix]; ok {
			hasPrevious = "yes"
		}
		fmt.Printf("  %-30s %-12s %s\n", key, hasPrevious, "••••••••")
	}
	fmt.Println()

	return nil
}

func runSecretReveal(cmd *cobra.Command, args []string) error {
	appName := args[0]
	key := args[1]

	ns, clientset, _, err := workloadClients(cmd, appName)
	if err != nil {
		return err
	}

	ctx := context.Background()

	secret, err := clientset.CoreV1().Secrets(ns).Get(ctx, secretname.Secrets(workloadKind(cmd), appName), metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("no secrets configured for %s", appName)
		}
		return fmt.Errorf("getting secrets: %w", err)
	}

	value, ok := secret.Data[key]
	if !ok {
		return fmt.Errorf("secret %q not found for %s", key, appName)
	}

	strVal := string(value)

	// If the value is valid JSON, pretty-print it
	var jsonObj interface{}
	if json.Unmarshal(value, &jsonObj) == nil {
		pretty, err := json.MarshalIndent(jsonObj, "  ", "  ")
		if err == nil {
			fmt.Printf("  %s=\n  %s\n", key, string(pretty))
			return nil
		}
	}

	fmt.Printf("  %s=%s\n", key, strVal)
	return nil
}

func runSecretDelete(cmd *cobra.Command, args []string) error {
	appName := args[0]
	key := args[1]

	ns, clientset, dyn, err := workloadClients(cmd, appName)
	if err != nil {
		return err
	}

	ctx := context.Background()

	// The same check `set` makes, and for the same reason: a delete aimed at a
	// workload that is not there would otherwise report success for a key it
	// never removed.
	if err := requireWorkload(ctx, clientset, dyn, workloadKind(cmd), ns, appName); err != nil {
		return err
	}

	secret, err := clientset.CoreV1().Secrets(ns).Get(ctx, secretname.Secrets(workloadKind(cmd), appName), metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("no secrets configured for %s", appName)
		}
		return fmt.Errorf("getting secrets: %w", err)
	}

	if _, ok := secret.Data[key]; !ok {
		return fmt.Errorf("secret %q not found for %s", key, appName)
	}

	delete(secret.Data, key)
	delete(secret.Data, key+previousSuffix)

	if _, err := clientset.CoreV1().Secrets(ns).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating secrets: %w", err)
	}

	fmt.Printf("  ✔  Secret %q deleted from %s\n", key, appName)
	return applyConfigChange(cmd, ctx, clientset, dyn, workloadKind(cmd), ns, appName)
}

func runSecretRollback(cmd *cobra.Command, args []string) error {
	appName := args[0]
	key := args[1]

	ns, clientset, dyn, err := workloadClients(cmd, appName)
	if err != nil {
		return err
	}

	ctx := context.Background()

	// The same check `set` makes, and for the same reason: a rollback aimed at a
	// workload that is not there would otherwise report a value restored that
	// nothing reads.
	if err := requireWorkload(ctx, clientset, dyn, workloadKind(cmd), ns, appName); err != nil {
		return err
	}

	secret, err := clientset.CoreV1().Secrets(ns).Get(ctx, secretname.Secrets(workloadKind(cmd), appName), metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("no secrets configured for %s", appName)
		}
		return fmt.Errorf("getting secrets: %w", err)
	}

	prevKey := key + previousSuffix
	prevValue, ok := secret.Data[prevKey]
	if !ok {
		return fmt.Errorf("no previous version of %q for %s", key, appName)
	}

	// Current becomes previous, previous becomes current
	secret.Data[prevKey] = secret.Data[key]
	secret.Data[key] = prevValue

	if _, err := clientset.CoreV1().Secrets(ns).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating secrets: %w", err)
	}

	fmt.Printf("  ✔  Secret %q rolled back to previous value for %s\n", key, appName)
	return applyConfigChange(cmd, ctx, clientset, dyn, workloadKind(cmd), ns, appName)
}

func getOrCreateSecret(ctx context.Context, clientset kubernetes.Interface, kind secretname.Kind, ns, appName string) map[string][]byte {
	secret, err := clientset.CoreV1().Secrets(ns).Get(ctx, secretname.Secrets(kind, appName), metav1.GetOptions{})
	if err != nil {
		return make(map[string][]byte)
	}
	if secret.Data == nil {
		return make(map[string][]byte)
	}
	return secret.Data
}

func saveSecret(ctx context.Context, clientset kubernetes.Interface, kind secretname.Kind, ns, appName string, newData map[string][]byte) error {
	name := secretname.Secrets(kind, appName)
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				"app":                          appName,
				"app.kubernetes.io/managed-by": "kipper",
			},
		},
		Data: withoutPreviousKeys(newData),
	}
	_, err := clientset.CoreV1().Secrets(ns).Create(ctx, desired, metav1.CreateOptions{})
	if !errors.IsAlreadyExists(err) {
		return err
	}

	// Update the live object in place rather than replacing it: a fresh Secret
	// carries no ownerReferences, so a blind Update would detach the controller
	// reference the reconciler set and the Secret would outlive the App again.
	// RetryOnConflict absorbs a concurrent write between the Get and the Update.
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		live, err := clientset.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if live.Data == nil {
			live.Data = map[string][]byte{}
		}
		for k, v := range newData {
			if strings.HasSuffix(k, previousSuffix) {
				continue
			}
			// Save the current value as previous before overwriting
			if current, exists := live.Data[k]; exists && !bytes.Equal(current, v) {
				live.Data[k+previousSuffix] = current
			}
			live.Data[k] = v
		}
		_, err = clientset.CoreV1().Secrets(ns).Update(ctx, live, metav1.UpdateOptions{})
		return err
	})
}

// withoutPreviousKeys copies data minus the `.__previous` bookkeeping keys, so
// a freshly created Secret never starts out with stale version history.
func withoutPreviousKeys(data map[string][]byte) map[string][]byte {
	clean := make(map[string][]byte, len(data))
	for k, v := range data {
		if !strings.HasSuffix(k, previousSuffix) {
			clean[k] = v
		}
	}
	return clean
}

func promptHidden(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	if term.IsTerminal(int(os.Stdin.Fd())) { //nolint:gosec // safe conversion for terminal check
		value, err := term.ReadPassword(int(os.Stdin.Fd())) //nolint:gosec // safe conversion for terminal check
		fmt.Fprintln(os.Stderr)
		return string(value), err
	}
	// Fallback for non-terminal input (pipes)
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		return scanner.Text(), nil
	}
	return "", fmt.Errorf("no input")
}

func readEnvFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path from CLI flag
	if err != nil {
		return nil, fmt.Errorf("reading file %s: %w", path, err)
	}

	var pairs []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pairs = append(pairs, line)
	}

	// Share parseEnvVars' atomic-failure guarantee so a malformed line in a
	// .env file is reported instead of silently dropped, the same way a
	// malformed --env argument is.
	result, err := parseEnvVars(pairs)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return result, nil
}
