package migration

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A migration overwrites a Secret that already existed on the target whenever
// the operator confirmed the project overwrite. Recording only the Secrets the
// run created is not enough to undo that: abort would delete the new arrivals
// and leave every pre-existing Secret holding the source's credentials, with no
// copy of what was there before. So the original is journaled to a backup
// Secret in the same namespace before the first overwrite, restored on abort,
// and dropped once the migration commits.
const (
	//nolint:gosec // G101: these are label and annotation key names, not credentials
	secretBackupSessionLabel = "kipper.run/migration-backup-session"
	//nolint:gosec // G101: annotation key name, not a credential
	secretBackupNameAnn = "kipper.run/migration-backup-of"
	//nolint:gosec // G101: annotation key name, not a credential
	secretBackupTypeAnn = "kipper.run/migration-backup-type"
	secretBackupPrefix  = "kipper-migration-backup-"
	//nolint:gosec // G101: annotation key name, not a credential
	secretBackupMetaAnn = "kipper.run/migration-backup-metadata"
	//nolint:gosec // G101: annotation key name, not a credential
	secretBackupMACAnn = "kipper.run/migration-backup-mac"
)

// journaledMeta is the original Secret's metadata. It travels as one JSON
// annotation value rather than as individual annotation keys (a label like
// app.kubernetes.io/managed-by would make an illegal two-slash key) and rather
// than as a data entry (which could collide with an application's own key).
type journaledMeta struct {
	Labels          map[string]string       `json:"labels,omitempty"`
	Annotations     map[string]string       `json:"annotations,omitempty"`
	OwnerReferences []metav1.OwnerReference `json:"ownerReferences,omitempty"`
	Immutable       *bool                   `json:"immutable,omitempty"`
}

// journalMAC authenticates a backup as this migration's own. The structural
// fields of an entry — its name, session label, and backup-of annotation — are
// all derivable from public identifiers and writable by anyone who can create a
// Secret in the namespace, so they cannot show who wrote it. This MAC is keyed
// on the migration session secret, which a namespace principal does not hold,
// and covers the whole restorable record so no part of it can be swapped.
//
// Every field is written length-prefixed. A separator byte would be ambiguous
// because Secret values are arbitrary bytes: {"a":"x","b":"y"} and
// {"a":"x\x00b\x00y"} would otherwise hash identically, letting a namespace
// writer move a genuine entry between colliding shapes and keep its MAC.
func journalMAC(sessionSecret, sessionID, namespace, name, secretType, meta string, data map[string][]byte) string {
	mac := hmac.New(sha256.New, []byte(sessionSecret))
	write := func(b []byte) {
		var l [8]byte
		binary.BigEndian.PutUint64(l[:], uint64(len(b)))
		_, _ = mac.Write(l[:])
		_, _ = mac.Write(b)
	}
	for _, part := range []string{sessionID, namespace, name, secretType, meta} {
		write([]byte(part))
	}
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// The key count is bound in too, so a shorter map cannot be padded to look
	// like a longer one.
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(keys)))
	_, _ = mac.Write(n[:])
	for _, k := range keys {
		write([]byte(k))
		write(data[k])
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// sessionSecret returns the migration secret for a session, or "" when the
// session is unknown. Journaling and restoring both fail closed without it.
func (h *Handler) sessionSecret(sessionID string) string {
	session, ok := h.Sessions.Get(sessionID)
	if !ok {
		return ""
	}
	return session.Secret
}

// backupSecretName is deterministic per (session, secret) and always a legal
// object name, so a retried transfer addresses the same backup instead of
// minting a second one, and a long original name cannot overflow the limit.
func backupSecretName(sessionID, name string) string {
	sum := sha256.Sum256([]byte(sessionID + "\x00" + name))
	return secretBackupPrefix + hex.EncodeToString(sum[:8])
}

// verifyJournalEntry confirms an existing backup really is this migration's
// entry for this Secret, by its authenticator rather than by fields any
// namespace writer could set. A squatted or tampered entry means the original
// is not recoverable, so it fails closed rather than letting the overwrite
// proceed on the strength of a well-formed decoy.
func (h *Handler) verifyJournalEntry(ctx context.Context, sessionID string, existing *corev1.Secret) error {
	secret := h.sessionSecret(sessionID)
	if secret == "" {
		return fmt.Errorf("no migration session secret available to authenticate the rollback copy of %s/%s", existing.Namespace, existing.Name)
	}
	entry, err := h.Client.CoreV1().Secrets(existing.Namespace).Get(ctx, backupSecretName(sessionID, existing.Name), metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading the existing rollback copy of %s/%s: %w", existing.Namespace, existing.Name, err)
	}
	if err := h.authenticJournalEntry(sessionID, entry); err != nil {
		return fmt.Errorf("the secret occupying the rollback-copy name for %s/%s is not this migration's: %w", existing.Namespace, existing.Name, err)
	}
	if entry.Annotations[secretBackupNameAnn] != existing.Name {
		return fmt.Errorf("the rollback copy at %s/%s records a different original", existing.Namespace, entry.Name)
	}
	return nil
}

// authenticJournalEntry checks an entry's MAC over its own identity and
// payload. Returns the decoded original name on success.
func (h *Handler) authenticJournalEntry(sessionID string, entry *corev1.Secret) error {
	secret := h.sessionSecret(sessionID)
	if secret == "" {
		return fmt.Errorf("no session secret to verify against")
	}
	name := entry.Annotations[secretBackupNameAnn]
	if name == "" || entry.Name != backupSecretName(sessionID, name) {
		return fmt.Errorf("name does not match the recorded original")
	}
	want := journalMAC(secret, sessionID, entry.Namespace, name, entry.Annotations[secretBackupTypeAnn], entry.Annotations[secretBackupMetaAnn], entry.Data)
	got := entry.Annotations[secretBackupMACAnn]
	if got == "" || subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
		return fmt.Errorf("authenticator does not match")
	}
	return nil
}

