package resourcebounds

import (
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/getkipper/kipper/controller/pkg/provenance"
)

// Quantity is one resource value from a workload's spec and who set it. An
// Unset quantity has no value.
type Quantity struct {
	Value  resource.Quantity
	Source Source
}

// Pair is a container's request and limit for one resource.
type Pair struct {
	Request resource.Quantity
	Limit   resource.Quantity
}

// AutoRange is the floor and ceiling Kipper keeps automatic values inside. A
// zero Ceiling means no ceiling.
type AutoRange struct {
	Floor   resource.Quantity
	Ceiling resource.Quantity
}

// Mode describes how [Resolve] sizes a resource; see [provenance.Mode].
type Mode = provenance.Mode

const (
	ModeAutomatic = provenance.ModeAutomatic
	ModeBounded   = provenance.ModeBounded
	ModeFixed     = provenance.ModeFixed
	ModeHeld      = provenance.ModeHeld
)

// Resolve returns the container's request and limit for CPU or memory.
// request and limit carry the spec values and their sources. recommended is
// optional; fallback supplies live values or profile defaults.
//
// For user bounds, only the request moves. One user value, or an equal pair,
// sets a fixed size. Held values are preserved. Automatic values are clamped
// to auto.
func Resolve(request, limit Quantity, recommended *Pair, fallback Pair, auto AutoRange) (Pair, Mode) {
	mode := ModeOf(request, limit)
	switch mode {
	case ModeHeld:
		return mirrored(request, limit), mode
	case ModeFixed:
		if limit.Source == User {
			return Pair{Request: limit.Value, Limit: limit.Value}, mode
		}
		return Pair{Request: request.Value, Limit: request.Value}, mode
	case ModeBounded:
		wanted := fallback.Request
		if recommended != nil {
			wanted = recommended.Request
		}
		return Pair{Request: clamp(wanted, request.Value, limit.Value), Limit: limit.Value}, mode
	}

	p := fallback
	if recommended != nil {
		p = *recommended
	}
	p.Request = withinAuto(p.Request, auto)
	p.Limit = withinAuto(p.Limit, auto)
	if p.Limit.Cmp(p.Request) < 0 {
		p.Limit = p.Request
	}
	return p, ModeAutomatic
}

// ModeOf says how a resource with these spec quantities is sized.
func ModeOf(request, limit Quantity) Mode {
	return provenance.ModeOf(request.Source, limit.Source, request.Value.Cmp(limit.Value) < 0)
}

// mirrored returns the spec's own values, copying a lone value to the other
// side as ResolveResourcePair does for one-sided specs.
func mirrored(request, limit Quantity) Pair {
	switch {
	case request.Source == Unset:
		return Pair{Request: limit.Value, Limit: limit.Value}
	case limit.Source == Unset:
		return Pair{Request: request.Value, Limit: request.Value}
	default:
		return Pair{Request: request.Value, Limit: limit.Value}
	}
}

func clamp(v, lo, hi resource.Quantity) resource.Quantity {
	if v.Cmp(lo) < 0 {
		return lo
	}
	if v.Cmp(hi) > 0 {
		return hi
	}
	return v
}

func withinAuto(v resource.Quantity, auto AutoRange) resource.Quantity {
	if v.Cmp(auto.Floor) < 0 {
		v = auto.Floor
	}
	if !auto.Ceiling.IsZero() && v.Cmp(auto.Ceiling) > 0 {
		v = auto.Ceiling
	}
	return v
}
