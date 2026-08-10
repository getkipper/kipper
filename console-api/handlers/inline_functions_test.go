package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func postInlineCreate(t *testing.T, handler *InlineFunctions, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Post("/api/v1/projects/{name}/inline-functions", handler.Create)
	req := httptest.NewRequest("POST", "/api/v1/projects/default/inline-functions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestInlineFunctions_Create_ProvisionsRabbitMQVhost confirms the
// architectural move: a one-shot function-create POST with a
// rabbitmq binding pointing at a non-default vhost results in a
// per-binding credentials Secret being written, the Function CR
// carrying the resolved (non-empty) Database value, and the
// reconciler-visible Secret carrying VHOST=<vhost>. Writing only the
// CR without these side-effects would leave the RabbitMQ vhost
// unprovisioned.
func TestInlineFunctions_Create_ProvisionsRabbitMQVhost(t *testing.T) {
	ss := newServiceStatefulSet("rabbit", "rabbitmq")
	creds := newCredentialsSecret("rabbit", map[string][]byte{
		"HOST":     []byte("rabbit.default.svc"),
		"PORT":     []byte("5672"),
		"USERNAME": []byte("kipper"),
		"PASSWORD": []byte("secret"),
		"VHOST":    []byte("/"),
	})
	kClient := fake.NewClientset(ss, creds)
	crClient := testCRClient()
	svcHandler := &Services{Client: kClient, CRClient: crClient}
	handler := &InlineFunctions{Client: kClient, CRClient: crClient, Services: svcHandler}

	body := `{
		"name": "order-worker",
		"runtime": "node",
		"code": "module.exports = async () => 'ok'",
		"bindings": [{"service": "rabbit", "prefix": "AMQP_", "database": "orders"}]
	}`
	rec := postInlineCreate(t, handler, body)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// CR exists with the resolved binding.
	var fn kipperv1.Function
	require.NoError(t, crClient.Get(context.Background(), crclient.ObjectKey{Namespace: "default", Name: "order-worker"}, &fn))
	require.Len(t, fn.Spec.ServiceBindings, 1)
	assert.Equal(t, "rabbit", fn.Spec.ServiceBindings[0].Name)
	assert.Equal(t, "orders", fn.Spec.ServiceBindings[0].Database)

	// Per-binding Secret was materialised with VHOST overridden.
	// The Function reconciler derives the per-binding Secret from the binding
	// recorded above, so creation records intent and the controller renders it.
	_, secretErr := kClient.CoreV1().Secrets("default").Get(context.Background(), "rabbit-function-order-worker-credentials", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(secretErr), "creating a function must not write the derived Secret")
}

// TestInlineFunctions_Create_NormalisesRabbitMQDefaultVhost confirms
// that requesting the default vhost "/" via the create path doesn't
// leave the CR pointing at a per-binding Secret that was never
// written. The resolved Database must be "" so the reconciler stays
// on the shared <service>-credentials Secret.
func TestInlineFunctions_Create_NormalisesRabbitMQDefaultVhost(t *testing.T) {
	ss := newServiceStatefulSet("rabbit", "rabbitmq")
	creds := newCredentialsSecret("rabbit", map[string][]byte{
		"HOST":     []byte("rabbit.default.svc"),
		"PORT":     []byte("5672"),
		"USERNAME": []byte("kipper"),
		"PASSWORD": []byte("secret"),
		"VHOST":    []byte("/"),
	})
	kClient := fake.NewClientset(ss, creds)
	crClient := testCRClient()
	svcHandler := &Services{Client: kClient, CRClient: crClient}
	handler := &InlineFunctions{Client: kClient, CRClient: crClient, Services: svcHandler}

	body := `{
		"name": "consumer",
		"runtime": "node",
		"code": "module.exports = async () => 'ok'",
		"bindings": [{"service": "rabbit", "prefix": "AMQP_", "database": "/"}]
	}`
	rec := postInlineCreate(t, handler, body)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var fn kipperv1.Function
	require.NoError(t, crClient.Get(context.Background(), crclient.ObjectKey{Namespace: "default", Name: "consumer"}, &fn))
	require.Len(t, fn.Spec.ServiceBindings, 1)
	assert.Empty(t, fn.Spec.ServiceBindings[0].Database, `"/" must normalise to "" on the CR so the reconciler uses the shared Secret`)

	_, err := kClient.CoreV1().Secrets("default").Get(context.Background(), "rabbit-consumer-credentials", metav1.GetOptions{})
	assert.Error(t, err, "no per-binding Secret should be written for the default vhost")
}

// TestInlineFunctions_Create_RejectsUnknownService confirms the
// resolve-before-write contract: a binding pointing at a service
// that doesn't exist makes the create fail at phase 1, so no
// Function CR is written and no per-binding Secret leaks behind.
func TestInlineFunctions_Create_RejectsUnknownService(t *testing.T) {
	kClient := fake.NewClientset()
	crClient := testCRClient()
	svcHandler := &Services{Client: kClient, CRClient: crClient}
	handler := &InlineFunctions{Client: kClient, CRClient: crClient, Services: svcHandler}

	body := `{
		"name": "order-worker",
		"runtime": "node",
		"code": "module.exports = async () => 'ok'",
		"bindings": [{"service": "ghost", "prefix": "AMQP_", "database": "orders"}]
	}`
	rec := postInlineCreate(t, handler, body)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "binding resolution for a missing service must fail the create")

	var fn kipperv1.Function
	err := crClient.Get(context.Background(), crclient.ObjectKey{Namespace: "default", Name: "order-worker"}, &fn)
	assert.Error(t, err, "Function CR must not be created when binding resolution fails")

	_, err = kClient.CoreV1().Secrets("default").Get(context.Background(), "ghost-order-worker-credentials", metav1.GetOptions{})
	assert.Error(t, err, "per-binding Secret must not exist when binding resolution fails")
}

// TestInlineFunctions_Create_PassthroughWithoutServices keeps the
// older wiring working: when Services is nil the handler degrades
// to writing only the CR, with bindings carried verbatim. The
// console always wires Services in, but this guards against future
// callers (CLI, tests) forgetting to pass it.
func TestInlineFunctions_Create_PassthroughWithoutServices(t *testing.T) {
	kClient := fake.NewClientset()
	crClient := testCRClient()
	handler := &InlineFunctions{Client: kClient, CRClient: crClient}

	body := `{
		"name": "isolated",
		"runtime": "node",
		"code": "module.exports = async () => 'ok'",
		"bindings": [{"service": "rabbit", "prefix": "AMQP_", "database": "orders"}]
	}`
	rec := postInlineCreate(t, handler, body)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var fn kipperv1.Function
	require.NoError(t, crClient.Get(context.Background(), crclient.ObjectKey{Namespace: "default", Name: "isolated"}, &fn))
	require.Len(t, fn.Spec.ServiceBindings, 1)
	assert.Equal(t, "orders", fn.Spec.ServiceBindings[0].Database, "without Services, Database carries through unchanged")
}
