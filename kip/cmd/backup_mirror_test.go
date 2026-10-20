package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/kip/internal/mirror"
)

func writeCredentials(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func testMirrorFlags(t *testing.T) mirrorFlags {
	creds := writeCredentials(t, "[default]\naws_access_key_id = AKSRC\naws_secret_access_key = SKSRC\n\n[mirror]\naws_access_key_id = AKDST\naws_secret_access_key = SKDST\n")
	return mirrorFlags{
		source: "s3://src.example.com/app", sourceRegion: "r1", sourceCredentials: creds, sourceProfile: "default",
		target: "s3://dst.example.com/app-backup", targetRegion: "r2", targetCredentials: creds, targetProfile: "mirror",
		retentionDays: 30, maxDelete: -1, schedule: "0 3 * * *",
	}
}

func TestMirrorFlagsBuildTheSpec(t *testing.T) {
	spec, interval, err := testMirrorFlags(t).spec("uploads")
	require.NoError(t, err)
	assert.Equal(t, 24*time.Hour, interval)
	assert.Equal(t, mirror.Spec{
		Name:          "uploads",
		Source:        mirror.Remote{Endpoint: "src.example.com", Bucket: "app", Region: "r1", AccessKeyID: "AKSRC", SecretAccessKey: "SKSRC"},
		Target:        mirror.Remote{Endpoint: "dst.example.com", Bucket: "app-backup", Region: "r2", AccessKeyID: "AKDST", SecretAccessKey: "SKDST"},
		RetentionDays: 30,
		MaxDelete:     -1,
	}, spec)
}

func TestMirrorFlagsRefuseBadInput(t *testing.T) {
	cases := map[string]func(*mirrorFlags){
		"bad source":          func(f *mirrorFlags) { f.source = "https://src.example.com/app" },
		"bad target":          func(f *mirrorFlags) { f.target = "s3://dst.example.com/app/prefix" },
		"no region":           func(f *mirrorFlags) { f.targetRegion = "" },
		"missing profile":     func(f *mirrorFlags) { f.targetProfile = "absent" },
		"missing file":        func(f *mirrorFlags) { f.sourceCredentials = "/nonexistent/credentials" },
		"no retention":        func(f *mirrorFlags) { f.retentionDays = 0 },
		"bad schedule":        func(f *mirrorFlags) { f.schedule = "daily" },
		"bad image tag":       func(f *mirrorFlags) { f.imageTag = "latest" },
		"bad max-delete":      func(f *mirrorFlags) { f.maxDelete = -2 },
		"invalid mirror name": nil,
	}
	for name, mutate := range cases {
		f := testMirrorFlags(t)
		mirrorName := "uploads"
		if mutate == nil {
			mirrorName = "Uploads"
		} else {
			mutate(&f)
		}
		_, _, err := f.spec(mirrorName)
		assert.Error(t, err, name)
	}
}

func TestMirrorImage(t *testing.T) {
	assert.Equal(t, "ghcr.io/getkipper/kipper-mirror:latest", mirrorImage(""))
	sha := strings.Repeat("a", 40)
	assert.Equal(t, "ghcr.io/getkipper/kipper-mirror:"+sha, mirrorImage(sha))
}

var prometheusRuleListKind = map[schema.GroupVersionResource]string{mirror.PrometheusRuleGVR: "PrometheusRuleList"}

func testMirrorSpec() mirror.Spec {
	return mirror.Spec{
		Name:          "uploads",
		Source:        mirror.Remote{Endpoint: "src.example.com", Bucket: "app", Region: "r1", AccessKeyID: "AK1", SecretAccessKey: "SK1"},
		Target:        mirror.Remote{Endpoint: "dst.example.com", Bucket: "app-backup", Region: "r2", AccessKeyID: "AK2", SecretAccessKey: "SK2"},
		RetentionDays: 30,
		MaxDelete:     -1,
	}
}

func TestCreateMirrorWritesSecretCronJobAndRule(t *testing.T) {
	cs := fake.NewClientset()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), prometheusRuleListKind)
	var out bytes.Buffer
	require.NoError(t, createMirror(context.Background(), cs, dyn, testMirrorSpec(), "0 3 * * *", 24*time.Hour, mirror.DefaultImage, &out))

	ctx := context.Background()
	secret, err := cs.CoreV1().Secrets("velero").Get(ctx, "backup-mirror-uploads", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Contains(t, string(secret.Data[mirror.SpecKey]), "SK2")
	cj, err := cs.BatchV1().CronJobs("velero").Get(ctx, "backup-mirror-uploads", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "0 3 * * *", cj.Spec.Schedule)
	_, err = dyn.Resource(mirror.PrometheusRuleGVR).Namespace("velero").Get(ctx, "backup-mirror-uploads", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Contains(t, out.String(), "backup-mirror-uploads")
	assert.Contains(t, out.String(), "no successful run within 48h\n")
}

func TestCreateMirrorWithoutMonitoringWarnsAndContinues(t *testing.T) {
	cs := fake.NewClientset()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), prometheusRuleListKind)
	dyn.PrependReactor("create", "prometheusrules", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(mirror.PrometheusRuleGVR.GroupResource(), "")
	})
	var out bytes.Buffer
	require.NoError(t, createMirror(context.Background(), cs, dyn, testMirrorSpec(), "0 3 * * *", 24*time.Hour, mirror.DefaultImage, &out))
	assert.Contains(t, out.String(), "no monitoring stack")
	_, err := cs.BatchV1().CronJobs("velero").Get(context.Background(), "backup-mirror-uploads", metav1.GetOptions{})
	require.NoError(t, err)
}

