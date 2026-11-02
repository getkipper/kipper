package installer

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/config"
)

const (
	moveOldBucket = "old-backups"
	moveNewBucket = "new-backups"
	moveEndpoint  = "https://objects.example.com"
	moveNewCreds  = "AKIDNEW"
)

var moveNow = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

// fakeMoveHost simulates the velero namespace for a backup storage move: its
// Secrets, the record ConfigMap, the backup location, the Velero server's
// pods and the check Jobs. A chart apply moves the location at once, and the
// location is Available when it has the target's credentials and one server replica.
type fakeMoveHost struct {
	secrets  map[string]map[string][]byte
	record   map[string]string
	bucket   string
	s3URL    string
	sse      bool
	readOnly bool
	prefix   string
	creds    string

	velero       int
	inFlight     []string // Lines of the in-flight listing, as "Kind/name phase".
	busyOnStop   []string
	jobs         string
	backups      string
	populated    bool
	sseWriteFail bool
	resetStopped bool
	locations    int // Additional BackupStorageLocations besides default.

	failOn       string // A command containing this fails, once.
	generated    int
	generateWeak string
	cmds         []string
}

func newFakeMoveHost(liveKey string) *fakeMoveHost {
	return &fakeMoveHost{
		secrets: map[string]map[string][]byte{repositoryKeySecret: {repositoryKeyField: []byte(liveKey)}},
		record:  map[string]string{},
		bucket:  moveOldBucket, creds: "AKIDOLD", velero: 1,
	}
}

func (h *fakeMoveHost) Run(cmd string) (string, error) { return h.RunStdin(cmd, nil) }

var secretNamePattern = regexp.MustCompile(`secret(?: generic)? ([a-z0-9-]+)`)

func (h *fakeMoveHost) RunStdin(cmd string, in io.Reader) (string, error) {
	h.cmds = append(h.cmds, cmd)
	if h.failOn != "" && strings.Contains(cmd, h.failOn) {
		h.failOn = ""
		return "", errors.New("connection lost")
	}
	var input []byte
	if in != nil {
		input, _ = io.ReadAll(in)
	}
	name := ""
	if m := secretNamePattern.FindStringSubmatch(cmd); m != nil {
		name = m[1]
	}
	switch {
	case strings.Contains(cmd, "helmchartconfig velero"):
		return `{"chart":"velero","repo":"r","version":"12.0.0","targetNamespace":"velero","createNamespace":true,"valuesContent":"x"}` + "\n", nil
	case strings.Contains(cmd, "chart-values-velero"):
		return "values values 1|12.0.0|r|velero|SHA256=H 1|12.0.0|r|velero|SHA256=H [\"install\"] |HelmChartValuesContent,", nil
	case strings.Contains(cmd, "get configmap "+StorageRecordConfigMap):
		if len(h.record) == 0 {
			return "", nil
		}
		b, _ := json.Marshal(h.record)
		return string(b), nil
	case cmd == "kubectl apply -f -":
		var cm struct {
			Data map[string]string `json:"data"`
		}
		if err := json.Unmarshal(input, &cm); err != nil {
			return "", err
		}
		h.record = cm.Data
		if h.record == nil {
			h.record = map[string]string{}
		}
	case strings.Contains(cmd, "get secret") && strings.Contains(cmd, "-o name"):
		if _, ok := h.secrets[name]; ok {
			return "secret/" + name, nil
		}
		return "", nil
	case strings.Contains(cmd, "get secret") && strings.Contains(cmd, "jsonpath"):
		for field, v := range h.secrets[name] {
			if strings.Contains(cmd, "{.data."+field+"}") {
				return base64.StdEncoding.EncodeToString(v), nil
			}
		}
		return "", nil
	case strings.Contains(cmd, "delete secret"):
		for _, n := range strings.Fields(strings.SplitN(strings.TrimPrefix(cmd, "kubectl -n velero delete secret "), " --", 2)[0]) {
			delete(h.secrets, n)
		}
	case strings.Contains(cmd, "/dev/urandom"):
		if _, ok := h.secrets[name]; ok {
			return "", errors.New("AlreadyExists")
		}
		h.generated++
		if name == sseKeySecret {
			h.secrets[name] = map[string][]byte{sseKeyField: bytes.Repeat([]byte(fmt.Sprint(h.generated%10)), sseKeyBytes)}
		} else {
			h.secrets[name] = map[string][]byte{repositoryKeyField: []byte(fmt.Sprintf("generated-key-%02d-%s", h.generated, strings.Repeat("x", 26)))}
			if h.generateWeak != "" {
				h.secrets[name] = map[string][]byte{repositoryKeyField: []byte(h.generateWeak)}
			}
		}
	case strings.Contains(cmd, "create secret generic cloud-credentials"):
		h.creds = strings.Fields(strings.SplitN(string(input), "aws_access_key_id = ", 2)[1])[0]
	case strings.Contains(cmd, "create secret generic") && strings.Contains(cmd, "kubectl create -f -"):
		if _, ok := h.secrets[name]; ok {
			return "", errors.New("AlreadyExists")
		}
		h.secrets[name] = map[string][]byte{repositoryKeyField: input}
	case strings.Contains(cmd, "create secret generic") && strings.Contains(cmd, "kubectl apply -f -") && name == repositoryKeySecret:
		h.secrets[name] = map[string][]byte{repositoryKeyField: input}
	case strings.Contains(cmd, "get crd backupstoragelocations.velero.io"):
		return "customresourcedefinition.apiextensions.k8s.io/backupstoragelocations.velero.io", nil
	case strings.Contains(cmd, "get backupstoragelocation default") && strings.Contains(cmd, ".status.phase"):
		if h.bucket == moveNewBucket && h.creds == moveNewCreds && h.velero == 1 {
			return "Available|", nil
		}
		return "Unavailable|rpc error: access denied", nil
	case strings.Contains(cmd, "get backupstoragelocation default"):
		sse, mode := "", ""
		if h.sse {
			sse = sseKeySecret + "/" + sseKeyField
		}
		if h.readOnly {
			mode = "ReadOnly"
		}
		return strings.Join([]string{h.bucket, h.s3URL, sse, "eu-1", h.prefix, mode}, "|"), nil
	case strings.Contains(cmd, "get backups.velero.io,restores.velero.io"):
		return strings.Join(h.inFlight, "\n"), nil
	case strings.Contains(cmd, "get jobs -l velero.io/repo-name"):
		return h.jobs, nil
	case strings.Contains(cmd, "get backups.velero.io -o"):
		return h.backupListing(strings.Contains(cmd, ".status.startTimestamp")), nil
	case strings.Contains(cmd, "scale deploy/velero --replicas=0"):
		h.velero = 0
		h.inFlight = append(h.inFlight, h.busyOnStop...)
	case strings.Contains(cmd, "scale deploy/velero --replicas=1"):
		h.velero = 1
	case strings.Contains(cmd, "get pods"):
		return strings.Repeat("velero-6f9 ReplicaSet velero-6f\n", h.velero) + "node-agent-x DaemonSet node-agent\n", nil
	case strings.Contains(cmd, "delete backups.velero.io"):
		h.inFlight = nil
	case cmd == osReleaseCommand:
		return "ID=ubuntu\n", nil
	case strings.Contains(cmd, "get backupstoragelocations.velero.io -o name"):
		return strings.Repeat("backupstoragelocation.velero.io/x\n", h.locations+1), nil
	case strings.Contains(cmd, `kipper\.run/stopped-by`):
		if h.resetStopped {
			return veleroStoppedByReset + "|0", nil
		}
		return fmt.Sprintf("|%d", h.velero), nil
	case strings.Contains(cmd, "kind: HelmChart"):
		h.bucket = regexp.MustCompile(`bucket: (\S+)`).FindStringSubmatch(cmd)[1]
		h.s3URL = ""
		if m := regexp.MustCompile(`s3Url: (\S+)`).FindStringSubmatch(cmd); m != nil {
			h.s3URL = m[1]
		}
		h.sse = strings.Contains(cmd, "customerKeyEncryptionSecret")
	case strings.Contains(cmd, "get job "):
		return "1/", nil
	case strings.Contains(cmd, "logs job/"+bucketCheckJob):
		if h.populated {
			return bucketPopulatedMarker, nil
		}
		return bucketEmptyMarker, nil
	case strings.Contains(cmd, "logs job/"+sseCheckJob):
		if h.sseWriteFail {
			return "refused", nil
		}
		return sseWriteOKMarker, nil
	}
	return "", nil
}

