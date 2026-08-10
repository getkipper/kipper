package cmd

import (
	"bufio"
	"context"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/getkipper/kipper/controller/pkg/envtemplate"
	"github.com/getkipper/kipper/controller/pkg/secretname"
	"github.com/getkipper/kipper/kip/internal/deployer"
	"github.com/getkipper/kipper/kip/internal/k8s"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

var envCmd = &cobra.Command{
	Use:   "env",
	Short: "Manage application environment variables",
}

var envSetCmd = &cobra.Command{
	Use:   "set [app-name] KEY=VALUE...",
	Short: "Set environment variables for an application",
	Args:  cobra.MinimumNArgs(2),
	RunE:  runEnvSet,
}

var envListCmd = &cobra.Command{
	Use:   "list [app-name]",
	Short: "List environment variables for an application",
	Args:  cobra.ExactArgs(1),
	RunE:  runEnvList,
}

var envDeleteCmd = &cobra.Command{
	Use:   "delete [app-name] KEY",
	Short: "Delete an environment variable from an application",
	Args:  cobra.ExactArgs(2),
	RunE:  runEnvDelete,
}

func init() {
	envSetCmd.Flags().String("from-file", "", "load env vars from a file (KEY=VALUE per line)")
	envSetCmd.Flags().String("project", "", "project name")
	envSetCmd.Flags().String("environment", "", "target environment (e.g. test, acc, prod)")

	envListCmd.Flags().String("project", "", "project name")
	envListCmd.Flags().String("environment", "", "target environment")

	restartConfigFlag(envSetCmd, envDeleteCmd, secretSetCmd, secretDeleteCmd, secretRollbackCmd)

	envDeleteCmd.Flags().String("project", "", "project name")
	envDeleteCmd.Flags().String("environment", "", "target environment")

	envCmd.AddCommand(envSetCmd)
	envCmd.AddCommand(envListCmd)
	envCmd.AddCommand(envDeleteCmd)
	appCmd.AddCommand(envCmd)
}

func resolveAppNamespace(cmd *cobra.Command, appName string) (string, *k8s.Client, error) {
	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return "", nil, err
	}

	project, _ := cmd.Flags().GetString("project")
	environment, _ := cmd.Flags().GetString("environment")

	if project != "" {
		ns := cluster.ResolveNamespace(project, environment)
		return ns, k8sClient, nil
	}

	// Every command reaching here is registered under `kip app` or
	// `kip function`, so the tree names the kind to look for.
	ctx := context.Background()
	ns, err := findWorkloadNamespace(ctx, k8sClient.Clientset(), k8sClient.Dynamic(), workloadKind(cmd), appName)
	if err != nil {
		return "", nil, err
	}
	return ns, k8sClient, nil
}

// workloadClients resolves the namespace and the clients a workload command
// acts through. It is a variable so tests can drive the real handlers against
// fake clients rather than a stand-in: the defect this seam exists for was a
// handler deriving the workload kind and then applying it to only some of its
// writes, which no test of the helpers on their own can see.
var workloadClients = func(cmd *cobra.Command, name string) (string, kubernetes.Interface, dynamic.Interface, error) {
	ns, k8sClient, err := resolveAppNamespace(cmd, name)
	if err != nil {
		return "", nil, nil, err
	}
	return ns, k8sClient.Clientset(), k8sClient.Dynamic(), nil
}

