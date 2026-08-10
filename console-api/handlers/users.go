package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"github.com/getkipper/kipper/console-api/middleware"
	"github.com/getkipper/kipper/console-api/security"
	"github.com/getkipper/kipper/console-api/uisession"
)

// Users provides handlers for user management.
type Users struct {
	Client    kubernetes.Interface
	RoleStore *middleware.RoleStore
	// Security records account mutations. Deleting or demoting admins is
	// the quiet first move before silencing notification channels, so the
	// mutation itself alerts — while the current recipient list still
	// includes everyone about to be cut out.
	Security *security.Notifier
	// UISessions revokes a removed user's service-UI sessions. Record
	// deletion is the authoritative revocation path (the role check is only
	// defense-in-depth), so removing a user must delete their session
	// records. Nil skips it (record deletion is best effort).
	UISessions *uisession.RecordStore
}

// recipientSnapshot captures who must hear about an account mutation,
// taken BEFORE the mutation runs: the current admins plus the affected
// account. Resolving recipients any later would let a deletion or demotion
// cut its own victim out of the alert.
func (u *Users) recipientSnapshot(affected string) []string {
	seen := map[string]bool{}
	var recipients []string
	for email, role := range u.RoleStore.ListUsers() {
		if role == middleware.RoleAdmin && !seen[email] {
			seen[email] = true
			recipients = append(recipients, email)
		}
	}
	if affected != "" && !seen[affected] {
		recipients = append(recipients, affected)
	}
	return recipients
}

// emitUserEvent reports one account mutation, naming the acting admin and
// the affected account, delivered to the pre-mutation recipient snapshot.
func (u *Users) emitUserEvent(r *http.Request, recipients []string, kind, summary string, fields ...security.Field) {
	if u.Security == nil {
		return
	}
	actor := ""
	if claims := middleware.UserFromContext(r.Context()); claims != nil {
		actor = claims.Email
	}
	u.Security.Emit(r.Context(), security.Event{Kind: kind, User: actor, Summary: summary, Fields: fields, Recipients: recipients})
}

type userResponse struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

type createUserRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

type updateRoleRequest struct {
	Role string `json:"role"`
}

// List returns all users with their roles.
// GET /api/v1/users
func (u *Users) List(w http.ResponseWriter, _ *http.Request) {
	users := u.RoleStore.ListUsers()

	result := make([]userResponse, 0, len(users))
	for email, role := range users {
		result = append(result, userResponse{Email: email, Role: role})
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Email < result[j].Email
	})

	respondJSON(w, http.StatusOK, result)
}

// Create creates a new user in both the role store and Dex.
// POST /api/v1/users
func (u *Users) Create(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Email == "" || req.Password == "" {
		respondError(w, http.StatusBadRequest, "email and password are required")
		return
	}

	if err := validatePassword(req.Password); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	if req.Role == "" {
		req.Role = middleware.RoleDeployer
	}

	if req.Role != middleware.RoleAdmin && req.Role != middleware.RoleDeployer && req.Role != middleware.RoleViewer {
		respondError(w, http.StatusBadRequest, "role must be admin, deployer, or viewer")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	recipients := u.recipientSnapshot(req.Email)

	// Add to Dex static passwords
	if err := u.addDexUser(ctx, req.Email, req.Password); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to add user to Dex: %v", err))
		return
	}

	// Add to role store
	if err := u.RoleStore.SetRole(ctx, req.Email, req.Role); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to set role: %v", err))
		return
	}

	u.emitUserEvent(r, recipients, "user_created",
		fmt.Sprintf("user %s created with role %s", req.Email, req.Role),
		security.Field{Key: "user", Value: req.Email},
		security.Field{Key: "role", Value: req.Role})
	respondJSON(w, http.StatusOK, userResponse{Email: req.Email, Role: req.Role})
}

