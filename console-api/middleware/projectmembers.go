package middleware

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// CRProjectMembers reads project membership from Project custom resources.
type CRProjectMembers struct {
	Client crclient.Client
}

// ProjectMembers returns the project's members as an email->role map. found is
// false when the Project CR does not exist.
func (c *CRProjectMembers) ProjectMembers(ctx context.Context, project string) (map[string]string, bool, error) {
	var p kipperv1.Project
	if err := c.Client.Get(ctx, crclient.ObjectKey{Name: project}, &p); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	members := make(map[string]string, len(p.Spec.Members))
	for _, m := range p.Spec.Members {
		members[m.Email] = string(m.Role)
	}
	return members, true, nil
}