func runEnvSet(cmd *cobra.Command, args []string) error {
	appName := args[0]
	fromFile, _ := cmd.Flags().GetString("from-file")

	ns, clientset, dyn, err := workloadClients(cmd, appName)
	if err != nil {
		return err
	}

	ctx := context.Background()
	kind := workloadKind(cmd)

	existing, home, err := readWorkloadEnv(ctx, dyn, clientset, kind, ns, appName)
	if err != nil {
		return err
	}

	set := make(map[string]string)
	if fromFile != "" {
		pairs, err := readEnvFile(fromFile)
		if err != nil {
			return err
		}
		for k, v := range pairs {
			existing[k] = v
			set[k] = v
		}
	} else {
		env, parseErr := parseEnvVars(args[1:])
		if parseErr != nil {
			return parseErr
		}
		for k, v := range env {
			existing[k] = v
			set[k] = v
		}
	}

	// runEnvSet backs both `kip app env set` and `kip function env set`; the
	// grandparent command ("app" or "function") names the right secret command.
	warnIfSensitiveEnvKeys(set, fmt.Sprintf("use 'kip %s secret set' instead", cmd.Parent().Parent().Name()))

	if err := writeWorkloadEnv(ctx, dyn, clientset, kind, ns, appName, existing, home); err != nil {
		return err
	}

	rolled, err := checkDirectEnvConflicts(ctx, clientset, kind, ns, appName, slices.Collect(maps.Keys(existing)))
	if err != nil {
		return err
	}

	fmt.Printf("  ✔  Environment updated for %s\n", appName)
	if rolled {
		// The cleanup already changed the pod template, so the pods are coming
		// back on the new values. Saying they kept the old ones would be wrong.
		fmt.Printf("      Removing the direct entries restarted %s, so the new values are live.\n", appName)
		return nil
	}
	return applyConfigChange(cmd, ctx, clientset, dyn, kind, ns, appName)
}

// workloadKind reports which workload a command operates on. The env and secret
// commands are registered under both `kip app` and `kip function` and share this
// code, but they address different Secrets and different CRs, so the ancestor
// command is what distinguishes them: `kip app env set` against
// `kip function env set`.
func workloadKind(cmd *cobra.Command) secretname.Kind {
	for c := cmd; c != nil; c = c.Parent() {
		if c.Name() == "function" {
			return secretname.KindFunction
		}
	}
	return secretname.KindApp
}

// workloadGVR is the CR that owns a workload kind's spec.env. The reconciler
// rebuilds the env Secret from that field, so an env change written to the wrong
// kind's CR is reverted on the next pass — and, where both kinds share a name,
// lands in the other workload's configuration. Every kind is named rather than
// falling through to Apps, because that fallthrough is the shape of the bug.
func workloadGVR(kind secretname.Kind) schema.GroupVersionResource {
	switch kind {
	case secretname.KindFunction:
		return manifest.FunctionGVR
	case secretname.KindJob:
		return manifest.JobGVR
	case secretname.KindApp:
		return manifest.AppGVR
	}
	// Unreachable: Kind has exactly the three values above.
	return manifest.AppGVR
}

// applyConfigChange puts a saved configuration change in front of the running
// pods, or says that it has not been.
//
// Saving without restarting is the default because restarting is the
// destructive half: it drops every connection the workload is serving, and
// doing that as a side effect of setting a variable is not what the person
// typing it asked for. The console has always worked this way — it writes the
// change and raises a banner — and the platform is built for it, since an
// environment is published as its own immutable generation and the pods move to
// it when they restart rather than when it is written.
//
// What the CLI owed was not the restart but the sentence: a container reads
// envFrom once, at start, so a command that saves and says nothing leaves
// someone believing a change took effect that has not.
// A restart that was asked for and did not happen fails the command. Exit 0
// would tell automation the new values are live while the pods still run the
// old ones, which is exactly wrong during a credential rotation — the same rule
// `kip app deploy` applies to the secrets it stores.
func applyConfigChange(cmd *cobra.Command, ctx context.Context, clientset kubernetes.Interface, dyn dynamic.Interface, kind secretname.Kind, ns, name string) error {
	if restart, _ := cmd.Flags().GetBool("restart"); restart {
		if err := restartWorkload(ctx, clientset, dyn, kind, ns, name); err != nil {
			return fmt.Errorf("the change was saved, but the restart failed so running pods still use the old values — %s: %w",
				howToRestart(kind, name), err)
		}
		return nil
	}
	fmt.Printf("      Saved. The running pods keep the values they started with until %s restarts.\n", name)
	// --restart is named first because it is the one that exists for every kind:
	// there is no `kip function restart`, and suggesting one would send a
	// function's operator to a command that is not there.
	fmt.Printf("      Re-run with --restart to apply it now")
	if kind == secretname.KindApp {
		fmt.Printf(", or %s", howToRestart(kind, name))
	}
	fmt.Printf(".\n")
	return nil
}

