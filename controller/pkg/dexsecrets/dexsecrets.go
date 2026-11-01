// Package dexsecrets manages the Secret that holds SSO connectors' client
// secrets. Dex loads its keys as environment variables, and connector configs
// refer to them as $KEY.
package dexsecrets

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const (
	Namespace     = "dex"
	SecretName    = "dex-connector-secrets" //nolint:gosec // G101: Kubernetes Secret object name, not a credential value
	ConfigMapName = "dex-config"

	// UnusedSinceAnnotation holds a JSON object that maps each unused key to
	// the RFC 3339 time Retire first found it unused.
	UnusedSinceAnnotation = "kipper.run/unused-since"

	// UnusedGrace is how long a key stays unused before Retire deletes it.
	UnusedGrace = time.Hour
)

var generatedKey = regexp.MustCompile(`^KIPPER_[A-Z0-9_]+_CLIENT_SECRET_[A-Z2-7]{12}$`)

// NewKey returns an environment variable name for connectorID's client secret.
// It uppercases connectorID and replaces characters outside A-Z and 0-9 with
// underscores. A random suffix protects values referenced by saved configs
// and concurrent updates from being overwritten.
func NewKey(connectorID string) string {
	name := strings.Map(func(r rune) rune {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, strings.ToUpper(connectorID))
	return "KIPPER_" + name + "_CLIENT_SECRET_" + rand.Text()[:12]
}

// generated reports whether key matches the naming pattern eligible for retirement.
func generated(key string) bool {
	return generatedKey.MatchString(key)
}

// Retire checks Secret entries matching the generated key pattern against all
// values in the Dex ConfigMap's Data. Any occurrence of a key's name counts as
// use. An absent key name gets an unused timestamp based on now. If its existing
// timestamp is at least UnusedGrace old, the entry is deleted. Missing or invalid
// timestamps start a new grace period. Marks for keys in use, missing entries,
// and keys outside the generated pattern are removed.
//
// Retire returns deleted keys in sorted order, or nil if none were deleted.
// A missing Secret returns nil without error. Read and update errors are returned.
// An unreadable config or a Secret write conflict leaves the stored Secret
// unchanged. Conflicts are returned without retrying, so the caller can start a
// new pass with fresh reads. The ConfigMap and Secret are read separately, so
// the grace period allows writers time to save a config after storing its key.
func Retire(ctx context.Context, client kubernetes.Interface, now time.Time) ([]string, error) {
	secrets := client.CoreV1().Secrets(Namespace)
	secret, err := secrets.Get(ctx, SecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s/%s: %w", Namespace, SecretName, err)
	}
	cm, err := client.CoreV1().ConfigMaps(Namespace).Get(ctx, ConfigMapName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading %s/%s: %w", Namespace, ConfigMapName, err)
	}
	var config strings.Builder
	for _, v := range cm.Data {
		config.WriteString(v)
		config.WriteByte('\n')
	}

	marks := readMarks(secret)
	kept := map[string]string{}
	var deleted []string
	for key := range secret.Data {
		if !generated(key) || strings.Contains(config.String(), key) {
			continue
		}
		since, err := time.Parse(time.RFC3339, marks[key])
		switch {
		case err != nil:
			kept[key] = now.UTC().Format(time.RFC3339)
		case now.Sub(since) >= UnusedGrace:
			delete(secret.Data, key)
			deleted = append(deleted, key)
		default:
			kept[key] = marks[key]
		}
	}
	if len(deleted) == 0 && sameMarks(marks, kept) && (len(kept) > 0 || secret.Annotations[UnusedSinceAnnotation] == "") {
		return nil, nil
	}
	if err := writeMarks(secret, kept); err != nil {
		return nil, err
	}
	if _, err := secrets.Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		return nil, fmt.Errorf("updating %s/%s: %w", Namespace, SecretName, err)
	}
	sort.Strings(deleted)
	return deleted, nil
}

// Store adds or replaces values in the Secret, creating it if needed, and clears
// the written keys' unused marks when the annotation is readable. An unreadable
// annotation is preserved for Retire to replace. Writers must store a key before
// saving a config that uses it. Write conflicts and concurrent creation trigger
// fresh reads to preserve other keys. Read errors, other write errors, and errors
// remaining after retries are returned.
func Store(ctx context.Context, client kubernetes.Interface, values map[string]string) error {
	secrets := client.CoreV1().Secrets(Namespace)
	raced := func(err error) bool { return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) }
	return retry.OnError(retry.DefaultRetry, raced, func() error {
		secret, err := secrets.Get(ctx, SecretName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: SecretName, Namespace: Namespace}}
			setValues(secret, values)
			_, err = secrets.Create(ctx, secret, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		setValues(secret, values)
		if raw := secret.Annotations[UnusedSinceAnnotation]; raw != "" {
			marks := map[string]string{}
			// Retire owns recovery from an unreadable annotation.
			if json.Unmarshal([]byte(raw), &marks) == nil {
				for k := range values {
					delete(marks, k)
				}
				if err := writeMarks(secret, marks); err != nil {
					return err
				}
			}
		}
		_, err = secrets.Update(ctx, secret, metav1.UpdateOptions{})
		return err
	})
}

// EnsureDexReads adds an environment reference to the Secret in the dex
// Deployment's dex container, triggering a rollout. The added reference is
// optional so Dex can start before the Secret exists. An existing reference
// is left unchanged. It returns read or update errors, or an error if the
// Deployment has no dex container.
func EnsureDexReads(ctx context.Context, client kubernetes.Interface) error {
	deploys := client.AppsV1().Deployments(Namespace)
	deploy, err := deploys.Get(ctx, "dex", metav1.GetOptions{})
	if err != nil {
		return err
	}
	containers := deploy.Spec.Template.Spec.Containers
	for i := range containers {
		if containers[i].Name != "dex" {
			continue
		}
		for _, from := range containers[i].EnvFrom {
			if from.SecretRef != nil && from.SecretRef.Name == SecretName {
				return nil
			}
		}
		optional := true
		containers[i].EnvFrom = append(containers[i].EnvFrom, corev1.EnvFromSource{SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: SecretName},
			Optional:             &optional,
		}})
		_, err = deploys.Update(ctx, deploy, metav1.UpdateOptions{})
		return err
	}
	return fmt.Errorf("the dex Deployment has no container named dex")
}

func setValues(secret *corev1.Secret, values map[string]string) {
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	for k, v := range values {
		secret.Data[k] = []byte(v)
	}
}

// readMarks returns the Secret's unused marks, or none when the annotation is
// missing or unreadable.
func readMarks(secret *corev1.Secret) map[string]string {
	marks := map[string]string{}
	if raw := secret.Annotations[UnusedSinceAnnotation]; raw != "" {
		if json.Unmarshal([]byte(raw), &marks) != nil {
			return map[string]string{}
		}
	}
	return marks
}

func writeMarks(secret *corev1.Secret, marks map[string]string) error {
	if len(marks) == 0 {
		delete(secret.Annotations, UnusedSinceAnnotation)
		return nil
	}
	raw, err := json.Marshal(marks)
	if err != nil {
		return err
	}
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[UnusedSinceAnnotation] = string(raw)
	return nil
}

func sameMarks(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
