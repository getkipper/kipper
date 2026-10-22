package migration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// fakeTool records its arguments and how it received the password. It stands
// in for kip, which runs everything after "--" with the pod's environment, and
// for each engine tool the guide calls.
const fakeTool = `#!/bin/sh
name="$(basename "$0")"
if [ "$name" = kip ]; then
	while [ "$1" != "--" ]; do shift; done
	shift
	exec "$@"
fi
printf '%s\n' "$*" >>"$LOG/args"
for a in "$@"; do
	case "$a" in --config=*) cat "${a#--config=}" >"$LOG/$name.config" ;; esac
done
[ -n "$MYSQL_PWD" ] && printf '%s' "$MYSQL_PWD" >"$LOG/$name.mysqlpwd"
cat >/dev/null
exit 0
`

// runManualSteps runs each kip command of a guide through sh with fakes for
// kip and the engine tools, and returns the directory holding their records.
func runManualSteps(t *testing.T, svcType, password string) string {
	t.Helper()
	bin, log, work := t.TempDir(), t.TempDir(), t.TempDir()
	for _, name := range []string{"kip", "mongodump", "mongorestore", "mysqldump", "mysql"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(fakeTool), 0o755); err != nil { //nolint:gosec // test-local executable
			t.Fatal(err)
		}
	}
	for _, step := range buildManualMigrationSteps(svcType, "orders", "shop") {
		if !strings.HasPrefix(step, "kip ") {
			continue
		}
		// Reuse the exported file in the work directory to simulate the copy step.
		cmd := exec.Command("sh", "-c", strings.ReplaceAll(step, "/tmp/", "")) //nolint:gosec // test-local commands from the guide
		cmd.Dir = work
		cmd.Env = append(os.Environ(),
			"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"LOG="+log,
			"MONGO_INITDB_ROOT_USERNAME=root",
			"MONGO_INITDB_ROOT_PASSWORD="+password,
			"MYSQL_ROOT_PASSWORD="+password,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", step, err, out)
		}
	}
	return log
}

func TestManualMongoStepsKeepThePasswordOutOfTheArguments(t *testing.T) {
	password := "guide-sentinel 'q' #x: y"
	log := runManualSteps(t, "mongodb", password)

	args, err := os.ReadFile(filepath.Join(log, "args"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), "guide-sentinel") {
		t.Errorf("the password reached a tool's arguments:\n%s", args)
	}
	for _, tool := range []string{"mongodump", "mongorestore"} {
		raw, err := os.ReadFile(filepath.Join(log, tool+".config"))
		if err != nil {
			t.Fatalf("%s got no config file: %v", tool, err)
		}
		var config struct {
			Password string `json:"password"`
		}
		if err := yaml.UnmarshalStrict(raw, &config); err != nil || config.Password != password {
			t.Errorf("%s read password %q (%v), want %q", tool, config.Password, err, password)
		}
	}
}

func TestManualMySQLStepsAuthenticateWithoutArguments(t *testing.T) {
	password := "guide-sentinel 'q' #x: y"
	log := runManualSteps(t, "mysql", password)

	args, err := os.ReadFile(filepath.Join(log, "args"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), "guide-sentinel") {
		t.Errorf("the password reached a tool's arguments:\n%s", args)
	}
	for _, tool := range []string{"mysqldump", "mysql"} {
		got, err := os.ReadFile(filepath.Join(log, tool+".mysqlpwd"))
		if err != nil || string(got) != password {
			t.Errorf("%s must receive the root password through MYSQL_PWD, got %q (%v)", tool, got, err)
		}
	}
}
