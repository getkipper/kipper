package cmd

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/kip/internal/velerolabel"
)

var downloadRequestGVR = schema.GroupVersionResource{Group: "velero.io", Version: "v1", Resource: "downloadrequests"}

var backupDescribeCmd = &cobra.Command{
	Use:   "describe <backup-name>",
	Short: "Show a backup's outcome, its volume backups, and its errors and warnings",
	Long: `Shows how a backup ended: its status, timing, items, the file-system backup of each
volume, and the error and warning messages Velero recorded.

The messages are stored alongside the backup. kip asks Velero for a short-lived
download link and fetches them from this machine, so the backup storage must be
reachable from here. In-cluster MinIO is reachable only within the cluster. If kip
cannot fetch the messages, it reports the problem and shows the remaining details.`,
	Args: cobra.ExactArgs(1),
	RunE: runBackupDescribe,
}

func init() {
	backupCmd.AddCommand(backupDescribeCmd)
}

func runBackupDescribe(cmd *cobra.Command, args []string) error {
	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	ctx := context.Background()
	if !backupsConfiguredOn(ctx, k8sClient.Dynamic()) {
		fmt.Printf("\n  %s\n\n", platform.BackupsNotConfigured)
		return nil
	}
	return describeBackup(ctx, k8sClient.Dynamic(), args[0], os.Stdout, &http.Client{Timeout: time.Minute})
}

// describeBackup prints the named backup. It downloads the error and warning
// messages only when Velero counted some. Failures to read volume backups or
// messages are printed in place of those details.
func describeBackup(ctx context.Context, dyn dynamic.Interface, name string, out io.Writer, httpClient *http.Client) error {
	backup, err := dyn.Resource(backupGVR).Namespace("velero").Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("there is no backup named %q. Run kip backup list to see available backups", name)
	}
	if err != nil {
		return fmt.Errorf("reading backup %s: %w", name, err)
	}

	var pvbs []unstructured.Unstructured
	list, pvbErr := dyn.Resource(podVolumeBackupGVR).Namespace("velero").List(ctx, metav1.ListOptions{
		LabelSelector: "velero.io/backup-name=" + velerolabel.ValidName(name),
	})
	if pvbErr == nil {
		pvbs = list.Items
	}

	var results *backupResults
	var resultsErr error
	if backupCount(*backup, "errors")+backupCount(*backup, "warnings") > 0 {
		var r backupResults
		if r, resultsErr = fetchBackupResults(ctx, dyn, name, httpClient); resultsErr == nil {
			results = &r
		}
	}
	writeBackupDescription(out, *backup, pvbs, pvbErr, results, resultsErr)
	return nil
}

// backupResultSet holds messages the way Velero stores them: its own, the
// cluster-scoped ones, and those for each namespace.
type backupResultSet struct {
	Velero     []string            `json:"velero,omitempty"`
	Cluster    []string            `json:"cluster,omitempty"`
	Namespaces map[string][]string `json:"namespaces,omitempty"`
}

type backupResults struct {
	Errors   backupResultSet `json:"errors"`
	Warnings backupResultSet `json:"warnings"`
}

// downloadPollInterval spaces the reads of a DownloadRequest while Velero
// prepares its link.
var downloadPollInterval = 500 * time.Millisecond

// downloadWait bounds the creation of a DownloadRequest and the wait for
// Velero's link, including API calls that stall.
var downloadWait = 30 * time.Second

// downloadCleanupWait bounds the removal of kip's DownloadRequest.
const downloadCleanupWait = 10 * time.Second

