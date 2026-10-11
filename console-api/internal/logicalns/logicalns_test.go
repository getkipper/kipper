package logicalns

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func credentials(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{Data: data}
}

func TestCmd_RabbitMQ(t *testing.T) {
	creds := credentials(map[string][]byte{"USERNAME": []byte("kipper")})
	cmd, err := Cmd("rabbitmq", "orders", creds)
	if err != nil {
		t.Fatalf("logicalNamespaceCmd: %v", err)
	}
	// Positional arguments keep user input separate from the shell script.
	if len(cmd) < 5 || cmd[0] != "sh" || cmd[1] != "-c" || cmd[3] != "--" {
		t.Fatalf("expected `sh -c <script> -- <vhost> <user>`, got %v", cmd)
	}
	if cmd[4] != "orders" {
		t.Errorf("expected vhost as positional arg 1, got %q", cmd[4])
	}
	if cmd[5] != "kipper" {
		t.Errorf("expected username as positional arg 2, got %q", cmd[5])
	}
	script := cmd[2]
	for _, want := range []string{"list_vhosts", `add_vhost -- "$1"`, `set_permissions -p "$1" "$2"`} {
		if !strings.Contains(script, want) {
			t.Errorf("expected script to contain %q, got %q", want, script)
		}
	}
	if strings.Contains(script, "orders") {
		t.Errorf("vhost value must not be interpolated into the script, got %q", script)
	}
}

func TestValidName(t *testing.T) {
	cases := map[string]bool{
		"orders":                true,
		"order_service_test":    true,
		"order-service":         true,
		"v1.5":                  true,
		"orders/sub":            true,
		"":                      false,
		"orders; rm -rf /":      false,
		"orders\"":              false,
		"orders`whoami`":        false,
		"orders$(whoami)":       false,
		"orders\\":              false,
		strings.Repeat("a", 64): false,
	}
	for input, want := range cases {
		if got := ValidName(input); got != want {
			t.Errorf("ValidName(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestCmd_RejectsShellMetacharacters(t *testing.T) {
	creds := credentials(map[string][]byte{"USERNAME": []byte("kipper")})
	_, err := Cmd("rabbitmq", `orders"; rm -rf / #`, creds)
	if err == nil {
		t.Fatal("expected error for vhost with shell metacharacters, got nil")
	}
}

func TestCmd_MySQLCreatesAsRootAndGrantsTheServiceUser(t *testing.T) {
	cmd, err := Cmd("mysql", "orders", credentials(map[string][]byte{"USERNAME": []byte("kipper")}))
	if err != nil {
		t.Fatal(err)
	}
	script := cmd[2]
	if !strings.Contains(script, "-uroot") {
		t.Errorf("the database must be created as root, got %q", script)
	}
	if !strings.Contains(script, "GRANT ALL PRIVILEGES ON \\`$1\\`.* TO \\`$2\\`@'%'") {
		t.Errorf("the service user must be granted the new database, got %q", script)
	}
	if cmd[4] != "orders" || cmd[5] != "kipper" {
		t.Errorf("values travel as positional arguments, got %v", cmd[3:])
	}
}

func TestCmd_RejectsAnUnsafeServiceUsername(t *testing.T) {
	for _, svcType := range []string{"postgres", "mysql", "rabbitmq"} {
		for _, user := range []string{"kipper`@'%'; DROP DATABASE app; -- ", "kip per", ""} {
			if _, err := Cmd(svcType, "orders", credentials(map[string][]byte{"USERNAME": []byte(user)})); err == nil {
				t.Errorf("%s: expected an error for username %q", svcType, user)
			}
		}
	}
}
