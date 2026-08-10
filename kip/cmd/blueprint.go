package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/getkipper/kipper/kip/internal/blueprint"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

var blueprintCmd = &cobra.Command{
	Use:   "blueprint",
	Short: "Browse and install application blueprints",
}

var blueprintListCmd = &cobra.Command{
	Use:   "list",
	Short: "List available blueprints",
	RunE:  runBlueprintList,
}

var blueprintInfoCmd = &cobra.Command{
	Use:   "info [name]",
	Short: "Show details about a blueprint",
	Args:  cobra.ExactArgs(1),
	RunE:  runBlueprintInfo,
}

var blueprintInstallCmd = &cobra.Command{
	Use:   "install [name]",
	Short: "Install a blueprint (render and apply to the cluster)",
	Args:  cobra.ExactArgs(1),
	RunE:  runBlueprintInstall,
}

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Generate a kipper.yaml from a blueprint template",
	RunE:  runInit,
}

func init() {
	blueprintInstallCmd.Flags().StringSlice("set", nil, "parameter values (key=value, repeatable)")
	blueprintInstallCmd.Flags().String("project", "", "project name (overrides template)")
	blueprintInstallCmd.Flags().String("environment", "", "target environment")

	initCmd.Flags().String("blueprint", "", "blueprint name to use as template")
	initCmd.Flags().StringSlice("set", nil, "parameter values (key=value, repeatable)")
	initCmd.Flags().StringP("output", "o", "kipper.yaml", "output file")
	_ = initCmd.MarkFlagRequired("blueprint")

	blueprintCmd.AddCommand(blueprintListCmd)
	blueprintCmd.AddCommand(blueprintInfoCmd)
	blueprintCmd.AddCommand(blueprintInstallCmd)
	rootCmd.AddCommand(blueprintCmd)
	rootCmd.AddCommand(initCmd)
}

func runBlueprintList(cmd *cobra.Command, args []string) error {
	blueprints, err := blueprint.List()
	if err != nil {
		return err
	}

	if len(blueprints) == 0 {
		fmt.Println("  No blueprints available")
		return nil
	}

	fmt.Printf("\n  %-20s %-8s %s\n", "NAME", "VERSION", "DESCRIPTION")
	for _, bp := range blueprints {
		fmt.Printf("  %-20s %-8s %s\n", bp.Name, bp.Version, bp.Description)
	}
	fmt.Println()

	return nil
}

func runBlueprintInfo(cmd *cobra.Command, args []string) error {
	bp, err := blueprint.Get(args[0])
	if err != nil {
		return err
	}

	fmt.Printf("\n  %s (v%s)\n", bp.Metadata.Name, bp.Metadata.Version)
	fmt.Printf("  %s\n\n", bp.Metadata.Description)

	if len(bp.Metadata.Parameters) > 0 {
		fmt.Printf("  Parameters:\n")
		for _, p := range bp.Metadata.Parameters {
			req := ""
			if p.Required {
				req = " (required)"
			}
			def := ""
			if p.Default != "" {
				def = fmt.Sprintf(" [default: %s]", p.Default)
			}
			fmt.Printf("    %-20s %s%s%s\n", p.Name, p.Description, req, def)
		}
		fmt.Println()
	}

	fmt.Printf("  Install:\n")
	fmt.Printf("    kip blueprint install %s --set projectName=my-project\n\n", bp.Metadata.Name)

	return nil
}

func runBlueprintInstall(cmd *cobra.Command, args []string) error {
	name := args[0]
	setFlags, _ := cmd.Flags().GetStringSlice("set")
	projectOverride, _ := cmd.Flags().GetString("project")
	envOverride, _ := cmd.Flags().GetString("environment")

	bp, err := blueprint.Get(name)
	if err != nil {
		return err
	}

	params := parseSetFlags(setFlags)
	if projectOverride != "" {
		params["projectName"] = projectOverride
	}
	if envOverride != "" {
		params["environment"] = envOverride
	}

	m, err := bp.Render(params)
	if err != nil {
		return fmt.Errorf("rendering blueprint: %w", err)
	}

	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	dynClient := k8sClient.Dynamic()
	clientset := k8sClient.Clientset()

	namespace := cluster.ResolveNamespace(m.Project, m.Environment)
	resources := manifest.Convert(m, namespace)

	// Ensure namespace exists
	if _, nsErr := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{}); errors.IsNotFound(nsErr) {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "kipper",
					"kipper.run/project":           m.Project,
				},
			},
		}
		if _, createErr := clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); createErr != nil && !errors.IsAlreadyExists(createErr) {
			return fmt.Errorf("creating namespace %s: %w", namespace, createErr)
		}
		fmt.Printf("  ✔  Namespace %s created\n", namespace)
	}

	fmt.Printf("\n  Installing %s into %s...\n", name, namespace)

	var created int
	for _, res := range resources {
		kind := res.Object.GetKind()
		resName := res.Object.GetName()

		_, getErr := dynClient.Resource(res.GVR).Namespace(namespace).Get(ctx, resName, metav1.GetOptions{})
		switch {
		case errors.IsNotFound(getErr):
			if _, createErr := dynClient.Resource(res.GVR).Namespace(namespace).Create(ctx, res.Object, metav1.CreateOptions{}); createErr != nil {
				return fmt.Errorf("creating %s/%s: %w", kind, resName, createErr)
			}
			fmt.Printf("    ✔  %s/%s created\n", kind, resName)
			created++
		case getErr != nil:
			return fmt.Errorf("checking %s/%s: %w", kind, resName, getErr)
		default:
			spec := res.Object.Object["spec"]
			specJSON, _ := json.Marshal(map[string]interface{}{"spec": spec})
			if _, patchErr := dynClient.Resource(res.GVR).Namespace(namespace).Patch(ctx, resName, types.MergePatchType, specJSON, metav1.PatchOptions{}); patchErr != nil {
				return fmt.Errorf("updating %s/%s: %w", kind, resName, patchErr)
			}
			fmt.Printf("    ✔  %s/%s updated\n", kind, resName)
		}
	}

	fmt.Printf("\n  ✔  %s installed (%d resources)\n\n", name, created)
	return nil
}

func runInit(cmd *cobra.Command, args []string) error {
	name, _ := cmd.Flags().GetString("blueprint")
	setFlags, _ := cmd.Flags().GetStringSlice("set")
	output, _ := cmd.Flags().GetString("output")

	bp, err := blueprint.Get(name)
	if err != nil {
		return err
	}

	params := parseSetFlags(setFlags)
	data, err := bp.RenderYAML(params)
	if err != nil {
		return fmt.Errorf("rendering blueprint: %w", err)
	}

	if err := os.WriteFile(output, data, 0600); err != nil {
		return fmt.Errorf("writing %s: %w", output, err)
	}

	fmt.Printf("  ✔  Generated %s from %s blueprint\n", output, name)
	fmt.Printf("     Edit it, then run: kip apply -f %s\n\n", output)
	return nil
}

func parseSetFlags(flags []string) map[string]string {
	params := make(map[string]string)
	for _, f := range flags {
		parts := strings.SplitN(f, "=", 2)
		if len(parts) == 2 {
			params[parts[0]] = parts[1]
		}
	}
	return params
}
