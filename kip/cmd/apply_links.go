package cmd

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/getkipper/kipper/kip/internal/deployer"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

// refuseUnreadableLinkTargets requires read access to new links outside this
// namespace. Existing links are trusted; local targets may be created by this apply.
func refuseUnreadableLinkTargets(ctx context.Context, dyn dynamic.Interface, namespace string, resources []manifest.Resource) error {
	for _, res := range resources {
		if res.GVR != deployer.AppGVR {
			continue
		}
		wanted, _, _ := unstructured.NestedSlice(res.Object.Object, "spec", "links")
		if len(wanted) == 0 {
			continue
		}
		live := map[string]bool{}
		existing, err := dyn.Resource(deployer.AppGVR).Namespace(namespace).Get(ctx, res.Object.GetName(), metav1.GetOptions{})
		if err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("reading app %q: %w", res.Object.GetName(), err)
		}
		if err == nil {
			links, _, _ := unstructured.NestedSlice(existing.Object, "spec", "links")
			for _, l := range links {
				if lm, ok := l.(map[string]interface{}); ok {
					live[fmt.Sprint(lm["namespace"], "/", lm["app"])] = true
				}
			}
		}
		for _, l := range wanted {
			lm, ok := l.(map[string]interface{})
			if !ok {
				continue
			}
			app, _ := lm["app"].(string)
			ns, _ := lm["namespace"].(string)
			if ns == namespace || live[ns+"/"+app] {
				continue
			}
			if _, err := dyn.Resource(deployer.AppGVR).Namespace(ns).Get(ctx, app, metav1.GetOptions{}); err != nil {
				return fmt.Errorf("app %q: cannot add link to %s in namespace %s: target app is missing or unreadable with your credentials", res.Object.GetName(), app, ns)
			}
		}
	}
	return nil
}