func TestCreateMirrorRemovesTheSecretWhenTheCronJobFails(t *testing.T) {
	cs := fake.NewClientset()
	cs.PrependReactor("create", "cronjobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "cronjobs"}, "x", nil)
	})
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), prometheusRuleListKind)
	err := createMirror(context.Background(), cs, dyn, testMirrorSpec(), "0 3 * * *", 24*time.Hour, mirror.DefaultImage, &bytes.Buffer{})
	require.Error(t, err)
	_, err = cs.CoreV1().Secrets("velero").Get(context.Background(), "backup-mirror-uploads", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "no Secret with keys is left behind without its CronJob")
}

func TestMirrorExistsChecksSecretAndCronJob(t *testing.T) {
	ctx := context.Background()
	assert.NoError(t, mirrorAbsent(ctx, fake.NewClientset(), "uploads"))

	secret, err := mirror.Secret(testMirrorSpec())
	require.NoError(t, err)
	assert.Error(t, mirrorAbsent(ctx, fake.NewClientset(secret), "uploads"))
	assert.Error(t, mirrorAbsent(ctx, fake.NewClientset(mirror.CronJob(testMirrorSpec(), "0 3 * * *", mirror.DefaultImage)), "uploads"))
}

func TestRemoveMirrorDeletesOnlyItsObjects(t *testing.T) {
	spec := testMirrorSpec()
	secret, err := mirror.Secret(spec)
	require.NoError(t, err)
	other := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cloud-credentials", Namespace: "velero"}}
	cs := fake.NewClientset(secret, mirror.CronJob(spec, "0 3 * * *", mirror.DefaultImage), other)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), prometheusRuleListKind, mirror.AlertRule("uploads", 24*time.Hour))

	ctx := context.Background()
	var out bytes.Buffer
	require.NoError(t, removeMirror(ctx, cs, dyn, "uploads", &out))

	_, err = cs.CoreV1().Secrets("velero").Get(ctx, "backup-mirror-uploads", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
	_, err = cs.BatchV1().CronJobs("velero").Get(ctx, "backup-mirror-uploads", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
	_, err = dyn.Resource(mirror.PrometheusRuleGVR).Namespace("velero").Get(ctx, "backup-mirror-uploads", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
	_, err = cs.CoreV1().Secrets("velero").Get(ctx, "cloud-credentials", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Contains(t, out.String(), "Both buckets and their contents are untouched")
}

func TestRemoveMirrorRefusesAnUnknownMirror(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), prometheusRuleListKind)
	err := removeMirror(context.Background(), fake.NewClientset(), dyn, "absent", &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no backup mirror named")
}

var statusNow = time.Date(2026, time.October, 20, 8, 0, 0, 0, time.UTC)

func mirrorJob(name string, started time.Time, cond batchv1.JobConditionType, reason string) *batchv1.Job {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "velero",
			Labels:            map[string]string{mirror.NameLabel: "uploads"},
			CreationTimestamp: metav1.NewTime(started),
		},
		Status: batchv1.JobStatus{StartTime: ptrTime(started)},
	}
	if cond != "" {
		job.Status.Conditions = []batchv1.JobCondition{{Type: cond, Status: corev1.ConditionTrue, Reason: reason}}
	} else {
		job.Status.Active = 1
	}
	return job
}

