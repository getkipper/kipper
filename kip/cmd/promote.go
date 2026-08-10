package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"

	"github.com/getkipper/kipper/kip/internal/deployer"
)

var promoteCmd = &cobra.Command{
	Use:   "promote [app-name]",
	Short: "Promote an app from one environment to the next",
	Long: `Copies the image tag from the source environment to the target
environment. Only the image is promoted — secrets and environment
variables are environment-specific and not copied.

The app must already exist in the target environment. Create it there
first with kip app deploy.

Use --all to promote all apps in the source environment at once.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runPromote,
}

func init() {
	promoteCmd.Flags().String("from", "", "source environment (e.g. test)")
	promoteCmd.Flags().String("to", "", "target environment (e.g. acc)")
	promoteCmd.Flags().String("project", "", "project name")
	promoteCmd.Flags().Bool("all", false, "promote all apps in the environment")

	_ = promoteCmd.MarkFlagRequired("from")
	_ = promoteCmd.MarkFlagRequired("to")
	_ = promoteCmd.MarkFlagRequired("project")

	appCmd.AddCommand(promoteCmd)
}

func runPromote(cmd *cobra.Command, args []string) error {
	from, _ := cmd.Flags().GetString("from")
	to, _ := cmd.Flags().GetString("to")
	project, _ := cmd.Flags().GetString("project")
	all, _ := cmd.Flags().GetBool("all")

	if !all && len(args) == 0 {
		return fmt.Errorf("provide an app name or use --all to promote all apps")
	}

	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	dyn := k8sClient.Dynamic()

	fromNs := cluster.ResolveNamespace(project, from)
	toNs := cluster.ResolveNamespace(project, to)

	clientset := k8sClient.Clientset()
	if _, err := clientset.CoreV1().Namespaces().Get(ctx, fromNs, metav1.GetOptions{}); err != nil {
		return fmt.Errorf("source environment %q not found (namespace %s)", from, fromNs)
	}
	if _, err := clientset.CoreV1().Namespaces().Get(ctx, toNs, metav1.GetOptions{}); err != nil {
		return fmt.Errorf("target environment %q not found (namespace %s)", to, toNs)
	}

	var appNames []string
	if all {
		apps, err := dyn.Resource(deployer.AppGVR).Namespace(fromNs).List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("listing apps in %s: %w", fromNs, err)
		}
		for _, a := range apps.Items {
			appNames = append(appNames, a.GetName())
		}
		if len(appNames) == 0 {
			return fmt.Errorf("no apps found in %s environment", from)
		}
	} else {
		appNames = []string{args[0]}
	}

	fmt.Printf("\n  Promote %s → %s (%s)\n", from, to, project)
	fmt.Printf("  Apps: %s\n\n", strings.Join(appNames, ", "))
	if !confirmPrompt(fmt.Sprintf("Promote to %s?", to)) {
		fmt.Println("  Cancelled.")
		return nil
	}

	fmt.Println()

	promoted := 0
	for _, appName := range appNames {
		if err := promoteApp(ctx, dyn, fromNs, toNs, appName, from, to); err != nil {
			fmt.Printf("  ✗  %s: %v\n", appName, err)
			continue
		}
		promoted++
	}

	fmt.Printf("\n  Done: %d of %d promoted\n\n", promoted, len(appNames))
	if promoted < len(appNames) {
		return fmt.Errorf("%d of %d apps were not promoted", len(appNames)-promoted, len(appNames))
	}
	return nil
}

// promoteApp copies one app's image from the source environment to the target,
// and reports what the cluster holds afterwards rather than that the call
// returned.
//
// The image is read from, and written to, the App CR. That is the desired
// state: the reconciler builds the Deployment's pod template from
// spec.image every time it runs, so a Deployment patched directly is put back
// within milliseconds and the promotion leaves nothing behind. Doing exactly
// that is what let this command print a tick, twice, against a spec.image that
// never moved.
func promoteApp(ctx context.Context, dyn dynamic.Interface, fromNs, toNs, appName, from, to string) error {
	source, err := dyn.Resource(deployer.AppGVR).Namespace(fromNs).Get(ctx, appName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return fmt.Errorf("not found in %s", from)
	}
	if err != nil {
		return fmt.Errorf("reading it in %s: %w", from, err)
	}
	image, found, _ := unstructured.NestedString(source.Object, "spec", "image")
	if !found || image == "" {
		return fmt.Errorf("has no image in %s yet; a git app has none until its first build finishes", from)
	}

	stored, err := setAppImage(ctx, dyn, toNs, appName, image, from, to)
	if err != nil {
		return err
	}
	fmt.Printf("  ✔  %s → %s (%s)\n", appName, to, stored)
	return nil
}

// setAppImage writes the image and returns what the cluster stored, so the
// caller reports the promotion it can see rather than the request it sent.
//
// The retry is not decoration: the reconciler writes status and finalizers onto
// the same object, so a plain update loses to it often enough to matter, and a
// lost update here is a promotion that silently did not happen.
func setAppImage(ctx context.Context, dyn dynamic.Interface, namespace, appName, image, from, to string) (string, error) {
	var stored string
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		app, err := dyn.Resource(deployer.AppGVR).Namespace(namespace).Get(ctx, appName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		// A git app's image is build output the controller owns, so writing one
		// here is undone by the next build. Promoting to it is a request this
		// cannot carry out rather than one it should do quietly.
		if _, isGit, _ := unstructured.NestedMap(app.Object, "spec", "git"); isGit {
			return errBuildsFromGit
		}
		// Checked on every read, like the git decision: an app waiting on its
		// finalizer accepts the write and then goes, so reporting the promotion
		// would be the same misleading tick in a different place.
		if app.GetDeletionTimestamp() != nil {
			return errBeingDeleted
		}
		if err := unstructured.SetNestedField(app.Object, image, "spec", "image"); err != nil {
			return err
		}
		if app.GetAnnotations() == nil {
			app.SetAnnotations(map[string]string{})
		}
		annotations := app.GetAnnotations()
		annotations["kipper.run/promoted-from"] = from
		annotations["kipper.run/promoted-at"] = time.Now().UTC().Format(time.RFC3339)
		app.SetAnnotations(annotations)

		updated, err := dyn.Resource(deployer.AppGVR).Namespace(namespace).Update(ctx, app, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		stored, _, _ = unstructured.NestedString(updated.Object, "spec", "image")
		return nil
	})
	switch {
	case errors.IsNotFound(err):
		return "", fmt.Errorf("not found in %s; create it there first with 'kip app deploy'", to)
	case err == errBuildsFromGit:
		return "", fmt.Errorf("builds from git in %s, so its image is build output rather than something to promote", to)
	case err == errBeingDeleted:
		return "", fmt.Errorf("is being deleted in %s", to)
	case err != nil:
		return "", fmt.Errorf("writing the image in %s: %w", to, err)
	}
	if stored != image {
		return "", fmt.Errorf("the cluster stored %q rather than %q", stored, image)
	}
	return stored, nil
}

// errBuildsFromGit and errBeingDeleted stop the retry loop without being
// mistaken for a conflict.
var (
	errBuildsFromGit = fmt.Errorf("app builds from git")
	errBeingDeleted  = fmt.Errorf("app is being deleted")
)
