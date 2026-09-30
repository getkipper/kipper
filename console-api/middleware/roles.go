package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/getkipper/kipper/controller/pkg/usergrants"
)

const (
	roleConfigMapName      = "kipper-users"
	roleConfigMapNamespace = "kipper-system"

	// RoleContextKey is the request-context key holding the caller's global
	// role, injected by RoleMiddleware.
	RoleContextKey = contextKey("role")

	RoleAdmin    = "admin"
	RoleDeployer = "deployer"
	RoleViewer   = "viewer"

	// monitoringTrustAge bounds how old the last successful read may be before
	// monitoring access is refused, two refresh intervals.
	monitoringTrustAge = 60 * time.Second
)

// ErrUnknownUser is returned when granting monitoring to an email with no role.
var ErrUnknownUser = usergrants.ErrUnknownUser

// RoleStore caches user-role mappings and monitoring grants from the
// kipper-users ConfigMap.
type RoleStore struct {
	client          kubernetes.Interface
	roles           map[string]string
	monitoring      map[string]bool
	monitoringValid bool
	mu              sync.RWMutex
	lastFetch       time.Time
	cacheTTL        time.Duration
	now             func() time.Time
}

// NewRoleStore creates a RoleStore that reads from the cluster.
func NewRoleStore(client kubernetes.Interface) *RoleStore {
	return &RoleStore{
		client:   client,
		roles:    make(map[string]string),
		cacheTTL: 30 * time.Second,
		now:      time.Now,
	}
}

// GetRole returns the cached or refreshed role, or an empty string for an unknown user.
func (s *RoleStore) GetRole(email string) string {
	s.mu.RLock()
	if s.now().Sub(s.lastFetch) < s.cacheTTL && len(s.roles) > 0 {
		role, ok := s.roles[email]
		s.mu.RUnlock()
		if ok {
			return role
		}
		return ""
	}
	s.mu.RUnlock()

	s.refresh()

	s.mu.RLock()
	defer s.mu.RUnlock()

	if role, ok := s.roles[email]; ok {
		return role
	}

	// Installation seeds the admin role. Unknown users receive no role,
	// including when the store has never loaded.
	return ""
}

func (s *RoleStore) refresh() {
	s.mu.RLock()
	if s.now().Sub(s.lastFetch) < s.cacheTTL {
		s.mu.RUnlock()
		return
	}
	s.mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cm, err := s.client.CoreV1().ConfigMaps(roleConfigMapNamespace).Get(ctx, roleConfigMapName, metav1.GetOptions{})
	if err != nil {
		// Preserve known roles through read failures. MonitoringAccess separately
		// limits how long it trusts this cached data.
		if !apierrors.IsNotFound(err) {
			log.Printf("role store: failed to read %s/%s: %v", roleConfigMapNamespace, roleConfigMapName, err)
		}
		return
	}

	rolesJSON, ok := cm.Data["users"]
	if !ok {
		return
	}

	var roles map[string]string
	if err := json.Unmarshal([]byte(rolesJSON), &roles); err != nil {
		return
	}

	grants, grantsErr := usergrants.Monitoring(cm.Data)
	if grantsErr != nil {
		log.Printf("role store: %v; monitoring access is refused until it is fixed", grantsErr)
	}

	s.mu.Lock()
	s.roles = roles
	s.monitoring = grants
	s.monitoringValid = grantsErr == nil
	s.lastFetch = s.now()
	s.mu.Unlock()
}

// MonitoringAccess reports whether email may use monitoring, and its console
// role when it may. Admins hold monitoring implicitly; anyone else needs a role
// and a grant. Access is refused when the last successful read is older than
// monitoringTrustAge or the grant list is malformed.
func (s *RoleStore) MonitoringAccess(email string) (role string, allowed bool) {
	s.refresh()

	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.lastFetch.IsZero() || s.now().Sub(s.lastFetch) > monitoringTrustAge || !s.monitoringValid {
		return "", false
	}
	role = s.roles[email]
	if role == RoleAdmin || (role != "" && s.monitoring[email]) {
		return role, true
	}
	return "", false
}

// MonitoringHolders lists admins and valid grant holders for display.
// MonitoringAccess additionally checks cache freshness before allowing access.
func (s *RoleStore) MonitoringHolders() map[string]bool {
	s.refresh()

	s.mu.RLock()
	defer s.mu.RUnlock()

	holders := make(map[string]bool)
	for email, role := range s.roles {
		if role == RoleAdmin || (s.monitoringValid && s.monitoring[email]) {
			holders[email] = true
		}
	}
	return holders
}

