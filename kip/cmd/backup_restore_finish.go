package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/getkipper/kipper/kip/internal/service"
	"github.com/getkipper/kipper/kip/internal/velerolabel"
)

const restoreWaitLimit = 4 * time.Hour

type restoreState struct {
	Phase, Reason string
	// Scope contains destination namespaces; empty means unrestricted.
	Scope []string
}

type restoredSecret struct {
	Namespace, Name string
}

type restoreSteps struct {
	phase    func(context.Context) (restoreState, error)
	pause    func(context.Context) error
	restored func(context.Context) ([]restoredSecret, error)
	audit    func(context.Context, string) (service.CredentialAudit, error)
	repair   func(context.Context, string, service.CredentialAudit) ([]string, error)
	// ask is nil without a terminal.
	ask func(string) bool
	// waited releases the wait-specific interrupt handler before auditing or prompting.
	waited func()
}

const restoreReadRetries = 5

// errWaitStopped tells the caller to print follow-up commands for the restore.
var errWaitStopped = errors.New("stopped waiting; the restore continues on the cluster")

// finishRestore waits for completion and offers credential repair, including after
// partial failure. A partially failed restore returns an error even if repair succeeds.
func finishRestore(ctx context.Context, s restoreSteps, out io.Writer, repair bool, org string) error {
	var state restoreState
	last := ""
	failures := 0
	for {
		var err error
		if state, err = s.phase(ctx); err != nil {
			failures++
			if failures >= restoreReadRetries {
				return fmt.Errorf("%w (reading restore status failed %d times in a row): %w", errWaitStopped, failures, err)
			}
			if err := s.pause(ctx); err != nil {
				return fmt.Errorf("%w: %w", errWaitStopped, err)
			}
			continue
		}
		failures = 0
		if state.Phase != last && state.Phase != "" {
			_, _ = fmt.Fprintf(out, "  ·  %s\n", state.Phase)
			last = state.Phase
		}
		if state.Phase == "Completed" || state.Phase == "PartiallyFailed" {
			break
		}
		if state.Phase == "Failed" || state.Phase == "FailedValidation" {
			reason := state.Reason
			if reason == "" {
				reason = "Velero gave no reason"
			}
			return fmt.Errorf("the restore ended %s: %s", state.Phase, reason)
		}
		if err := s.pause(ctx); err != nil {
			return fmt.Errorf("%w: %w", errWaitStopped, err)
		}
	}

	if s.waited != nil {
		s.waited()
	}

	err := reattachRestored(ctx, s, state.Scope, out, repair, org)
	if state.Phase == "PartiallyFailed" {
		return errors.Join(err, errors.New("the restore ended PartiallyFailed: resources or volume data may be incomplete; check the restored projects before using them"))
	}
	return err
}

