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

// reportUnclaimedBindings names every membership binding no live project
// explains.
//
// Explaining one takes two things, not one. The project that wrote it has to
// still exist, and it has to still hold the namespace the binding sits in: a
// namespace that changed hands, or an environment removed while cleanup was
// interrupted, leaves the writer alive and the grant behind somewhere it no
// longer has any say. That is the case this exists for, so the project
// existing is not on its own an answer.
//
// It enumerates by shape and applies no label selector. The label is what
// drifts — it is the first thing an edit or a restore loses — and selecting on
// it would let exactly the bindings this exists to find hide from it.
//
// Two things can explain a binding. A generated name carries its project's
// digest, so the name alone attributes it even after every mutable trail back
// has gone. A legacy name carries no digest and is identical in every
// namespace, so the only thing that can explain one is the namespace it sits
// in being held by a project at all.
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

// namespaceOwner is the project the live namespace belongs to, as the
// console-api's own gate answers that question, and whether the project's
// records confirm it.
//
// It is written out here rather than reused, for the reason the credential
// seeding says the same thing: nsowner lives in console-api's internal
// packages and no other module can import it. What it must not do is borrow a
// helper whose contract is something else. heldByProject is the project delete
// path's rule, and that path feeds it namespaces it has already selected by
// label; handed every namespace on the cluster instead, it answers from a claim
// alone and calls a namespace held by a project the resolver would say has no
// claim on it at all.
//
// So: the label names a candidate, and the candidate has to exist. A claim
// naming this namespace at a different object means the project took a
// different object of that name, and the records and the label disagree. In
// release 1 the gate answers with the labelled project either way, so this
// does too, and returns the disagreement alongside. Reporting a binding the
// gate still honours as nobody's would send an operator to delete a grant that
// is working; the disagreement is real, and it is reported as itself.
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
