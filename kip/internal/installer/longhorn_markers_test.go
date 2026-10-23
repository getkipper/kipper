package installer

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLonghornVersionToApply(t *testing.T) {
	for name, tc := range map[string]struct {
		m     LonghornMarkers
		want  string
		fresh bool
	}{
		"fresh host":                     {LonghornMarkers{}, longhornVersion, true},
		"settled old pin":                {LonghornMarkers{Applied: "v1.7.3", Running: "v1.7.3", CRDs: true}, "v1.7.3", false},
		"manifest applied, manager busy": {LonghornMarkers{Applied: "v1.8.2", Running: "v1.7.3", CRDs: true}, "v1.8.2", false},
		"manager DaemonSet deleted":      {LonghornMarkers{Running: "v1.9.2", CRDs: true}, "v1.9.2", false},
		// A record whose manifest never reached the cluster keeps the old release.
		"prepared hop record": {LonghornMarkers{Record: &LonghornHopRecord{From: "v1.8.2", To: "v1.9.2", Backup: "b", Export: "e"}, Applied: "v1.8.2", Running: "v1.8.2", CRDs: true}, "v1.8.2", false},
		// The record outranks the DaemonSet: a partial apply can leave the old image.
		"record, manager moved":      {LonghornMarkers{Record: &LonghornHopRecord{From: "v1.8.2", To: "v1.9.2", Backup: "b", Export: "e"}, Applied: "v1.8.2", Running: "v1.9.2", CRDs: true}, "v1.9.2", false},
		"catch-up record":            {LonghornMarkers{Record: &LonghornHopRecord{From: "v1.9.2", To: "v1.9.2", Backup: "b", Export: "e"}, Applied: "v1.9.2", Running: "v1.9.2", CRDs: true}, "v1.9.2", false},
		"record, manifest applied":   {LonghornMarkers{Record: &LonghornHopRecord{From: "v1.8.2", To: "v1.9.2", Backup: "b", Export: "e"}, Applied: "v1.9.2", Running: "v1.8.2", CRDs: true}, "v1.9.2", false},
		"older pin outside the path": {LonghornMarkers{Applied: "v1.6.4", Running: "v1.6.4", CRDs: true}, "v1.6.4", false},
		// An earlier kip's path can name another patch of the next minor.
		"record from another kip's path": {LonghornMarkers{Record: &LonghornHopRecord{From: "v1.9.2", To: "v1.10.1", Backup: "b", Export: "e"}, Applied: "v1.10.1", Running: "v1.9.2", CRDs: true}, "v1.10.1", false},
		"prepared patch step":            {LonghornMarkers{Record: &LonghornHopRecord{From: "v1.10.1", To: "v1.10.2", Backup: "b", Export: "e"}, Applied: "v1.10.1", Running: "v1.10.1", CRDs: true}, "v1.10.1", false},
		"started patch step":             {LonghornMarkers{Record: &LonghornHopRecord{From: "v1.10.1", To: "v1.10.2", Backup: "b", Export: "e"}, Applied: "v1.10.2", Running: "v1.10.1", CRDs: true}, "v1.10.2", false},
		"catch-up off the path":          {LonghornMarkers{Record: &LonghornHopRecord{From: "v1.10.1", To: "v1.10.1", Backup: "b", Export: "e"}, Applied: "v1.10.1", Running: "v1.10.1", CRDs: true}, "v1.10.1", false},
	} {
		got, fresh, err := LonghornVersionToApply(tc.m)
		require.NoError(t, err, name)
		assert.Equal(t, tc.want, got, name)
		assert.Equal(t, tc.fresh, fresh, name)
	}
}

func TestLonghornVersionToApplyRefusesWhatWouldDowngradeOrGuess(t *testing.T) {
	for name, tc := range map[string]struct {
		m    LonghornMarkers
		want string
	}{
		"CRDs without a version":    {LonghornMarkers{CRDs: true}, "cannot tell which Longhorn version"},
		"manager ahead of manifest": {LonghornMarkers{Applied: "v1.8.2", Running: "v1.9.2", CRDs: true}, "already runs"},
		"manager ahead of record":   {LonghornMarkers{Record: &LonghornHopRecord{From: "v1.8.2", To: "v1.9.2", Backup: "b", Export: "e"}, Running: "v1.10.2", CRDs: true}, "current-longhorn-version setting shows v1.10.2"},
		"manifest ahead of record":  {LonghornMarkers{Record: &LonghornHopRecord{From: "v1.8.2", To: "v1.9.2", Backup: "b", Export: "e"}, Applied: "v1.10.2", Running: "v1.9.2", CRDs: true}, "DaemonSet shows v1.10.2"},
		"unreadable record":         {LonghornMarkers{Record: &LonghornHopRecord{From: "v1.8.2", To: "soon", Backup: "b", Export: "e"}, CRDs: true}, "cannot read"},
		"record skips a minor":      {LonghornMarkers{Record: &LonghornHopRecord{From: "v1.7.3", To: "v1.11.3", Backup: "b", Export: "e"}, Applied: "v1.7.3", CRDs: true}, "not a step kip takes"},
		"record past the target":    {LonghornMarkers{Record: &LonghornHopRecord{From: "v1.11.3", To: "v1.12.0", Backup: "b", Export: "e"}, Applied: "v1.11.3", CRDs: true}, "not a step kip takes"},
		"patch step backwards":      {LonghornMarkers{Record: &LonghornHopRecord{From: "v1.10.2", To: "v1.10.1", Backup: "b", Export: "e"}, Applied: "v1.10.2", CRDs: true}, "not a step kip takes"},
		"record goes backwards":     {LonghornMarkers{Record: &LonghornHopRecord{From: "v1.9.2", To: "v1.8.2", Backup: "b", Export: "e"}, Applied: "v1.9.2", CRDs: true}, "not a step kip takes"},
		"record without a backup":   {LonghornMarkers{Record: &LonghornHopRecord{From: "v1.8.2", To: "v1.9.2"}, Applied: "v1.8.2", CRDs: true}, "without the backup and export"},
		"newer than this kip":       {LonghornMarkers{Applied: "v1.13.0", Running: "v1.13.0", CRDs: true}, "newer than"},
		"unreadable image tag":      {LonghornMarkers{Applied: "master-head", CRDs: true}, "cannot read"},
	} {
		_, _, err := LonghornVersionToApply(tc.m)
		assert.ErrorContains(t, err, tc.want, name)
	}
}

