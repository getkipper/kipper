package cmd

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
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
// approved onto the entries that still have no allow-list, before the rollout.
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
// grants carries what credentialSeedConsent decided before the rollout. Only
// its approved pairs are used here, and an empty set writes nothing, which is
// both the declined path and the one where nothing was referenced.
func seedSharedCredentialGrants(ctx context.Context, clientset kubernetes.Interface, out io.Writer, grants credentialGrants) error {
	if len(grants.approved) == 0 {
		return nil
	}
	// No repair record. Nothing can have erased a list between the consent read
	// and a call three statements later, and if anything did, pass two puts it
	// back. Withholding it here is what makes "pass one may fill but must never
	// decide" true with no argument: writing back a decided-empty list would be
	// deciding, against a writer that still allows every build.
	return fillSharedCredentialGrants(ctx, clientset, out, grants.approved, grants.boundTo, nil, false)
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
// grants.mayClose is false when consent was declined; the migration then stays
// open, the lists recorded before the rollout are still written back, and
// reportConsentDeclined names what is still waiting and how to grant it.
//
// Live app usage is not read here. What can be granted is exactly what the
// operator saw and approved at consent time: an app that arrived during the
// rollout is a reference under the new rules, and the plan this pass belongs
// to exists to stop a reference becoming a grant on its own.
func closeSharedCredentialGrants(ctx context.Context, clientset kubernetes.Interface, out io.Writer, grants credentialGrants) error {
	done, err := grantsAlreadySeeded(ctx, clientset)
	if err != nil {
		return err
	}
	if done {
		repairErasedAllowLists(ctx, clientset, out, grants.decided)
		return reportClearedAllowLists(ctx, clientset, out)
	}
	if !grants.mayClose {
		repairErasedAllowLists(ctx, clientset, out, grants.decided)
		return reportConsentDeclined(ctx, clientset, out)
	}
	stamped, err := consoleAPIKeepsGrants(ctx, clientset)
	if err != nil {
		return err
	}
	if !stamped {
		if err := fillSharedCredentialGrants(ctx, clientset, out, grants.approved, grants.boundTo, grants.decided, false); err != nil {
			return err
		}
		return reportMigrationLeftOpen(ctx, clientset, out)
	}
	// Deciding is permanent, so it waits until nothing can write after it. The
	// console-api this upgrade replaces writes the whole list from a Secret it
	// builds fresh, with no resourceVersion, so its write cannot conflict with
	// anything: a request still running inside a terminating pod lands on top
	// of this pass and recording the migration over it would seal the loss.
	if err := waitForConsoleAPIQuiescence(ctx, clientset); err != nil {
		_, _ = fmt.Fprintf(out, "  !   %v\n"+
			"      The migration stays open, and the next upgrade finishes it.\n", err)
		return fillSharedCredentialGrants(ctx, clientset, out, grants.approved, grants.boundTo, grants.decided, false)
	}
	if err := fillSharedCredentialGrants(ctx, clientset, out, grants.approved, grants.boundTo, grants.decided, true); err != nil {
		return err
	}
	// A closing fill decides every list, so one still absent means the write did
	// not survive. Only the writer being replaced produces an absent list; a
	// concurrent kip credentials revoke writes an empty one.
	stillOpen, err := credentialsStillUndecided(ctx, clientset)
	if err != nil {
		_, _ = fmt.Fprintf(out, "  !   Could not confirm the shared credential allow-lists were written: %v\n"+
			"      The migration stays open, and the next upgrade finishes it.\n", err)
		return nil
	}
	if len(stillOpen) > 0 {
		_, _ = fmt.Fprintf(out, "  !   Nobody may build with %s, and this upgrade could not decide them: something\n"+
			"      cleared the list after it was written. The migration stays open, and the next\n"+
			"      upgrade finishes it.\n", strings.Join(stillOpen, ", "))
		return nil
	}
	return recordGrantsSeeded(ctx, clientset)
}

// repairErasedAllowLists writes back the allow-lists this run recorded before
// the rollout, on the paths that grant nothing and decide nothing.
//
// Both are advisory ends to an upgrade that has already done its work, so a
// failure is printed rather than returned: an operator whose cluster is
// otherwise upgraded should not meet this as a failed command.
func repairErasedAllowLists(ctx context.Context, clientset kubernetes.Interface, out io.Writer, decided map[string]sharedcred.Decision) {
	if len(decided) == 0 {
		return
	}
	if err := fillSharedCredentialGrants(ctx, clientset, out, nil, nil, decided, false); err != nil {
		_, _ = fmt.Fprintf(out, "  !   Could not write back the shared credential allow-lists: %v\n", err)
	}
}

// credentialsStillUndecided names the shared credentials whose allow-list is
// absent: nobody has decided who may build with them.
func credentialsStillUndecided(ctx context.Context, clientset kubernetes.Interface) ([]string, error) {
	stored, err := sharedcred.Load(ctx, clientset)
	if err != nil {
		return nil, err
	}
	var undecided []string
	for _, entry := range stored {
		if entry.AllowedProjects == nil {
			undecided = append(undecided, entry.Name)
		}
	}
	return undecided, nil
}

// fillSharedCredentialGrants writes back what was already stored and what the
// operator approved, and — when closing — decides the rest as nobody.
//
// It never reads live app usage. Its job is to make the stored list match what
// was recorded and approved before the rollout, and to close what stayed
// undecided once the writer that erases lists is gone.
//
// Restore runs before Seed, and both before CloseUndecided, inside one mutation:
// so a list this pass repairs can never be decided as nobody by the same write,
// and a repair can never be overwritten by an inference.
func fillSharedCredentialGrants(
	ctx context.Context,
	clientset kubernetes.Interface,
	out io.Writer,
	approved map[string][]string,
	boundTo map[string]string,
	decided map[string]sharedcred.Decision,
	closing bool,
) error {
	done, err := grantsAlreadySeeded(ctx, clientset)
	if err != nil {
		return err
	}
	if done {
		// A migrated cluster is never seeded again and never re-closed. A list
		// erased ten seconds ago is still this upgrade's to put back, so the
		// repair is left armed.
		approved, closing = nil, false
	}

	stored, err := sharedcred.Load(ctx, clientset)
	if err != nil {
		return err
	}
	if len(stored) == 0 {
		return nil
	}
	approved, movedSinceApproved := approvedStillBoundWhereShown(stored, approved, boundTo)

	var seeded, restored, moved, replaced, closedUnseen []string
	if err := sharedcred.Update(ctx, clientset, func(entries []sharedcred.Entry) ([]sharedcred.Entry, error) {
		updated, back, gone, swapped := sharedcred.Restore(entries, decided)
		restored, moved, replaced = back, gone, swapped
		updated, filled, _ := sharedcred.Seed(updated, approved)
		seeded = filled
		if closing {
			// What is about to be decided as nobody without anybody having seen
			// it: a credential that did not exist when the operator was shown
			// this cluster, which the writer being replaced can create with no
			// allow-list at all. Deciding it is right, and deciding it in
			// silence is not: the marker goes on straight after, and no later
			// upgrade revisits a migrated cluster.
			closedUnseen = nil
			for i := range updated {
				if updated[i].AllowedProjects != nil {
					continue
				}
				if _, known := decided[updated[i].Name]; known {
					continue
				}
				closedUnseen = append(closedUnseen, updated[i].Name)
			}
			updated, _ = sharedcred.CloseUndecided(updated)
		}
		return updated, nil
	}); err != nil {
		return fmt.Errorf("writing the shared credential allow-lists: %w", err)
	}

	sort.Strings(seeded)
	for _, name := range seeded {
		projects := append([]string(nil), approved[name]...)
		sort.Strings(projects)
		_, _ = fmt.Fprintf(out, "  ✔  Shared credential %s now allows %s\n", name, strings.Join(projects, ", "))
	}
	reportRepairedAllowLists(out, decided, restored, moved, replaced)
	reportApprovedButMoved(out, movedSinceApproved)
	reportClosedUnseen(out, closedUnseen)
	return nil
}

// approvedStillBoundWhereShown drops an approved grant whose credential has
// been re-pointed at a different server since the operator saw it.
//
// What the preview showed was a credential at a host, and what a grant lets a
// project do is present that credential's token to that host. An entry
// re-pointed during the rollout is a different one to allow, so the approval
// does not carry over to it, exactly as a repair does not.
func approvedStillBoundWhereShown(stored []sharedcred.Entry, approved map[string][]string, boundTo map[string]string) (map[string][]string, []string) {
	if len(approved) == 0 {
		return approved, nil
	}
	kept := make(map[string][]string, len(approved))
	var moved []string
	for name, projects := range approved {
		shown, known := boundTo[name]
		entry := sharedcred.Find(stored, name)
		if known && entry != nil && entry.Server != shown {
			moved = append(moved, name)
			continue
		}
		kept[name] = projects
	}
	sort.Strings(moved)
	return kept, moved
}

// reportApprovedButMoved names a credential the operator approved that has
// since been re-pointed, so nothing lands silently short of what was agreed.
func reportApprovedButMoved(out io.Writer, moved []string) {
	for _, name := range moved {
		_, _ = fmt.Fprintf(out, "  !   Shared credential %s was approved for a grant, then re-pointed at a\n"+
			"      different server before it could be written, so nothing was granted. Allow the\n"+
			"      projects again with 'kip credentials allow %s --project <project>' if it is\n"+
			"      still the credential they should build with.\n", name, name)
	}
}

// reportClosedUnseen names every credential this pass decided as nobody without
// the operator ever having been shown it.
//
// The writer being replaced can create a credential during the rollout, and one
// it creates carries no allow-list, so the closing pass decides it. That is the
// right way round — a credential nobody has been asked about grants nobody —
// but the migration is recorded straight afterwards and no later upgrade
// revisits a migrated cluster, so unnamed it arrives as a build refused months
// later with nothing connecting it to this.
func reportClosedUnseen(out io.Writer, closed []string) {
	if len(closed) == 0 {
		return
	}
	sort.Strings(closed)
	_, _ = fmt.Fprintf(out, "  !   Nobody may build with %s. It was added while this upgrade was running, so\n"+
		"      it was never shown for a decision and this upgrade decided it as nobody. Allow\n"+
		"      a project with 'kip credentials allow <name> --project <project>'.\n",
		strings.Join(closed, ", "))
}

// reportRepairedAllowLists names every allow-list written back, and every one
// refused.
//
// Restoring a list that was decided as nobody says nothing: nobody could build
// before and nobody can now. The rest all cost somebody something, and the
// allow-list is written once and no later upgrade revisits it, so without this
// the first evidence is a build refused long after anybody connects it to an
// upgrade.
func reportRepairedAllowLists(out io.Writer, decided map[string]sharedcred.Decision, restored, moved, replaced []string) {
	sort.Strings(restored)
	sort.Strings(moved)
	sort.Strings(replaced)
	for _, name := range restored {
		if projects := allowedIn(decided, name); projects != "" {
			_, _ = fmt.Fprintf(out, "  ✔  Shared credential %s allows %s again. Its allowed projects were taken\n"+
				"      off while this upgrade was running, which the console-api it replaces does\n"+
				"      when a credential is edited. If you revoked one of them during the upgrade,\n"+
				"      revoke it again.\n", name, projects)
		}
	}
	for _, name := range moved {
		_, _ = fmt.Fprintf(out, "  !   Shared credential %s is bound to a different server than when this\n"+
			"      upgrade started, so its allowed projects were not written back. It was bound\n"+
			"      to %s. A grant lets a project use one token against one host.\n",
			name, decided[name].Server)
		printRegrantCommands(out, decided, name)
	}
	for _, name := range replaced {
		_, _ = fmt.Fprintf(out, "  !   Shared credential %s carries a different token than when this upgrade\n"+
			"      started, so its allowed projects were not written back. Rotating a token keeps\n"+
			"      them, but a credential deleted and recreated under the same name is a different\n"+
			"      credential, and nothing here can tell the two apart.\n", name)
		printRegrantCommands(out, decided, name)
	}
}

// printRegrantCommands writes the exact command per project, rather than one
// line with a placeholder in it.
//
// What was lost is a list somebody curated, and the operator reading this is
// usually the person who just edited the credential. Making them assemble the
// commands from a placeholder is where a grant gets left off and turns up as a
// refused build months later.
func printRegrantCommands(out io.Writer, decided map[string]sharedcred.Decision, name string) {
	projects := append([]string(nil), decided[name].AllowedProjects...)
	sort.Strings(projects)
	if len(projects) == 0 {
		_, _ = fmt.Fprintf(out, "      It allowed nobody, so there is nothing to put back.\n")
		return
	}
	_, _ = fmt.Fprintf(out, "      It allowed %s. If that is still right, put it back with:\n", strings.Join(projects, ", "))
	for _, project := range projects {
		_, _ = fmt.Fprintf(out, "        kip credentials allow %s --project %s\n", name, project)
	}
}

func allowedIn(decided map[string]sharedcred.Decision, name string) string {
	projects := append([]string(nil), decided[name].AllowedProjects...)
	sort.Strings(projects)
	return strings.Join(projects, ", ")
}

// reportMissedGrants names every namespace// reportMissedGrants names every namespace that references a shared credential
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

// quiescenceWait bounds the wait for the console-api being replaced to stop.
// Overridden in tests, as stampWait is.
var (
	quiescenceWait = 90 * time.Second
	quiescencePoll = 2 * time.Second
)

// waitForConsoleAPIQuiescence waits until no console-api pod from before the
// rollout can still write.
//
// A completed rollout does not prove this. rollout.Ready compares updated and
// available replicas against the desired count, and a pod that has been sent
// SIGTERM stops being available long before it stops running: it keeps serving
// whatever request it already had for the rest of its termination grace. The
// console-api it replaces writes the whole allow-list from a Secret it builds
// fresh, with no resourceVersion, so that late write conflicts with nothing and
// silently replaces everything this pass just decided.
//
// What it waits for is every pod under the Deployment's selector being one the
// Deployment is keeping: none deleted, and none from an earlier revision.
func waitForConsoleAPIQuiescence(ctx context.Context, clientset kubernetes.Interface) error {
	// The Deployment is what says which pods are the console-api's and how many
	// of them belong here, so without it there is nothing to ask. It is not
	// evidence that no writer is left: a Deployment can go before the pods it
	// owns do, and those pods keep serving until the garbage collector catches
	// up. The upgrade has just rolled this Deployment, so its absence is odd
	// rather than ordinary, and holding the migration open for one run is the
	// answer that cannot seal a loss.
	dep, err := clientset.AppsV1().Deployments(kipperSystemNS).Get(ctx, consoleAPIName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("could not tell whether the console-api being replaced has stopped: reading %s: %w", consoleAPIName, err)
	}
	updated := dep.Status.UpdatedReplicas
	selector := metav1.FormatLabelSelector(dep.Spec.Selector)
	deadline := time.Now().Add(quiescenceWait)
	for {
		pods, err := clientset.CoreV1().Pods(kipperSystemNS).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return fmt.Errorf("listing %s pods to wait for the one it replaces to stop: %w", consoleAPIName, err)
		}
		lingering := lingeringConsoleAPIPods(pods.Items, updated)
		if lingering == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d console-api pod(s) from before the upgrade were still running after %s, and one of them can still clear a shared credential's allowed projects",
				lingering, quiescenceWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(quiescencePoll):
		}
	}
}

// lingeringConsoleAPIPods counts the pods that are not the ones the Deployment
// means to keep: any pod on its way out, plus any surplus over the replica count
// the rollout reported as updated.
//
// Counting the surplus as well as the deletions is what catches a pod whose
// deletion has not been recorded yet, which is exactly the window this exists
// for.
func lingeringConsoleAPIPods(pods []corev1.Pod, updated int32) int {
	live := 0
	for i := range pods {
		if pods[i].DeletionTimestamp != nil {
			live++
			continue
		}
		if pods[i].Status.Phase == corev1.PodRunning || pods[i].Status.Phase == corev1.PodPending {
			live++
		}
	}
	if surplus := live - int(updated); surplus > 0 {
		return surplus
	}
	for i := range pods {
		if pods[i].DeletionTimestamp != nil {
			return 1
		}
	}
	return 0
}
