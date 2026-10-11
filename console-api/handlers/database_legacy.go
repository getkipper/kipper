package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// ServiceExistsFunc reports whether a Kipper Service exists in the given namespace.
type ServiceExistsFunc func(ctx context.Context, namespace, name string) (bool, error)

// CRServiceExists checks for Kipper Service custom resources using c.
func CRServiceExists(c crclient.Client) ServiceExistsFunc {
	return func(ctx context.Context, namespace, name string) (bool, error) {
		var svc kipperv1.Service
		err := c.Get(ctx, crclient.ObjectKey{Namespace: namespace, Name: name}, &svc)
		if k8serrors.IsNotFound(err) {
			return false, nil
		}
		return err == nil, err
	}
}

// RunLegacyDBConfigMapRetirement runs migration sweeps until ctx is canceled.
// Repeated sweeps handle concurrent writes and older replicas still writing
// ConfigMaps during a rolling upgrade.
func RunLegacyDBConfigMapRetirement(ctx context.Context, client kubernetes.Interface, serviceExists ServiceExistsFunc, every time.Duration) {
	for {
		RetireLegacyDBConfigMaps(ctx, client, serviceExists)
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// RetireLegacyDBConfigMaps migrates legacy query history and snippets to
// same-name Secrets to protect SQL that may contain credentials. It selects
// ConfigMaps by service name and payload shape, merges them with mergeLegacyData,
// and deletes them only after a successful merge, using UID and resource-version
// preconditions to preserve concurrent ConfigMap changes.
func RetireLegacyDBConfigMaps(ctx context.Context, client kubernetes.Interface, serviceExists ServiceExistsFunc) {
	list, err := client.CoreV1().ConfigMaps("").List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("db legacy: listing ConfigMaps: %v", err)
		return
	}
	for i := range list.Items {
		cm := &list.Items[i]
		svc, history := legacyDBService(cm.Name)
		if svc == "" {
			continue
		}
		if exists, err := serviceExists(ctx, cm.Namespace, svc); err != nil || !exists {
			if err != nil {
				log.Printf("db legacy: checking service %s/%s: %v", cm.Namespace, svc, err)
			}
			continue
		}
		if !legacyDBPayload(cm, history) {
			log.Printf("db legacy: skipping %s/%s: payload does not match the legacy database format", cm.Namespace, cm.Name)
			continue
		}
		if err := retry.OnError(retry.DefaultRetry, func(err error) bool {
			return k8serrors.IsConflict(err) || k8serrors.IsAlreadyExists(err)
		}, func() error { return moveIntoSecret(ctx, client, cm) }); err != nil {
			log.Printf("db legacy: moving %s/%s into a Secret: %v", cm.Namespace, cm.Name, err)
			continue
		}
		uid, rv := cm.UID, cm.ResourceVersion
		if err := client.CoreV1().ConfigMaps(cm.Namespace).Delete(ctx, cm.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv},
		}); err != nil && !k8serrors.IsNotFound(err) {
			log.Printf("db legacy: deleting %s/%s: %v", cm.Namespace, cm.Name, err)
			continue
		}
		log.Printf("db legacy: moved %s/%s into a Secret", cm.Namespace, cm.Name)
	}
}

// legacyDBService extracts the service name and history flag from a legacy
// ConfigMap name. An empty service name means the ConfigMap should be skipped.
func legacyDBService(name string) (svc string, history bool) {
	if s, ok := strings.CutPrefix(name, historySecretPrefix); ok {
		return s, true
	}
	if s, ok := strings.CutPrefix(name, snippetsSecretPrefix); ok {
		return s, false
	}
	return "", false
}

// legacyDBPayload accepts text-only ConfigMaps whose values are nonempty
// history arrays or snippet objects, with nonempty SQL in every entry.
// Empty ConfigMaps are also accepted.
func legacyDBPayload(cm *corev1.ConfigMap, history bool) bool {
	if len(cm.BinaryData) > 0 {
		return false
	}
	for _, v := range cm.Data {
		if history {
			var entries []*historyEntry
			if json.Unmarshal([]byte(v), &entries) != nil || len(entries) == 0 {
				return false
			}
			for _, e := range entries {
				if e == nil || e.SQL == "" {
					return false
				}
			}
			continue
		}
		var s snippet
		if json.Unmarshal([]byte(v), &s) != nil || s.SQL == "" {
			return false
		}
	}
	return true
}

// migratedAnnotation stores copied-value digests so later sweeps can detect
// changes to migrated Secret values.
const migratedAnnotation = "kipper.run/migrated-from-configmap"

func moveIntoSecret(ctx context.Context, client kubernetes.Interface, cm *corev1.ConfigMap) error {
	secrets := client.CoreV1().Secrets(cm.Namespace)
	secret, err := secrets.Get(ctx, cm.Name, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		labels := map[string]string{kipperLabel: kipperValue}
		if strings.HasPrefix(cm.Name, historySecretPrefix) {
			labels["kipper.run/db-history"] = "true"
		}
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: cm.Name, Namespace: cm.Namespace, Labels: labels}}
		if !mergeLegacyData(cm, secret) {
			return nil
		}
		_, err = secrets.Create(ctx, secret, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if !mergeLegacyData(cm, secret) {
		return nil
	}
	_, err = secrets.Update(ctx, secret, metav1.UpdateOptions{})
	return err
}

// mergeLegacyData reports whether it copied any values into the Secret.
// It adds previously uncopied keys and refreshes values matching their recorded
// migration digest, preserving other Secret values and deletions.
func mergeLegacyData(cm *corev1.ConfigMap, secret *corev1.Secret) bool {
	copied := map[string]string{}
	if raw := secret.Annotations[migratedAnnotation]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &copied)
	}
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	changed := false
	for k, v := range cm.Data {
		current, exists := secret.Data[k]
		// Preserve deletions made after an earlier migration sweep.
		if !exists && copied[k] != "" {
			continue
		}
		if exists && (copied[k] != digest(current) || string(current) == v) {
			continue
		}
		secret.Data[k] = []byte(v)
		copied[k] = digest([]byte(v))
		changed = true
	}
	if changed {
		raw, _ := json.Marshal(copied)
		if secret.Annotations == nil {
			secret.Annotations = map[string]string{}
		}
		secret.Annotations[migratedAnnotation] = string(raw)
	}
	return changed
}

func digest(v []byte) string {
	sum := sha256.Sum256(v)
	return hex.EncodeToString(sum[:])
}