// backupListing formats h.backups, whose lines contain "phase created started",
// with the timestamp requested by the command. A start time of "-" becomes empty.
func (h *fakeMoveHost) backupListing(started bool) string {
	var out strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(h.backups), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		at := f[1]
		if started {
			at = strings.TrimPrefix(f[2], "-")
		}
		out.WriteString(f[0] + " " + at + "\n")
	}
	return out.String()
}

func (h *fakeMoveHost) ran(sub string) bool {
	for _, c := range h.cmds {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

func (h *fakeMoveHost) key(name string) string {
	return string(h.secrets[name][repositoryKeyField])
}

func (h *fakeMoveHost) storageRecord(t *testing.T) StorageRecord {
	t.Helper()
	rec, err := ParseStorageRecord(h.record)
	require.NoError(t, err)
	return rec
}

type moveRun struct {
	opts      MoveOptions
	confirmed int
	out       bytes.Buffer
}

func newMoveRun(t *testing.T, sseC bool) *moveRun {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, config.Save(&config.Config{Clusters: []config.Cluster{{Name: "prod",
		BackupStorage: &config.BackupStorageRef{Mode: "external", Bucket: moveOldBucket, Region: "eu-west-1"}}}}))
	dir := t.TempDir()
	r := &moveRun{}
	r.opts = MoveOptions{
		Target: &BackupStorageConfig{Bucket: moveNewBucket, Region: "eu-1", Endpoint: moveEndpoint,
			AccessKeyID: moveNewCreds, SecretAccessKey: "s3cret", SSEC: sseC},
		ClusterName:     "prod",
		NewKeyFile:      filepath.Join(dir, "new.key"),
		PreviousKeyFile: filepath.Join(dir, "previous.key"),
		Confirm:         func([]string) error { r.confirmed++; return nil },
		Out:             &r.out,
		Now:             func() time.Time { return moveNow },
	}
	if sseC {
		r.opts.SSECKeyFile = filepath.Join(dir, "sse.key")
	}
	return r
}

func fastMove(t *testing.T) {
	t.Helper()
	wait, poll := moveWait, backupMovePoll
	moveWait, backupMovePoll = 50*time.Millisecond, time.Millisecond
	t.Cleanup(func() { moveWait, backupMovePoll = wait, poll })
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

func TestMoveBackupStorageMovesADefaultKeyClusterToAFreshKey(t *testing.T) {
	fastMove(t)
	h := newFakeMoveHost("static-passw0rd")
	r := newMoveRun(t, false)

	require.NoError(t, MoveBackupStorage(h, r.opts))

	newKey := h.key(repositoryKeySecret)
	assert.False(t, IsDefaultRepositoryKey([]byte(newKey)), "the new location never gets the published key")
	assert.Equal(t, newKey, readFile(t, r.opts.NewKeyFile))
	assert.Equal(t, "static-passw0rd", readFile(t, r.opts.PreviousKeyFile), "the old bucket's key is on disk for a recovery install")
	assert.Equal(t, "static-passw0rd", h.key(previousKeySecret))
	assert.NotContains(t, h.secrets, nextKeySecret, "the staged key goes once the move is done")
	assert.Equal(t, 1, r.confirmed)

	assert.Equal(t, moveNewBucket, h.bucket)
	assert.Equal(t, moveNewCreds, h.creds)
	assert.True(t, h.ran("rollout restart deploy/velero"), "the server must restart to read the new credentials")
	assert.True(t, h.ran("rollout restart ds/node-agent"), "the node agents must restart to read the new credentials")

	rec := h.storageRecord(t)
	assert.Nil(t, rec.Move)
	require.NotNil(t, rec.Current)
	assert.Equal(t, moveNewBucket, rec.Current.Bucket)
	require.NotNil(t, rec.Previous)
	assert.Equal(t, moveOldBucket, rec.Previous.Bucket)
	assert.Equal(t, moveNow.Format(time.RFC3339), rec.Previous.SwitchedAt)

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, moveNewBucket, cfg.GetCluster("prod").BackupStorage.Bucket)
	assert.Equal(t, moveEndpoint, cfg.GetCluster("prod").BackupStorage.Endpoint)
}

func TestMoveBackupStorageSwapsTheKeyOnlyAfterVeleroStoppedAndItsRecordsAreGone(t *testing.T) {
	fastMove(t)
	h := newFakeMoveHost("own-key-of-this-cluster")
	r := newMoveRun(t, false)

	require.NoError(t, MoveBackupStorage(h, r.opts))

	order := func(sub string) int {
		for i, c := range h.cmds {
			if strings.Contains(c, sub) {
				return i
			}
		}
		t.Fatalf("%q never ran", sub)
		return -1
	}
	assert.Less(t, order("logs job/"+bucketCheckJob), order("create secret generic "+previousKeySecret), "the new bucket is checked before anything is staged")
	assert.Less(t, order("--replicas=0"), order("delete backups.velero.io"))
	assert.Less(t, order("delete backups.velero.io"), order("create secret generic "+repositoryKeySecret))
	assert.Less(t, order("create secret generic "+repositoryKeySecret), order("kind: HelmChart"), "Velero switches to the new bucket only with the new key")
	assert.Less(t, order("create secret generic cloud-credentials"), order("kind: HelmChart"))
	assert.Equal(t, "own-key-of-this-cluster", readFile(t, r.opts.PreviousKeyFile))
}

func TestMoveBackupStorageWithSSECGeneratesTestsAndExportsTheKey(t *testing.T) {
	fastMove(t)
	h := newFakeMoveHost("static-passw0rd")
	r := newMoveRun(t, true)

	require.NoError(t, MoveBackupStorage(h, r.opts))

	key := h.secrets[sseKeySecret][sseKeyField]
	require.Len(t, key, sseKeyBytes)
	got, err := ReadSSECKeyFile(r.opts.SSECKeyFile)
	require.NoError(t, err)
	assert.Equal(t, key, got)
	assert.True(t, h.ran("logs job/"+sseCheckJob), "the key is tested on the new bucket before the switch")
	assert.True(t, h.sse, "Velero's location uses the key")
}

func TestMoveBackupStorageRefusesBeforeChangingAnything(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*fakeMoveHost, *moveRun)
		want  string
	}{
		{"populated target", func(h *fakeMoveHost, _ *moveRun) { h.populated = true }, "already holds backups"},
		{"running backup", func(h *fakeMoveHost, _ *moveRun) { h.inFlight = []string{"Backup/daily InProgress"} }, "in progress"},
		{"maintenance job", func(h *fakeMoveHost, _ *moveRun) { h.jobs = "repo-maint-1 1\n" }, "in progress"},
		{"new deletion request", func(h *fakeMoveHost, _ *moveRun) { h.inFlight = []string{"DeleteBackupRequest/daily-x New"} }, "in progress"},
		{"unread deletion request", func(h *fakeMoveHost, _ *moveRun) { h.inFlight = []string{"DeleteBackupRequest/daily-x"} }, "in progress"},
		{"read-only location", func(h *fakeMoveHost, _ *moveRun) { h.readOnly = true }, "read-only"},
		{"in-cluster MinIO", func(h *fakeMoveHost, _ *moveRun) { h.s3URL = inClusterMinIOURL }, "in-cluster MinIO"},
		{"same bucket", func(h *fakeMoveHost, _ *moveRun) { h.bucket, h.s3URL = moveNewBucket, moveEndpoint }, "already go to"},
		{"SSE-C held without --sse-c", func(h *fakeMoveHost, _ *moveRun) {
			h.secrets[sseKeySecret] = map[string][]byte{sseKeyField: bytes.Repeat([]byte{1}, 32)}
		}, "Pass --sse-c"},
		{"unfinished key reset", func(h *fakeMoveHost, _ *moveRun) { h.resetStopped = true }, "kip backup repository-key reset"},
		{"second backup location", func(h *fakeMoveHost, _ *moveRun) { h.locations = 1 }, "2 backup locations"},
		{"public HTTP endpoint", func(_ *fakeMoveHost, r *moveRun) { r.opts.Target.Endpoint = "http://objects.example.com" }, "plain HTTP"},
		{"location with a prefix", func(h *fakeMoveHost, _ *moveRun) { h.prefix = "cluster-a" }, "prefix cluster-a"},
		{"previous not retired", func(h *fakeMoveHost, _ *moveRun) {
			h.record["previous"] = `{"bucket":"older"}`
		}, "retire-previous"},
		{"declined", func(_ *fakeMoveHost, r *moveRun) {
			r.opts.Confirm = func([]string) error { return errors.New("move canceled") }
		}, "move canceled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fastMove(t)
			h := newFakeMoveHost("static-passw0rd")
			r := newMoveRun(t, false)
			tt.setup(h, r)
			before := len(h.record)

			err := MoveBackupStorage(h, r.opts)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
			assert.Len(t, h.record, before, "no move is recorded")
			assert.Equal(t, "static-passw0rd", h.key(repositoryKeySecret))
			assert.NotContains(t, h.secrets, previousKeySecret)
			assert.NotContains(t, h.secrets, nextKeySecret)
			assert.Equal(t, 1, h.velero)
			assert.NoFileExists(t, r.opts.NewKeyFile)
		})
	}
}

