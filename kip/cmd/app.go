package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/getkipper/kipper/controller/pkg/secretname"
	"github.com/getkipper/kipper/kip/internal/auth"
	"github.com/getkipper/kipper/kip/internal/deployer"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/manifest"
	"github.com/getkipper/kipper/kip/internal/webhook"
)

var appCmd = &cobra.Command{
	Use:   "app",
	Short: "Manage applications",
}

var appDeployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Deploy an application",
	RunE:  runAppDeploy,
}

var appListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all applications",
	RunE:  runAppList,
}

var appLogsCmd = &cobra.Command{
	Use:   "logs [app-name]",
	Short: "Stream application logs",
	Args:  cobra.ExactArgs(1),
	RunE:  runAppLogs,
}

var appRestartCmd = &cobra.Command{
	Use:   "restart [app-name]",
	Short: "Restart an application",
	Args:  cobra.ExactArgs(1),
	RunE:  runAppRestart,
}

var appDeleteCmd = &cobra.Command{
	Use:   "delete [app-name]",
	Short: "Delete an application",
	Args:  cobra.ExactArgs(1),
	RunE:  runAppDelete,
}

var appUpdateCmd = &cobra.Command{
	Use:   "update [app-name]",
	Short: "Update an application's image, resource profile or redirect domains",
	Args:  cobra.ExactArgs(1),
	RunE:  runAppUpdate,
}

var appScaleCmd = &cobra.Command{
	Use:   "scale [app-name]",
	Short: "Scale an application to a given number of replicas",
	Args:  cobra.ExactArgs(1),
	RunE:  runAppScale,
}

var appRebuildCmd = &cobra.Command{
	Use:   "rebuild [app-name]",
	Short: "Trigger a new build from the configured git repository",
	Args:  cobra.ExactArgs(1),
	RunE:  runAppRebuild,
}

var appBuildLogsCmd = &cobra.Command{
	Use:   "build-logs [app-name]",
	Short: "Stream build logs for a git-based application",
	Args:  cobra.ExactArgs(1),
	RunE:  runAppBuildLogs,
}

func init() {
	appDeployCmd.Flags().String("name", "", "application name")
	appDeployCmd.Flags().String("image", "", "container image to deploy")
	appDeployCmd.Flags().String("git", "", "git repository URL to build from")
	appDeployCmd.Flags().String("branch", "main", "git branch to build")
	appDeployCmd.Flags().String("git-token", "", "git access token for private repos")
	appDeployCmd.Flags().String("build-memory", "", "memory limit for the in-cluster build, e.g. 6Gi (heavy SSR/bundler builds may need several GiB)")
	appDeployCmd.Flags().String("build-cpu", "", "CPU limit for the in-cluster build, e.g. 2")
	appDeployCmd.Flags().Int("port", 0, "application port")
	appDeployCmd.Flags().Int("replicas", 1, "number of replicas")
	appDeployCmd.Flags().String("project", "", "project namespace to deploy into")
	appDeployCmd.Flags().StringArray("env", nil, "environment variables (KEY=VALUE)")
	appDeployCmd.Flags().StringArray("secret", nil, "secrets (KEY=VALUE, or KEY alone for a hidden prompt)")
	appDeployCmd.Flags().String("route", "", "path route in format group/path (e.g. blog/api/users)")
	appDeployCmd.Flags().Bool("no-security-headers", false, "disable default security headers for this app")
	appDeployCmd.Flags().Int("rate-limit", 0, "custom rate limit in requests per second (0 = cluster default of 100)")
	appDeployCmd.Flags().StringSlice("redirect-from", nil, "hostnames that 301 to this app's own hostname (e.g. www.example.com); repeat or comma-separate, max 10")
	appDeployCmd.Flags().String("environment", "", "target environment (e.g. test, acc, prod)")
	appDeployCmd.Flags().String("memory", "", "memory limit (e.g. 256Mi, 1Gi)")
	appDeployCmd.Flags().String("cpu", "", "CPU limit (e.g. 500m, 1)")
	appDeployCmd.Flags().String("profile", "", "resource profile: lightweight, standard, compute-heavy, memory-heavy, or jvm")
	appDeployCmd.MarkFlagsMutuallyExclusive("profile", "memory")
	appDeployCmd.MarkFlagsMutuallyExclusive("profile", "cpu")

	_ = appDeployCmd.MarkFlagRequired("name")
	_ = appDeployCmd.MarkFlagRequired("port")

	appListCmd.Flags().String("project", "", "project name")
	appListCmd.Flags().String("environment", "", "target environment")

	appRestartCmd.Flags().String("project", "", "project name")
	appRestartCmd.Flags().String("environment", "", "target environment")

	appUpdateCmd.Flags().String("image", "", "new container image (e.g. registry.git.example.com/app:v2)")
	appUpdateCmd.Flags().String("profile", "", "resource profile: lightweight, standard, compute-heavy, memory-heavy, or jvm")
	appUpdateCmd.Flags().StringSlice("redirect-from", nil, "hostnames that 301 to this app's hostname (e.g. www.example.com); pass empty to clear, max 10")
	appUpdateCmd.Flags().String("project", "", "project name")
	appUpdateCmd.Flags().String("environment", "", "target environment")

	appLogsCmd.Flags().String("project", "", "project name")
	appLogsCmd.Flags().String("environment", "", "target environment")
	appLogsCmd.Flags().Int("tail", 500, "number of lines to show from the end of the logs")

	appDeleteCmd.Flags().String("project", "", "project name")
	appDeleteCmd.Flags().String("environment", "", "target environment")

	appScaleCmd.Flags().String("project", "", "project name")
	appScaleCmd.Flags().String("environment", "", "target environment")
	appScaleCmd.Flags().Int("replicas", 0, "number of replicas")
	_ = appScaleCmd.MarkFlagRequired("replicas")

	appRebuildCmd.Flags().String("project", "", "project name")
	appRebuildCmd.Flags().String("environment", "", "target environment")
	appRebuildCmd.Flags().String("commit", "", "git commit SHA to build (defaults to latest)")

	appBuildLogsCmd.Flags().String("project", "", "project name")
	appBuildLogsCmd.Flags().String("environment", "", "target environment")

	appCmd.AddCommand(appDeployCmd)
	appCmd.AddCommand(appListCmd)
	appCmd.AddCommand(appLogsCmd)
	appCmd.AddCommand(appRestartCmd)
	appCmd.AddCommand(appUpdateCmd)
	appCmd.AddCommand(appDeleteCmd)
	appCmd.AddCommand(appScaleCmd)
	appCmd.AddCommand(appRebuildCmd)
	appCmd.AddCommand(appBuildLogsCmd)
	rootCmd.AddCommand(appCmd)
}

