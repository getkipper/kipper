package handlers

import (
	"net/http"

	"github.com/getkipper/kipper/controller/pkg/capability"
)

// projectRoleResponse is one role a project member may be given.
type projectRoleResponse struct {
	Name string `json:"name"`
	// Capabilities is what the role may do. The console shows it so somebody
	// choosing a role can see what they are handing over, rather than reading
	// three names and guessing the ordering between them.
	Capabilities []string `json:"capabilities"`
}

// ProjectRoles lists the roles a member may be given.
//
// The console used to hold this list itself, which meant the set of roles was
// decided in two places and only one of them shipped with the catalogue. A role
// this build gains appears here without the console changing, and a console
// older than the cluster shows the roles it knows rather than inventing one.
//
// It needs no project: the roles are the cluster's, and which of them a caller
// may hand out is decided when they try, by the gate on the member routes.
func ProjectRoles(w http.ResponseWriter, _ *http.Request) {
	roles := []capability.Role{capability.RoleViewer, capability.RoleDeployer, capability.RoleOwner}
	out := make([]projectRoleResponse, 0, len(roles))
	for _, role := range roles {
		out = append(out, projectRoleResponse{
			Name:         string(role),
			Capabilities: capabilitiesFor(string(role)),
		})
	}
	respondJSON(w, http.StatusOK, out)
}
