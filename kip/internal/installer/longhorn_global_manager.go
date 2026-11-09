package installer

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	globalManagerLeaseQuery = `kubectl -n longhorn-system get leases.coordination.k8s.io longhorn-global-manager --request-timeout=30s -o json`
	globalManagerPodQuery   = `kubectl -n longhorn-system get pods -l app=longhorn-global-manager --request-timeout=30s -o json`
)

// waitForGlobalLeader waits until a running, ready global manager pod holds
// Longhorn's global manager Lease and has renewed it since an earlier read.
// The global manager reports healthy before election and starts its controllers
// only as leader, so readiness alone cannot establish leadership. Renewal is
// detected by a change in the Lease's renewTime between reads, independently
// of kip's clock. Missing, unreadable or unchanged leases are retried every
// Interval until StepTimeout expires.
func (u LonghornUpgrade) waitForGlobalLeader(client CommandRunner) error {
	deadline := u.now().Add(u.StepTimeout)
	seen, last := "", ""
	for {
		renewed, problem := globalLeaderProblem(client, seen)
		if problem == "" {
			return nil
		}
		if renewed != "" {
			seen = renewed
		}
		if problem != last {
			say(u.Out, "       waiting for Longhorn's global manager: %s\n", problem)
			last = problem
		}
		if !u.now().Before(deadline) {
			return fmt.Errorf("%s did not settle within %s: %s. Run %s again to keep waiting", "Longhorn's global manager", u.StepTimeout, problem, LonghornUpgradeCommand)
		}
		time.Sleep(u.Interval)
	}
}

// globalLeaderProblem checks the Lease and, if it has a holder and renewal
// time, the global manager's pods. It returns the Lease's renewTime when
// available and a problem describing why the leader is not active. An empty
// problem means the holder is running, ready and not being deleted, and
// renewTime differs from a nonempty seen value.
func globalLeaderProblem(client CommandRunner, seen string) (renewed, problem string) {
	out, err := client.Run(globalManagerLeaseQuery)
	if err != nil {
		return "", fmt.Sprintf("reading the global manager's lease: %v", err)
	}
	var lease struct {
		Spec struct {
			HolderIdentity string `json:"holderIdentity"`
			RenewTime      string `json:"renewTime"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(jsonPayload(out), &lease); err != nil {
		return "", fmt.Sprintf("reading the global manager's lease: %v", err)
	}
	holder, renew := lease.Spec.HolderIdentity, lease.Spec.RenewTime
	if holder == "" || renew == "" {
		return renew, "no global manager holds its lease"
	}
	running, err := runningGlobalManagers(client)
	if err != nil {
		return renew, fmt.Sprintf("reading the global manager's pods: %v", err)
	}
	if !running[holder] {
		return renew, fmt.Sprintf("its lease is held by %s, which is not a running global manager pod", holder)
	}
	if seen == "" || renew == seen {
		return renew, fmt.Sprintf("its leader %s has not renewed its lease since kip last read it", holder)
	}
	return renew, ""
}

// runningGlobalManagers returns the names of the global manager's pods that
// run, are ready and are not being deleted.
func runningGlobalManagers(client CommandRunner) (map[string]bool, error) {
	out, err := client.Run(globalManagerPodQuery)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name              string `json:"name"`
				DeletionTimestamp string `json:"deletionTimestamp"`
			} `json:"metadata"`
			Status struct {
				Phase      string `json:"phase"`
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(jsonPayload(out), &list); err != nil {
		return nil, err
	}
	running := map[string]bool{}
	for _, p := range list.Items {
		if p.Status.Phase != "Running" || p.Metadata.DeletionTimestamp != "" {
			continue
		}
		for _, c := range p.Status.Conditions {
			if c.Type == "Ready" && strings.EqualFold(c.Status, "True") {
				running[p.Metadata.Name] = true
			}
		}
	}
	return running, nil
}