func runAppDeploy(cmd *cobra.Command, args []string) error {
	name, _ := cmd.Flags().GetString("name")
	image, _ := cmd.Flags().GetString("image")
	gitURL, _ := cmd.Flags().GetString("git")
	gitBranch, _ := cmd.Flags().GetString("branch")
	port, _ := cmd.Flags().GetInt("port")
	replicas, _ := cmd.Flags().GetInt("replicas")
	project, _ := cmd.Flags().GetString("project")
	envSlice, _ := cmd.Flags().GetStringArray("env")
	route, _ := cmd.Flags().GetString("route")
	environment, _ := cmd.Flags().GetString("environment")

	if image == "" && gitURL == "" {
		return fmt.Errorf("either --image or --git is required")
	}
	if image != "" && gitURL != "" {
		return fmt.Errorf("--image and --git are mutually exclusive")
	}

	// Git-based apps use a placeholder image until the first build completes
	if gitURL != "" && image == "" {
		image = "busybox:latest"
	}

	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	if project == "" {
		project = "default"
	}

	// Resolve namespace from org + project + environment
	namespace := cluster.ResolveNamespace(project, environment)
	if environment != "" {
		clientset := k8sClient.Clientset()
		if _, err := clientset.CoreV1().Namespaces().Get(context.Background(), namespace, metav1.GetOptions{}); err != nil {
			return fmt.Errorf("environment %q not found for project %q (namespace %s does not exist — create the project first with: kip project create %s --environments %s,...)", environment, project, namespace, project, environment)
		}
	}

	env, err := parseEnvVars(envSlice)
	if err != nil {
		return err
	}

	secretSlice, _ := cmd.Flags().GetStringArray("secret")
	secrets, err := collectDeploySecrets(secretSlice, env)
	if err != nil {
		return err
	}

	warnIfSensitiveEnvKeys(env, "pass them via --secret instead")

	var routeGroup, routePath, domain string
	if route != "" {
		parts := strings.SplitN(route, "/", 2)
		routeGroup = parts[0]
		if len(parts) == 2 {
			routePath = "/" + parts[1]
		} else {
			routePath = "/"
		}
		if environment != "" {
			domain = installer.SubdomainFor(fmt.Sprintf("%s-%s", routeGroup, environment), cluster.Domain)
		} else {
			domain = installer.SubdomainFor(routeGroup, cluster.Domain)
		}
	} else {
		if environment != "" {
			domain = installer.SubdomainFor(fmt.Sprintf("%s-%s", name, environment), cluster.Domain)
		} else {
			domain = installer.SubdomainFor(name, cluster.Domain)
		}
	}

	d := &deployer.Deployer{Client: k8sClient.Clientset(), Dynamic: k8sClient.Dynamic()}
	ctx := context.Background()

	fmt.Printf("\n  Deploying %s...\n", name)
	noSecurityHeaders, _ := cmd.Flags().GetBool("no-security-headers")
	rateLimit, _ := cmd.Flags().GetInt("rate-limit")
	redirectFrom, _ := cmd.Flags().GetStringSlice("redirect-from")
	// Checked before anything is written, and by the same rule kipper.yaml
	// applies, so the two routes to this field cannot disagree about what a
	// valid redirect host is.
	if err := manifest.ValidateRedirectFromHosts(redirectFrom); err != nil {
		return err
	}
	memory, _ := cmd.Flags().GetString("memory")
	cpu, _ := cmd.Flags().GetString("cpu")
	profile, _ := cmd.Flags().GetString("profile")
	if profile != "" {
		if err := validateAppProfile(profile); err != nil {
			return err
		}
	}
	gitToken, _ := cmd.Flags().GetString("git-token")
	buildMemory, _ := cmd.Flags().GetString("build-memory")
	buildCPU, _ := cmd.Flags().GetString("build-cpu")

	// Record which flags the user actually set so an update only writes those
	// fields (a bare redeploy must not reset replicas, route or branch to their
	// flag defaults). The deployer keys the git-source clear off Changed["image"]
	// so switching a git app to an image drops the stale repo.
	changed := map[string]bool{}
	for _, f := range []string{"image", "git", "branch", "port", "replicas", "env", "route", "no-security-headers", "rate-limit", "redirect-from", "memory", "cpu", "profile", "build-memory", "build-cpu"} {
		if cmd.Flags().Changed(f) {
			changed[f] = true
		}
	}

	// Everything from the first Secret write to the App CR write runs inside
	// one closure so that EVERY error return after a Secret exists — the app
	// lookup, the secret pre-check, saveSecret, and Deploy itself — funnels
	// through cleanupDeploySecrets. If the App CR never materialises there is
	// no owner to garbage-collect an invocation-created Secret, so the failure
	// path removes it rather than leaving plaintext credentials behind.
	var gitCredentials string
	gitCredsCreated := false
	appExisted := false
	secretsCreated := false
	deployErr := func() error {
		// Store git credentials as a K8s Secret if provided.
		if gitToken != "" {
			secretName := name + "-git-credentials"
			clientset := k8sClient.Clientset()
			gitSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      secretName,
					Namespace: namespace,
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": "kipper",
						"kipper.run/app":               name,
					},
				},
				Data: map[string][]byte{
					"token": []byte(gitToken),
				},
			}
			_, err := clientset.CoreV1().Secrets(namespace).Create(ctx, gitSecret, metav1.CreateOptions{})
			switch {
			case err == nil:
				gitCredsCreated = true
			case errors.IsAlreadyExists(err):
				// Update the live object in place: a blind replace would
				// strip the controller reference the reconciler set and the
				// Secret would outlive the App again.
				err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
					live, getErr := clientset.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
					if getErr != nil {
						return getErr
					}
					live.Labels = gitSecret.Labels
					live.Data = gitSecret.Data
					_, updErr := clientset.CoreV1().Secrets(namespace).Update(ctx, live, metav1.UpdateOptions{})
					return updErr
				})
				if err != nil {
					return fmt.Errorf("updating git credentials secret: %w", err)
				}
			default:
				return fmt.Errorf("creating git credentials secret: %w", err)
			}
			gitCredentials = secretName
			fmt.Printf("  ✔  Git credentials stored\n")
		}

		// Write the secrets before the App CR so even the first pod start reads
		// them: the reconciler folds app-<app>-secrets into the environment it
		// publishes, and one written afterwards would not be in the generation
		// the first pod starts on. On a redeploy the CR update alone may not roll
		// the pods, so remember whether the app existed and restart it afterwards.
		if len(secrets) > 0 {
			// Only NotFound means "new app". Any other lookup failure aborts
			// before the Secret is touched: silently treating it as new would
			// skip the restart below and leave running pods on the old values.
			_, getErr := d.Dynamic.Resource(deployer.AppGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
			switch {
			case getErr == nil:
				appExisted = true
			case !errors.IsNotFound(getErr):
				return fmt.Errorf("checking for existing app before storing secrets: %w", getErr)
			}
			// The pre-check must distinguish "missing" from "unknown": a
			// transient error mistaken for "pre-existing" would exempt a Secret
			// this invocation is about to create from the failure cleanup.
			_, secErr := k8sClient.Clientset().CoreV1().Secrets(namespace).Get(ctx, secretname.Secrets(secretname.KindApp, name), metav1.GetOptions{})
			switch {
			case errors.IsNotFound(secErr):
				secretsCreated = true
			case secErr != nil:
				return fmt.Errorf("checking for existing secrets before writing: %w", secErr)
			}
			data := make(map[string][]byte, len(secrets))
			for k, v := range secrets {
				data[k] = []byte(v)
			}
			if err := saveSecret(ctx, k8sClient.Clientset(), secretname.KindApp, namespace, name, data); err != nil {
				return fmt.Errorf("storing secrets: %w", err)
			}
			fmt.Printf("  ✔  Secrets stored (%d)\n", len(secrets))
		}

		return d.Deploy(ctx, deployer.Options{
			Name:              name,
			Namespace:         namespace,
			Image:             image,
			Port:              int32(port),     //nolint:gosec // CLI flag values are bounded
			Replicas:          int32(replicas), //nolint:gosec // CLI flag values are bounded
			Domain:            domain,
			Env:               env,
			RouteGroup:        routeGroup,
			RoutePath:         routePath,
			NoSecurityHeaders: noSecurityHeaders,
			RateLimit:         rateLimit,
			RedirectFrom:      redirectFrom,
			MemoryLimit:       memory,
			CPULimit:          cpu,
			Profile:           profile,
			GitURL:            gitURL,
			GitBranch:         gitBranch,
			GitCredentials:    gitCredentials,
			BuildMemory:       buildMemory,
			BuildCPU:          buildCPU,
			Changed:           changed,
		})
	}()
	if deployErr != nil {
		cleanupDeploySecrets(ctx, k8sClient.Clientset(), d, namespace, name, secretsCreated, gitCredsCreated)
		// A rotation over a pre-existing Secret has already been written at
		// this point: the pods keep the old values, the Secret holds the new
		// ones, and any later restart would flip credentials silently.
		if len(secrets) > 0 && !secretsCreated {
			fmt.Printf("  ⚠  New secret values are stored but not live: the deploy failed, so running pods keep the previous values until a successful deploy or restart\n")
		}
		return deployErr
	}

	// A redeploy that only changed secrets leaves the CR untouched, so the
	// reconciler never rolls the pods; restart explicitly. A brand-new app
	// reads the secrets on its first start and needs no restart. A failed
	// restart fails the command: exit 0 would tell automation the new values
	// are live while the pods still run the old ones, which is exactly wrong
	// during a credential rotation.
	if appExisted && len(secrets) > 0 {
		if restartErr := restartWorkload(ctx, k8sClient.Clientset(), k8sClient.Dynamic(), secretname.KindApp, namespace, name); restartErr != nil {
			return fmt.Errorf("secrets stored, but the app restart failed so running pods still use the old values — run 'kip app restart %s': %w", name, restartErr)
		}
	}

	fmt.Printf("  ✔  Deployment created\n")
	fmt.Printf("  ✔  Service created\n")
	if gitURL != "" {
		fmt.Printf("  ✔  Git source configured: %s (%s)\n", gitURL, gitBranch)
		fmt.Printf("     Configure a webhook or run 'kip app rebuild %s' to trigger the first build\n", name)
	}
	if routeGroup != "" {
		fmt.Printf("  ✔  Route added: %s → %s\n", routePath, name)
	}
	// Report the app's real route, read back from the CR: a bare redeploy keeps
	// a console-set custom host, or may have no route at all, so the computed
	// default domain can be wrong.
	if app, getErr := d.Dynamic.Resource(deployer.AppGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{}); getErr == nil {
		if host, _, _ := unstructured.NestedString(app.Object, "spec", "route", "host"); host != "" {
			path, _, _ := unstructured.NestedString(app.Object, "spec", "route", "path")
			fmt.Printf("  ✔  Live at https://%s%s\n\n", host, path)
		} else {
			fmt.Printf("  ✔  Deployed (no public route)\n\n")
		}
	} else {
		// Fall back to the computed domain if the read-back fails.
		fmt.Printf("  ✔  Live at https://%s%s\n\n", domain, routePath)
	}

	return nil
}

