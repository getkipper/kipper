package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// One Deployment can only have one controller, so an App, a Function and a Job
// cannot share a name in a namespace. The controller refuses such a workload on
// every pass, correctly, but only into its own log: the CR sits with an empty
// phase and its URL 404s. Refusing the name at creation is what makes that
// visible, so these pin the 409 on every kind against every other kind.
func existingApp(namespace string) crclient.Object {
	return &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: namespace},
		Spec:       kipperv1.AppSpec{Image: "shop:v1", Port: 8080},
	}
}

func existingFunction(namespace string) crclient.Object {
	return &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: namespace},
		Spec:       kipperv1.FunctionSpec{Image: "shop:v1", Port: 8080},
	}
}

func existingJob(namespace string) crclient.Object {
	return &kipperv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: namespace},
		Spec:       kipperv1.JobSpec{Image: "shop:v1"},
	}
}

// createRequest posts body to the given handler route and returns the response.
func createRequest(t *testing.T, register func(*chi.Mux), path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	register(r)
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func requireCollision(t *testing.T, rec *httptest.ResponseRecorder, holder string) {
	t.Helper()
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not json: %s", rec.Body.String())
	}
	// The operator has to learn which kind is holding the name, because the
	// remedy is to rename one of the two and nothing else says which two.
	msg, _ := body["error"].(string)
	if !strings.Contains(strings.ToLower(msg), strings.ToLower(holder)) {
		t.Fatalf("error %q does not name the %s holding the name", msg, holder)
	}
}

func TestCreateFunctionRefusesANameAnotherKindHolds(t *testing.T) {
	for holder, existing := range map[string]crclient.Object{
		"app": existingApp("shop-prod"),
		"job": existingJob("shop-prod"),
	} {
		t.Run(holder, func(t *testing.T) {
			h := &Functions{
				Client:   fake.NewClientset(),
				Dynamic:  dynfake.NewSimpleDynamicClient(runtime.NewScheme()),
				CRClient: testCRClient(existing),
			}
			rec := createRequest(t, func(r *chi.Mux) {
				r.Post("/projects/{name}/functions", h.Create)
			}, "/projects/shop-prod/functions",
				`{"name":"checkout","image":"checkout:v1"}`)
			requireCollision(t, rec, holder)
		})
	}
}

func TestCreateAppRefusesANameAnotherKindHolds(t *testing.T) {
	for holder, existing := range map[string]crclient.Object{
		"function": existingFunction("shop-prod"),
		"job":      existingJob("shop-prod"),
	} {
		t.Run(holder, func(t *testing.T) {
			h := &Apps{Client: fake.NewClientset(), CRClient: testCRClient(existing)}
			rec := createRequest(t, func(r *chi.Mux) {
				r.Post("/projects/{name}/apps", h.Create)
			}, "/projects/shop-prod/apps",
				`{"name":"checkout","image":"checkout:v1","port":8080,"replicas":1}`)
			requireCollision(t, rec, holder)
		})
	}
}

func TestCreateJobRefusesANameAnotherKindHolds(t *testing.T) {
	for holder, existing := range map[string]crclient.Object{
		"app":      existingApp("shop-prod"),
		"function": existingFunction("shop-prod"),
	} {
		t.Run(holder, func(t *testing.T) {
			h := &Jobs{Client: fake.NewClientset(), CRClient: testCRClient(existing)}
			// A job carries its namespace in the body rather than the path.
			rec := createRequest(t, func(r *chi.Mux) {
				r.Post("/jobs", h.Create)
			}, "/jobs",
				`{"name":"checkout","image":"checkout:v1","namespace":"shop-prod"}`)
			requireCollision(t, rec, holder)
		})
	}
}

// The guard is per namespace: two projects may each have a workload of the same
// name, which is the normal case for an environment pair.
func TestCreateAcceptsANameHeldInAnotherNamespace(t *testing.T) {
	crc := testCRClient(existingApp("shop-staging"))
	h := &Functions{
		Client:   fake.NewClientset(),
		Dynamic:  dynfake.NewSimpleDynamicClient(runtime.NewScheme()),
		CRClient: crc,
	}
	rec := createRequest(t, func(r *chi.Mux) {
		r.Post("/projects/{name}/functions", h.Create)
	}, "/projects/shop-prod/functions",
		`{"name":"checkout","image":"checkout:v1"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	// The status alone would pass on a handler that answered 200 without
	// creating anything.
	var created kipperv1.Function
	if err := crc.Get(context.Background(),
		types.NamespacedName{Name: "checkout", Namespace: "shop-prod"}, &created); err != nil {
		t.Fatalf("the function was not created: %v", err)
	}
}

// A cluster that could not answer is not a cluster saying the name is taken.
// 409 tells a client to change its request, which would be a lie here and sends
// an operator renaming a workload over a transient apiserver failure.
func TestCreateAnswers500WhenAvailabilityCannotBeDetermined(t *testing.T) {
	crc := crfake.NewClientBuilder().WithScheme(testScheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ crclient.WithWatch, _ crclient.ObjectKey, obj crclient.Object, _ ...crclient.GetOption) error {
				if _, ok := obj.(*kipperv1.App); ok {
					return apierrors.NewServiceUnavailable("etcd leader changed")
				}
				return apierrors.NewNotFound(schema.GroupResource{Resource: "functions"}, "checkout")
			},
		}).Build()

	h := &Functions{
		Client:   fake.NewClientset(),
		Dynamic:  dynfake.NewSimpleDynamicClient(runtime.NewScheme()),
		CRClient: crc,
	}
	rec := createRequest(t, func(r *chi.Mux) {
		r.Post("/projects/{name}/functions", h.Create)
	}, "/projects/shop-prod/functions",
		`{"name":"checkout","image":"checkout:v1"}`)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
}

// The git token is written to a Secret before the App CR exists, so a refusal
// after that point answers "not created" while leaving the credential behind.
func TestCreateAppRefusedByNameLeavesNoGitCredential(t *testing.T) {
	k8s := fake.NewClientset()
	h := &Apps{Client: k8s, CRClient: testCRClient(existingFunction("shop-prod"))}

	rec := createRequest(t, func(r *chi.Mux) {
		r.Post("/projects/{name}/apps", h.Create)
	}, "/projects/shop-prod/apps",
		`{"name":"checkout","port":8080,"replicas":1,`+
			`"git":{"url":"https://github.com/example/checkout.git","token":"ghp_examplevalue"}}`)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}
	_, err := k8s.CoreV1().Secrets("shop-prod").Get(context.Background(), "checkout-git-credentials", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("a rejected create left its git credential behind (err=%v)", err)
	}
}
