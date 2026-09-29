package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/getkipper/kipper/kip/internal/deployer"
)

// resourceFlags are the kip app update flags that change CPU and memory.
type resourceFlags struct {
	memory, memoryRequest, memoryLimit string
	cpu, cpuRequest, cpuLimit          string
	tuning                             string
}

func readResourceFlags(cmd *cobra.Command) resourceFlags {
	get := func(name string) string { v, _ := cmd.Flags().GetString(name); return v }
	return resourceFlags{
		memory: get("memory"), memoryRequest: get("memory-request"), memoryLimit: get("memory-limit"),
		cpu: get("cpu"), cpuRequest: get("cpu-request"), cpuLimit: get("cpu-limit"),
		tuning: get("tuning"),
	}
}

// edits turns the flags into a resource change, nil when none was asked for.
// --memory and --cpu set a fixed size; the request and limit flags set the
// bounds the auto-sizer tunes inside; --tuning auto hands both resources back.
func (f resourceFlags) edits() (*deployer.ResourceEdits, error) {
	if f.tuning != "" {
		if f.tuning != "auto" {
			return nil, fmt.Errorf("--tuning takes only \"auto\", which hands CPU and memory back to automatic sizing")
		}
		if f.memory+f.memoryRequest+f.memoryLimit+f.cpu+f.cpuRequest+f.cpuLimit != "" {
			return nil, fmt.Errorf("--tuning auto clears CPU and memory, so it cannot be combined with values for them")
		}
		return &deployer.ResourceEdits{CPU: &deployer.PairEdit{Clear: true}, Memory: &deployer.PairEdit{Clear: true}}, nil
	}
	memory, err := pairFromFlags("memory", f.memory, f.memoryRequest, f.memoryLimit)
	if err != nil {
		return nil, err
	}
	cpu, err := pairFromFlags("cpu", f.cpu, f.cpuRequest, f.cpuLimit)
	if err != nil {
		return nil, err
	}
	if memory == nil && cpu == nil {
		return nil, nil
	}
	return &deployer.ResourceEdits{CPU: cpu, Memory: memory}, nil
}

// pairFromFlags reads one resource's flags. A lone request or limit is a
// fixed size, like --memory and --cpu.
func pairFromFlags(name, fixed, request, limit string) (*deployer.PairEdit, error) {
	if fixed != "" && (request != "" || limit != "") {
		return nil, fmt.Errorf("--%s sets a fixed size; use it or --%s-request/--%s-limit, not both", name, name, name)
	}
	if fixed != "" {
		request, limit = fixed, fixed
	}
	if request == "" && limit == "" {
		return nil, nil
	}
	if request == "" {
		request = limit
	}
	if limit == "" {
		limit = request
	}
	req, err := parseResourceQuantity("--"+name+"-request", request)
	if err != nil {
		return nil, err
	}
	lim, err := parseResourceQuantity("--"+name+"-limit", limit)
	if err != nil {
		return nil, err
	}
	if req.Cmp(lim) > 0 {
		return nil, fmt.Errorf("the %s request %s is above its limit %s; the request is the floor and the limit the ceiling", name, request, limit)
	}
	return &deployer.PairEdit{Request: request, Limit: limit}, nil
}

func parseResourceQuantity(flag, value string) (resource.Quantity, error) {
	q, err := resource.ParseQuantity(value)
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("%s %q is not a valid quantity (e.g. 512Mi, 2Gi, 250m, 1)", flag, value)
	}
	if q.Sign() < 0 {
		return resource.Quantity{}, fmt.Errorf("%s cannot be negative", flag)
	}
	return q, nil
}

func describeResourceEdits(e deployer.ResourceEdits) string {
	if e.Memory != nil && e.Memory.Clear && e.CPU != nil && e.CPU.Clear {
		return "CPU and memory handed back to automatic sizing"
	}
	var parts []string
	describe := func(name string, p *deployer.PairEdit) {
		switch {
		case p == nil:
		case p.Request == p.Limit:
			parts = append(parts, fmt.Sprintf("%s fixed at %s", name, p.Limit))
		default:
			parts = append(parts, fmt.Sprintf("%s between %s and %s", name, p.Request, p.Limit))
		}
	}
	describe("memory", e.Memory)
	describe("CPU", e.CPU)
	return strings.Join(parts, ", ") + ": rollout in progress"
}