func runAppList(cmd *cobra.Command, args []string) error {
	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	project, environment := resolveProjectAndEnvironment(cmd, cluster)
	if project == "" {
		project = "default"
	}
	namespace := cluster.ResolveNamespace(project, environment)

	d := &deployer.Deployer{Client: k8sClient.Clientset(), Dynamic: k8sClient.Dynamic()}
	ctx := context.Background()

	apps, err := d.List(ctx, namespace)
	if err != nil {
		return err
	}

	if len(apps) == 0 {
		fmt.Printf("\n  No apps deployed in %q\n\n", namespace)
		return nil
	}

	fmt.Printf("\n  %-20s %-10s %-40s %-10s\n", "NAME", "STATUS", "IMAGE", "READY")
	for _, app := range apps {
		fmt.Printf("  %-20s %-10s %-40s %d/%d\n",
			app.Name, app.Status, app.Image, app.Ready, app.Replicas)
	}
	fmt.Println()

	return nil
}

func runAppLogs(cmd *cobra.Command, args []string) error {
	appName := args[0]

	ns, k8sClient, err := resolveAppNamespace(cmd, appName)
	if err != nil {
		return err
	}

	ctx := context.Background()
	clientset := k8sClient.Clientset()

	pods, err := clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", appName),
	})
	if err != nil {
		return fmt.Errorf("listing pods: %w", err)
	}

	if len(pods.Items) == 0 {
		return fmt.Errorf("no pods found for app %q", appName)
	}

	pod := pods.Items[0]
	tail, _ := cmd.Flags().GetInt("tail")
	tailLines := int64(tail)
	req := clientset.CoreV1().Pods(ns).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: workloadContainerName(&pod, appName),
		Follow:    true,
		TailLines: &tailLines,
	})

	stream, err := req.Stream(ctx)
	if err != nil {
		return fmt.Errorf("opening log stream: %w", err)
	}
	defer func() { _ = stream.Close() }()

	_, err = io.Copy(os.Stdout, stream)
	return err
}

