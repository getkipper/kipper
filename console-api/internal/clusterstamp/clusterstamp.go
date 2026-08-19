// Package clusterstamp records which console-api build is serving a cluster.
package clusterstamp

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/getkipper/kipper/controller/pkg/labels"
)

// Namespace is where the stamp lives, which is the namespace console-api runs in.
const Namespace = "kipper-system"

// Record stamps the running build on the kipper-system namespace.
//
// kip reads it to tell whether the console-api serving a cluster is one that
// keeps a shared credential's allow-list, which it cannot tell from the
// deployment: the image is a moving tag, so a completed rollout says a new pod
// is running and not which code it holds.
func Record(ctx context.Context, client kubernetes.Interface, build string) error {
	namespaces := client.CoreV1().Namespaces()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		ns, err := namespaces.Get(ctx, Namespace, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("reading %s: %w", Namespace, err)
		}
		if ns.Annotations[labels.AnnoConsoleAPIBuild] == build {
			return nil
		}
		if ns.Annotations == nil {
			ns.Annotations = map[string]string{}
		}
		ns.Annotations[labels.AnnoConsoleAPIBuild] = build
		_, err = namespaces.Update(ctx, ns, metav1.UpdateOptions{})
		return err
	})
}
