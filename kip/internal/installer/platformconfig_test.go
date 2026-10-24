package installer

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/controller/pkg/platform"
)

func TestPickProfile(t *testing.T) {
	tests := []struct {
		name  string
		ramMB int
		want  string
	}{
		{"tiny 2 GB box", 2048, ProfileNano},
		{"3 GB reported", 3000, ProfileNano},
		{"4 GB box reporting 3831 MB", 3831, ProfileNano},
		{"4 GB box reporting 4096 MB", 4096, ProfileNano},
		{"just under nano threshold", 5999, ProfileNano},

		{"6 GB box reporting 6000 MB", 6000, ProfileSmall},
		{"6 GB box", 6144, ProfileSmall},
		{"just under small ceiling", 7499, ProfileSmall},

		{"8 GB box reporting 7700 MB", 7700, ProfileMedium},
		{"8 GB box reporting 8192 MB", 8192, ProfileMedium},
		{"12 GB box", 12288, ProfileMedium},
		{"just under medium ceiling", 14999, ProfileMedium},

		{"16 GB box reporting 15800 MB", 15800, ProfileLarge},
		{"16 GB box reporting 16384 MB", 16384, ProfileLarge},
		{"24 GB box", 24576, ProfileLarge},
		{"just under xlarge threshold", 29999, ProfileLarge},

		{"32 GB box reporting 30000 MB", 30000, ProfileXLarge},
		{"32 GB box reporting 32768 MB", 32768, ProfileXLarge},
		{"64 GB cluster node", 64253, ProfileXLarge},
		{"128 GB box", 131072, ProfileXLarge},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, pickProfile(tt.ramMB, 8))
		})
	}
}

func TestPickProfileCovers64GBProductionNode(t *testing.T) {
	// The xlarge profile gives Prometheus a higher memory limit on large hosts.
	assert.Equal(t, ProfileXLarge, pickProfile(64253, 16))
}

func TestPickProfileStepsDownWhenTheCPUsCannotCarryTheRAMPick(t *testing.T) {
	tests := []struct {
		name  string
		ramMB int
		cpus  int
		want  string
	}{
		{"8 GB on 2 vCPU drops from medium to small", 8192, 2, ProfileSmall},
		{"32 GB on 2 vCPU drops from xlarge to small", 32768, 2, ProfileSmall},
		{"4 GB on 2 vCPU gets nano from its RAM", 4096, 2, ProfileNano},
		{"6 GB on 2 vCPU stays small", 6144, 2, ProfileSmall},
		{"8 GB on 4 vCPU keeps medium", 8192, 4, ProfileMedium},
		{"8 GB on 3 vCPU keeps medium", 8192, 3, ProfileMedium},
		{"1 vCPU lands on nano, which is the floor", 8192, 1, ProfileNano},
		{"an unknown CPU count leaves the RAM pick alone", 8192, 0, ProfileMedium},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, pickProfile(tt.ramMB, tt.cpus))
		})
	}
}

func TestProfilePickNoteSaysWhenTheCPUsLoweredThePick(t *testing.T) {
	note := profilePickNote(8192, 2, ProfileSmall)
	assert.Contains(t, note, "small")
	assert.Contains(t, note, "medium")
	assert.Contains(t, note, "2 CPUs")
	assert.Contains(t, note, "500m")

	assert.Empty(t, profilePickNote(8192, 4, ProfileMedium), "the RAM pick was used as is")
	assert.Empty(t, profilePickNote(8192, 0, ProfileMedium), "no CPU count, no note")

	// Nano is the minimum even when its estimated CPU requests exceed capacity.
	floor := profilePickNote(8192, 1, ProfileNano)
	assert.Contains(t, floor, "1 CPU")
	assert.Contains(t, floor, "smallest")
	assert.NotContains(t, floor, "still fits")
}

func TestKeptProfileNotesCheckTheProfileThatStays(t *testing.T) {
	notes := keptProfileNotes(ProfileSmall, ProfileMedium, 2)
	if assert.Len(t, notes, 2) {
		assert.Contains(t, notes[0], "already runs the 'medium' profile")
		assert.Contains(t, notes[0], "kip platform profile set small")
		assert.Contains(t, notes[1], "500m")
	}
	assert.Len(t, keptProfileNotes(ProfileSmall, ProfileMedium, 4), 1, "medium fits 4 CPUs")
	assert.Empty(t, keptProfileNotes(ProfileMedium, ProfileMedium, 2))
	assert.Empty(t, keptProfileNotes(ProfileMedium, "", 2), "a fresh cluster has nothing recorded")
}

