package cmd

import (
	"compress/gzip"
	"context"
	"crypto/md5" //nolint:gosec // S3 checks an SSE-C key against its MD5
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

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
	return describeBackup(ctx, k8sClient.Dynamic(), k8sClient.Clientset(), args[0], os.Stdout, &http.Client{Timeout: time.Minute})
}

// describeBackup prints the named backup. It downloads the error and warning
// messages only when Velero counted some. Failures to read volume backups or
// messages are printed in place of those details.
func describeBackup(ctx context.Context, dyn dynamic.Interface, cs kubernetes.Interface, name string, out io.Writer, httpClient *http.Client) error {
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
		var sseKey *locationSSEKey
		if sseKey, resultsErr = backupSSEKey(ctx, dyn, cs, *backup); resultsErr == nil {
			if r, resultsErr = fetchBackupResults(ctx, dyn, name, sseKey, httpClient); resultsErr == nil {
				results = &r
			}
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

// maxBackupResultsSize limits decompressed message bytes to bound memory use
// when reading a corrupt or unexpected object.
var maxBackupResultsSize int64 = 32 << 20

// downloadCleanupWait bounds the removal of kip's DownloadRequest.
const downloadCleanupWait = 10 * time.Second

// fetchBackupResults asks Velero for a link to the backup's error and warning
// messages and downloads them. It attempts to delete its DownloadRequest when
// done, allowing up to downloadCleanupWait even if ctx has ended. Cleanup errors
// are ignored. The HTTP client controls the download timeout. A non-nil sseKey
// supplies the decryption key and allowed hosts, requires HTTPS, and disables
// redirects. Downloads exceeding maxBackupResultsSize after decompression fail.
func fetchBackupResults(ctx context.Context, dyn dynamic.Interface, backup string, sseKey *locationSSEKey, httpClient *http.Client) (backupResults, error) {
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

	link, err := waitForDownloadURL(waitCtx, requests, name)
	if err != nil {
		return backupResults{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return backupResults{}, downloadError(err)
	}
	if sseKey != nil {
		if err := sseKey.checkDestination(req.URL); err != nil {
			return backupResults{}, err
		}
		sum := md5.Sum(sseKey.key) //nolint:gosec // S3 checks an SSE-C key against its MD5
		req.Header.Set("X-Amz-Server-Side-Encryption-Customer-Algorithm", "AES256")
		req.Header.Set("X-Amz-Server-Side-Encryption-Customer-Key", base64.StdEncoding.EncodeToString(sseKey.key))
		req.Header.Set("X-Amz-Server-Side-Encryption-Customer-Key-Md5", base64.StdEncoding.EncodeToString(sum[:]))
		keyed := *httpClient
		keyed.CheckRedirect = func(*http.Request, []*http.Request) error {
			return errors.New("the backup storage redirected the download, and kip does not follow a redirect with the SSE-C key")
		}
		httpClient = &keyed
	}
	resp, err := httpClient.Do(req) //nolint:gosec // the URL is Velero's presigned link to the backup storage
	if err != nil {
		return backupResults{}, downloadError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return backupResults{}, fmt.Errorf("the backup storage answered %s", resp.Status)
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return backupResults{}, fmt.Errorf("reading the messages: %w", err)
	}
	limited := &io.LimitedReader{R: zr, N: maxBackupResultsSize + 1}
	var results backupResults
	err = json.NewDecoder(limited).Decode(&results)
	if limited.N <= 0 {
		return backupResults{}, fmt.Errorf("the messages are larger than %d MiB, exceeding kip's read limit", maxBackupResultsSize>>20)
	}
	if err != nil {
		return backupResults{}, fmt.Errorf("reading the messages: %w", err)
	}
	return results, nil
}

// locationSSEKey is a backup location's SSE-C key and the hosts that may
// receive it.
type locationSSEKey struct {
	key   []byte
	hosts []string
}

// checkDestination returns an error unless u is an HTTPS link to one of the
// location's hosts. The key decrypts every backup, so it goes only over TLS and
// only to the storage the location names.
func (k *locationSSEKey) checkDestination(u *url.URL) error {
	if u.Scheme != "https" {
		return errors.New("the backup storage's download link is not HTTPS, so kip does not send it the SSE-C key")
	}
	if _, ok := keyHost(k.hosts[0]); !ok {
		return fmt.Errorf("the backup location's endpoint %s is not a plain ASCII host name, so kip does not send it the SSE-C key. Set the endpoint to its ASCII (punycode) spelling", k.hosts[0])
	}
	dest, ok := keyHost(u.Host)
	if !ok || !slices.ContainsFunc(k.hosts, func(h string) bool { allowed, ok := keyHost(h); return ok && allowed == dest }) {
		return fmt.Errorf("the backup storage's download link names %s, which is not the backup location's endpoint %s, so kip does not send it the SSE-C key", u.Host, k.hosts[0])
	}
	return nil
}

// keyHost returns host in the form the key check compares: lower case, without
// an empty port. Only ASCII hosts without an IPv6 zone qualify, because Unicode
// case folding and the IDNA mapping the HTTP client applies can disagree about
// which name is meant, and zone names are case-sensitive.
func keyHost(host string) (string, bool) {
	for i := 0; i < len(host); i++ {
		if host[i] >= utf8.RuneSelf || host[i] == '%' {
			return "", false
		}
	}
	return strings.ToLower(strings.TrimSuffix(host, ":")), true
}

// sseKeyHosts returns the allowed download hosts, starting with the s3Url host
// or the region's AWS S3 host when s3Url is unset. A configured bucket adds its
// subdomain unless s3ForcePathStyle is true. An invalid endpoint or missing
// endpoint and region returns an error.
func sseKeyHosts(bsl unstructured.Unstructured) ([]string, error) {
	config, _, _ := unstructured.NestedStringMap(bsl.Object, "spec", "config")
	var host string
	switch {
	case config["s3Url"] != "":
		u, err := url.Parse(config["s3Url"])
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("backup location %s has the endpoint %q, which kip cannot read, so it does not send the SSE-C key", bsl.GetName(), config["s3Url"])
		}
		host = u.Host
	case config["region"] != "":
		host = "s3." + config["region"] + ".amazonaws.com"
	default:
		return nil, fmt.Errorf("backup location %s sets neither s3Url nor region, so kip cannot tell which host may receive the SSE-C key", bsl.GetName())
	}
	hosts := []string{host}
	bucket, _, _ := unstructured.NestedString(bsl.Object, "spec", "objectStorage", "bucket")
	if pathStyle, _ := strconv.ParseBool(config["s3ForcePathStyle"]); !pathStyle && bucket != "" {
		hosts = append(hosts, bucket+"."+host)
	}
	return hosts, nil
}

// backupSSEKey returns the SSE-C key of the storage location a backup was
// written to, or nil when no key is configured. A missing location also returns
// nil, leaving the download to report any storage access problem.
func backupSSEKey(ctx context.Context, dyn dynamic.Interface, cs kubernetes.Interface, backup unstructured.Unstructured) (*locationSSEKey, error) {
	location, _, _ := unstructured.NestedString(backup.Object, "spec", "storageLocation")
	if location == "" {
		location = "default"
	}
	bsl, err := dyn.Resource(backupStorageLocationGVR).Namespace("velero").Get(ctx, location, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading backup location %s: %w", location, err)
	}
	ref, _, _ := unstructured.NestedString(bsl.Object, "spec", "config", "customerKeyEncryptionSecret")
	if ref == "" {
		return nil, nil
	}
	secretName, field, ok := strings.Cut(ref, "/")
	if !ok {
		return nil, fmt.Errorf("backup location %s names its SSE-C key as %q, which is not <secret>/<key>", location, ref)
	}
	hosts, err := sseKeyHosts(*bsl)
	if err != nil {
		return nil, err
	}
	secret, err := cs.CoreV1().Secrets("velero").Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading the SSE-C key in Secret velero/%s: %w", secretName, err)
	}
	key := secret.Data[field]
	if len(key) != 32 {
		return nil, fmt.Errorf("the SSE-C key in Secret velero/%s is not 32 bytes long", secretName)
	}
	return &locationSSEKey{key: key, hosts: hosts}, nil
}

// downloadError describes a failed download without the presigned link, whose
// query string grants read access to the messages until it expires. It keeps
// the storage host when the URL can be parsed, and the transport's reason.
func downloadError(err error) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return fmt.Errorf("downloading from the backup storage: %w", withoutQueries{err})
	}
	if u, parseErr := url.Parse(urlErr.URL); parseErr == nil && u.Host != "" {
		return fmt.Errorf("downloading from the backup storage at %s: %w", u.Host, withoutQueries{urlErr.Err})
	}
	return fmt.Errorf("downloading from the backup storage: %w", withoutQueries{urlErr.Err})
}

// urlQuery matches from a question mark to the next whitespace or double quote
// to redact URL queries in error messages.
var urlQuery = regexp.MustCompile(`\?[^\s"]*`)

// withoutQueries prints an error with every URL query string removed, because
// the HTTP client quotes a malformed redirect target in full and that target
// can carry the signature too.
type withoutQueries struct{ err error }

func (e withoutQueries) Error() string { return urlQuery.ReplaceAllString(e.err.Error(), "") }

func (e withoutQueries) Unwrap() error { return e.err }

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
