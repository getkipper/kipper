package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/memberbinding"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

var projectBindingsCmd = &cobra.Command{
	Use:   "bindings",
	Short: "List Kipper role bindings that no project explains",
	Long: `Lists the RoleBindings Kipper's project membership writes that no live
project accounts for.

A binding outlives the thing that explains it. A project deleted while the
console-api was down leaves its grants behind in namespaces, and so does a
namespace that changed hands. Nothing else on the cluster reports them: they
carry a project's name in their own, and once that project is gone there is
nothing left to join them to.

It reads and changes nothing. What it prints is what to look at.`,
	RunE: runProjectBindings,
}

func runProjectBindings(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	return reportUnclaimedBindings(ctx, k8sClient.Clientset(), k8sClient.Dynamic(), os.Stdout)
}

// reportUnclaimedBindings finds managed bindings by name shape, including
// those with missing labels. Generated names must identify the namespace's
// current project; legacy names require a current namespace owner. A live
// project alone does not justify a binding in a namespace it no longer holds.
func reportUnclaimedBindings(ctx context.Context, clientset kubernetes.Interface, dyn dynamic.Interface, out io.Writer) error {
	bindings, err := clientset.RbacV1().RoleBindings("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing role bindings: %w", err)
	}
	projects, err := dyn.Resource(manifest.ProjectGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing projects to see which bindings they explain: %w", err)
	}
	// The live namespaces, for their UIDs. A project's records carry names, and
	// a name outlives the object that carried it: a namespace deleted and
	// recreated, or handed to another project, leaves the old record saying the
	// old owner still has it. The object is what settles that.
	namespaces, err := clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing namespaces to see who holds them: %w", err)
	}
	live := make(map[string]corev1.Namespace, len(namespaces.Items))
	for _, ns := range namespaces.Items {
		live[ns.Name] = ns
	}

	prefixes := map[string]*unstructured.Unstructured{}
	for i := range projects.Items {
		prefixes[memberbinding.Prefix(projects.Items[i].GetName())] = &projects.Items[i]
	}

	var unclaimed []string
	disputed := map[string]string{}
	for _, b := range bindings.Items {
		if !memberbinding.IsManaged(b.Name) {
			continue
		}
		ns, exists := live[b.Namespace]
		if exists && !ns.DeletionTimestamp.IsZero() {
			// The namespace is going. An ordinary project delete removes the
			// Project CR first and leaves its namespaces finalizing, so every
			// binding in them would read as belonging to nobody, and the
			// operator would be sent to delete what is already on its way out.
			continue
		}
		owner := ""
		if exists {
			var mismatch bool
			owner, mismatch = namespaceOwner(ns, projects.Items)
			if mismatch {
				disputed[b.Namespace] = owner
			}
		}
		if prefix, ok := memberbinding.ProjectPrefixOf(b.Name); ok {
			// The name says which project wrote it. That project still
			// existing does not explain the binding: a namespace that changed
			// hands, or an environment removed while cleanup was interrupted,
			// leaves the writer alive and the grant behind in a namespace it
			// no longer holds. So the writer has to still be the namespace's
			// owner, by the rule namespaceOwner states.
			if project, alive := prefixes[prefix]; alive && project.GetName() == owner {
				continue
			}
		} else if owner != "" {
			// A legacy name carries no project digest, so which project wrote
			// it cannot be read off the name. A project holding the namespace
			// is the only one that would have.
			continue
		}
		unclaimed = append(unclaimed, b.Namespace+"/"+b.Name)
	}

	if len(unclaimed) == 0 {
		_, _ = fmt.Fprintln(out, "  ✔  Every Kipper role binding belongs to a project that still holds its namespace")
		reportDisputedNamespaces(disputed, out)
		return nil
	}
	sort.Strings(unclaimed)
	_, _ = fmt.Fprintf(out, "  !   %d Kipper role binding(s) sit in a namespace no project that could have\n"+
		"      written them still holds:\n", len(unclaimed))
	for _, u := range unclaimed {
		_, _ = fmt.Fprintf(out, "      - %s\n", u)
	}
	_, _ = fmt.Fprintf(out, "      They still grant what they were written to grant. Delete one with\n"+
		"      'kubectl -n <namespace> delete rolebinding <name>' once you have\n"+
		"      established nothing wants it.\n")
	reportDisputedNamespaces(disputed, out)
	return nil
}

// reportDisputedNamespaces names the namespaces whose label and whose owner's
// records point at different objects. Their bindings are left off the unclaimed
// list because the gate still honours them in this release, so they are not
// orphans to delete. They are worth seeing: the reconciler republishes the
// claim at the object that is actually there, and release 2 stops reading the
// label, at which point a namespace still in this state loses its owner.
func reportDisputedNamespaces(disputed map[string]string, out io.Writer) {
	if len(disputed) == 0 {
		return
	}
	names := make([]string, 0, len(disputed))
	for ns := range disputed {
		names = append(names, ns)
	}
	sort.Strings(names)
	_, _ = fmt.Fprintf(out, "  !   %d namespace(s) are labelled for a project whose records name a\n"+
		"      different object of that name, which is what a namespace deleted and\n"+
		"      recreated outside Kipper leaves behind:\n", len(names))
	for _, ns := range names {
		_, _ = fmt.Fprintf(out, "      - %s (labelled %s)\n", ns, disputed[ns])
	}
	_, _ = fmt.Fprintf(out, "      The project still reaches them, and the reconciler republishes the\n"+
		"      claim at the object that is there. Check they have healed before\n"+
		"      upgrading past the release that stops reading the label.\n")
}

// namespaceOwner mirrors nsowner's compatibility behavior: an existing project
// named by the namespace label remains the owner, while a mismatched claim UID
// is reported separately. Keep aligned with the API resolver when its fallback
// changes; the cleanup-specific heldByProject rule answers a different question.
func namespaceOwner(ns corev1.Namespace, projects []unstructured.Unstructured) (owner string, claimNamesAnotherObject bool) {
	candidate := ns.Labels[labels.Project]
	if candidate == "" {
		return "", false
	}
	var project *unstructured.Unstructured
	for i := range projects {
		if projects[i].GetName() == candidate {
			project = &projects[i]
			break
		}
	}
	if project == nil {
		return "", false
	}
	claims := claimedObjects(project)
	claimed, named := claims[ns.Name]
	return candidate, named && claimed != string(ns.UID)
}