// journalExistingSecret stores a copy of a Secret about to be overwritten. It
// is create-only: a retried transfer must keep the FIRST copy, since by the
// second attempt the live Secret already holds the source's values and copying
// it again would journal the overwrite as if it were the original.
func (h *Handler) journalExistingSecret(ctx context.Context, sessionID string, existing *corev1.Secret) error {
	sessionSecret := h.sessionSecret(sessionID)
	if sessionSecret == "" {
		return fmt.Errorf("no migration session secret available to authenticate a rollback copy of %s/%s", existing.Namespace, existing.Name)
	}

	meta, err := json.Marshal(journaledMeta{
		Labels:          existing.Labels,
		Annotations:     existing.Annotations,
		OwnerReferences: existing.OwnerReferences,
		Immutable:       existing.Immutable,
	})
	if err != nil {
		return fmt.Errorf("encoding metadata of %s/%s: %w", existing.Namespace, existing.Name, err)
	}

	// The payload is copied rather than aliased: writing into the caller's map
	// would mutate the live object the receive path still holds, and a Secret
	// with no data at all would otherwise panic on the first write.
	data := make(map[string][]byte, len(existing.Data))
	for k, v := range existing.Data {
		data[k] = v
	}

	// The inventory is recorded and made durable BEFORE the copy exists. The
	// two are separate objects, so one of them has to be written first, and
	// this order is the safe one: an inventory entry with no copy is reported
	// as a missing copy, whereas a copy with no inventory entry is invisible to
	// abort and commit and would sit in the namespace holding plaintext
	// forever. Persistence failure stops the overwrite outright.
	session, ok := h.Sessions.Get(sessionID)
	if !ok {
		return fmt.Errorf("unknown migration session — refusing to overwrite %s/%s without a recoverable record", existing.Namespace, existing.Name)
	}
	session.RecordJournaledSecret(existing.Namespace, existing.Name)
	if err := h.Sessions.SaveDurable(session); err != nil {
		return fmt.Errorf("recording that %s/%s is being overwritten: %w", existing.Namespace, existing.Name, err)
	}

	backup := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      backupSecretName(sessionID, existing.Name),
			Namespace: existing.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kipper",
				secretBackupSessionLabel:       sessionID,
			},
			Annotations: map[string]string{
				secretBackupNameAnn: existing.Name,
				secretBackupTypeAnn: string(existing.Type),
				// Metadata rides as one annotation value, so neither an
				// application data key nor an illegal annotation key can be
				// produced from the original's own keys.
				secretBackupMetaAnn: string(meta),
				secretBackupMACAnn:  journalMAC(sessionSecret, sessionID, existing.Namespace, existing.Name, string(existing.Type), string(meta), data),
			},
		},
		// The backup always stores the payload opaquely: a copy of, say, a
		// kubernetes.io/tls Secret must not have to satisfy that type's required
		// keys while it is only being held for a possible restore.
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}

	_, err = h.Client.CoreV1().Secrets(existing.Namespace).Create(ctx, backup, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		// A rollback copy is already here — from an earlier attempt, possibly
		// one whose inventory entry never reached disk. It must be this
		// migration's genuine entry: anything else means the original is not
		// recoverable, and proceeding would overwrite it with no way back. The
		// inventory write above has already repaired the record either way.
		return h.verifyJournalEntry(ctx, sessionID, existing)
	}
	if err != nil {
		return fmt.Errorf("journaling secret %s/%s before overwrite: %w", existing.Namespace, existing.Name, err)
	}
	return nil
}

