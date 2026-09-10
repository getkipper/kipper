package handlers

import (
	"strconv"
	"sync/atomic"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// enforceResourceVersions makes a fake client behave like an API server: a
// create is stamped with a resourceVersion, and an update carrying none is
// refused. Use it for any handler that writes a ConfigMap or Secret.
func enforceResourceVersions(client *fake.Clientset, resource string) {
	var version int64

	client.PrependReactor("create", resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
		obj, err := meta.Accessor(action.(k8stesting.CreateAction).GetObject())
		if err != nil {
			return true, nil, err
		}
		if obj.GetResourceVersion() == "" {
			obj.SetResourceVersion(strconv.FormatInt(atomic.AddInt64(&version, 1), 10))
		}
		return false, nil, nil
	})

	client.PrependReactor("update", resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
		obj, err := meta.Accessor(action.(k8stesting.UpdateAction).GetObject())
		if err != nil {
			return true, nil, err
		}
		if obj.GetResourceVersion() == "" {
			return true, nil, apierrors.NewInvalid(
				schema.GroupKind{Kind: resource}, obj.GetName(), nil)
		}
		obj.SetResourceVersion(strconv.FormatInt(atomic.AddInt64(&version, 1), 10))
		return false, nil, nil
	})
}
