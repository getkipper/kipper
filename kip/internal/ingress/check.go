package ingress

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// CheckHostAvailable verifies that a hostname is not already claimed by
// another Ingress in the cluster. Returns an error if the host is taken.
// The ownerName parameter allows the check to pass if the existing Ingress
// belongs to the same resource (for updates).
func CheckHostAvailable(ctx context.Context, client kubernetes.Interface, host, ownerName string) error {
	ingresses, err := client.NetworkingV1().Ingresses("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing ingresses: %w", err)
	}

	for _, ing := range ingresses.Items {
		// Skip if this is the same resource being updated
		if ing.Name == ownerName || ing.Name == "fn-"+ownerName {
			continue
		}

		for _, rule := range ing.Spec.Rules {
			if rule.Host == host {
				owner := ing.Labels["app"]
				if owner == "" {
					owner = ing.Name
				}
				return fmt.Errorf("domain %q is already in use by %q in namespace %q", host, owner, ing.Namespace)
			}
		}
	}

	return nil
}
