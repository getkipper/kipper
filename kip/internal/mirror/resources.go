package mirror

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"

	"github.com/getkipper/kipper/controller/pkg/labels"
)

// Namespace and ObjectPrefix place mirror resources alongside the cluster's
// backups. Each mirror's Secret, CronJob and optional PrometheusRule share a name.
const (
	Namespace    = "velero"
	ObjectPrefix = "backup-mirror-"
	NameLabel    = "kipper.run/backup-mirror"
	SpecKey      = "mirror.json"
	DefaultImage = "ghcr.io/getkipper/kipper-mirror:latest"

	AnnotationSource    = "kipper.run/mirror-source"
	AnnotationTarget    = "kipper.run/mirror-target"
	AnnotationRetention = "kipper.run/mirror-retention-days"
	AnnotationMaxDelete = "kipper.run/mirror-max-delete"

	specDir     = "/etc/kipper-mirror"
	runAsUser   = 65532
	runDeadline = 6 * time.Hour
)

// PrometheusRuleGVR addresses the monitoring stack's alert rules.
var PrometheusRuleGVR = schema.GroupVersionResource{Group: "monitoring.coreos.com", Version: "v1", Resource: "prometheusrules"}

// ObjectName returns the name shared by a mirror's Secret, CronJob and rule.
func ObjectName(name string) string { return ObjectPrefix + name }

// Selector returns a Kubernetes label selector for the named mirror.
func Selector(name string) string { return NameLabel + "=" + name }

func objectLabels(name string) map[string]string {
	return map[string]string{
		labels.ManagedBy: labels.Kipper,
		NameLabel:        name,
		"app":            ObjectName(name),
	}
}

// Secret renders the Secret that holds the spec, keys included.
func Secret(spec Spec) (*corev1.Secret, error) {
	data, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: ObjectName(spec.Name), Namespace: Namespace, Labels: objectLabels(spec.Name)},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{SpecKey: data},
	}, nil
}

// CronJob renders the scheduled mirror run. The non-secret parts of the
// spec are repeated as annotations so status can show them without reading
// the Secret.
func CronJob(spec Spec, schedule, image string) *batchv1.CronJob {
	name := ObjectName(spec.Name)
	maxDelete := "off"
	if spec.MaxDelete >= 0 {
		maxDelete = strconv.Itoa(spec.MaxDelete)
	}
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: Namespace,
			Labels:    objectLabels(spec.Name),
			Annotations: map[string]string{
				AnnotationSource:    spec.Source.URL(),
				AnnotationTarget:    spec.Target.URL(),
				AnnotationRetention: strconv.Itoa(spec.RetentionDays),
				AnnotationMaxDelete: maxDelete,
			},
		},
		Spec: batchv1.CronJobSpec{
			Schedule:                   schedule,
			ConcurrencyPolicy:          batchv1.ForbidConcurrent,
			SuccessfulJobsHistoryLimit: ptr.To[int32](3),
			FailedJobsHistoryLimit:     ptr.To[int32](3),
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: objectLabels(spec.Name)},
				Spec: batchv1.JobSpec{
					BackoffLimit:          ptr.To[int32](2),
					ActiveDeadlineSeconds: ptr.To(int64(runDeadline.Seconds())),
					// A retry of a run stopped by --max-delete would start a
					// fresh deletion budget and could finish the deletion.
					PodFailurePolicy: &batchv1.PodFailurePolicy{Rules: []batchv1.PodFailurePolicyRule{{
						Action: batchv1.PodFailurePolicyActionFailJob,
						OnExitCodes: &batchv1.PodFailurePolicyOnExitCodesRequirement{
							ContainerName: ptr.To("mirror"),
							Operator:      batchv1.PodFailurePolicyOnExitCodesOpIn,
							Values:        []int32{FatalExitCode},
						},
					}}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: objectLabels(spec.Name)},
						Spec:       podSpec(name, image),
					},
				},
			},
		},
	}
}

func podSpec(secretName, image string) corev1.PodSpec {
	return corev1.PodSpec{
		RestartPolicy:                corev1.RestartPolicyNever,
		AutomountServiceAccountToken: ptr.To(false),
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot:   ptr.To(true),
			RunAsUser:      ptr.To[int64](runAsUser),
			RunAsGroup:     ptr.To[int64](runAsUser),
			FSGroup:        ptr.To[int64](runAsUser),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Containers: []corev1.Container{{
			Name:  "mirror",
			Image: image,
			Args:  []string{"backup", "mirror", "run", "--spec", specDir + "/" + SpecKey},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			},
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: ptr.To(false),
				ReadOnlyRootFilesystem:   ptr.To(true),
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "spec", MountPath: specDir, ReadOnly: true},
				{Name: "tmp", MountPath: "/tmp"},
			},
		}},
		Volumes: []corev1.Volume{
			{Name: "spec", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: secretName, DefaultMode: ptr.To[int32](0o440),
			}}},
			{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		},
	}
}

// FormatDuration formats positive whole hours as "48h" and other durations
// using time.Duration.String.
func FormatDuration(d time.Duration) string {
	if d > 0 && d%time.Hour == 0 {
		return fmt.Sprintf("%dh", d/time.Hour)
	}
	return d.String()
}

// AlertRule renders a PrometheusRule that fires when the mirror has not
// succeeded for twice its longest schedule interval, including a mirror that
// has never succeeded since it was added. The condition must hold for 15 minutes.
func AlertRule(name string, interval time.Duration) *unstructured.Unstructured {
	cronJob := ObjectName(name)
	selector := fmt.Sprintf(`{namespace=%q,cronjob=%q}`, Namespace, cronJob)
	threshold := int64((2 * interval).Seconds())
	expr := fmt.Sprintf(
		"(time() - kube_cronjob_status_last_successful_time%[1]s > %[2]d)"+
			" or (time() - kube_cronjob_created%[1]s > %[2]d unless on(namespace, cronjob) kube_cronjob_status_last_successful_time%[1]s)",
		selector, threshold)
	ruleLabels := map[string]any{}
	for k, v := range objectLabels(name) {
		ruleLabels[k] = v
	}
	ruleLabels["release"] = "kube-prometheus-stack"
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "monitoring.coreos.com/v1",
		"kind":       "PrometheusRule",
		"metadata": map[string]any{
			"name":      cronJob,
			"namespace": Namespace,
			"labels":    ruleLabels,
		},
		"spec": map[string]any{
			"groups": []any{map[string]any{
				"name": cronJob,
				"rules": []any{map[string]any{
					"alert": "KipperBackupMirrorStale",
					"expr":  expr,
					"for":   "15m",
					"labels": map[string]any{
						"severity": "warning",
					},
					"annotations": map[string]any{
						"summary": fmt.Sprintf("backup mirror %s has not succeeded for over %s", name, FormatDuration(2*interval)),
						"description": "The mirror's CronJob has no successful run within twice its schedule interval. " +
							"Run 'kip backup mirror status " + name + "' for the last run, its preflight result and the last error.",
					},
				}},
			}},
		},
	}}
}