func runAppRestart(cmd *cobra.Command, args []string) error {
	appName := args[0]

	ns, k8sClient, err := resolveAppNamespace(cmd, appName)
	if err != nil {
		return err
	}

	d := &deployer.Deployer{Client: k8sClient.Clientset(), Dynamic: k8sClient.Dynamic()}
	ctx := context.Background()

	fmt.Printf("\n  Restarting %s...\n", appName)
	if err := d.Restart(ctx, ns, appName); err != nil {
		return err
	}
	fmt.Printf("  ✔  Restart triggered\n\n")

	return nil
}

func runAppUpdate(cmd *cobra.Command, args []string) error {
	appName := args[0]
	image, _ := cmd.Flags().GetString("image")
	profile, _ := cmd.Flags().GetString("profile")
	redirectSet := cmd.Flags().Changed("redirect-from")
	redirectFrom, _ := cmd.Flags().GetStringSlice("redirect-from")
	if redirectSet {
		if err := manifest.ValidateRedirectFromHosts(redirectFrom); err != nil {
			return err
		}
	}
	if image == "" && profile == "" && !redirectSet {
		return fmt.Errorf("nothing to update — pass --image, --profile, --redirect-from, or a combination")
	}

	ns, k8sClient, err := resolveAppNamespace(cmd, appName)
	if err != nil {
		return err
	}

	d := &deployer.Deployer{Client: k8sClient.Clientset(), Dynamic: k8sClient.Dynamic()}
	ctx := context.Background()

	if profile != "" {
		if err := validateAppProfile(profile); err != nil {
			return err
		}
		if err := d.UpdateProfile(ctx, ns, appName, profile); err != nil {
			return err
		}
		fmt.Printf("\n  ✔  Resource profile set to %s — rollout in progress\n", profile)
	}

	if redirectSet {
		if err := d.UpdateRedirectFrom(ctx, ns, appName, redirectFrom); err != nil {
			return err
		}
		if len(redirectFrom) == 0 {
			fmt.Printf("\n  ✔  Redirect domains cleared — no restart, the route is rebuilt in place\n")
		} else {
			fmt.Printf("\n  ✔  Redirecting %s — no restart, the route is rebuilt in place\n", strings.Join(redirectFrom, ", "))
			fmt.Printf("     Each needs its own A record pointing at this cluster.\n")
		}
	}

	if image != "" {
		fmt.Printf("\n  Updating %s → %s\n", appName, image)
		if err := d.UpdateImage(ctx, ns, appName, image); err != nil {
			return err
		}
		_ = webhook.RecordDeploy(ctx, k8sClient.Dynamic(), ns, appName, image, "", "manual")
		fmt.Printf("  ✔  Image updated — rollout in progress\n")
	}
	fmt.Println()

	return nil
}

