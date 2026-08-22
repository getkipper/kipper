package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"

	"github.com/go-chi/chi/v5"
	"k8s.io/client-go/util/retry"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/middleware"
)

// errLastOwner signals an attempt to remove a project's only owner.
var errLastOwner = errors.New("cannot remove the last owner")

// Members manages the membership of a project (Project CR spec.members).
type Members struct {
	CRClient  crclient.Client
	RoleStore *middleware.RoleStore
}

type memberEntry struct {
	Email string `json:"email"`
	Role  string `json:"role"`

	// Unrecognised marks a role this build does not know. Such a member holds
	// no access at all, because the projection binds only the roles it knows,
	// and the console has to say so rather than show the name as though it
	// granted something. A role arrives here by kubectl, by a restore, or by a
	// migration from a cluster that had it.
	Unrecognised bool `json:"unrecognised,omitempty"`
}

func validProjectRole(role string) bool {
	switch role {
	case middleware.ProjectRoleOwner, middleware.ProjectRoleDeployer, middleware.ProjectRoleViewer:
		return true
	default:
		return false
	}
}

// projectFromRequest returns the canonical project name resolved by the
// project-scope middleware, falling back to the {name} path segment.
func projectFromRequest(r *http.Request) string {
	if access, ok := middleware.ProjectAccessFromContext(r.Context()); ok && access.Project != "" {
		return access.Project
	}
	return chi.URLParam(r, "name")
}

// List returns the members of a project.
// GET /api/v1/projects/{name}/members
func (m *Members) List(w http.ResponseWriter, r *http.Request) {
	project := projectFromRequest(r)

	var p kipperv1.Project
	if err := m.CRClient.Get(r.Context(), crclient.ObjectKey{Name: project}, &p); err != nil {
		respondError(w, http.StatusNotFound, "project not found")
		return
	}

	members := make([]memberEntry, 0, len(p.Spec.Members))
	for _, mem := range p.Spec.Members {
		members = append(members, memberEntry{
			Email:        mem.Email,
			Role:         string(mem.Role),
			Unrecognised: !validProjectRole(string(mem.Role)),
		})
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Email < members[j].Email })
	respondJSON(w, http.StatusOK, members)
}

// Set adds a member or changes their role.
// PUT /api/v1/projects/{name}/members
func (m *Members) Set(w http.ResponseWriter, r *http.Request) {
	project := projectFromRequest(r)

	var req memberEntry
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Email == "" || !validProjectRole(req.Role) {
		respondError(w, http.StatusBadRequest, "email and a valid role (owner, deployer, viewer) are required")
		return
	}
	// A member must have a console account to sign in; project membership does
	// not create one. Onboard new people through an invite instead.
	if m.RoleStore.GetRole(req.Email) == "" {
		respondError(w, http.StatusBadRequest, "no account exists for that email: invite the user first")
		return
	}

	if err := addProjectMember(r.Context(), m.CRClient, project, req.Email, req.Role); err != nil {
		if errors.Is(err, errLastOwner) {
			respondError(w, http.StatusBadRequest, "cannot demote the last owner of a project")
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to update members")
		return
	}
	respondJSON(w, http.StatusOK, req)
}

// addProjectMember adds email to the project with the given role, or updates
// their role if they are already a member. Demoting the project's only owner is
// rejected so a project always keeps at least one owner.
func addProjectMember(ctx context.Context, cr crclient.Client, project, email, role string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var p kipperv1.Project
		if err := cr.Get(ctx, crclient.ObjectKey{Name: project}, &p); err != nil {
			return err
		}
		for i := range p.Spec.Members {
			if p.Spec.Members[i].Email == email {
				if p.Spec.Members[i].Role == kipperv1.ProjectRoleOwner &&
					role != string(kipperv1.ProjectRoleOwner) &&
					countOwners(p.Spec.Members) == 1 {
					return errLastOwner
				}
				p.Spec.Members[i].Role = kipperv1.ProjectMemberRole(role)
				return cr.Update(ctx, &p)
			}
		}
		p.Spec.Members = append(p.Spec.Members, kipperv1.ProjectMember{
			Email: email,
			Role:  kipperv1.ProjectMemberRole(role),
		})
		return cr.Update(ctx, &p)
	})
}

func countOwners(members []kipperv1.ProjectMember) int {
	n := 0
	for _, m := range members {
		if m.Role == kipperv1.ProjectRoleOwner {
			n++
		}
	}
	return n
}

// Remove removes a member from a project.
// DELETE /api/v1/projects/{name}/members/{email}
func (m *Members) Remove(w http.ResponseWriter, r *http.Request) {
	project := projectFromRequest(r)

	// The address arrives percent-encoded — the console sends
	// encodeURIComponent, so `@` is `%40` — and chi routes on RawPath when one
	// is set, handing the raw segment through as the parameter. Comparing that
	// against the decoded address stored on the CR matched nothing, so the
	// member list was written back unchanged and the response said "removed".
	// The operator saw the row go and the member kept their access.
	//
	// A failed unescape is refused rather than half-used: the partial string
	// PathUnescape returns alongside its error is not an address.
	email, unescapeErr := url.PathUnescape(chi.URLParam(r, "email"))
	if unescapeErr != nil {
		respondError(w, http.StatusBadRequest, "invalid member address")
		return
	}

	// A cluster admin may remove a project's last owner; nobody else may.
	//
	// The guard below counts owners by their role in the member list, and it
	// cannot know whether an address belongs to anyone. An owner added under a
	// mistyped address therefore counts, and the real owner can be removed
	// against it — leaving the project owned by somebody who will never sign in,
	// with the guard then refusing every attempt to put it right.
	//
	// Adding a valid owner and removing the bad one needs no exemption and is
	// the better repair, because it ends with the project owned. This exists for
	// the case where the phantom should go before a replacement is chosen. A
	// project owner is still held to the rule, since an owner removing the last
	// owner is the accident it is for.
	//
	// The route already resolved the caller's authority, so it is read back
	// rather than derived a second time from the cluster role.
	access, _ := middleware.ProjectAccessFromContext(r.Context())
	isClusterAdmin := access.IsAdmin

	removed := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var p kipperv1.Project
		if err := m.CRClient.Get(r.Context(), crclient.ObjectKey{Name: project}, &p); err != nil {
			return err
		}

		kept := make([]kipperv1.ProjectMember, 0, len(p.Spec.Members))
		owners := 0
		removingOwner := false
		removed = false
		for _, mem := range p.Spec.Members {
			if mem.Email == email {
				if mem.Role == kipperv1.ProjectRoleOwner {
					removingOwner = true
				}
				removed = true
				continue
			}
			if mem.Role == kipperv1.ProjectRoleOwner {
				owners++
			}
			kept = append(kept, mem)
		}
		// Keep at least one owner so a project can't be orphaned.
		if removingOwner && owners == 0 && !isClusterAdmin {
			return errLastOwner
		}
		// An address in no entry leaves the list identical, so writing it back
		// bumps the resourceVersion to say nothing.
		if !removed {
			return nil
		}
		p.Spec.Members = kept
		return m.CRClient.Update(r.Context(), &p)
	})
	if errors.Is(err, errLastOwner) {
		respondError(w, http.StatusBadRequest, "cannot remove the last owner of a project")
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to remove member")
		return
	}
	// Saying "removed" about a member who was never there is how the encoding
	// bug above stayed invisible: the console believed every removal.
	if !removed {
		respondError(w, http.StatusNotFound, "that address is not a member of this project")
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "removed", "email": email})
}