func TestResolveProfileHonorsAnExplicitChoice(t *testing.T) {
	sys := SystemInfo{RAMMB: 8192, CPUs: 2}
	assert.Equal(t, ProfileMedium, resolveProfile(ProfileMedium, "", sys))
	assert.Equal(t, ProfileNano, resolveProfile(ProfileNano, "", sys))
	assert.Equal(t, ProfileSmall, resolveProfile("", "", sys), "no --profile uses the RAM and CPU pick")
}

func TestResolveProfileKeepsASavedProfile(t *testing.T) {
	sys := SystemInfo{RAMMB: 8192, CPUs: 2}
	assert.Equal(t, ProfileMedium, resolveProfile("", ProfileMedium, sys))
	assert.Equal(t, ProfileNano, resolveProfile(ProfileMedium, ProfileNano, sys))
}

func TestReadSavedPlatformStateAcceptsOnlyAKnownProfile(t *testing.T) {
	state := readSavedPlatformState(&scriptedRunner{out: "'small|loki=;false\nprometheus=1Gi;\n'\n"})
	assert.Equal(t, ProfileSmall, state.Profile)
	assert.False(t, state.LokiEnabled(), "the explicit disable travels with the profile")
	assert.True(t, state.PrometheusEnabled())
	assert.Equal(t, ProfileMedium, readSavedPlatformState(&scriptedRunner{out: "medium|"}).Profile)
	assert.Empty(t, readSavedPlatformState(&scriptedRunner{out: ""}).Profile, "no cluster, no CR")
	assert.Empty(t, readSavedPlatformState(&scriptedRunner{out: "error: the server could not be reached"}).Profile)
	assert.Empty(t, readSavedPlatformState(&scriptedRunner{err: errors.New("ssh: connection lost")}).Profile)
}

func TestResourcesForProfile(t *testing.T) {
	tests := []struct {
		profile   string
		promReq   string
		promLim   string
		lokiReq   string
		lokiLim   string
		reasoning string
	}{
		{
			profile:   ProfileSmall,
			promReq:   "256Mi",
			promLim:   "512Mi",
			lokiReq:   "128Mi",
			lokiLim:   "384Mi",
			reasoning: "The small profile uses lower monitoring memory limits.",
		},
		{
			profile:   ProfileMedium,
			promReq:   "512Mi",
			promLim:   "1Gi",
			lokiReq:   "256Mi",
			lokiLim:   "512Mi",
			reasoning: "8-16 GB box: enough headroom for typical small-team clusters",
		},
		{
			profile:   ProfileLarge,
			promReq:   "512Mi",
			promLim:   "1Gi",
			lokiReq:   "256Mi",
			lokiLim:   "512Mi",
			reasoning: "The large profile shares monitoring memory limits with medium.",
		},
		{
			profile:   ProfileXLarge,
			promReq:   "1Gi",
			promLim:   "2Gi",
			lokiReq:   "512Mi",
			lokiLim:   "1Gi",
			reasoning: "The xlarge profile gives Prometheus and Loki higher memory limits.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.profile, func(t *testing.T) {
			got := platform.ResourcesForProfile(tt.profile)
			assert.Equal(t, tt.promReq, got.PrometheusMemoryRequest, tt.reasoning)
			assert.Equal(t, tt.promLim, got.PrometheusMemoryLimit, tt.reasoning)
			assert.Equal(t, tt.lokiReq, got.LokiMemoryRequest, tt.reasoning)
			assert.Equal(t, tt.lokiLim, got.LokiMemoryLimit, tt.reasoning)
		})
	}
}

func TestResourcesForProfileFallsBackToSmall(t *testing.T) {
	// Nano uses the small profile's limits if monitoring is explicitly enabled.
	got := platform.ResourcesForProfile(ProfileNano)
	assert.Equal(t, "512Mi", got.PrometheusMemoryLimit)
	assert.Equal(t, "384Mi", got.LokiMemoryLimit)

	// Unknown profiles also need usable memory limits.
	got = platform.ResourcesForProfile("garbage-from-future")
	assert.Equal(t, "512Mi", got.PrometheusMemoryLimit)
}

