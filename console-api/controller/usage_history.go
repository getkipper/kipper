package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
)

const (
	pressureWarnPct = 80
	pressureCritPct = 90
	// pressureClearPct sits below the warning threshold so usage hovering right
	// at 80% doesn't repeatedly clear the cooldown and re-alert on every
	// re-crossing. Pressure must fall below this band to count as recovered.
	pressureClearPct     = 75
	nodePressureCooldown = 30 * time.Minute
)

func (rc *ResourceController) checkNodePressure(_ context.Context, podMetrics map[string][]podMetricsEntry, nodes []corev1.Node) []ResourceLogEntry {
	// Sum actual usage across all pods
	var totalMem int64
	for _, pods := range podMetrics {
		for _, pe := range pods {
			totalMem += pe.MemoryBytes
		}
	}

	if len(nodes) == 0 {
		return nil
	}

	var allocatableMem int64
	for _, n := range nodes {
		if mem, ok := n.Status.Allocatable[corev1.ResourceMemory]; ok {
			allocatableMem += mem.Value()
		}
	}

	if allocatableMem == 0 {
		return nil
	}

	usagePct := float64(totalMem) / float64(allocatableMem) * 100

	if usagePct < float64(pressureClearPct) {
		// Pressure recovered past the hysteresis band: clear the cooldown so a
		// later recurrence alerts again instead of being suppressed as if it
		// were continuous.
		rc.mu.Lock()
		rc.nodePressureAt = time.Time{}
		rc.nodePressureSev = ""
		rc.mu.Unlock()
		return nil
	}

	if usagePct < float64(pressureWarnPct) {
		// In the 75-80% band: elevated but below the alert threshold. Don't
		// alert, and don't reset the cooldown, so hovering at the threshold
		// can't produce a fresh alert on every re-crossing.
		return nil
	}

	severity := "warning"
	if usagePct >= float64(pressureCritPct) {
		severity = "critical"
	}

	// Throttle node-pressure alerts: sustained pressure would otherwise fire an
	// alert every tick. Suppress within the cooldown, with one exception — a
	// genuine escalation from warning to critical alerts straight away. A
	// de-escalation (critical → warning) stays suppressed so oscillation across
	// the 90% line can't storm.
	rc.mu.Lock()
	escalated := severity == "critical" && rc.nodePressureSev == "warning"
	suppress := time.Since(rc.nodePressureAt) < nodePressureCooldown && !escalated
	if !suppress {
		rc.nodePressureAt = time.Now()
		rc.nodePressureSev = severity
	}
	rc.mu.Unlock()
	if suppress {
		return nil
	}

	reason := fmt.Sprintf("node memory at %.0f%%%s", usagePct, topConsumers(podMetrics))

	now := time.Now().UTC().Format(time.RFC3339)
	// The tick appends this entry to the resource log and creates exactly one
	// alert from it via createAlerts, carrying the severity computed above.
	entry := ResourceLogEntry{
		Time:      now,
		App:       "cluster",
		Namespace: "system",
		Action:    "node memory pressure",
		From:      fmt.Sprintf("%.0f%%", usagePct),
		To:        fmt.Sprintf("%.0f%%", usagePct),
		Reason:    reason,
		Severity:  severity,
	}

	return []ResourceLogEntry{entry}
}

// topConsumers returns a ", top consumers: ..." suffix naming the three
// workloads using the most memory right now, or an empty string when there are
// none. Empty means the reason is just the node percentage.
func topConsumers(podMetrics map[string][]podMetricsEntry) string {
	type workloadUsage struct {
		name      string
		namespace string
		memBytes  int64
	}

	var users []workloadUsage
	for key, pods := range podMetrics {
		parts := splitKey(key)
		if parts == nil {
			continue
		}
		var memSum int64
		for _, pe := range pods {
			memSum += pe.MemoryBytes
		}
		users = append(users, workloadUsage{name: parts[1], namespace: parts[0], memBytes: memSum})
	}
	if len(users) == 0 {
		return ""
	}

	sort.Slice(users, func(i, j int) bool { return users[i].memBytes > users[j].memBytes })

	limit := 3
	if len(users) < limit {
		limit = len(users)
	}
	out := ". Top consumers: "
	for i := 0; i < limit; i++ {
		if i > 0 {
			out += ", "
		}
		u := users[i]
		out += fmt.Sprintf("%s/%s %dMi", u.namespace, u.name, u.memBytes/(1024*1024))
	}
	return out
}

func splitKey(key string) []string {
	idx := -1
	for i, c := range key {
		if c == '/' {
			idx = i
			break
		}
	}
	if idx < 0 || idx == len(key)-1 {
		return nil
	}
	return []string{key[:idx], key[idx+1:]}
}
