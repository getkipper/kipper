package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const journalTestNS = "shop-prod"

// journalHandler wires a handler whose session accepts journalTestNS, since
// restore and drop only touch namespaces the migration was accepted for.
func journalHandler(t *testing.T, objs ...runtime.Object) *Handler {
	t.Helper()
	all := []runtime.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   journalTestNS,
			Labels: map[string]string{"kipper.run/project": "shop"},
		}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "other-ns"}},
	}
	all = append(all, objs...)
	client := fake.NewSimpleClientset(all...)
	h := &Handler{Client: client, Sessions: NewSessionStore()}
	h.Sessions.Put(&Session{
		ID:        "sess-1",
		Projects:  []string{"shop"},
		Status:    SessionRunning,
		Secret:    "s",
		ExpiresAt: time.Now().Add(time.Hour),
	})
	return h
}

func existingSecret(name string, data map[string][]byte, typ corev1.SecretType) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: journalTestNS,
			// A real-world qualified key: re-expressing this as an annotation
			// key would produce two slashes and the API would reject it.
			Labels:      map[string]string{"app.kubernetes.io/managed-by": "helm", "team": "payments"},
			Annotations: map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "{}"},
		},
		Type: typ,
		Data: data,
	}
}

// An aborted migration must put back the credentials it overwrote, including
// metadata whose keys cannot be used as annotation names.
func TestAbortRestoresOverwrittenSecret(t *testing.T) {
	ctx := context.Background()
	original := existingSecret("api-secrets",
		map[string][]byte{"DATABASE_URL": []byte("postgres://target-own")}, corev1.SecretTypeOpaque)
	h := journalHandler(t, original)
	client := h.Client

	if err := h.journalExistingSecret(ctx, "sess-1", original); err != nil {
		t.Fatalf("journaling: %v", err)
	}
	overwritten := original.DeepCopy()
	overwritten.Data = map[string][]byte{"DATABASE_URL": []byte("postgres://SOURCE")}
	if _, err := client.CoreV1().Secrets(journalTestNS).Update(ctx, overwritten, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("overwriting: %v", err)
	}

	restored, failures := h.restoreJournaledSecrets(ctx, "sess-1")
	if restored != 1 || len(failures) > 0 {
		t.Fatalf("restored=%d failures=%v, want 1 and none", restored, failures)
	}

	got, err := client.CoreV1().Secrets(journalTestNS).Get(ctx, "api-secrets", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading restored secret: %v", err)
	}
	if string(got.Data["DATABASE_URL"]) != "postgres://target-own" {
		t.Errorf("DATABASE_URL = %q, want the target's own value back", got.Data["DATABASE_URL"])
	}
	if got.Labels["app.kubernetes.io/managed-by"] != "helm" || got.Labels["team"] != "payments" {
		t.Errorf("qualified labels not restored: %v", got.Labels)
	}
	if got.Annotations["kubectl.kubernetes.io/last-applied-configuration"] != "{}" {
		t.Errorf("qualified annotations not restored: %v", got.Annotations)
	}
	// The envelope lives in an annotation, so it can never collide with or
	// leak into the application's own data keys.
	if _, leaked := got.Data[secretBackupMetaAnn]; leaked {
		t.Error("the metadata envelope leaked into the restored secret's data")
	}
	if remaining, _ := client.CoreV1().Secrets(journalTestNS).List(ctx, metav1.ListOptions{
		LabelSelector: secretBackupSessionLabel + "=sess-1"}); len(remaining.Items) != 0 {
		t.Errorf("backup left behind: %d", len(remaining.Items))
	}
}

// A controller-owned Secret must come back owned, or aborting would silently
// detach it from its controller.
func TestRestorePreservesOwnerReferences(t *testing.T) {
	ctx := context.Background()
	tru := true
	original := existingSecret("owned-secrets", map[string][]byte{"K": []byte("v")}, corev1.SecretTypeOpaque)
	original.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "kipper.run/v1alpha1", Kind: "App", Name: "storefront",
		UID: "abc", Controller: &tru,
	}}
	h := journalHandler(t, original)

	if err := h.journalExistingSecret(ctx, "sess-1", original); err != nil {
		t.Fatalf("journaling: %v", err)
	}
	if restored, failures := h.restoreJournaledSecrets(ctx, "sess-1"); restored != 1 || len(failures) > 0 {
		t.Fatalf("restored=%d failures=%v", restored, failures)
	}
	got, err := h.Client.CoreV1().Secrets(journalTestNS).Get(ctx, "owned-secrets", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if len(got.OwnerReferences) != 1 || got.OwnerReferences[0].Name != "storefront" {
		t.Errorf("owner references not restored: %v", got.OwnerReferences)
	}
}

