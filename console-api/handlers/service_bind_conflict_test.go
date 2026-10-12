package handlers

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// racingWriter simulates a concurrent environment update followed by a conflict
// on the first workload write.
func racingWriter(objs ...crclient.Object) crclient.Client {
	raced := false
	return crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).WithInterceptorFuncs(interceptor.Funcs{
		Update: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
			if raced {
				return c.Update(ctx, obj, opts...)
			}
			raced = true
			key := crclient.ObjectKeyFromObject(obj)
			switch obj.(type) {
			case *kipperv1.Function:
				var live kipperv1.Function
				if err := c.Get(ctx, key, &live); err != nil {
					return err
				}
				live.Spec.Env = map[string]string{"COMPETING": "1"}
				if err := c.Update(ctx, &live); err != nil {
					return err
				}
			case *kipperv1.App:
				var live kipperv1.App
				if err := c.Get(ctx, key, &live); err != nil {
					return err
				}
				live.Spec.Env = map[string]string{"COMPETING": "1"}
				if err := c.Update(ctx, &live); err != nil {
					return err
				}
			}
			return k8serrors.NewConflict(schema.GroupResource{Group: "kipper.run", Resource: "workloads"}, obj.GetName(), nil)
		},
	}).Build()
}

func redisWorld() *Services {
	return &Services{Client: fake.NewClientset(newServiceStatefulSet("cache", "redis"), newCredentialsSecret("cache", map[string][]byte{"HOST": []byte("cache.default.svc")}))}
}

func TestBindRetriesAConflictAndKeepsTheOtherWrite(t *testing.T) {
	for _, target := range []string{"app", "function"} {
		t.Run(target, func(t *testing.T) {
			s := redisWorld()
			var c crclient.Client
			if target == "app" {
				c = racingWriter(&kipperv1.App{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"}, Spec: kipperv1.AppSpec{Image: "web:1", Port: 8080}})
			} else {
				c = racingWriter(&kipperv1.Function{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"}, Spec: kipperv1.FunctionSpec{Runtime: "python"}})
			}
			s.CRClient = c

			rec := postBind(t, s, `{"service":"cache","app":"web","namespace":"default","target":"`+target+`"}`)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			bindings, env := workloadBindings(t, c, target)
			require.Len(t, bindings, 1)
			assert.Equal(t, "cache", bindings[0].Name)
			assert.Equal(t, "1", env["COMPETING"], "the other writer's change survives")
		})
	}
}

func TestUnbindRetriesAConflictAndKeepsTheOtherWrite(t *testing.T) {
	binding := []kipperv1.ServiceBinding{{Name: "cache", Prefix: "REDIS_"}}
	for _, target := range []string{"app", "function"} {
		t.Run(target, func(t *testing.T) {
			s := redisWorld()
			var c crclient.Client
			if target == "app" {
				c = racingWriter(&kipperv1.App{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"}, Spec: kipperv1.AppSpec{Image: "web:1", Port: 8080, ServiceBindings: binding}})
			} else {
				c = racingWriter(&kipperv1.Function{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"}, Spec: kipperv1.FunctionSpec{Runtime: "python", ServiceBindings: binding}})
			}
			s.CRClient = c

			rec := postUnbind(t, s, `{"service":"cache","app":"web","namespace":"default","target":"`+target+`"}`)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			bindings, env := workloadBindings(t, c, target)
			assert.Empty(t, bindings)
			assert.Equal(t, "1", env["COMPETING"], "the other writer's change survives")
		})
	}
}

func workloadBindings(t *testing.T, c crclient.Client, target string) ([]kipperv1.ServiceBinding, map[string]string) {
	t.Helper()
	key := crclient.ObjectKey{Namespace: "default", Name: "web"}
	if target == "app" {
		var app kipperv1.App
		require.NoError(t, c.Get(context.Background(), key, &app))
		return app.Spec.ServiceBindings, app.Spec.Env
	}
	var fn kipperv1.Function
	require.NoError(t, c.Get(context.Background(), key, &fn))
	return fn.Spec.ServiceBindings, fn.Spec.Env
}

func TestUnbindAnswersForAMissingOrUnreadableWorkload(t *testing.T) {
	s := redisWorld()
	s.CRClient = testCRClient()
	rec := postUnbind(t, s, `{"service":"cache","app":"web","namespace":"default"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "no bindings found")

	for _, target := range []string{"app", "function"} {
		workloadRead := false
		s.CRClient = crfake.NewClientBuilder().WithScheme(testScheme()).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
				switch obj.(type) {
				case *kipperv1.App, *kipperv1.Function:
					workloadRead = true
					return k8serrors.NewServiceUnavailable("the API server is unavailable")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
		rec = postUnbind(t, s, `{"service":"cache","app":"web","namespace":"default","target":"`+target+`"}`)
		assert.Equal(t, http.StatusInternalServerError, rec.Code, target)
		assert.True(t, workloadRead, "%s: the failure must come from reading the workload", target)
	}
}

func TestBindKeepsABindingAddedByAConcurrentBind(t *testing.T) {
	raced := false
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(
		&kipperv1.App{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"}, Spec: kipperv1.AppSpec{Image: "web:1", Port: 8080}},
	).WithInterceptorFuncs(interceptor.Funcs{
		Update: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
			if !raced {
				raced = true
				var live kipperv1.App
				if err := cl.Get(ctx, crclient.ObjectKeyFromObject(obj), &live); err != nil {
					return err
				}
				live.Spec.ServiceBindings = append(live.Spec.ServiceBindings, kipperv1.ServiceBinding{Name: "db", Prefix: "DATABASE_"})
				if err := cl.Update(ctx, &live); err != nil {
					return err
				}
				return k8serrors.NewConflict(schema.GroupResource{Group: "kipper.run", Resource: "apps"}, obj.GetName(), nil)
			}
			return cl.Update(ctx, obj, opts...)
		},
	}).Build()
	s := redisWorld()
	s.CRClient = c

	rec := postBind(t, s, `{"service":"cache","app":"web","namespace":"default"}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	bindings, _ := workloadBindings(t, c, "app")
	names := []string{}
	for _, b := range bindings {
		names = append(names, b.Name)
	}
	assert.ElementsMatch(t, []string{"db", "cache"}, names)
}
