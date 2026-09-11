package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The CronJob is a child the reconciler rebuilds from the Job CR, so a write
// made straight to it lives only until the next reconcile. Anything that means
// to change what a job runs with has to change the CR.
func TestReconcileCronJob_RebuildsResourcesFromTheJobCR(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	job := jobWithSchedule()

	// The CronJob as a direct write would leave it: raised limits the CR knows
	// nothing about.
	edited := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name: job.Name, Namespace: job.Namespace, Labels: jobLabels(job),
		},
		Spec: batchv1.CronJobSpec{
			Schedule: job.Spec.Schedule,
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{Containers: []corev1.Container{{
							Name:  job.Name,
							Image: job.Spec.Image,
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("512Mi"),
									corev1.ResourceCPU:    resource.MustParse("500m"),
								},
							},
						}}},
					},
				},
			},
		},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(job, edited).Build()
	r := &JobReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.reconcileCronJob(ctx, job, ""))

	var got batchv1.CronJob
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: job.Name, Namespace: job.Namespace}, &got))
	limits := got.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Resources.Limits
	assert.Equal(t, "128Mi", limits.Memory().String(),
		"the reconcile rebuilds the template from the CR, so a direct edit is gone")
	assert.Equal(t, "100m", limits.Cpu().String())
}