// reattachRestored offers repair for restored credentials within the destination scope.
// Repair requires consent through the flag or prompt; matching services may already exist.
func reattachRestored(ctx context.Context, s restoreSteps, scope []string, out io.Writer, repair bool, org string) error {
	labelled, err := s.restored(ctx)
	if err != nil {
		return fmt.Errorf("finding the restored Secrets: %w", err)
	}
	restored := map[string][]string{}
	for _, secret := range labelled {
		if len(scope) > 0 && !slices.Contains(scope, secret.Namespace) {
			continue
		}
		restored[secret.Namespace] = append(restored[secret.Namespace], secret.Name)
	}
	if len(restored) == 0 {
		_, _ = fmt.Fprintf(out, "  ✔  The restore brought back no Secrets with service credentials\n\n")
		return nil
	}
	namespaces := make([]string, 0, len(restored))
	for ns := range restored {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)

	candidates := map[string]service.CredentialAudit{}
	var listed, unfixable, others, orphans []string
	for _, ns := range namespaces {
		audit, err := s.audit(ctx, ns)
		if err != nil {
			return fmt.Errorf("checking the credentials in %s: %w", ns, err)
		}
		if len(audit.Projections) > 0 {
			others = append(others, ns)
		}
		for _, name := range restored[ns] {
			if !slices.ContainsFunc(audit.Services, func(r service.CredentialReport) bool { return r.Secret == name }) {
				orphans = append(orphans, ns+"/"+name)
			}
		}
		for _, report := range audit.Services {
			if !slices.Contains(restored[ns], report.Secret) {
				if report.State != service.CredentialOwned && !slices.Contains(others, ns) {
					others = append(others, ns)
				}
				continue
			}
			switch report.State {
			case service.CredentialOwned:
			case service.CredentialUnowned, service.CredentialAbandoned:
				c := candidates[ns]
				c.Services = append(c.Services, report)
				candidates[ns] = c
				listed = append(listed, ns+"/"+report.Service)
			default:
				state := string(report.State)
				if report.Owner != "" {
					state += ", held by " + report.Owner
				}
				unfixable = append(unfixable, fmt.Sprintf("%s/%s (%s)", ns, report.Service, state))
			}
		}
	}
	if len(listed) == 0 && len(unfixable) == 0 && len(others) == 0 && len(orphans) == 0 {
		_, _ = fmt.Fprintf(out, "  ✔  The restored credentials Secrets belong to their services\n\n")
		return nil
	}
	if len(orphans) > 0 {
		_, _ = fmt.Fprintf(out, "  ·  These restored credentials Secrets have no matching service: %s\n", strings.Join(orphans, ", "))
	}
	for _, ns := range others {
		_, _ = fmt.Fprintf(out, "  ·  Other credentials or binding Secrets in %s need review: %s\n", ns, credentialsCommandFor(ns, org, false))
	}

	if len(listed) > 0 {
		_, _ = fmt.Fprintf(out, "\n  These services have restored credentials Secrets with no live owner: %s\n", strings.Join(listed, ", "))
		if !repair && s.ask != nil {
			repair = s.ask("Re-attach them? Answer yes only if nobody else could write Secrets in those projects while the restore ran.")
		}
		if repair {
			for _, ns := range namespaces {
				c, ok := candidates[ns]
				if !ok {
					continue
				}
				done, err := s.repair(ctx, ns, c)
				for _, line := range done {
					_, _ = fmt.Fprintf(out, "  ✔  %s\n", line)
				}
				if err != nil {
					return err
				}
			}
			_, _ = fmt.Fprintln(out)
		} else {
			for _, ns := range namespaces {
				if _, ok := candidates[ns]; ok {
					_, _ = fmt.Fprintf(out, "  To re-attach them later: %s\n", credentialsCommandFor(ns, org, true))
				}
			}
			_, _ = fmt.Fprintln(out)
		}
	}
	if len(unfixable) > 0 {
		return fmt.Errorf("kip cannot re-attach the credentials of %s; run kip service credentials for those projects to see why", strings.Join(unfixable, ", "))
	}
	return nil
}

func clusterRestoreSteps(dyn dynamic.Interface, mgr *service.Manager, restoreName string, ask func(string) bool) restoreSteps {
	return restoreSteps{
		phase: func(ctx context.Context) (restoreState, error) {
			r, err := dyn.Resource(restoreGVR).Namespace("velero").Get(ctx, restoreName, metav1.GetOptions{})
			if err != nil {
				return restoreState{}, err
			}
			return restoreStateOf(r), nil
		},
		pause: func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
				return nil
			}
		},
		restored: func(ctx context.Context) ([]restoredSecret, error) {
			list, err := mgr.Client.CoreV1().Secrets("").List(ctx, metav1.ListOptions{
				LabelSelector: "velero.io/restore-name=" + velerolabel.ValidName(restoreName) + ",kipper.run/service-type",
			})
			if err != nil {
				return nil, err
			}
			secrets := make([]restoredSecret, 0, len(list.Items))
			for _, item := range list.Items {
				secrets = append(secrets, restoredSecret{Namespace: item.Namespace, Name: item.Name})
			}
			return secrets, nil
		},
		audit:  mgr.AuditCredentials,
		repair: mgr.RepairCredentials,
		ask:    ask,
	}
}

func restoreStateOf(r *unstructured.Unstructured) restoreState {
	var s restoreState
	s.Phase, _, _ = unstructured.NestedString(r.Object, "status", "phase")
	s.Reason, _, _ = unstructured.NestedString(r.Object, "status", "failureReason")
	included, _, _ := unstructured.NestedStringSlice(r.Object, "spec", "includedNamespaces")
	mapping, _, _ := unstructured.NestedStringMap(r.Object, "spec", "namespaceMapping")
	for _, ns := range included {
		if ns == "*" {
			return restoreState{Phase: s.Phase, Reason: s.Reason}
		}
		if target, ok := mapping[ns]; ok {
			ns = target
		}
		s.Scope = append(s.Scope, ns)
	}
	return s
}

// credentialsCommandFor checks namespace resolution before suggesting a project command.
func credentialsCommandFor(ns, org string, repair bool) string {
	if org != "" && !strings.HasPrefix(ns, org+"-") {
		return fmt.Sprintf("namespace %s is outside organization %s; kip service credentials requires a namespace in this organization", ns, org)
	}
	cmd := "kip service credentials --project " + ns
	if repair {
		cmd += " --repair"
	}
	return cmd
}
