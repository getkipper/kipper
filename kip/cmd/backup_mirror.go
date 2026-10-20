package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/mirror"
)

var backupMirrorCmd = &cobra.Command{
	Use:   "mirror",
	Short: "Copy an S3 bucket daily into a bucket protected by Object Lock",
	Long: `A backup mirror copies one S3 bucket, such as the bucket an app stores its
uploads in, into a second bucket at another endpoint once per schedule. The
second bucket must be versioned and carry an Object Lock default retention in
compliance mode, so nothing the mirror writes can be deleted early, not even
with the mirror's own key.

Before every run the mirror checks that this protection is in place and stops
with an error if it is not. See the Backups page in the Kipper docs for the
bucket setup and the restore procedure.`,
}

var backupMirrorAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Check a source and a locked target bucket, then schedule the mirror",
	Long: `Checks both buckets from this machine. Once all checks pass, it creates a
Secret holding both keys and a CronJob that runs the mirror, then attempts
to add an alert for a mirror that stops succeeding. All three
live in the velero namespace.

The checks are: the endpoints differ, versioning is enabled on the source,
the target has an Object Lock default retention in compliance mode of at
least --retention-days, a one-byte canary written to the target is locked
that long, deleting the canary's version is refused, and the source key is
denied writing, deleting and deleting a version of the canary key in the
source. The canary stays in the target until its lock expires.

Credentials files use the AWS format. The same file and profile may be given
for both sides when one key reads the source and writes the target.`,
	Example: `  kip backup mirror add uploads \
    --source s3://eu-1.objects.example.com/app-uploads --source-region eu-1 \
    --target s3://eu-2.objects.example.com/app-uploads-backup --target-region eu-2 \
    --source-credentials ~/.aws/mirror --target-credentials ~/.aws/mirror \
    --retention-days 30`,
	Args: cobra.ExactArgs(1),
	RunE: runBackupMirrorAdd,
}