// restoreJournaledSecrets puts every Secret this session overwrote back the way
// it was. Used on abort: a migration that did not finish must not leave the
// target's own apps running on the source's credentials. Returns how many were
// restored and the failures, so an abort reports what it could not undo rather
// than claiming a clean rollback.
func (h *Handler) restoreJournaledSecrets(ctx context.Context, sessionID string) (int, []string) {
	session, ok := h.Sessions.Get(sessionID)
	if !ok {
		return 0, []string{"unknown migration session — cannot identify which secrets to roll back"}
	}

	restored := 0
	var failures []string
	for ns, names := range session.JournaledSecretsSnapshot() {
		if !h.namespaceInScope(ctx, sessionID, ns) {
			failures = append(failures, fmt.Sprintf("%s: outside this migration's namespaces, rollback skipped", ns))
			continue
		}
		for _, name := range names {
			backup, err := h.Client.CoreV1().Secrets(ns).Get(ctx, backupSecretName(sessionID, name), metav1.GetOptions{})
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s/%s: rollback copy is missing: %v", ns, name, err))
				continue
			}
			// Restoring writes with the console-api's credentials, so an entry
			// is only used once its authenticator proves this migration wrote
			// it and that nothing in it has been altered since.
			if err := h.authenticJournalEntry(sessionID, backup); err != nil {
				failures = append(failures, fmt.Sprintf("%s/%s: rollback copy is not usable: %v", ns, name, err))
				continue
			}

			var meta journaledMeta
			if raw := backup.Annotations[secretBackupMetaAnn]; raw != "" {
				if err := json.Unmarshal([]byte(raw), &meta); err != nil {
					failures = append(failures, fmt.Sprintf("%s/%s: rollback copy is unreadable: %v", ns, name, err))
					continue
				}
			}
			data := backup.Data

			originalType := corev1.SecretType(backup.Annotations[secretBackupTypeAnn])
			if originalType == "" {
				originalType = corev1.SecretTypeOpaque
			}
			original := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:            name,
					Namespace:       ns,
					Labels:          meta.Labels,
					Annotations:     meta.Annotations,
					OwnerReferences: meta.OwnerReferences,
				},
				Type:      originalType,
				Data:      data,
				Immutable: meta.Immutable,
			}

			if err := h.putBackSecret(ctx, original); err != nil {
				failures = append(failures, fmt.Sprintf("%s/%s: %v", ns, name, err))
				continue
			}
			restored++

			// The copy goes only once the original is safely back, and only if
			// it is confirmed gone: a finalizer can leave a deleted Secret in
			// place, still holding plaintext.
			if err := h.deleteConfirmed(ctx, ns, backup.Name); err != nil {
				failures = append(failures, fmt.Sprintf("%s/%s: original restored, but its rollback copy is still present: %v", ns, name, err))
			}
		}
	}
	return restored, failures
}

// deleteConfirmed removes a Secret and verifies it actually went. A namespace
// principal can attach a finalizer, in which case the API accepts the delete
// and the object lingers in Terminating with its plaintext intact, so a nil
// error from Delete is not on its own proof of removal.
func (h *Handler) deleteConfirmed(ctx context.Context, ns, name string) error {
	secrets := h.Client.CoreV1().Secrets(ns)
	if err := secrets.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		_, err := secrets.Get(ctx, name, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("still present after deletion when the request ended (last read: %v): %w", lastErr, ctx.Err())
			}
			return fmt.Errorf("still present after deletion when the request ended: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	if lastErr != nil {
		return fmt.Errorf("still present after deletion (last read: %v)", lastErr)
	}
	return fmt.Errorf("still present after deletion (a finalizer may be holding it)")
}