func TestMoveBackupStorageGivesVeleroBackWhenWorkStartsWhileItStops(t *testing.T) {
	fastMove(t)
	h := newFakeMoveHost("static-passw0rd")
	h.busyOnStop = []string{"Restore/r1 InProgress"}
	r := newMoveRun(t, false)

	err := MoveBackupStorage(h, r.opts)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "in progress")
	assert.Equal(t, 1, h.velero, "nothing irreversible happened, so the old location keeps running")
	assert.Equal(t, "static-passw0rd", h.key(repositoryKeySecret))
	assert.Equal(t, movePreparing, h.storageRecord(t).Move.Phase)

	h.inFlight = nil
	h.busyOnStop = nil
	require.NoError(t, MoveBackupStorage(h, r.opts), "the same command continues the move")
	assert.Equal(t, moveNewBucket, h.bucket)
	assert.Equal(t, 1, r.confirmed, "a continued move does not ask again")
}

func TestMoveBackupStorageContinuesAfterAnInterruptionAtEachStep(t *testing.T) {
	steps := []string{
		"create secret generic " + previousKeySecret,
		"/dev/urandom",
		"--replicas=0",
		"delete backups.velero.io",
		"create secret generic " + repositoryKeySecret,
		"create secret generic cloud-credentials",
		"kind: HelmChart",
		"rollout restart deploy/velero",
		"delete secret " + nextKeySecret,
	}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			fastMove(t)
			h := newFakeMoveHost("static-passw0rd")
			h.failOn = step
			r := newMoveRun(t, false)

			err := MoveBackupStorage(h, r.opts)
			require.Error(t, err)
			// Initial staged-key cleanup fails before the move is recorded, so it has no resume hint.
			if step != "delete secret "+nextKeySecret {
				assert.Contains(t, err.Error(), "Run the same kip backup storage set command again", "a failed move names the command that continues it")
			}
			require.NoError(t, MoveBackupStorage(h, r.opts))

			assert.Equal(t, moveNewBucket, h.bucket)
			assert.Equal(t, moveNewCreds, h.creds)
			assert.Equal(t, 1, h.velero)
			newKey := h.key(repositoryKeySecret)
			assert.False(t, IsDefaultRepositoryKey([]byte(newKey)))
			assert.Equal(t, newKey, readFile(t, r.opts.NewKeyFile))
			assert.Equal(t, "static-passw0rd", h.key(previousKeySecret), "the previous key copy is never replaced")
			assert.Equal(t, "static-passw0rd", readFile(t, r.opts.PreviousKeyFile))
			assert.Nil(t, h.storageRecord(t).Move)
			assert.NotContains(t, h.secrets, nextKeySecret)
		})
	}
}

