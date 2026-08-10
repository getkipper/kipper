package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/getkipper/kipper/controller/pkg/applink"
	"github.com/getkipper/kipper/kip/internal/k8s"
)

var appLinkCmd = &cobra.Command{
	Use:   "link [target-app] [app]",
	Short: "Link an app so it can reach another app via an internal URL",
	Long: `Injects the target app's internal URL as an environment variable into the calling app.
For example, linking "domain-service" to "api-gateway" sets
DOMAIN_SERVICE_URL=http://domain-service.{namespace}.svc.cluster.local:{port} on api-gateway.

The target may live in another project, written as "project/app":

  kip app link docuseal/docuseal hrportal-backend

A cross-project link also opens the egress the calling app needs to reach it.
Workloads are otherwise confined to their own project, so without a link there
is no path between two projects at all, not by service name, not by pod, and
not through a public route. The allowance names both apps and the single port
the target's pods listen on, and it goes when the link or the calling app does.

You can only link to a project you can already read; a target you cannot see is
reported as not found.`,
	Args: cobra.ExactArgs(2),
	RunE: runAppLink,
}

var appUnlinkCmd = &cobra.Command{
	Use:   "unlink [target-app] [app]",
	Short: "Remove a linked app's URL from another app's environment",
	Long: `Removes the linked URL and, for a cross-project link, the egress allowance
that went with it. The target may be written as "project/app".`,
	Args: cobra.ExactArgs(2),
	RunE: runAppUnlink,
}

func init() {
	appLinkCmd.Flags().String("project", "", "project name")
	appLinkCmd.Flags().String("environment", "", "target environment")
	appLinkCmd.Flags().Bool("public", false, "use the target's public route URL instead of internal Kubernetes DNS")

	appUnlinkCmd.Flags().String("project", "", "project name")
	appUnlinkCmd.Flags().String("environment", "", "target environment")

	appCmd.AddCommand(appLinkCmd)
	appCmd.AddCommand(appUnlinkCmd)
}

// maxLinks mirrors the CRD's bound on spec.links. Mirrored rather than imported
// because the bound lives on the Go type in console-api, which this module
// cannot reach; both are 64 and a change to one wants the other.
const maxLinks = 64

var appGVR = schema.GroupVersionResource{
	Group:    "kipper.run",
	Version:  "v1alpha1",
	Resource: "apps",
}

// parseLinkTarget splits a link target into a project and an app name.
// "docuseal/docuseal" names another project; "docuseal" means the calling app's
// own project, which is what every link meant before cross-project ones existed.
func parseLinkTarget(target string) (project, app string, err error) {
	parts := strings.Split(target, "/")
	switch len(parts) {
	case 1:
		if parts[0] == "" {
			return "", "", fmt.Errorf("no target app given")
		}
		return "", parts[0], nil
	case 2:
		if parts[0] == "" || parts[1] == "" {
			return "", "", fmt.Errorf("target %q should be written as project/app", target)
		}
		return parts[0], parts[1], nil
	default:
		return "", "", fmt.Errorf("target %q should be written as app or project/app", target)
	}
}

// resolveLinkNamespace returns the namespace the target app lives in. A target
// naming another project goes through the same rules the rest of the CLI uses,
// so a link records the namespace an operator would see in kubectl.
func resolveLinkNamespace(cmd *cobra.Command, callerNS, targetProject string) (string, error) {
	if targetProject == "" {
		return callerNS, nil
	}
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return "", err
	}
	environment, _ := cmd.Flags().GetString("environment")
	return cluster.ResolveNamespace(targetProject, environment), nil
}

