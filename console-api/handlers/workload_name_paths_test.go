package handlers

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/handlers/copyenv"
)

// The console's code editor posts here, not to Functions.Create, so guarding
// only the latter left the console's own workflow open.
func TestInlineFunctionCreateRefusesANameAnotherKindHolds(t *testing.T) {
	crc := testCRClient(existingApp("shop-prod"))
	h := &InlineFunctions{Client: fake.NewClientset(), CRClient: crc}

	rec := createRequest(t, func(r *chi.Mux) {
		r.Post("/projects/{name}/functions/inline", h.Create)
	}, "/projects/shop-prod/functions/inline",
		`{"name":"checkout","runtime":"node","code":"export default () => new Response('hi')"}`)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}
	var created kipperv1.Function
	err := crc.Get(context.Background(),
		types.NamespacedName{Name: "checkout", Namespace: "shop-prod"}, &created)
	if err == nil {
		t.Fatal("the refused function was created anyway")
	}
}

// A copy is best-effort: one refused name must not abandon the rest of the
// environment, which is what returning the error did.
func TestCopyEnvironmentSkipsACollidingNameAndCarriesOn(t *testing.T) {
	source := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-staging",
			Labels: map[string]string{kipperLabel: kipperValue}},
		Spec: kipperv1.AppSpec{Image: "checkout:v1", Port: 8080},
	}
	other := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "billing", Namespace: "shop-staging",
			Labels: map[string]string{kipperLabel: kipperValue}},
		Spec: kipperv1.AppSpec{Image: "billing:v1", Port: 8080},
	}
	// The target already has a function called checkout, so only that name is
	// unavailable.
	blocker := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-prod"},
		Spec:       kipperv1.FunctionSpec{Image: "checkout:v1", Port: 8080},
	}
	crc := testCRClient(source, other, blocker)

	c := &copyenv.Copier{Client: fake.NewClientset(), CRClient: crc}
	if _, err := c.Run(context.Background(), copyenv.Options{
		Source: "shop-staging", Target: "shop-prod",
	}); err != nil {
		t.Fatalf("a single refused name aborted the whole copy: %v", err)
	}

	var copied kipperv1.App
	if err := crc.Get(context.Background(),
		types.NamespacedName{Name: "billing", Namespace: "shop-prod"}, &copied); err != nil {
		t.Fatalf("the app after the refused one was never copied: %v", err)
	}

	// The point of the skip: the colliding name must not be copied, or the copy
	// has created exactly the collision this change exists to prevent.
	var collided kipperv1.App
	if err := crc.Get(context.Background(),
		types.NamespacedName{Name: "checkout", Namespace: "shop-prod"}, &collided); err == nil {
		t.Fatal("the copy created an app over a name a function already held")
	}
}

// A copy that skips a workload has to say so. Reporting a completed copy while
// a function is simply absent from the new environment is silent data loss.
func TestCopyEnvironmentWarnsAboutEveryKindItSkips(t *testing.T) {
	source := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-staging",
			Labels: map[string]string{kipperLabel: kipperValue}},
		Spec: kipperv1.FunctionSpec{Image: "checkout:v1", Port: 8080},
	}
	// The target already has an app of that name, and it is older.
	blocker := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-prod",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-24 * time.Hour))},
		Spec: kipperv1.AppSpec{Image: "checkout:v1", Port: 8080},
	}
	crc := testCRClient(source, blocker)

	c := &copyenv.Copier{Client: fake.NewClientset(), CRClient: crc}
	summary, err := c.Run(context.Background(), copyenv.Options{
		Source: "shop-staging", Target: "shop-prod",
	})
	require.NoError(t, err)

	var mentioned bool
	for _, w := range summary.Warnings {
		if strings.Contains(w, "checkout") {
			mentioned = true
		}
	}
	assert.True(t, mentioned,
		"the copy skipped a function and reported success without saying which one or why: %v", summary.Warnings)
}

// An upgraded cluster's workload has no reservation until something makes one.
// A create that finds the workload already there has just backfilled that
// workload's own first claim, so releasing it would undo the conversion and
// nothing else would enqueue it.
func TestCreateKeepsTheReservationWhenTheWorkloadAlreadyExists(t *testing.T) {
	existing := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-prod"},
		Spec:       kipperv1.FunctionSpec{Image: "checkout:v1", Port: 8080},
	}
	crc := testCRClient(existing)
	h := &Functions{
		Client:   fake.NewClientset(),
		Dynamic:  dynfake.NewSimpleDynamicClient(runtime.NewScheme()),
		CRClient: crc,
	}

	rec := createRequest(t, func(r *chi.Mux) {
		r.Post("/projects/{name}/functions", h.Create)
	}, "/projects/shop-prod/functions", `{"name":"checkout","image":"checkout:v2"}`)
	require.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())

	var claim kipperv1.WorkloadName
	require.NoError(t, crc.Get(context.Background(),
		types.NamespacedName{Name: "checkout", Namespace: "shop-prod"}, &claim),
		"the reservation this call backfilled for the existing workload was released again")
	assert.Equal(t, "function", claim.Spec.Kind)
}
