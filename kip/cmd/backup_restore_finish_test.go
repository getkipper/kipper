package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/getkipper/kipper/kip/internal/service"
)

type fakeRestore struct {
	phases   []string
	reason   string
	scope    []string
	secrets  []restoredSecret
	audits   map[string]service.CredentialAudit
	repaired map[string][]string
	asked    []string
	answer   bool
}

func (f *fakeRestore) steps(tty bool) restoreSteps {
	s := restoreSteps{
		phase: func(context.Context) (restoreState, error) {
			p := f.phases[0]
			if len(f.phases) > 1 {
				f.phases = f.phases[1:]
			}
			return restoreState{Phase: p, Reason: f.reason, Scope: f.scope}, nil
		},
		pause:    func(ctx context.Context) error { return ctx.Err() },
		restored: func(context.Context) ([]restoredSecret, error) { return f.secrets, nil },
		audit: func(_ context.Context, ns string) (service.CredentialAudit, error) {
			return f.audits[ns], nil
		},
		repair: func(_ context.Context, ns string, audit service.CredentialAudit) ([]string, error) {
			if f.repaired == nil {
				f.repaired = map[string][]string{}
			}
			var done []string
			for _, s := range audit.Services {
				f.repaired[ns] = append(f.repaired[ns], s.Service)
				done = append(done, s.Secret+" now belongs to service "+s.Service)
			}
			return done, nil
		},
	}
	if tty {
		s.ask = func(q string) bool { f.asked = append(f.asked, q); return f.answer }
	}
	return s
}

func unowned(name string) service.CredentialReport {
	return service.CredentialReport{Service: name, Secret: name + "-credentials", State: service.CredentialUnowned}
}

func shopRestore() *fakeRestore {
	return &fakeRestore{
		phases:  []string{"New", "InProgress", "InProgress", "Completed"},
		scope:   []string{"shop"},
		secrets: []restoredSecret{{Namespace: "shop", Name: "db-credentials"}},
		audits: map[string]service.CredentialAudit{"shop": {Services: []service.CredentialReport{
			unowned("db"),
			// Existing credentials require a separate repair decision.
			unowned("cache"),
		}}},
	}
}

func TestFinishRestoreWaitsThenReattachesOnlyRestoredServicesWithTheFlag(t *testing.T) {
	f := shopRestore()
	var out bytes.Buffer

	require.NoError(t, finishRestore(context.Background(), f.steps(true), &out, true, ""))

	assert.Equal(t, map[string][]string{"shop": {"db"}}, f.repaired)
	assert.Empty(t, f.asked, "the flag is the consent")
	assert.Contains(t, out.String(), "Completed")
	assert.Contains(t, out.String(), "db-credentials now belongs to service db")
}

func TestFinishRestoreReattachesOnlySecretsTheRestoreBroughtBack(t *testing.T) {
	f := shopRestore()
	f.secrets = append(f.secrets, restoredSecret{Namespace: "billing", Name: "ledger-credentials"})
	f.audits["billing"] = service.CredentialAudit{Services: []service.CredentialReport{unowned("ledger")}}
	var out bytes.Buffer

	require.NoError(t, finishRestore(context.Background(), f.steps(true), &out, true, ""))

	assert.Equal(t, map[string][]string{"shop": {"db"}}, f.repaired)
	assert.NotContains(t, out.String(), "ledger")
	assert.NotContains(t, out.String(), "cache")
}

func TestFinishRestoreAcceptsAnyNamespaceForAnUnscopedRestore(t *testing.T) {
	f := shopRestore()
	f.scope = nil

	require.NoError(t, finishRestore(context.Background(), f.steps(true), &bytes.Buffer{}, true, ""))

	assert.Equal(t, map[string][]string{"shop": {"db"}}, f.repaired)
}

func TestFinishRestoreAsksOnATerminal(t *testing.T) {
	f := shopRestore()
	f.answer = true
	var out bytes.Buffer

	require.NoError(t, finishRestore(context.Background(), f.steps(true), &out, false, ""))

	require.Len(t, f.asked, 1)
	assert.True(t, strings.HasPrefix(f.asked[0], "Re-attach"), "confirm indents the question itself")
	assert.Contains(t, out.String(), "shop/db")
	assert.Equal(t, map[string][]string{"shop": {"db"}}, f.repaired)
}