func TestMoveBackupStorageFinishesWithCorrectedCredentialsAfterTheLocationStaysUnavailable(t *testing.T) {
	fastMove(t)
	h := newFakeMoveHost("static-passw0rd")
	r := newMoveRun(t, false)
	r.opts.Target.AccessKeyID = "AKIDWRONG"

	err := MoveBackupStorage(h, r.opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "access denied")
	assert.Contains(t, err.Error(), "--credentials")
	assert.Equal(t, moveSwitched, h.storageRecord(t).Move.Phase)

	h.cmds = nil
	r.opts.Target.AccessKeyID = moveNewCreds
	require.NoError(t, MoveBackupStorage(h, r.opts))
	assert.Equal(t, moveNewCreds, h.creds)
	assert.True(t, h.ran("rollout restart deploy/velero"), "the running server must restart to read the corrected credentials")
	assert.False(t, h.ran("/dev/urandom"), "a switched move never makes another key")
	assert.False(t, h.ran("delete backups.velero.io"), "the new location's records stay")
}

func TestMoveBackupStorageRecordsACorrectedRegion(t *testing.T) {
	fastMove(t)
	h := newFakeMoveHost("static-passw0rd")
	r := newMoveRun(t, false)
	r.opts.Target.AccessKeyID = "AKIDWRONG"
	require.Error(t, MoveBackupStorage(h, r.opts))
	require.Equal(t, moveSwitched, h.storageRecord(t).Move.Phase)

	r.opts.Target.AccessKeyID = moveNewCreds
	r.opts.Target.Region = "eu-2"
	require.NoError(t, MoveBackupStorage(h, r.opts))

	rec := h.storageRecord(t)
	require.NotNil(t, rec.Current)
	assert.Equal(t, "eu-2", rec.Current.Region)
	cfg, err := config.Load()
	require.NoError(t, err)
	local := BackupStorageFromRef(cfg.GetCluster("prod").BackupStorage)
	assert.Equal(t, "eu-2", local.Region)
	got, err := UpgradeBackupStorage(rec, local, "prod")
	require.NoError(t, err)
	assert.Equal(t, "eu-2", got.Region, "an upgrade must not bring the old region back")
}