// validateAppProfile checks a --profile value against the App CRD's named
// profiles. custom is excluded: it is what explicit --cpu/--memory values
// produce, never something to ask for by name.
func validateAppProfile(profile string) error {
	switch profile {
	case "lightweight", "standard", "compute-heavy", "memory-heavy", "jvm":
		return nil
	}
	return fmt.Errorf("unknown profile %q (available: lightweight, standard, compute-heavy, memory-heavy, jvm)", profile)
}

func runAppDelete(cmd *cobra.Command, args []string) error {
	appName := args[0]

	ns, k8sClient, err := resolveAppNamespace(cmd, appName)
	if err != nil {
		return err
	}

	d := &deployer.Deployer{Client: k8sClient.Clientset(), Dynamic: k8sClient.Dynamic()}
	ctx := context.Background()

	fmt.Printf("\n  Deleting %s...\n", appName)
	if err := d.Delete(ctx, ns, appName); err != nil {
		return err
	}
	fmt.Printf("  ✔  App deleted\n\n")

	return nil
}

func runAppScale(cmd *cobra.Command, args []string) error {
	appName := args[0]
	replicas, _ := cmd.Flags().GetInt("replicas")

	ns, k8sClient, err := resolveAppNamespace(cmd, appName)
	if err != nil {
		return err
	}

	d := &deployer.Deployer{Client: k8sClient.Clientset(), Dynamic: k8sClient.Dynamic()}
	ctx := context.Background()

	fmt.Printf("\n  Scaling %s to %d replicas...\n", appName, replicas)
	if err := d.Scale(ctx, ns, appName, int32(replicas)); err != nil { //nolint:gosec // CLI flag values are bounded
		return err
	}
	fmt.Printf("  ✔  Scaled to %d replicas\n\n", replicas)

	return nil
}