func TestIgnoredProfileFlagNamesTheProfileCommand(t *testing.T) {
	note := ignoredProfileNote(ProfileMedium, ProfileSmall)
	assert.Contains(t, note, "--profile small")
	assert.Contains(t, note, "'medium'")
	assert.Contains(t, note, "kip platform profile set small")
	assert.Empty(t, ignoredProfileNote(ProfileMedium, ProfileMedium))
	assert.Empty(t, ignoredProfileNote(ProfileMedium, ""), "no --profile given")
}

func TestInstallPlatformConfigRecordsTheStorageMode(t *testing.T) {
	host := &hostScript{}
	require.NoError(t, InstallPlatformConfig(host, ProfileNano, "longhorn", BackupInCluster))
	all := strings.Join(host.commands, "\n")
	assert.Contains(t, all, "  storage: longhorn\n", "a new cluster records its mode")
	assert.Contains(t, all, "  backup: in-cluster\n", "and its backup mode")
	assert.Contains(t, all, `kubectl patch platformconfig platform --type merge -p '{"spec":{"storage":"longhorn"}}'`,
		"a legacy CR gets the Longhorn it already runs")
	assert.Contains(t, all, `[ -n "$s" ] ||`, "a recorded mode is left alone")
}

func TestPlatformStateQueriesReadTheStorageMode(t *testing.T) {
	assert.Contains(t, platformStateJSONPath, "{.spec.profile}/{.spec.storage}/{.spec.backup}|")
	state := readSavedPlatformState(&scriptedRunner{out: "'nano/local|'\n"})
	assert.Equal(t, "local", state.Storage)
}

