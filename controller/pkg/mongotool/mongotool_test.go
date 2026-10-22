package mongotool

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// testPasswords includes values a bare YAML scalar would misread or reject.
var testPasswords = []string{
	"5f2c9e0a7b",
	"pa#ss", "pa #ss", "a: b", "*star", "&anchor", "{x}", "[y]", "!tag", "%percent", "@at", "`tick`",
	"'single'", `"double"`, `back\slash`, `\x41`, "yes", "null", "~", "0x10", "1e3",
	" leading", "trailing ", "tab\there", "new\nline", "trailing\n", "bell\a", "del\x7f",
	"ünïcødé €", "$HOME", "-dash", "? q", "| pipe", "> gt",
}

func TestScriptConfigCarriesAnyPassword(t *testing.T) {
	// The invocation stands in for a MongoDB tool and prints the config file
	// it was given, which the tools parse as YAML.
	printConfig := `f() { cat "${1#--config=}"; }; f`
	for _, password := range testPasswords {
		cmd := exec.Command("sh", "-c", Script(printConfig)) //nolint:gosec // test-local script built from constants
		cmd.Env = append(os.Environ(), "MONGO_INITDB_ROOT_PASSWORD="+password)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("password %q: %v", password, err)
		}
		var config struct {
			Password string `json:"password"`
		}
		if err := yaml.UnmarshalStrict(out, &config); err != nil {
			t.Errorf("password %q: the config is not valid YAML: %v\n%s", password, err, out)
			continue
		}
		if config.Password != password {
			t.Errorf("password %q came back as %q", password, config.Password)
		}
	}
}

func TestScriptKeepsThePasswordOutOfTheArguments(t *testing.T) {
	script := Script("mongodump --archive")
	for _, unwanted := range []string{" -p ", "--password", "'"} {
		if strings.Contains(script, unwanted) {
			t.Errorf("the script must not contain %q: %s", unwanted, script)
		}
	}
	for _, want := range []string{`--config="$c"`, `-u "$MONGO_INITDB_ROOT_USERNAME"`, "--authenticationDatabase admin", "chmod 600"} {
		if !strings.Contains(script, want) {
			t.Errorf("the script must contain %q: %s", want, script)
		}
	}
}

func TestScriptKeepsTheToolStatusAndRemovesTheConfig(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("sh", "-c", Script(`f() { echo "${1#--config=}"; return 3; }; f`)) //nolint:gosec // test-local script built from constants
	cmd.Env = append(os.Environ(), "MONGO_INITDB_ROOT_PASSWORD=x", "TMPDIR="+dir)
	out, err := cmd.Output()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 3 {
		t.Fatalf("the script must exit with the tool's status 3, got %v", err)
	}
	if _, statErr := os.Stat(strings.TrimSpace(string(out))); !os.IsNotExist(statErr) {
		t.Errorf("the config file must be removed, stat says %v", statErr)
	}
}
