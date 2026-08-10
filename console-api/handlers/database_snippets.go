// Package handlers — database_snippets.go provides per-service saved
// snippets and a per-user query history. State lives in two Secrets
// in the service namespace so the raw SQL is not readable by a project
// viewer (who can read ConfigMaps but not Secrets) and follows the cluster.
package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getkipper/kipper/console-api/middleware"
)

const (
	snippetsSecretPrefix = "kipper-db-snippets-" //nolint:gosec // G101: a Secret object name prefix, not a credential
	historySecretPrefix  = "kipper-db-history-"  //nolint:gosec // G101: a Secret object name prefix, not a credential
	historyMaxEntries    = 100
)

// snippet is a saved query stored in a Secret. Updated and CreatedBy
// are best-effort — when the user identity isn't on the request (e.g.
// CLI without auth) the fields stay empty.
type snippet struct {
	Name      string `json:"name"`
	SQL       string `json:"sql"`
	Pinned    bool   `json:"pinned,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	UpdatedBy string `json:"updated_by,omitempty"`
}

type historyEntry struct {
	SQL        string `json:"sql"`
	DurationMs int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
	Timestamp  string `json:"timestamp"`
	User       string `json:"user,omitempty"`
}

// ListSnippets returns the snippet library for a service.
// GET /api/v1/services/{name}/db/snippets?namespace={ns}
func (d *Database) ListSnippets(w http.ResponseWriter, r *http.Request) {
	svcName, namespace, ok := requireService(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if _, err := d.findServiceCR(ctx, svcName, namespace); err != nil {
		respondError(w, http.StatusNotFound, fmt.Sprintf("service %q not found", svcName))
		return
	}
	cm, err := d.Client.CoreV1().Secrets(namespace).Get(ctx, snippetsSecretPrefix+svcName, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			respondJSON(w, http.StatusOK, []snippet{})
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to read snippets")
		return
	}

	out := make([]snippet, 0, len(cm.Data))
	for _, raw := range cm.Data {
		var s snippet
		if err := json.Unmarshal(raw, &s); err != nil {
			continue
		}
		out = append(out, s)
	}
	// Pinned first, then by name.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pinned != out[j].Pinned {
			return out[i].Pinned
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	respondJSON(w, http.StatusOK, out)
}

// SaveSnippet creates or updates a snippet by name.
// POST /api/v1/services/{name}/db/snippets?namespace={ns}
func (d *Database) SaveSnippet(w http.ResponseWriter, r *http.Request) {
	svcName, namespace, ok := requireService(w, r)
	if !ok {
		return
	}

	var s snippet
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if s.Name == "" || s.SQL == "" {
		respondError(w, http.StatusBadRequest, "name and sql are required")
		return
	}
	s.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if claims := middleware.UserFromContext(r.Context()); claims != nil {
		s.UpdatedBy = claims.Email
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if _, err := d.findServiceCR(ctx, svcName, namespace); err != nil {
		respondError(w, http.StatusNotFound, fmt.Sprintf("service %q not found", svcName))
		return
	}
	if err := d.upsertSecretEntry(ctx, namespace, snippetsSecretPrefix+svcName, snippetKey(s.Name), mustJSON(s)); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("save snippet: %v", err))
		return
	}
	respondJSON(w, http.StatusOK, s)
}

// DeleteSnippet removes a snippet by name.
// DELETE /api/v1/services/{name}/db/snippets/{snippetName}?namespace={ns}
func (d *Database) DeleteSnippet(w http.ResponseWriter, r *http.Request) {
	svcName, namespace, ok := requireService(w, r)
	if !ok {
		return
	}
	name := chi.URLParam(r, "snippetName")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if _, err := d.findServiceCR(ctx, svcName, namespace); err != nil {
		respondError(w, http.StatusNotFound, fmt.Sprintf("service %q not found", svcName))
		return
	}
	cm, err := d.Client.CoreV1().Secrets(namespace).Get(ctx, snippetsSecretPrefix+svcName, metav1.GetOptions{})
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	delete(cm.Data, snippetKey(name))
	if _, err := d.Client.CoreV1().Secrets(namespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		respondError(w, http.StatusInternalServerError, "delete snippet")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListHistory returns the calling user's recent queries against this
// service. Most-recent first.
// GET /api/v1/services/{name}/db/history?namespace={ns}
func (d *Database) ListHistory(w http.ResponseWriter, r *http.Request) {
	svcName, namespace, ok := requireService(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if _, err := d.findServiceCR(ctx, svcName, namespace); err != nil {
		respondError(w, http.StatusNotFound, fmt.Sprintf("service %q not found", svcName))
		return
	}

	userKey := dataKey(userIdentifier(r))
	cm, err := d.Client.CoreV1().Secrets(namespace).Get(ctx, historySecretPrefix+svcName, metav1.GetOptions{})
	if err != nil {
		respondJSON(w, http.StatusOK, []historyEntry{})
		return
	}
	raw, ok := cm.Data[userKey]
	if !ok {
		respondJSON(w, http.StatusOK, []historyEntry{})
		return
	}
	var entries []historyEntry
	_ = json.Unmarshal(raw, &entries)
	// Already most-recent-first by recordHistory; trim defensively.
	if len(entries) > historyMaxEntries {
		entries = entries[:historyMaxEntries]
	}
	respondJSON(w, http.StatusOK, entries)
}

// recordHistory appends a query to the user's history ring. Logged at
// every failure path so a missing-history report is debuggable from
// the console-api logs alone. The user's full identifier (email)
// stays inside the JSON payload as entry.User; the Secret data key
// is the sanitised form so K8s accepts it.
func (d *Database) recordHistory(ctx context.Context, svcName, namespace, user string, entry historyEntry) {
	entry.User = user
	entry.Timestamp = time.Now().UTC().Format(time.RFC3339)
	userKey := dataKey(user)

	cmName := historySecretPrefix + svcName
	ns := namespace
	cm, err := d.Client.CoreV1().Secrets(ns).Get(ctx, cmName, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		newSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: ns,
				Labels: map[string]string{
					kipperLabel:             kipperValue,
					"kipper.run/db-history": "true",
					"app":                   svcName,
				},
			},
			Data: map[string][]byte{userKey: []byte(mustJSON([]historyEntry{entry}))},
		}
		if _, cerr := d.Client.CoreV1().Secrets(ns).Create(ctx, newSecret, metav1.CreateOptions{}); cerr != nil {
			log.Printf("db history: create %s/%s for user=%s failed: %v", ns, cmName, user, cerr)
		}
		return
	}
	if err != nil {
		log.Printf("db history: get %s/%s for user=%s failed: %v", ns, cmName, user, err)
		return
	}

	if cm.Data == nil {
		cm.Data = map[string][]byte{}
	}
	var existing []historyEntry
	if raw, ok := cm.Data[userKey]; ok {
		_ = json.Unmarshal(raw, &existing)
	}
	// Dedupe: if the same SQL is already in the ring, drop the older
	// entry. The new prepend below promotes this run to the top with
	// fresh timestamp / duration / error — so re-running a query just
	// bumps it instead of accumulating duplicates.
	filtered := existing[:0]
	for _, e := range existing {
		if e.SQL != entry.SQL {
			filtered = append(filtered, e)
		}
	}
	// Newest first; cap to historyMaxEntries.
	existing = append([]historyEntry{entry}, filtered...)
	if len(existing) > historyMaxEntries {
		existing = existing[:historyMaxEntries]
	}
	cm.Data[userKey] = []byte(mustJSON(existing))
	if _, uerr := d.Client.CoreV1().Secrets(ns).Update(ctx, cm, metav1.UpdateOptions{}); uerr != nil {
		log.Printf("db history: update %s/%s for user=%s failed: %v", ns, cmName, user, uerr)
	}
}

// auditLog emits a structured stdout line for every query so Loki can
// pick it up. We never log values — only a SHA-256 hash of the SQL,
// the duration, the user, and the error class.
func auditLog(r *http.Request, svcName, sqlText string, durationMs int64, errMsg string) {
	user := userIdentifier(r)
	hash := sha256.Sum256([]byte(sqlText))
	hashHex := hex.EncodeToString(hash[:8])
	status := "ok"
	if errMsg != "" {
		status = "error"
	}
	log.Printf("db_audit service=%s user=%s sql_hash=%s duration_ms=%d status=%s",
		svcName, user, hashHex, durationMs, status)
}

// upsertSecretEntry sets a single key in a Secret, creating the
// Secret if it does not exist yet.
func (d *Database) upsertSecretEntry(ctx context.Context, namespace, name, key, value string) error {
	cm, err := d.Client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		newSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: namespace,
				Labels: map[string]string{kipperLabel: kipperValue},
			},
			Data: map[string][]byte{key: []byte(value)},
		}
		_, cerr := d.Client.CoreV1().Secrets(namespace).Create(ctx, newSecret, metav1.CreateOptions{})
		return cerr
	}
	if err != nil {
		return err
	}
	if cm.Data == nil {
		cm.Data = map[string][]byte{}
	}
	cm.Data[key] = []byte(value)
	_, err = d.Client.CoreV1().Secrets(namespace).Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

// dataKey sanitises any string into a valid Secret data key.
// Kubernetes accepts only [-._a-zA-Z0-9]+ for data keys; emails (@),
// snippet names with spaces, and similar identifiers all need this
// treatment. The original value is preserved inside the JSON payload
// for display purposes.
func dataKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '.' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// snippetKey is kept as a thin alias for callers that read more naturally.
func snippetKey(name string) string { return dataKey(name) }

// userIdentifier returns the calling user's email when JWT auth is
// active, or "anonymous" otherwise. Used as the per-user history key.
func userIdentifier(r *http.Request) string {
	claims := middleware.UserFromContext(r.Context())
	if claims != nil && claims.Email != "" {
		return claims.Email
	}
	return "anonymous"
}

// mustJSON marshals to JSON and panics on failure (only if the input
// has a cyclic graph, which our shapes never do).
func mustJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
