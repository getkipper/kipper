package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func usersCM(usersJSON string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-users", Namespace: "kipper-system"},
		Data:       map[string]string{"users": usersJSON},
	}
}

// Project membership is an address and nothing downstream checks that it belongs
// to anyone, so a typo becomes a member who cannot sign in, reads correctly in
// `members list`, and counts as an owner in the rule that keeps a project from
// being left ownerless. The console has always refused an unknown address; the
// CLI wrote it straight through.
func TestAccountExists(t *testing.T) {
	cs := k8sfake.NewSimpleClientset(usersCM(`{"lead@acme.com":"admin","dev@acme.com":"viewer"}`))
	ctx := context.Background()

	known, err := accountExists(ctx, cs, "dev@acme.com")
	require.NoError(t, err)
	assert.True(t, known)

	known, err = accountExists(ctx, cs, "typo@acme.com")
	require.NoError(t, err)
	assert.False(t, known, "an address with no account is not a member")
}

// A read that fails is not an absent account. Reporting one as the other would
// refuse a legitimate add with a message saying the person does not exist, which
// sends the operator looking for the wrong problem.
func TestAccountExists_ReportsAFailedReadRatherThanAbsence(t *testing.T) {
	cs := k8sfake.NewSimpleClientset(usersCM(`{"lead@acme.com":"admin"}`))
	cs.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(assert.AnError)
	})

	known, err := accountExists(context.Background(), cs, "lead@acme.com")
	require.Error(t, err, "a failed read must not be reported as a missing account")
	assert.False(t, known)
	assert.Contains(t, err.Error(), "user list")
}

// Malformed content is a failed read too, not an empty cluster.
func TestAccountExists_RefusesUnreadableContent(t *testing.T) {
	cs := k8sfake.NewSimpleClientset(usersCM(`not json`))
	_, err := accountExists(context.Background(), cs, "lead@acme.com")
	require.Error(t, err)
}

// The rule that keeps a project owned, and the flag that steps past it. Both
// halves shipped without a test.
func TestLastOwnerDecision(t *testing.T) {
	cases := []struct {
		name           string
		removingOwner  bool
		remainingOwner int
		force          bool
		refused        bool
	}{
		{"removing a deployer, owners untouched", false, 1, false, false},
		{"removing an owner while another remains", true, 1, false, false},
		{"removing the last owner", true, 0, false, true},
		{"removing the last owner with --force", true, 0, true, false},
		{"--force is irrelevant when an owner remains", true, 1, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.refused, refusesLastOwner(c.removingOwner, c.remainingOwner, c.force))
		})
	}
}

func member(email, role string) interface{} {
	return map[string]interface{}{"email": email, "role": role}
}

// Removing an address that is in no entry used to write the unchanged list back
// and print "✔ removed", which is the same defect the console API carried: an
// operator watches a member disappear from the output while their access
// continues. It also decides the owner count the last-owner rule reads.
func TestWithoutMember(t *testing.T) {
	cases := []struct {
		name            string
		members         []interface{}
		email           string
		matched         bool
		kept            []interface{}
		removingOwner   bool
		remainingOwners int
	}{
		{
			name:            "removes a deployer and leaves the owner",
			members:         []interface{}{member("lead@acme.com", "owner"), member("dev@acme.com", "deployer")},
			email:           "dev@acme.com",
			matched:         true,
			kept:            []interface{}{member("lead@acme.com", "owner")},
			removingOwner:   false,
			remainingOwners: 1,
		},
		{
			// A role this build does not know reaches a Project by kubectl, by
			// a restore, or by a migration off a cluster that had it. Removing
			// that member has to work: not understanding what somebody holds is
			// a reason to revoke them, never a reason the command cannot.
			name:            "removes a member holding a role this build does not know",
			members:         []interface{}{member("lead@acme.com", "owner"), member("stranger@acme.com", "acme.support")},
			email:           "stranger@acme.com",
			matched:         true,
			kept:            []interface{}{member("lead@acme.com", "owner")},
			removingOwner:   false,
			remainingOwners: 1,
		},
		{
			// The last-owner guard keeps a project from being orphaned, and a
			// role nobody can interpret is not evidence that somebody can still
			// administer it.
			name:            "an unrecognised role does not count towards the remaining owners",
			members:         []interface{}{member("lead@acme.com", "owner"), member("stranger@acme.com", "acme.support")},
			email:           "lead@acme.com",
			matched:         true,
			kept:            []interface{}{member("stranger@acme.com", "acme.support")},
			removingOwner:   true,
			remainingOwners: 0,
		},
		{
			name:            "an address in no entry matches nothing and keeps everyone",
			members:         []interface{}{member("lead@acme.com", "owner"), member("dev@acme.com", "deployer")},
			email:           "typo@acme.com",
			matched:         false,
			kept:            []interface{}{member("lead@acme.com", "owner"), member("dev@acme.com", "deployer")},
			removingOwner:   false,
			remainingOwners: 1,
		},
		{
			name:            "removing the only owner leaves none behind",
			members:         []interface{}{member("lead@acme.com", "owner"), member("dev@acme.com", "deployer")},
			email:           "lead@acme.com",
			matched:         true,
			kept:            []interface{}{member("dev@acme.com", "deployer")},
			removingOwner:   true,
			remainingOwners: 0,
		},
		{
			// Both entries go, so the project is left with no owner. Reading
			// only the last one would report a deployer leaving and walk past
			// the rule that keeps a project owned.
			name:            "a duplicate entry cannot clear the owner the first one found",
			members:         []interface{}{member("lead@acme.com", "owner"), member("lead@acme.com", "deployer")},
			email:           "lead@acme.com",
			matched:         true,
			kept:            []interface{}{},
			removingOwner:   true,
			remainingOwners: 0,
		},
		{
			name:    "an entry that is not a map is skipped rather than kept",
			members: []interface{}{member("lead@acme.com", "owner"), "junk"},
			email:   "dev@acme.com",
			matched: false, kept: []interface{}{member("lead@acme.com", "owner")},
			removingOwner: false, remainingOwners: 1,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			removal := withoutMember(c.members, c.email)
			assert.Equal(t, c.matched, removal.matched)
			// The exact survivors, not their count: rebuilding a different
			// list of the same length would otherwise pass.
			assert.Equal(t, c.kept, removal.kept)
			assert.Equal(t, c.removingOwner, removal.removingOwner)
			assert.Equal(t, c.remainingOwners, removal.remainingOwners)
		})
	}
}
