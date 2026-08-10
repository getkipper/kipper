package handlers

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/client-go/kubernetes"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/middleware"
	"github.com/getkipper/kipper/console-api/serviceui"
	"github.com/getkipper/kipper/console-api/share"
)

// Shares mints, lists, and revokes service-UI share links. All routes
// are admin-only (wired in main): a share link is a capability handed to
// someone outside the cluster's user base, so creating one is a
// platform-level act, not a project-level one.
type Shares struct {
	Client   kubernetes.Interface
	CRClient crclient.Client
	Grants   *share.GrantStore
	// Domain is the cluster's base domain, used to build the UI host a
	// minted link opens.
	Domain string
	// Now supplies the clock; nil means time.Now. Tests set it.
	Now func() time.Time
}

type createShareRequest struct {
	// ExpiresIn is a Go duration string ("72h"). Empty means the
	// default lifetime.
	ExpiresIn string `json:"expires_in"`
	// Label is a non-secret note shown in listings ("PO review").
	Label string `json:"label"`
}

type shareLinkResponse struct {
	ID        string    `json:"id"`
	URL       string    `json:"url,omitempty"`
	Label     string    `json:"label,omitempty"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Shares) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Create mints a new share link for a service's browseable UI.
// POST /api/v1/services/{name}/shares?namespace={ns}
func (s *Shares) Create(w http.ResponseWriter, r *http.Request) {
	name, namespace, ok := requireService(w, r)
	if !ok {
		return
	}

	var req createShareRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	lifetime := share.DefaultLifetime
	if req.ExpiresIn != "" {
		d, err := time.ParseDuration(req.ExpiresIn)
		if err != nil || d <= 0 {
			respondError(w, http.StatusBadRequest, "expires_in must be a positive duration like 72h")
			return
		}
		if d > share.MaxLifetime {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("expires_in must not exceed %s", share.MaxLifetime))
			return
		}
		lifetime = d
	}
	if len(req.Label) > 100 {
		respondError(w, http.StatusBadRequest, "label must be 100 characters or fewer")
		return
	}

	ctx := r.Context()
	var svc kipperv1.Service
	if err := s.CRClient.Get(ctx, crclient.ObjectKey{Namespace: namespace, Name: name}, &svc); err != nil {
		respondError(w, http.StatusNotFound, fmt.Sprintf("service %q not found", name))
		return
	}
	if !serviceui.Browseable(svc.Spec.Type) {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("service type %q has no browseable web UI to share", svc.Spec.Type))
		return
	}
	host := serviceui.Hostname(name, namespace, s.Domain)
	if host == "" {
		respondError(w, http.StatusConflict, "the cluster has no domain configured, so service UIs are not reachable")
		return
	}

	// The link must open a UI that actually routes: require the
	// reconciled Ingress to exist and serve the same host the token
	// will be bound to, instead of predicting one and hoping.
	var ing networkingv1.Ingress
	if err := s.CRClient.Get(ctx, crclient.ObjectKey{Namespace: namespace, Name: name + "-ui"}, &ing); err != nil {
		respondError(w, http.StatusConflict, "the service UI is not reconciled yet. Try again shortly")
		return
	}
	if len(ing.Spec.Rules) == 0 || ing.Spec.Rules[0].Host != host {
		respondError(w, http.StatusConflict, "the service UI hostname is not reconciled yet. Try again shortly")
		return
	}

	createdBy := ""
	if claims := middleware.UserFromContext(ctx); claims != nil {
		createdBy = claims.Email
	}

	grant, err := share.NewGrant(string(svc.UID), name, namespace, host, req.Label, createdBy, lifetime, s.now())
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Grants.Create(ctx, grant); err != nil {
		if errors.Is(err, share.ErrGrantLimit) {
			respondError(w, http.StatusConflict, err.Error())
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to store the share link")
		return
	}

	keyring, err := share.LoadOrCreateKeyring(ctx, s.Client)
	if err != nil {
		// The grant without its token is inert; remove it anyway so the
		// listing doesn't show a link that never existed.
		_ = s.Grants.Revoke(ctx, namespace, name, grant.JTI)
		respondError(w, http.StatusInternalServerError, "failed to load the signing key")
		return
	}
	token, err := share.MintGrant(keyring, grant, s.now())
	if err != nil {
		_ = s.Grants.Revoke(ctx, namespace, name, grant.JTI)
		respondError(w, http.StatusInternalServerError, "failed to sign the share link")
		return
	}

	// Security event, not an audit record (docs say so): identifies the
	// link and the actor, never the token.
	log.Printf("security event: share link minted service=%s/%s id=%s by=%s expires=%s",
		namespace, name, share.JTIPrefix(grant.JTI), createdBy, grant.ExpiresAt.Format(time.RFC3339))

	respondJSON(w, http.StatusCreated, shareLinkResponse{
		ID:        grant.JTI,
		URL:       fmt.Sprintf("https://%s/?kipper_share=%s", host, token),
		Label:     grant.Label,
		CreatedBy: grant.CreatedBy,
		CreatedAt: grant.CreatedAt,
		ExpiresAt: grant.ExpiresAt,
	})
}

// List returns a service's live share links. The token itself is not
// reconstructable — the response identifies links, it cannot open them.
// GET /api/v1/services/{name}/shares?namespace={ns}
func (s *Shares) List(w http.ResponseWriter, r *http.Request) {
	name, namespace, ok := requireService(w, r)
	if !ok {
		return
	}
	grants, err := s.Grants.List(r.Context(), namespace, name)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list share links")
		return
	}
	out := make([]shareLinkResponse, 0, len(grants))
	for _, g := range grants {
		out = append(out, shareLinkResponse{
			ID:        g.JTI,
			Label:     g.Label,
			CreatedBy: g.CreatedBy,
			CreatedAt: g.CreatedAt,
			ExpiresAt: g.ExpiresAt,
		})
	}
	respondJSON(w, http.StatusOK, out)
}

// Revoke deletes one share link.
// DELETE /api/v1/services/{name}/shares/{id}?namespace={ns}
func (s *Shares) Revoke(w http.ResponseWriter, r *http.Request) {
	name, namespace, ok := requireService(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if err := s.Grants.Revoke(r.Context(), namespace, name, id); err != nil {
		if errors.Is(err, share.ErrGrantNotFound) {
			respondError(w, http.StatusNotFound, "share link not found")
			return
		}
		// A storage failure means the link may still be live. Surface it
		// as a server error and log the failed revoke, rather than
		// telling the caller it is gone.
		log.Printf("security event: share link revoke FAILED service=%s/%s id=%s by=%s err=%v",
			namespace, name, share.JTIPrefix(id), actorEmail(r), err)
		respondError(w, http.StatusServiceUnavailable, "failed to revoke the share link: it may still be active, try again")
		return
	}
	log.Printf("security event: share link revoked service=%s/%s id=%s by=%s",
		namespace, name, share.JTIPrefix(id), actorEmail(r))
	w.WriteHeader(http.StatusNoContent)
}

// RevokeAll deletes every share link in the cluster — the emergency
// lever, paired with a key rotation in the compromise runbook.
// DELETE /api/v1/shares
func (s *Shares) RevokeAll(w http.ResponseWriter, r *http.Request) {
	if err := s.Grants.RevokeAll(r.Context()); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to revoke share links")
		return
	}
	log.Printf("security event: all share links revoked by=%s", actorEmail(r))
	w.WriteHeader(http.StatusNoContent)
}

// RotateKey demotes the current signing key and installs a fresh one.
// Links signed before the rotation stay valid until they expire or the
// next rotation; two rotations retire a key entirely.
// POST /api/v1/shares/rotate-key
func (s *Shares) RotateKey(w http.ResponseWriter, r *http.Request) {
	keyring, err := share.RotateKeyring(r.Context(), s.Client)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to rotate the signing key")
		return
	}
	log.Printf("security event: share signing key rotated to kid=%s by=%s", keyring.CurrentKID, actorEmail(r))
	respondJSON(w, http.StatusOK, map[string]string{"current_kid": keyring.CurrentKID})
}

func actorEmail(r *http.Request) string {
	if claims := middleware.UserFromContext(r.Context()); claims != nil {
		return claims.Email
	}
	return ""
}
