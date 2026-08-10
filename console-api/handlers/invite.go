package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/getkipper/kipper/console-api/middleware"
)

const (
	inviteConfigMapName = "kipper-invites"
	inviteNamespace     = "kipper-system"
)

// Invites handles invite link creation and acceptance.
type Invites struct {
	Client    kubernetes.Interface
	CRClient  crclient.Client
	RoleStore *middleware.RoleStore
	Users     *Users
}

type invite struct {
	Token   string `json:"token"`
	Role    string `json:"role"`
	Expires string `json:"expires"`
	Email   string `json:"email,omitempty"`
	// Project and ProjectRole are set when the invite grants membership of a
	// specific project on acceptance.
	Project     string `json:"project,omitempty"`
	ProjectRole string `json:"project_role,omitempty"`
}

type createInviteRequest struct {
	Role        string `json:"role"`
	Expires     string `json:"expires"` // duration string: "24h", "48h", "7d"
	Email       string `json:"email"`   // optional — if set, invite is sent by email
	Project     string `json:"project"`
	ProjectRole string `json:"project_role"`
}

type createInviteResponse struct {
	URL         string `json:"url"`
	Token       string `json:"token"`
	Role        string `json:"role"`
	Expires     string `json:"expires"`
	EmailSent   bool   `json:"email_sent"`
	Project     string `json:"project,omitempty"`
	ProjectRole string `json:"project_role,omitempty"`
}