func TestFinishRestoreLeavesCredentialsAloneWhenTheOperatorDeclines(t *testing.T) {
	for name, tty := range map[string]bool{"declined": true, "no terminal": false} {
		f := shopRestore()
		var out bytes.Buffer

		require.NoError(t, finishRestore(context.Background(), f.steps(tty), &out, false, ""), name)

		assert.Empty(t, f.repaired, name)
		assert.Contains(t, out.String(), "kip service credentials --project shop --repair", name)
	}
}

func TestFinishRestoreReportsAFailedRestore(t *testing.T) {
	f := shopRestore()
	f.phases = []string{"InProgress", "Failed"}
	f.reason = "error getting backup: not found"
	var out bytes.Buffer

	err := finishRestore(context.Background(), f.steps(true), &out, true, "")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "error getting backup: not found")
	assert.Empty(t, f.repaired, "nothing is re-attached after a failed restore")
}

func TestFinishRestoreStopsWaitingWhenTheContextEnds(t *testing.T) {
	f := shopRestore()
	f.phases = []string{"InProgress"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := finishRestore(ctx, f.steps(true), &bytes.Buffer{}, true, "")

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
	assert.True(t, errors.Is(err, errWaitStopped), "the caller prints the follow-up")
	assert.Contains(t, err.Error(), "continues on the cluster")
	assert.Empty(t, f.repaired)
}

func TestFinishRestoreSaysWhenRestoredServicesOwnTheirCredentials(t *testing.T) {
	f := shopRestore()
	f.audits["shop"] = service.CredentialAudit{Services: []service.CredentialReport{{Service: "db", Secret: "db-credentials", State: service.CredentialOwned}}}
	var out bytes.Buffer

	require.NoError(t, finishRestore(context.Background(), f.steps(true), &out, false, ""))

	assert.Empty(t, f.asked)
	assert.Contains(t, out.String(), "belong to their services")
}

func TestFinishRestoreReportsCredentialsItCannotReattach(t *testing.T) {
	f := shopRestore()
	f.secrets = []restoredSecret{{Namespace: "shop", Name: "queue-credentials"}}
	f.audits["shop"] = service.CredentialAudit{Services: []service.CredentialReport{
		{Service: "db", Secret: "db-credentials", State: service.CredentialMissing},
		{Service: "queue", Secret: "queue-credentials", State: service.CredentialForeign, Owner: "App/other"}, //nolint:gosec // a Secret name, not a credential
	}}
	var out bytes.Buffer

	err := finishRestore(context.Background(), f.steps(true), &out, true, "")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "shop/queue (foreign")
	assert.Contains(t, out.String(), "need review", "missing credentials require a separate review")
	assert.NotContains(t, out.String(), "belong to their services")
	assert.Empty(t, f.repaired)
}

func TestFinishRestoreSaysWhenItBroughtBackNoSecrets(t *testing.T) {
	f := shopRestore()
	f.secrets = nil
	var out bytes.Buffer

	require.NoError(t, finishRestore(context.Background(), f.steps(true), &out, false, ""))

	assert.Contains(t, out.String(), "brought back no Secrets")
}

func TestFinishRestoreRidesOutATransientReadError(t *testing.T) {
	f := shopRestore()
	steps := f.steps(true)
	reads := 0
	inner := steps.phase
	steps.phase = func(ctx context.Context) (restoreState, error) {
		reads++
		if reads == 2 {
			return restoreState{}, errors.New("connection reset by peer")
		}
		return inner(ctx)
	}

	require.NoError(t, finishRestore(context.Background(), steps, &bytes.Buffer{}, true, ""))
	assert.Equal(t, map[string][]string{"shop": {"db"}}, f.repaired)
}

