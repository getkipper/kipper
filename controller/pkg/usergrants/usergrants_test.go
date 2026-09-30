package usergrants

import (
	"errors"
	"testing"
)

func data(monitoring string) map[string]string {
	d := map[string]string{UsersKey: users}
	if monitoring != "" {
		d[MonitoringKey] = monitoring
	}
	return d
}

const users = `{"admin@test.com":"admin","dev@test.com":"deployer"}`

func TestMonitoring(t *testing.T) {
	got, err := Monitoring(data(`["dev@test.com"]`))
	if err != nil || !got["dev@test.com"] || len(got) != 1 {
		t.Errorf("Monitoring = %v, %v", got, err)
	}
	if got, err := Monitoring(data("")); err != nil || len(got) != 0 {
		t.Errorf("missing key: %v, %v; want no grants", got, err)
	}
	if _, err := Monitoring(data(`{"x":1}`)); err == nil {
		t.Error("malformed list accepted")
	}
}

func TestSetMonitoring(t *testing.T) {
	d := data("")
	if err := SetMonitoring(d, "dev@test.com", true); err != nil {
		t.Fatal(err)
	}
	if err := SetMonitoring(d, "admin@test.com", true); err != nil {
		t.Fatal(err)
	}
	if d[MonitoringKey] != `["admin@test.com","dev@test.com"]` {
		t.Errorf("stored = %s, want a sorted JSON array", d[MonitoringKey])
	}
	if err := SetMonitoring(d, "dev@test.com", false); err != nil {
		t.Fatal(err)
	}
	if d[MonitoringKey] != `["admin@test.com"]` {
		t.Errorf("after revoke = %s", d[MonitoringKey])
	}
	if d[UsersKey] != users {
		t.Error("users key changed")
	}
}

func TestSetMonitoring_UnknownUser(t *testing.T) {
	d := data("")
	if err := SetMonitoring(d, "stranger@test.com", true); !errors.Is(err, ErrUnknownUser) {
		t.Errorf("err = %v, want ErrUnknownUser", err)
	}
	if err := SetMonitoring(d, "stranger@test.com", false); err != nil {
		t.Errorf("revoking for an unknown user should be a no-op, got %v", err)
	}
}

func TestSetMonitoring_RefusesMalformedList(t *testing.T) {
	if err := SetMonitoring(data(`"oops"`), "dev@test.com", true); err == nil {
		t.Error("grant written over a malformed list")
	}
}
