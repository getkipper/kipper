package cmd

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/controller/pkg/labels"
)

// reportNamespacesWithoutAClaim names every namespace labelled for a project
// whose records do not cover it.
//
// A namespace resolves to its project through the label today and through the
// project's own records in the release after this one. While both are true the
// difference is invisible; once only the records count, a namespace on this list
// is one whose members cannot reach it, whose builds lose their shared
// credentials and whose workloads lose their pull secrets. Printing it here is
// what turns "will the next upgrade lock anyone out" from a guess into a command
// somebody already ran.
//
// The records are the two the resolver reads, and the older one is why a cluster
// that never ran this upgrade is not reported as broken: a project carries the
// namespaces it took long before claims existed. Listing only the unclaimed ones
// would name every namespace on such a cluster and say its members were about to
// lose them, which is the opposite of true.
//
// It writes nothing, arms nothing and gates nothing, and returns nothing for
// the same reason: an upgrade must not fail over an advisory, and a function
// that can return an error is one a later edit will make fail. The console
// publishes the claims on its own; this only says whether it has.
//
// settle is how long to wait for the controller to publish claims before
// reporting. An upgrade can finish in seconds, and the first pass over every
// project has not run by then, so reporting immediately would name the whole
// cluster and teach the operator that the list means nothing.
func reportNamespacesWithoutAClaim(ctx context.Context, clientset kubernetes.Interface, dyn dynamic.Interface, out io.Writer, settle time.Duration) {
	deadline := time.Now().Add(settle)
	// What has already been named, so a later poll reports what it newly finds
	// rather than repeating itself or, worse, staying silent. A namespace can
	// become orphaned during the settle, which is the very interleaving this
	// list exists for, and one flag saying "an orphan was reported" swallows it.
	named := map[string]struct{}{}
	for {
		drifted, orphaned, err := namespacesWithoutAClaim(ctx, clientset, dyn)
		if err != nil {
			_, _ = fmt.Fprintf(out, "  !   Could not check which namespaces their projects have claimed: %v\n", err)
			return
		}
		// Reported at once rather than after the settle, because waiting is for
		// a record the controller has not written yet and no controller will
		// write this one.
		fresh := make([]string, 0, len(orphaned))
		for _, ns := range orphaned {
			if _, said := named[ns]; said {
				continue
			}
			named[ns] = struct{}{}
			fresh = append(fresh, ns)
		}
		fresh = stillOrphaned(ctx, clientset, fresh)
		if len(fresh) > 0 {
			_, _ = fmt.Fprintf(out, "  !   Labelled for a project that is gone: %s.\n"+
				"      Nothing collects these: the project that would have deleted them no\n"+
				"      longer exists, and no record names them. Check what is inside, then\n"+
				"      delete them with kubectl.\n",
				strings.Join(fresh, ", "))
		}
		if len(drifted) == 0 {
			return
		}
		if !time.Now().Before(deadline) {
			_, _ = fmt.Fprintf(out, "  !   Not yet recorded as their project's: %s.\n"+
				"      The console records what each project took as it reconciles, and a\n"+
				"      namespace still on this list when the next release lands is one its\n"+
				"      members cannot reach. Check the console-api logs for the projects named.\n",
				strings.Join(drifted, ", "))
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(claimsSettlePoll):
		}
	}
}

const claimsSettlePoll = 5 * time.Second

// claimsSettleWait is how long an upgrade waits for the console to record what
// each project holds before reporting what it has not. An upgrade can finish
// well inside one reconcile pass, and a report printed then names every
// namespace on the cluster.
const claimsSettleWait = 2 * time.Minute

