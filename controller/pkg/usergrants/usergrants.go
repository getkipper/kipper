// Package usergrants reads and writes the monitoring grant list stored beside
// the console roles in the kipper-users ConfigMap, for console-api and kip alike.
package usergrants

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

const (
	// ConfigMapName and Namespace locate the kipper-users ConfigMap.
	ConfigMapName = "kipper-users"
	Namespace     = "kipper-system"
	// UsersKey holds the console roles, a JSON object of email to role.
	UsersKey = "users"
	// MonitoringKey holds the emails granted monitoring, a JSON array.
	MonitoringKey = "monitoring"
)

// ErrUnknownUser is returned when granting monitoring to an email with no role.
var ErrUnknownUser = errors.New("unknown user")

// Monitoring returns the granted emails. A missing key means no grants.
func Monitoring(data map[string]string) (map[string]bool, error) {
	grants := make(map[string]bool)
	raw, ok := data[MonitoringKey]
	if !ok {
		return grants, nil
	}
	var emails []string
	if err := json.Unmarshal([]byte(raw), &emails); err != nil {
		return nil, fmt.Errorf("parsing %s in %s/%s: %w", MonitoringKey, Namespace, ConfigMapName, err)
	}
	for _, email := range emails {
		grants[email] = true
	}
	return grants, nil
}

// SetMonitoring grants or revokes monitoring for email in data. Granting
// requires the email to hold a console role.
func SetMonitoring(data map[string]string, email string, granted bool) error {
	grants, err := Monitoring(data)
	if err != nil {
		return err
	}
	if granted {
		userRoles, err := roles(data)
		if err != nil {
			return err
		}
		if userRoles[email] == "" {
			return fmt.Errorf("%w: %s", ErrUnknownUser, email)
		}
		grants[email] = true
	} else {
		delete(grants, email)
	}
	out, err := json.Marshal(sortedEmails(grants))
	if err != nil {
		return err
	}
	data[MonitoringKey] = string(out)
	return nil
}

// Merge adds backup roles and monitoring grants for users whose live role is
// empty or missing and returns their emails in sorted order. Existing roles and grants
// are preserved. Both maps contain kipper-users ConfigMap data, and live must
// be non-nil. If no users are added, it returns nil without changing live.
// Invalid role or monitoring JSON returns an error and leaves live unchanged.
func Merge(live, backup map[string]string) ([]string, error) {
	liveRoles, err := roles(live)
	if err != nil {
		return nil, err
	}
	backupRoles, err := roles(backup)
	if err != nil {
		return nil, err
	}
	backupGrants, err := Monitoring(backup)
	if err != nil {
		return nil, err
	}
	grants, err := Monitoring(live)
	if err != nil {
		return nil, err
	}

	var added []string
	for email, role := range backupRoles {
		if liveRoles[email] != "" {
			continue
		}
		liveRoles[email] = role
		added = append(added, email)
		if backupGrants[email] {
			grants[email] = true
		}
	}
	if len(added) == 0 {
		return nil, nil
	}
	sort.Strings(added)

	usersJSON, err := json.Marshal(liveRoles)
	if err != nil {
		return nil, err
	}
	monitoringJSON, err := json.Marshal(sortedEmails(grants))
	if err != nil {
		return nil, err
	}
	live[UsersKey] = string(usersJSON)
	live[MonitoringKey] = string(monitoringJSON)
	return added, nil
}

// roles returns the console roles in data. A missing key means no roles.
func roles(data map[string]string) (map[string]string, error) {
	roles := map[string]string{}
	if raw := data[UsersKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &roles); err != nil {
			return nil, fmt.Errorf("parsing %s in %s/%s: %w", UsersKey, Namespace, ConfigMapName, err)
		}
	}
	if roles == nil {
		roles = map[string]string{}
	}
	return roles, nil
}

func sortedEmails(grants map[string]bool) []string {
	emails := make([]string, 0, len(grants))
	for e := range grants {
		emails = append(emails, e)
	}
	sort.Strings(emails)
	return emails
}