func TestMoveBackupStorageLeavesCredentialsAloneWhileTheNewLocationIsBusy(t *testing.T) {
	fastMove(t)
	h := newFakeMoveHost("static-passw0rd")
	r := newMoveRun(t, false)
	r.opts.Target.AccessKeyID = "AKIDWRONG"
	require.Error(t, MoveBackupStorage(h, r.opts))

	h.inFlight = []string{"Backup/daily InProgress"}
	r.opts.Target.AccessKeyID = "AKIDOTHER"
	err := MoveBackupStorage(h, r.opts)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "in progress")
	assert.Equal(t, "AKIDWRONG", h.creds)
}

func TestMoveBackupStorageRefusesALiveKeyChangedOutsideTheMove(t *testing.T) {
	fastMove(t)
	h := newFakeMoveHost("static-passw0rd")
	h.failOn = "create secret generic " + repositoryKeySecret
	r := newMoveRun(t, false)
	require.Error(t, MoveBackupStorage(h, r.opts))
	h.secrets[repositoryKeySecret] = map[string][]byte{repositoryKeyField: []byte("someone-elses-key")}

	err := MoveBackupStorage(h, r.opts)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "neither the previous nor the new key")
	assert.Equal(t, "someone-elses-key", h.key(repositoryKeySecret))
}

func TestMoveBackupStorageRefusesAnExportFileWithAnotherKey(t *testing.T) {
	fastMove(t)
	h := newFakeMoveHost("static-passw0rd")
	r := newMoveRun(t, false)
	require.NoError(t, os.WriteFile(r.opts.PreviousKeyFile, []byte("something-else"), 0o600))

	err := MoveBackupStorage(h, r.opts)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "holds a different key")
	assert.Equal(t, "static-passw0rd", h.key(repositoryKeySecret))
	assert.Equal(t, 1, h.velero)
}

func TestMoveBackupStorageIgnoresSettledWork(t *testing.T) {
	fastMove(t)
	h := newFakeMoveHost("static-passw0rd")
	h.inFlight = []string{"Backup/daily Completed", "Backup/queued New", "Restore/r1 PartiallyFailed", "DeleteBackupRequest/daily-x Processed"}
	r := newMoveRun(t, false)

	require.NoError(t, MoveBackupStorage(h, r.opts))
}

func TestMoveBackupStorageRewritesAnEmptyExportFile(t *testing.T) {
	fastMove(t)
	h := newFakeMoveHost("static-passw0rd")
	r := newMoveRun(t, false)
	require.NoError(t, os.WriteFile(r.opts.NewKeyFile, nil, 0o600))

	require.NoError(t, MoveBackupStorage(h, r.opts), "a run stopped between creating and writing the file continues")

	assert.Equal(t, h.key(repositoryKeySecret), readFile(t, r.opts.NewKeyFile))
}

