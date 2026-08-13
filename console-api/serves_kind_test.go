package main

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// stubDiscovery answers one group version, the way the API server does for a
// cluster that has some of Kipper's CRDs applied and not others.
type stubDiscovery struct {
	resources *metav1.APIResourceList
	err       error
}

func (s stubDiscovery) ServerResourcesForGroupVersion(string) (*metav1.APIResourceList, error) {
	return s.resources, s.err
}

func (s stubDiscovery) ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	return nil, nil, s.err
}

func (s stubDiscovery) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	return nil, s.err
}

func (s stubDiscovery) ServerPreferredNamespacedResources() ([]*metav1.APIResourceList, error) {
	return nil, s.err
}

// A cluster upgrades by pulling this image before its CRDs are applied, because
// the Deployment tracks :latest and `kip upgrade` applies the CRDs separately.
// Registering a watch for a kind the API server does not serve leaves that
// informer unable to sync, and the manager then gives up and stops every other
// reconciler with it, while the pod stays Running and serving HTTP. So the
// absent case has to be answered before the controller is registered.
func TestServesKind(t *testing.T) {
	workloadName := &metav1.APIResourceList{APIResources: []metav1.APIResource{
		{Kind: "App"}, {Kind: "WorkloadName"},
	}}
	withoutIt := &metav1.APIResourceList{APIResources: []metav1.APIResource{{Kind: "App"}}}

	for name, tc := range map[string]struct {
		dc   stubDiscovery
		want bool
	}{
		"the cluster serves the kind":        {stubDiscovery{resources: workloadName}, true},
		"the CRD has not been applied yet":   {stubDiscovery{resources: withoutIt}, false},
		"the group version is not served":    {stubDiscovery{err: errors.New("the server could not find the requested resource")}, false},
		"discovery could not be reached":     {stubDiscovery{err: errors.New("etcd leader changed")}, false},
		"an empty group version is returned": {stubDiscovery{resources: &metav1.APIResourceList{}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, servesKind(tc.dc, kipperv1.GroupVersion, "WorkloadName"))
		})
	}
}

// A discovery client that could not be built must not panic the process it was
// meant to protect.
func TestServesKind_NoDiscoveryClientReportsAbsent(t *testing.T) {
	assert.False(t, servesKind(nil, schema.GroupVersion{Group: "kipper.run", Version: "v1alpha1"}, "WorkloadName"))
}
