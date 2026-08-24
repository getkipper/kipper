package cmd

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/sharedcred"
	"github.com/getkipper/kipper/kip/internal/deployer"
)

// seedSharedCredentialGrants writes the credential/project pairs the operator
// approved onto the entries that still have no allow-list.
//
// A shared credential names the projects that may build with it, and one with no
// projects named allows none. Credentials written before that list existed have
// none, so upgrading such a cluster stops every build that used one, and the
// operator meets it as a refused rebuild rather than as anything the upgrade
// said. What the cluster is already doing is written down here instead — but
// only what the operator consented to, and only for entries nobody has decided.
//
// Only a list nobody has decided is filled. A list somebody has curated is their
// decision and an upgrade does not get to widen it.
//
// It runs once per cluster, recorded on the kipper-system namespace by
// recordGrantsSeeded once the upgrade has replaced the writer that made the
// seeding necessary. Per-entry the question cannot be answered
// honestly for long: while the old writer is still serving, what it creates
// carries no list either, so a later upgrade would read an app's reference to it
// as a grant nobody made.
//
// Seeding itself is idempotent and safe to run more than once in an upgrade,
// which is what repairs a grant the old writer erased while it was still up.
//
// approved is the snapshot captured by credentialSeedConsent before the
// rollout. nil means consent was declined; nothing is filled. A non-nil map,
// even empty, is a permission to proceed: the empty case is the auto-close
// path where nothing was referenced at consent time.
func seedSharedCredentialGrants(ctx context.Context, clientset kubernetes.Interface, out io.Writer, approved map[string][]string) error {
	if approved == nil {
		return nil
	}
	return fillSharedCredentialGrants(ctx, clientset, out, approved, false)
}

// closeSharedCredentialGrants fills the approved grants a second time, then
// decides everything still undecided, which ends the migration for this
// cluster.
//
// Only this pass decides, and only when the console-api serving the cluster is
// one that keeps an allow-list. Deciding is what Seed will not revisit, so
// deciding against a console-api that still allows every build freezes a
// snapshot of a cluster that is still changing: an app pointed at an undecided
// credential the next day was building perfectly well, and would be refused by
// the upgrade that finally does replace the writer.
//
// Without that evidence the pass still fills, since repairing a grant the old
// writer erased is wanted either way, and says why it stopped there.
//
// approved is nil when consent was declined; the migration then stays open and
// reportConsentDeclined names what is still waiting and how to grant it. A
// non-nil map may still be empty, which is the auto-close path.
//
// Live app usage is not read here. What can be granted is exactly what the
// operator saw and approved at consent time: an app that arrived during the
// rollout is a reference under the new rules, and the plan this pass belongs
// to exists to stop a reference becoming a grant on its own.
func closeSharedCredentialGrants(ctx context.Context, clientset kubernetes.Interface, out io.Writer, approved map[string][]string) error {
	done, err := grantsAlreadySeeded(ctx, clientset)
	if err != nil {
		return err
	}
	if done {
		return reportClearedAllowLists(ctx, clientset, out)
	}
	if approved == nil {
		return reportConsentDeclined(ctx, clientset, out)
	}
	stamped, err := consoleAPIKeepsGrants(ctx, clientset)
	if err != nil {
		return err
	}
	if err := fillSharedCredentialGrants(ctx, clientset, out, approved, stamped); err != nil {
		return err
	}
	if !stamped {
		return reportMigrationLeftOpen(ctx, clientset, out)
	}
	return recordGrantsSeeded(ctx, clientset)
}

// fillSharedCredentialGrants writes approved into every entry that still has
// no allow-list, and — when closing — decides the rest as nobody.
//
// It never reads live app usage. Its job is to make the stored list match what
// was approved, and to close what stayed undecided when the writer that
// erased lists is gone.
func fillSharedCredentialGrants(ctx context.Context, clientset kubernetes.Interface, out io.Writer, approved map[string][]string, closing bool) error {
	done, err := grantsAlreadySeeded(ctx, clientset)
	if err != nil {
		return err
	}
	if done {
		return nil
	}

	stored, err := sharedcred.Load(ctx, clientset)
	if err != nil {
		return err
	}
	if len(stored) == 0 {
		return nil
	}

	var seeded []string
	if err := sharedcred.Update(ctx, clientset, func(entries []sharedcred.Entry) ([]sharedcred.Entry, error) {
		updated, filled, _ := sharedcred.Seed(entries, approved)
		seeded = filled
		if closing {
			updated, _ = sharedcred.CloseUndecided(updated)
		}
		return updated, nil
	}); err != nil {
		return fmt.Errorf("writing the approved shared credential grants: %w", err)
	}

	sort.Strings(seeded)
	for _, name := range seeded {
		projects := append([]string(nil), approved[name]...)
		sort.Strings(projects)
		_, _ = fmt.Fprintf(out, "  ✔  Shared credential %s now allows %s\n", name, strings.Join(projects, ", "))
	}
	return nil
}

