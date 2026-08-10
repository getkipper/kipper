package ai

import "fmt"

// Tier classifies the AI workload a cluster can host. Higher tiers can run
// larger models with better quality and lower latency.
type Tier int

const (
	// TierNone means the cluster cannot run a useful local LLM. Below tier 1.
	TierNone Tier = iota
	// TierOne is CPU-only with at least 8 GiB free memory. Runs a 3B Q4 model.
	TierOne
	// TierTwo is CPU-only with at least 16 GiB free memory. Runs a 7B Q4 model.
	TierTwo
	// TierThree is GPU-enabled with at least 16 GiB free memory and a GPU
	// with under 16 GiB VRAM (or unknown VRAM). Runs a 7B-14B model with low
	// latency.
	TierThree
	// TierFour is GPU-enabled with 16+ GiB VRAM. Runs a 14B-32B model.
	TierFour
)

// Memory thresholds in bytes. Powers of two (GiB) match Kubernetes
// allocatable reporting more closely than decimal GB.
const (
	tierOneMemory   int64 = 8 * 1024 * 1024 * 1024
	tierTwoMemory   int64 = 16 * 1024 * 1024 * 1024
	tierFourGPUVRAM int64 = 16 * 1024 * 1024 * 1024
)

// String returns the tier name used in CLI output and error messages.
func (t Tier) String() string {
	switch t {
	case TierNone:
		return "none"
	case TierOne:
		return "tier 1 (CPU, 8 GiB)"
	case TierTwo:
		return "tier 2 (CPU, 16 GiB)"
	case TierThree:
		return "tier 3 (GPU)"
	case TierFour:
		return "tier 4 (GPU, 16+ GiB VRAM)"
	default:
		return fmt.Sprintf("unknown tier (%d)", int(t))
	}
}

// DefaultModel returns the Ollama model tag the CLI should pull for this
// tier when the user does not override --model.
func (t Tier) DefaultModel() string {
	switch t {
	case TierOne:
		return "qwen2.5:3b-instruct-q4_K_M"
	case TierTwo, TierThree:
		return "qwen2.5:7b-instruct-q4_K_M"
	case TierFour:
		return "qwen2.5:14b-instruct-q4_K_M"
	default:
		return ""
	}
}

// MeetsMinimum reports whether the cluster can host any local model.
func (t Tier) MeetsMinimum() bool {
	return t >= TierOne
}
