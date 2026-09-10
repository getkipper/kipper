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

// enforceResourceVersions makes a fake client behave like an API server in the
// one way that catches a whole class of bug: a create is stamped with a
// resourceVersion, and an update carrying none is refused.
//
// Without it, a handler that builds a fresh object and calls Update passes
// every test here and then fails on its second save against a real cluster,
// because the API server rejects an update with no resourceVersion. Two
// handlers in this package shipped that way.
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
