package cmd

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/controller/pkg/backupscope"
)

func resettableServer() *routedHost {
	return &routedHost{routes: map[string]string{
		"get crd backupstoragelocations.velero.io": "customresourcedefinition.apiextensions.k8s.io/backupstoragelocations.velero.io",
		"{.spec.objectStorage.bucket}":             "backups|https://objects.example.com||hel1|",
		"{.status.phase}|{.status.message}'":       "Available|",
		"get deploy velero":                        "1",
		"get pods -l name=velero":                  "pod/velero-abc",
		"get backups.velero.io -o":                 "daily-apps-20261031=Completed\nweekly-system-20261026=Completed",
		"get backuprepositories.velero.io":         "backuprepository.velero.io/a\nbackuprepository.velero.io/b\nbackuprepository.velero.io/c",
		"get cronjobs":                             "offsite|s3://objects.example.com/backups|s3://vault.example.net/locked|30|500",
		"chart-values-velero":                      "values values 1|12.0.0|r|velero|SHA256=H 1|12.0.0|r|velero|SHA256=H [\"install\"] |HelmChartValuesContent,",
		"helmchartconfig velero":                   `{"chart":"velero","repo":"r","version":"12.0.0","targetNamespace":"velero","createNamespace":true,"valuesContent":"x"}` + "\n",
	}}
}

func changedAnything(h *routedHost) bool {
	for _, cmd := range h.ran {
		if strings.Contains(cmd, "scale ") || strings.Contains(cmd, "delete ") || strings.Contains(cmd, "apply -f") {
			return true
		}
	}
	return false
}

func TestRepositoryKeyResetNeedsYesWithoutATerminal(t *testing.T) {
	h := resettableServer()

	err := resetRepositoryKeyOn(h, strings.NewReader(""), &bytes.Buffer{}, false, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--yes")
	assert.False(t, changedAnything(h))
}

func TestRepositoryKeyResetAsksForTheBucketName(t *testing.T) {
	h := resettableServer()
	var out bytes.Buffer

	err := resetRepositoryKeyOn(h, strings.NewReader("y\n"), &out, true, false)

	require.NoError(t, err)
	assert.False(t, changedAnything(h), "only the bucket's name confirms deleting every backup in it")
	assert.Contains(t, out.String(), "Nothing was changed")
}

func TestRepositoryKeyResetSaysWhatItDeletesAndWhatStays(t *testing.T) {
	h := resettableServer()
	var out bytes.Buffer

	_ = resetRepositoryKeyOn(h, strings.NewReader("no\n"), &out, true, false)

	text := out.String()
	assert.Contains(t, text, "bucket backups at https://objects.example.com: 2 backups and 3 volume repositories")
	assert.Contains(t, text, "Mirror offsite keeps the copies it made in s3://vault.example.net/locked")
	assert.Contains(t, text, "kip backup repository-key export", "the old key is the only way to read the mirrored copies")
	assert.Contains(t, text, "--max-delete limit of 500")
	assert.Contains(t, text, "at least 30 days", "a mirror's target may lock copies longer than the mirror asks")
}

func TestRepositoryKeyResetSaysWhenOldVersionsMayRemain(t *testing.T) {
	h := resettableServer()
	h.routes["get deploy velero"] = "0" // Stopped, so no pod has to go away.
	delete(h.routes, "get pods -l name=velero")
	h.routes["get job kipper-bucket-cleanup"] = "1/"
	h.routes["logs job/kipper-bucket-cleanup"] = "KIPPER_VERSIONS_CHECKED no\nKIPPER_OBJECTS_LEFT 0"
	h.routes["get backups.velero.io/"] = "Completed"
	h.routes["{.items[*].metadata.name}"] = "velero-abc"
	var out bytes.Buffer

	err := resetRepositoryKeyOn(h, strings.NewReader(""), &out, false, true)

	require.NoError(t, err)
	assert.Contains(t, out.String(), "could not check for older versions")
}

// outdatedScheduleHost serves one kip schedule that lacks the Dex exclusions
// until kip patches it.
type outdatedScheduleHost struct {
	*routedHost
	scoped bool
}

func (h *outdatedScheduleHost) Run(cmd string) (string, error) {
	switch {
	case strings.Contains(cmd, "patch schedules.velero.io/"):
		h.ran = append(h.ran, cmd)
		h.scoped = true
		return "", nil
	case strings.Contains(cmd, "managed-by=Helm -o name"):
		h.ran = append(h.ran, cmd)
		return "schedule.velero.io/weekly-system", nil
	case strings.Contains(cmd, "managed-by=Helm") && h.scoped:
		h.ran = append(h.ran, cmd)
		return `weekly-system=["` + strings.Join(backupscope.TransientResources(), `","`) + `"]`, nil
	case strings.Contains(cmd, "managed-by=Helm"):
		h.ran = append(h.ran, cmd)
		return `weekly-system=["certificaterequests.cert-manager.io"]`, nil
	}
	return h.routedHost.Run(cmd)
}

func (h *outdatedScheduleHost) RunStdin(cmd string, _ io.Reader) (string, error) { return h.Run(cmd) }

func TestRepositoryKeyResetFinishingOnOutdatedSchedulesSendsTheOperatorToUpgrade(t *testing.T) {
	h := &outdatedScheduleHost{routedHost: resettableServer()}
	delete(h.routes, "get deploy velero")
	h.routes["jsonpath='{.spec.replicas}'"] = "0"
	h.routes["stopped-by}"] = "repository-key-reset|0" // An unfinished reset stopped Velero, so kip upgrade skipped it.
	delete(h.routes, "get pods -l name=velero")
	h.routes["get job kipper-bucket-cleanup"] = "1/"
	h.routes["logs job/kipper-bucket-cleanup"] = "KIPPER_VERSIONS_CHECKED yes\nKIPPER_UPLOADS_CHECKED yes\nKIPPER_OBJECTS_LEFT 0"
	h.routes["{.items[*].metadata.name}"] = "velero-abc"
	var out bytes.Buffer

	err := resetRepositoryKeyOn(h, strings.NewReader(""), &out, false, true)

	require.NoError(t, err)
	text := out.String()
	assert.Contains(t, text, "Schedule weekly-system predates")
	assert.NotContains(t, text, "takes a full backup from each schedule")
	assert.Contains(t, text, "Run kip upgrade now")
	assert.Contains(t, text, "kip backup create", "the reset took no backup, so the cluster has none until a schedule runs")
	assert.False(t, h.ranContaining("backup create"))
	assert.True(t, h.scoped, "Velero's scheduler must not run the schedule before it excludes Dex's keys")
	assert.NotContains(t, text, "schedules back up Dex's signing keys and sessions")
}

func TestRepositoryKeyResetSaysWhenIncompleteUploadsMayRemain(t *testing.T) {
	h := resettableServer()
	h.routes["get deploy velero"] = "0"
	delete(h.routes, "get pods -l name=velero")
	h.routes["get job kipper-bucket-cleanup"] = "1/"
	h.routes["logs job/kipper-bucket-cleanup"] = "KIPPER_VERSIONS_CHECKED yes\nKIPPER_UPLOADS_CHECKED no\nKIPPER_OBJECTS_LEFT 0"
	h.routes["get backups.velero.io/"] = "Completed"
	h.routes["{.items[*].metadata.name}"] = "velero-abc"
	var out bytes.Buffer

	err := resetRepositoryKeyOn(h, strings.NewReader(""), &out, false, true)

	require.NoError(t, err)
	assert.Contains(t, out.String(), "could not check for incomplete uploads")
	assert.NotContains(t, out.String(), "older versions")
}