// requireWorkload refuses to write configuration for a workload that is not
// there.
//
// A mistyped name with an explicit --project resolves to a namespace without
// anything being looked up, so the secret was written to
// <kind>-<name>-secrets and the command reported success. That leaves a
// credential in an object nothing owns, which a later workload of that name
// would then read.
func requireWorkload(ctx context.Context, clientset kubernetes.Interface, dyn dynamic.Interface, kind secretname.Kind, ns, name string) error {
	_, err := dyn.Resource(workloadGVR(kind)).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("looking for %s %q in %s: %w", strings.ToLower(string(kind)), name, ns, err)
	}
	// A promoted app has no CR, and its Deployment carries the stamp that says
	// so. That shape is a workload too.
	if promotedDeployment(ctx, clientset, kind, ns, name) != nil {
		return nil
	}
	return fmt.Errorf("no %s named %q in %s", strings.ToLower(string(kind)), name, ns)
}

// restartConfigFlag is the opt-in on every command that changes a workload's
// configuration.
func restartConfigFlag(cmds ...*cobra.Command) {
	for _, c := range cmds {
		c.Flags().Bool("restart", false, "restart the workload so the change takes effect now")
	}
}

// restartWorkload triggers a rolling restart by bumping the restartedAt
// annotation on the workload's CR, which its reconciler projects onto the pod
// template. Restarting through the CR keeps the CLI and the console on one
// mechanism, so a later reconcile doesn't treat a direct Deployment edit as
// drift and roll the workload a second time.
//
// A promoted app has no CR for anything to project from, so its pod template is
// annotated directly. Without that the command saves new values, reports
// success, and leaves every running pod on the old ones — a container reads
// envFrom once, at start.
func restartWorkload(ctx context.Context, clientset kubernetes.Interface, dyn dynamic.Interface, kind secretname.Kind, ns, name string) error {
	d := &deployer.Deployer{Client: clientset, Dynamic: dyn}
	err := d.RestartWorkload(ctx, workloadGVR(kind), string(kind), ns, name)
	if err == nil {
		return nil
	}
	deploy := promotedDeployment(ctx, clientset, kind, ns, name)
	if deploy == nil {
		return err
	}
	return restartPromotedDeployment(ctx, clientset, deploy)
}

// restartPromotedDeployment rolls a Deployment nothing reconciles, the way
// `kubectl rollout restart` does.
func restartPromotedDeployment(ctx context.Context, clientset kubernetes.Interface, deploy *appsv1.Deployment) error {
	if deploy.Spec.Template.Annotations == nil {
		deploy.Spec.Template.Annotations = map[string]string{}
	}
	deploy.Spec.Template.Annotations["kipper.run/restartedAt"] = time.Now().Format(time.RFC3339Nano)
	if _, err := clientset.AppsV1().Deployments(deploy.Namespace).Update(ctx, deploy, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("restarting %s: %w", deploy.Name, err)
	}
	return nil
}

// promotedDeployment returns the Deployment of a promoted app — one `kip app
// promote` built directly, with no CR behind it — or nil for anything else.
//
// The CR simply being absent does not say that. A controller-managed Deployment
// outlives its App CR for as long as garbage collection takes, and a stranded
// one outlives it indefinitely, so "no CR" alone would treat an ordinary app
// mid-deletion as a promoted one. Both marks are required: nothing owns it, and
// the promote command's own stamp is on its pod template.
func promotedDeployment(ctx context.Context, clientset kubernetes.Interface, kind secretname.Kind, ns, name string) *appsv1.Deployment {
	deploy := workloadDeployment(ctx, clientset, kind, ns, name)
	if deploy == nil {
		return nil
	}
	if metav1.GetControllerOf(deploy) != nil {
		return nil
	}
	if _, promoted := deploy.Spec.Template.Annotations["kipper.run/promoted-from"]; !promoted {
		return nil
	}
	return deploy
}