// UpdateRole changes a user's role.
// PUT /api/v1/users/{email}/role
func (u *Users) UpdateRole(w http.ResponseWriter, r *http.Request) {
	email, _ := url.PathUnescape(chi.URLParam(r, "email"))

	var req updateRoleRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Role != middleware.RoleAdmin && req.Role != middleware.RoleDeployer && req.Role != middleware.RoleViewer {
		respondError(w, http.StatusBadRequest, "role must be admin, deployer, or viewer")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	previousRole := u.RoleStore.GetRole(email)
	recipients := u.recipientSnapshot(email)
	if err := u.RoleStore.SetRole(ctx, email, req.Role); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update role")
		return
	}

	u.emitUserEvent(r, recipients, "user_role_changed",
		fmt.Sprintf("role of %s changed from %s to %s", email, previousRole, req.Role),
		security.Field{Key: "user", Value: email},
		security.Field{Key: "previous_role", Value: previousRole},
		security.Field{Key: "new_role", Value: req.Role})
	respondJSON(w, http.StatusOK, userResponse{Email: email, Role: req.Role})
}

// Delete removes a user from both Dex and the role store.
// DELETE /api/v1/users/{email}
func (u *Users) Delete(w http.ResponseWriter, r *http.Request) {
	email, _ := url.PathUnescape(chi.URLParam(r, "email"))

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	recipients := u.recipientSnapshot(email)
	removedRole := u.RoleStore.GetRole(email)

	// Revoke the user's service-UI sessions first, and treat a failure as
	// fatal to the whole deletion. Record deletion is the authoritative
	// revocation path, so it must not be downgraded to best effort: if it
	// were and only Dex + the role were removed, the surviving records would
	// resurrect the user's old session cookies the moment the same email is
	// re-added (the role check goes live again). Doing it first means a
	// failure aborts before anything else changes, and removeDexUser /
	// RemoveUser are both idempotent, so the admin simply retries. Records
	// are labelled by an email hash too, so removal (which knows the email,
	// not the Dex subject) still targets them.
	if u.UISessions != nil {
		if err := u.UISessions.DeleteBySubject(ctx, email); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to revoke user sessions — no account state changed, retry")
			return
		}
	}

	// Remove from Dex
	if err := u.removeDexUser(ctx, email); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to remove from Dex: %v", err))
		return
	}

	// Remove from role store
	if err := u.RoleStore.RemoveUser(ctx, email); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to remove role")
		return
	}

	u.emitUserEvent(r, recipients, "user_deleted",
		fmt.Sprintf("user %s (%s) deleted", email, removedRole),
		security.Field{Key: "user", Value: email},
		security.Field{Key: "role", Value: removedRole})
	w.WriteHeader(http.StatusNoContent)
}

// ResetPassword generates a new password for a user.
// POST /api/v1/users/{email}/reset-password
func (u *Users) ResetPassword(w http.ResponseWriter, r *http.Request) {
	email, _ := url.PathUnescape(chi.URLParam(r, "email"))

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	// Generate new password
	passBytes := make([]byte, 12)
	if _, err := rand.Read(passBytes); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to generate password")
		return
	}
	newPassword := hex.EncodeToString(passBytes)

	recipients := u.recipientSnapshot(email)

	// Remove old entry and add new one
	_ = u.removeDexUser(ctx, email)
	if err := u.addDexUser(ctx, email, newPassword); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to reset password: %v", err))
		return
	}

	u.emitUserEvent(r, recipients, "user_password_reset",
		fmt.Sprintf("password of %s was reset", email),
		security.Field{Key: "user", Value: email})
	respondJSON(w, http.StatusOK, map[string]string{
		"email":    email,
		"password": newPassword,
	})
}

