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
		roles := map[string]string{}
		if raw := data[UsersKey]; raw != "" {
			if err := json.Unmarshal([]byte(raw), &roles); err != nil {
				return fmt.Errorf("parsing %s in %s/%s: %w", UsersKey, Namespace, ConfigMapName, err)
			}
		}
		if roles[email] == "" {
			return fmt.Errorf("%w: %s", ErrUnknownUser, email)
		}
		grants[email] = true
	} else {
		delete(grants, email)
	}
	emails := make([]string, 0, len(grants))
	for e := range grants {
		emails = append(emails, e)
	}
	sort.Strings(emails)
	out, err := json.Marshal(emails)
	if err != nil {
		return err
	}
	data[MonitoringKey] = string(out)
	return nil
}