const longhornCRDs = "customresourcedefinition.apiextensions.k8s.io/settings.longhorn.io\ncustomresourcedefinition.apiextensions.k8s.io/volumes.longhorn.io\n"

func longhornMarkerHost(image, running, crd, record string) *routedRunner {
	return &routedRunner{routes: []route{
		{contains: "configmap " + LonghornHopRecordName, out: record},
		{contains: "daemonset longhorn-manager", out: image},
		{contains: "current-longhorn-version", out: running},
		{contains: "get crd --request-timeout=30s -o name", out: crd},
	}}
}

func TestReadLonghornMarkers(t *testing.T) {
	r := longhornMarkerHost("Warning: something\n"+"docker.io/longhornio/longhorn-manager:v1.10.2", "v1.9.2", longhornCRDs, "")
	m, err := ReadLonghornMarkers(r)
	require.NoError(t, err)
	assert.Equal(t, LonghornMarkers{Applied: "v1.10.2", Running: "v1.9.2", CRDs: true}, m)

	r = longhornMarkerHost("", "", "", "")
	m, err = ReadLonghornMarkers(r)
	require.NoError(t, err)
	assert.Equal(t, LonghornMarkers{}, m, "nothing installed")

	r = longhornMarkerHost("longhornio/longhorn-manager:v1.8.2", "v1.8.2", longhornCRDs, "v1.8.2\tv1.9.2\t2026-10-24T01:00:00Z\tbefore-longhorn-v1.9.2\t/home/op/.kip/x")
	m, err = ReadLonghornMarkers(r)
	require.NoError(t, err)
	require.NotNil(t, m.Record)
	assert.Equal(t, LonghornHopRecord{From: "v1.8.2", To: "v1.9.2", Started: "2026-10-24T01:00:00Z", Backup: "before-longhorn-v1.9.2", Export: "/home/op/.kip/x"}, *m.Record)
}

func TestReadLonghornMarkersFailsClosed(t *testing.T) {
	r := longhornMarkerHost("an image kip does not know", "v1.7.3", longhornCRDs, "")
	_, err := ReadLonghornMarkers(r)
	assert.ErrorContains(t, err, "longhorn-manager image")

	r = longhornMarkerHost("longhornio/longhorn-manager:v1.7.3", "v1.7.3", longhornCRDs, "")
	r.routes[2].err = errors.New("connection refused")
	_, err = ReadLonghornMarkers(r)
	assert.ErrorContains(t, err, "connection refused")
}

func TestHopRecordIsWrittenAndRemoved(t *testing.T) {
	r := &routedRunner{}
	rec := LonghornHopRecord{From: "v1.7.3", To: "v1.8.2", Started: "2026-10-24T01:00:00Z", Backup: "b", Export: "/tmp/x"}
	require.NoError(t, writeLonghornHopRecord(r, rec))
	require.Len(t, r.stdins, 1)
	for _, want := range []string{"name: " + LonghornHopRecordName, "namespace: kipper-system", `from: "v1.7.3"`, `to: "v1.8.2"`, `backup: "b"`} {
		assert.Contains(t, r.stdins[0], want)
	}
	assert.True(t, r.ran("kubectl apply -f -"))

	require.NoError(t, deleteLonghornHopRecord(r))
	assert.True(t, r.ran("delete configmap "+LonghornHopRecordName+" --ignore-not-found"))
	assert.False(t, strings.Contains(strings.Join(r.commands, "\n"), "longhorn-system delete"))
}

// kubectl fails on a resource type the API does not know, so a fresh host
// must never be asked for Longhorn's settings.
func TestReadLonghornMarkersOnAFreshHostSkipsTheSettings(t *testing.T) {
	r := longhornMarkerHost("", "", "customresourcedefinition.apiextensions.k8s.io/apps.kipper.run\n", "")
	r.routes[2].err = errors.New(`error: the server doesn't have a resource type "settings"`)
	m, err := ReadLonghornMarkers(r)
	require.NoError(t, err)
	assert.Equal(t, LonghornMarkers{}, m)
	assert.False(t, r.ran("current-longhorn-version"))
}