// howToRestart says how to apply a saved change, in terms of the workload kind.
func howToRestart(kind secretname.Kind, name string) string {
	if kind == secretname.KindApp {
		return fmt.Sprintf("run 'kip app restart %s'", name)
	}
	// Only Apps have a restart, in the CLI and in the console alike: there is one
	// restart route and it is theirs. So for every other kind the command itself
	// is the whole of the remedy, and naming anything else sends someone already
	// stuck somewhere that does not exist.
	return "re-run the command with --restart once that is cleared"
}

// looksLikeSecret reports whether an env var name suggests a sensitive value
// that belongs in a secret rather than a plain-text env var. Env values are
// stored in the App CR's spec.env and appear in `kip export`, so a password or
// token set this way is far more exposed than one in `kip app secret set`.
func looksLikeSecret(key string) bool {
	upper := strings.ToUpper(key)
	for _, token := range []string{"PASSWORD", "PASSWD", "SECRET", "TOKEN", "APIKEY", "API_KEY", "ACCESS_KEY", "PRIVATE_KEY", "CREDENTIAL", "PASSPHRASE"} {
		if strings.Contains(upper, token) {
			return true
		}
	}
	return false
}

// embeddedUserinfo matches a connection string carrying a literal password, as
// in postgresql://kipper:s3cret@host:5432/app.
var embeddedUserinfo = regexp.MustCompile(`://[^/@\s]*:[^/@\s]+@`)

// valueLooksLikeCredential reports whether a value carries a credential
// whatever its key is called. looksLikeSecret matches none of DATABASE_URL, so
// a URL with an embedded password is otherwise stored in silence.
//
// Placeholders are removed before the test. A templated URL resolves its
// credential into a Secret and never stores one on the CR, so flagging it would
// warn against the safe construction and teach people to ignore the warning.
func valueLooksLikeCredential(value string) bool {
	return embeddedUserinfo.MatchString(envtemplate.StripPlaceholders(value))
}

// flagSensitiveEnv returns the keys whose name or value suggests a credential,
// sorted so the warning reads the same way twice.
func flagSensitiveEnv(set map[string]string) []string {
	var flagged []string
	for k, v := range set {
		if looksLikeSecret(k) || valueLooksLikeCredential(v) {
			flagged = append(flagged, k)
		}
	}
	sort.Strings(flagged)
	return flagged
}

// warnIfSensitiveEnvKeys nudges the user toward secrets when a variable they
// just set as a plain-text env var looks like one. remedy names the
// command-appropriate alternative ("use 'kip app secret set' instead"). It only
// warns; the value is still stored, since the heuristic can misfire
// (e.g. TOKEN_BUCKET).
func warnIfSensitiveEnvKeys(set map[string]string, remedy string) {
	flagged := flagSensitiveEnv(set)
	if len(flagged) == 0 {
		return
	}

	fmt.Fprintf(os.Stderr, "\n  Warning: these look like secrets, but env vars are stored in plain text\n")
	fmt.Fprintf(os.Stderr, "  (visible in the App resource, in `kip export`, and to anyone with read access):\n")
	for _, k := range flagged {
		fmt.Fprintf(os.Stderr, "    - %s\n", k)
	}
	fmt.Fprintf(os.Stderr, "  If they hold sensitive values, %s.\n", remedy)
}

// workloadDeployment returns the Deployment serving this workload, or nil when
// there is none of its kind.
//
// A Deployment name is unique per namespace while a workload name is unique only
// per kind, so the name alone does not identify one: a cron-only Function has no
// Deployment, and an App of the same name does. Fetching by name would hand a
// `kip function` command the App's Deployment to describe and to edit. Function
// Deployments are the ones the reconciler labels kipper.run/resource-type.
func workloadDeployment(ctx context.Context, clientset kubernetes.Interface, kind secretname.Kind, ns, name string) *appsv1.Deployment {
	deploy, err := clientset.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	if (deploy.Labels["kipper.run/resource-type"] == "function") != (kind == secretname.KindFunction) {
		return nil
	}
	return deploy
}