// putBackSecret reinstates one journaled Secret. An in-place update is
// preferred: deleting first would leave the Secret absent if the recreate then
// failed, which is worse than the overwrite being undone. Only a type change
// forces delete-and-recreate, and that path restores the overwritten object if
// the recreate fails, so a rollback cannot destroy what it was undoing.
func (h *Handler) putBackSecret(ctx context.Context, original *corev1.Secret) error {
	secrets := h.Client.CoreV1().Secrets(original.Namespace)

	current, err := secrets.Get(ctx, original.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, createErr := secrets.Create(ctx, original, metav1.CreateOptions{})
		return createErr
	}
	if err != nil {
		return err
	}

	// A nil Immutable and an explicit false both mean mutable, so only a truly
	// immutable object needs the delete-and-recreate path.
	if current.Type == original.Type && (current.Immutable == nil || !*current.Immutable) {
		updated := current.DeepCopy()
		updated.Data = original.Data
		updated.Labels = original.Labels
		updated.Annotations = original.Annotations
		updated.OwnerReferences = original.OwnerReferences
		updated.Immutable = original.Immutable
		_, updateErr := secrets.Update(ctx, updated, metav1.UpdateOptions{})
		return updateErr
	}

	// Type (and an immutable payload) cannot be updated in place.
	rollback := current.DeepCopy()
	rollback.ResourceVersion = ""
	rollback.UID = ""
	rollback.CreationTimestamp = metav1.Time{}
	if delErr := secrets.Delete(ctx, original.Name, metav1.DeleteOptions{}); delErr != nil && !errors.IsNotFound(delErr) {
		return delErr
	}
	var createErr error
	for attempt := 0; attempt < 10; attempt++ {
		if _, createErr = secrets.Create(ctx, original, metav1.CreateOptions{}); !errors.IsAlreadyExists(createErr) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if createErr != nil {
		// Put back what was there rather than leave the namespace with no
		// Secret at all; the caller still reports this as a failed restore.
		if _, rbErr := secrets.Create(ctx, rollback, metav1.CreateOptions{}); rbErr != nil && !errors.IsAlreadyExists(rbErr) {
			return fmt.Errorf("restoring failed (%v) and the overwritten secret could not be put back (%v)", createErr, rbErr)
		}
		return createErr
	}
	return nil
}

// CommitHandler is the target's signal that the migration landed: the operator
// cut over, so the overwritten Secrets are now the intended state and the
// journal can go. Without it the backups would sit in the namespace forever
// holding a plaintext copy of credentials the operator has already replaced.
// POST /api/v1/migrate-target/{session}/commit
func (h *Handler) CommitHandler(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "session")
	session, ok := h.Sessions.Get(sessionID)
	if !ok {
		respondError(w, http.StatusNotFound, "session not found")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	failures := h.dropJournaledSecrets(ctx, sessionID)

	session.Finish(SessionCompleted, "")
	h.Sessions.Save(session)

	resp := map[string]interface{}{"status": "committed"}
	// The copies hold a second plaintext of the target's former credentials, so
	// a commit that could not remove them must say so rather than report a
	// clean finish.
	if len(failures) > 0 {
		resp["rollback_copies_remaining"] = failures
	}
	respondJSON(w, http.StatusOK, resp)
}

// dropJournaledSecrets removes this session's backups once the migration has
// committed and the overwrite is the intended state. Anything it could not
// remove is returned so the caller can name it: a committed session is never
// aborted, so nothing else comes back for these.
func (h *Handler) dropJournaledSecrets(ctx context.Context, sessionID string) []string {
	session, ok := h.Sessions.Get(sessionID)
	if !ok {
		return []string{"unknown migration session — cannot identify which rollback copies to clear"}
	}
	var failures []string
	for ns, names := range session.JournaledSecretsSnapshot() {
		if !h.namespaceInScope(ctx, sessionID, ns) {
			continue
		}
		for _, name := range names {
			backupName := backupSecretName(sessionID, name)
			entry, err := h.Client.CoreV1().Secrets(ns).Get(ctx, backupName, metav1.GetOptions{})
			if errors.IsNotFound(err) {
				continue
			}
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s/%s: %v", ns, backupName, err))
				continue
			}
			if authErr := h.authenticJournalEntry(sessionID, entry); authErr != nil {
				// Not provably ours, so it is left alone — but it is still here
				// holding plaintext, so it is reported rather than passed over.
				failures = append(failures, fmt.Sprintf("%s/%s: left in place, it is not this migration's rollback copy: %v", ns, backupName, authErr))
				continue
			}
			if delErr := h.deleteConfirmed(ctx, ns, backupName); delErr != nil {
				failures = append(failures, fmt.Sprintf("%s/%s: %v", ns, backupName, delErr))
			}
		}
	}
	return failures
}
