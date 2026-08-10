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
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "old-build", Namespace: buildsNamespace, Labels: labels, CreationTimestamp: old}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "young-build", Namespace: buildsNamespace, Labels: labels, CreationTimestamp: young}},
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
