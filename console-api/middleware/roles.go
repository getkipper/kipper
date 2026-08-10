package middleware

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
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
)

// RoleStore caches user-role mappings from the kipper-users ConfigMap.
type RoleStore struct {
	client    kubernetes.Interface
	roles     map[string]string
	mu        sync.RWMutex
	lastFetch time.Time
	cacheTTL  time.Duration
}

// NewRoleStore creates a RoleStore that reads from the cluster.
func NewRoleStore(client kubernetes.Interface) *RoleStore {
	return &RoleStore{
		client:   client,
		roles:    make(map[string]string),
		cacheTTL: 30 * time.Second,
	}
}

// GetRole returns the role for an email. If no user store exists, the first
// authenticated user is auto-registered as admin (initial setup only).
func (s *RoleStore) GetRole(email string) string {
	s.mu.RLock()
	if time.Since(s.lastFetch) < s.cacheTTL && len(s.roles) > 0 {
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

	// Unknown user (or a cold store that has never loaded) — deny. The
	// admin is seeded into the kipper-users ConfigMap at install time (see
	// DeployConsole), so there is no first-login bootstrap to grant admin
	// here. Access never widens on a read error: refresh() keeps the last
	// good cache rather than blanking it, so a user not in that cache is
	// denied. Already-known users keep their last role through a transient
	// ConfigMap read failure (an availability choice, bounded by the cache
	// TTL once reads recover) — a removed user is dropped on the next
	// successful refresh.
	return ""
}

func (s *RoleStore) refresh() {
	s.mu.RLock()
	if time.Since(s.lastFetch) < s.cacheTTL {
		s.mu.RUnlock()
		return
	}
	s.mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cm, err := s.client.CoreV1().ConfigMaps(roleConfigMapNamespace).Get(ctx, roleConfigMapName, metav1.GetOptions{})
	if err != nil {
		// Keep the existing cache rather than blanking it. A NotFound is
		// expected only before install seeds the store; any other error is
		// transient and must not widen access, so callers fail closed.
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

	s.mu.Lock()
	s.roles = roles
	s.lastFetch = time.Now()
	s.mu.Unlock()
}

func (s *RoleStore) invalidateCache() {
	s.mu.Lock()
	s.lastFetch = time.Time{}
	s.mu.Unlock()
}

// SetRole updates a user's role in the ConfigMap.
func (s *RoleStore) SetRole(ctx context.Context, email, role string) error {
	s.invalidateCache()
	s.refresh()

	s.mu.Lock()
	s.roles[email] = role
	rolesSnapshot := make(map[string]string)
	for k, v := range s.roles {
		rolesSnapshot[k] = v
	}
	s.mu.Unlock()

	return s.save(ctx, rolesSnapshot)
}

// RemoveUser removes a user from the role store.
func (s *RoleStore) RemoveUser(ctx context.Context, email string) error {
	s.invalidateCache()
	s.refresh()

	s.mu.Lock()
	_, existed := s.roles[email]
	delete(s.roles, email)
	rolesSnapshot := make(map[string]string)
	for k, v := range s.roles {
		rolesSnapshot[k] = v
	}
	s.mu.Unlock()

	log.Printf("RemoveUser: email=%q existed=%v remaining=%d", email, existed, len(rolesSnapshot))

	if err := s.save(ctx, rolesSnapshot); err != nil {
		log.Printf("RemoveUser: save failed: %v", err)
		return err
	}
	return nil
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

func (s *RoleStore) save(ctx context.Context, roles map[string]string) error {
	data, err := json.Marshal(roles)
	if err != nil {
		return err
	}

	cm, err := s.client.CoreV1().ConfigMaps(roleConfigMapNamespace).Get(ctx, roleConfigMapName, metav1.GetOptions{})
	if err != nil {
		// Create it
		_, err = s.client.CoreV1().ConfigMaps(roleConfigMapNamespace).Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      roleConfigMapName,
				Namespace: roleConfigMapNamespace,
				Labels:    map[string]string{"app.kubernetes.io/managed-by": "kipper"},
			},
			Data: map[string]string{"users": string(data)},
		}, metav1.CreateOptions{})
		return err
	}

	cm.Data["users"] = string(data)
	_, err = s.client.CoreV1().ConfigMaps(roleConfigMapNamespace).Update(ctx, cm, metav1.UpdateOptions{})
	return err
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
				http.Error(w, "forbidden: no role assigned — contact your cluster admin", http.StatusForbidden)
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
