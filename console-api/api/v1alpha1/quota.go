package v1alpha1

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Project tiers, ordered by capacity. A tier is a capacity label only; it
// carries no pricing meaning.
const (
	TierSmall  = "small"
	TierMedium = "medium"
	TierLarge  = "large"
)

// Names of the quota objects the project reconciler manages in every
// environment namespace. Deterministic names stand in for owner references,
// which a cluster-scoped Project cannot reliably hold on namespaced objects.
const (
	ProjectQuotaName      = "kipper-quota"
	ProjectLimitRangeName = "kipper-limits"
)

// TierQuota returns the default per-namespace quota for a project tier.
// Every environment namespace of a tiered project gets its own quota of this
// size unless it carries an explicit override. Unknown tiers fall back to
// small. Callers must not use this for tierless projects (empty tier):
// tierless environments get no quota objects at all.
//
// Each tier must fit its heaviest supported workload's full lifecycle, not
// just its steady state: a rolling update runs old and new pods at once, and
// a Git build adds a build pod on top. Small is sized so one jvm-profile app
// (2Gi guaranteed memory) can roll out with its sidecar while a build runs.
func TierQuota(tier string) EnvQuota {
	switch tier {
	case TierMedium:
		return EnvQuota{CPURequest: "4", CPULimit: "12", MemoryRequest: "12Gi", MemoryLimit: "24Gi"}
	case TierLarge:
		return EnvQuota{CPURequest: "8", CPULimit: "24", MemoryRequest: "24Gi", MemoryLimit: "48Gi"}
	default:
		return EnvQuota{CPURequest: "2", CPULimit: "6", MemoryRequest: "6Gi", MemoryLimit: "12Gi"}
	}
}

// Default environment-count caps per tier. A tiered project's total
// owner-reachable capacity grant is TierQuota(tier) × its effective
// environment limit, so capping the count is what makes the admin-only tier
// control meaningful: without it an owner could add environments to multiply
// the grant past the tier. small=4 covers the test/acc/prod triad plus one
// spare without a day-one admin request.
//
// A tierless project has no per-environment quota, so its cap is a topology
// brake rather than a capacity grant: the only synchronous limit on how many
// uncapped namespaces one project can claim. 6 covers dev/test/acc/prod plus
// a hotfix and a preview lane; the seventh environment is the point where a
// human should confirm the hardware exists (raise MaxEnvironments, or assign
// a tier for managed capacity envelopes).
const (
	envLimitSmall    = 4
	envLimitMedium   = 6
	envLimitLarge    = 10
	envLimitTierless = 6
)

// TierEnvLimit returns the default maximum number of environments a tier
// allows, with the empty string meaning tierless. Keep these values in sync
// with the CEL rule on ProjectSpec (a drift test asserts it).
func TierEnvLimit(tier string) int {
	switch tier {
	case TierMedium:
		return envLimitMedium
	case TierLarge:
		return envLimitLarge
	case "":
		return envLimitTierless
	default:
		return envLimitSmall
	}
}

// EffectiveEnvLimit returns the environment cap in force for a project: the
// admin-set MaxEnvironments override when present, otherwise the tier default.
func (p *Project) EffectiveEnvLimit() int {
	if p.Spec.MaxEnvironments != nil && *p.Spec.MaxEnvironments > 0 {
		return *p.Spec.MaxEnvironments
	}
	return TierEnvLimit(p.Spec.Tier)
}

// EnvQuotaValues is a parsed, validated EnvQuota.
type EnvQuotaValues struct {
	CPURequest    resource.Quantity
	CPULimit      resource.Quantity
	MemoryRequest resource.Quantity
	MemoryLimit   resource.Quantity
}

// Parsed validates every quantity in the quota. Override values come from
// user input, so parsing must fail with a field-specific error instead of
// panicking.
func (q EnvQuota) Parsed() (EnvQuotaValues, error) {
	var v EnvQuotaValues
	for _, field := range []struct {
		name string
		raw  string
		dst  *resource.Quantity
	}{
		{"cpuRequest", q.CPURequest, &v.CPURequest},
		{"cpuLimit", q.CPULimit, &v.CPULimit},
		{"memoryRequest", q.MemoryRequest, &v.MemoryRequest},
		{"memoryLimit", q.MemoryLimit, &v.MemoryLimit},
	} {
		parsed, err := resource.ParseQuantity(field.raw)
		if err != nil {
			return EnvQuotaValues{}, fmt.Errorf("invalid quota %s %q: %w", field.name, field.raw, err)
		}
		*field.dst = parsed
	}
	return v, nil
}
