package installer

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/util/jsonpath"
)

// envBearingStatefulSet is a StatefulSet as kubectl prints it, with a
// literal password in its pod template and in kubectl's last-applied
// annotation.
const envBearingStatefulSet = `{
  "metadata": {"name": "db", "namespace": "shop", "resourceVersion": "41",
    "annotations": {
      "kipper.run/fence-stop": "{\"replicas\":1}",
      "kipper.run/storage-move": "move-1",
      "kipper.run/storage-move-replicas": "2",
      "kubectl.kubernetes.io/last-applied-configuration": "{\"env\":[{\"name\":\"POSTGRES_PASSWORD\",\"value\":\"s3cret-applied\"}]}"
    }},
  "spec": {"replicas": 0,
    "template": {"spec": {"containers": [{"name": "pg", "image": "postgres:17", "command": ["s3cret-command"],
      "env": [{"name": "POSTGRES_PASSWORD", "value": "s3cret-env"}]}]}},
    "volumeClaimTemplates": [{"metadata": {"name": "data"}}, {"metadata": {"name": "dump"}}]}
}`

// renderStatefulSets prints StatefulSets, given as kubectl's JSON for one
// object or a list, through the projection template.
func renderStatefulSets(t *testing.T, objects string) string {
	t.Helper()
	if !strings.Contains(objects, `"items"`) {
		objects = `{"items":[` + objects + `]}`
	}
	var list any
	require.NoError(t, json.Unmarshal([]byte(objects), &list))
	jp := jsonpath.New("statefulsets").AllowMissingKeys(true)
	require.NoError(t, jp.Parse(statefulSetTemplate(testReadMarker)))
	var out bytes.Buffer
	require.NoError(t, jp.Execute(&out, list))
	return out.String()
}

func TestStatefulSetReadLeavesThePodTemplateOnTheServer(t *testing.T) {
	out := renderStatefulSets(t, envBearingStatefulSet)

	assert.NotContains(t, out, "s3cret")
	sets, err := parseStatefulSets(out, testReadMarker)
	require.NoError(t, err)
	require.Len(t, sets, 1)
	s := sets[0]
	assert.Equal(t, "shop", s.Namespace)
	assert.Equal(t, "db", s.Name)
	assert.Equal(t, "41", s.ResourceVersion)
	require.NotNil(t, s.Spec.Replicas)
	assert.Equal(t, int32(0), *s.Spec.Replicas)
	assert.Equal(t, map[string]string{
		FenceStopAnnotation:                "{\"replicas\":1}",
		"kipper.run/storage-move":          "move-1",
		"kipper.run/storage-move-replicas": "2",
	}, s.Annotations)
	require.Len(t, s.Spec.VolumeClaimTemplates, 2)
	assert.Equal(t, "data", s.Spec.VolumeClaimTemplates[0].Name)
	assert.Equal(t, "dump", s.Spec.VolumeClaimTemplates[1].Name)
}

func TestStatefulSetReadKeepsAnUnsetReplicaCountUnset(t *testing.T) {
	out := renderStatefulSets(t, `{"metadata":{"name":"q","namespace":"jobs","resourceVersion":"7"},"spec":{}}`)

	sets, err := parseStatefulSets(out, testReadMarker)

	require.NoError(t, err)
	require.Len(t, sets, 1)
	assert.Nil(t, sets[0].Spec.Replicas)
	assert.Empty(t, sets[0].Annotations)
	assert.Empty(t, sets[0].Spec.VolumeClaimTemplates)
}

func TestStatefulSetReadRefusesAValueThatBreaksItsLine(t *testing.T) {
	out := renderStatefulSets(t, `{"metadata":{"name":"db","namespace":"shop","annotations":{"kipper.run/storage-move":"a\tb"}},"spec":{}}`)

	_, err := parseStatefulSets(out, testReadMarker)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "StatefulSet shop/db")
}

func TestStatefulSetReadRefusesALineThatIsNotARecord(t *testing.T) {
	_, err := parseStatefulSets("Warning: something\n"+renderStatefulSets(t, envBearingStatefulSet), testReadMarker)

	require.Error(t, err)
}

func TestStatefulSetReadKeepsStderrApart(t *testing.T) {
	cmd := statefulSetsCommand("-A", testReadMarker)

	assert.Contains(t, cmd, `kubectl get statefulsets -A -o jsonpath='`)
	assert.Contains(t, cmd, `2>"$e"`)
}
