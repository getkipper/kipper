// Package capacity shares the rules for an app's replica bounds and
// autoscaling policy between the CLI and console.
package capacity

import (
	"errors"
	"fmt"
)

// Policy preserves omitted fields so validation can distinguish them from
// explicit zeros in requests and manifests.
type Policy struct {
	Enabled      bool
	MinReplicas  *int32
	MaxReplicas  *int32
	CPUTarget    *int32
	MemoryTarget *int32
}

// Bounds returns a range when maxReplicas is positive, defaulting the minimum
// to one. Call Validate to check the minimum and ordering of the bounds.
func (p *Policy) Bounds() (lo, hi int32, ok bool) {
	if p == nil || p.MaxReplicas == nil || *p.MaxReplicas < 1 {
		return 0, 0, false
	}
	lo = 1
	if p.MinReplicas != nil {
		lo = *p.MinReplicas
	}
	return lo, *p.MaxReplicas, true
}

// Usable reports whether the policy is on and valid, so an autoscaler can be
// built from it.
func (p *Policy) Usable() bool {
	return p != nil && p.Enabled && Validate(p, nil) == nil
}

// Validate checks a policy and, when given, the replica count it bounds.
// A nil policy has no rules. A nil replica count is left for the caller to
// resolve.
func Validate(p *Policy, replicas *int32) error {
	if p == nil {
		return nil
	}
	var errs []error
	if p.MinReplicas != nil && *p.MinReplicas < 1 {
		errs = append(errs, fmt.Errorf("minReplicas must be at least 1"))
	}
	if p.MaxReplicas != nil && *p.MaxReplicas < 1 {
		errs = append(errs, fmt.Errorf("maxReplicas must be at least 1"))
	}
	if p.Enabled && p.MaxReplicas == nil {
		errs = append(errs, fmt.Errorf("maxReplicas is required when autoscaling is enabled"))
	}
	// A minimum of 1 is the CRD default, which the API server stores in every
	// block that leaves minReplicas out.
	if !p.Enabled && p.MaxReplicas == nil && p.MinReplicas != nil && *p.MinReplicas > 1 {
		errs = append(errs, fmt.Errorf("set maxReplicas with minReplicas, or leave both out to remove the bounds"))
	}
	if p.MinReplicas != nil && p.MaxReplicas != nil && *p.MaxReplicas >= 1 && *p.MinReplicas > *p.MaxReplicas {
		errs = append(errs, fmt.Errorf("minReplicas (%d) must not exceed maxReplicas (%d)", *p.MinReplicas, *p.MaxReplicas))
	}
	// Disabled policies still constrain replicas but leave stored targets unchecked.
	if p.Enabled {
		if p.CPUTarget != nil && *p.CPUTarget < 0 {
			errs = append(errs, fmt.Errorf("cpuTarget must not be negative"))
		}
		if p.MemoryTarget != nil && *p.MemoryTarget < 0 {
			errs = append(errs, fmt.Errorf("memoryTarget must not be negative"))
		}
		if !positive(p.CPUTarget) && !positive(p.MemoryTarget) {
			errs = append(errs, fmt.Errorf("set cpuTarget or memoryTarget when autoscaling is enabled"))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if lo, hi, ok := p.Bounds(); ok && replicas != nil && (*replicas < lo || *replicas > hi) {
		return fmt.Errorf("replicas (%d) must be between %d and %d, the minimum and maximum", *replicas, lo, hi)
	}
	return nil
}

func positive(v *int32) bool { return v != nil && *v > 0 }

// IntoBounds moves n into the policy's bounds and reports whether it moved.
// A policy without bounds keeps n. Call Validate first, since the bounds are
// taken as they are.
func (p *Policy) IntoBounds(n int32) (int32, bool) {
	lo, hi, ok := p.Bounds()
	if !ok {
		return n, false
	}
	moved := min(max(n, lo), hi)
	return moved, moved != n
}

// SwitchOff describes the replica count retained when autoscaling is disabled.
type SwitchOff struct {
	// AlreadyOff means the policy was already disabled or absent.
	AlreadyOff bool
	// Replicas is the resulting App spec.replicas value.
	Replicas int32
	Stopped  bool
	// CountUnknown marks a failed Deployment read; the stored App count is retained.
	CountUnknown bool
	// Live is the Deployment’s desired count before clamping, when read successfully.
	Live int32
	// InvalidBounds means the Deployment’s desired count was kept without clamping.
	InvalidBounds bool
}

// WritesReplicas reports whether Replicas replaces the stored App count.
func (s SwitchOff) WritesReplicas() bool {
	return !s.AlreadyOff && !s.Stopped && !s.CountUnknown
}

// Clamped reports whether the Deployment's desired count was moved into bounds.
func (s SwitchOff) Clamped() bool {
	return s.WritesReplicas() && s.Live != s.Replicas
}

// PlanSwitchOff decides what switching policy p off does to the stored count.
// A running app keeps the Deployment's desired count, clamped into valid
// bounds, so it does not drop back to a stale stored count. A policy that is
// already off, a stopped app and an unreadable count keep the stored count.
//
// readLive returns the Deployment's desired count and is called only for a
// non-stopped app whose policy is on.
func PlanSwitchOff(p *Policy, stored int32, stopped bool, readLive func() (int32, error)) SwitchOff {
	res := SwitchOff{Replicas: stored}
	if p == nil || !p.Enabled {
		res.AlreadyOff = true
		return res
	}
	if stopped {
		res.Stopped = true
		return res
	}
	live, err := readLive()
	if err != nil {
		res.CountUnknown = true
		return res
	}
	res.Live, res.Replicas = live, live
	off := *p
	off.Enabled = false
	// Preserve the desired count when invalid bounds make clamping unsafe.
	if Validate(&off, nil) != nil {
		res.InvalidBounds = true
		return res
	}
	res.Replicas, _ = off.IntoBounds(live)
	return res
}
