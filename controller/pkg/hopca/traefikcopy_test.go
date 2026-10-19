package hopca

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTraefikCopyData_HoldsTheLivePairAndTheFileConfig(t *testing.T) {
	data := TraefikCopyData([]byte("CERT"), []byte("KEY"))

	assert.Equal(t, []byte("CERT"), data["tls.crt"])
	assert.Equal(t, []byte("KEY"), data["tls.key"])
	assert.Equal(t, []byte(TraefikFileConfig), data[TraefikConfigKey])
	assert.Len(t, data, 3, "never a rotation candidate")
}

func TestTraefikFileConfig_PointsAtTheMountedPairWithoutAStore(t *testing.T) {
	assert.Contains(t, TraefikFileConfig, "certFile: "+TraefikMountPath+"/tls.crt")
	assert.Contains(t, TraefikFileConfig, "keyFile: "+TraefikMountPath+"/tls.key")
	assert.NotContains(t, TraefikFileConfig, "stores", "a second default store makes Traefik drop the default store")
}

func TestTraefikFileArgs_UseFilenameMode(t *testing.T) {
	args := strings.Join(TraefikFileArgs, " ")

	assert.Contains(t, args, "--providers.file.filename="+TraefikMountPath+"/"+TraefikConfigKey)
	assert.Contains(t, args, "--providers.file.watch=true")
	assert.NotContains(t, args, "providers.file.directory", "directory mode loads the file twice and logs the private key")
}

func TestTraefikRestartPatch_SetsThePodTemplateAnnotation(t *testing.T) {
	assert.JSONEq(t,
		`{"spec":{"template":{"metadata":{"annotations":{"`+TraefikCopyAnnotation+`":"12345"}}}}}`,
		string(TraefikRestartPatch("12345")))
}