func TestMoveBackupStorageRefusesAnotherTargetWhileAMoveIsOpen(t *testing.T) {
	fastMove(t)
	h := newFakeMoveHost("static-passw0rd")
	h.busyOnStop = []string{"Restore/r1 InProgress"}
	r := newMoveRun(t, false)
	require.Error(t, MoveBackupStorage(h, r.opts))

	r.opts.Target.Bucket = "third-bucket"
	err := MoveBackupStorage(h, r.opts)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--bucket "+moveNewBucket)
}

func TestMoveBackupStorageToTheCurrentBucketOnlyClearsALeftoverStagedKey(t *testing.T) {
	fastMove(t)
	h := newFakeMoveHost("static-passw0rd")
	r := newMoveRun(t, false)
	require.NoError(t, MoveBackupStorage(h, r.opts))
	h.secrets[nextKeySecret] = map[string][]byte{repositoryKeyField: []byte("leftover")}
	h.cmds = nil

	require.NoError(t, MoveBackupStorage(h, r.opts))

	assert.NotContains(t, h.secrets, nextKeySecret)
	assert.Contains(t, r.out.String(), "already go to")
	assert.False(t, h.ran("--replicas=0"))
}

func TestMoveBackupStorageClearsALeftoverStagedKeyBeforeAFreshMove(t *testing.T) {
	fastMove(t)
	h := newFakeMoveHost("static-passw0rd")
	h.secrets[nextKeySecret] = map[string][]byte{repositoryKeyField: []byte("static-passw0rd")}
	r := newMoveRun(t, false)

	require.NoError(t, MoveBackupStorage(h, r.opts))

	assert.False(t, IsDefaultRepositoryKey([]byte(h.key(repositoryKeySecret))))
}

func TestAbortBackupStorageMoveUndoesAPreparingMove(t *testing.T) {
	fastMove(t)
	h := newFakeMoveHost("static-passw0rd")
	h.sseWriteFail = true
	r := newMoveRun(t, true)

	err := MoveBackupStorage(h, r.opts)
	require.Error(t, err)
	assert.Contains(t, h.secrets, sseKeySecret)

	require.NoError(t, AbortBackupStorageMove(h, io.Discard))

	assert.Nil(t, h.storageRecord(t).Move)
	assert.NotContains(t, h.secrets, sseKeySecret, "the key this move generated goes with it")
	assert.NotContains(t, h.secrets, nextKeySecret)
	assert.NotContains(t, h.secrets, previousKeySecret)
	assert.Equal(t, "static-passw0rd", h.key(repositoryKeySecret))
	assert.Equal(t, 1, h.velero)
}

func TestAbortBackupStorageMoveKeepsAnSSECKeyTheMoveDidNotMake(t *testing.T) {
	fastMove(t)
	h := newFakeMoveHost("static-passw0rd")
	h.sse = true
	h.secrets[sseKeySecret] = map[string][]byte{sseKeyField: bytes.Repeat([]byte{7}, sseKeyBytes)}
	h.busyOnStop = []string{"Backup/b1 InProgress"}
	r := newMoveRun(t, true)
	require.Error(t, MoveBackupStorage(h, r.opts))

	require.NoError(t, AbortBackupStorageMove(h, io.Discard))

	assert.Contains(t, h.secrets, sseKeySecret)
}

func TestAbortBackupStorageMoveRefusesOnceTheCutoverBegan(t *testing.T) {
	fastMove(t)
	h := newFakeMoveHost("static-passw0rd")
	h.failOn = "create secret generic " + repositoryKeySecret
	r := newMoveRun(t, false)
	require.Error(t, MoveBackupStorage(h, r.opts))

	err := AbortBackupStorageMove(h, io.Discard)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "can only be finished")
	assert.NotNil(t, h.storageRecord(t).Move)
}

func TestRetirePreviousBackupStorageNeedsABackupAfterTheMove(t *testing.T) {
	fastMove(t)
	h := newFakeMoveHost("static-passw0rd")
	r := newMoveRun(t, false)
	require.NoError(t, MoveBackupStorage(h, r.opts))
	confirm := func([]string) error { return nil }

	before, after := moveNow.Add(-time.Hour).Format(time.RFC3339), moveNow.Add(time.Hour).Format(time.RFC3339)
	h.backups = "Completed " + before + " " + before + "\nFailed " + after + " " + after + "\n" +
		"Completed " + after + " " + before + "\nCompleted " + after + " -\n"
	err := RetirePreviousBackupStorage(h, confirm, io.Discard)
	require.Error(t, err, "a backup made before the move or with no start time cannot verify that backups work at the new location")
	assert.Contains(t, err.Error(), "no backup has completed")
	assert.Contains(t, h.secrets, previousKeySecret)

	h.backups += "Completed " + after + " " + after + "\n"
	require.NoError(t, RetirePreviousBackupStorage(h, confirm, io.Discard))
	rec := h.storageRecord(t)
	assert.Nil(t, rec.Previous)
	assert.NotNil(t, rec.Current, "the current location stays recorded for other operators' kip")
	assert.NotContains(t, h.secrets, previousKeySecret)
}

func (h *fakeMoveHost) Upload(io.Reader, string, os.FileMode) error { return nil }

