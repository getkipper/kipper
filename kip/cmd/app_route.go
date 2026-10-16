package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/controller/pkg/labels"

	"github.com/getkipper/kipper/kip/internal/deployer"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

var appRouteCmd = &cobra.Command{
	Use:   "route",
	Short: "Show, set or remove an app's public route",
}

var appRouteShowCmd = &cobra.Command{
	Use:   "show [app-name]",
	Short: "Show an app's public route, settings and controller status",
	Args:  cobra.ExactArgs(1),
	RunE:  runAppRouteShow,
}

var appRouteSetCmd = &cobra.Command{
	Use:   "set [app-name]",
	Short: "Set the host or path an app is served on",
	Long: `Sets an app's public route, creating one if needed. Only the host and path
you provide change; other route settings are preserved. Updating the host or
path keeps pods running. Adding a route may restart pods to add the instance
proxy.

A new route uses the host you provide or a default derived from the app name,
environment and cluster domain. The path defaults to /. For a custom host,
point its DNS record at the server so its certificate can be issued.`,
	Args: cobra.ExactArgs(1),
	RunE: runAppRouteSet,
}

var appRouteRemoveCmd = &cobra.Command{
	Use:   "remove [app-name]",
	Short: "Remove an app's public route",
	Long: `Removes an app's public route. The app remains accessible inside the cluster
and through links. Pods may restart to remove the instance proxy.

Removal also deletes the route's settings, including authentication, internal
and public paths, redirects, the CSP allowlist and rate limit. Adding a route
later uses defaults rather than restoring these settings. The command lists
the settings being removed and asks for confirmation. Pass --yes to confirm
in scripts or skip the prompt.`,
	Args: cobra.ExactArgs(1),
	RunE: runAppRouteRemove,
}

func init() {
	for _, c := range []*cobra.Command{appRouteShowCmd, appRouteSetCmd, appRouteRemoveCmd} {
		c.Flags().String("project", "", "project name")
		c.Flags().String("environment", "", "target environment")
		appRouteCmd.AddCommand(c)
	}
	appRouteSetCmd.Flags().String("host", "", "hostname to serve the app on")
	appRouteSetCmd.Flags().String("path", "", "path the app is served under (default / for a new route)")
	appRouteRemoveCmd.Flags().BoolP("yes", "y", false, "remove without asking")
	appCmd.AddCommand(appRouteCmd)
}

func runAppRouteShow(cmd *cobra.Command, args []string) error {
	ns, k8sClient, err := resolveAppNamespace(cmd, args[0])
	if err != nil {
		return err
	}
	app, err := k8sClient.Dynamic().Resource(deployer.AppGVR).Namespace(ns).Get(context.Background(), args[0], metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading app %q: %w", args[0], err)
	}
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return err
	}
	writeRouteWithHost(os.Stdout, app, derivedRouteHost(args[0], namespaceEnvironment(context.Background(), k8sClient.Clientset(), ns), cluster.Domain))
	return nil
}

func writeRoute(out io.Writer, app *unstructured.Unstructured) {
	writeRouteWithHost(out, app, "")
}

func writeRouteWithHost(out io.Writer, app *unstructured.Unstructured, derivedHost string) {
	route, found, _ := unstructured.NestedMap(app.Object, "spec", "route")
	if !found {
		say(out, "\n  %s has no route. Add one with: kip app route set %s --host <host>\n\n", app.GetName(), app.GetName())
		return
	}
	view := routeView{app: app, derivedHost: derivedHost}
	path, _ := route["path"].(string)
	if path == "" {
		path = "/"
	}
	if host, _ := route["host"].(string); host != "" {
		say(out, "\n  Host:   %s\n", host)
	} else {
		say(out, "\n  Host:   %s (derived from the app name)\n", view.host())
	}
	say(out, "  Path:   %s\n", path)
	say(out, "  URL:    %s\n", view.url())
	if closed, _ := route["closed"].(bool); closed {
		// These paths reflect configuration; RouteReady below reports reconciliation status.
		if public, _, _ := unstructured.NestedStringSlice(route, "publicPaths"); len(public) > 0 {
			if base := strings.TrimSuffix(path, "/"); base != "" {
				for i := range public {
					public[i] = base + public[i]
				}
			}
			say(out, "  Closed: configured to publish only %s\n", strings.Join(public, ", "))
		} else {
			say(out, "  Closed: configured with no public paths\n")
		}
	}
	if gates := deployer.RouteGates(route); len(gates) > 0 {
		say(out, "  Gates:  %s\n", strings.Join(gates, "; "))
	}
	for _, c := range deployer.AppConditions(app) {
		if c.Type == "RouteReady" {
			writeConditions(out, []deployer.Condition{c})
		}
	}
	say(out, "\n")
}