func TestResolveStorage(t *testing.T) {
	for name, tc := range map[string]struct {
		requested, profile, recorded string
		existing, unreadable         bool
		want, err                    string
	}{
		"a new nano cluster defaults to local":             {"", ProfileNano, "", false, false, StorageLocal, ""},
		"a new small cluster defaults to longhorn":         {"", ProfileSmall, "", false, false, StorageLonghorn, ""},
		"a new nano cluster can ask for longhorn":          {StorageLonghorn, ProfileNano, "", false, false, StorageLonghorn, ""},
		"a new medium cluster can ask for local":           {StorageLocal, ProfileMedium, "", false, false, StorageLocal, ""},
		"an existing cluster keeps its recorded mode":      {"", ProfileNano, StorageLocal, true, false, StorageLocal, ""},
		"a legacy nano cluster stays on longhorn":          {"", ProfileNano, "", true, false, StorageLonghorn, ""},
		"repeating the recorded mode is fine":              {StorageLonghorn, ProfileSmall, StorageLonghorn, true, false, StorageLonghorn, ""},
		"switching a legacy cluster to local is refused":   {StorageLocal, ProfileNano, "", true, false, "", "runs longhorn storage"},
		"switching a local cluster to longhorn is refused": {StorageLonghorn, ProfileNano, StorageLocal, true, false, "", "kip cluster storage longhorn"},
		"an unreadable cluster stays on longhorn":          {"", ProfileNano, "", true, true, StorageLonghorn, ""},
		"local on an unreadable cluster is refused":        {StorageLocal, ProfileNano, "", true, true, "", "could not be read"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := resolveStorage(tc.requested, tc.profile, tc.existing, tc.unreadable, tc.recorded)
			if tc.err != "" {
				assert.ErrorContains(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestDiscoverClusterStorage(t *testing.T) {
	k3s := route{contains: "test -d /var/lib/rancher/k3s", out: "existing\n"}
	for name, tc := range map[string]struct {
		saved                string
		routes               []route
		existing, unreadable bool
	}{
		"a saved PlatformConfig":           {"nano", nil, true, false},
		"no k3s on the host":               {"", []route{{contains: "test -d /var/lib/rancher/k3s", out: "fresh\n"}}, false, false},
		"k3s with Longhorn's classes":      {"", []route{k3s, {contains: "get storageclass", out: "storageclass.storage.k8s.io/longhorn-single\n"}}, true, false},
		"k3s without Longhorn":             {"", []route{k3s, {contains: "get storageclass", out: ""}}, false, false},
		"k3s with Longhorn still starting": {"", []route{k3s, {contains: "get storageclass", out: "namespace/longhorn-system\n"}}, true, false},
		"k3s that does not answer":         {"", []route{k3s, {contains: "get storageclass", err: errors.New("connection refused")}}, true, true},
	} {
		t.Run(name, func(t *testing.T) {
			existing, unreadable := discoverClusterStorage(&routedRunner{routes: tc.routes}, tc.saved)
			assert.Equal(t, tc.existing, existing, "existing")
			assert.Equal(t, tc.unreadable, unreadable, "unreadable")
		})
	}
}

func TestResolveBackup(t *testing.T) {
	for name, tc := range map[string]struct {
		external          bool
		storage, recorded string
		existing          bool
		want              string
	}{
		"a new local cluster has no backups":            {false, StorageLocal, "", false, BackupNone},
		"a new Longhorn cluster backs up to MinIO":      {false, StorageLonghorn, "", false, BackupInCluster},
		"external storage wins on a new cluster":        {true, StorageLocal, "", false, BackupExternal},
		"a legacy cluster keeps its in-cluster backups": {false, StorageLonghorn, "", true, BackupInCluster},
		"a recorded mode stays":                         {false, StorageLocal, BackupNone, true, BackupNone},
		"a cluster without backups can add external":    {true, StorageLocal, BackupNone, true, BackupExternal},
		"a re-run never adds MinIO to a local cluster":  {false, StorageLocal, BackupNone, true, BackupNone},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, resolveBackup(tc.external, tc.storage, tc.existing, tc.recorded))
		})
	}
}

// TestSettleBackup checks that the recorded mode is preserved unless external
// storage is supplied, and that external mode requires connection settings.
func TestSettleBackup(t *testing.T) {
	for name, tc := range map[string]struct {
		external          bool
		storage, recorded string
		want, err         string
	}{
		"a recorded external mode with its destination": {true, StorageLonghorn, BackupExternal, BackupExternal, ""},
		"a recorded external mode kip has no record of": {false, StorageLonghorn, BackupExternal, "", "--backup-storage-bucket"},
		"a recorded none stays none":                    {false, StorageLocal, BackupNone, BackupNone, ""},
		"a recorded in-cluster mode stays":              {false, StorageLocal, BackupInCluster, BackupInCluster, ""},
		"a legacy cluster is in-cluster":                {false, StorageLonghorn, "", BackupInCluster, ""},
		"external storage given now wins":               {true, StorageLocal, BackupNone, BackupExternal, ""},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := settleBackup(tc.external, tc.storage, tc.recorded)
			if tc.err != "" {
				assert.ErrorContains(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestInstallPlatformConfigLeavesTheBackupModeToTheInstaller(t *testing.T) {
	host := &hostScript{}
	require.NoError(t, InstallPlatformConfig(host, ProfileNano, StorageLocal, BackupNone))
	all := strings.Join(host.commands, "\n")
	assert.Contains(t, all, "  backup: none\n", "a new cluster records its mode")
	assert.NotContains(t, all, `"backup"`, "an existing cluster's backup mode is not patched on a guess")
}

// Clusters created before spec.backup existed can use external Velero storage
// even when kip has no local record of its settings.
func TestVeleroWritesExternally(t *testing.T) {
	for name, tc := range map[string]struct {
		out  string
		err  error
		want bool
		bad  bool
	}{
		"in-cluster MinIO":                   {out: "bsl[http://minio.velero.svc:9000]", want: false},
		"an S3-compatible endpoint":          {out: "bsl[https://s3.example.com]", want: true},
		"native AWS S3":                      {out: "bsl[]", want: true},
		"no backup location":                 {out: `Error from server (NotFound): backupstoragelocations.velero.io "default" not found`, err: errors.New("exit status 1")},
		"a cluster without Velero's types":   {out: `error: the server doesn't have a resource type "backupstoragelocation"`, err: errors.New("exit status 1")},
		"an API server that does not answer": {out: "The connection to the server was refused", err: errors.New("exit status 1"), bad: true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := veleroWritesExternally(&scriptedRunner{out: tc.out, err: tc.err})
			if tc.bad {
				assert.Error(t, err, "an unreadable location must not read as in-cluster")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestSettleBackupRefusesALegacyExternalClusterWithoutItsStorage(t *testing.T) {
	_, err := settleLegacyBackup(false, StorageLonghorn, "", true)
	assert.ErrorIs(t, err, errExternalBackupUnknown)
	mode, err := settleLegacyBackup(false, StorageLonghorn, "", false)
	assert.NoError(t, err)
	assert.Equal(t, BackupInCluster, mode)
}