func runAppRebuild(cmd *cobra.Command, args []string) error {
	appName := args[0]
	commit, _ := cmd.Flags().GetString("commit")

	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	project, _ := cmd.Flags().GetString("project")
	environment, _ := cmd.Flags().GetString("environment")
	if project == "" {
		project = "default"
	}
	ns := cluster.ResolveNamespace(project, environment)

	// Verify the app has git configured
	ctx := context.Background()
	appObj, err := k8sClient.Dynamic().Resource(deployer.AppGVR).Namespace(ns).Get(ctx, appName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("app %q not found: %w", appName, err)
	}

	gitSpec, found, _ := unstructured.NestedMap(appObj.Object, "spec", "git")
	if !found || gitSpec == nil {
		return fmt.Errorf("app %q has no git source configured — deploy with --git first", appName)
	}

	// Get auth token
	store, err := auth.Load()
	if err != nil {
		return fmt.Errorf("loading auth: %w", err)
	}

	token, err := store.Token(cluster.Domain, cluster.DexHost())
	if err != nil {
		return err
	}

	// Call the console-api rebuild endpoint with auth
	consoleAPI := fmt.Sprintf("https://%s", cluster.ConsoleAPIHost())
	body, _ := json.Marshal(map[string]string{"commit": commit})
	url := fmt.Sprintf("%s/api/v1/projects/%s/apps/%s/rebuild", consoleAPI, ns, appName)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("calling rebuild API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		return authRejectedError(ctx, cluster)
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("rebuild failed: %s", string(respBody))
	}

	var result map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&result)

	fmt.Printf("\n  ✔  Build triggered for %s\n", appName)
	if job, ok := result["job"].(string); ok {
		fmt.Printf("     Job: %s\n", job)
	}
	fmt.Printf("     Stream logs: kip app build-logs %s --project %s", appName, project)
	if environment != "" {
		fmt.Printf(" --environment %s", environment)
	}
	fmt.Printf("\n\n")

	return nil
}

func runAppBuildLogs(cmd *cobra.Command, args []string) error {
	appName := args[0]

	ns, k8sClient, err := resolveAppNamespace(cmd, appName)
	if err != nil {
		return err
	}

	ctx := context.Background()
	clientset := k8sClient.Clientset()

	// Find the latest build pod
	pods, err := clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("kipper.run/app=%s,kipper.run/build=true", appName),
	})
	if err != nil || len(pods.Items) == 0 {
		return fmt.Errorf("no build found for %q", appName)
	}

	// Find the most recent pod
	latest := pods.Items[0]
	for _, p := range pods.Items[1:] {
		if p.CreationTimestamp.After(latest.CreationTimestamp.Time) {
			latest = p
		}
	}

	fmt.Printf("  Streaming build logs for %s (pod: %s)...\n\n", appName, latest.Name)

	// Stream all container logs (init + main)
	for _, c := range append(latest.Spec.InitContainers, latest.Spec.Containers...) {
		tailLines := int64(1000)
		logReq := clientset.CoreV1().Pods(ns).GetLogs(latest.Name, &corev1.PodLogOptions{
			Container: c.Name,
			Follow:    true,
			TailLines: &tailLines,
		})

		stream, streamErr := logReq.Stream(ctx)
		if streamErr != nil {
			continue
		}

		fmt.Printf("--- %s ---\n", c.Name)
		_, _ = io.Copy(os.Stdout, stream)
		_ = stream.Close()
	}

	return nil
}