// checkDirectEnvConflicts warns if the workload's deployment has direct env:
// entries that would override envFrom values. In Kubernetes, direct env: always
// takes precedence over envFrom, so envFrom values are silently ignored.
// checkDirectEnvConflicts reports whether it changed the pod template, because
// removing a direct entry rolls the workload — the operator accepted a restart
// without the word being used, and the caller must not then tell them their
// pods kept the old values.
func checkDirectEnvConflicts(ctx context.Context, clientset kubernetes.Interface, kind secretname.Kind, ns, appName string, names []string) (rolled bool, err error) {
	deploy := workloadDeployment(ctx, clientset, kind, ns, appName)
	if deploy == nil {
		// No Deployment of this kind: not created yet, or the workload doesn't
		// have one (a cron-only Function). Either way there is nothing to check
		// and nothing this command may edit.
		return false, nil
	}

	if len(deploy.Spec.Template.Spec.Containers) == 0 {
		return false, nil
	}

	container := &deploy.Spec.Template.Spec.Containers[0]
	if len(container.Env) == 0 {
		return false, nil
	}

	managed := make(map[string]bool, len(names))
	for _, n := range names {
		managed[n] = true
	}

	var conflicts []string
	for _, e := range container.Env {
		if managed[e.Name] {
			conflicts = append(conflicts, e.Name)
		}
	}

	if len(conflicts) == 0 {
		return false, nil
	}

	fmt.Fprintf(os.Stderr, "\n  Warning: %s has direct env entries that override envFrom:\n", appName)
	for _, k := range conflicts {
		fmt.Fprintf(os.Stderr, "    - %s\n", k)
	}
	fmt.Fprintf(os.Stderr, "  These direct entries take precedence over anything set with 'kip %s env set' or 'kip %s secret set'.\n", kind, kind)
	fmt.Fprintf(os.Stderr, "  Removing them changes the pod template, so %s restarts.\n", appName)
	fmt.Fprintf(os.Stderr, "  Remove them so envFrom takes effect? [y/N] ")

	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() && strings.TrimSpace(strings.ToLower(scanner.Text())) == "y" {
		conflictSet := make(map[string]bool)
		for _, k := range conflicts {
			conflictSet[k] = true
		}

		var cleaned []corev1.EnvVar
		for _, e := range container.Env {
			if !conflictSet[e.Name] {
				cleaned = append(cleaned, e)
			}
		}
		container.Env = cleaned

		if _, err := clientset.AppsV1().Deployments(ns).Update(ctx, deploy, metav1.UpdateOptions{}); err != nil {
			return false, fmt.Errorf("removing direct env entries: %w", err)
		}
		fmt.Fprintf(os.Stderr, "  ✔  Removed %d direct env entries\n", len(conflicts))
		return true, nil
	}

	return false, nil
}

// runEnvList prints what was set rather than what it resolved to, so a
// templated value shows as ${DB_PASSWORD} and the password stays where it was
// put. `kip service credentials` is the command for looking a credential up.
func runEnvList(cmd *cobra.Command, args []string) error {
	appName := args[0]
	kind := workloadKind(cmd)

	ns, clientset, dyn, err := workloadClients(cmd, appName)
	if err != nil {
		return err
	}

	ctx := context.Background()

	env, _, err := readWorkloadEnv(ctx, dyn, clientset, kind, ns, appName)
	if err != nil {
		return err
	}

	if len(env) == 0 {
		fmt.Printf("\n  No environment variables configured for %s\n\n", appName)
		return nil
	}

	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	fmt.Printf("\n  %-30s %s\n", "KEY", "VALUE")
	for _, key := range keys {
		fmt.Printf("  %-30s %s\n", key, env[key])
	}
	fmt.Println()

	return nil
}

func runEnvDelete(cmd *cobra.Command, args []string) error {
	appName := args[0]
	key := args[1]
	kind := workloadKind(cmd)

	ns, clientset, dyn, err := workloadClients(cmd, appName)
	if err != nil {
		return err
	}

	ctx := context.Background()

	env, home, err := readWorkloadEnv(ctx, dyn, clientset, kind, ns, appName)
	if err != nil {
		return err
	}
	if len(env) == 0 {
		return fmt.Errorf("no environment variables configured for %s", appName)
	}
	if _, ok := env[key]; !ok {
		return fmt.Errorf("env var %q not found for %s", key, appName)
	}
	delete(env, key)

	if err := writeWorkloadEnv(ctx, dyn, clientset, kind, ns, appName, env, home); err != nil {
		return err
	}

	fmt.Printf("  ✔  Environment variable %q deleted from %s\n", key, appName)
	return applyConfigChange(cmd, ctx, clientset, dyn, kind, ns, appName)
}

