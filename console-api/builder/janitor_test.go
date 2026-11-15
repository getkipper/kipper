package builder

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestBuildJanitor_SweepsOldKeepsYoung(t *testing.T) {
	old := metav1.NewTime(time.Now().Add(-4 * time.Hour))
	young := metav1.NewTime(time.Now())
	labels := map[string]string{buildLabel: "true"}

	client := fake.NewClientset(
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "old-build", Namespace: buildsNamespace, Labels: labels, CreationTimestamp: old}, Status: batchv1.JobStatus{StartTime: &old}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "young-build", Namespace: buildsNamespace, Labels: labels, CreationTimestamp: young}, Status: batchv1.JobStatus{StartTime: &young}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "old-secret", Namespace: buildsNamespace, Labels: labels, CreationTimestamp: old}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "young-secret", Namespace: buildsNamespace, Labels: labels, CreationTimestamp: young}},
	)

	sweepBuildNamespace(context.Background(), client, 3*time.Hour)

	jobs, err := client.BatchV1().Jobs(buildsNamespace).List(context.Background(), metav1.ListOptions{})
	assert.NoError(t, err)
	if assert.Len(t, jobs.Items, 1, "the build past max age is swept, the running one is kept") {
		assert.Equal(t, "young-build", jobs.Items[0].Name)
	}

	secrets, err := client.CoreV1().Secrets(buildsNamespace).List(context.Background(), metav1.ListOptions{})
	assert.NoError(t, err)
	if assert.Len(t, secrets.Items, 1, "the orphaned ephemeral secret past max age is swept") {
		assert.Equal(t, "young-secret", secrets.Items[0].Name)
	}
}

// A build waits in the queue for as long as the builds ahead of it take, so
// only time since it started counts against it, and its credentials stay
// while it waits.
func TestBuildJanitor_AgesABuildFromWhenItStarted(t *testing.T) {
	old := metav1.NewTime(time.Now().Add(-4 * time.Hour))
	now := metav1.Now()
	labels := map[string]string{buildLabel: "true"}
	suspended := true
	queued := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "queued", Namespace: buildsNamespace, Labels: labels, CreationTimestamp: old, UID: "queued-uid"},
		Spec:       batchv1.JobSpec{Suspend: &suspended},
	}
	justStarted := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "just-started", Namespace: buildsNamespace, Labels: labels, CreationTimestamp: old},
		Status:     batchv1.JobStatus{StartTime: &now, Active: 1},
	}
	hung := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "hung", Namespace: buildsNamespace, Labels: labels, CreationTimestamp: old},
		Status:     batchv1.JobStatus{StartTime: &old, Active: 1},
	}
	queuedSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "queued-git", Namespace: buildsNamespace, Labels: labels, CreationTimestamp: old,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: "queued", UID: "queued-uid"}},
	}}
	client := fake.NewClientset(queued, justStarted, hung, queuedSecret)

	sweepBuildNamespace(context.Background(), client, 3*time.Hour)

	jobs, err := client.BatchV1().Jobs(buildsNamespace).List(context.Background(), metav1.ListOptions{})
	assert.NoError(t, err)
	var names []string
	for _, j := range jobs.Items {
		names = append(names, j.Name)
	}
	assert.ElementsMatch(t, []string{"queued", "just-started"}, names)

	_, err = client.CoreV1().Secrets(buildsNamespace).Get(context.Background(), "queued-git", metav1.GetOptions{})
	assert.NoError(t, err, "a waiting build keeps its credentials")
}

// A restart between creating a build and taking ownership of its credentials
// leaves those credentials without an owner. While the build waits they are
// still in use, which its pod template shows.
func TestBuildJanitor_KeepsCredentialsAWaitingBuildReferences(t *testing.T) {
	old := metav1.NewTime(time.Now().Add(-4 * time.Hour))
	labels := map[string]string{buildLabel: "true"}
	suspended := true
	queued := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "queued", Namespace: buildsNamespace, Labels: labels, CreationTimestamp: old},
		Spec: batchv1.JobSpec{
			Suspend: &suspended,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Volumes: []corev1.Volume{{Name: "push", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "queued-push"}}}},
				InitContainers: []corev1.Container{{Name: "clone", Env: []corev1.EnvVar{{Name: "GIT_TOKEN", ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "queued-git"}, Key: "token"},
				}}}}},
			}},
		},
	}
	secret := func(name string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: buildsNamespace, Labels: labels, CreationTimestamp: old}}
	}
	client := fake.NewClientset(queued, secret("queued-push"), secret("queued-git"), secret("orphan"))

	sweepBuildNamespace(context.Background(), client, 3*time.Hour)

	secrets, err := client.CoreV1().Secrets(buildsNamespace).List(context.Background(), metav1.ListOptions{})
	assert.NoError(t, err)
	var names []string
	for _, s := range secrets.Items {
		names = append(names, s.Name)
	}
	assert.ElementsMatch(t, []string{"queued-push", "queued-git"}, names)
}

// A cluster that lets builds run longer than the janitor's age would otherwise
// lose a build that is still within its time limit.
func TestBuildJanitor_LeavesABuildWithinItsTimeLimit(t *testing.T) {
	t.Setenv("BUILD_TIMEOUT", "6h")
	started := metav1.NewTime(time.Now().Add(-4 * time.Hour))
	labels := map[string]string{buildLabel: "true"}
	client := fake.NewClientset(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "long-build", Namespace: buildsNamespace, Labels: labels, CreationTimestamp: started},
		Status:     batchv1.JobStatus{StartTime: &started, Active: 1},
	})

	sweepBuildNamespace(context.Background(), client, 3*time.Hour)

	jobs, err := client.BatchV1().Jobs(buildsNamespace).List(context.Background(), metav1.ListOptions{})
	assert.NoError(t, err)
	assert.Len(t, jobs.Items, 1)
}
