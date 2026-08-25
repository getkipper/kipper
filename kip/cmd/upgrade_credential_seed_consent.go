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

// credentialGrants is what one upgrade run may write to the shared credential
// allow-lists, decided once before the rollout.
//
// Three independent facts travel, and none of them is encoded in whether a map
// is nil: what may be inferred from the apps, what may be repaired from what was
// already stored, and whether this run may end the migration. The zero value
// infers nothing, repairs nothing and closes nothing, so a caller that forgets a
// field fails closed.
type credentialGrants struct {
	// approved is the credential/project pairs the operator agreed to infer
	// from the apps referencing them. Empty both when consent was declined and
	// when nothing was referenced.
	approved map[string][]string
	// decided is the allow-list each credential already carried before the
	// rollout. Writing one back is a repair, so it is not gated on consent.
	decided map[string]sharedcred.Decision
	// mayClose is whether this run may decide the rest as nobody and record the
	// migration as finished.
	mayClose bool
}

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

// credentialSeedConsent decides once, before the rollout, what this upgrade may
// write to the shared credential allow-lists.
//
// It answers two separate questions from one read. What may be *inferred* is
// gated on the operator: an app naming a credential is a reference, and
// spec.git.credentialsSecret is a field a project member writes. What may be
// *repaired* is not gated on anybody: it is the list the cluster was already
// enforcing when the operator typed kip upgrade, and writing it back leaves no
// project holding anything it did not already hold.
//
// Freezing both here is what stops the closing pass turning an app that arrived
// during the rollout window into a grant nobody agreed to.
func credentialSeedConsent(
	ctx context.Context,
	clientset kubernetes.Interface,
	dyn dynamic.Interface,
	out io.Writer,
	flag, isTTY bool,
	confirm func() (bool, error),
) (credentialGrants, error) {
	done, err := grantsAlreadySeeded(ctx, clientset)
	if err != nil {
		return credentialGrants{}, err
	}
	stored, err := sharedcred.Load(ctx, clientset)
	if err != nil {
		if done {
			// A migrated cluster did not read this list at all before the
			// repair existed, so a blip reading it must not become a new way
			// for an upgrade to abort on a working cluster. Saying it was
			// skipped keeps that from passing as a clean bill of health.
			_, _ = fmt.Fprintf(out, "  !   Could not record the shared credential allow-lists before the rollout: %v\n"+
				"      An allow-list an older console-api clears during this upgrade will not be written back.\n", err)
			return credentialGrants{}, nil
		}
		return credentialGrants{}, err
	}
	// Taken before both shortcuts below. A credential decided before the
	// rollout is exactly the one neither of them looks at, and it is the one
	// the old writer erases.
	decided := sharedcred.Decisions(stored)
	if done {
		return credentialGrants{decided: decided}, nil
	}
	undecided := map[string]bool{}
	for _, e := range stored {
		if e.AllowedProjects == nil {
			undecided[e.Name] = true
		}
	}
	if len(undecided) == 0 {
		return credentialGrants{decided: decided, mayClose: true}, nil
	}
	usage, missed, err := sharedCredentialUsage(ctx, clientset, dyn, undecided)
	if err != nil {
		return credentialGrants{}, err
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
		return credentialGrants{}, err
	}
	switch decision {
	case consentDecline:
		printSeedConsentDeclineHint(out)
		return credentialGrants{decided: decided}, nil
	case consentGrant:
		return credentialGrants{approved: usage, decided: decided, mayClose: true}, nil
	default:
		return credentialGrants{decided: decided, mayClose: true}, nil
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
