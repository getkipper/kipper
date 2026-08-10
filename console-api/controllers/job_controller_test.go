package controllers

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

func newTestJob(schedule string) *kipperv1.Job {
	return &kipperv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "migrate", Namespace: "shop-test"},
		Spec: kipperv1.JobSpec{
			Image:    "ghcr.io/example/migrate:1.4.0",
			Schedule: schedule,
			Env:      map[string]string{"DB_HOST": "postgres", "LOG_LEVEL": "info"},
		},
	}
}

func reconcileJob(t *testing.T, job *kipperv1.Job) *JobReconciler {
	t.Helper()
	scheme := testScheme()
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(job).WithStatusSubresource(job).Build()
	r := &JobReconciler{Client: c, Scheme: scheme}

	// Twice: the first pass only adds the finalizer and returns.
	for range 2 {
		_, err := r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: job.Name, Namespace: job.Namespace},
		})
		require.NoError(t, err)
	}
	return r
}

// Job pods reference their env Secret by name and the reference is Optional, so
// a Job whose Secret nothing renders starts normally and runs with none of the
// environment it declares. The name is spelled out here rather than built with
// the helper production uses: the point of the assertion is that the object the
// pod asks for is the object the controller writes.
func TestJobReconcile_RendersTheEnvSecretThePodTemplateReferences(t *testing.T) {
	job := newTestJob("0 3 * * *")
	r := reconcileJob(t, job)

	secret := renderedEnvSecret(t, context.Background(), r.Client, secretname.KindJob, "migrate", "shop-test")
	assert.Equal(t, map[string][]byte{
		"DB_HOST":   []byte("postgres"),
		"LOG_LEVEL": []byte("info"),
	}, secret.Data)

	require.Len(t, secret.OwnerReferences, 1, "the Secret must be garbage-collected with its Job")
	assert.Equal(t, "migrate", secret.OwnerReferences[0].Name)

	var cj batchv1.CronJob
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Name: "migrate", Namespace: "shop-test"}, &cj))
	published := podEnvGeneration(t, context.Background(), r.Client,
		cj.Spec.JobTemplate.Spec.Template.Spec, "job-migrate-env-", "shop-test")
	assert.Equal(t, secret.Data["DATABASE_URL"], published.Data["DATABASE_URL"],
		"the CronJob's pod template must read the environment the controller published")
}

func TestJobReconcile_OneOffJobReadsTheRenderedSecret(t *testing.T) {
	job := newTestJob("")
	r := reconcileJob(t, job)

	secret := renderedEnvSecret(t, context.Background(), r.Client, secretname.KindJob, "migrate", "shop-test")

	var nj batchv1.Job
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Name: "migrate", Namespace: "shop-test"}, &nj))
	published := podEnvGeneration(t, context.Background(), r.Client,
		nj.Spec.Template.Spec, "job-migrate-env-", "shop-test")
	assert.Equal(t, secret.Data["DATABASE_URL"], published.Data["DATABASE_URL"],
		"a one-off Job's pod template is immutable, so it must reference an object that already exists")
}

// A Job called api must not share an env Secret with an App or a Function of
// the same name: all three are workload names in one namespace, and the Secret
// carries resolved service credentials.
func TestJobReconcile_EnvSecretIsDisjointFromOtherKinds(t *testing.T) {
	job := &kipperv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop-test"},
		Spec:       kipperv1.JobSpec{Image: "busybox:1.37", Env: map[string]string{"ROLE": "job"}},
	}
	r := reconcileJob(t, job)

	published := renderedEnvSecret(t, context.Background(), r.Client, secretname.KindJob, "api", "shop-test")
	assert.Equal(t, []byte("job"), published.Data["ROLE"])

	var all corev1.SecretList
	require.NoError(t, r.List(context.Background(), &all, crclient.InNamespace("shop-test")))
	for i := range all.Items {
		name := all.Items[i].Name
		for _, taken := range []string{"api-env", "app-api-env", "function-api-env"} {
			assert.Falsef(t, strings.HasPrefix(name, taken),
				"a Job must not write %s, which belongs to another workload kind (got %q)", taken, name)
		}
	}
}

// Emptying spec.env has to clear the Secret. Leaving the old keys behind means
// the next run loads values the user removed.
func TestJobReconcile_ClearsTheSecretWhenEnvEmptied(t *testing.T) {
	job := newTestJob("")
	scheme := testScheme()
	stale := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "job-migrate-env", Namespace: "shop-test"},
		Data:       map[string][]byte{"OLD_KEY": []byte("stale")},
	}
	job.Spec.Env = nil
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(job, stale).WithStatusSubresource(job).Build()
	r := &JobReconciler{Client: c, Scheme: scheme}

	mustPublishJobEnv(t, r, context.Background(), job)

	secret := renderedEnvSecret(t, context.Background(), r.Client, secretname.KindJob, "migrate", "shop-test")
	assert.Empty(t, secret.Data, "emptying spec.env must clear the Secret, not leave stale keys")
}