type acceptInviteRequest struct {
	Token    string `json:"token"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

// Create generates an invite link.
// POST /api/v1/invites
func (inv *Invites) Create(w http.ResponseWriter, r *http.Request) {
	var req createInviteRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Every invite is addressed. Without one it is a bearer credential: whoever
	// sees the link takes its role under whatever identity they type, and the
	// role can be admin. The account is keyed by an email either way — the
	// person accepting has to supply one — so requiring it here asks for nothing
	// the flow does not already need. It moves the choice of identity from
	// whoever holds the token to whoever issues the invite.
	//
	// This does not require mail to be configured. The response carries the URL
	// whether or not it was also sent; emailSent below says which happened.
	req.Email = strings.TrimSpace(req.Email)
	if req.Email == "" {
		respondError(w, http.StatusBadRequest, "an invite needs the email address of the person it is for")
		return
	}

	if req.Role == "" {
		req.Role = middleware.RoleDeployer
	}
	if req.Role != middleware.RoleAdmin && req.Role != middleware.RoleDeployer && req.Role != middleware.RoleViewer {
		respondError(w, http.StatusBadRequest, "role must be admin, deployer, or viewer")
		return
	}

	// A project invite grants membership of one project on acceptance. The
	// global role stays viewer so the account can sign in without cluster-wide
	// powers; the project role is what governs their access there.
	if req.Project != "" {
		if req.ProjectRole == "" {
			req.ProjectRole = middleware.ProjectRoleDeployer
		}
		if req.ProjectRole != middleware.ProjectRoleOwner &&
			req.ProjectRole != middleware.ProjectRoleDeployer &&
			req.ProjectRole != middleware.ProjectRoleViewer {
			respondError(w, http.StatusBadRequest, "project_role must be owner, deployer, or viewer")
			return
		}
	}

	if req.Expires == "" {
		req.Expires = "48h"
	}

	duration, err := parseDuration(req.Expires)
	if err != nil {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("invalid expiry: %v", err))
		return
	}

	// Generate token
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}
	token := hex.EncodeToString(tokenBytes)

	expires := time.Now().Add(duration).Format(time.RFC3339)

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Store invite
	if err := inv.storeInvite(ctx, invite{
		Token: token, Role: req.Role, Expires: expires, Email: req.Email,
		Project: req.Project, ProjectRole: req.ProjectRole,
	}); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to store invite: %v", err))
		return
	}

	// Build URL from request host
	scheme := "https"
	host := r.Host
	if host == "" {
		host = r.Header.Get("Origin")
	}
	// Console URL: replace the console-api prefix with console, under both
	// the kipper.run double-dash convention and the custom-domain dot form.
	consoleHost := host
	for _, m := range []struct{ apiPrefix, consolePrefix string }{
		{"console-api--", "console--"},
		{"console-api.", "console."},
		{"api.", "console."},
	} {
		if len(consoleHost) > len(m.apiPrefix) && consoleHost[:len(m.apiPrefix)] == m.apiPrefix {
			consoleHost = m.consolePrefix + consoleHost[len(m.apiPrefix):]
			break
		}
	}

	url := fmt.Sprintf("%s://%s/invite/%s", scheme, consoleHost, token)

	emailSent := false
	if req.Email != "" {
		emailSvc := &EmailService{Client: inv.Client}
		if emailSvc.Configured(ctx) {
			subject := fmt.Sprintf("You're invited to join %s", consoleHost)
			body := inviteEmailHTML(url, req.Role, req.Expires, consoleHost)
			if err := emailSvc.Send(ctx, req.Email, subject, body); err == nil {
				emailSent = true
			}
		}
	}

	respondJSON(w, http.StatusOK, createInviteResponse{
		URL:         url,
		Token:       token,
		Role:        req.Role,
		Expires:     expires,
		EmailSent:   emailSent,
		Project:     req.Project,
		ProjectRole: req.ProjectRole,
	})
}

// Accept processes an invite — creates the user account.
// POST /api/v1/invites/{token}/accept
// This endpoint is NOT authenticated — the invite token is the auth.
func (inv *Invites) Accept(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")

	var req acceptInviteRequest
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

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	// Load and validate invite
	inviteData, err := inv.loadInvite(ctx, token)
	if err != nil {
		respondError(w, http.StatusNotFound, "invite not found or expired")
		return
	}

	// Check expiry
	expires, err := time.Parse(time.RFC3339, inviteData.Expires)
	if err != nil || time.Now().After(expires) {
		respondError(w, http.StatusGone, "invite has expired")
		return
	}

	// An invite addressed to someone is theirs. The token travels by email, so
	// anyone who comes by the message — a forward, an archive, a shared inbox —
	// could otherwise redeem it under an address of their own and take the role
	// and project membership it carries. Checked before the invite is claimed,
	// so a refused attempt leaves it for the person it was sent to.
	//
	// An invite created without an address is a link anyone holding it may
	// redeem, which is what a link invite is; there is nothing here to bind it
	// to.
	if inviteData.Email != "" && !strings.EqualFold(strings.TrimSpace(req.Email), strings.TrimSpace(inviteData.Email)) {
		respondError(w, http.StatusForbidden, "this invite was sent to a different address")
		return
	}

	// Claim the invite before doing anything with it. Deleting it afterwards
	// let two requests arriving together both find it live and both go through,
	// so one invite minted two accounts — and for an admin invite, two admins.
	// A claim that fails because someone else took it is not an error to report
	// as a server fault: the invite is simply spent.
	//
	// The cost of claiming first is that a later failure burns the invite. That
	// is the right way round: an operator can issue another invite, and cannot
	// un-mint an account.
	claimed, err := inv.claimInvite(ctx, token)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "could not claim the invite")
		return
	}
	if !claimed {
		respondError(w, http.StatusGone, "this invite has already been used")
		return
	}

	// Once an invite carries an address, that address is the identity. The match
	// above is a courtesy so a typo says so rather than silently making a second
	// account; it is not what decides who gets created. EqualFold is Unicode
	// simple folding rather than address canonicalisation, so keying the account
	// by what the request typed would let a spelling that merely folds to the
	// invited one hold the role — and would carry any surrounding whitespace
	// into Dex, the role store and the project membership alike.
	accountEmail := strings.TrimSpace(req.Email)
	if inviteData.Email != "" {
		accountEmail = strings.TrimSpace(inviteData.Email)
	}

	// Create the user in Dex
	if err := inv.Users.addDexUser(ctx, accountEmail, req.Password); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to create account: %v", err))
		return
	}

	// Assign the role
	if err := inv.RoleStore.SetRole(ctx, accountEmail, inviteData.Role); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to assign role")
		return
	}

	// Grant project membership if the invite was scoped to a project.
	if inviteData.Project != "" && inv.CRClient != nil {
		if err := addProjectMember(ctx, inv.CRClient, inviteData.Project, accountEmail, inviteData.ProjectRole); err != nil {
			respondError(w, http.StatusInternalServerError, "account created but failed to add you to the project")
			return
		}
	}

	respondJSON(w, http.StatusOK, map[string]string{
		"status": "created",
		"email":  accountEmail,
		"role":   inviteData.Role,
	})
}

// Validate checks if an invite token is valid (for the frontend to show the form).
// GET /api/v1/invites/{token}
func (inv *Invites) Validate(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	inviteData, err := inv.loadInvite(ctx, token)
	if err != nil {
		respondError(w, http.StatusNotFound, "invite not found")
		return
	}

	expires, err := time.Parse(time.RFC3339, inviteData.Expires)
	if err != nil || time.Now().After(expires) {
		respondError(w, http.StatusGone, "invite has expired")
		return
	}

	resp := map[string]string{
		"role":    inviteData.Role,
		"expires": inviteData.Expires,
	}
	// The address is deliberately not returned. Acceptance requires it to match,
	// and handing it to an unauthenticated caller holding the token turns that
	// check into a formality: whoever has the link is told what to type. It does
	// not help against a forwarded invitation email, whose headers carry the
	// address anyway, but it does against a link pasted into chat, left in
	// browser history, or captured in a log.
	if inviteData.Project != "" {
		resp["project"] = inviteData.Project
		resp["project_role"] = inviteData.ProjectRole
	}
	respondJSON(w, http.StatusOK, resp)
}

type pendingInvite struct {
	Token       string `json:"token"`
	Email       string `json:"email,omitempty"`
	Role        string `json:"role"`
	Expires     string `json:"expires"`
	Project     string `json:"project,omitempty"`
	ProjectRole string `json:"project_role,omitempty"`
}

// List returns all pending (non-expired) invites.
// GET /api/v1/invites
func (inv *Invites) List(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	cm, err := inv.Client.CoreV1().ConfigMaps(inviteNamespace).Get(ctx, inviteConfigMapName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		respondJSON(w, http.StatusOK, []pendingInvite{})
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to read invites")
		return
	}

	var invites map[string]invite
	if err := json.Unmarshal([]byte(cm.Data["invites"]), &invites); err != nil {
		respondJSON(w, http.StatusOK, []pendingInvite{})
		return
	}

	now := time.Now()
	var result []pendingInvite
	for _, i := range invites {
		exp, err := time.Parse(time.RFC3339, i.Expires)
		if err != nil || now.After(exp) {
			continue
		}
		result = append(result, pendingInvite{
			Token:       i.Token[:8] + "...",
			Email:       i.Email,
			Role:        i.Role,
			Expires:     i.Expires,
			Project:     i.Project,
			ProjectRole: i.ProjectRole,
		})
	}

	if result == nil {
		result = []pendingInvite{}
	}
	respondJSON(w, http.StatusOK, result)
}

// Revoke deletes a pending invite by token prefix.
// DELETE /api/v1/invites/{token}
func (inv *Invites) Revoke(w http.ResponseWriter, r *http.Request) {
	tokenPrefix := chi.URLParam(r, "token")

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	cm, err := inv.Client.CoreV1().ConfigMaps(inviteNamespace).Get(ctx, inviteConfigMapName, metav1.GetOptions{})
	if err != nil {
		respondError(w, http.StatusNotFound, "invite not found")
		return
	}

	var invites map[string]invite
	if err := json.Unmarshal([]byte(cm.Data["invites"]), &invites); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to parse invites")
		return
	}

	found := false
	for token := range invites {
		if len(token) >= 8 && token[:8]+"..." == tokenPrefix {
			delete(invites, token)
			found = true
			break
		}
	}

	if !found {
		respondError(w, http.StatusNotFound, "invite not found")
		return
	}

	data, _ := json.Marshal(invites)
	cm.Data["invites"] = string(data)
	if _, err := inv.Client.CoreV1().ConfigMaps(inviteNamespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to revoke invite")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

func (inv *Invites) storeInvite(ctx context.Context, i invite) error {
	cm, err := inv.Client.CoreV1().ConfigMaps(inviteNamespace).Get(ctx, inviteConfigMapName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		invites := map[string]invite{i.Token: i}
		data, _ := json.Marshal(invites)
		_, err = inv.Client.CoreV1().ConfigMaps(inviteNamespace).Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: inviteConfigMapName, Namespace: inviteNamespace,
				Labels: map[string]string{"app.kubernetes.io/managed-by": "kipper"},
			},
			Data: map[string]string{"invites": string(data)},
		}, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}

	var invites map[string]invite
	if err := json.Unmarshal([]byte(cm.Data["invites"]), &invites); err != nil {
		invites = map[string]invite{}
	}

	// Clean expired invites while we're at it
	now := time.Now()
	for k, v := range invites {
		if exp, err := time.Parse(time.RFC3339, v.Expires); err == nil && now.After(exp) {
			delete(invites, k)
		}
	}

	invites[i.Token] = i
	data, _ := json.Marshal(invites)
	cm.Data["invites"] = string(data)
	_, err = inv.Client.CoreV1().ConfigMaps(inviteNamespace).Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

func (inv *Invites) loadInvite(ctx context.Context, token string) (*invite, error) {
	cm, err := inv.Client.CoreV1().ConfigMaps(inviteNamespace).Get(ctx, inviteConfigMapName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	var invites map[string]invite
	if err := json.Unmarshal([]byte(cm.Data["invites"]), &invites); err != nil {
		return nil, err
	}

	i, ok := invites[token]
	if !ok {
		return nil, fmt.Errorf("invite not found")
	}
	return &i, nil
}

// claimInvite removes an invite and reports whether this caller is the one that
// removed it. Exactly one concurrent caller can be told true.
//
// The invites live in one ConfigMap, so the claim is a compare-and-set on that
// object: the write carries the resourceVersion it was read at, and the API
// server rejects it if anything changed in between. A rejection means someone
// else wrote — so the map is read again and the token looked for afresh, and a
// caller that no longer finds it lost the race rather than hit an error.
//
// RetryOnConflict is not enough on its own. It would retry the write, but a
// retry that reapplied a stale map would put back invites another request had
// just claimed, which is the same lost update in the other direction.
func (inv *Invites) claimInvite(ctx context.Context, token string) (bool, error) {
	claimed := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		claimed = false
		cm, err := inv.Client.CoreV1().ConfigMaps(inviteNamespace).Get(ctx, inviteConfigMapName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		var invites map[string]invite
		if err := json.Unmarshal([]byte(cm.Data["invites"]), &invites); err != nil {
			return err
		}
		if _, ok := invites[token]; !ok {
			// Someone else claimed it between the read that validated it and
			// this one. Nothing to write, and nothing to report as an error.
			return nil
		}
		delete(invites, token)
		data, err := json.Marshal(invites)
		if err != nil {
			return err
		}
		cm.Data["invites"] = string(data)
		if _, err := inv.Client.CoreV1().ConfigMaps(inviteNamespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
			return err
		}
		claimed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return claimed, nil
}

func parseDuration(s string) (time.Duration, error) {
	// Support "7d" as well as standard Go durations
	if len(s) > 1 && s[len(s)-1] == 'd' {
		var days int
		if _, err := fmt.Sscanf(s, "%dd", &days); err == nil {
			return time.Duration(days) * 24 * time.Hour, nil
		}
	}
	return time.ParseDuration(s)
}
