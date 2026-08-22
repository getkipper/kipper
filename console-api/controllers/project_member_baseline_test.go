package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// The audit trail can answer who changed a role and when, and not what it was
// before: no API audit event carries the object a write replaced. The
// reconciler is the one component that could know, so it records the member
// list it last projected.
//
// Nothing reads it yet, and that is the point. Project.status is written whole
// by every controller already running, so a pod whose struct lacks this field
// drops it on its next status write. In a rolling window that is one old pod
// away from an empty baseline, and a build that trusted it would report a
// widening with nothing to compare against. The field ships first and is read a
// release later, once every pod writes it.
func TestTheReconcilerRecordsTheMemberListItProjected(t *testing.T) {
	project := projectWithMembers(
		kipperv1.ProjectMember{Email: "anna@example.com", Role: kipperv1.ProjectRoleOwner},
		kipperv1.ProjectMember{Email: "ben@example.com", Role: kipperv1.ProjectRoleDeployer},
	)
	// The fake client leaves Generation at zero, and a baseline recording zero
	// against zero proves nothing. A real object's generation moves with every
	// spec write, which is what makes a stale baseline tellable from a current
	// one, so the fixture has to carry one.
	project.Generation = 7
	c := reconcileProject(t, project)

	var stored kipperv1.Project
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "shop"}, &stored))

	require.Len(t, stored.Status.ProjectedMembers, 2,
		"the reconciler projected two members and recorded no baseline, so a later release has nothing to diff a change against")
	byEmail := map[string]string{}
	for _, m := range stored.Status.ProjectedMembers {
		byEmail[m.Email] = string(m.Role)
	}
	assert.Equal(t, "owner", byEmail["anna@example.com"])
	assert.Equal(t, "deployer", byEmail["ben@example.com"])

	assert.Equal(t, int64(7), stored.Status.ProjectedMembersGeneration,
		"the baseline does not say which generation it came from, so nothing can tell a stale record from a current one")
}

// A stale baseline is worse than none, because it reads as current. The
// generation it came from is what tells them apart, and it has to move when the
// members do.
func TestTheBaselineFollowsTheMemberList(t *testing.T) {
	project := projectWithMembers(
		kipperv1.ProjectMember{Email: "anna@example.com", Role: kipperv1.ProjectRoleOwner},
		kipperv1.ProjectMember{Email: "ben@example.com", Role: kipperv1.ProjectRoleDeployer},
	)
	c := reconcileProject(t, project)

	var stored kipperv1.Project
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "shop"}, &stored))
	stored.Spec.Members = []kipperv1.ProjectMember{
		{Email: "anna@example.com", Role: kipperv1.ProjectRoleOwner},
	}
	require.NoError(t, c.Update(context.Background(), &stored))

	reconcileNamed(t, c, "shop")

	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "shop"}, &stored))
	require.Len(t, stored.Status.ProjectedMembers, 1,
		"the baseline still holds a member the project no longer lists, so it describes a state that is gone")
	assert.Equal(t, "anna@example.com", stored.Status.ProjectedMembers[0].Email)
}

// Nothing reads it yet. A build that started trusting the baseline before every
// pod wrote it would be trusting a field an older pod had just dropped, so this
// holds the release boundary rather than the behaviour.
func TestNothingReadsTheBaselineYet(t *testing.T) {
	project := projectWithMembers(
		kipperv1.ProjectMember{Email: "anna@example.com", Role: kipperv1.ProjectRoleOwner},
	)
	c := reconcileProject(t, project)

	var stored kipperv1.Project
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "shop"}, &stored))

	// An older pod's whole-status write drops the field. The next reconcile has
	// to carry on exactly as before rather than treating the gap as a change.
	stored.Status.ProjectedMembers = nil
	stored.Status.ProjectedMembersGeneration = 0
	require.NoError(t, c.Status().Update(context.Background(), &stored))

	reconcileNamed(t, c, "shop")

	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "shop"}, &stored))
	assert.Equal(t, "Active", stored.Status.Phase,
		"an erased baseline changed what the reconcile did, so something is already reading it")
	assert.Len(t, stored.Status.ProjectedMembers, 1,
		"the reconcile did not put the baseline back after an older pod dropped it")
}
