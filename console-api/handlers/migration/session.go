package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// StepStatus represents the state of a migration step.
type StepStatus string

const (
	StepPending   StepStatus = "pending"
	StepRunning   StepStatus = "running"
	StepCompleted StepStatus = "completed"
	StepFailed    StepStatus = "failed"
	StepSkipped   StepStatus = "skipped"
)

// Step tracks progress of a single migration operation.
type Step struct {
	Name        string     `json:"name"`
	Phase       string     `json:"phase"`
	Status      StepStatus `json:"status"`
	BytesTotal  int64      `json:"bytes_total,omitempty"`
	BytesDone   int64      `json:"bytes_done,omitempty"`
	Detail      string     `json:"detail,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	Error       string     `json:"error,omitempty"`
	// ManualSteps contains instructions for the user when automatic transfer
	// is not possible (e.g. database too large for in-memory transfer).
	ManualSteps []string `json:"manual_steps,omitempty"`
}

// SessionStatus represents the overall migration state.
type SessionStatus string

const (
	SessionRunning   SessionStatus = "running"
	SessionCompleted SessionStatus = "completed"
	SessionFailed    SessionStatus = "failed"
	SessionCancelled SessionStatus = "cancelled"
	// Waiting for user to confirm domain cutover
	SessionVerifying SessionStatus = "verifying"
)

// Session tracks the full state of a migration operation.
type Session struct {
	ID            string        `json:"id"`
	SourceCluster string        `json:"source_cluster"`
	TargetCluster string        `json:"target_cluster"`
	TargetAPI     string        `json:"target_api"`
	Projects      []string      `json:"projects"`
	Steps         []Step        `json:"steps"`
	Status        SessionStatus `json:"status"`
	StartedAt     time.Time     `json:"started_at"`
	CompletedAt   *time.Time    `json:"completed_at,omitempty"`
	Error         string        `json:"error,omitempty"`

	// StartedBy is the email of the admin whose JWT initiated the migration.
	// Empty on target-side sessions, where the accept is authenticated by the
	// migration token rather than a user.
	StartedBy string `json:"started_by,omitempty"`

	// TargetBaseDomain is the target cluster's CLUSTER_DOMAIN, confirmed by the
	// target at accept. It drives the coexist target URLs and the env/secret
	// rewrite, so it must survive a restart.
	TargetBaseDomain string `json:"-"`

	// KeepDomains marks, by "namespace/name" key, the custom-domain apps the
	// operator chose to leave on the source instead of moving. Consent-bound and
	// persisted so a restart cannot silently move a domain they kept.
	KeepDomains map[string]bool `json:"-"`

	// MoveBaseDomain is the Mode B flag: adopt the source base domain on the
	// target as a whole-cluster evacuation. Consent-bound and persisted.
	MoveBaseDomain bool `json:"-"`

	// Route configs for movers (custom-domain apps being moved), stored during
	// Phase 3 for domain cutover in Phase 5. Coexist and gateway apps are not
	// saved here, so cutover and the DNS screen act on movers only.
	SavedRoutes map[string]map[string]interface{} `json:"-"`

	// JournaledSecrets records, per namespace, the pre-existing Secrets this
	// session overwrote and holds a rollback copy of. The copies themselves
	// live in the project namespace, where a workload principal could strip
	// their labels; this inventory lives with the session, so abort and commit
	// still find every entry they are responsible for.
	JournaledSecrets map[string][]string `json:"-"`

	// ReceivedSecrets records, per namespace, the Secrets this session
	// created on the target. An aborted run removes the unadopted ones so a
	// failed migration does not strand plaintext credentials. Secrets that
	// already existed before the transfer are never recorded: aborting must
	// not delete anything the migration did not bring into existence.
	ReceivedSecrets map[string][]string `json:"-"`

	// Target-side authentication. On the target cluster, AcceptHandler records
	// the migration secret it validated so the subsequent per-session receive
	// endpoints can authenticate each write without re-reading the (consumed,
	// single-use) k8s token. Never serialised.
	Secret    string    `json:"-"`
	ExpiresAt time.Time `json:"-"`

	// IdempotencyKey identifies the accept that created this target-side
	// session, so a retried accept whose response was lost finds it instead
	// of failing on the already-consumed token.
	IdempotencyKey string `json:"-"`

	mu        sync.Mutex
	listeners []chan Step
	// cancel aborts the running transfer's context. Set by runMigration so a
	// cancel takes effect mid-stream rather than at the next project
	// boundary.
	cancel context.CancelFunc
}

// SetCancel registers the running transfer's cancel function.
func (s *Session) SetCancel(fn context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancel = fn
}

// Cancel aborts the running transfer, if one is attached.
func (s *Session) Cancel() {
	s.mu.Lock()
	fn := s.cancel
	s.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// AddStep appends a step and notifies listeners.
func (s *Session) AddStep(step Step) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Steps = append(s.Steps, step)
	s.notifyListeners(step)
}

// CurrentStatus returns the session status under the lock. The migration
// goroutine transitions the status while HTTP handlers read it, so every
// cross-goroutine read goes through here.
func (s *Session) CurrentStatus() SessionStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Status
}

// IsCancelled reports whether the session was cancelled.
func (s *Session) IsCancelled() bool {
	return s.CurrentStatus() == SessionCancelled
}

// SetStatus transitions the session status.
func (s *Session) SetStatus(status SessionStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Status = status
}

// Finish moves the session into a terminal status, recording the error and
// completion time.
func (s *Session) Finish(status SessionStatus, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Status = status
	if errMsg != "" {
		s.Error = errMsg
	}
	now := time.Now()
	s.CompletedAt = &now
}

// SaveRoute records an app's original route config for the cutover phase.
func (s *Session) SaveRoute(key string, route map[string]interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.SavedRoutes[key] = route
}

// RecordJournaledSecret notes a pre-existing Secret this session overwrote and
// journaled, so cleanup does not depend on a label anyone in the namespace
// could remove.
func (s *Session) RecordJournaledSecret(namespace, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.JournaledSecrets == nil {
		s.JournaledSecrets = make(map[string][]string)
	}
	for _, existing := range s.JournaledSecrets[namespace] {
		if existing == name {
			return
		}
	}
	s.JournaledSecrets[namespace] = append(s.JournaledSecrets[namespace], name)
}

// JournaledSecretsSnapshot returns a copy for iteration outside the lock.
func (s *Session) JournaledSecretsSnapshot() map[string][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]string, len(s.JournaledSecrets))
	for ns, names := range s.JournaledSecrets {
		out[ns] = append([]string(nil), names...)
	}
	return out
}

// RecordSecret notes a Secret this session created on the target.
func (s *Session) RecordSecret(namespace, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ReceivedSecrets == nil {
		s.ReceivedSecrets = make(map[string][]string)
	}
	s.ReceivedSecrets[namespace] = append(s.ReceivedSecrets[namespace], name)
}

// ReceivedSecretsSnapshot returns a copy of the recorded secrets for
// iteration outside the lock.
func (s *Session) ReceivedSecretsSnapshot() map[string][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]string, len(s.ReceivedSecrets))
	for ns, names := range s.ReceivedSecrets {
		out[ns] = append([]string(nil), names...)
	}
	return out
}

// RoutesSnapshot returns a copy of the saved routes for iteration outside
// the lock. The inner route maps are written once on save and only read
// afterwards, so sharing them is safe.
func (s *Session) RoutesSnapshot() map[string]map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	routes := make(map[string]map[string]interface{}, len(s.SavedRoutes))
	for k, v := range s.SavedRoutes {
		routes[k] = v
	}
	return routes
}

// StepsSnapshot returns a copy of the step list for iteration outside the lock.
func (s *Session) StepsSnapshot() []Step {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Step(nil), s.Steps...)
}

// SessionView is the JSON shape API responses expose for a session. The live
// Session cannot be marshalled directly: the migration goroutine mutates it
// concurrently, and encoding/json reads the fields without the lock.
type SessionView struct {
	ID            string        `json:"id"`
	SourceCluster string        `json:"source_cluster"`
	TargetCluster string        `json:"target_cluster"`
	TargetAPI     string        `json:"target_api"`
	Projects      []string      `json:"projects"`
	Steps         []Step        `json:"steps"`
	Status        SessionStatus `json:"status"`
	StartedAt     time.Time     `json:"started_at"`
	CompletedAt   *time.Time    `json:"completed_at,omitempty"`
	Error         string        `json:"error,omitempty"`
	StartedBy     string        `json:"started_by,omitempty"`
}

// View returns a consistent snapshot of the session for JSON responses.
func (s *Session) View() SessionView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SessionView{
		ID:            s.ID,
		SourceCluster: s.SourceCluster,
		TargetCluster: s.TargetCluster,
		TargetAPI:     s.TargetAPI,
		Projects:      s.Projects,
		Steps:         append([]Step(nil), s.Steps...),
		Status:        s.Status,
		StartedAt:     s.StartedAt,
		CompletedAt:   s.CompletedAt,
		Error:         s.Error,
		StartedBy:     s.StartedBy,
	}
}

// UpdateStep updates the last step with the given name and notifies listeners.
func (s *Session) UpdateStep(name string, update func(*Step)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.Steps) - 1; i >= 0; i-- {
		if s.Steps[i].Name == name {
			update(&s.Steps[i])
			s.notifyListeners(s.Steps[i])
			return
		}
	}
}

// Subscribe returns a channel that receives step updates.
func (s *Session) Subscribe() chan Step {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan Step, 64)
	s.listeners = append(s.listeners, ch)
	return ch
}

// Unsubscribe removes a listener channel.
func (s *Session) Unsubscribe(ch chan Step) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, l := range s.listeners {
		if l == ch {
			s.listeners = append(s.listeners[:i], s.listeners[i+1:]...)
			close(ch)
			return
		}
	}
}

func (s *Session) notifyListeners(step Step) {
	for _, ch := range s.listeners {
		select {
		case ch <- step:
		default:
		}
	}
}

// SessionStore holds active migration sessions in memory and, when built
// with NewPersistentSessionStore, mirrors them into Secrets so a console-api
// restart keeps the migration recoverable: the target keeps authenticating
// the session's writes, and a source session in verifying can still cut over.
type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	// persistMu serialises the whole snapshot-then-write of a session's
	// Secret. The snapshot is taken under the session's own lock and the
	// Kubernetes write happens after it is released, so without this an older
	// call could get the Secret after a newer one updated it and replace the
	// payload with its stale copy — silently dropping, for example, a
	// journaled-secret entry that a caller had just been told was durable.
	persistMu sync.Mutex
	client    kubernetes.Interface
	namespace string
}

// NewSessionStore creates an empty, memory-only store.
func NewSessionStore() *SessionStore {
	return &SessionStore{sessions: make(map[string]*Session)}
}

const (
	sessionSecretPrefix = "migration-session-"
	sessionSecretLabel  = "kipper.run/migration-session" //nolint:gosec // G101: a label key, not a credential
	// sessionRetention bounds how long finished sessions survive restarts
	// before the store stops restoring them and deletes their Secrets.
	sessionRetention = 7 * 24 * time.Hour
)

// persistedSession is the Secret payload. Session itself excludes the target
// secret and saved routes from JSON on purpose (they never belong in API
// responses), so persistence uses its own shape.
type persistedSession struct {
	ID               string                            `json:"id"`
	SourceCluster    string                            `json:"source_cluster"`
	TargetCluster    string                            `json:"target_cluster"`
	TargetAPI        string                            `json:"target_api"`
	Projects         []string                          `json:"projects"`
	Status           SessionStatus                     `json:"status"`
	StartedAt        time.Time                         `json:"started_at"`
	CompletedAt      *time.Time                        `json:"completed_at,omitempty"`
	Error            string                            `json:"error,omitempty"`
	Steps            []Step                            `json:"steps,omitempty"`
	SavedRoutes      map[string]map[string]interface{} `json:"saved_routes,omitempty"`
	ReceivedSecrets  map[string][]string               `json:"received_secrets,omitempty"`
	JournaledSecrets map[string][]string               `json:"journaled_secrets,omitempty"`
	Secret           string                            `json:"secret,omitempty"`
	ExpiresAt        time.Time                         `json:"expires_at,omitempty"`
	StartedBy        string                            `json:"started_by,omitempty"`
	IdempotencyKey   string                            `json:"idempotency_key,omitempty"`
	TargetBaseDomain string                            `json:"target_base_domain,omitempty"`
	KeepDomains      map[string]bool                   `json:"keep_domains,omitempty"`
	MoveBaseDomain   bool                              `json:"move_base_domain,omitempty"`
}

// NewPersistentSessionStore creates a store backed by Secrets in the given
// namespace and restores the sessions a previous process left behind. A
// session that was mid-run when the process died cannot resume (its transfer
// loop is gone), so it comes back as failed with a clear reason; sessions in
// verifying stay actionable.
func NewPersistentSessionStore(client kubernetes.Interface, namespace string) *SessionStore {
	ss := &SessionStore{
		sessions:  make(map[string]*Session),
		client:    client,
		namespace: namespace,
	}
	ss.restore()
	return ss
}

func (ss *SessionStore) restore() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	list, err := ss.client.CoreV1().Secrets(ss.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: sessionSecretLabel + "=true",
	})
	if err != nil {
		fmt.Printf("[migration] restoring sessions: %v\n", err)
		return
	}

	for i := range list.Items {
		secret := &list.Items[i]
		var p persistedSession
		if err := json.Unmarshal(secret.Data["session"], &p); err != nil || p.ID == "" {
			continue
		}

		if time.Since(p.StartedAt) > sessionRetention {
			_ = ss.client.CoreV1().Secrets(ss.namespace).Delete(ctx, secret.Name, metav1.DeleteOptions{})
			continue
		}

		session := &Session{
			ID:               p.ID,
			SourceCluster:    p.SourceCluster,
			TargetCluster:    p.TargetCluster,
			TargetAPI:        p.TargetAPI,
			TargetBaseDomain: p.TargetBaseDomain,
			Projects:         p.Projects,
			Status:           p.Status,
			StartedAt:        p.StartedAt,
			CompletedAt:      p.CompletedAt,
			Error:            p.Error,
			Steps:            p.Steps,
			SavedRoutes:      p.SavedRoutes,
			ReceivedSecrets:  p.ReceivedSecrets,
			JournaledSecrets: p.JournaledSecrets,
			Secret:           p.Secret,
			ExpiresAt:        p.ExpiresAt,
			StartedBy:        p.StartedBy,
			IdempotencyKey:   p.IdempotencyKey,
			KeepDomains:      p.KeepDomains,
			MoveBaseDomain:   p.MoveBaseDomain,
		}
		if session.SavedRoutes == nil {
			session.SavedRoutes = make(map[string]map[string]interface{})
		}

		// Only a source session runs the transfer loop, and only that loop
		// dies with the process. Target sessions (no TargetAPI) exist to
		// authenticate the source's writes and must keep doing so after a
		// restart, whatever phase the source is in.
		if session.Status == SessionRunning && session.TargetAPI != "" {
			session.Status = SessionFailed
			session.Error = "console-api restarted while the migration was running; the run cannot resume, restart the migration"
			now := time.Now()
			session.CompletedAt = &now
		}

		ss.sessions[session.ID] = session
		_ = ss.persist(session)
	}
}

// FindByIdempotencyKey returns the session an accept with this key created,
// if any.
func (ss *SessionStore) FindByIdempotencyKey(key string) *Session {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	for _, s := range ss.sessions {
		if s.IdempotencyKey == key {
			return s
		}
	}
	return nil
}

// Get returns a session by ID.
func (ss *SessionStore) Get(id string) (*Session, bool) {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	s, ok := ss.sessions[id]
	return s, ok
}

// Put stores a session and mirrors it to its Secret.
func (ss *SessionStore) Put(s *Session) {
	ss.mu.Lock()
	ss.sessions[s.ID] = s
	ss.mu.Unlock()
	_ = ss.persist(s)
}

// Save re-persists a session after a lifecycle change (failed, verifying,
// completed, cancelled). The in-memory object is shared, so only the Secret
// mirror needs refreshing.
func (ss *SessionStore) Save(s *Session) {
	_ = ss.persist(s)
}

// SaveDurable persists a session and reports whether the write landed. Callers
// that may only proceed once the session's record is durable use this instead
// of Save, which is deliberately best-effort for progress mirroring.
func (ss *SessionStore) SaveDurable(s *Session) error {
	return ss.persist(s)
}

// Delete removes a session and its Secret.
func (ss *SessionStore) Delete(id string) {
	ss.mu.Lock()
	delete(ss.sessions, id)
	ss.mu.Unlock()

	if ss.client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = ss.client.CoreV1().Secrets(ss.namespace).Delete(ctx, sessionSecretPrefix+id, metav1.DeleteOptions{})
}

// shortID abbreviates a session ID for logs without assuming its length: a
// panic inside an error path would turn a recoverable persistence failure into
// a crash.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (ss *SessionStore) persist(s *Session) error {
	if ss.client == nil {
		return nil
	}
	ss.persistMu.Lock()
	defer ss.persistMu.Unlock()

	s.mu.Lock()
	// SavedRoutes is copied under the lock: json.Marshal below runs outside
	// it, and marshalling the live map while the migration goroutine inserts
	// a route is a fatal concurrent map access.
	routes := make(map[string]map[string]interface{}, len(s.SavedRoutes))
	for k, v := range s.SavedRoutes {
		routes[k] = v
	}
	received := make(map[string][]string, len(s.ReceivedSecrets))
	for ns, names := range s.ReceivedSecrets {
		received[ns] = append([]string(nil), names...)
	}
	journaled := make(map[string][]string, len(s.JournaledSecrets))
	for ns, names := range s.JournaledSecrets {
		journaled[ns] = append([]string(nil), names...)
	}
	keep := make(map[string]bool, len(s.KeepDomains))
	for k, v := range s.KeepDomains {
		keep[k] = v
	}
	p := persistedSession{
		ID:               s.ID,
		SourceCluster:    s.SourceCluster,
		TargetCluster:    s.TargetCluster,
		TargetAPI:        s.TargetAPI,
		TargetBaseDomain: s.TargetBaseDomain,
		Projects:         s.Projects,
		Status:           s.Status,
		StartedAt:        s.StartedAt,
		CompletedAt:      s.CompletedAt,
		Error:            s.Error,
		Steps:            append([]Step(nil), s.Steps...),
		SavedRoutes:      routes,
		ReceivedSecrets:  received,
		JournaledSecrets: journaled,
		Secret:           s.Secret,
		ExpiresAt:        s.ExpiresAt,
		StartedBy:        s.StartedBy,
		IdempotencyKey:   s.IdempotencyKey,
		KeepDomains:      keep,
		MoveBaseDomain:   s.MoveBaseDomain,
	}
	s.mu.Unlock()

	payload, err := json.Marshal(p) //nolint:gosec // G117: persisting the migration secret into a Kubernetes Secret is the point of this store
	if err != nil {
		return err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sessionSecretPrefix + s.ID,
			Namespace: ss.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kipper",
				sessionSecretLabel:             "true",
			},
		},
		Data: map[string][]byte{"session": payload},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err = ss.client.CoreV1().Secrets(ss.namespace).Create(ctx, secret, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		existing, getErr := ss.client.CoreV1().Secrets(ss.namespace).Get(ctx, secret.Name, metav1.GetOptions{})
		if getErr != nil {
			fmt.Printf("[migration %s] persisting session: %v\n", shortID(s.ID), getErr)
			return getErr
		}
		existing.Data = secret.Data
		existing.Labels = secret.Labels
		_, err = ss.client.CoreV1().Secrets(ss.namespace).Update(ctx, existing, metav1.UpdateOptions{})
	}
	if err != nil {
		fmt.Printf("[migration %s] persisting session: %v\n", shortID(s.ID), err)
		return err
	}
	return nil
}
