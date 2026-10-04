package v1alpha1

import "github.com/getkipper/kipper/controller/pkg/capacity"

// Policy returns the block as the shared capacity policy, or nil for a nil
// block. Explicit zeros stay set, so validation sees them.
func (a *AppAutoscale) Policy() *capacity.Policy {
	if a == nil {
		return nil
	}
	return &capacity.Policy{
		Enabled:      a.Enabled,
		MinReplicas:  a.MinReplicas,
		MaxReplicas:  a.MaxReplicas,
		CPUTarget:    a.CPUTarget,
		MemoryTarget: a.MemoryTarget,
	}
}
