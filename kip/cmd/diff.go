package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getkipper/kipper/kip/internal/manifest"
)

var diffCmd = &cobra.Command{
	Use:   "diff",
	Short: "Show differences between a kipper.yaml manifest and the live cluster",
	RunE:  runDiff,
}

func init() {
	diffCmd.Flags().StringP("file", "f", "kipper.yaml", "path to manifest file or directory")
	diffCmd.Flags().String("project", "", "override the project name from the manifest")
	diffCmd.Flags().String("environment", "", "override the environment from the manifest")
	rootCmd.AddCommand(diffCmd)
}

func runDiff(cmd *cobra.Command, args []string) error {
	filePath, _ := cmd.Flags().GetString("file")
	projectOverride, _ := cmd.Flags().GetString("project")
	envOverride, _ := cmd.Flags().GetString("environment")

	manifests, err := manifest.Parse(filePath)
	if err != nil {
		return err
	}

	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	dynClient := k8sClient.Dynamic()

	hasChanges := false
	cleared := 0
	defaults := newSchemaDefaults()
	var uncertain []resourceChange

	for _, m := range manifests {
		project := m.Project
		if projectOverride != "" {
			project = projectOverride
		}
		environment := m.Environment
		if envOverride != "" {
			environment = envOverride
		}

		namespace := cluster.ResolveNamespace(project, environment)
		resources := manifest.Convert(m, namespace)

		fmt.Printf("\n  Comparing %s (%s)...\n", project, namespace)

		var existing []manifest.Resource
		for _, res := range resources {
			name := res.Object.GetName()
			_, getErr := dynClient.Resource(res.GVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
			switch {
			case errors.IsNotFound(getErr):
				fmt.Printf("    + %s/%s (new)\n", res.Object.GetKind(), name)
				hasChanges = true
			case getErr != nil:
				return fmt.Errorf("checking %s/%s: %w", res.Object.GetKind(), name, getErr)
			default:
				existing = append(existing, res)
			}
		}

		// The same function apply uses, so a diff cannot promise something the
		// apply then does differently.
		changes, err := scanChangesWith(ctx, dynClient, namespace, existing, defaults)
		if err != nil {
			return err
		}
		lastResource := ""
		for _, rc := range changes {
			hasChanges = true
			if id := rc.kind + "/" + rc.name; id != lastResource {
				fmt.Printf("    ~ %s\n", id)
				lastResource = id
			}
			switch rc.change.Kind {
			case manifest.Added:
				fmt.Printf("        + %s: %s\n", rc.change.Path, rc.change.New)
			case manifest.Changed:
				fmt.Printf("        ~ %s: %s -> %s\n", rc.change.Path, rc.change.Live, rc.change.New)
			case manifest.Reset:
				cleared++
				uncertain = append(uncertain, rc)
				fmt.Printf("        - %s: %s -> %s (the cluster's default)\n", rc.change.Path, rc.change.Live, rc.change.New)
			case manifest.Cleared:
				cleared++
				uncertain = append(uncertain, rc)
				fmt.Printf("        - %s: %s (will be cleared)\n", rc.change.Path, rc.change.Live)
			}
		}
	}

	if !hasChanges {
		fmt.Printf("\n  No changes detected\n")
	}
	if cleared > 0 {
		// Said plainly, because this is the half nobody expects. A spec is
		// replaced rather than merged, so a value the cluster has and the
		// manifest does not is removed — whether or not the manifest ever
		// carried it.
		fmt.Printf("\n  %d field(s) would be lost: they are set on the cluster and absent from the\n", cleared)
		fmt.Printf("  manifest, and apply replaces a spec rather than merging into it. Add them to the\n")
		fmt.Printf("  manifest to keep them, or run 'kip export' to fold the live state back in.\n")
		printSchemaCaveat(defaults, uncertain)
	}
	fmt.Println()

	return nil
}
