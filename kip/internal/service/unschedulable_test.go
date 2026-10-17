package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// Scheduling failure details come from pod conditions, which distinguish
// blocked scheduling from normal startup.

func unschedulablePod(namespace, service, message string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      service + "-0",
			Namespace: namespace,
			Labels:    map[string]string{"app": service, "kipper.run/service-type": "postgres"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: message,
			}},
		},
	}
}

func TestListNamesAServiceTheSchedulerCannotPlace(t *testing.T) {
	client := fake.NewSimpleClientset(unschedulablePod("shop", "db", "0/1 nodes are available: 1 Insufficient cpu.")) //nolint:staticcheck
	mgr := &Manager{Client: client, Dynamic: fakeDynamicForServices(t, "shop", map[string]string{"db": "postgres"})}

	services, err := mgr.List(context.Background(), "shop")
	require.NoError(t, err)
	require.Len(t, services, 1)
	assert.Equal(t, "FailedScheduling", services[0].BlockedReason)
	assert.Contains(t, services[0].BlockedMessage, "1 Insufficient cpu", "the scheduler's own words")
	assert.Contains(t, services[0].BlockedMessage, "kip service update db", "and the way out")
}

func TestListLeavesAScheduledPendingServiceAlone(t *testing.T) {
	starting := unschedulablePod("shop", "db", "")
	starting.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}}
	starting.Spec.NodeName = "server"
	client := fake.NewSimpleClientset(starting) //nolint:staticcheck
	mgr := &Manager{Client: client, Dynamic: fakeDynamicForServices(t, "shop", map[string]string{"db": "postgres"})}

	services, err := mgr.List(context.Background(), "shop")
	require.NoError(t, err)
	assert.Empty(t, services[0].BlockedReason, "a pod that is merely starting raises nothing")
}

func TestPlacementCarriesTheSchedulerReason(t *testing.T) {
	client := fake.NewSimpleClientset(unschedulablePod("shop", "db", "0/1 nodes are available: 1 Insufficient cpu.")) //nolint:staticcheck
	mgr := &Manager{Client: client}

	p, err := mgr.Placement(context.Background(), "shop", "db")
	require.NoError(t, err)
	assert.Empty(t, p.PodNode)
	assert.Equal(t, "0/1 nodes are available: 1 Insufficient cpu.", p.Unscheduled)
}
