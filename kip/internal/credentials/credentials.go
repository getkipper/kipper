// Package credentials reads Kipper git and container-registry credentials
// stored as Kubernetes Secrets in the kipper-system namespace.
package credentials

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Type identifies which credential store a Credential belongs to.
type Type string

const (
	TypeGit      Type = "git"
	TypeRegistry Type = "registry"

	systemNamespace          = "kipper-system"
	gitCredentialsConfigName = "kipper-git-credentials" //nolint:gosec // kubernetes Secret object name, not a credential value
	registryConfigName       = "kipper-registries"
)

// Credential is a single git or registry credential entry.
type Credential struct {
	Type     Type
	Name     string
	Server   string
	Username string
	// Value is the token (for git) or password (for registry).
	Value string
}

// List returns all configured credentials across both stores. Pass an empty
// filter to return everything, or a specific Type to limit the result.
func List(ctx context.Context, client kubernetes.Interface, filter Type) ([]Credential, error) {
	var out []Credential

	if filter == "" || filter == TypeGit {
		gits, err := loadGit(ctx, client)
		if err != nil {
			return nil, err
		}
		out = append(out, gits...)
	}

	if filter == "" || filter == TypeRegistry {
		regs, err := loadRegistries(ctx, client)
		if err != nil {
			return nil, err
		}
		out = append(out, regs...)
	}

	return out, nil
}

// Get returns the credential with the given name. If preferred is empty and
// the name exists in both stores, it returns an ErrAmbiguous error.
func Get(ctx context.Context, client kubernetes.Interface, name string, preferred Type) (*Credential, error) {
	all, err := List(ctx, client, preferred)
	if err != nil {
		return nil, err
	}

	var matches []Credential
	for _, c := range all {
		if c.Name == name {
			matches = append(matches, c)
		}
	}

	switch len(matches) {
	case 0:
		if preferred != "" {
			return nil, fmt.Errorf("%s credential %q not found", preferred, name)
		}
		return nil, fmt.Errorf("credential %q not found", name)
	case 1:
		c := matches[0]
		return &c, nil
	default:
		return nil, &AmbiguousError{Name: name, Types: typesOf(matches)}
	}
}

// GetForApp returns the git credential a Kipper app uses to clone its source
// repository. Unlike the global credential store, an app's git token lives in
// a Secret in the app's own namespace (named by spec.git.credentialsSecret)
// and never appears in List. The caller resolves the namespace and secret
// name from the App CR; this reads the secret's token key.
func GetForApp(ctx context.Context, client kubernetes.Interface, namespace, secretName, appName string) (*Credential, error) {
	secret, err := client.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, fmt.Errorf("git credentials secret %q not found in namespace %q", secretName, namespace)
		}
		return nil, fmt.Errorf("reading git credentials secret %q: %w", secretName, err)
	}

	token, ok := secret.Data["token"]
	if !ok || len(token) == 0 {
		return nil, fmt.Errorf("secret %q has no token key", secretName)
	}

	return &Credential{
		Type:   TypeGit,
		Name:   secretName,
		Server: appName,
		Value:  string(token),
	}, nil
}

// AmbiguousError is returned when a credential name matches entries in more
// than one store and the caller did not specify which one.
type AmbiguousError struct {
	Name  string
	Types []Type
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("credential %q exists in multiple stores (%v): specify --type", e.Name, e.Types)
}

func typesOf(cs []Credential) []Type {
	seen := map[Type]bool{}
	var out []Type
	for _, c := range cs {
		if !seen[c.Type] {
			seen[c.Type] = true
			out = append(out, c.Type)
		}
	}
	return out
}

type gitEntry struct {
	Name     string `json:"name"`
	Server   string `json:"server"`
	Username string `json:"username"`
	Token    string `json:"token,omitempty"`
}

type registryEntry struct {
	Name     string `json:"name"`
	Server   string `json:"server"`
	Username string `json:"username"`
	Password string `json:"password,omitempty"`
}

func loadGit(ctx context.Context, client kubernetes.Interface) ([]Credential, error) {
	secret, err := client.CoreV1().Secrets(systemNamespace).Get(ctx, gitCredentialsConfigName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading git credentials: %w", err)
	}

	data, ok := secret.Data["credentials"]
	if !ok {
		return nil, nil
	}

	var entries []gitEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parsing git credentials: %w", err)
	}

	out := make([]Credential, 0, len(entries))
	for _, e := range entries {
		out = append(out, Credential{
			Type:     TypeGit,
			Name:     e.Name,
			Server:   e.Server,
			Username: e.Username,
			Value:    e.Token,
		})
	}
	return out, nil
}

func loadRegistries(ctx context.Context, client kubernetes.Interface) ([]Credential, error) {
	secret, err := client.CoreV1().Secrets(systemNamespace).Get(ctx, registryConfigName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading registry credentials: %w", err)
	}

	data, ok := secret.Data["registries"]
	if !ok {
		return nil, nil
	}

	var entries []registryEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parsing registry credentials: %w", err)
	}

	out := make([]Credential, 0, len(entries))
	for _, e := range entries {
		out = append(out, Credential{
			Type:     TypeRegistry,
			Name:     e.Name,
			Server:   e.Server,
			Username: e.Username,
			Value:    e.Password,
		})
	}
	return out, nil
}

// Mask returns a display-safe version of a secret value: first four chars
// followed by bullets. Mirrors the console-api mask format so both surfaces
// agree on what a masked credential looks like.
func Mask(s string) string {
	if len(s) <= 8 {
		return "••••••••"
	}
	return s[:4] + "••••••••"
}