func ptrTime(t time.Time) *metav1.Time { mt := metav1.NewTime(t); return &mt }

func mirrorPod(job string, created time.Time, message string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: job + "-" + created.Format("150405"), Namespace: "velero",
			Labels:            map[string]string{mirror.NameLabel: "uploads"},
			CreationTimestamp: metav1.NewTime(created),
			OwnerReferences:   []metav1.OwnerReference{{Kind: "Job", Name: job}},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "mirror",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: message}},
		}}},
	}
}

func statusCronJob(lastSchedule, lastSuccess *time.Time) *batchv1.CronJob {
	cj := mirror.CronJob(testMirrorSpec(), "0 3 * * *", mirror.DefaultImage)
	cj.CreationTimestamp = metav1.NewTime(statusNow.Add(-72 * time.Hour))
	if lastSchedule != nil {
		cj.Status.LastScheduleTime = ptrTime(*lastSchedule)
	}
	if lastSuccess != nil {
		cj.Status.LastSuccessfulTime = ptrTime(*lastSuccess)
	}
	return cj
}

func statusOutput(t *testing.T, name string, objects ...runtime.Object) string {
	t.Helper()
	var out bytes.Buffer
	require.NoError(t, showMirrorStatus(context.Background(), fake.NewClientset(objects...), name, statusNow, &out))
	return out.String()
}

func TestMirrorStatusShowsTheLastRun(t *testing.T) {
	run := statusNow.Add(-5 * time.Hour)
	sum := mirror.Summary{
		Started: run, Preflight: "passed", Check: "passed",
		Sync: &mirror.Stats{Bytes: 3 << 20, Checks: 1200, Transfers: 12, Deletes: 2, ElapsedSeconds: 12.5},
	}
	out := statusOutput(t, "uploads",
		statusCronJob(&run, &run),
		mirrorJob("backup-mirror-uploads-1", run, batchv1.JobComplete, ""),
		mirrorJob("backup-mirror-uploads-0", run.Add(-24*time.Hour), batchv1.JobFailed, "BackoffLimitExceeded"),
		mirrorPod("backup-mirror-uploads-1", run, string(sum.Encode())),
	)

	for _, want := range []string{
		"Backup mirror uploads",
		"s3://src.example.com/app",
		"s3://dst.example.com/app-backup",
		"30 days, compliance lock",
		"Max delete    off",
		"0 3 * * *",
		"Last run      2026-10-20 03:00 UTC (5h ago), succeeded",
		"Last success  5h ago",
		"Preflight     passed",
		"Sync          12 copied (3 MiB), 2 deleted, 1200 checked, 0 errors in 12s",
		"Check         passed",
	} {
		assert.Contains(t, out, want)
	}
}