// namespacesWithoutAClaim returns "<namespace> (project <name>)" for each
// namespace whose label names a project whose records do not cover it.
//
// It answers the resolver's question rather than a narrower one: a claim on this
// exact object, or the namespace in what the project recorded holding, and a
// claim naming the name at some other object rules the older record out. A
// namespace deleted and recreated is a different object, so a claim naming the
// one that is gone covers nothing and belongs on this list.
// The second list is namespaces whose labelled project is not on the cluster at
// all. Those are not waiting for anything: no pass will ever record them,
// because there is no project left to run one. A project deleted while another
// replica was mid-pass creating a namespace for it leaves exactly this, and so
// does a label pointed at a project that has since gone.
func namespacesWithoutAClaim(ctx context.Context, clientset kubernetes.Interface, dyn dynamic.Interface) ([]string, []string, error) {
	namespaces, err := clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: labels.Project,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("listing namespaces: %w", err)
	}
	if len(namespaces.Items) == 0 {
		return nil, nil, nil
	}

	held, err := readProjectRecords(ctx, dyn)
	if err != nil {
		return nil, nil, err
	}

	var drifted, orphaned []string
	for _, ns := range namespaces.Items {
		project := ns.Labels[labels.Project]
		if project == "" {
			continue
		}
		// A namespace that is already terminating is Kubernetes' to finish and
		// belongs on neither list. Saying nothing collects it sends an operator
		// to delete something that is going anyway; calling it unrecorded sends
		// them to the console-api logs for a namespace no pass will ever record,
		// once every five seconds until the settle runs out. The reconciler
		// deletes a project's namespaces and takes its finalizer off without
		// waiting for them to finish, so an ordinary project delete passes
		// through the first state and an ordinary environment removal, with a
		// namespace slow to finalize, through the second.
		if !ns.DeletionTimestamp.IsZero() {
			continue
		}
		records, exists := held[project]
		if !exists {
			orphaned = append(orphaned, fmt.Sprintf("%s (project %s, which does not exist)", ns.Name, project))
			continue
		}
		if records.covers(ns.Name, string(ns.UID)) {
			continue
		}
		drifted = append(drifted, fmt.Sprintf("%s (project %s)", ns.Name, project))
	}
	sort.Strings(drifted)
	sort.Strings(orphaned)
	return drifted, orphaned, nil
}

// claimedObjects reads a project's claims as namespace name to object UID.
//
// A claim missing either half is skipped rather than recorded as a claim on the
// empty string, which would match a namespace whose UID could not be read.
func claimedObjects(project *unstructured.Unstructured) map[string]string {
	raw, found, err := unstructured.NestedSlice(project.Object, "status", "namespaceClaims")
	if err != nil || !found {
		return nil
	}
	out := make(map[string]string, len(raw))
	for _, entry := range raw {
		claim, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		name, _ := claim["name"].(string)
		uid, _ := claim["uid"].(string)
		if name == "" || uid == "" {
			continue
		}
		out[name] = uid
	}
	return out
}

// projectRecords is what one project's status says it holds: the claims by
// namespace name, and the older name-only list.
type projectRecords struct {
	claims   map[string]string
	recorded []string
}

// covers is the resolver's rule, asked of one project's records.
//
// A claim on this exact object settles it. A claim naming the namespace at a
// different object settles it the other way, because the claim knows which
// object the project took and this is not it. With no claim naming it at all,
// the older record answers, which is every namespace on a cluster that has not
// yet run a pass under this release.
//
// The zero value covers nothing, so a project that does not exist owns nothing.
func (r projectRecords) covers(namespace, uid string) bool {
	claimed, named := r.claims[namespace]
	if named {
		return claimed == uid
	}
	return slices.Contains(r.recorded, namespace)
}

// readProjectRecords reads every project's records in one list.
//
// console-api answers the same question from one function and kip cannot call
// it, because it lives in that module's internal package. This reads the same
// two status fields.
func readProjectRecords(ctx context.Context, dyn dynamic.Interface) (map[string]projectRecords, error) {
	projects, err := dyn.Resource(projectGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing projects: %w", err)
	}
	held := make(map[string]projectRecords, len(projects.Items))
	for i := range projects.Items {
		recorded, _, _ := unstructured.NestedStringSlice(projects.Items[i].Object, "status", "namespaces")
		held[projects.Items[i].GetName()] = projectRecords{
			claims:   claimedObjects(&projects.Items[i]),
			recorded: recorded,
		}
	}
	return held, nil
}

// stillOrphaned drops any namespace that has started terminating since the list
// was read.
//
// The lists this report works from are two snapshots taken one after the other:
// namespaces first, projects second. The reconciler deletes a project's
// namespaces and only then takes its finalizer off, so a project delete landing
// between those two reads shows namespaces that were not yet terminating beside
// a project that has already gone, and they read as orphans. Kubernetes is
// collecting them. The window is two adjacent list calls wide, and the check
// costs one read per orphan on a list that is almost always empty.
func stillOrphaned(ctx context.Context, clientset kubernetes.Interface, orphaned []string) []string {
	if len(orphaned) == 0 {
		return orphaned
	}
	out := make([]string, 0, len(orphaned))
	for _, entry := range orphaned {
		name, _, _ := strings.Cut(entry, " ")
		ns, err := clientset.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			// Gone between the list and here, which is the outcome the operator
			// would have been asked to bring about.
			continue
		case err != nil:
			// A read that failed is not evidence either way, and the reason
			// this list exists is that nothing else reports these at all.
			out = append(out, entry)
		case !ns.DeletionTimestamp.IsZero():
			continue
		default:
			out = append(out, entry)
		}
	}
	return out
}