func TestFinishRestoreGivesUpAfterRepeatedReadErrors(t *testing.T) {
	f := shopRestore()
	steps := f.steps(true)
	steps.phase = func(context.Context) (restoreState, error) { return restoreState{}, errors.New("connection refused") }

	err := finishRestore(context.Background(), steps, &bytes.Buffer{}, true, "")

	require.Error(t, err)
	assert.True(t, errors.Is(err, errWaitStopped), "the caller prints the follow-up")
	assert.Contains(t, err.Error(), "continues on the cluster")
}

func TestFinishRestoreSaysWhenTheWaitIsOver(t *testing.T) {
	f := shopRestore()
	steps := f.steps(true)
	waited := false
	steps.waited = func() { waited = true }

	require.NoError(t, finishRestore(context.Background(), steps, &bytes.Buffer{}, true, ""))
	assert.True(t, waited)
}

func TestFinishRestoreHandlesPartiallyFailedAndAbandoned(t *testing.T) {
	f := shopRestore()
	f.phases = []string{"InProgress", "PartiallyFailed"}
	f.audits["shop"] = service.CredentialAudit{Services: []service.CredentialReport{{Service: "db", Secret: "db-credentials", State: service.CredentialAbandoned}}}

	err := finishRestore(context.Background(), f.steps(true), &bytes.Buffer{}, true, "")

	require.Error(t, err, "a partially failed restore is not a success, whatever the credentials")
	assert.Contains(t, err.Error(), "PartiallyFailed")
	assert.Equal(t, map[string][]string{"shop": {"db"}}, f.repaired, "the credentials are still re-attached")
}

func TestFinishRestoreReportsAPartiallyFailedRestoreWithNothingToRepair(t *testing.T) {
	for name, secrets := range map[string][]restoredSecret{"no Secrets": nil, "all owned": {{Namespace: "shop", Name: "db-credentials"}}} {
		f := shopRestore()
		f.phases = []string{"PartiallyFailed"}
		f.secrets = secrets
		f.audits["shop"] = service.CredentialAudit{Services: []service.CredentialReport{{Service: "db", Secret: "db-credentials", State: service.CredentialOwned}}}

		err := finishRestore(context.Background(), f.steps(true), &bytes.Buffer{}, true, "")

		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "PartiallyFailed", name)
	}
}

func TestFinishRestorePointsAtCredentialsItLeavesToTheOperator(t *testing.T) {
	f := shopRestore()
	f.audits["shop"] = service.CredentialAudit{
		Services:    []service.CredentialReport{{Service: "db", Secret: "db-credentials", State: service.CredentialOwned}, unowned("cache")},
		Projections: []service.StaleProjection{{Name: "web-db-binding"}},
	}
	var out bytes.Buffer

	require.NoError(t, finishRestore(context.Background(), f.steps(true), &out, true, ""))

	assert.Contains(t, out.String(), "kip service credentials --project shop")
	assert.NotContains(t, out.String(), "✔  The restored credentials Secrets belong to their services")
	assert.Empty(t, f.repaired)
}

func TestRestoreStateOfReadsTheMappedScope(t *testing.T) {
	restore := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"includedNamespaces": []interface{}{"shop", "billing"},
			"namespaceMapping":   map[string]interface{}{"shop": "shop-copy"},
		},
		"status": map[string]interface{}{"phase": "Failed", "failureReason": "no backup"},
	}}

	state := restoreStateOf(restore)

	assert.Equal(t, restoreState{Phase: "Failed", Reason: "no backup", Scope: []string{"shop-copy", "billing"}}, state)
}

func TestRestoreStateOfTreatsAWildcardAsAnyNamespace(t *testing.T) {
	restore := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{"includedNamespaces": []interface{}{"*"}},
	}}

	assert.Empty(t, restoreStateOf(restore).Scope)
}

func TestFinishRestoreNamesRestoredCredentialsWithoutAService(t *testing.T) {
	f := shopRestore()
	f.audits["shop"] = service.CredentialAudit{}
	var out bytes.Buffer

	require.NoError(t, finishRestore(context.Background(), f.steps(true), &out, true, ""))

	assert.Contains(t, out.String(), "have no matching service: shop/db-credentials")
	assert.NotContains(t, out.String(), "belong to their services")
	assert.Empty(t, f.repaired)
}