func TestMirrorStatusShowsAFailedPreflight(t *testing.T) {
	run := statusNow.Add(-5 * time.Hour)
	success := statusNow.Add(-29 * time.Hour)
	sum := mirror.Summary{Started: run, Preflight: "failed: target object lock: the default retention is 7 days"}
	out := statusOutput(t, "uploads",
		statusCronJob(&run, &success),
		mirrorJob("backup-mirror-uploads-1", run, batchv1.JobFailed, "BackoffLimitExceeded"),
		mirrorPod("backup-mirror-uploads-1", run, "garbage from an earlier attempt"),
		mirrorPod("backup-mirror-uploads-1", run.Add(time.Minute), string(sum.Encode())),
	)
	assert.Contains(t, out, "Last run      2026-10-20 03:00 UTC (5h ago), failed (BackoffLimitExceeded)")
	assert.Contains(t, out, "Last success  1d ago")
	assert.Contains(t, out, "Preflight     failed: target object lock: the default retention is 7 days")
	assert.NotContains(t, out, "Sync ")
}

func TestMirrorStatusBeforeTheFirstRun(t *testing.T) {
	out := statusOutput(t, "uploads", statusCronJob(nil, nil))
	assert.Contains(t, out, "Last run      not yet")
	assert.Contains(t, out, "Last success  never")
}

func TestMirrorStatusShowsARunInProgress(t *testing.T) {
	run := statusNow.Add(-10 * time.Minute)
	out := statusOutput(t, "uploads", statusCronJob(&run, nil), mirrorJob("backup-mirror-uploads-1", run, "", ""))
	assert.Contains(t, out, "Running       for 10m")
}

func TestMirrorStatusWithoutSummary(t *testing.T) {
	run := statusNow.Add(-5 * time.Hour)
	out := statusOutput(t, "uploads", statusCronJob(&run, nil), mirrorJob("backup-mirror-uploads-1", run, batchv1.JobFailed, "DeadlineExceeded"))
	assert.Contains(t, out, "Preflight     unknown, the run left no summary")
}

func TestMirrorStatusListsEveryMirror(t *testing.T) {
	other := testMirrorSpec()
	other.Name = "media"
	out := statusOutput(t, "", statusCronJob(nil, nil), mirror.CronJob(other, "0 4 * * *", mirror.DefaultImage))
	assert.Contains(t, out, "Backup mirror uploads")
	assert.Contains(t, out, "Backup mirror media")
}

func TestMirrorStatusUnknownName(t *testing.T) {
	err := showMirrorStatus(context.Background(), fake.NewClientset(), "absent", statusNow, &bytes.Buffer{})
	require.Error(t, err)

	var out bytes.Buffer
	require.NoError(t, showMirrorStatus(context.Background(), fake.NewClientset(), "", statusNow, &out))
	assert.Contains(t, out.String(), "No backup mirrors")
}

func TestRunMirrorJobWritesTheSummaryOnFailure(t *testing.T) {
	spec := testMirrorSpec()
	spec.Target.Endpoint = spec.Source.Endpoint
	secret, err := mirror.Secret(spec)
	require.NoError(t, err)
	dir := t.TempDir()
	specPath := filepath.Join(dir, "mirror.json")
	summaryPath := filepath.Join(dir, "termination-log")
	require.NoError(t, os.WriteFile(specPath, secret.Data[mirror.SpecKey], 0o600))

	var log bytes.Buffer
	err = runMirrorJob(context.Background(), specPath, summaryPath, &log)
	require.Error(t, err)

	data, err := os.ReadFile(summaryPath)
	require.NoError(t, err)
	sum, err := mirror.DecodeSummary(data)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(sum.Preflight, "failed: endpoints differ"), sum.Preflight)
	assert.NotContains(t, log.String(), "SK1")
}

func TestMirrorExitCode(t *testing.T) {
	assert.Equal(t, 0, mirrorExitCode(nil))
	assert.Equal(t, 1, mirrorExitCode(errors.New("preflight failed")))
	assert.Equal(t, mirror.FatalExitCode, mirrorExitCode(fmt.Errorf("rclone sync: %w", mirror.ErrFatal)))
}
