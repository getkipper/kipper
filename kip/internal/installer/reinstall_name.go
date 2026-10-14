package installer

import (
	"fmt"

	"github.com/getkipper/kipper/kip/internal/config"
)

// reinstallNameHint offers a manual rename after a domain change.
// Reinstalls preserve the local name because it may be a chosen alias.
func reinstallNameHint(existing *config.Cluster, domain string) string {
	if existing == nil || existing.Domain == domain || existing.Name == domain {
		return ""
	}
	return fmt.Sprintf("  ℹ  This cluster will keep its local name %s and use domain %s.\n"+
		"     To rename it: kip cluster rename %s %s\n", existing.Name, domain, existing.Name, domain)
}
