package copyenv

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func copiesSource(maxCopies *int32) (*Copier, crclient.Client) {
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "demo-test"},
		Spec:       kipperv1.ServiceSpec{Type: "postgres", Copies: int32Ptr(2)},
	}
	single := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "cache", Namespace: "demo-test"},
		Spec:       kipperv1.ServiceSpec{Type: "redis", Copies: int32Ptr(1)},
	}
	pc := &kipperv1.PlatformConfig{ObjectMeta: metav1.ObjectMeta{Name: "platform"}, Spec: kipperv1.PlatformConfigSpec{MaxCopies: maxCopies}}
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc, single, pc).Build()
	return &Copier{CRClient: c, Client: fake.NewClientset()}, c
}

func TestCopierCarriesTheSourcesCopies(t *testing.T) {
	copier, c := copiesSource(int32Ptr(2))
	_, err := copier.Run(context.Background(), Options{Source: "demo-test", Target: "demo-prod"})
	require.NoError(t, err)
	var copied kipperv1.Service
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Namespace: "demo-prod", Name: "db"}, &copied))
	assert.Equal(t, int32Ptr(2), copied.Spec.Copies)
}

func TestCopierRefusesTwoCopiesTheClusterDoesNotOffer(t *testing.T) {
	copier, c := copiesSource(nil)
	_, err := copier.Run(context.Background(), Options{Source: "demo-test", Target: "demo-prod"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "db")
	assert.Contains(t, err.Error(), "kip cluster storage --max-copies 2")

	var list kipperv1.ServiceList
	require.NoError(t, c.List(context.Background(), &list, crclient.InNamespace("demo-prod")))
	assert.Empty(t, list.Items, "nothing is copied before the refusal")
}