// A retried transfer re-sends the same secret. The journal must keep the FIRST
// copy, or the rollback would restore the source's values as if they were the
// target's own.
func TestJournalKeepsFirstCopyOnRetry(t *testing.T) {
	ctx := context.Background()
	original := existingSecret("api-secrets",
		map[string][]byte{"K": []byte("target-own")}, corev1.SecretTypeOpaque)
	h := journalHandler(t, original)

	if err := h.journalExistingSecret(ctx, "sess-1", original); err != nil {
		t.Fatalf("first journal: %v", err)
	}
	overwritten := original.DeepCopy()
	overwritten.Data = map[string][]byte{"K": []byte("SOURCE")}
	if err := h.journalExistingSecret(ctx, "sess-1", overwritten); err != nil {
		t.Fatalf("second journal: %v", err)
	}

	backup, err := h.Client.CoreV1().Secrets(journalTestNS).Get(ctx, backupSecretName("sess-1", "api-secrets"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading backup: %v", err)
	}
	if string(backup.Data["K"]) != "target-own" {
		t.Errorf("backup holds %q, want the original target-own value", backup.Data["K"])
	}
}

// A Secret squatting the deterministic backup name must not be taken as proof
// the original is recoverable: the overwrite has to fail closed instead.
func TestJournalRefusesForeignEntryAtItsName(t *testing.T) {
	ctx := context.Background()
	original := existingSecret("api-secrets", map[string][]byte{"K": []byte("v")}, corev1.SecretTypeOpaque)
	squatter := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      backupSecretName("sess-1", "api-secrets"),
		Namespace: journalTestNS,
	}}
	h := journalHandler(t, original, squatter)

	if err := h.journalExistingSecret(ctx, "sess-1", original); err == nil {
		t.Fatal("journaling must fail when the backup name holds something that is not this migration's copy")
	}
}

// A type change is a delete-and-recreate, so the restore has to put the
// original type back too.
func TestRestoreReinstatesOriginalType(t *testing.T) {
	ctx := context.Background()
	original := existingSecret("tls-secret",
		map[string][]byte{"tls.crt": []byte("cert"), "tls.key": []byte("key")}, corev1.SecretTypeTLS)
	h := journalHandler(t, original)

	if err := h.journalExistingSecret(ctx, "sess-1", original); err != nil {
		t.Fatalf("journaling: %v", err)
	}
	_ = h.Client.CoreV1().Secrets(journalTestNS).Delete(ctx, "tls-secret", metav1.DeleteOptions{})
	replacement := existingSecret("tls-secret", map[string][]byte{"x": []byte("y")}, corev1.SecretTypeOpaque)
	if _, err := h.Client.CoreV1().Secrets(journalTestNS).Create(ctx, replacement, metav1.CreateOptions{}); err != nil {
		t.Fatalf("replacing: %v", err)
	}

	if restored, failures := h.restoreJournaledSecrets(ctx, "sess-1"); restored != 1 || len(failures) > 0 {
		t.Fatalf("restored=%d failures=%v", restored, failures)
	}
	got, err := h.Client.CoreV1().Secrets(journalTestNS).Get(ctx, "tls-secret", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading restored: %v", err)
	}
	if got.Type != corev1.SecretTypeTLS {
		t.Errorf("type = %q, want %q", got.Type, corev1.SecretTypeTLS)
	}
	if string(got.Data["tls.crt"]) != "cert" {
		t.Errorf("tls.crt not restored: %q", got.Data["tls.crt"])
	}
}

