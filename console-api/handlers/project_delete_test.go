package handlers

import (
	"context"
	goerrors "errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
)

// Deleting a project deletes its namespaces, and the label is not what says
// which those are. Anyone who can write a namespace can point its label at
// another project, and this route then hands them a way to have somebody else's
// namespace destroyed: delete a project of your own and the namespace pointed at
// it goes with it, workloads and secrets and all.
//
// So the same rule the reconciler deletes by applies here: the project needs its
// own record of having held the namespace, which a label cannot manufacture.
func deleteRouter(h *Projects) *chi.Mux {
	r := chi.NewRouter()
	r.Delete("/api/v1/projects/{name}", h.Delete)
	return r
}

func TestDeletingAProjectLeavesANamespaceOnlyPointedAtIt(t *testing.T) {
	victim := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "victim-prod",
		UID:    "the-victims-namespace",
		Labels: map[string]string{kipperlabels.Project: "attacker"},
	}}
	own := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "attacker-test",
		UID:    "the-attackers-namespace",
		Labels: map[string]string{kipperlabels.Project: "attacker"},
	}}
	attacker := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "attacker"},
		Status:     kipperv1.ProjectStatus{Namespaces: []string{"attacker-test"}},
	}
	client := fake.NewClientset(victim, own)
	h := &Projects{Client: client, CRClient: testCRClient(attacker, victim, own), Domain: "example.com"}

	rec := httptest.NewRecorder()
	deleteRouter(h).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/projects/attacker", nil))
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	_, err := client.CoreV1().Namespaces().Get(context.Background(), "victim-prod", metav1.GetOptions{})
	require.NoError(t, err,
		"deleting a project took a namespace with it that the project had only been pointed at, so relabelling somebody else's namespace is a way to have it destroyed")
}

// The project's own namespaces still go, or deleting a project leaves its
// workloads running with nothing left to collect them.
func TestDeletingAProjectStillTakesTheNamespacesItHeld(t *testing.T) {
	own := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "shop-test",
		UID:    "the-namespace",
		Labels: map[string]string{kipperlabels.Project: "shop"},
	}}
	shop := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Status:     kipperv1.ProjectStatus{Namespaces: []string{"shop-test"}},
	}
	client := fake.NewClientset(own)
	h := &Projects{Client: client, CRClient: testCRClient(shop, own), Domain: "example.com"}

	rec := httptest.NewRecorder()
	deleteRouter(h).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/projects/shop", nil))
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	_, err := client.CoreV1().Namespaces().Get(context.Background(), "shop-test", metav1.GetOptions{})
	assert.Error(t, err, "the project's own namespace outlived it")
}

// A claim is the other record, and it is the one a namespace adopted this pass
// has before anything is written to the namespace list.
func TestDeletingAProjectTakesANamespaceItHasOnlyClaimed(t *testing.T) {
	own := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "shop-test",
		UID:    "the-namespace",
		Labels: map[string]string{kipperlabels.Project: "shop"},
	}}
	shop := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Status: kipperv1.ProjectStatus{
			NamespaceClaims: []kipperv1.NamespaceClaim{{Name: "shop-test", UID: types.UID("the-namespace")}},
		},
	}
	client := fake.NewClientset(own)
	h := &Projects{Client: client, CRClient: testCRClient(shop, own), Domain: "example.com"}

	rec := httptest.NewRecorder()
	deleteRouter(h).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/projects/shop", nil))
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	_, err := client.CoreV1().Namespaces().Get(context.Background(), "shop-test", metav1.GetOptions{})
	assert.Error(t, err, "a namespace the project had claimed outlived it")
}

