package cmd

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/controller/pkg/sharedcred"
)

// consentDecision is the outcome of asking whether the upgrade may fill the
// shared credential allow-lists nobody has decided.
type consentDecision int

const (
	// consentAuto is picked when nothing is referenced, so the fail-closed
	// close needs no permission. A cluster whose undecided credentials nothing
	// points at cannot be asked on every upgrade for ever.
	consentAuto consentDecision = iota
	// consentGrant is picked when the operator has said yes, or --seed-credential-grants
	// is set. Both passes fill exactly the previewed pairs and pass two records.
	consentGrant
	// consentDecline is picked when the answer is no, or there is no TTY and no
	// flag. Neither pass fills; pass two only reports what is still open.
	consentDecline
)

// decideCredentialSeedConsent picks a consentDecision from the credentials
// undecided-but-referenced usage, the --seed-credential-grants flag, and
// whether stdin is a terminal.
//
// It is deliberately free of I/O: confirm is called only in the ask case, and
// the table test supplies it as a stub. The wrapper credentialSeedConsent
// prints the preview and the decline hint outside this function.
func decideCredentialSeedConsent(usage map[string][]string, flag, isTTY bool, confirm func() (bool, error)) (consentDecision, error) {
	if len(usage) == 0 {
		return consentAuto, nil
	}
	if flag {
		return consentGrant, nil
	}
	if !isTTY {
		return consentDecline, nil
	}
	ok, err := confirm()
	if err != nil {
		return consentDecline, err
	}
	if ok {
		return consentGrant, nil
	}
	return consentDecline, nil
}

// credentialSeedConsent decides once for both passes what the upgrade may
// grant, and returns the snapshot the passes work from.
//
// The return convention distinguishes three states with one map:
//   - nil map: consent was declined. Neither pass fills; pass two reports why.
//   - non-nil empty map: nothing was referenced at consent time; the
//     fail-closed close needs no permission. Pass two decides everything still
//     undecided as nobody.
//   - non-nil non-empty map: the operator approved these credential/project
//     pairs. Pass one and pass two fill exactly these.
//
// A migrated cluster and a cluster with no undecided credentials both shortcut
// to a non-nil empty map with no prompt and nothing printed. Freezing the
// snapshot here is what stops the closing pass turning an app that arrived
// during the rollout window into a silent grant.
func credentialSeedConsent(
	ctx context.Context,
	clientset kubernetes.Interface,
	dyn dynamic.Interface,
	out io.Writer,
	flag, isTTY bool,
	confirm func() (bool, error),
) (map[string][]string, error) {
	empty := map[string][]string{}
	done, err := grantsAlreadySeeded(ctx, clientset)
	if err != nil {
		return nil, err
	}
	if done {
		return empty, nil
	}
	stored, err := sharedcred.Load(ctx, clientset)
	if err != nil {
		return nil, err
	}
	undecided := map[string]bool{}
	for _, e := range stored {
		if e.AllowedProjects == nil {
			undecided[e.Name] = true
		}
	}
	if len(undecided) == 0 {
		return empty, nil
	}
	usage, missed, err := sharedCredentialUsage(ctx, clientset, dyn, undecided)
	if err != nil {
		return nil, err
	}
	if len(usage) > 0 {
		printSeedConsentPreview(out, usage)
	}
	// Missed grants are printed regardless of the decision that follows, and
	// before it: they are the namespaces this migration will not cover no
	// matter how the operator answers, so seeing them before the yes/no is
	// what puts them into the same run rather than the next one.
	reportMissedGrants(out, missed)
	decision, err := decideCredentialSeedConsent(usage, flag, isTTY, confirm)
	if err != nil {
		return nil, err
	}
	switch decision {
	case consentDecline:
		printSeedConsentDeclineHint(out)
		return nil, nil
	case consentGrant:
		return usage, nil
	default:
		return empty, nil
	}
}

// printSeedConsentPreview lists every undecided credential that an app in a
// project references, in a deterministic order so the operator can tell what a
// scripted upgrade would grant. Projects under each credential are sorted too,
// since the app-list order is not stable and reordering the same reference is
// noise.
//
// It describes the projects as referencing the credential rather than as
// having been built with it: on a cluster where the guard has always been
// active, a reference could just as well have produced only refused builds,
// and framing an inference as evidence of success would tilt the operator
// towards approving things they did not mean to.
func printSeedConsentPreview(out io.Writer, usage map[string][]string) {
	names := make([]string, 0, len(usage))
	for name := range usage {
		names = append(names, name)
	}
	sort.Strings(names)
	_, _ = fmt.Fprintf(out, "  !   These shared git credentials have no allow-list, and apps in these\n"+
		"      projects reference them:\n")
	for _, name := range names {
		projects := append([]string(nil), usage[name]...)
		sort.Strings(projects)
		_, _ = fmt.Fprintf(out, "      - %s → %s\n", name, strings.Join(projects, ", "))
	}
	_, _ = fmt.Fprintf(out, "      The new console-api refuses a build whose credential does not list its\n"+
		"      project, so any of these that stays ungranted stops building on this upgrade.\n")
}

// printSeedConsentDeclineHint says how to grant later, on both the automation
// path and the per-project path. Named separately so the caller can print it
// after a declined prompt as well as after a non-interactive skip.
func printSeedConsentDeclineHint(out io.Writer) {
	_, _ = fmt.Fprintf(out, "  !   Nothing was granted. Re-run 'kip upgrade --seed-credential-grants' to\n"+
		"      grant every referenced project, or 'kip credentials allow <name>\n"+
		"      --project <project>' per pair.\n")
}