func runAppLink(cmd *cobra.Command, args []string) error {
	targetProject, target, err := parseLinkTarget(args[0])
	if err != nil {
		return err
	}
	app := args[1]

	ns, k8sClient, err := resolveAppNamespace(cmd, app)
	if err != nil {
		return err
	}
	targetNS, err := resolveLinkNamespace(cmd, ns, targetProject)
	if err != nil {
		return err
	}
	if target == app && targetNS == ns {
		return fmt.Errorf("an app cannot link to itself")
	}

	ctx := context.Background()
	dynamic := k8sClient.Dynamic()

	// Reading the target with the operator's own credentials is what decides
	// whether they may link to it: a project they cannot see reports not found,
	// so the existing permission model answers the question without a new one.
	targetObj, err := dynamic.Resource(appGVR).Namespace(targetNS).Get(ctx, target, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("target app %q not found in %s", target, targetNS)
	}

	port, _, _ := unstructured.NestedInt64(targetObj.Object, "spec", "port")
	if port == 0 {
		return fmt.Errorf("target app %q has no port configured", target)
	}

	// Said here rather than left to be discovered as a link that quietly carries
	// no traffic. The link is still recorded, so it starts working the moment
	// the other project agrees.
	consented := true
	if targetProject != "" && targetNS != ns {
		consented, err = targetProjectAllowsLinksFrom(ctx, k8sClient, targetProject, cmd)
		if err != nil {
			return err
		}
	}

	appObj, err := dynamic.Resource(appGVR).Namespace(ns).Get(ctx, app, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("app %q not found in %s", app, ns)
	}

	isPublic, _ := cmd.Flags().GetBool("public")

	envKey := applink.EnvKey(target)
	var url string
	if isPublic {
		host, _, _ := unstructured.NestedString(targetObj.Object, "spec", "route", "host")
		if host == "" {
			return fmt.Errorf("target app %q has no public route. Create one first or remove --public", target)
		}
		routePath, _, _ := unstructured.NestedString(targetObj.Object, "spec", "route", "path")
		if routePath == "/" {
			routePath = ""
		}
		url = "https://" + host + routePath
	} else {
		url = fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", target, targetNS, port)
	}

	// A public link is a plain environment variable and nothing more: the URL is
	// for a browser, no egress policy applies to it, and there is no declaration
	// to derive it from. So that one is stored, and it withdraws any internal
	// link to the same target.
	//
	// An internal link stores nothing. spec.links is the declaration and the
	// reconciler derives the address from it on every pass, so a target that
	// changes port takes its callers with it rather than leaving them dialling
	// a number that was right once.
	env, _, _ := unstructured.NestedStringMap(appObj.Object, "spec", "env")
	if env == nil {
		env = make(map[string]string)
	}
	if isPublic {
		env[envKey] = url
		if _, rerr := removeAppLink(appObj, target); rerr != nil {
			return rerr
		}
	} else {
		// If the operator already set that variable, the link is refused rather
		// than taking the name. Deleting it destroys a value somebody chose, and
		// leaving it is worse: the derived one is an explicit container env entry
		// and would silently win over theirs.
		if _, taken := env[envKey]; taken {
			return fmt.Errorf("%s is already set on %s, and linking to %q would set it too.\n"+
				"    Remove it first if the link should own it: kip app env delete %s %s",
				envKey, app, target, app, envKey)
		}
		if err := setAppLink(appObj, target, targetNS); err != nil {
			return err
		}
	}
	if err := unstructured.SetNestedStringMap(appObj.Object, env, "spec", "env"); err != nil {
		return fmt.Errorf("setting env: %w", err)
	}

	if _, err := dynamic.Resource(appGVR).Namespace(ns).Update(ctx, appObj, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating app: %w", err)
	}

	fmt.Printf("\n  ✔  Linked %s → %s\n", target, app)
	fmt.Printf("     %s=%s\n", envKey, url)
	switch {
	case isPublic:
		fmt.Printf("\n     A public URL is for code running in a browser. A server-side call to it\n")
		fmt.Printf("     leaves the cluster and comes back through the gateway, which the workload\n")
		fmt.Printf("     egress policy blocks. Drop --public for the internal address.\n")
	case targetNS != ns && !consented:
		fmt.Printf("\n     No traffic is allowed yet: %s has not agreed to be linked to.\n", targetProject)
		fmt.Printf("     Someone who owns it runs:\n")
		fmt.Printf("       kip project allow-links <your-project> --project %s\n", targetProject)
		fmt.Printf("     The link starts working as soon as they do.\n")
	case targetNS != ns:
		// No port here. The policy allows the port the target's pods listen on,
		// which is the sidecar's when the target serves a public route, and the
		// CLI cannot know that — it does not hold the reconciler's sidecar
		// image. The address above is what the caller dials, and that is the
		// number worth printing.
		fmt.Printf("     Egress opened to %s in %s\n", target, targetNS)
	}
	fmt.Println()

	return nil
}