// Me returns the current user's info including role.
// GET /api/v1/me
func (u *Users) Me(w http.ResponseWriter, r *http.Request) {
	claims := middleware.UserFromContext(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	role := middleware.RoleFromContext(r.Context())

	respondJSON(w, http.StatusOK, map[string]string{
		"email": claims.Email,
		"name":  claims.Name,
		"role":  role,
	})
}

// addDexUser adds a static password entry to the Dex ConfigMap.
func (u *Users) addDexUser(ctx context.Context, email, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hashing password: %w", err)
	}

	userID := make([]byte, 16)
	if _, err := rand.Read(userID); err != nil {
		return fmt.Errorf("generating user ID: %w", err)
	}

	cm, err := u.Client.CoreV1().ConfigMaps("dex").Get(ctx, "dex-config", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading Dex config: %w", err)
	}

	configYAML := cm.Data["config.yaml"]

	// Parse the YAML to find the staticPasswords section
	var config map[string]interface{}
	if err := yaml.Unmarshal([]byte(configYAML), &config); err != nil {
		return fmt.Errorf("parsing Dex config: %w", err)
	}

	passwords, _ := config["staticPasswords"].([]interface{})

	// Check if user already exists
	username := strings.Split(email, "@")[0]
	for _, p := range passwords {
		if entry, ok := p.(map[string]interface{}); ok {
			if entry["email"] == email {
				return fmt.Errorf("user %s already exists", email)
			}
		}
	}

	newEntry := map[string]interface{}{
		"email":    email,
		"hash":     string(hash),
		"username": username,
		"userID":   hex.EncodeToString(userID),
	}
	passwords = append(passwords, newEntry)
	config["staticPasswords"] = passwords

	newYAML, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("marshalling Dex config: %w", err)
	}

	cm.Data["config.yaml"] = string(newYAML)
	if _, err := u.Client.CoreV1().ConfigMaps("dex").Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating Dex config: %w", err)
	}

	// Restart Dex to pick up the new user
	return u.restartDex(ctx)
}

// removeDexUser removes a static password entry from the Dex ConfigMap.
func (u *Users) removeDexUser(ctx context.Context, email string) error {
	cm, err := u.Client.CoreV1().ConfigMaps("dex").Get(ctx, "dex-config", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading Dex config: %w", err)
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal([]byte(cm.Data["config.yaml"]), &config); err != nil {
		return fmt.Errorf("parsing Dex config: %w", err)
	}

	passwords, _ := config["staticPasswords"].([]interface{})
	originalCount := len(passwords)
	var filtered []interface{}
	for _, p := range passwords {
		if entry, ok := p.(map[string]interface{}); ok {
			if entry["email"] == email {
				log.Printf("removeDexUser: removing entry for %s", email)
				continue
			}
		}
		filtered = append(filtered, p)
	}

	if len(filtered) == originalCount {
		log.Printf("removeDexUser: email %q not found in %d static passwords", email, originalCount)
		for i, p := range passwords {
			if entry, ok := p.(map[string]interface{}); ok {
				log.Printf("  password[%d]: email=%v (type %T)", i, entry["email"], entry["email"])
			}
		}
	}

	config["staticPasswords"] = filtered

	newYAML, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("marshalling Dex config: %w", err)
	}

	cm.Data["config.yaml"] = string(newYAML)
	if _, err := u.Client.CoreV1().ConfigMaps("dex").Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating Dex config: %w", err)
	}

	log.Printf("removeDexUser: successfully updated ConfigMap, %d -> %d passwords", originalCount, len(filtered))
	return u.restartDex(ctx)
}

func (u *Users) restartDex(ctx context.Context) error {
	deploy, err := u.Client.AppsV1().Deployments("dex").Get(ctx, "dex", metav1.GetOptions{})
	if err != nil {
		return nil //nolint:nilerr // non-fatal — Dex restart is best-effort
	}

	if deploy.Spec.Template.Annotations == nil {
		deploy.Spec.Template.Annotations = make(map[string]string)
	}
	deploy.Spec.Template.Annotations["kipper.run/restartedAt"] = time.Now().Format(time.RFC3339)
	_, err = u.Client.AppsV1().Deployments("dex").Update(ctx, deploy, metav1.UpdateOptions{})
	return err
}