// fetchBackupResults asks Velero for a link to the backup's error and warning
// messages and downloads them. It attempts to delete its DownloadRequest when
// done, allowing up to downloadCleanupWait even if ctx has ended. Cleanup errors
// are ignored. The HTTP client controls the download timeout.
func fetchBackupResults(ctx context.Context, dyn dynamic.Interface, backup string, httpClient *http.Client) (backupResults, error) {
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return backupResults{}, err
	}
	requests := dyn.Resource(downloadRequestGVR).Namespace("velero")
	name := "kip-describe-" + hex.EncodeToString(suffix)
	waitCtx, cancel := context.WithTimeout(ctx, downloadWait)
	defer cancel()
	_, err := requests.Create(waitCtx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "velero.io/v1",
		"kind":       "DownloadRequest",
		"metadata":   map[string]any{"name": name, "namespace": "velero"},
		"spec":       map[string]any{"target": map[string]any{"kind": "BackupResults", "name": backup}},
	}}, metav1.CreateOptions{})
	if err != nil {
		return backupResults{}, fmt.Errorf("asking Velero for the messages: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), downloadCleanupWait)
		defer cancel()
		_ = requests.Delete(cleanupCtx, name, metav1.DeleteOptions{})
	}()

	url, err := waitForDownloadURL(waitCtx, requests, name)
	if err != nil {
		return backupResults{}, err
	}
	resp, err := httpClient.Get(url) //nolint:gosec // the URL is Velero's presigned link to the backup storage
	if err != nil {
		return backupResults{}, fmt.Errorf("downloading from the backup storage: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return backupResults{}, fmt.Errorf("the backup storage answered %s", resp.Status)
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return backupResults{}, fmt.Errorf("reading the messages: %w", err)
	}
	var results backupResults
	if err := json.NewDecoder(zr).Decode(&results); err != nil {
		return backupResults{}, fmt.Errorf("reading the messages: %w", err)
	}
	return results, nil
}

// waitForDownloadURL polls until the DownloadRequest is processed with a
// non-empty URL. API errors and context cancellation return an error.
func waitForDownloadURL(ctx context.Context, requests dynamic.ResourceInterface, name string) (string, error) {
	gaveUp := fmt.Errorf("velero did not provide a download link within %s", downloadWait)
	for {
		req, err := requests.Get(ctx, name, metav1.GetOptions{})
		if ctx.Err() != nil {
			return "", gaveUp
		}
		if err != nil {
			return "", fmt.Errorf("reading the download request: %w", err)
		}
		phase, _, _ := unstructured.NestedString(req.Object, "status", "phase")
		url, _, _ := unstructured.NestedString(req.Object, "status", "downloadURL")
		if phase == "Processed" && url != "" {
			return url, nil
		}
		select {
		case <-ctx.Done():
			return "", gaveUp
		case <-time.After(downloadPollInterval):
		}
	}
}

// writeBackupDescription prints a backup, its volume backups and, when given,
// its messages. Read errors replace the affected details. It sorts pvbs in place
// by namespace, pod and volume for display.
func writeBackupDescription(out io.Writer, backup unstructured.Unstructured, pvbs []unstructured.Unstructured, pvbErr error, results *backupResults, resultsErr error) {
	field := func(path ...string) string {
		v, _, _ := unstructured.NestedString(backup.Object, path...)
		return v
	}
	say(out, "\n  Backup:     %s\n", backup.GetName())
	if s := backup.GetLabels()["velero.io/schedule-name"]; s != "" {
		say(out, "  Schedule:   %s\n", s)
	}
	errs, warnings := backupCount(backup, "errors"), backupCount(backup, "warnings")
	status := field("status", "phase")
	if errs+warnings > 0 {
		status += fmt.Sprintf(" (%d %s, %d %s)", errs, plural(int(errs), "error", "errors"), warnings, plural(int(warnings), "warning", "warnings"))
	}
	say(out, "  Status:     %s\n", status)
	if reason := field("status", "failureReason"); reason != "" {
		say(out, "  Failure:    %s\n", reason)
	}
	validation, _, _ := unstructured.NestedStringSlice(backup.Object, "status", "validationErrors")
	for _, v := range validation {
		say(out, "  Validation: %s\n", v)
	}
	for _, t := range []struct{ label, path string }{{"Started", "startTimestamp"}, {"Completed", "completionTimestamp"}, {"Expires", "expiration"}} {
		if v := field("status", t.path); v != "" {
			say(out, "  %-11s %s\n", t.label+":", backupTime(v))
		}
	}
	say(out, "  Namespaces: %s\n", backupNamespaces(backup))
	done, _, _ := unstructured.NestedInt64(backup.Object, "status", "progress", "itemsBackedUp")
	total, _, _ := unstructured.NestedInt64(backup.Object, "status", "progress", "totalItems")
	if total > 0 {
		say(out, "  Items:      %d of %d backed up\n", done, total)
	}

	switch {
	case pvbErr != nil:
		say(out, "\n  Volume backups: not read (%v)\n", pvbErr)
	case len(pvbs) > 0:
		say(out, "\n  Volume backups:\n")
		sort.Slice(pvbs, func(i, j int) bool { return podVolumeLabel(pvbs[i]) < podVolumeLabel(pvbs[j]) })
		for _, p := range pvbs {
			phase, _, _ := unstructured.NestedString(p.Object, "status", "phase")
			bytesDone, _, _ := unstructured.NestedInt64(p.Object, "status", "progress", "bytesDone")
			switch phase {
			case "Completed":
				say(out, "    ✔  %s  %s\n", podVolumeLabel(p), humanVolumeSize(bytesDone))
			case "Failed":
				message, _, _ := unstructured.NestedString(p.Object, "status", "message")
				say(out, "    ✗  %s  Failed: %s\n", podVolumeLabel(p), message)
			default:
				say(out, "    ⚠  %s  %s\n", podVolumeLabel(p), phase)
			}
		}
	}

	switch {
	case resultsErr != nil:
		say(out, "\n  kip could not read the error and warning messages: %v\n", resultsErr)
	case results != nil:
		writeBackupResultSet(out, "Errors", results.Errors)
		writeBackupResultSet(out, "Warnings", results.Warnings)
	}
	say(out, "\n")
}

func writeBackupResultSet(out io.Writer, title string, set backupResultSet) {
	if len(set.Velero)+len(set.Cluster)+len(set.Namespaces) == 0 {
		return
	}
	say(out, "\n  %s:\n", title)
	for _, m := range set.Velero {
		say(out, "    Velero: %s\n", m)
	}
	for _, m := range set.Cluster {
		say(out, "    cluster: %s\n", m)
	}
	namespaces := make([]string, 0, len(set.Namespaces))
	for ns := range set.Namespaces {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)
	for _, ns := range namespaces {
		for _, m := range set.Namespaces[ns] {
			say(out, "    %s: %s\n", ns, m)
		}
	}
}

func podVolumeLabel(pvb unstructured.Unstructured) string {
	ns, _, _ := unstructured.NestedString(pvb.Object, "spec", "pod", "namespace")
	pod, _, _ := unstructured.NestedString(pvb.Object, "spec", "pod", "name")
	volume, _, _ := unstructured.NestedString(pvb.Object, "spec", "volume")
	return ns + "/" + pod + " " + volume
}

func backupNamespaces(backup unstructured.Unstructured) string {
	if ns, ok, _ := unstructured.NestedStringSlice(backup.Object, "spec", "includedNamespaces"); ok && len(ns) > 0 && !slices.Equal(ns, []string{"*"}) {
		return strings.Join(ns, ", ")
	}
	if ex, ok, _ := unstructured.NestedStringSlice(backup.Object, "spec", "excludedNamespaces"); ok && len(ex) > 0 {
		// Distinguish namespace exclusions from an unrestricted backup.
		return "all except system"
	}
	return "all"
}

func backupCount(backup unstructured.Unstructured, field string) int64 {
	n, _, _ := unstructured.NestedInt64(backup.Object, "status", field)
	return n
}

func backupTime(rfc3339 string) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}