func runAppRouteSet(cmd *cobra.Command, args []string) error {
	name := args[0]
	host, _ := cmd.Flags().GetString("host")
	path, _ := cmd.Flags().GetString("path")
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return err
	}
	ns, k8sClient, err := resolveAppNamespace(cmd, name)
	if err != nil {
		return err
	}
	ctx := context.Background()
	app, err := k8sClient.Dynamic().Resource(deployer.AppGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading app %q: %w", name, err)
	}
	derived := derivedRouteHost(name, namespaceEnvironment(ctx, k8sClient.Clientset(), ns), cluster.Domain)
	if _, hasRoute, _ := unstructured.NestedMap(app.Object, "spec", "route"); !hasRoute && host == "" {
		host = derived
	} else if hasRoute && host == "" && path == "" {
		return fmt.Errorf("nothing to change: pass --host, --path or both")
	}
	if host != "" {
		if err := manifest.ValidateRouteHost(host); err != nil {
			return err
		}
		if err := checkKipperRunHost(host, cluster.Domain); err != nil {
			return err
		}
	}
	if path != "" {
		if err := manifest.ValidateRoutePath(path); err != nil {
			return err
		}
	}

	d := &deployer.Deployer{Client: k8sClient.Clientset(), Dynamic: k8sClient.Dynamic()}
	if err := d.SetRoute(ctx, ns, name, host, path); err != nil {
		return err
	}
	fmt.Printf("\n  ✔  Route for %s saved\n", name)
	line, _ := waitForRouteOutcome(ctx, routeReader(d, ns, name, derived), routeOutcomeWait, time.Second)
	fmt.Printf("%s\n\n", line)
	return nil
}

// The gateway identifies a cluster by its registered name or the suffix after "--".
// Its wildcard certificate covers one label under kipper.run.
func checkKipperRunHost(host, clusterDomain string) error {
	if !strings.HasSuffix(host, ".kipper.run") {
		return nil
	}
	if !strings.HasSuffix(clusterDomain, ".kipper.run") {
		return fmt.Errorf("%s is a kipper.run host, but this cluster serves %s; use a host under your own domain", host, clusterDomain)
	}
	if strings.Contains(strings.TrimSuffix(host, ".kipper.run"), ".") {
		return fmt.Errorf("%s must have a single label under kipper.run to match the gateway's certificate", host)
	}
	if host == clusterDomain || strings.HasSuffix(host, "--"+clusterDomain) {
		return nil
	}
	return fmt.Errorf("%s is outside this cluster's kipper.run domain; use %s or <name>--%s", host, clusterDomain, clusterDomain)
}

func runAppRouteRemove(cmd *cobra.Command, args []string) error {
	name := args[0]
	ns, k8sClient, err := resolveAppNamespace(cmd, name)
	if err != nil {
		return err
	}
	ctx := context.Background()
	app, err := k8sClient.Dynamic().Resource(deployer.AppGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading app %q: %w", name, err)
	}
	route, found, _ := unstructured.NestedMap(app.Object, "spec", "route")
	if !found {
		return fmt.Errorf("app %q has no route to remove", name)
	}
	fmt.Printf("\n  Removing the route for %s (%s)\n", name, routeView{app: app}.url())
	for _, gate := range deployer.RouteGates(route) {
		fmt.Printf("    drops %s\n", gate)
	}
	yes, _ := cmd.Flags().GetBool("yes")
	if err := confirmRouteRemoval(os.Stdin, term.IsTerminal(int(os.Stdin.Fd())), yes); err != nil { //nolint:gosec // safe conversion for terminal check
		return err
	}
	d := &deployer.Deployer{Client: k8sClient.Clientset(), Dynamic: k8sClient.Dynamic()}
	if _, err := d.RemoveRoute(ctx, ns, name); err != nil {
		return err
	}
	ingresses := k8sClient.Clientset().NetworkingV1().Ingresses(ns)
	gone := func(ctx context.Context, ingress string) bool {
		_, err := ingresses.Get(ctx, ingress, metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, routeOutcomeWait)
	defer cancel()
	if waitForRouteWithdrawn(waitCtx, gone, name, time.Second) {
		fmt.Printf("  ✔  %s no longer has a public route\n\n", name)
		return nil
	}
	fmt.Printf("  !   Route removed from %s's settings; withdrawal is still unconfirmed. Check the cluster's Ingresses for %s\n\n", name, name)
	return nil
}

// waitForRouteWithdrawn checks the main route and both path-specific Ingresses.
// The gone callback must honor ctx to keep reads within the deadline.
func waitForRouteWithdrawn(ctx context.Context, gone func(context.Context, string) bool, name string, every time.Duration) bool {
	for {
		if gone(ctx, name) && gone(ctx, name+"-internal-paths") && gone(ctx, name+"-public-paths") {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(every):
		}
	}
}

// derivedRouteHost mirrors the controller's default hostname for an app route.
func derivedRouteHost(app, environment, clusterDomain string) string {
	prefix := app
	if environment != "" && environment != "default" {
		prefix = app + "-" + environment
	}
	return installer.SubdomainFor(prefix, clusterDomain)
}

// An empty environment falls back to the default hostname when the label is unavailable.
func namespaceEnvironment(ctx context.Context, clientset kubernetes.Interface, namespace string) string {
	ns, err := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return ns.Labels[labels.Environment]
}

func confirmRouteRemoval(in io.Reader, interactive, yes bool) error {
	if yes {
		return nil
	}
	if !interactive {
		return fmt.Errorf("removing a route also deletes its settings; pass --yes to confirm")
	}
	fmt.Print("  Remove it? [y/N]: ")
	answer, _ := bufio.NewReader(in).ReadString('\n')
	if isYes(answer) {
		return nil
	}
	return fmt.Errorf("route removal canceled")
}