var backupMirrorStatusCmd = &cobra.Command{
	Use:   "status [name]",
	Short: "Show a mirror's last run, preflight result and copy totals",
	Long: `Shows the schedule, the last run and its outcome, the time since the last
successful run, the preflight result and the totals of the last sync. Without
a name, every mirror is shown.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runBackupMirrorStatus,
}

var backupMirrorRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Stop a mirror and delete its keys from the cluster",
	Long: `Deletes the mirror's CronJob, its Secret and its alert. Both buckets and
everything in them stay as they are.`,
	Args: cobra.ExactArgs(1),
	RunE: runBackupMirrorRemove,
}

var backupMirrorRunCmd = &cobra.Command{
	Use:          "run",
	Short:        "Run one mirror pass inside the mirror's Job",
	Hidden:       true,
	SilenceUsage: true,
	Args:         cobra.NoArgs,
	RunE:         runBackupMirrorRun,
}

func init() {
	f := backupMirrorAddCmd.Flags()
	f.String("source", "", "bucket to copy, as s3://endpoint/bucket (required)")
	f.String("source-region", "", "region of the source bucket, as the provider names it (required)")
	f.String("source-credentials", "", "AWS-style credentials file with the key that reads the source (required)")
	f.String("source-profile", "default", "profile within --source-credentials")
	f.String("target", "", "locked bucket to copy into, as s3://endpoint/bucket (required)")
	f.String("target-region", "", "region of the target bucket, as the provider names it (required)")
	f.String("target-credentials", "", "AWS-style credentials file with the key that writes the target (required)")
	f.String("target-profile", "default", "profile within --target-credentials")
	f.Int("retention-days", 0, "minimum default retention of the target bucket, in days (required)")
	f.String("schedule", "0 3 * * *", "cron schedule of the mirror runs, in the cluster's time zone")
	f.Int("max-delete", -1, "maximum objects one run may delete from the target. Exceeding it fails the run without a retry. Use -1 to disable the limit")
	f.String("image-tag", "", "run the mirror image built from this commit (full 40-character SHA) instead of the released one")
	for _, name := range []string{"source", "source-region", "source-credentials", "target", "target-region", "target-credentials", "retention-days"} {
		_ = backupMirrorAddCmd.MarkFlagRequired(name)
	}

	backupMirrorRunCmd.Flags().String("spec", "", "path of the mirror spec")
	backupMirrorRunCmd.Flags().String("summary", "/dev/termination-log", "file to write the run summary to")
	_ = backupMirrorRunCmd.MarkFlagRequired("spec")

	backupMirrorCmd.AddCommand(backupMirrorAddCmd, backupMirrorStatusCmd, backupMirrorRemoveCmd, backupMirrorRunCmd)
	backupCmd.AddCommand(backupMirrorCmd)
}

type mirrorFlags struct {
	source, sourceRegion, sourceCredentials, sourceProfile string
	target, targetRegion, targetCredentials, targetProfile string
	retentionDays, maxDelete                               int
	schedule, imageTag                                     string
}

func mirrorFlagsFrom(cmd *cobra.Command) mirrorFlags {
	f := cmd.Flags()
	var m mirrorFlags
	m.source, _ = f.GetString("source")
	m.sourceRegion, _ = f.GetString("source-region")
	m.sourceCredentials, _ = f.GetString("source-credentials")
	m.sourceProfile, _ = f.GetString("source-profile")
	m.target, _ = f.GetString("target")
	m.targetRegion, _ = f.GetString("target-region")
	m.targetCredentials, _ = f.GetString("target-credentials")
	m.targetProfile, _ = f.GetString("target-profile")
	m.retentionDays, _ = f.GetInt("retention-days")
	m.maxDelete, _ = f.GetInt("max-delete")
	m.schedule, _ = f.GetString("schedule")
	m.imageTag, _ = f.GetString("image-tag")
	return m
}

// spec builds the mirror's spec from the flags, reading both keys, and
// returns the longest interval between two scheduled runs.
func (m mirrorFlags) spec(name string) (mirror.Spec, time.Duration, error) {
	if m.maxDelete < -1 {
		return mirror.Spec{}, 0, errors.New("--max-delete must be -1 (off) or a number of objects")
	}
	if m.imageTag != "" {
		if err := installer.ValidateImageTag(m.imageTag); err != nil {
			return mirror.Spec{}, 0, err
		}
	}
	interval, err := mirror.ScheduleInterval(m.schedule)
	if err != nil {
		return mirror.Spec{}, 0, err
	}
	source, err := mirrorRemote("source", m.source, m.sourceRegion, m.sourceCredentials, m.sourceProfile)
	if err != nil {
		return mirror.Spec{}, 0, err
	}
	target, err := mirrorRemote("target", m.target, m.targetRegion, m.targetCredentials, m.targetProfile)
	if err != nil {
		return mirror.Spec{}, 0, err
	}
	spec := mirror.Spec{Name: name, Source: source, Target: target, RetentionDays: m.retentionDays, MaxDelete: m.maxDelete}
	if err := spec.Validate(); err != nil {
		return mirror.Spec{}, 0, err
	}
	return spec, interval, nil
}

func mirrorRemote(side, rawURL, region, credsPath, profile string) (mirror.Remote, error) {
	endpoint, bucket, err := mirror.ParseURL(rawURL)
	if err != nil {
		return mirror.Remote{}, fmt.Errorf("--%s: %w", side, err)
	}
	if region == "" {
		return mirror.Remote{}, fmt.Errorf("--%s-region is required", side)
	}
	if strings.HasPrefix(credsPath, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return mirror.Remote{}, fmt.Errorf("expanding ~ in --%s-credentials: %w", side, err)
		}
		credsPath = filepath.Join(home, credsPath[2:])
	}
	creds, err := installer.LoadAWSCredentials(credsPath, profile)
	if err != nil {
		return mirror.Remote{}, fmt.Errorf("--%s-credentials: %w", side, err)
	}
	return mirror.Remote{
		Endpoint: endpoint, Bucket: bucket, Region: region,
		AccessKeyID: creds.AccessKeyID, SecretAccessKey: creds.SecretAccessKey,
	}, nil
}

// mirrorImage returns the released mirror image, or the one built from tag.
func mirrorImage(tag string) string {
	if tag == "" {
		return mirror.DefaultImage
	}
	return strings.TrimSuffix(mirror.DefaultImage, ":latest") + ":" + tag
}

func runBackupMirrorAdd(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true
	flags := mirrorFlagsFrom(cmd)
	spec, interval, err := flags.spec(args[0])
	if err != nil {
		return err
	}
	_, client, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := mirrorAbsent(ctx, client.Clientset(), spec.Name); err != nil {
		return err
	}

	source, err := mirror.NewStore(spec.Source)
	if err != nil {
		return err
	}
	target, err := mirror.NewStore(spec.Target)
	if err != nil {
		return err
	}
	fmt.Printf("\n  Checking %s and %s...\n\n", spec.Source.URL(), spec.Target.URL())
	res := mirror.Preflight(ctx, spec, source, target, time.Now())
	printPreflight(os.Stdout, res)
	if !res.Passed() {
		f := res.Failure()
		return fmt.Errorf("the mirror was not created because the %s check failed: %s", f.Name, f.Detail)
	}
	return createMirror(ctx, client.Clientset(), client.Dynamic(), spec, flags.schedule, interval, mirrorImage(flags.imageTag), os.Stdout)
}

func printPreflight(out io.Writer, res mirror.Result) {
	for _, c := range res.Checks {
		mark := "✔"
		if !c.Passed {
			mark = "✖"
		}
		_, _ = fmt.Fprintf(out, "  %s  %s: %s\n", mark, c.Name, c.Detail)
	}
	_, _ = fmt.Fprintln(out)
}

// mirrorAbsent refuses a name that already has a Secret or a CronJob, because
// the retention of an existing mirror is never changed in place.
func mirrorAbsent(ctx context.Context, cs kubernetes.Interface, name string) error {
	objName := mirror.ObjectName(name)
	exists := errors.New("a backup mirror named " + name + " already exists. Remove it with 'kip backup mirror remove " + name + "' first")
	if _, err := cs.BatchV1().CronJobs(mirror.Namespace).Get(ctx, objName, metav1.GetOptions{}); err == nil {
		return exists
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("checking for an existing mirror: %w", err)
	}
	if _, err := cs.CoreV1().Secrets(mirror.Namespace).Get(ctx, objName, metav1.GetOptions{}); err == nil {
		return exists
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("checking for an existing mirror: %w", err)
	}
	return nil
}

// createMirror creates the Secret and CronJob, then attempts to add the alert.
// If CronJob creation fails, Secret cleanup is best effort. Alert creation
// failures produce warnings and leave the mirror running.
func createMirror(ctx context.Context, cs kubernetes.Interface, dyn dynamic.Interface, spec mirror.Spec, schedule string, interval time.Duration, image string, out io.Writer) error {
	secret, err := mirror.Secret(spec)
	if err != nil {
		return err
	}
	if _, err := cs.CoreV1().Secrets(mirror.Namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("the %s namespace does not exist on this cluster: %w", mirror.Namespace, err)
		}
		return fmt.Errorf("creating Secret %s: %w", secret.Name, err)
	}
	cronJob := mirror.CronJob(spec, schedule, image)
	if _, err := cs.BatchV1().CronJobs(mirror.Namespace).Create(ctx, cronJob, metav1.CreateOptions{}); err != nil {
		_ = cs.CoreV1().Secrets(mirror.Namespace).Delete(ctx, secret.Name, metav1.DeleteOptions{})
		return fmt.Errorf("creating CronJob %s: %w", cronJob.Name, err)
	}
	_, _ = fmt.Fprintf(out, "  ✔  Created Secret and CronJob %s in %s, schedule %q\n", cronJob.Name, mirror.Namespace, schedule)

	rule := mirror.AlertRule(spec.Name, interval)
	_, err = dyn.Resource(mirror.PrometheusRuleGVR).Namespace(mirror.Namespace).Create(ctx, rule, metav1.CreateOptions{})
	switch {
	case err == nil:
		_, _ = fmt.Fprintf(out, "  ✔  Added alert KipperBackupMirrorStale for no successful run within %s\n", mirror.FormatDuration(2*interval))
	case apierrors.IsNotFound(err):
		_, _ = fmt.Fprintln(out, "  ⚠  This cluster runs no monitoring stack, so no alert watches for a mirror that stops running. A failed run still raises a failed-job alert.")
	default:
		_, _ = fmt.Fprintf(out, "  ⚠  The alert for a mirror that stops running was not added: %v\n", err)
	}
	_, _ = fmt.Fprintf(out, "\n  Check the first run with 'kip backup mirror status %s'.\n\n", spec.Name)
	return nil
}

func runBackupMirrorRemove(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true
	_, client, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return removeMirror(ctx, client.Clientset(), client.Dynamic(), args[0], os.Stdout)
}

// removeMirror deletes the mirror's CronJob, Secret and alert rule, and
// never touches either bucket.
func removeMirror(ctx context.Context, cs kubernetes.Interface, dyn dynamic.Interface, name string, out io.Writer) error {
	if err := mirror.ValidateName(name); err != nil {
		return err
	}
	objName := mirror.ObjectName(name)
	found := false
	background := metav1.DeletePropagationBackground
	err := cs.BatchV1().CronJobs(mirror.Namespace).Delete(ctx, objName, metav1.DeleteOptions{PropagationPolicy: &background})
	if err == nil {
		found = true
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting CronJob %s: %w", objName, err)
	}
	err = cs.CoreV1().Secrets(mirror.Namespace).Delete(ctx, objName, metav1.DeleteOptions{})
	if err == nil {
		found = true
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting Secret %s: %w", objName, err)
	}
	err = dyn.Resource(mirror.PrometheusRuleGVR).Namespace(mirror.Namespace).Delete(ctx, objName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		_, _ = fmt.Fprintf(out, "  ⚠  The alert rule %s was not deleted: %v\n", objName, err)
	}
	if !found {
		return fmt.Errorf("there is no backup mirror named %s", name)
	}
	_, _ = fmt.Fprintf(out, "\n  ✔  Removed backup mirror %s. Both buckets and their contents are untouched.\n\n", name)
	return nil
}

func runBackupMirrorStatus(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true
	_, client, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	name := ""
	if len(args) == 1 {
		name = args[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return showMirrorStatus(ctx, client.Clientset(), name, time.Now(), os.Stdout)
}

// showMirrorStatus prints the named mirror, or every mirror when name is
// empty. The run details come from the newest finished Job and the summary
// its pod left as a termination message.
func showMirrorStatus(ctx context.Context, cs kubernetes.Interface, name string, now time.Time, out io.Writer) error {
	selector := mirror.NameLabel
	if name != "" {
		if err := mirror.ValidateName(name); err != nil {
			return err
		}
		selector = mirror.Selector(name)
	}
	list := metav1.ListOptions{LabelSelector: selector}
	cronJobs, err := cs.BatchV1().CronJobs(mirror.Namespace).List(ctx, list)
	if err != nil {
		return fmt.Errorf("listing backup mirrors: %w", err)
	}
	if len(cronJobs.Items) == 0 {
		if name != "" {
			return fmt.Errorf("there is no backup mirror named %s", name)
		}
		_, _ = fmt.Fprintln(out, "\n  No backup mirrors. Add one with 'kip backup mirror add'.")
		_, _ = fmt.Fprintln(out)
		return nil
	}
	jobs, err := cs.BatchV1().Jobs(mirror.Namespace).List(ctx, list)
	if err != nil {
		return fmt.Errorf("listing mirror runs: %w", err)
	}
	pods, err := cs.CoreV1().Pods(mirror.Namespace).List(ctx, list)
	if err != nil {
		return fmt.Errorf("listing mirror pods: %w", err)
	}
	sort.Slice(cronJobs.Items, func(i, j int) bool { return cronJobs.Items[i].Name < cronJobs.Items[j].Name })
	for i := range cronJobs.Items {
		writeMirrorStatus(out, &cronJobs.Items[i], jobs.Items, pods.Items, now)
	}
	return nil
}

func writeMirrorStatus(out io.Writer, cj *batchv1.CronJob, jobs []batchv1.Job, pods []corev1.Pod, now time.Time) {
	name := cj.Labels[mirror.NameLabel]
	line := func(label, value string) { _, _ = fmt.Fprintf(out, "  %-13s %s\n", label, value) }

	_, _ = fmt.Fprintf(out, "\n  Backup mirror %s\n\n", name)
	line("Source", cj.Annotations[mirror.AnnotationSource])
	line("Target", cj.Annotations[mirror.AnnotationTarget])
	line("Retention", "at least "+cj.Annotations[mirror.AnnotationRetention]+" days, compliance lock")
	line("Max delete", cj.Annotations[mirror.AnnotationMaxDelete])
	schedule := cj.Spec.Schedule
	if cj.Spec.Suspend != nil && *cj.Spec.Suspend {
		schedule += " (suspended)"
	}
	line("Schedule", schedule)

	var finished, running *batchv1.Job
	for i := range jobs {
		job := &jobs[i]
		if job.Labels[mirror.NameLabel] != name {
			continue
		}
		if _, done := jobOutcome(job); !done {
			if running == nil || job.CreationTimestamp.After(running.CreationTimestamp.Time) {
				running = job
			}
			continue
		}
		if finished == nil || job.CreationTimestamp.After(finished.CreationTimestamp.Time) {
			finished = job
		}
	}
	if running != nil {
		line("Running", "for "+humanAge(now, jobStart(running)))
	}
	if finished == nil {
		line("Last run", "not yet")
	} else {
		outcome, _ := jobOutcome(finished)
		started := jobStart(finished)
		line("Last run", fmt.Sprintf("%s (%s ago), %s", started.UTC().Format("2006-01-02 15:04 UTC"), humanAge(now, started), outcome))
	}
	if cj.Status.LastSuccessfulTime == nil {
		line("Last success", "never")
	} else {
		line("Last success", humanAge(now, cj.Status.LastSuccessfulTime.Time)+" ago")
	}
	if finished != nil {
		writeRunSummary(line, runSummary(finished.Name, pods))
	}
	_, _ = fmt.Fprintln(out)
}

func writeRunSummary(line func(label, value string), sum *mirror.Summary) {
	if sum == nil {
		line("Preflight", "unknown, the run left no summary")
		return
	}
	line("Preflight", sum.Preflight)
	if sum.Sync != nil {
		s := sum.Sync
		elapsed := time.Duration(s.ElapsedSeconds * float64(time.Second)).Truncate(time.Second)
		line("Sync", fmt.Sprintf("%d copied (%s), %d deleted, %d checked, %d errors in %s",
			s.Transfers, humanBytes(s.Bytes), s.Deletes, s.Checks, s.Errors, elapsed))
	}
	if sum.SyncError != "" {
		line("Sync error", sum.SyncError)
	}
	if sum.Check != "" {
		line("Check", sum.Check)
	}
}

// jobOutcome describes a finished Job and reports whether it has finished.
func jobOutcome(job *batchv1.Job) (string, bool) {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return "succeeded", true
		case batchv1.JobFailed:
			if c.Reason != "" {
				return "failed (" + c.Reason + ")", true
			}
			return "failed", true
		}
	}
	return "", false
}

func jobStart(job *batchv1.Job) time.Time {
	if job.Status.StartTime != nil {
		return job.Status.StartTime.Time
	}
	return job.CreationTimestamp.Time
}

// runSummary returns the summary written by the newest of the Job's pods
// that left one.
func runSummary(jobName string, pods []corev1.Pod) *mirror.Summary {
	var best *mirror.Summary
	var bestAt time.Time
	for i := range pods {
		pod := &pods[i]
		if !ownedByJob(pod, jobName) || (best != nil && !pod.CreationTimestamp.After(bestAt)) {
			continue
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name != "mirror" || cs.State.Terminated == nil {
				continue
			}
			if sum, err := mirror.DecodeSummary([]byte(cs.State.Terminated.Message)); err == nil {
				best, bestAt = &sum, pod.CreationTimestamp.Time
			}
		}
	}
	return best
}

func ownedByJob(pod *corev1.Pod, jobName string) bool {
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "Job" && ref.Name == jobName {
			return true
		}
	}
	return false
}

func runBackupMirrorRun(cmd *cobra.Command, _ []string) error {
	specPath, _ := cmd.Flags().GetString("spec")
	summaryPath, _ := cmd.Flags().GetString("summary")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := runMirrorJob(ctx, specPath, summaryPath, os.Stderr)
	stop()
	if code := mirrorExitCode(err); code == mirror.FatalExitCode {
		_, _ = fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(code)
	}
	return err
}

// mirrorExitCode returns 0 for success, FatalExitCode for a run the Job must
// not retry, and 1 for any other failure.
func mirrorExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, mirror.ErrFatal):
		return mirror.FatalExitCode
	}
	return 1
}

// runMirrorJob loads and validates the spec, constructs the stores and runs
// one mirror pass. Once the pass returns, it attempts to write its summary
// even on failure. Summary write failures are logged.
func runMirrorJob(ctx context.Context, specPath, summaryPath string, log io.Writer) error {
	data, err := os.ReadFile(specPath) //nolint:gosec // the path is the mounted Secret named in the CronJob
	if err != nil {
		return fmt.Errorf("reading the mirror spec: %w", err)
	}
	var spec mirror.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return fmt.Errorf("reading the mirror spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return fmt.Errorf("the mirror spec is invalid: %w", err)
	}
	source, err := mirror.NewStore(spec.Source)
	if err != nil {
		return err
	}
	target, err := mirror.NewStore(spec.Target)
	if err != nil {
		return err
	}
	sum, runErr := mirror.Run(ctx, spec, source, target, execRclone, log, time.Now().UTC())
	if err := os.WriteFile(summaryPath, sum.Encode(), 0o600); err != nil {
		_, _ = fmt.Fprintf(log, "writing the run summary: %v\n", err)
	}
	return runErr
}

func execRclone(ctx context.Context, args, env []string, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "rclone", args...) //nolint:gosec // a fixed binary with arguments built from the validated spec
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = stderr
	cmd.Stderr = stderr
	return cmd.Run()
}