// Committing drops the journal, since the copies hold plaintext credentials.
func TestCommitDropsJournal(t *testing.T) {
	ctx := context.Background()
	original := existingSecret("api-secrets", map[string][]byte{"K": []byte("v")}, corev1.SecretTypeOpaque)
	h := journalHandler(t, original)

	if err := h.journalExistingSecret(ctx, "sess-1", original); err != nil {
		t.Fatalf("journaling: %v", err)
	}
	if failures := h.dropJournaledSecrets(ctx, "sess-1"); len(failures) > 0 {
		t.Fatalf("drop reported failures: %v", failures)
	}

	remaining, _ := h.Client.CoreV1().Secrets(journalTestNS).List(ctx, metav1.ListOptions{
		LabelSelector: secretBackupSessionLabel + "=sess-1"})
	if len(remaining.Items) != 0 {
		t.Errorf("journal not dropped: %d backups remain", len(remaining.Items))
	}
	if _, err := h.Client.CoreV1().Secrets(journalTestNS).Get(ctx, "api-secrets", metav1.GetOptions{}); err != nil {
		t.Errorf("commit removed the live secret: %v", err)
	}
}

// A rollback copy planted outside the session's accepted namespaces must not
// drive a privileged overwrite of an arbitrary Secret.
func TestRestoreIgnoresOutOfScopeBackups(t *testing.T) {
	ctx := context.Background()
	victim := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "victim", Namespace: "other-ns"},
		Data:       map[string][]byte{"K": []byte("untouched")},
	}
	forged := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        backupSecretName("sess-1", "victim"),
			Namespace:   "other-ns",
			Labels:      map[string]string{secretBackupSessionLabel: "sess-1"},
			Annotations: map[string]string{secretBackupNameAnn: "victim"},
		},
		Data: map[string][]byte{"K": []byte("ATTACKER")},
	}
	h := journalHandler(t, victim, forged)

	// The inventory of what this migration overwrote lives with the session, so
	// a copy planted in the cluster is never even consulted.
	restored, _ := h.restoreJournaledSecrets(ctx, "sess-1")
	if restored != 0 {
		t.Errorf("restored %d planted backups, want 0", restored)
	}
	got, _ := h.Client.CoreV1().Secrets("other-ns").Get(ctx, "victim", metav1.GetOptions{})
	if string(got.Data["K"]) != "untouched" {
		t.Errorf("out-of-scope secret was overwritten: %q", got.Data["K"])
	}
}

// The structural fields of a rollback copy are all derivable from public
// identifiers, so a principal who can create Secrets in an accepted namespace
// could forge a well-formed one. Restoring must refuse it: otherwise abort
// would write attacker-chosen bytes into a live Secret using the console-api's
// credentials.
func TestRestoreRefusesForgedEntryInAcceptedNamespace(t *testing.T) {
	ctx := context.Background()
	victim := existingSecret("api-secrets", map[string][]byte{"K": []byte("real")}, corev1.SecretTypeOpaque)
	forged := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      backupSecretName("sess-1", "api-secrets"),
			Namespace: journalTestNS,
			Labels:    map[string]string{secretBackupSessionLabel: "sess-1"},
			Annotations: map[string]string{
				secretBackupNameAnn: "api-secrets",
				secretBackupTypeAnn: string(corev1.SecretTypeOpaque),
				secretBackupMetaAnn: "{}",
				// Every field above is guessable; the MAC is not.
				secretBackupMACAnn: "0000000000000000000000000000000000000000000000000000000000000000",
			},
		},
		Data: map[string][]byte{"K": []byte("ATTACKER")},
	}
	h := journalHandler(t, victim, forged)

	// The forged copy is not in the session's inventory, so restore never
	// reaches it; and were it reached, its authenticator would not verify.
	restored, _ := h.restoreJournaledSecrets(ctx, "sess-1")
	if restored != 0 {
		t.Errorf("restored %d forged copies, want 0", restored)
	}
	got, _ := h.Client.CoreV1().Secrets(journalTestNS).Get(ctx, "api-secrets", metav1.GetOptions{})
	if string(got.Data["K"]) != "real" {
		t.Errorf("live secret was overwritten from a forged copy: %q", got.Data["K"])
	}
}

// A Secret with no data at all must journal cleanly rather than panic.
func TestJournalHandlesSecretWithNoData(t *testing.T) {
	ctx := context.Background()
	empty := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: journalTestNS}}
	h := journalHandler(t, empty)

	if err := h.journalExistingSecret(ctx, "sess-1", empty); err != nil {
		t.Fatalf("journaling a secret with nil data: %v", err)
	}
	// Journaling must not have written into the caller's object.
	if empty.Data != nil {
		t.Error("journaling mutated the live secret's data map")
	}
}

