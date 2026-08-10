package uisession

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// A session record is the authoritative liveness switch: a session is valid
// only while its record exists. Redeeming an SSO code creates the record
// under the code's sid, and Kubernetes Create atomicity makes that redemption
// single-use across any number of console-api replicas — a replayed code hits
// AlreadyExists. Deleting a subject's records (logout, or user removal)
// revokes their sessions within the record cache TTL, which is the
// revocation SLA. Role staleness is defence-in-depth, not the SLA (a
// Kubernetes API outage freezes both mint and revocation, and existing
// sessions ride the record cache then fail closed).
const (
	recordNamePrefix = "kipper-ui-session-"
	subjectHashLabel = "kipper.run/ui-session-sub"
	emailHashLabel   = "kipper.run/ui-session-email"
	// recordCacheTTL bounds how long a deleted record can still read as live.
	recordCacheTTL = 30 * time.Second
)

// RecordStore persists and reads session records, with a short positive/
// negative cache so the hot forwardAuth path does not Get a Secret per
// request.
type RecordStore struct {
	client    kubernetes.Interface
	namespace string
	now       func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	live    bool
	expires time.Time
}

// NewRecordStore builds a record store in the given namespace.
func NewRecordStore(client kubernetes.Interface, namespace string) *RecordStore {
	return &RecordStore{client: client, namespace: namespace, now: time.Now, cache: map[string]cacheEntry{}}
}

func hashLabel(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])[:32]
}

func recordName(sid string) string { return recordNamePrefix + sid }

// Create records a new session under sid. It returns ErrReplay when a record
// already exists for that sid, which is how SSO-code single-use is enforced.
func (s *RecordStore) Create(ctx context.Context, sid, sub, email, host string, authTime, absoluteExpiry time.Time) error {
	rec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      recordName(sid),
			Namespace: s.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kipper",
				subjectHashLabel:               hashLabel(sub),
				emailHashLabel:                 hashLabel(email),
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"host":      []byte(host),
			"email":     []byte(email),
			"auth_time": []byte(fmt.Sprintf("%d", authTime.Unix())),
			"expires":   []byte(fmt.Sprintf("%d", absoluteExpiry.Unix())),
		},
	}
	if _, err := s.client.CoreV1().Secrets(s.namespace).Create(ctx, rec, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return ErrReplay
		}
		return fmt.Errorf("uisession: creating record: %w", err)
	}
	s.setCache(sid, true)
	return nil
}

// ErrReplay signals a replayed SSO code (a record already exists for its sid).
var ErrReplay = fmt.Errorf("uisession: code already redeemed")

// Live reports whether the session record for sid exists, through a short
// pos/neg cache. It fails closed: any API error other than NotFound is
// treated as "not live" (the caller then rejects the session), and a
// genuine NotFound is cached as not-live so a deleted session dies within the
// cache TTL.
func (s *RecordStore) Live(ctx context.Context, sid string) bool {
	if e, ok := s.getCache(sid); ok {
		return e
	}
	_, err := s.client.CoreV1().Secrets(s.namespace).Get(ctx, recordName(sid), metav1.GetOptions{})
	if err != nil {
		// Cache a NotFound as not-live; leave a transient error uncached so
		// it is re-checked next request, but report not-live now (fail closed).
		if apierrors.IsNotFound(err) {
			s.setCache(sid, false)
		}
		return false
	}
	s.setCache(sid, true)
	return true
}

func (s *RecordStore) setCache(sid string, live bool) {
	s.mu.Lock()
	s.cache[sid] = cacheEntry{live: live, expires: s.now().Add(recordCacheTTL)}
	s.mu.Unlock()
}

func (s *RecordStore) getCache(sid string) (bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.cache[sid]
	if !ok || s.now().After(e.expires) {
		return false, false
	}
	return e.live, true
}

// DeleteBySubject removes every session record for a Dex subject or an email
// (records carry both hashes, so either key works — user removal knows the
// email, logout knows the subject). It also drops the cache so a revocation is
// not masked by a still-live positive cache.
func (s *RecordStore) DeleteBySubject(ctx context.Context, subOrEmail string) error {
	h := hashLabel(subOrEmail)
	for _, sel := range []string{subjectHashLabel + "=" + h, emailHashLabel + "=" + h} {
		if err := s.deleteMatching(ctx, sel); err != nil {
			return err
		}
	}
	s.clearCache()
	return nil
}

// DeleteAll removes every session record — the bulk-revocation companion to
// ResetKeyring.
func (s *RecordStore) DeleteAll(ctx context.Context) error {
	if err := s.deleteMatching(ctx, "app.kubernetes.io/managed-by=kipper,"+subjectHashLabel); err != nil {
		return err
	}
	s.clearCache()
	return nil
}

// deleteMatching lists records by selector and deletes each. List+Delete is
// used over DeleteCollection so the store needs only the list and delete verbs
// that console-api already holds, not the rarer deletecollection verb.
func (s *RecordStore) deleteMatching(ctx context.Context, selector string) error {
	list, err := s.client.CoreV1().Secrets(s.namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("uisession: listing records: %w", err)
	}
	for i := range list.Items {
		name := list.Items[i].Name
		if err := s.client.CoreV1().Secrets(s.namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("uisession: deleting record %s: %w", name, err)
		}
	}
	return nil
}

func (s *RecordStore) clearCache() {
	s.mu.Lock()
	s.cache = map[string]cacheEntry{}
	s.mu.Unlock()
}