func TestInstallBackupRefusesWhileABackupStorageMoveIsOpen(t *testing.T) {
	h := newFakeMoveHost("static-passw0rd")
	h.record["move"] = `{"target":{"bucket":"new-backups"},"phase":"cutover"}`

	err := installBackup(h, &BackupStorageConfig{Bucket: moveOldBucket, Region: "eu-west-1"}, "longhorn", nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--bucket new-backups")
	assert.False(t, h.ran("kind: HelmChart"), "neither location's chart is applied mid-move")
}

func TestRepositoryKeyResetRefusesWhileABackupStorageMoveIsOpen(t *testing.T) {
	h := newFakeMoveHost("static-passw0rd")
	h.record["move"] = `{"target":{"bucket":"new-backups"},"phase":"cutover"}`

	_, err := SurveyRepositoryKeyReset(h)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--bucket new-backups")
	assert.Contains(t, err.Error(), "Nothing was changed")
}

func TestMoveBackupStorageNeverSwitchesToTheDefaultOrThePreviousKey(t *testing.T) {
	for _, weak := range []string{"static-passw0rd", "own-key-of-this-cluster"} {
		t.Run(weak, func(t *testing.T) {
			fastMove(t)
			h := newFakeMoveHost("own-key-of-this-cluster")
			h.generateWeak = weak
			r := newMoveRun(t, false)

			err := MoveBackupStorage(h, r.opts)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "stopped before switching")
			assert.Equal(t, "own-key-of-this-cluster", h.key(repositoryKeySecret))
			assert.Equal(t, 1, h.velero)
			assert.NoFileExists(t, r.opts.NewKeyFile)
		})
	}
}

func TestUpgradeBackupStorageTakesTheRecordedLocation(t *testing.T) {
	newMoveRun(t, false)
	local := &BackupStorageConfig{Bucket: moveOldBucket, Region: "eu-west-1"}
	moved := StorageRecord{Current: &StorageLocation{Bucket: moveNewBucket, Region: "eu-1", Endpoint: moveEndpoint}}

	got, err := UpgradeBackupStorage(moved, local, "prod")

	require.NoError(t, err)
	assert.Equal(t, &BackupStorageConfig{Bucket: moveNewBucket, Region: "eu-1", Endpoint: moveEndpoint}, got,
		"a second machine's stale config must not send Velero back to the old bucket")
	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, moveNewBucket, cfg.GetCluster("prod").BackupStorage.Bucket)

	got, err = UpgradeBackupStorage(moved, got, "prod")
	require.NoError(t, err)
	assert.Equal(t, moveNewBucket, got.Bucket)

	got, err = UpgradeBackupStorage(StorageRecord{}, local, "prod")
	require.NoError(t, err)
	assert.Same(t, local, got, "a cluster that never moved keeps its local setting")
}

func TestUpgradeBackupStorageRefusesAnUnfinishedMove(t *testing.T) {
	newMoveRun(t, false)
	open := StorageRecord{Move: &BackupStorageMove{Target: StorageLocation{Bucket: moveNewBucket, Endpoint: moveEndpoint}, Phase: moveSwitched}}

	_, err := UpgradeBackupStorage(open, &BackupStorageConfig{Bucket: moveOldBucket, Region: "eu-west-1"}, "prod")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--bucket "+moveNewBucket)
}

func TestMoveBackupStorageRefusesAPublicHTTPEndpointBeforeReadingTheCluster(t *testing.T) {
	h := newFakeMoveHost("static-passw0rd")
	r := newMoveRun(t, false)
	r.opts.Target.Endpoint = "http://objects.example.com"

	err := MoveBackupStorage(h, r.opts)

	require.Error(t, err)
	assert.Empty(t, h.cmds, "nothing is read or changed for an endpoint that would send Secrets over plain HTTP")
}

func TestMoveBackupStorageResumeKeepsTheSSECSetting(t *testing.T) {
	h := newFakeMoveHost("static-passw0rd")
	h.record["move"] = `{"target":{"bucket":"` + moveNewBucket + `","endpoint":"` + moveEndpoint + `","sseC":true},"phase":"preparing"}`
	r := newMoveRun(t, false)

	err := MoveBackupStorage(h, r.opts)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "same --sse-c setting")
}

func TestMoveBackupStorageResumeWhilePreparingRechecksTheNewBucket(t *testing.T) {
	h := newFakeMoveHost("static-passw0rd")
	h.record["move"] = `{"target":{"bucket":"` + moveNewBucket + `","endpoint":"` + moveEndpoint + `"},"phase":"preparing"}`
	h.populated = true
	r := newMoveRun(t, false)

	err := MoveBackupStorage(h, r.opts)

	require.ErrorContains(t, err, "already holds backups")
	assert.Contains(t, err.Error(), "--abort")
	assert.False(t, h.ran("--replicas=0"), "Velero keeps running on the old bucket")
}

func TestRequireSSECKeyFlagMatchesTheSSECSetting(t *testing.T) {
	require.Error(t, requireSSECKeyFlag(true, ""), "an SSE-C move needs a place to export its key")
	require.Error(t, requireSSECKeyFlag(false, "sse.key"), "a key file without SSE-C points at a mistake")
	require.NoError(t, requireSSECKeyFlag(true, "sse.key"))
	require.NoError(t, requireSSECKeyFlag(false, ""))
}