// SetMonitoring updates a monitoring grant; granting requires a console role.
func (s *RoleStore) SetMonitoring(ctx context.Context, email string, granted bool) error {
	defer s.invalidateCache()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm, err := s.client.CoreV1().ConfigMaps(roleConfigMapNamespace).Get(ctx, roleConfigMapName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		if err := usergrants.SetMonitoring(cm.Data, email, granted); err != nil {
			return err
		}
		_, err = s.client.CoreV1().ConfigMaps(roleConfigMapNamespace).Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
}

func (s *RoleStore) invalidateCache() {
	s.mu.Lock()
	s.lastFetch = time.Time{}
	s.mu.Unlock()
}

// SetRole updates the role and clears any stale monitoring grant for a new user
// in one write. Older kip versions can leave grants behind when removing users.
func (s *RoleStore) SetRole(ctx context.Context, email, role string) error {
	return s.updateUsers(ctx, func(roles, data map[string]string) {
		if _, existed := roles[email]; !existed {
			// A malformed grant list is left for MonitoringAccess to refuse.
			_ = usergrants.SetMonitoring(data, email, false)
		}
		roles[email] = role
	})
}

// RemoveUser removes the role and, if the grant list is valid, its monitoring
// grant in one write. MonitoringAccess rejects users without a role.
func (s *RoleStore) RemoveUser(ctx context.Context, email string) error {
	err := s.updateUsers(ctx, func(roles, data map[string]string) {
		delete(roles, email)
		_ = usergrants.SetMonitoring(data, email, false)
	})
	if err != nil {
		log.Printf("RemoveUser: email=%q save failed: %v", email, err)
	}
	return err
}

// updateUsers applies mutate to the stored roles and the rest of the
// ConfigMap data in one conflict-retried write, creating the ConfigMap when
// missing, then drops the cache so the next read sees the stored result.
func (s *RoleStore) updateUsers(ctx context.Context, mutate func(roles, data map[string]string)) error {
	defer s.invalidateCache()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cms := s.client.CoreV1().ConfigMaps(roleConfigMapNamespace)
		cm, err := cms.Get(ctx, roleConfigMapName, metav1.GetOptions{})
		create := apierrors.IsNotFound(err)
		switch {
		case create:
			cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Name:      roleConfigMapName,
				Namespace: roleConfigMapNamespace,
				Labels:    map[string]string{"app.kubernetes.io/managed-by": "kipper"},
			}}
		case err != nil:
			return err
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		roles := map[string]string{}
		if raw := cm.Data[usergrants.UsersKey]; raw != "" {
			if err := json.Unmarshal([]byte(raw), &roles); err != nil {
				return fmt.Errorf("parsing %s in %s/%s: %w", usergrants.UsersKey, roleConfigMapNamespace, roleConfigMapName, err)
			}
		}
		mutate(roles, cm.Data)
		out, err := json.Marshal(roles)
		if err != nil {
			return err
		}
		cm.Data[usergrants.UsersKey] = string(out)
		if create {
			_, err = cms.Create(ctx, cm, metav1.CreateOptions{})
		} else {
			_, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
		}
		return err
	})
}

// ListUsers returns all users and their roles.
func (s *RoleStore) ListUsers() map[string]string {
	s.refresh()

	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make(map[string]string)
	for k, v := range s.roles {
		result[k] = v
	}
	return result
}

// RoleFromContext returns the authenticated user's role from the request context.
func RoleFromContext(ctx context.Context) string {
	role, _ := ctx.Value(RoleContextKey).(string)
	return role
}

// RoleMiddleware injects the user's role into the request context.
func RoleMiddleware(store *RoleStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims := UserFromContext(r.Context())
			if claims == nil {
				next.ServeHTTP(w, r)
				return
			}

			role := store.GetRole(claims.Email)
			if role == "" {
				http.Error(w, "forbidden: no role assigned. Contact your cluster admin", http.StatusForbidden)
				return
			}

			ctx := context.WithValue(r.Context(), RoleContextKey, role)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireRole returns middleware that restricts access to specific roles.
func RequireRole(roles ...string) func(http.Handler) http.Handler {
	allowed := make(map[string]bool)
	for _, r := range roles {
		allowed[r] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			role := RoleFromContext(r.Context())
			if !allowed[role] {
				http.Error(w, "forbidden: insufficient permissions", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