// An application key that looks like Kipper's own must survive the round trip.
func TestJournalPreservesReservedLookingDataKey(t *testing.T) {
	ctx := context.Background()
	original := existingSecret("api-secrets", map[string][]byte{
		"kipper.run/migration-backup-metadata": []byte("application-value"),
	}, corev1.SecretTypeOpaque)
	h := journalHandler(t, original)

	if err := h.journalExistingSecret(ctx, "sess-1", original); err != nil {
		t.Fatalf("journaling: %v", err)
	}
	if restored, failures := h.restoreJournaledSecrets(ctx, "sess-1"); restored != 1 || len(failures) > 0 {
		t.Fatalf("restored=%d failures=%v", restored, failures)
	}
	got, _ := h.Client.CoreV1().Secrets(journalTestNS).Get(ctx, "api-secrets", metav1.GetOptions{})
	if string(got.Data["kipper.run/migration-backup-metadata"]) != "application-value" {
		t.Errorf("an application key colliding with Kipper's naming was lost: %q",
			got.Data["kipper.run/migration-backup-metadata"])
	}
}

// The MAC must bind the metadata envelope too. Otherwise a namespace writer
// could leave a genuine entry's payload alone and rewrite its labels,
// annotations, or owner references, and abort would apply them with the
// console-api's credentials.
func TestRestoreRefusesTamperedMetadata(t *testing.T) {
	ctx := context.Background()
	original := existingSecret("api-secrets", map[string][]byte{"K": []byte("v")}, corev1.SecretTypeOpaque)
	h := journalHandler(t, original)
	if err := h.journalExistingSecret(ctx, "sess-1", original); err != nil {
		t.Fatalf("journaling: %v", err)
	}

	entry, err := h.Client.CoreV1().Secrets(journalTestNS).Get(ctx, backupSecretName("sess-1", "api-secrets"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading entry: %v", err)
	}
	entry.Annotations[secretBackupMetaAnn] = `{"labels":{"injected":"yes"}}`
	if _, err := h.Client.CoreV1().Secrets(journalTestNS).Update(ctx, entry, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("tampering: %v", err)
	}

	restored, failures := h.restoreJournaledSecrets(ctx, "sess-1")
	if restored != 0 || len(failures) == 0 {
		t.Fatalf("restored=%d failures=%v, want the tampered entry refused and reported", restored, failures)
	}
	got, _ := h.Client.CoreV1().Secrets(journalTestNS).Get(ctx, "api-secrets", metav1.GetOptions{})
	if got.Labels["injected"] == "yes" {
		t.Error("attacker-chosen metadata was applied to the live secret")
	}
}

// Secret values are arbitrary bytes, so a separator-based MAC would let these
// two payloads hash identically. Length prefixing must keep them distinct.
func TestJournalMACDistinguishesAmbiguousPayloads(t *testing.T) {
	a := map[string][]byte{"a": []byte("x"), "b": []byte("y")}
	b := map[string][]byte{"a": []byte("x\x00b\x00y")}

	macA := journalMAC("secret", "sess-1", "ns", "name", "Opaque", "{}", a)
	macB := journalMAC("secret", "sess-1", "ns", "name", "Opaque", "{}", b)
	if macA == macB {
		t.Error("two different payloads produced the same authenticator — the encoding is ambiguous")
	}

	// And an added key must change it, so a map cannot be padded to look like
	// another.
	c := map[string][]byte{"a": []byte("x"), "b": []byte("y"), "c": []byte("")}
	if journalMAC("secret", "sess-1", "ns", "name", "Opaque", "{}", c) == macA {
		t.Error("adding an empty-valued key did not change the authenticator")
	}
}

// Committing must name a plaintext copy it refused to delete, rather than
// reporting a clean finish while it sits in the namespace.
func TestCommitReportsUndeletableCopy(t *testing.T) {
	ctx := context.Background()
	original := existingSecret("api-secrets", map[string][]byte{"K": []byte("v")}, corev1.SecretTypeOpaque)
	h := journalHandler(t, original)
	if err := h.journalExistingSecret(ctx, "sess-1", original); err != nil {
		t.Fatalf("journaling: %v", err)
	}
	entry, _ := h.Client.CoreV1().Secrets(journalTestNS).Get(ctx, backupSecretName("sess-1", "api-secrets"), metav1.GetOptions{})
	entry.Annotations[secretBackupMACAnn] = "tampered"
	if _, err := h.Client.CoreV1().Secrets(journalTestNS).Update(ctx, entry, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("tampering: %v", err)
	}

	failures := h.dropJournaledSecrets(ctx, "sess-1")
	if len(failures) == 0 {
		t.Error("a rollback copy that could not be removed must be reported, not silently left behind")
	}
}

// Cleanup must not depend on a label a workload principal can strip: the
// inventory of what was overwritten lives with the session, so removing the
// label from a genuine copy changes nothing about abort or commit.
func TestCleanupSurvivesStrippedLabelOnGenuineCopy(t *testing.T) {
	ctx := context.Background()
	original := existingSecret("api-secrets", map[string][]byte{"K": []byte("target-own")}, corev1.SecretTypeOpaque)
	h := journalHandler(t, original)
	if err := h.journalExistingSecret(ctx, "sess-1", original); err != nil {
		t.Fatalf("journaling: %v", err)
	}

	entry, _ := h.Client.CoreV1().Secrets(journalTestNS).Get(ctx, backupSecretName("sess-1", "api-secrets"), metav1.GetOptions{})
	delete(entry.Labels, secretBackupSessionLabel)
	if _, err := h.Client.CoreV1().Secrets(journalTestNS).Update(ctx, entry, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("stripping label: %v", err)
	}

	// The overwrite happened; abort must still put the original back.
	overwritten := original.DeepCopy()
	overwritten.Data = map[string][]byte{"K": []byte("SOURCE")}
	if _, err := h.Client.CoreV1().Secrets(journalTestNS).Update(ctx, overwritten, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("overwriting: %v", err)
	}

	restored, failures := h.restoreJournaledSecrets(ctx, "sess-1")
	if restored != 1 || len(failures) > 0 {
		t.Fatalf("restored=%d failures=%v, want the label-stripped copy still honoured", restored, failures)
	}
	got, _ := h.Client.CoreV1().Secrets(journalTestNS).Get(ctx, "api-secrets", metav1.GetOptions{})
	if string(got.Data["K"]) != "target-own" {
		t.Errorf("K = %q, want the original restored despite the stripped label", got.Data["K"])
	}
}

// The rollback copy and its inventory entry are separate objects, so one is
// written first. If a crash lands between them, the retry must end with both
// present: a copy nobody has inventoried is invisible to abort and commit and
// would hold plaintext in the namespace indefinitely.
func TestJournalRepairsInventoryAfterCrashBetweenWrites(t *testing.T) {
	ctx := context.Background()
	original := existingSecret("api-secrets", map[string][]byte{"K": []byte("target-own")}, corev1.SecretTypeOpaque)
	h := journalHandler(t, original)

	if err := h.journalExistingSecret(ctx, "sess-1", original); err != nil {
		t.Fatalf("journaling: %v", err)
	}
	// Simulate the inventory entry having been lost with the process, leaving
	// only the copy behind.
	session, _ := h.Sessions.Get("sess-1")
	session.JournaledSecrets = nil

	// The retry sees AlreadyExists and must repair the record.
	if err := h.journalExistingSecret(ctx, "sess-1", original); err != nil {
		t.Fatalf("retry after lost inventory: %v", err)
	}
	if got := session.JournaledSecretsSnapshot()["shop-prod"]; len(got) != 1 || got[0] != "api-secrets" {
		t.Fatalf("inventory = %v, want the copy re-recorded so cleanup can find it", got)
	}

	// And cleanup now finds it.
	if restored, failures := h.restoreJournaledSecrets(ctx, "sess-1"); restored != 1 || len(failures) > 0 {
		t.Errorf("restored=%d failures=%v, want the repaired entry honoured", restored, failures)
	}
}

// If the record of an overwrite cannot be made durable, the overwrite must not
// happen: proceeding would leave a Secret that abort could never put back.
func TestJournalRefusesWhenInventoryCannotPersist(t *testing.T) {
	ctx := context.Background()
	original := existingSecret("api-secrets", map[string][]byte{"K": []byte("v")}, corev1.SecretTypeOpaque)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: journalTestNS, Labels: map[string]string{"kipper.run/project": "shop"},
	}}
	client := fake.NewSimpleClientset(ns, original)

	// A session store that actually writes, so persistence can be made to fail.
	store := NewPersistentSessionStore(client, "kipper-system")
	store.Put(&Session{ID: "sess-1", Projects: []string{"shop"}, Status: SessionRunning, Secret: "s"})
	h := &Handler{Client: client, Sessions: store}

	client.PrependReactor("create", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("etcd unavailable")
	})
	client.PrependReactor("update", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("etcd unavailable")
	})

	if err := h.journalExistingSecret(ctx, "sess-1", original); err == nil {
		t.Fatal("journaling must fail when the overwrite cannot be durably recorded")
	}
	// And it must fail before writing anything: a copy created without a
	// durable inventory entry is exactly the invisible-plaintext case.
	if _, err := client.CoreV1().Secrets(journalTestNS).Get(ctx, backupSecretName("sess-1", "api-secrets"), metav1.GetOptions{}); err == nil {
		t.Error("a rollback copy was created even though its inventory entry never persisted")
	}
}

