package handlers

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"k8s.io/apimachinery/pkg/api/errors"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// linkConsentRequest names the project being granted or refused the ability to
// link into this one.
type linkConsentRequest struct {
	Project string `json:"project"`
	// Allow is a pointer so omitting it is an error rather than the
	// destructive default. This grants and withdraws access to a backend; a
	// misspelled or missing field must not quietly mean "withdraw".
	Allow *bool `json:"allow"`
}

type linkConsentResponse struct {
	Project        string   `json:"project"`
	AllowLinksFrom []string `json:"allowLinksFrom"`
}

// LinkConsent returns the projects allowed to link into this one.
//
// GET /api/v1/projects/{name}/link-consent
func (p *Projects) LinkConsent(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var project kipperv1.Project
	if err := p.CRClient.Get(ctx, crclient.ObjectKey{Name: name}, &project); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("project %q not found", name))
			return
		}
		respondError(w, http.StatusInternalServerError, "reading project")
		return
	}
	respondJSON(w, http.StatusOK, linkConsentResponse{
		Project:        name,
		AllowLinksFrom: project.Spec.AllowLinksFrom,
	})
}

// SetLinkConsent grants or withdraws another project's ability to link into this
// one.
//
// PUT /api/v1/projects/{name}/link-consent
//
// This exists as a server-side operation rather than a direct edit of the
// Project because the decision belongs to whoever owns the project being
// reached, and a Kipper project owner holds a namespaced role — they have no
// access to the cluster-scoped Project resource, and granting them one would
// hand them every project on the cluster. The route's own middleware resolves
// membership of this project and requires the owner role, so the authority to
// decide sits exactly where the decision does.
//
// What is being granted is not small: a link opens a direct route to a backend
// in this project, past the ingress and so past anything enforced on a public
// route. Each individual link still names the one app it reaches.
func (p *Projects) SetLinkConsent(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	var req linkConsentRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Project == "" {
		respondError(w, http.StatusBadRequest, "project is required")
		return
	}
	if req.Allow == nil {
		respondError(w, http.StatusBadRequest, "allow is required: true to grant, false to withdraw")
		return
	}
	allow := *req.Allow
	if req.Project == name {
		respondError(w, http.StatusBadRequest, "a project already reaches its own apps")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var project kipperv1.Project
	if err := p.CRClient.Get(ctx, crclient.ObjectKey{Name: name}, &project); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("project %q not found", name))
			return
		}
		respondError(w, http.StatusInternalServerError, "reading project")
		return
	}

	// Granting names a project that has to exist. Withdrawing does not: the
	// entry may be left over from a project that is already gone, and refusing
	// to clean that up would strand it.
	if allow {
		var granted kipperv1.Project
		if err := p.CRClient.Get(ctx, crclient.ObjectKey{Name: req.Project}, &granted); err != nil {
			if errors.IsNotFound(err) {
				respondError(w, http.StatusNotFound, fmt.Sprintf("project %q not found", req.Project))
				return
			}
			respondError(w, http.StatusInternalServerError, "reading project")
			return
		}
	}

	updated, changed := applyLinkConsent(project.Spec.AllowLinksFrom, req.Project, allow)
	if changed {
		project.Spec.AllowLinksFrom = updated
		if err := p.CRClient.Update(ctx, &project); err != nil {
			// A conflict means someone else changed this project while the
			// request was in flight. Nothing was lost, and the caller can
			// simply try again — which 500 does not tell them.
			if errors.IsConflict(err) {
				respondError(w, http.StatusConflict, "the project changed while this was being applied; try again")
				return
			}
			respondError(w, http.StatusInternalServerError, "updating project")
			return
		}
	}
	respondJSON(w, http.StatusOK, linkConsentResponse{Project: name, AllowLinksFrom: updated})
}

// applyLinkConsent adds or removes a project from a consent list, keeping it
// sorted and free of duplicates, and reports whether anything changed.
func applyLinkConsent(allowed []string, project string, allow bool) ([]string, bool) {
	out := make([]string, 0, len(allowed)+1)
	found := false
	for _, existing := range allowed {
		if existing == project {
			found = true
			continue
		}
		out = append(out, existing)
	}
	if !allow {
		return out, found
	}
	if found {
		return allowed, false
	}
	out = append(out, project)
	sort.Strings(out)
	return out, true
}