func TestRepositoryKeyResetRefusesARecoveryInstallsReadOnlyLocation(t *testing.T) {
	h := newFakeMoveHost("static-passw0rd")
	h.readOnly = true

	_, err := SurveyRepositoryKeyReset(h)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "read-only")
}

func TestInstallRefusesToRepointAClusterWhoseBackupsWereMoved(t *testing.T) {
	h := newFakeMoveHost("static-passw0rd")
	h.bucket, h.s3URL = moveNewBucket, moveEndpoint
	h.record["current"] = `{"bucket":"` + moveNewBucket + `","region":"eu-1","endpoint":"` + moveEndpoint + `"}`

	err := installBackup(h, &BackupStorageConfig{Bucket: "third-backups", Region: "eu-1", Endpoint: moveEndpoint}, "longhorn", nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip backup storage set --bucket third-backups")
	assert.False(t, h.ran("kind: HelmChart"))
}

func TestInstallKeepsTheBucketAFinishedMoveRecorded(t *testing.T) {
	h := newFakeMoveHost("static-passw0rd")
	h.bucket, h.s3URL = moveNewBucket, moveEndpoint
	h.secrets["cloud-credentials"] = map[string][]byte{"cloud": []byte("x")}
	h.record["current"] = `{"bucket":"` + moveNewBucket + `","region":"eu-1","endpoint":"` + moveEndpoint + `"}`

	require.NoError(t, installBackup(h, &BackupStorageConfig{Bucket: moveNewBucket, Region: "eu-1", Endpoint: moveEndpoint}, "longhorn", nil))
	assert.True(t, h.ran("kind: HelmChart"), "an install naming the recorded bucket goes ahead")
}

func TestUpgradeOffersAbortOnlyWhileAMoveIsPreparing(t *testing.T) {
	preparing := StorageRecord{Move: &BackupStorageMove{Target: StorageLocation{Bucket: moveNewBucket}, Phase: movePreparing}}
	_, err := UpgradeBackupStorage(preparing, nil, "prod")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--abort")

	cutover := StorageRecord{Move: &BackupStorageMove{Target: StorageLocation{Bucket: moveNewBucket}, Phase: moveCutover}}
	_, err = UpgradeBackupStorage(cutover, nil, "prod")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "--abort", "abort works only before the switch begins")
}

func TestAMoveToAnotherBucketOffersAbortOnlyWhileTheOpenMoveIsPreparing(t *testing.T) {
	h := newFakeMoveHost("static-passw0rd")
	r := newMoveRun(t, false)
	h.record["move"] = `{"target":{"bucket":"other-backups"},"phase":"preparing"}`
	require.ErrorContains(t, MoveBackupStorage(h, r.opts), "--abort")

	h.record["move"] = `{"target":{"bucket":"other-backups"},"phase":"cutover"}`
	err := MoveBackupStorage(h, r.opts)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "--abort")
}

func TestAbortBackupStorageMoveWithoutAMoveRefuses(t *testing.T) {
	h := newFakeMoveHost("static-passw0rd")

	require.Error(t, AbortBackupStorageMove(h, io.Discard))
	assert.False(t, h.ran("delete secret"))
}

func TestRetirePreviousBackupStorageRefusesWithoutAPreviousOrDuringAMove(t *testing.T) {
	confirm := func([]string) error { return nil }
	h := newFakeMoveHost("static-passw0rd")
	require.Error(t, RetirePreviousBackupStorage(h, confirm, io.Discard), "nothing to retire")

	h.record["previous"] = `{"bucket":"old-backups"}`
	h.record["move"] = `{"target":{"bucket":"newer-backups"},"phase":"preparing"}`
	require.Error(t, RetirePreviousBackupStorage(h, confirm, io.Discard), "a second move is still open")
	assert.NotNil(t, h.storageRecord(t).Previous)
}

func TestAnUpgradeKeepsARecoveryInstallReadOnlyWithoutTheLocalFlag(t *testing.T) {
	h := newFakeMoveHost("static-passw0rd")
	h.readOnly = true
	h.secrets["cloud-credentials"] = map[string][]byte{"cloud": []byte("x")}

	require.NoError(t, installBackup(h, &BackupStorageConfig{Bucket: moveOldBucket, Region: "eu-1"}, "longhorn", nil))

	chart := ""
	for _, c := range h.cmds {
		if strings.Contains(c, "kind: HelmChart") {
			chart = c
		}
	}
	assert.Contains(t, chart, "accessMode: ReadOnly", "Velero must never expire the backups a recovery install reads")
	assert.NotContains(t, chart, "disabled: false", "both schedules stay off")
	assert.Contains(t, chart, "disabled: true")
}

func TestInstallOffersAbortOnlyWhileAMoveIsPreparing(t *testing.T) {
	h := newFakeMoveHost("static-passw0rd")
	h.record["move"] = `{"target":{"bucket":"new-backups"},"phase":"preparing"}`
	require.ErrorContains(t, refuseDuringStorageMove(h), "--abort")

	h.record["move"] = `{"target":{"bucket":"new-backups"},"phase":"cutover"}`
	err := refuseDuringStorageMove(h)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "--abort")
}