// reportMissedGrants names every namespace that references a shared credential
// and whose project could not be proven from its project's own records.
//
// It is the only notice an operator gets. The allow-list is written once and
// no later upgrade revisits it, so a project left off stays off, and without
// this the first evidence is a build refused long after anybody connects it
// to an upgrade. What causes it is a namespace the project's own records do
// not cover, which on a cluster whose reconciler was refusing that namespace
// is exactly the one that matters.
//
// The consent wrapper prints this: it is the one place that reads live app
// usage, and the fill passes work from the approved snapshot alone.
func reportMissedGrants(out io.Writer, missed []missedGrant) {
	for _, m := range missed {
		_, _ = fmt.Fprintf(out, "  !   %s references shared credential %s, and project %s was not previewed for a grant:\n"+
			"      no record proves that namespace is theirs, and a label alone cannot decide a\n"+
			"      standing grant to somebody else's credential. If it is theirs, run\n"+
			"      kip credentials allow %s --project %s\n",
			m.namespace, m.credential, m.project, m.credential, m.project)
	}
}

// reportMigrationLeftOpen says which credentials are still waiting on a
// decision because the console-api that keeps allow-lists is not yet the one
// serving. Its wording is specific to that cause; the declined-consent case
// uses reportConsentDeclined.
//
// It stays quiet when nothing is undecided: an upgrade that grants nothing and
// decides nothing has nothing to report, and a warning about allow-lists reads
// oddly on a cluster that has none.
func reportMigrationLeftOpen(ctx context.Context, clientset kubernetes.Interface, out io.Writer) error {
	stored, err := sharedcred.Load(ctx, clientset)
	if err != nil {
		_, _ = fmt.Fprintf(out, "  !   Could not check which shared credentials are still undecided: %v\n", err)
		return nil
	}
	var undecided []string
	for _, entry := range stored {
		if entry.AllowedProjects == nil {
			undecided = append(undecided, entry.Name)
		}
	}
	if len(undecided) == 0 {
		return nil
	}
	_, _ = fmt.Fprintf(out, "  !   Still to decide who may build with %s: the console-api serving this\n"+
		"      cluster has not recorded its build, so this upgrade has not replaced the\n"+
		"      one that clears allow-lists. The next upgrade finishes it.\n",
		strings.Join(undecided, ", "))
	return nil
}

// reportConsentDeclined names the credentials that stay undecided because the
// operator declined consent, and repeats the two ways to grant later.
//
// It is separate from reportMigrationLeftOpen because the two states share a
// shape and nothing else: the console-api may be perfectly current, and the
// next upgrade will ask again rather than finish anything.
func reportConsentDeclined(ctx context.Context, clientset kubernetes.Interface, out io.Writer) error {
	stored, err := sharedcred.Load(ctx, clientset)
	if err != nil {
		_, _ = fmt.Fprintf(out, "  !   Could not check which shared credentials are still undecided: %v\n", err)
		return nil
	}
	var undecided []string
	for _, entry := range stored {
		if entry.AllowedProjects == nil {
			undecided = append(undecided, entry.Name)
		}
	}
	if len(undecided) == 0 {
		return nil
	}
	_, _ = fmt.Fprintf(out, "  !   Still to decide who may build with %s: consent was declined this run,\n"+
		"      so no reference was turned into a grant. Re-run 'kip upgrade\n"+
		"      --seed-credential-grants' to grant every referenced project, or\n"+
		"      'kip credentials allow <name> --project <project>' per pair.\n",
		strings.Join(undecided, ", "))
	return nil
}

