package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// conflictOnce simulates a concurrent write by rejecting the first Function update.
func conflictOnce(objs ...crclient.Object) crclient.Client {
	conflicted := false
	return crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).WithInterceptorFuncs(interceptor.Funcs{
		Update: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
			if _, ok := obj.(*kipperv1.Function); ok && !conflicted {
				conflicted = true
				return k8serrors.NewConflict(schema.GroupResource{Group: "kipper.run", Resource: "functions"}, obj.GetName(), nil)
			}
			return c.Update(ctx, obj, opts...)
		},
	}).Build()
}

func TestFunctionWritesRetryAConflict(t *testing.T) {
	for _, tc := range []struct {
		name, route, path, body string
		handler                 func(crclient.Client) http.HandlerFunc
	}{
		{"env", "/projects/{name}/functions/{fn}/env", "/projects/test/functions/fn/env", `{"A":"1"}`,
			func(c crclient.Client) http.HandlerFunc {
				return (&FunctionConfig{Client: fake.NewClientset(), CRClient: c}).UpdateEnv
			}},
		{"dependencies", "/projects/{name}/functions/{fn}/dependencies", "/projects/test/functions/fn/dependencies", `{"requests":"2.32"}`,
			func(c crclient.Client) http.HandlerFunc {
				return (&FunctionConfig{Client: fake.NewClientset(), CRClient: c}).UpdateDependencies
			}},
		{"code", "/projects/{name}/inline-functions/{fn}/code", "/projects/test/inline-functions/fn/code", `{"code":"print(1)"}`,
			func(c crclient.Client) http.HandlerFunc {
				return (&InlineFunctions{Client: fake.NewClientset(), CRClient: c}).UpdateCode
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := chi.NewRouter()
			r.Put(tc.route, tc.handler(conflictOnce(testFunction(nil, map[string]string{}))))
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest("PUT", tc.path, strings.NewReader(tc.body)))

			if rec.Code != http.StatusOK {
				t.Fatalf("a conflict must be retried, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestInlineCreateOfAnExistingFunctionRetriesAConflict(t *testing.T) {
	existing := testFunction(nil, map[string]string{})
	existing.Namespace = "default"
	c := conflictOnce(existing)

	rec := postInlineCreate(t, &InlineFunctions{Client: fake.NewClientset(), CRClient: c},
		`{"name": "fn", "runtime": "python", "code": "print(1)"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("a conflict must be retried, got %d: %s", rec.Code, rec.Body.String())
	}
	var fn kipperv1.Function
	if err := c.Get(context.Background(), crclient.ObjectKey{Namespace: "default", Name: "fn"}, &fn); err != nil {
		t.Fatal(err)
	}
	if fn.Spec.Source == nil || fn.Spec.Source.Code != "print(1)" {
		t.Fatalf("the retried update must store the new code, got %+v", fn.Spec.Source)
	}
}
