package mirror

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestSecretCarriesTheSpec(t *testing.T) {
	spec := validSpec()
	secret, err := Secret(spec)
	require.NoError(t, err)

	assert.Equal(t, "backup-mirror-uploads", secret.Name)
	assert.Equal(t, "velero", secret.Namespace)
	assert.Equal(t, "kipper", secret.Labels["app.kubernetes.io/managed-by"])
	assert.Equal(t, "uploads", secret.Labels[NameLabel])
	assert.Equal(t, corev1.SecretTypeOpaque, secret.Type)

	var stored Spec
	require.NoError(t, json.Unmarshal(secret.Data[SpecKey], &stored))
	assert.Equal(t, spec, stored)
}

func TestCronJobRunsTheMirrorLockedDown(t *testing.T) {
	cj := CronJob(validSpec(), "0 3 * * *", DefaultImage)

	assert.Equal(t, "backup-mirror-uploads", cj.Name)
	assert.Equal(t, "velero", cj.Namespace)
	assert.Equal(t, "0 3 * * *", cj.Spec.Schedule)
	assert.Equal(t, batchv1.ForbidConcurrent, cj.Spec.ConcurrencyPolicy)
	assert.Equal(t, int32(3), *cj.Spec.SuccessfulJobsHistoryLimit)
	assert.Equal(t, int32(3), *cj.Spec.FailedJobsHistoryLimit)
	assert.Equal(t, "s3://src.example.com/app", cj.Annotations[AnnotationSource])
	assert.Equal(t, "s3://dst.example.com/app-backup", cj.Annotations[AnnotationTarget])
	assert.Equal(t, "30", cj.Annotations[AnnotationRetention])
	assert.Equal(t, "off", cj.Annotations[AnnotationMaxDelete])

	job := cj.Spec.JobTemplate
	assert.Equal(t, "kipper", job.Labels["app.kubernetes.io/managed-by"], "the failed-job alert lists Jobs by this label")
	assert.Equal(t, "backup-mirror-uploads", job.Labels["app"], "the failed-job alert names the job by its app label")
	assert.Equal(t, "uploads", job.Labels[NameLabel])
	assert.Equal(t, int32(2), *job.Spec.BackoffLimit)
	assert.Equal(t, int64(6*60*60), *job.Spec.ActiveDeadlineSeconds)
	require.NotNil(t, job.Spec.PodFailurePolicy, "a fatal rclone error fails the Job instead of retrying")
	rule := job.Spec.PodFailurePolicy.Rules[0]
	assert.Equal(t, batchv1.PodFailurePolicyActionFailJob, rule.Action)
	assert.Equal(t, "mirror", *rule.OnExitCodes.ContainerName)
	assert.Equal(t, batchv1.PodFailurePolicyOnExitCodesOpIn, rule.OnExitCodes.Operator)
	assert.Equal(t, []int32{FatalExitCode}, rule.OnExitCodes.Values)

	pod := job.Spec.Template.Spec
	assert.Equal(t, corev1.RestartPolicyNever, pod.RestartPolicy)
	assert.False(t, *pod.AutomountServiceAccountToken)
	assert.True(t, *pod.SecurityContext.RunAsNonRoot)
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, pod.SecurityContext.SeccompProfile.Type)
	require.Len(t, pod.Containers, 1)
	c := pod.Containers[0]
	assert.Equal(t, DefaultImage, c.Image)
	assert.Equal(t, []string{"backup", "mirror", "run", "--spec", "/etc/kipper-mirror/mirror.json"}, c.Args)
	assert.True(t, *c.SecurityContext.ReadOnlyRootFilesystem)
	assert.False(t, *c.SecurityContext.AllowPrivilegeEscalation)
	assert.Equal(t, []corev1.Capability{"ALL"}, c.SecurityContext.Capabilities.Drop)
	for _, r := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		assert.Contains(t, c.Resources.Requests, r)
		assert.Contains(t, c.Resources.Limits, r)
	}
	assert.Empty(t, c.Env, "keys stay in the mounted Secret, out of the pod spec")

	var secretVolume *corev1.SecretVolumeSource
	for _, v := range pod.Volumes {
		if v.Secret != nil {
			secretVolume = v.Secret
		}
	}
	require.NotNil(t, secretVolume)
	assert.Equal(t, "backup-mirror-uploads", secretVolume.SecretName)
	assert.Equal(t, int32(0o440), *secretVolume.DefaultMode, "readable by the pod's fsGroup only")
	assert.Equal(t, *pod.SecurityContext.RunAsUser, *pod.SecurityContext.FSGroup)
}

func TestCronJobRecordsTheMaxDelete(t *testing.T) {
	spec := validSpec()
	spec.MaxDelete = 500
	assert.Equal(t, "500", CronJob(spec, "0 3 * * *", DefaultImage).Annotations[AnnotationMaxDelete])
}

func TestAlertRuleFiresAfterTwiceTheInterval(t *testing.T) {
	rule := AlertRule("uploads", 24*time.Hour)

	assert.Equal(t, "PrometheusRule", rule.GetKind())
	assert.Equal(t, "monitoring.coreos.com/v1", rule.GetAPIVersion())
	assert.Equal(t, "backup-mirror-uploads", rule.GetName())
	assert.Equal(t, "velero", rule.GetNamespace())
	assert.Equal(t, "kube-prometheus-stack", rule.GetLabels()["release"], "the stack only loads rules with its release label")

	groups, _, _ := unstructured.NestedSlice(rule.Object, "spec", "groups")
	require.Len(t, groups, 1)
	rules := groups[0].(map[string]any)["rules"].([]any)
	require.Len(t, rules, 1)
	r := rules[0].(map[string]any)
	assert.Equal(t, "KipperBackupMirrorStale", r["alert"])
	expr := r["expr"].(string)
	assert.Contains(t, expr, `kube_cronjob_status_last_successful_time{namespace="velero",cronjob="backup-mirror-uploads"}`)
	assert.Contains(t, expr, "> 172800", "48 hours for a daily schedule")
	assert.Contains(t, expr, `kube_cronjob_created{namespace="velero",cronjob="backup-mirror-uploads"}`, "a mirror that never succeeded alerts too")
}

func TestFormatDuration(t *testing.T) {
	assert.Equal(t, "48h", FormatDuration(48*time.Hour))
	assert.Equal(t, "1h30m0s", FormatDuration(90*time.Minute))
}
