package migrationjob

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestSubmit_CreatesJobAndMirrorsSecret(t *testing.T) {
	source := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "backend-credentials", Namespace: "demo-test"},
		Data: map[string][]byte{
			"HOST":     []byte("backend.demo-test.svc.cluster.local"),
			"PORT":     []byte("5432"),
			"USERNAME": []byte("kipper"),
			"PASSWORD": []byte("hunter2"),
			"NAME":     []byte("app"),
		},
	}
	client := fake.NewClientset(source)

	jobName, err := Submit(context.Background(), client, Spec{
		Namespace: "demo-prod",
		JobName:   "migrate-backend-1",
		Image:     "postgres:16-alpine",
		Command:   []string{"sh", "-c", "echo migrating"},
		EnvFrom: []corev1.EnvFromSource{
			{Prefix: "DST_", SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "backend-credentials"}}},
			{Prefix: "SRC_", SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "backend-from-test-credentials"}}},
		},
		Mirrors: []MirrorSpec{
			{SourceNamespace: "demo-test", SourceName: "backend-credentials", TargetName: "backend-from-test-credentials"},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "migrate-backend-1", jobName)

	job, err := client.BatchV1().Jobs("demo-prod").Get(context.Background(), "migrate-backend-1", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, int32(0), *job.Spec.BackoffLimit, "destructive ops must not retry")
	assert.NotNil(t, job.Spec.ActiveDeadlineSeconds)
	assert.NotNil(t, job.Spec.TTLSecondsAfterFinished)

	mirror, err := client.CoreV1().Secrets("demo-prod").Get(context.Background(), "backend-from-test-credentials", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, source.Data["PASSWORD"], mirror.Data["PASSWORD"])
	require.Len(t, mirror.OwnerReferences, 1)
	assert.Equal(t, "Job", mirror.OwnerReferences[0].Kind)
	assert.Equal(t, "migrate-backend-1", mirror.OwnerReferences[0].Name)
}

func TestSubmit_RollsBackJobIfMirrorFails(t *testing.T) {
	// No source secret in the cluster — mirror will fail with NotFound.
	client := fake.NewClientset()

	_, err := Submit(context.Background(), client, Spec{
		Namespace: "demo-prod",
		JobName:   "migrate-backend-1",
		Image:     "postgres:16-alpine",
		Command:   []string{"sh", "-c", "echo go"},
		Mirrors: []MirrorSpec{
			{SourceNamespace: "demo-test", SourceName: "missing", TargetName: "x"},
		},
	})
	assert.Error(t, err, "missing source secret should bubble up")

	_, getErr := client.BatchV1().Jobs("demo-prod").Get(context.Background(), "migrate-backend-1", metav1.GetOptions{})
	assert.Error(t, getErr, "Job must be rolled back when mirror fails")
}

func TestSubmit_OverwritesPreExistingMirror(t *testing.T) {
	source := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "src", Namespace: "demo-test"},
		Data:       map[string][]byte{"PASSWORD": []byte("new")},
	}
	stale := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "src-mirror", Namespace: "demo-prod"},
		Data:       map[string][]byte{"PASSWORD": []byte("old")},
	}
	client := fake.NewClientset(source, stale)

	_, err := Submit(context.Background(), client, Spec{
		Namespace: "demo-prod",
		JobName:   "migrate-1",
		Image:     "x", Command: []string{"sh"},
		Mirrors: []MirrorSpec{{SourceNamespace: "demo-test", SourceName: "src", TargetName: "src-mirror"}},
	})
	require.NoError(t, err)

	mirror, _ := client.CoreV1().Secrets("demo-prod").Get(context.Background(), "src-mirror", metav1.GetOptions{})
	assert.Equal(t, []byte("new"), mirror.Data["PASSWORD"], "stale mirror must be overwritten with fresh data")
	assert.Len(t, mirror.OwnerReferences, 1, "owner ref should attach to the new Job")
}

func TestSubmit_RequiresMandatoryFields(t *testing.T) {
	client := fake.NewClientset()

	cases := []struct {
		name string
		spec Spec
	}{
		{"missing namespace", Spec{JobName: "x", Image: "y", Command: []string{"sh"}}},
		{"missing job name", Spec{Namespace: "x", Image: "y", Command: []string{"sh"}}},
		{"missing image", Spec{Namespace: "x", JobName: "y", Command: []string{"sh"}}},
		{"empty command", Spec{Namespace: "x", JobName: "y", Image: "z"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Submit(context.Background(), client, tt.spec)
			assert.Error(t, err)
		})
	}
}

func TestGetStatus_ClassifiesPhases(t *testing.T) {
	now := metav1.NewTime(time.Now())

	cases := []struct {
		name string
		job  *batchv1.Job
		want Phase
	}{
		{
			name: "succeeded job",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "ns"},
				Status:     batchv1.JobStatus{Succeeded: 1, CompletionTime: &now, StartTime: &now},
			},
			want: PhaseSucceeded,
		},
		{
			name: "failed job",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "ns"},
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "boom"},
					},
					StartTime: &now,
				},
			},
			want: PhaseFailed,
		},
		{
			name: "running job",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "ns"},
				Status:     batchv1.JobStatus{Active: 1, StartTime: &now},
			},
			want: PhaseRunning,
		},
		{
			name: "pending job",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "ns"},
				Status:     batchv1.JobStatus{},
			},
			want: PhasePending,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewClientset(tt.job)
			s, err := GetStatus(context.Background(), client, "ns", "j")
			require.NoError(t, err)
			assert.Equal(t, tt.want, s.Phase)
		})
	}
}

func TestGetStatus_CleanedUpJobReportsSuccess(t *testing.T) {
	client := fake.NewClientset()
	s, err := GetStatus(context.Background(), client, "ns", "missing")
	require.NoError(t, err)
	assert.Equal(t, PhaseSucceeded, s.Phase)
	assert.Equal(t, "job has been cleaned up", s.Message)
}