// setAppLink records the target on the calling app, replacing any earlier link
// to the same app. The list is what this app depends on; the reconciler turns
// the entries naming another namespace into egress.
func setAppLink(appObj *unstructured.Unstructured, target, targetNS string) error {
	links, _, err := unstructured.NestedSlice(appObj.Object, "spec", "links")
	if err != nil {
		return fmt.Errorf("reading links: %w", err)
	}
	kept := make([]any, 0, len(links)+1)
	for _, raw := range links {
		if entry, ok := raw.(map[string]any); ok && entry["app"] == target {
			continue
		}
		kept = append(kept, raw)
	}
	// The CRD caps the list. Checked here so reaching the limit reads as a limit
	// rather than as the API server rejecting an update the command built.
	if len(kept) >= maxLinks {
		return fmt.Errorf("this app already declares the most links an app may have (%d).\n"+
			"    Unlink one first: kip app unlink <target> <app>", maxLinks)
	}
	kept = append(kept, map[string]any{"app": target, "namespace": targetNS})
	if err := unstructured.SetNestedSlice(appObj.Object, kept, "spec", "links"); err != nil {
		return fmt.Errorf("setting links: %w", err)
	}
	return nil
}

// removeAppLink drops the target from the calling app's links and reports
// whether anything was there to drop.
func removeAppLink(appObj *unstructured.Unstructured, target string) (bool, error) {
	links, _, err := unstructured.NestedSlice(appObj.Object, "spec", "links")
	if err != nil {
		return false, fmt.Errorf("reading links: %w", err)
	}
	kept := make([]any, 0, len(links))
	removed := false
	for _, raw := range links {
		if entry, ok := raw.(map[string]any); ok && entry["app"] == target {
			removed = true
			continue
		}
		kept = append(kept, raw)
	}
	if !removed {
		return false, nil
	}
	if len(kept) == 0 {
		unstructured.RemoveNestedField(appObj.Object, "spec", "links")
		return true, nil
	}
	if err := unstructured.SetNestedSlice(appObj.Object, kept, "spec", "links"); err != nil {
		return false, fmt.Errorf("setting links: %w", err)
	}
	return true, nil
}

func runAppUnlink(cmd *cobra.Command, args []string) error {
	_, target, err := parseLinkTarget(args[0])
	if err != nil {
		return err
	}
	app := args[1]

	ns, k8sClient, err := resolveAppNamespace(cmd, app)
	if err != nil {
		return err
	}

	ctx := context.Background()
	dynamic := k8sClient.Dynamic()

	appObj, err := dynamic.Resource(appGVR).Namespace(ns).Get(ctx, app, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("app %q not found in %s", app, ns)
	}

	envKey := applink.EnvKey(target)
	env, _, _ := unstructured.NestedStringMap(appObj.Object, "spec", "env")
	_, hadEnv := env[envKey]

	hadLink, err := removeAppLink(appObj, target)
	if err != nil {
		return err
	}
	if !hadEnv && !hadLink {
		fmt.Printf("\n  No link to %s found\n\n", target)
		return nil
	}

	if hadEnv {
		delete(env, envKey)
		if err := unstructured.SetNestedStringMap(appObj.Object, env, "spec", "env"); err != nil {
			return fmt.Errorf("setting env: %w", err)
		}
	}

	if _, err := dynamic.Resource(appGVR).Namespace(ns).Update(ctx, appObj, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating app: %w", err)
	}

	fmt.Printf("\n  ✔  Unlinked %s from %s\n", target, app)
	if hadEnv {
		fmt.Printf("     Removed %s\n", envKey)
	}
	if hadLink {
		fmt.Printf("     Egress to %s withdrawn\n", target)
	}
	fmt.Println()

	return nil
}

// targetProjectAllowsLinksFrom reports whether the target project has already
// agreed to be linked to from the calling app's project. A project the operator
// cannot read is reported as not consenting rather than as an error: the link is
// still worth recording, and the reconciler is what decides in the end.
func targetProjectAllowsLinksFrom(ctx context.Context, k8sClient *k8s.Client, targetProject string, cmd *cobra.Command) (bool, error) {
	callerProject, err := resolveProjectName(cmd)
	if err != nil {
		return false, err
	}
	// A project already reaches its own apps, across its environments as much as
	// within one, and the reconciler opens those without consulting anybody. The
	// probe has to know that too: reporting a same-project link as unconsented
	// contradicts traffic that is already flowing, and the remedy it would go on
	// to suggest is a command that refuses to run.
	if callerProject != "" && callerProject == targetProject {
		return true, nil
	}
	obj, err := k8sClient.Dynamic().Resource(projectGVR).Get(ctx, targetProject, metav1.GetOptions{})
	if err != nil {
		//nolint:nilerr // A project the operator cannot read reads as no consent, per this function's contract. Reporting the error instead would turn a link worth recording into a failed command.
		return false, nil
	}
	allowed, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "allowLinksFrom")
	for _, name := range allowed {
		if name == callerProject {
			return true, nil
		}
	}
	return false, nil
}
