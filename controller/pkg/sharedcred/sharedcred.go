// Package sharedcred stores the admin-managed shared git credentials. The whole
// list lives in one Secret in kipper-system and is read by the settings handlers
// that manage it, the builder that resolves a shared credential at build time,
// and kip, so a shared token never has to be copied into a tenant namespace.
//
// It lives here rather than in one of those modules because the list is a
// security control: an entry's allowed projects decide who may build with the
// token. A second definition of the shape would drop the field it does not
// know about, which is how a writer erases every grant on the list.
package sharedcred

import (
	"context"
	"encoding/json"
	"fmt"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// ConfigSecretName is the Secret holding the shared-credential list.
	ConfigSecretName = "kipper-git-credentials" //nolint:gosec // k8s Secret object name, not a credential value
	// Namespace is where the list Secret lives; tenants cannot read it.
	Namespace = "kipper-system"
	// managedByLabel/managedByValue mark the Secret as Kipper-owned.
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "kipper"
	dataKey        = "credentials"
)

// Entry is one shared git credential. AllowedProjects lists the project names
// permitted to build with it; an empty list denies every project (fail closed),
// so a shared token is never usable until an admin explicitly grants a project.
//
// The field is written even when it is empty, because an empty list and an
// absent one mean different things: nobody may build with this, against nobody
// has ever decided. Only the second is a credential an upgrade may seed from the
// apps that reference it.
type Entry struct {
	Name            string   `json:"name"`
	Server          string   `json:"server"`
	Username        string   `json:"username"`
	Token           string   `json:"token,omitempty"`
	AllowedProjects []string `json:"allowedProjects"`
}

// AllowsProject reports whether project is on the entry's allow-list.
func (e Entry) AllowsProject(project string) bool {
	if project == "" {
		return false
	}
	for _, p := range e.AllowedProjects {
		if p == project {
			return true
		}
	}
	return false
}

// Load returns the shared-credential list. A missing list Secret (or a Secret
// with no credentials key) is not an error — it means no shared credentials are
// configured — and returns (nil, nil). A read or parse failure returns an error
// so a caller enforcing on the list can fail closed rather than mistake an
// unreadable list for an empty one.
func Load(ctx context.Context, client kubernetes.Interface) ([]Entry, error) {
	secret, err := client.CoreV1().Secrets(Namespace).Get(ctx, ConfigSecretName, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading shared git credentials: %w", err)
	}
	data, ok := secret.Data[dataKey]
	if !ok {
		return nil, nil
	}
	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parsing shared git credentials: %w", err)
	}
	return entries, nil
}

// Find returns the entry with the given name, or nil if none matches.
func Find(entries []Entry, name string) *Entry {
	for i := range entries {
		if entries[i].Name == name {
			return &entries[i]
		}
	}
	return nil
}
