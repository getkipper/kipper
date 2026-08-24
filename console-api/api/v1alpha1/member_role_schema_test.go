package v1alpha1

import (
	"os"
	"strings"
	"testing"
)

// The member role is a fixed enum of owner, deployer and viewer, and stops
// being one when custom roles arrive: a Project naming a role this build does
// not know must still validate, or the whole object becomes unwriteable and the
// console cannot even revoke the member holding it.
//
// The schema still has to constrain the value. A role name reaches a generated
// RoleBinding name, and an unconstrained string there is how a name with a
// slash or a space becomes an object nobody can address.
func TestTheMemberRoleAcceptsACustomRoleNameAndStillConstrainsIt(t *testing.T) {
	data, err := os.ReadFile("../../../deploy/crds/kipper.run_projects.yaml")
	if err != nil {
		t.Fatalf("reading the Project CRD: %v", err)
	}
	crd := string(data)

	// The enum listed the three built-ins as the only permitted values. While
	// it is there a Project carrying a custom role fails validation as a whole
	// object, so removing the member holding it is impossible.
	const builtInEnum = "enum:\n                      - owner\n                      - deployer\n                      - viewer"
	if strings.Contains(crd, builtInEnum) {
		t.Error("the member role is still a closed enum of the built-ins: a Project naming a custom role cannot be written at all, which locks the console out of revoking that member")
	}

	// What replaces it has to be narrow enough that a role name is safe to put
	// in an object name.
	if !strings.Contains(crd, "pattern: "+memberRolePattern) {
		t.Errorf("the member role carries no pattern; the CRD must constrain it to %s so a name cannot carry a character an object name may not", memberRolePattern)
	}
	if !strings.Contains(crd, "maxLength: 127") {
		t.Error("the member role carries no length cap, and a role name reaches a generated object name that has one")
	}
}