// envHome says where a workload's environment lives, and for the shape that
// lives in a Secret, which one.
//
// The name is resolved once, when the environment is read, and carried to the
// write. Looking it up a second time would let a Deployment edited in between
// send the write to a different object than the one that was read.
type envHome struct {
	// secret is empty for the ordinary shape: spec.env on the workload's CR,
	// which its reconciler renders into the Secret its pods read.
	secret string
}

// envOnCR is the ordinary shape.
var envOnCR = envHome{}

// envOnSecretNamed is a promoted app. `kip app promote` builds a Deployment
// with no CR behind it, so nothing renders that Secret and the Secret is where
// its environment lives.
func envOnSecretNamed(secret string) envHome { return envHome{secret: secret} }

// readWorkloadEnv returns the environment an operator set, and where it lives.
//
// spec.env is what was written and the Secret is what the reconciler made of
// it. Those stopped being the same thing when values became templates: the
// Secret holds `postgres://kipper:hunter2@db/app` where the CR holds
// `postgres://${DB_USERNAME}:${DB_PASSWORD}@${DB_HOST}/app`. Reading the Secret
// back would print the password, and writing what it read would store the
// password on the CR in place of the template — where `kip export` copies it.
//
// So the Secret is read only for the one shape nothing renders, identified by
// its Deployment rather than by the CR's absence: an ordinary app's Deployment
// and Secret outlive its CR by however long garbage collection takes, and
// "the CR is gone" would print that app's resolved credentials.
func readWorkloadEnv(ctx context.Context, dyn dynamic.Interface, clientset kubernetes.Interface, kind secretname.Kind, ns, name string) (map[string]string, envHome, error) {
	workload, err := dyn.Resource(workloadGVR(kind)).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		env, _, err := unstructured.NestedStringMap(workload.Object, "spec", "env")
		if err != nil {
			return nil, envOnCR, fmt.Errorf("reading %s env: %w", kind, err)
		}
		if env == nil {
			env = map[string]string{}
		}
		return env, envOnCR, nil
	}
	if !errors.IsNotFound(err) {
		return nil, envOnCR, fmt.Errorf("getting %s: %w", kind, err)
	}

	promoted := promotedDeployment(ctx, clientset, kind, ns, name)
	if promoted == nil {
		return nil, envOnCR, fmt.Errorf("%s %q not found in namespace %s", kind, name, ns)
	}

	home := envOnSecretNamed(envSecretOf(promoted, kind, name))
	secret, secretErr := clientset.CoreV1().Secrets(ns).Get(ctx, home.secret, metav1.GetOptions{})
	if secretErr != nil {
		if errors.IsNotFound(secretErr) {
			return map[string]string{}, home, nil
		}
		// A transient apiserver error read as "no env" would be written back as
		// an empty map, wiping every variable the workload has.
		return nil, home, fmt.Errorf("reading env vars: %w", secretErr)
	}
	env := make(map[string]string, len(secret.Data))
	for k, v := range secret.Data {
		env[k] = string(v)
	}
	return env, home, nil
}

// writeWorkloadEnv stores the environment where the workload's reconciler reads
// it from. The Secret is left alone: it is the render's output, and the next
// reconcile rebuilds it from what this wrote.
func writeWorkloadEnv(ctx context.Context, dyn dynamic.Interface, clientset kubernetes.Interface, kind secretname.Kind, ns, name string, env map[string]string, home envHome) error {
	if home.secret != "" {
		warnUnrenderedTemplates(env)
		return saveEnvSecret(ctx, clientset, ns, name, home.secret, env)
	}

	gvr := workloadGVR(kind)
	// Retry on conflict: the reconciler bumps the CR's resourceVersion between
	// our Get and Update, so re-fetch and re-apply rather than fail the command.
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		workload, err := dyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("getting %s: %w", kind, err)
		}

		if len(env) == 0 {
			unstructured.RemoveNestedField(workload.Object, "spec", "env")
		} else {
			values := make(map[string]interface{}, len(env))
			for k, v := range env {
				values[k] = v
			}
			if err := unstructured.SetNestedMap(workload.Object, values, "spec", "env"); err != nil {
				return fmt.Errorf("setting spec.env: %w", err)
			}
		}

		if _, err := dyn.Resource(gvr).Namespace(ns).Update(ctx, workload, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("updating %s env: %w", kind, err)
		}
		return nil
	})
}