// A namespace that could not be deleted is still there with its workloads, its
// credentials and its member bindings, and answering 204 says it was collected.
// The finalizer does retry the same cleanup, so what is reported here is often
// collected shortly afterwards; what the caller must not be told is that the
// work is done when it is not.
func TestDeletingAProjectSaysWhichNamespacesItCouldNotCollect(t *testing.T) {
	own := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "shop-test",
		UID:    "the-namespace",
		Labels: map[string]string{kipperlabels.Project: "shop"},
	}}
	shop := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Status:     kipperv1.ProjectStatus{Namespaces: []string{"shop-test"}},
	}
	client := fake.NewClientset(own)
	client.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(goerrors.New("the api server is having a moment"))
	})
	h := &Projects{Client: client, CRClient: testCRClient(shop, own), Domain: "example.com"}

	rec := httptest.NewRecorder()
	deleteRouter(h).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/projects/shop", nil))

	require.Equal(t, http.StatusInternalServerError, rec.Code,
		"a namespace nothing will retry was left on the cluster and the caller was told the project had been cleaned up")
	assert.Contains(t, rec.Body.String(), "shop-test", "the answer does not say which namespace is still there")
}

// The preconditions are what bind each delete to the object its authorisation
// was checked against, and nothing else in these tests would notice if they
// were dropped: the fake applies a delete by name whether or not one is set.
// So assert that they are sent.
func TestDeletingAProjectBindsEveryDeleteToTheObjectItChecked(t *testing.T) {
	own := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "shop-test",
		UID:    "the-namespace",
		Labels: map[string]string{kipperlabels.Project: "shop"},
	}}
	shop := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", UID: "the-project"},
		Status:     kipperv1.ProjectStatus{Namespaces: []string{"shop-test"}},
	}
	client := fake.NewClientset(own)
	var namespaceDelete k8stesting.DeleteActionImpl
	client.PrependReactor("delete", "namespaces", func(a k8stesting.Action) (bool, runtime.Object, error) {
		namespaceDelete = a.(k8stesting.DeleteActionImpl)
		return false, nil, nil
	})
	// The Project CR goes through the other client, and its precondition is
	// invisible for a second reason: the controller-runtime fake checks only
	// the resourceVersion half of one, so a UID-only precondition is neither
	// enforced nor observable without recording it here.
	pinned := map[string]types.UID{}
	crClient := crfake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(shop, own).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.DeleteOption) error {
				var options crclient.DeleteOptions
				options.ApplyOptions(opts)
				if options.Preconditions != nil && options.Preconditions.UID != nil {
					pinned[obj.GetName()] = *options.Preconditions.UID
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).Build()
	h := &Projects{Client: client, CRClient: crClient, Domain: "example.com"}

	rec := httptest.NewRecorder()
	deleteRouter(h).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/projects/shop", nil))
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	require.NotNil(t, namespaceDelete.DeleteOptions.Preconditions,
		"the namespace delete carried no precondition, so it would take whatever object holds the name by the time it lands")
	require.NotNil(t, namespaceDelete.DeleteOptions.Preconditions.UID)
	assert.Equal(t, types.UID("the-namespace"), *namespaceDelete.DeleteOptions.Preconditions.UID)

	assert.Equal(t, types.UID("the-project"), pinned["shop"],
		"the project delete carried no precondition, so a project recreated under this name between the read and the delete is deleted on its predecessor's authorisation")
}

// The HTTP path makes the same decision as the reconciler and the CLI, so it
// needs the same rule: a claim on the live object outranks any record of the
// name, whoever holds it.
func TestDeletingAProjectLeavesANamespaceAnotherProjectClaims(t *testing.T) {
	contested := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "shop-prod",
		UID:    "the-live-object",
		Labels: map[string]string{kipperlabels.Project: "shop"},
	}}
	shop := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Status:     kipperv1.ProjectStatus{Namespaces: []string{"shop-prod"}},
	}
	grocer := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "grocer"},
		Status: kipperv1.ProjectStatus{
			NamespaceClaims: []kipperv1.NamespaceClaim{{Name: "shop-prod", UID: "the-live-object"}},
		},
	}
	client := fake.NewClientset(contested)
	h := &Projects{Client: client, CRClient: testCRClient(shop, grocer, contested), Domain: "example.com"}

	rec := httptest.NewRecorder()
	deleteRouter(h).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/projects/shop", nil))
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	_, err := client.CoreV1().Namespaces().Get(context.Background(), "shop-prod", metav1.GetOptions{})
	require.NoError(t, err,
		"deleting a project destroyed a live namespace another project holds and claims, because this project had the name on record")
}
