package ai

import (
	"strings"
	"testing"
)

func TestTheMinIOScriptsKeepThePasswordOutOfArguments(t *testing.T) {
	for name, script := range map[string]string{
		"list":   listMinIOBackupsScript(),
		"remove": removeMinIOBackupsScript([]string{"velero-daily-apps-20261021030000"}),
	} {
		if strings.Contains(script, "mc alias set") {
			t.Errorf("%s: mc alias set takes the password as an argument:\n%s", name, script)
		}
		if !strings.Contains(script, `export MC_HOST_kipper="http://$MINIO_ROOT_USER:$MINIO_ROOT_PASSWORD@minio.velero.svc:9000"`) {
			t.Errorf("%s: the credentials should reach mc through MC_HOST_kipper:\n%s", name, script)
		}
	}
}

func TestTheRemoveScriptStripsSlashesFromNames(t *testing.T) {
	script := removeMinIOBackupsScript([]string{"../velero"})

	if !strings.Contains(script, "kipper/velero/backups/..velero ") {
		t.Errorf("slashes should be stripped:\n%s", script)
	}
}
