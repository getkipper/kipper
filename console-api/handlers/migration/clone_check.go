package migration

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/getkipper/kipper/console-api/builder"
)

// cloneCheckConcurrency limits simultaneous repository checks so slow hosts
// share the plan's one-minute budget. Each repository request has a five-second
// timeout.
const cloneCheckConcurrency = 8

// cloneOutcome holds a blocker or the name of an app whose check was canceled.
type cloneOutcome struct {
	blocker, unchecked string
}

// checkClones adds blockers for invalid git sources, credential resolution
// errors, and repository refusals, preserving inventory order. The caller
// excludes apps using shared credentials because their builds use the target's
// token. Canceled or timed-out checks produce a warning in inventory order,
// leaving the target's build to establish access, as with unreachable hosts.
func (h *Handler) checkClones(ctx context.Context, resp *planResponse) {
	outcomes := make([]cloneOutcome, len(resp.cloneChecks))
	sem := make(chan struct{}, cloneCheckConcurrency)
	var wg sync.WaitGroup
	for i, app := range resp.cloneChecks {
		name := app.Namespace + "/" + app.Name
		if ctx.Err() != nil {
			outcomes[i].unchecked = name
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			outcomes[i].unchecked = name
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			refusal, err := builder.CloneRefusal(ctx, h.Client, h.CRClient, app)
			switch {
			case ctx.Err() != nil:
				outcomes[i].unchecked = name
			case err != nil:
				outcomes[i].blocker = fmt.Sprintf("Could not check whether app %s can be rebuilt on the target: %v", name, err)
			case refusal != "":
				outcomes[i].blocker = fmt.Sprintf("App %s cannot be rebuilt on the target: %s. Fix the app's git source or token, then plan again.", name, refusal)
			}
		}()
	}
	wg.Wait()
	var unchecked []string
	for _, o := range outcomes {
		if o.blocker != "" {
			resp.Blockers = append(resp.Blockers, o.blocker)
		}
		if o.unchecked != "" {
			unchecked = append(unchecked, o.unchecked)
		}
	}
	if len(unchecked) > 0 {
		warning := fmt.Sprintf(
			"Planning ended before repository access could be checked for %s using each app's token, if configured. Each app's first build on the target will check access.",
			strings.Join(unchecked, ", "))
		resp.Warnings = append(resp.Warnings, warning)
		if resp.unboundWarnings == nil {
			resp.unboundWarnings = map[string]bool{}
		}
		resp.unboundWarnings[warning] = true
	}
}