// parseEnvVars turns KEY=VALUE strings into a map. A malformed entry (no '=',
// or an empty key) fails the whole batch rather than being silently dropped, so
// a typo like `env set app FOO` is reported instead of quietly setting nothing.
func parseEnvVars(envSlice []string) (map[string]string, error) {
	env := make(map[string]string, len(envSlice))
	for _, e := range envSlice {
		key, value, found := strings.Cut(e, "=")
		if !found {
			return nil, fmt.Errorf("invalid env var %q: expected KEY=VALUE", e)
		}
		if key == "" {
			return nil, fmt.Errorf("invalid env var %q: empty key", e)
		}
		env[key] = value
	}
	return env, nil
}

// promptSecretValue is swapped in tests; interactive hidden input otherwise.
var promptSecretValue = promptHidden

// collectDeploySecrets turns --secret entries into a key→value map. Entries
// with '=' carry the value inline (with a shell-history warning); bare keys
// prompt for the value with hidden input. A key also passed via --env fails
// the deploy: the same name in both stores would shadow one of the values at
// envFrom merge time. The overlap check runs before any prompt so the user is
// not asked to type values into a deploy that is going to be rejected.
func collectDeploySecrets(entries []string, env map[string]string) (map[string]string, error) {
	secrets := make(map[string]string, len(entries))
	var bare []string
	inlineCount := 0
	for _, e := range entries {
		key, value, found := strings.Cut(e, "=")
		if key == "" {
			// The entry may carry a secret value after the '='; naming it in
			// the error would put the value into terminal output and CI logs.
			return nil, fmt.Errorf("invalid --secret entry: empty key before '='")
		}
		if found {
			secrets[key] = value
			inlineCount++
		} else {
			bare = append(bare, key)
		}
	}

	var overlap []string
	for _, k := range bare {
		if _, ok := env[k]; ok {
			overlap = append(overlap, k)
		}
	}
	for k := range secrets {
		if _, ok := env[k]; ok {
			overlap = append(overlap, k)
		}
	}
	if len(overlap) > 0 {
		sort.Strings(overlap)
		return nil, fmt.Errorf("passed as both --env and --secret: %s", strings.Join(overlap, ", "))
	}

	if inlineCount > 0 {
		fmt.Fprintf(os.Stderr, "  Warning: inline secret values are visible in shell history. Use '--secret KEY' without =VALUE for hidden input.\n")
	}

	for _, k := range bare {
		if _, exists := secrets[k]; exists {
			continue
		}
		value, err := promptSecretValue(fmt.Sprintf("  Enter value for %s: ", k))
		if err != nil {
			return nil, fmt.Errorf("reading value for %s: %w", k, err)
		}
		secrets[k] = value
	}

	return secrets, nil
}

// cleanupDeploySecrets removes Secrets this deploy invocation created when the
// App CR never materialised. Without an App there is no owner to adopt them,
// so a failed deploy would otherwise leave plaintext credentials in the
// namespace indefinitely. Secrets that pre-existed the invocation are kept:
// they belong to an earlier deploy or another writer, and so is everything
// when the CR does exist, because the reconciler will adopt it. Deletion is
// best-effort; a failure is reported so the user can remove the Secret by
// hand.
func cleanupDeploySecrets(ctx context.Context, clientset kubernetes.Interface, d *deployer.Deployer, namespace, name string, secretsCreated, gitCredsCreated bool) {
	if !secretsCreated && !gitCredsCreated {
		return
	}
	if _, err := d.Dynamic.Resource(deployer.AppGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{}); err == nil || !errors.IsNotFound(err) {
		if err != nil {
			// The conservative keep is right when the check itself failed,
			// but silence would leave a plaintext Secret nobody knows about.
			fmt.Printf("  ⚠  Could not confirm whether app %s exists (%v); its stored secrets were kept — remove %s by hand if the deploy never went through\n", name, err, secretname.Secrets(secretname.KindApp, name))
		}
		return
	}
	var toDelete []string
	if secretsCreated {
		toDelete = append(toDelete, secretname.Secrets(secretname.KindApp, name))
	}
	if gitCredsCreated {
		toDelete = append(toDelete, name+"-git-credentials")
	}
	for _, sec := range toDelete {
		if err := clientset.CoreV1().Secrets(namespace).Delete(ctx, sec, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
			fmt.Fprintf(os.Stderr, "  ⚠  the deploy failed and the %s Secret it created could not be removed: %v — delete it manually: kubectl delete secret %s -n %s\n", sec, err, sec, namespace)
			continue
		}
		fmt.Fprintf(os.Stderr, "  Removed Secret %s created by this failed deploy so no credentials are left behind.\n", sec)
	}
}