// saveEnvSecret writes the env Secret directly, for a workload with no CR to
// render one from.
//
// The name comes from the Deployment rather than from the current convention.
// A promoted app built before the env Secret was kind-qualified still names
// `<app>-env`, so writing `app-<app>-env` would create an object beside the one
// its pods read, restart them, and report success while nothing changed.
func saveEnvSecret(ctx context.Context, clientset kubernetes.Interface, ns, appName, name string, env map[string]string) error {
	data := make(map[string][]byte, len(env))
	for k, v := range env {
		data[k] = []byte(v)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				"app":                          appName,
				"app.kubernetes.io/managed-by": "kipper",
			},
		},
		Data: data,
	}

	_, err := clientset.CoreV1().Secrets(ns).Create(ctx, secret, metav1.CreateOptions{})
	if !errors.IsAlreadyExists(err) {
		return err
	}

	// Update in place rather than replacing the object: a fresh Secret carries
	// no ownerReferences, so a blind Update would detach the App's controller
	// reference and leave the Secret (plaintext env) behind when the app is
	// deleted. Re-fetch under RetryOnConflict and change only Data.
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		live, err := clientset.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		// The name came from a pod template or from convention, neither of which
		// proves the object under it is Kipper's. Replacing the data of a Secret
		// somebody else created would destroy it, so ownership is checked before
		// the write rather than assumed by the name — the same rule the
		// reconcilers apply to every Secret they did not create.
		if live.Labels["app.kubernetes.io/managed-by"] != "kipper" {
			return fmt.Errorf("secret %q in %s was not created by Kipper; refusing to overwrite it", name, ns)
		}
		live.Data = data
		_, err = clientset.CoreV1().Secrets(ns).Update(ctx, live, metav1.UpdateOptions{})
		return err
	})
}

// envSecretOf is the env Secret a Deployment's container reads. This shape has
// no reconciler to bring a stale reference up to date, so a promoted Deployment
// built before the name carried the workload kind still points at `<app>-env`
// and writing the current name would leave its pods reading the old object.
//
// Only the two names Kipper has ever written are accepted — the current one and
// the one it replaced — because any other `*-env` in that list belongs to
// somebody else, and this decides what gets overwritten. Anything else falls
// back to the current name, which at worst creates a Secret nothing reads.
func envSecretOf(deploy *appsv1.Deployment, kind secretname.Kind, name string) string {
	expected := secretname.Env(kind, name)
	if deploy == nil || len(deploy.Spec.Template.Spec.Containers) == 0 {
		return expected
	}
	legacy := name + "-env"
	for _, from := range deploy.Spec.Template.Spec.Containers[0].EnvFrom {
		if from.SecretRef == nil {
			continue
		}
		switch from.SecretRef.Name {
		case expected:
			return expected
		case legacy:
			return legacy
		}
	}
	return expected
}

// warnUnrenderedTemplates says so when a value carries a ${NAME} reference on a
// workload nothing renders. The reference reaches the process exactly as
// written, which is a broken connection string discovered at runtime rather
// than here.
func warnUnrenderedTemplates(env map[string]string) {
	var referencing []string
	for _, k := range sortedEnvKeys(env) {
		if len(envtemplate.Names(env[k])) > 0 {
			referencing = append(referencing, k)
		}
	}
	if len(referencing) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "\n  Warning: %s contains a ${...} reference, and this app has no Kipper resource to resolve it.\n", strings.Join(referencing, ", "))
	fmt.Fprintf(os.Stderr, "  The value reaches the process exactly as written. Apps created by 'kip app deploy' or 'kip apply' resolve references; one built by 'kip app promote' does not.\n")
}

func sortedEnvKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