// A finalizer keeps a deleted Secret in Terminating with its plaintext intact,
// so a successful Delete call is not proof the copy is gone.
func TestDeleteConfirmedReportsFinalizerHeldSecret(t *testing.T) {
	ctx := context.Background()
	held := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "held", Namespace: journalTestNS, Finalizers: []string{"example.com/keep"},
	}}
	h := journalHandler(t, held)
	client := h.Client.(*fake.Clientset)
	// The fake client honours delete, so emulate a finalizer by keeping the
	// object present afterwards.
	client.PrependReactor("delete", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil
	})

	if err := h.deleteConfirmed(ctx, journalTestNS, "held"); err == nil {
		t.Error("a secret still present after deletion must be reported, not counted as removed")
	}
}

// Persisting a session snapshots it and then writes to Kubernetes as two
// steps. If those interleave, an older snapshot can land last and erase an
// entry a caller was just told was durable, leaving a genuine rollback copy
// that abort and commit can never find.
func TestPersistDoesNotLoseInventoryToStaleWrite(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: journalTestNS, Labels: map[string]string{"kipper.run/project": "shop"},
	}}
	client := fake.NewSimpleClientset(ns)
	store := NewPersistentSessionStore(client, "kipper-system")
	session := &Session{ID: "sess-interleave-1", Projects: []string{"shop"}, Status: SessionRunning, Secret: "s"}
	store.Put(session)

	// Hold the first write inside the store so a second can be attempted while
	// it is in flight.
	release := make(chan struct{})
	inWrite := make(chan struct{})
	var once sync.Once
	client.PrependReactor("update", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		// Signal from inside the write, immediately before blocking, so the
		// older call is provably holding the store mid-write. Signalling before
		// Save would let the durable call take the mutex first and never
		// exercise the stale-snapshot ordering this test exists for.
		once.Do(func() {
			close(inWrite)
			<-release
		})
		return false, nil, nil
	})

	go func() { store.Save(session) }() // the older, best-effort write
	<-inWrite

	// The older call has already snapshotted without this entry.
	session.RecordJournaledSecret(journalTestNS, "api-secrets")
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(release)
	}()
	if err := store.SaveDurable(session); err != nil {
		t.Fatalf("durable save: %v", err)
	}

	// Whatever order the two writes completed in, the persisted record must
	// still contain the entry.
	stored, err := client.CoreV1().Secrets("kipper-system").Get(context.Background(), sessionSecretPrefix+session.ID, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading persisted session: %v", err)
	}
	var p persistedSession
	if err := json.Unmarshal(stored.Data["session"], &p); err != nil {
		t.Fatalf("decoding persisted session: %v", err)
	}
	if got := p.JournaledSecrets[journalTestNS]; len(got) != 1 || got[0] != "api-secrets" {
		t.Fatalf("persisted inventory = %v, want the entry retained through the interleaving", got)
	}
}