// workloadNotFoundError reports a lookup that completed and matched nothing, as
// distinct from one that failed. Only the first may be defaulted away: an
// unavailable or forbidden API answers neither "it is here" nor "it is not".
type workloadNotFoundError struct {
	kind secretname.Kind
	name string
}

func (e *workloadNotFoundError) Error() string {
	return fmt.Sprintf("%s %q not found", e.kind, e.name)
}

// ambiguousWorkloadError reports a name that exists in more than one namespace,
// so no single namespace can be inferred from it.
type ambiguousWorkloadError struct {
	kind       secretname.Kind
	name       string
	namespaces []string
}

func (e *ambiguousWorkloadError) Error() string {
	return fmt.Sprintf("%s %q exists in %d namespaces (%s) — name the one you mean with --project, plus --environment if the project has environments",
		e.kind, e.name, len(e.namespaces), strings.Join(e.namespaces, ", "))
}

// findWorkloadNamespace locates the namespace holding the named workload of the
// given kind.
//
// It asks for that kind's CRs rather than matching the app=<name> Deployment
// label, which Apps and Functions share. With an App and a Function both called
// api the label match returns whichever the apiserver lists first, so
// `kip function env set api` could act in the App's namespace — and now that
// each kind reads its own Secret, write one the Function never sees.
//
// A name matching in several namespaces is an error rather than a choice. One
// project's environments each get a namespace, so the same app name across
// blog-test and blog-prod is ordinary, and returning the first match makes
// `kip app env set blog DATABASE_URL=…` write to whichever the apiserver
// happened to list first and restart it.
func findWorkloadNamespace(ctx context.Context, clientset kubernetes.Interface, dyn dynamic.Interface, kind secretname.Kind, name string) (string, error) {
	if dyn != nil {
		list, err := dyn.Resource(workloadGVR(kind)).List(ctx, metav1.ListOptions{})
		if err != nil {
			// This list is the authoritative lookup, so its failure is the
			// answer. Reporting "not found" instead would turn an RBAC denial or
			// an apiserver outage into a command that quietly acts somewhere else.
			return "", fmt.Errorf("looking for %s %q: %w", kind, name, err)
		}
		var found []string
		for i := range list.Items {
			if list.Items[i].GetName() == name {
				found = append(found, list.Items[i].GetNamespace())
			}
		}
		if ns, err := onlyNamespace(kind, name, found); err != nil || ns != "" {
			return ns, err
		}
	}

	// `kip app promote` builds a Deployment with no App CR behind it, so an App
	// may have nothing to find above. Functions always have a CR, and excluding
	// their Deployments here is the point of the resource-type clause.
	if kind == secretname.KindApp {
		deployments, err := clientset.AppsV1().Deployments("").List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app=%s,%s=%s,kipper.run/resource-type!=function", name, "app.kubernetes.io/managed-by", "kipper"),
		})
		if err != nil {
			return "", fmt.Errorf("finding %q: %w", name, err)
		}
		found := make([]string, 0, len(deployments.Items))
		for i := range deployments.Items {
			found = append(found, deployments.Items[i].Namespace)
		}
		if ns, err := onlyNamespace(kind, name, found); err != nil || ns != "" {
			return ns, err
		}
	}

	return "", &workloadNotFoundError{kind: kind, name: name}
}

// onlyNamespace reduces the namespaces a name matched to the single one a
// command can act in. It returns "" with no error when nothing matched, so the
// caller carries on to its next lookup, and an ambiguousWorkloadError when more
// than one did.
func onlyNamespace(kind secretname.Kind, name string, found []string) (string, error) {
	switch len(found) {
	case 0:
		return "", nil
	case 1:
		return found[0], nil
	default:
		sorted := append([]string(nil), found...)
		sort.Strings(sorted) // the same two namespaces must read the same way twice
		return "", &ambiguousWorkloadError{kind: kind, name: name, namespaces: sorted}
	}
}

// findFunctionNamespaceOrDefault falls back to the default namespace when no
// function of that name exists, which is what `kip function create` wants for
// one that is not there yet. An ambiguous name still fails: falling back would
// act on "default" while the function the user meant sits in one of the
// namespaces the error names. App commands have no equivalent, because they all
// address a workload that already exists.
func findFunctionNamespaceOrDefault(ctx context.Context, clientset kubernetes.Interface, dyn dynamic.Interface, name string) (string, error) {
	ns, err := findWorkloadNamespace(ctx, clientset, dyn, secretname.KindFunction, name)
	if err == nil {
		return ns, nil
	}
	// Default only on a lookup that answered "nothing of that name". An
	// ambiguous name, or a lookup that could not run at all, both mean the
	// namespace is unknown, and "default" holds none of the candidates.
	var notFound *workloadNotFoundError
	if stderrors.As(err, &notFound) {
		return "default", nil
	}
	return "", err
}
