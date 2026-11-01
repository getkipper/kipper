package usergrants

import (
	"errors"
	"strings"
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

func TestMerge_AddsTheBackupsUsersAndKeepsLiveRoles(t *testing.T) {
	live := map[string]string{UsersKey: `{"admin@test.com":"admin"}`, MonitoringKey: `[]`}
	backup := map[string]string{
		UsersKey:      `{"admin@test.com":"viewer","dev@test.com":"deployer","ops@test.com":"admin"}`,
		MonitoringKey: `["admin@test.com","dev@test.com"]`,
	}

	added, err := Merge(live, backup)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Join(added, ",") != "dev@test.com,ops@test.com" {
		t.Errorf("added = %v, want the two emails the cluster lacks, sorted", added)
	}
	if live[UsersKey] != `{"admin@test.com":"admin","dev@test.com":"deployer","ops@test.com":"admin"}` {
		t.Errorf("users = %s, want the live admin role kept", live[UsersKey])
	}
	if live[MonitoringKey] != `["dev@test.com"]` {
		t.Errorf("monitoring = %s, want grants only for the added users", live[MonitoringKey])
	}
}

func TestMerge_WithNothingNewLeavesLiveUntouched(t *testing.T) {
	live := map[string]string{UsersKey: `{"admin@test.com": "admin"}`}

	added, err := Merge(live, map[string]string{UsersKey: `{"admin@test.com":"viewer"}`, MonitoringKey: `["admin@test.com"]`})
	if err != nil {
		t.Fatal(err)
	}

	if len(added) != 0 || live[UsersKey] != `{"admin@test.com": "admin"}` || live[MonitoringKey] != "" {
		t.Errorf("added = %v, live = %v; want no change", added, live)
	}
}

func TestMerge_RefusesUnreadableData(t *testing.T) {
	for name, pair := range map[string][2]map[string]string{
		"live users":        {{UsersKey: `[`}, {UsersKey: `{"a@test.com":"admin"}`}},
		"backup users":      {{UsersKey: `{}`}, {UsersKey: `[`}},
		"backup monitoring": {{UsersKey: `{}`}, {UsersKey: `{"a@test.com":"admin"}`, MonitoringKey: `{`}},
	} {
		live := pair[0]
		before := live[UsersKey]
		if _, err := Merge(live, pair[1]); err == nil {
			t.Errorf("%s: unreadable data accepted", name)
		}
		if live[UsersKey] != before {
			t.Errorf("%s: live changed after an error", name)
		}
	}
}

func TestMerge_ReadsANullRoleMapAsNoRoles(t *testing.T) {
	live := map[string]string{UsersKey: `null`}

	added, err := Merge(live, map[string]string{UsersKey: `{"dev@test.com":"deployer"}`})
	if err != nil {
		t.Fatal(err)
	}

	if len(added) != 1 || live[UsersKey] != `{"dev@test.com":"deployer"}` {
		t.Errorf("added = %v, users = %s", added, live[UsersKey])
	}
}