// reportClearedAllowLists names a credential on a migrated cluster whose
// allow-list has gone back to nobody having decided.
//
// Every writer since the migration records a decision, so this state means an
// older console-api edited the credential or an older copy of the list was
// restored. Saying so is all this does. Filling it from the apps that reference
// it would be the fail-open half of the same coin: a project revoked while its
// app still names the credential is exactly this shape, and would be granted
// again by a repair.
func reportClearedAllowLists(ctx context.Context, clientset kubernetes.Interface, out io.Writer) error {
	stored, err := sharedcred.Load(ctx, clientset)
	if err != nil {
		// Advisory, and the migration is long finished, so a blip reading the
		// list is not worth the rest of the upgrade. Saying it was skipped
		// keeps that from passing as a clean bill of health.
		_, _ = fmt.Fprintf(out, "  !   Could not check the shared credential allow-lists: %v\n", err)
		return nil
	}
	var cleared []string
	for _, entry := range stored {
		if entry.AllowedProjects == nil {
			cleared = append(cleared, entry.Name)
		}
	}
	if len(cleared) == 0 {
		return nil
	}
	_, _ = fmt.Fprintf(out, "  !   Nobody may build with %s. The allowed projects are gone, which an\n"+
		"      older console-api does when a credential is edited. Grant them again with\n"+
		"      'kip credentials allow <name> --project <project>'.\n", strings.Join(cleared, ", "))
	return nil
}

// consoleAPIKeepsGrants reports whether the console-api serving this cluster now
// is one that leaves a shared credential's allow-list alone. It stamps its build
// on the namespace when it starts, and the release that stopped replacing the
// list is the first that does, so the stamp being there is the answer.
//
// A completed rollout does not answer it: the image is a moving tag, so an
// upgrade run before the release it belongs to is published pulls the old image
// and rolls it happily.
//
// The stamp is cleared before the rollout, which is what makes it evidence about
// the pod serving now rather than about one that served at some point. An
// operator who pins an older console-api back during an incident leaves a stamp
// that would otherwise still be vouching for a writer that has been gone for
// weeks.
func consoleAPIKeepsGrants(ctx context.Context, clientset kubernetes.Interface) (bool, error) {
	deadline := time.Now().Add(stampWait)
	for {
		ns, err := clientset.CoreV1().Namespaces().Get(ctx, kipperSystemNS, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("reading %s: %w", kipperSystemNS, err)
		}
		if ns.Annotations[labels.AnnoConsoleAPIBuild] != "" {
			return true, nil
		}
		// The rollout is complete when the pod is running, and the pod records
		// its build a moment after that, so a miss here is usually only early.
		if !time.Now().Before(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(stampPoll):
		}
	}
}

// stampWait is how long the upgrade waits for the console-api it has just rolled
// to record its build. Missing it only leaves the migration open for the next
// upgrade, so this is short. Both are variables so a test can drive the timeout
// without waiting it out.
var (
	stampWait = 30 * time.Second
	stampPoll = time.Second
)

// clearConsoleAPIStamp takes the previous build's stamp off, so that what the
// upgrade reads afterwards was written by the console-api it has just started
// rather than by whatever ran here before.
func clearConsoleAPIStamp(ctx context.Context, clientset kubernetes.Interface) error {
	namespaces := clientset.CoreV1().Namespaces()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		ns, getErr := namespaces.Get(ctx, kipperSystemNS, metav1.GetOptions{})
		if getErr != nil {
			return getErr
		}
		if _, stamped := ns.Annotations[labels.AnnoConsoleAPIBuild]; !stamped {
			return nil
		}
		delete(ns.Annotations, labels.AnnoConsoleAPIBuild)
		_, updErr := namespaces.Update(ctx, ns, metav1.UpdateOptions{})
		return updErr
	})
	if err != nil {
		return fmt.Errorf("clearing the recorded console-api build: %w", err)
	}
	return nil
}

// grantsAlreadySeeded reports whether this cluster has had the migration run.
//
// The marker is on the namespace rather than on the credential list, so
// restoring an older copy of that list does not present entries written after
// the migration as predating it.
func grantsAlreadySeeded(ctx context.Context, clientset kubernetes.Interface) (bool, error) {
	ns, err := clientset.CoreV1().Namespaces().Get(ctx, kipperSystemNS, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", kipperSystemNS, err)
	}
	return ns.Annotations[labels.AnnoGitCredentialGrantsSeeded] != "", nil
}

