package deployer

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const restartStamp = "2026-10-12T14:00:00.123456789Z"

func restartedDeployment(stamp string, want, updated, available, total int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default", Generation: 4},
		Spec: appsv1.DeploymentSpec{
			Replicas: &want,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{"kipper.run/restartedAt": stamp},
			}},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 4, Replicas: total, UpdatedReplicas: updated, AvailableReplicas: available,
			UnavailableReplicas: want - available,
		},
	}
}

func appPod(name, stamp string) corev1.Pod {
	return corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "default", Labels: map[string]string{"app": "api"},
		Annotations: map[string]string{"kipper.run/restartedAt": stamp},
	}}
}

func TestRestartDone(t *testing.T) {
	fresh := []corev1.Pod{appPod("api-new-1", restartStamp), appPod("api-new-2", restartStamp)}
	for _, tc := range []struct {
		name string
		dep  *appsv1.Deployment
		want bool
	}{
		{"the controller has not applied this restart", restartedDeployment("an earlier restart", 2, 2, 2, 2), false},
		{"new pods are still starting", restartedDeployment(restartStamp, 2, 1, 1, 3), false},
		{"old pods are still shutting down", restartedDeployment(restartStamp, 2, 2, 2, 3), false},
		{"every pod is replaced and ready", restartedDeployment(restartStamp, 2, 2, 2, 2), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done, err := restartDone(tc.dep, fresh, restartStamp)
			require.NoError(t, err)
			assert.Equal(t, tc.want, done)
		})
	}

	stalled := restartedDeployment(restartStamp, 1, 1, 0, 2)
	stalled.Status.Conditions = []appsv1.DeploymentCondition{{
		Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded",
		Message: `ReplicaSet "api-7d4" has timed out progressing.`,
	}}
	_, err := restartDone(stalled, fresh, restartStamp)
	assert.ErrorContains(t, err, "timed out progressing")
	assert.ErrorContains(t, err, "1/1 pods replaced, 0 ready")

	// A stall reported before the controller observes the new template is stale.
	stale := stalled.DeepCopy()
	stale.Generation = 5
	done, err := restartDone(stale, fresh, restartStamp)
	assert.NoError(t, err)
	assert.False(t, done)
}

func TestRestartDoneWaitsForTerminatingOldPods(t *testing.T) {
	// The pod list can include terminating pods after replica counts settle.
	settled := restartedDeployment(restartStamp, 1, 1, 1, 1)

	old := appPod("api-old", "an earlier restart")
	old.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	done, err := restartDone(settled, []corev1.Pod{appPod("api-new", restartStamp), old}, restartStamp)
	require.NoError(t, err)
	assert.False(t, done)

	done, err = restartDone(settled, []corev1.Pod{appPod("api-new", restartStamp)}, restartStamp)
	require.NoError(t, err)
	assert.True(t, done)
}

func TestWaitForRestartReturnsOnceTheNewPodsAreReady(t *testing.T) {
	fresh := appPod("api-new", restartStamp)
	other := appPod("web-1", "an earlier restart")
	other.Labels = map[string]string{"app": "web"}
	client := fake.NewClientset(restartedDeployment("an earlier restart", 1, 1, 1, 1), &fresh, &other)
	gets := 0
	client.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		gets++
		switch {
		case gets < 2:
			return true, restartedDeployment("an earlier restart", 1, 1, 1, 1), nil
		case gets < 4:
			return true, restartedDeployment(restartStamp, 1, 1, 0, 2), nil
		}
		return true, restartedDeployment(restartStamp, 1, 1, 1, 1), nil
	})
	d := &Deployer{Client: client}

	err := d.WaitForRestart(context.Background(), "default", "api", restartStamp, time.Minute, time.Millisecond)

	require.NoError(t, err)
	assert.Equal(t, 4, gets, "an old or half-finished status must not end the wait")
}

func TestWaitForRestartTimesOut(t *testing.T) {
	notApplied := &Deployer{Client: fake.NewClientset(restartedDeployment("an earlier restart", 1, 1, 1, 1))}
	err := notApplied.WaitForRestart(context.Background(), "default", "api", restartStamp, 5*time.Millisecond, time.Millisecond)
	assert.ErrorContains(t, err, "has not applied the restart")

	starting := &Deployer{Client: fake.NewClientset(restartedDeployment(restartStamp, 2, 1, 1, 3))}
	err = starting.WaitForRestart(context.Background(), "default", "api", restartStamp, 5*time.Millisecond, time.Millisecond)
	assert.ErrorContains(t, err, "1/2")
	assert.ErrorContains(t, err, "kip app info api")
}
