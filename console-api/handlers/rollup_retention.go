package handlers

import (
	"context"
	"log"
	"time"

	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// rollupRetentionDays keeps three months of daily usage history — enough to
// cover any month-period quota plus a billing dispute window — while
// bounding the CR count at keys x 92.
const rollupRetentionDays = 92

// authzCanaryName/Namespace identify the authz freshness canary UsageRollup.
// Its spec deliberately carries an ancient day so it never collides with real
// usage, which also makes it permanently older than any retention cutoff. The
// sweep must skip it: authz cannot recreate canaries and a deleted one stalls
// every replica's freshness clock, failing all key-gated routes closed.
const (
	authzCanaryName      = "authz-canary"
	authzCanaryNamespace = "kipper-system"
)

// sweepExpiredRollups deletes UsageRollups older than the retention window,
// leaving the authz freshness canary untouched.
func sweepExpiredRollups(ctx context.Context, c crclient.Client) {
	sweepCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	var rollups kipperv1.UsageRollupList
	if err := c.List(sweepCtx, &rollups); err != nil {
		log.Printf("rollup retention: listing rollups: %v", err)
		return
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -rollupRetentionDays).Format("2006-01-02")
	deleted := 0
	for i := range rollups.Items {
		if rollups.Items[i].Spec.Day >= cutoff {
			continue
		}
		if rollups.Items[i].Namespace == authzCanaryNamespace && rollups.Items[i].Name == authzCanaryName {
			continue
		}
		if err := c.Delete(sweepCtx, &rollups.Items[i]); err != nil {
			log.Printf("rollup retention: deleting %s/%s: %v",
				rollups.Items[i].Namespace, rollups.Items[i].Name, err)
			continue
		}
		deleted++
	}
	if deleted > 0 {
		log.Printf("rollup retention: deleted %d rollups older than %s", deleted, cutoff)
	}
}

// RunRollupRetention deletes UsageRollups older than the retention window,
// once at startup and then daily.
func RunRollupRetention(ctx context.Context, c crclient.Client) {
	sweepExpiredRollups(ctx, c)
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweepExpiredRollups(ctx, c)
		}
	}
}
