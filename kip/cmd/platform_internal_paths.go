package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/getkipper/kipper/controller/pkg/internalpath"
)

var platformInternalPathsCmd = &cobra.Command{
	Use:   "internal-paths",
	Short: "Show or set whether routes refuse the well-known internal paths",
	Long: `A route publishes everything its app answers under its path prefix, including
the parts meant to be internal. With this on, every route refuses ` + strings.Join(internalpath.Default(), ", ") + `
with a 404.

The match is literal and case-sensitive, so this closes the accidental exposure
rather than filtering what reaches an app. An endpoint that must never be public
belongs on a port the service does not publish.

It is off on a cluster upgraded from a release that did not have it, because
turning it on changes what a running route serves. A new cluster starts with
it on.

Examples:
  kip platform internal-paths show
  kip platform internal-paths on
  kip platform internal-paths off`,
}

var platformInternalPathsShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Print whether routes refuse the well-known internal paths",
	RunE:  runInternalPathsShow,
}

var platformInternalPathsOnCmd = &cobra.Command{
	Use:   "on",
	Short: "Refuse the well-known internal paths on every route",
	RunE:  runInternalPathsSet(true),
}

var platformInternalPathsOffCmd = &cobra.Command{
	Use:   "off",
	Short: "Serve the well-known internal paths like any other path",
	RunE:  runInternalPathsSet(false),
}

var internalPathsAssumeYes bool

func init() {
	platformInternalPathsOnCmd.Flags().BoolVar(&internalPathsAssumeYes, "yes", false, "skip the confirmation prompt")
	platformInternalPathsCmd.AddCommand(platformInternalPathsShowCmd)
	platformInternalPathsCmd.AddCommand(platformInternalPathsOnCmd)
	platformInternalPathsCmd.AddCommand(platformInternalPathsOffCmd)
	platformCmd.AddCommand(platformInternalPathsCmd)
}

func runInternalPathsShow(_ *cobra.Command, _ []string) error {
	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pc, err := getPlatformConfig(ctx, k8sClient.Dynamic())
	if err != nil {
		return err
	}
	on, _, _ := unstructured.NestedBool(pc.Object, "spec", "routeGuard", "blockInternalPaths")
	routed, err := routedApps(ctx, k8sClient.Dynamic())
	if err != nil {
		return err
	}

	list := strings.Join(internalpath.Default(), ", ")
	if on {
		fmt.Printf("\n  Internal paths: refused on every route\n")
		fmt.Printf("  Refused: %s\n\n", list)
		fmt.Printf("  %d %s a route, and each is reconciled to answer 404 on those prefixes.\n", len(routed), appsHave(len(routed)))
		fmt.Printf("  The console's Routes page reports which of them the cluster has built.\n")
		fmt.Printf("  An app that needs one of them back names it in route.publicPaths.\n\n")
		return nil
	}

	fmt.Printf("\n  Internal paths: served like any other path\n\n")
	fmt.Printf("  %d %s a route, and each publishes everything its app answers\n", len(routed), appsHave(len(routed)))
	fmt.Printf("  under its path, %s included.\n\n", list)
	if len(routed) > 0 {
		for _, name := range routed {
			fmt.Printf("    %s\n", name)
		}
		fmt.Println()
	}
	fmt.Printf("  Turn the refusals on with:\n\n      kip platform internal-paths on\n\n")
	fmt.Printf("  An app that declares route.internalPaths has those refused either way.\n\n")
	return nil
}

func appsHave(n int) string {
	if n == 1 {
		return "app has"
	}
	return "apps have"
}

func runInternalPathsSet(on bool) func(*cobra.Command, []string) error {
	return func(_ *cobra.Command, _ []string) error {
		_, k8sClient, err := loadCurrentCluster()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if on && !internalPathsAssumeYes {
			routed, err := routedApps(ctx, k8sClient.Dynamic())
			if err != nil {
				return err
			}
			fmt.Printf("\n  %s stop answering on every route in this cluster.\n", strings.Join(internalpath.Default(), ", "))
			fmt.Printf("  That is %d %s a route. Anything scraping one of those prefixes\n", len(routed), appsHave(len(routed)))
			fmt.Printf("  through a public route stops working until the app names it in\n  route.publicPaths.\n\n")
			if !confirmPrompt("Refuse the internal paths on every route?") {
				fmt.Printf("\n  Left as it was.\n\n")
				return nil
			}
		}

		state, verb := "refused on every route", "on"
		if !on {
			state, verb = "served like any other path", "off"
		}
		return mutatePlatformConfig(ctx, k8sClient.Dynamic(), func(pc *unstructured.Unstructured) error {
			return unstructured.SetNestedField(pc.Object, on, "spec", "routeGuard", "blockInternalPaths")
		}, fmt.Sprintf("\n  ✔  Internal paths %s: %s.\n     Every route is rebuilt in place, with no restarts.\n\n", verb, state))
	}
}

func routedApps(ctx context.Context, dyn dynamic.Interface) ([]string, error) {
	list, err := dyn.Resource(appGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing apps: %w", err)
	}
	var names []string
	for i := range list.Items {
		app := &list.Items[i]
		if _, found, _ := unstructured.NestedMap(app.Object, "spec", "route"); !found {
			continue
		}
		names = append(names, app.GetNamespace()+"/"+app.GetName())
	}
	return names, nil
}