func recordGrantsSeeded(ctx context.Context, clientset kubernetes.Interface) error {
	namespaces := clientset.CoreV1().Namespaces()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		ns, getErr := namespaces.Get(ctx, kipperSystemNS, metav1.GetOptions{})
		if getErr != nil {
			return getErr
		}
		if ns.Annotations == nil {
			ns.Annotations = map[string]string{}
		}
		ns.Annotations[labels.AnnoGitCredentialGrantsSeeded] = time.Now().UTC().Format(time.RFC3339)
		_, updErr := namespaces.Update(ctx, ns, metav1.UpdateOptions{})
		return updErr
	})
	if err != nil {
		return fmt.Errorf("recording that shared credential grants were seeded: %w", err)
	}
	return nil
}

// sharedCredentialUsage maps each shared credential to the projects whose apps
// reference it, and lists the namespaces whose ownership could not be proven.
//
// It reads the apps because the cluster this runs for has no other record: the
// grant it is reconstructing is exactly what was never written down. A
// reference is what can be observed; whether the app has ever built with the
// credential is not, so the caller is the consent wrapper, which describes the
// pairs it returns as references rather than as grants.
//
// Which project each namespace belongs to is decided from that project's own
// records rather than from the namespace's label. What this builds is written
// into a shared credential's allow-list and stays there, so a namespace pointed
// at a project by anyone who can write namespace metadata would hand that
// project a standing grant to build with another tenant's credential.
//
// The cost of that strictness, which is why the drop is reported rather than
// passed over in silence: this runs before the console-api that fixes the
// records has rolled, so on a cluster where the released reconciler has been
// refusing a namespace it is exactly that namespace whose record is missing.
// Its project is left off the preview, and an unreported miss arrives as a
// refused build months later with nothing connecting it to this.
//
// Only names on the shared list count, so an app's own credential cannot
// invent a grant nobody asked for.
func sharedCredentialUsage(ctx context.Context, clientset kubernetes.Interface, dyn dynamic.Interface, shared map[string]bool) (map[string][]string, []missedGrant, error) {
	namespaces, err := clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: labels.ManagedBy + "=" + labels.Kipper,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("listing project namespaces: %w", err)
	}
	held, err := readProjectRecords(ctx, dyn)
	if err != nil {
		return nil, nil, err
	}
	project := make(map[string]string, len(namespaces.Items))
	unproven := map[string]string{}
	for _, ns := range namespaces.Items {
		name := ns.Labels[labels.Project]
		if name == "" {
			continue
		}
		if !held[name].covers(ns.Name, string(ns.UID)) {
			unproven[ns.Name] = name
			continue
		}
		project[ns.Name] = name
	}

	apps, err := dyn.Resource(deployer.AppGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("listing apps: %w", err)
	}

	var missed []missedGrant
	usage := map[string][]string{}
	for i := range apps.Items {
		app := &apps.Items[i]
		owner := project[app.GetNamespace()]
		if owner == "" {
			// Named only where it costs somebody something: a namespace whose
			// ownership could not be proven and which references a shared
			// credential. Anything else on that list is a namespace nobody was
			// going to preview a grant for anyway, and printing it would bury
			// the one line that matters.
			if project, dropped := unproven[app.GetNamespace()]; dropped {
				if credential := appGitCredential(app); shared[credential] {
					missed = append(missed, missedGrant{
						namespace:  app.GetNamespace(),
						project:    project,
						credential: credential,
					})
				}
			}
			// A namespace outside any project cannot name one, and stopping
			// here would refuse to upgrade a cluster with a stray namespace.
			continue
		}
		// Membership of the list decides, in the order the builder decides it:
		// a name on the list is resolved as shared even where it happens to
		// look like the one an app of that name would generate for itself.
		if credential := appGitCredential(app); shared[credential] {
			if !contains(usage[credential], owner) {
				usage[credential] = append(usage[credential], owner)
			}
		}
	}
	sort.Slice(missed, func(i, j int) bool { return missed[i].namespace < missed[j].namespace })
	return usage, missed, nil
}

func appGitCredential(app *unstructured.Unstructured) string {
	credential, _, err := unstructured.NestedString(app.Object, "spec", "git", "credentialsSecret")
	if err != nil {
		return ""
	}
	return credential
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// missedGrant is a namespace referencing a shared credential whose project
// could not be proven from that project's own records.
type missedGrant struct {
	namespace  string
	project    string
	credential string
}
