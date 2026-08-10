package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
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
)

func newServiceStatefulSet(name, svcType string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				"kipper.run/service-type": svcType,
				kipperLabel:               kipperValue,
				"app":                     name,
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: name, Image: svcType + ":latest"}}},
			},
		},
	}
}

func newCredentialsSecret(serviceName string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName + "-credentials",
			Namespace: "default",
		},
		Data: data,
	}
}

func postBind(t *testing.T, handler *Services, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Post("/api/v1/bind", handler.Bind)
	req := httptest.NewRequest("POST", "/api/v1/bind", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func postUnbind(t *testing.T, handler *Services, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Post("/api/v1/unbind", handler.Unbind)
	req := httptest.NewRequest("POST", "/api/v1/unbind", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestBind_PerBindingDatabaseWhenExplicit covers the explicit
// per-binding path: the form sent a database name and the backend
// creates a per-binding credentials secret pointing at it. The
// previous version of this test relied on auto-deriving
// <app>_<env> from an empty database field, which was removed
// because it silently created empty databases users didn't ask for.
func TestBind_PerBindingDatabaseWhenExplicit(t *testing.T) {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "blog-test",
			Labels: map[string]string{
				"kipper.run/environment": "test",
			},
		},
	}
	ss := newServiceStatefulSet("db", "postgres")
	ss.Namespace = "blog-test"
	creds := newCredentialsSecret("db", map[string][]byte{
		"HOST":     []byte("db.blog-test.svc"),
		"PORT":     []byte("5432"),
		"USERNAME": []byte("kipper"),
		"PASSWORD": []byte("secret"),
		"NAME":     []byte("app"),
	})
	creds.Namespace = "blog-test"
	appCR := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "domain-service", Namespace: "blog-test"},
		Spec:       kipperv1.AppSpec{Image: "domain:v1", Port: 8080, Replicas: int32Ptr(1)},
	}
	crClient := testCRClient(appCR)
	client := fake.NewClientset(ns, ss, creds)
	handler := &Services{Client: client, CRClient: crClient}

	rec := postBind(t, handler, `{"service":"db","app":"domain-service","namespace":"blog-test","database":"domain_service_test"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp bindResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	assert.Equal(t, "postgres", resp.Type)
	assert.Equal(t, "domain_service_test", resp.Database)
	assert.Equal(t, "domain_service_test", resp.Injected["DB_NAME"], "DB_NAME should reflect the explicit per-binding database")
	assert.Equal(t, "db.blog-test.svc", resp.Injected["DB_HOST"])
	assert.Equal(t, "********", resp.Injected["DB_PASSWORD"])

	var updatedApp kipperv1.App
	err := crClient.Get(context.Background(), crclient.ObjectKey{Namespace: "blog-test", Name: "domain-service"}, &updatedApp)
	if err != nil {
		t.Fatalf("expected app CR to exist: %v", err)
	}
	assert.Len(t, updatedApp.Spec.ServiceBindings, 1)
	assert.Equal(t, "db", updatedApp.Spec.ServiceBindings[0].Name)
	assert.Equal(t, "domain_service_test", updatedApp.Spec.ServiceBindings[0].Database)

	// The binding is recorded on the CR and the App reconciler renders the
	// Secret from it. The handler does not write one: a Secret copied here
	// would freeze the service password as it stood at bind time, and nothing
	// would refresh it when the service rotated.
	for _, name := range []string{"db-domain-service-credentials", "db-app-domain-service-credentials"} {
		_, secretErr := client.CoreV1().Secrets("blog-test").Get(context.Background(), name, metav1.GetOptions{})
		assert.Truef(t, apierrors.IsNotFound(secretErr), "the bind handler must not write %s; the reconciler derives it", name)
	}
}

// TestBind_DefaultUsesServiceDB covers the new default: when the
// caller leaves database empty, the binding attaches to the service's
// own default DB (the NAME on the credentials secret) — no per-app
// database is created, no per-binding secret is written.
func TestBind_DefaultUsesServiceDB(t *testing.T) {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "blog-test"},
	}
	ss := newServiceStatefulSet("db", "postgres")
	ss.Namespace = "blog-test"
	creds := newCredentialsSecret("db", map[string][]byte{
		"HOST":     []byte("db.blog-test.svc"),
		"PORT":     []byte("5432"),
		"USERNAME": []byte("kipper"),
		"PASSWORD": []byte("secret"),
		"NAME":     []byte("app"),
	})
	creds.Namespace = "blog-test"
	appCR := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "blog-test"},
		Spec:       kipperv1.AppSpec{Image: "myapp:v1", Port: 8080, Replicas: int32Ptr(1)},
	}
	crClient := testCRClient(appCR)
	client := fake.NewClientset(ns, ss, creds)
	handler := &Services{Client: client, CRClient: crClient}

	rec := postBind(t, handler, `{"service":"db","app":"myapp","namespace":"blog-test"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp bindResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	assert.Empty(t, resp.Database, "no database should be set on the binding when caller didn't pick one")
	assert.Equal(t, "app", resp.Injected["DB_NAME"], "DB_NAME should reflect the service default")

	// No per-binding secret should exist for the default-DB path.
	_, err := client.CoreV1().Secrets("blog-test").Get(context.Background(), "db-myapp-credentials", metav1.GetOptions{})
	assert.Error(t, err, "no per-binding secret should be created when caller used the default DB")
}

func TestBind_InjectsRedisKeys(t *testing.T) {
	ss := newServiceStatefulSet("myredis", "redis")
	creds := newCredentialsSecret("myredis", map[string][]byte{
		"HOST":     []byte("myredis.default.svc"),
		"PORT":     []byte("6379"),
		"PASSWORD": []byte("redispass"),
	})
	appCR := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "default"},
		Spec:       kipperv1.AppSpec{Image: "myapp:v1", Port: 8080, Replicas: int32Ptr(1)},
	}
	crClient := testCRClient(appCR)
	client := fake.NewClientset(ss, creds)
	handler := &Services{Client: client, CRClient: crClient}

	rec := postBind(t, handler, `{"service":"myredis","app":"myapp","namespace":"default"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp bindResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	assert.Equal(t, "redis", resp.Type)
	assert.Equal(t, "myredis.default.svc", resp.Injected["REDIS_HOST"])
	assert.Equal(t, "6379", resp.Injected["REDIS_PORT"])
	assert.Equal(t, "********", resp.Injected["REDIS_PASSWORD"])
}

func TestBind_InjectsMinIOKeys(t *testing.T) {
	ss := newServiceStatefulSet("myminio", "minio")
	creds := newCredentialsSecret("myminio", map[string][]byte{
		"ENDPOINT":   []byte("http://myminio.default.svc:9000"),
		"ACCESS_KEY": []byte("minioadmin"),
		"SECRET_KEY": []byte("miniosecret"),
	})
	appCR := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "default"},
		Spec:       kipperv1.AppSpec{Image: "myapp:v1", Port: 8080, Replicas: int32Ptr(1)},
	}
	crClient := testCRClient(appCR)
	client := fake.NewClientset(ss, creds)
	handler := &Services{Client: client, CRClient: crClient}

	rec := postBind(t, handler, `{"service":"myminio","app":"myapp","namespace":"default"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp bindResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	assert.Equal(t, "minio", resp.Type)
	assert.Equal(t, "http://myminio.default.svc:9000", resp.Injected["S3_ENDPOINT"])
	assert.Equal(t, "minioadmin", resp.Injected["S3_ACCESS_KEY"])
	// The secret key must be masked, never echoed in the bind preview.
	assert.Equal(t, "********", resp.Injected["S3_SECRET_KEY"])
}

// TestBind_RabbitMQDefaultVhostNoPerBindingSecret covers the happy
// path with no isolation: the caller didn't pick a vhost, so the
// binding shares the service-default `/` and no per-binding secret
// is written. AMQP_VHOST in the env reflects the shared value.
func TestBind_RabbitMQDefaultVhostNoPerBindingSecret(t *testing.T) {
	ss := newServiceStatefulSet("rabbit", "rabbitmq")
	creds := newCredentialsSecret("rabbit", map[string][]byte{
		"HOST":     []byte("rabbit.default.svc"),
		"PORT":     []byte("5672"),
		"USERNAME": []byte("kipper"),
		"PASSWORD": []byte("secret"),
		"VHOST":    []byte("/"),
	})
	appCR := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "consumer", Namespace: "default"},
		Spec:       kipperv1.AppSpec{Image: "consumer:v1", Port: 8080, Replicas: int32Ptr(1)},
	}
	crClient := testCRClient(appCR)
	client := fake.NewClientset(ss, creds)
	handler := &Services{Client: client, CRClient: crClient}

	rec := postBind(t, handler, `{"service":"rabbit","app":"consumer","namespace":"default"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp bindResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	assert.Equal(t, "rabbitmq", resp.Type)
	assert.Empty(t, resp.Database, "default vhost path must leave Database empty")
	assert.Equal(t, "/", resp.Injected["AMQP_VHOST"], "AMQP_VHOST should reflect the shared default")

	_, err := client.CoreV1().Secrets("default").Get(context.Background(), "rabbit-consumer-credentials", metav1.GetOptions{})
	assert.Error(t, err, "no per-binding secret should be created for the default vhost")
}

// TestBind_RabbitMQExplicitSlashIsTreatedAsDefault covers the
// degenerate "/ is the default vhost" case: the caller sent the
// default value explicitly, and the handler must not persist it as
// a per-binding namespace — otherwise the reconciler would later
// look for a per-binding secret that was never created.
func TestBind_RabbitMQExplicitSlashIsTreatedAsDefault(t *testing.T) {
	ss := newServiceStatefulSet("rabbit", "rabbitmq")
	creds := newCredentialsSecret("rabbit", map[string][]byte{
		"HOST":     []byte("rabbit.default.svc"),
		"PORT":     []byte("5672"),
		"USERNAME": []byte("kipper"),
		"PASSWORD": []byte("secret"),
		"VHOST":    []byte("/"),
	})
	appCR := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "consumer", Namespace: "default"},
		Spec:       kipperv1.AppSpec{Image: "consumer:v1", Port: 8080, Replicas: int32Ptr(1)},
	}
	crClient := testCRClient(appCR)
	client := fake.NewClientset(ss, creds)
	handler := &Services{Client: client, CRClient: crClient}

	rec := postBind(t, handler, `{"service":"rabbit","app":"consumer","namespace":"default","database":"/"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp bindResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	assert.Empty(t, resp.Database, `"/" should not be persisted on the binding — it's the shared default`)

	var updatedApp kipperv1.App
	if err := crClient.Get(context.Background(), crclient.ObjectKey{Namespace: "default", Name: "consumer"}, &updatedApp); err != nil {
		t.Fatalf("get app: %v", err)
	}
	if assert.Len(t, updatedApp.Spec.ServiceBindings, 1) {
		assert.Empty(t, updatedApp.Spec.ServiceBindings[0].Database, "binding.Database must be empty for the default vhost so the reconciler uses the shared secret")
	}

	_, err := client.CoreV1().Secrets("default").Get(context.Background(), "rabbit-consumer-credentials", metav1.GetOptions{})
	assert.Error(t, err, "no per-binding secret should be created when sharing the default vhost")
}

// TestBind_RabbitMQExplicitVhostPerBindingSecret covers the
// per-binding path: the caller picked a vhost, so the binding gets a
// dedicated credentials Secret with VHOST overridden, and the
// injected env shows AMQP_VHOST set to the binding's value.
func TestBind_RabbitMQExplicitVhostPerBindingSecret(t *testing.T) {
	ss := newServiceStatefulSet("rabbit", "rabbitmq")
	creds := newCredentialsSecret("rabbit", map[string][]byte{
		"HOST":     []byte("rabbit.default.svc"),
		"PORT":     []byte("5672"),
		"USERNAME": []byte("kipper"),
		"PASSWORD": []byte("secret"),
		"VHOST":    []byte("/"),
	})
	appCR := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "order-service", Namespace: "default"},
		Spec:       kipperv1.AppSpec{Image: "order:v1", Port: 8080, Replicas: int32Ptr(1)},
	}
	crClient := testCRClient(appCR)
	client := fake.NewClientset(ss, creds)
	handler := &Services{Client: client, CRClient: crClient}

	rec := postBind(t, handler, `{"service":"rabbit","app":"order-service","namespace":"default","database":"orders"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp bindResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	assert.Equal(t, "rabbitmq", resp.Type)
	assert.Equal(t, "orders", resp.Database, "binding carries the vhost name in the Database field")
	assert.Equal(t, "orders", resp.Injected["AMQP_VHOST"], "AMQP_VHOST should reflect the per-binding vhost")
	// NAME must not be advertised for rabbitmq — only VHOST.
	_, hasName := resp.Injected["AMQP_NAME"]
	assert.False(t, hasName, "AMQP_NAME should not appear for rabbitmq bindings")

	// The vhost is recorded on the binding; the reconciler derives the Secret
	// that overrides VHOST. See TestReconcileBindingSecrets_* for that half.
	_, secretErr := client.CoreV1().Secrets("default").Get(context.Background(), "rabbit-app-order-service-credentials", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(secretErr), "the bind handler must not write the derived Secret")
}

func TestLogicalNamespaceCmd_RabbitMQ(t *testing.T) {
	creds := newCredentialsSecret("rabbit", map[string][]byte{"USERNAME": []byte("kipper")})
	cmd, err := logicalNamespaceCmd("rabbitmq", "orders", creds)
	if err != nil {
		t.Fatalf("logicalNamespaceCmd: %v", err)
	}
	// Pin the shape: sh -c <script> -- <vhost> <user>. User input
	// must travel as positional args, never interpolated, so a name
	// with shell metacharacters can't break out of quoting.
	if len(cmd) < 5 || cmd[0] != "sh" || cmd[1] != "-c" || cmd[3] != "--" {
		t.Fatalf("expected `sh -c <script> -- <vhost> <user>`, got %v", cmd)
	}
	if cmd[4] != "orders" {
		t.Errorf("expected vhost as positional arg 1, got %q", cmd[4])
	}
	if cmd[5] != "kipper" {
		t.Errorf("expected username as positional arg 2, got %q", cmd[5])
	}
	script := cmd[2]
	for _, want := range []string{"list_vhosts", `add_vhost -- "$1"`, `set_permissions -p "$1" "$2"`} {
		if !strings.Contains(script, want) {
			t.Errorf("expected script to contain %q, got %q", want, script)
		}
	}
	// Make sure no raw value ended up interpolated into the script.
	if strings.Contains(script, "orders") {
		t.Errorf("vhost value must not be interpolated into the script, got %q", script)
	}
}

func TestValidNamespaceValue(t *testing.T) {
	cases := map[string]bool{
		"orders":                true,
		"order_service_test":    true,
		"order-service":         true,
		"v1.5":                  true,
		"orders/sub":            true,
		"":                      false,
		"orders; rm -rf /":      false,
		"orders\"":              false,
		"orders`whoami`":        false,
		"orders$(whoami)":       false,
		"orders\\":              false,
		strings.Repeat("a", 64): false,
	}
	for input, want := range cases {
		if got := validNamespaceValue(input); got != want {
			t.Errorf("validNamespaceValue(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestLogicalNamespaceCmd_RejectsShellMetacharacters(t *testing.T) {
	creds := newCredentialsSecret("rabbit", map[string][]byte{"USERNAME": []byte("kipper")})
	_, err := logicalNamespaceCmd("rabbitmq", `orders"; rm -rf / #`, creds)
	if err == nil {
		t.Fatal("expected error for vhost with shell metacharacters, got nil")
	}
}

func TestParseRabbitMQVhosts(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []rabbitMQVhost
	}{
		{
			name: "single default vhost",
			in:   "/\n",
			want: []rabbitMQVhost{{Name: "/", Default: true}},
		},
		{
			name: "default plus per-binding vhosts in arbitrary order",
			in:   "orders\n/\nbilling\n",
			want: []rabbitMQVhost{
				{Name: "orders", Default: false},
				{Name: "/", Default: true},
				{Name: "billing", Default: false},
			},
		},
		{
			name: "blank lines and duplicates are filtered",
			in:   "\norders\n\norders\n",
			want: []rabbitMQVhost{{Name: "orders", Default: false}},
		},
		{
			name: "empty output produces an empty slice (not nil) for JSON",
			in:   "",
			want: []rabbitMQVhost{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseRabbitMQVhosts(c.in)
			assert.Equal(t, c.want, got)
		})
	}
}

// TestProvisionPerBinding_NormalisesDefaults pins the contract the
// inline-function-create handler depends on: an empty input, or
// rabbitmq "/", both come back as "" so the reconciler stays on the
// shared service credentials Secret and no per-binding Secret is
// written. The full happy path is covered by TestBind_RabbitMQ*.
func TestProvisionPerBinding_NormalisesDefaults(t *testing.T) {
	creds := newCredentialsSecret("rabbit", map[string][]byte{
		"HOST":     []byte("rabbit.default.svc"),
		"PORT":     []byte("5672"),
		"USERNAME": []byte("kipper"),
		"PASSWORD": []byte("secret"),
		"VHOST":    []byte("/"),
	})
	client := fake.NewClientset(creds)
	handler := &Services{Client: client, CRClient: testCRClient()}

	t.Run("rabbitmq slash becomes empty", func(t *testing.T) {
		db, env, err := handler.provisionPerBinding(context.Background(), "rabbit", "default", "rabbitmq", "AMQP_", "/", creds)
		assert.NoError(t, err)
		assert.Empty(t, db, "rabbitmq \"/\" must normalise to empty so the reconciler uses the shared Secret")
		assert.Equal(t, "/", env["AMQP_VHOST"], "shared VHOST falls through from the service credentials")
		_, hasPer := client.CoreV1().Secrets("default").Get(context.Background(), "rabbit-consumer-credentials", metav1.GetOptions{})
		assert.Error(t, hasPer, "no per-binding Secret should be created for the default vhost path")
	})

	t.Run("empty input stays empty", func(t *testing.T) {
		db, _, err := handler.provisionPerBinding(context.Background(), "rabbit", "default", "rabbitmq", "AMQP_", "", creds)
		assert.NoError(t, err)
		assert.Empty(t, db)
	})
}

func TestServiceHasLogicalNamespace(t *testing.T) {
	cases := map[string]bool{
		"postgres":   true,
		"mysql":      true,
		"mongodb":    true,
		"rabbitmq":   true,
		"redis":      false,
		"minio":      false,
		"opensearch": false,
	}
	for svcType, want := range cases {
		if got := kipperv1.HasLogicalNamespace(svcType); got != want {
			t.Errorf("HasLogicalNamespace(%q) = %v, want %v", svcType, got, want)
		}
	}
}

func TestBind_AppNotFound(t *testing.T) {
	ss := newServiceStatefulSet("mydb", "postgres")
	creds := newCredentialsSecret("mydb", map[string][]byte{
		"HOST": []byte("mydb.default.svc"),
		"PORT": []byte("5432"),
		"NAME": []byte("appdb"),
	})
	client := fake.NewClientset(ss, creds)
	handler := &Services{Client: client, CRClient: testCRClient()}

	rec := postBind(t, handler, `{"service":"mydb","app":"nonexistent","namespace":"default"}`)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	var errResp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	assert.Contains(t, errResp["error"], "nonexistent")
}

func TestBind_ServiceNotFound(t *testing.T) {
	appCR := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "default"},
		Spec:       kipperv1.AppSpec{Image: "myapp:v1", Port: 8080, Replicas: int32Ptr(1)},
	}
	client := fake.NewClientset()
	handler := &Services{Client: client, CRClient: testCRClient(appCR)}

	rec := postBind(t, handler, `{"service":"nonexistent","app":"myapp","namespace":"default"}`)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	var errResp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	assert.Contains(t, errResp["error"], "nonexistent")
}

func TestUnbind_RemovesServiceBinding(t *testing.T) {
	ss := newServiceStatefulSet("mydb", "postgres")
	creds := newCredentialsSecret("mydb", map[string][]byte{
		"HOST": []byte("mydb.default.svc"),
		"PORT": []byte("5432"),
		"NAME": []byte("appdb"),
	})
	appCR := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "default"},
		Spec: kipperv1.AppSpec{
			Image:           "myapp:v1",
			Port:            8080,
			Replicas:        int32Ptr(1),
			ServiceBindings: []kipperv1.ServiceBinding{{Name: "mydb", Prefix: "DB_"}},
		},
	}
	crClient := testCRClient(appCR)
	client := fake.NewClientset(ss, creds)
	handler := &Services{Client: client, CRClient: crClient}

	rec := postUnbind(t, handler, `{"service":"mydb","app":"myapp","namespace":"default"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	var updatedApp kipperv1.App
	err := crClient.Get(context.Background(), crclient.ObjectKey{Namespace: "default", Name: "myapp"}, &updatedApp)
	if err != nil {
		t.Fatalf("expected app CR to still exist: %v", err)
	}
	assert.Empty(t, updatedApp.Spec.ServiceBindings, "service binding should be removed")
}

func TestBind_PreservesExistingEnvVars(t *testing.T) {
	ss := newServiceStatefulSet("mydb", "postgres")
	creds := newCredentialsSecret("mydb", map[string][]byte{
		"HOST":     []byte("mydb.default.svc.cluster.local"),
		"PORT":     []byte("5432"),
		"USERNAME": []byte("kipper"),
		"PASSWORD": []byte("secret"),
		"NAME":     []byte("app"),
	})
	appCR := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "default"},
		Spec: kipperv1.AppSpec{
			Image:    "myapp:v1",
			Port:     8080,
			Replicas: int32Ptr(1),
			Env: map[string]string{
				"LOG_LEVEL": "debug",
			},
		},
	}
	crClient := testCRClient(appCR)
	client := fake.NewClientset(ss, creds)
	handler := &Services{Client: client, CRClient: crClient}

	rec := postBind(t, handler, `{"service":"mydb","app":"myapp","namespace":"default"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	var updatedApp kipperv1.App
	err := crClient.Get(context.Background(), crclient.ObjectKey{Namespace: "default", Name: "myapp"}, &updatedApp)
	if err != nil {
		t.Fatalf("expected app CR to exist: %v", err)
	}
	assert.Equal(t, "debug", updatedApp.Spec.Env["LOG_LEVEL"], "existing LOG_LEVEL should be preserved")
	assert.Empty(t, updatedApp.Spec.Env["DB_URL"], "no URL should be written to Spec.Env")
}

func TestBind_RequiresNamespace(t *testing.T) {
	handler := &Services{Client: fake.NewClientset(), CRClient: testCRClient()}

	rec := postBind(t, handler, `{"service":"db","app":"myapp"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUnbind_RequiresNamespace(t *testing.T) {
	handler := &Services{Client: fake.NewClientset(), CRClient: testCRClient()}

	rec := postUnbind(t, handler, `{"service":"db","app":"myapp"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestBind_NeverResolvesAcrossNamespaces pins the tenant boundary: a service
// that exists only in another namespace must not satisfy a bind in the
// request namespace, even when that other namespace would sort first in a
// cluster-wide list.
func TestBind_NeverResolvesAcrossNamespaces(t *testing.T) {
	ss := newServiceStatefulSet("db", "postgres")
	ss.Namespace = "aaa-first"
	creds := newCredentialsSecret("db", map[string][]byte{
		"HOST": []byte("db.aaa-first.svc"),
		"NAME": []byte("app"),
	})
	creds.Namespace = "aaa-first"
	appCR := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "blog-test"},
		Spec:       kipperv1.AppSpec{Image: "myapp:v1", Port: 8080, Replicas: int32Ptr(1)},
	}
	crClient := testCRClient(appCR)
	handler := &Services{Client: fake.NewClientset(ss, creds), CRClient: crClient}

	rec := postBind(t, handler, `{"service":"db","app":"myapp","namespace":"blog-test"}`)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	var updatedApp kipperv1.App
	if err := crClient.Get(context.Background(), crclient.ObjectKey{Namespace: "blog-test", Name: "myapp"}, &updatedApp); err != nil {
		t.Fatalf("get app: %v", err)
	}
	assert.Empty(t, updatedApp.Spec.ServiceBindings, "no binding may be written when the service is absent from the request namespace")
}

// TestUnbind_NeverResolvesAcrossNamespaces mirrors the bind case: a same-named
// service in another tenant's namespace must never decide which variables get
// removed here.
//
// It no longer refuses. Unbinding is what a workload pointing at a service that
// is not there actually needs — it fails its whole reconcile until the binding
// goes — so the binding is removed. What the missing service costs is the
// default prefix, and without one the injected variables are left alone rather
// than guessed at: a prefix of "" matches every key and would empty spec.env.
func TestUnbind_NeverResolvesAcrossNamespaces(t *testing.T) {
	ss := newServiceStatefulSet("db", "redis")
	ss.Namespace = "aaa-first"
	appCR := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "blog-test"},
		Spec: kipperv1.AppSpec{
			Image:           "myapp:v1",
			Port:            8080,
			Replicas:        int32Ptr(1),
			ServiceBindings: []kipperv1.ServiceBinding{{Name: "db"}},
			Env:             map[string]string{"REDIS_HOST": "typed-by-hand", "LOG_LEVEL": "info"},
		},
	}
	crClient := testCRClient(appCR)
	handler := &Services{Client: fake.NewClientset(ss), CRClient: crClient}

	rec := postUnbind(t, handler, `{"service":"db","app":"myapp","namespace":"blog-test"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	var updatedApp kipperv1.App
	if err := crClient.Get(context.Background(), crclient.ObjectKey{Namespace: "blog-test", Name: "myapp"}, &updatedApp); err != nil {
		t.Fatalf("get app: %v", err)
	}
	assert.Empty(t, updatedApp.Spec.ServiceBindings, "the binding must go, or the workload stays wedged with no way out")
	assert.Equal(t, map[string]string{"REDIS_HOST": "typed-by-hand", "LOG_LEVEL": "info"}, updatedApp.Spec.Env,
		"another namespace's service type must not decide which variables are removed here")

	// With no service and no prefix on the binding there is nothing that
	// identifies the injected variables, so they stay — and the caller is told,
	// rather than finding stale entries later.
	assert.Contains(t, rec.Body.String(), "left in place",
		"a cleanup that could not be done must be reported, not implied by silence")
}

// The recovery path for a workload wedged by a deleted service: the binding
// goes even though nothing can resolve the service any more. With an explicit
// prefix on the binding there is no guessing to do, so the injected variables
// go with it.
func TestUnbind_WorksWhenTheServiceIsAlreadyGone(t *testing.T) {
	appCR := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "blog-test"},
		Spec: kipperv1.AppSpec{
			Image:           "myapp:v1",
			Port:            8080,
			Replicas:        int32Ptr(1),
			ServiceBindings: []kipperv1.ServiceBinding{{Name: "db", Prefix: "DB_"}},
			Env:             map[string]string{"DB_HOST": "db.blog-test.svc", "LOG_LEVEL": "info"},
		},
	}
	crClient := testCRClient(appCR)
	handler := &Services{Client: fake.NewClientset(), CRClient: crClient}

	rec := postUnbind(t, handler, `{"service":"db","app":"myapp","namespace":"blog-test"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var updatedApp kipperv1.App
	require.NoError(t, crClient.Get(context.Background(), crclient.ObjectKey{Namespace: "blog-test", Name: "myapp"}, &updatedApp))
	assert.Empty(t, updatedApp.Spec.ServiceBindings)
	assert.Equal(t, map[string]string{"LOG_LEVEL": "info"}, updatedApp.Spec.Env,
		"the binding named its own prefix, so its injected variables go with it")
}

// TestBind_ResolvesServiceCRInNamespace covers the Service-CR-first path of
// the namespace-scoped lookup: no StatefulSet exists, the type comes from
// the CR in the request namespace.
func TestBind_ResolvesServiceCRInNamespace(t *testing.T) {
	svcCR := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "cache", Namespace: "blog-test"},
		Spec:       kipperv1.ServiceSpec{Type: "redis"},
	}
	appCR := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "blog-test"},
		Spec:       kipperv1.AppSpec{Image: "myapp:v1", Port: 8080, Replicas: int32Ptr(1)},
	}
	creds := newCredentialsSecret("cache", map[string][]byte{
		"HOST":     []byte("cache.blog-test.svc"),
		"PORT":     []byte("6379"),
		"PASSWORD": []byte("redispass"),
	})
	creds.Namespace = "blog-test"
	handler := &Services{Client: fake.NewClientset(creds), CRClient: testCRClient(svcCR, appCR)}

	rec := postBind(t, handler, `{"service":"cache","app":"myapp","namespace":"blog-test"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp bindResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	assert.Equal(t, "redis", resp.Type)
}

// TestBind_IgnoresUnlabelledStatefulSet: a StatefulSet without the
// kipper.run/service-type label is not a Kipper service and must not
// resolve, whatever its name.
func TestBind_IgnoresUnlabelledStatefulSet(t *testing.T) {
	ss := newServiceStatefulSet("db", "postgres")
	delete(ss.Labels, "kipper.run/service-type")
	appCR := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "default"},
		Spec:       kipperv1.AppSpec{Image: "myapp:v1", Port: 8080, Replicas: int32Ptr(1)},
	}
	handler := &Services{Client: fake.NewClientset(ss), CRClient: testCRClient(appCR)}

	rec := postBind(t, handler, `{"service":"db","app":"myapp","namespace":"default"}`)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestBind_LookupAPIFailureIs500: a Kubernetes API failure during resolution
// must surface as 500, never masquerade as a 404 miss.
func TestBind_LookupAPIFailureIs500(t *testing.T) {
	appCR := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "default"},
		Spec:       kipperv1.AppSpec{Image: "myapp:v1", Port: 8080, Replicas: int32Ptr(1)},
	}
	client := fake.NewClientset()
	client.PrependReactor("get", "statefulsets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("apiserver unavailable")
	})
	handler := &Services{Client: client, CRClient: testCRClient(appCR)}

	rec := postBind(t, handler, `{"service":"db","app":"myapp","namespace":"default"}`)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// TestInjectedEnv_DefaultPrefixResolvedInProjectNamespace: when a binding
// has no explicit prefix, the default derives from the service type in the
// app's own namespace — a same-named service of a different type in another
// namespace (one that sorts first) must not influence it.
func TestInjectedEnv_DefaultPrefixResolvedInProjectNamespace(t *testing.T) {
	ssOwn := newServiceStatefulSet("db", "postgres")
	ssOwn.Namespace = "blog-test"
	ssDecoy := newServiceStatefulSet("db", "redis")
	ssDecoy.Namespace = "aaa-first"
	creds := newCredentialsSecret("db", map[string][]byte{
		"HOST": []byte("db.blog-test.svc"),
		"NAME": []byte("app"),
	})
	creds.Namespace = "blog-test"
	appCR := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "blog-test"},
		Spec: kipperv1.AppSpec{
			Image:           "myapp:v1",
			Port:            8080,
			Replicas:        int32Ptr(1),
			ServiceBindings: []kipperv1.ServiceBinding{{Name: "db"}},
		},
	}
	handler := &Services{Client: fake.NewClientset(ssOwn, ssDecoy, creds), CRClient: testCRClient(appCR)}

	r := chi.NewRouter()
	r.Get("/api/v1/projects/{name}/apps/{app}/env/injected", handler.InjectedEnv)
	req := httptest.NewRequest("GET", "/api/v1/projects/blog-test/apps/myapp/env/injected", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)

	var vars []injectedVar
	if err := json.Unmarshal(rec.Body.Bytes(), &vars); err != nil {
		t.Fatalf("decode: %v", err)
	}
	names := make([]string, 0, len(vars))
	for _, v := range vars {
		names = append(names, v.Name)
	}
	assert.Contains(t, names, "DB_HOST", "prefix must come from the postgres service in the app's namespace, not the redis decoy")
}

// Unbinding a database-pinned binding has to delete the derived Secret, and the
// name it deletes carries the workload kind. Nothing exercised this: no test
// unbound a pinned binding at all, so the kind selection was free to be wrong.
func TestUnbind_RemovesTheBindingFromTheRightKind(t *testing.T) {
	controller := true
	ownedBy := func(name, kind, workload string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "blog-test",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(), Kind: kind,
				Name: workload, UID: types.UID("uid-" + kind), Controller: &controller,
			}},
		}}
	}

	for _, tc := range []struct {
		target, derived, other, kind string
	}{
		{"app", "db-app-api-credentials", "db-function-api-credentials", "App"},
		{"function", "db-function-api-credentials", "db-app-api-credentials", "Function"},
	} {
		t.Run(tc.target, func(t *testing.T) {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
				Name: "blog-test", Labels: map[string]string{"kipper.run/environment": "test"}}}
			ss := newServiceStatefulSet("db", "postgres")
			ss.Namespace = "blog-test"
			creds := newCredentialsSecret("db", map[string][]byte{"NAME": []byte("app")})
			creds.Namespace = "blog-test"
			otherKind := "Function"
			if tc.kind == "Function" {
				otherKind = "App"
			}
			mine := ownedBy(tc.derived, tc.kind, "api")
			theirs := ownedBy(tc.other, otherKind, "api")

			appCR := &kipperv1.App{
				ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "blog-test", UID: types.UID("uid-App")},
				Spec: kipperv1.AppSpec{Image: "api:1", Port: 8080, Replicas: int32Ptr(1),
					ServiceBindings: []kipperv1.ServiceBinding{{Name: "db", Prefix: "DB_", Database: "api_test"}}},
			}
			fnCR := &kipperv1.Function{
				ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "blog-test", UID: types.UID("uid-Function")},
				Spec: kipperv1.FunctionSpec{
					ServiceBindings: []kipperv1.ServiceBinding{{Name: "db", Prefix: "DB_", Database: "api_test"}}},
			}
			crClient := testCRClient(appCR, fnCR, mine, theirs)
			handler := &Services{Client: fake.NewClientset(ns, ss, creds), CRClient: crClient}

			rec := postUnbind(t, handler,
				`{"service":"db","app":"api","namespace":"blog-test","target":"`+tc.target+`"}`)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			ctx := context.Background()

			// The projections are left where they are. The workload's own
			// reconcile retires them, and waits while a retained revision or a
			// live pod still names one — deleting here took an env source away
			// from something that re-reads it on every container restart.
			assert.NoError(t, crClient.Get(ctx, crclient.ObjectKey{Namespace: "blog-test", Name: tc.derived}, &corev1.Secret{}),
				"the projection is retired by the reconcile, not by unbinding (%s)", tc.derived)
			assert.NoError(t, crClient.Get(ctx, crclient.ObjectKey{Namespace: "blog-test", Name: tc.other}, &corev1.Secret{}),
				"and the other kind's is untouched either way (%s)", tc.other)

			// What unbinding does own is which workload loses the binding.
			var app kipperv1.App
			require.NoError(t, crClient.Get(ctx, crclient.ObjectKey{Namespace: "blog-test", Name: "api"}, &app))
			var fn kipperv1.Function
			require.NoError(t, crClient.Get(ctx, crclient.ObjectKey{Namespace: "blog-test", Name: "api"}, &fn))
			if tc.target == "function" {
				assert.Empty(t, fn.Spec.ServiceBindings, "the function loses its binding")
				assert.NotEmpty(t, app.Spec.ServiceBindings, "the same-named app keeps its own")
			} else {
				assert.Empty(t, app.Spec.ServiceBindings, "the app loses its binding")
				assert.NotEmpty(t, fn.Spec.ServiceBindings, "the same-named function keeps its own")
			}
		})
	}
}

// A `database` on a service type that never derives one names a Secret the
// render did not make, so unbinding must not delete whatever is under it. This
// path decided from the database field alone and destroyed it.
func TestUnbind_LeavesASecretTheBindingNeverDerived(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "blog-test", Labels: map[string]string{"kipper.run/environment": "test"}}}
	cache := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "cache", Namespace: "blog-test"},
		Spec:       kipperv1.ServiceSpec{Type: "redis"},
	}
	appCR := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "blog-test", UID: types.UID("uid-api")},
		Spec: kipperv1.AppSpec{Image: "api:1", Port: 8080, Replicas: int32Ptr(1),
			ServiceBindings: []kipperv1.ServiceBinding{{Name: "cache", Prefix: "REDIS_", Database: "2"}}},
	}
	bystander := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cache-app-api-credentials", Namespace: "blog-test"},
		Data:       map[string][]byte{"NOT": []byte("ours")},
	}
	crClient := testCRClient(cache, appCR, bystander)
	handler := &Services{Client: fake.NewClientset(ns), CRClient: crClient}

	rec := postUnbind(t, handler, `{"service":"cache","app":"api","namespace":"blog-test"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var survived corev1.Secret
	require.NoError(t, crClient.Get(context.Background(),
		crclient.ObjectKey{Namespace: "blog-test", Name: "cache-app-api-credentials"}, &survived),
		"redis has no logical namespace, so this binding derived nothing and the name is not ours to delete")
	assert.Equal(t, []byte("ours"), survived.Data["NOT"])

	var stored kipperv1.App
	require.NoError(t, crClient.Get(context.Background(), crclient.ObjectKey{Namespace: "blog-test", Name: "api"}, &stored))
	assert.Empty(t, stored.Spec.ServiceBindings, "the binding still goes")
}

// The injected-env endpoint answers for the same pod the reconciler configures,
// so it must decide which Secret a binding injects the same way. Deciding from
// spec.database alone made it a fourth implementation of that rule: a database
// on a service type with no logical namespace derives nothing, so this looked
// for a Secret nothing renders, found none, and dropped the binding from the
// answer entirely while the pod had its variables.
func TestInjectedEnv_ADatabaseOnATypeWithoutOneStillReportsTheSharedCredentials(t *testing.T) {
	svcCR := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "cache", Namespace: "blog-test"},
		Spec:       kipperv1.ServiceSpec{Type: "redis"},
	}
	appCR := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "blog-test"},
		Spec: kipperv1.AppSpec{
			Image: "myapp:v1", Port: 8080, Replicas: int32Ptr(1),
			// A database on redis: the bind path normalises it away, but a
			// direct CR write, a restore or an older object can carry one.
			ServiceBindings: []kipperv1.ServiceBinding{{Name: "cache", Database: "2"}},
		},
	}
	creds := newCredentialsSecret("cache", map[string][]byte{
		"HOST":     []byte("cache.blog-test.svc"),
		"PORT":     []byte("6379"),
		"PASSWORD": []byte("redispass"),
	})
	creds.Namespace = "blog-test"
	handler := &Services{Client: fake.NewClientset(creds), CRClient: testCRClient(svcCR, appCR)}

	r := chi.NewRouter()
	r.Get("/projects/{name}/apps/{app}/env/injected", handler.InjectedEnv)
	req := httptest.NewRequest(http.MethodGet, "/projects/blog-test/apps/myapp/env/injected", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var vars []injectedVar
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &vars))

	byName := map[string]injectedVar{}
	for _, v := range vars {
		byName[v.Name] = v
	}
	require.Contains(t, byName, "REDIS_HOST", "the binding the pod has must appear in the answer: %v", byName)
	assert.Equal(t, "cache.blog-test.svc", byName["REDIS_HOST"].Value)
	require.Contains(t, byName, "REDIS_PASSWORD")
	assert.True(t, byName["REDIS_PASSWORD"].Secret, "a credential is still reported as one")
	assert.Empty(t, byName["REDIS_PASSWORD"].Value, "and its value is still withheld")
}

// A Service that cannot be read is not the same answer as one that is not
// there. Collapsing them let a transient failure move this endpoint onto a
// Secret name the pod is not using — the reconciler stops on that error rather
// than guessing, so this reports it rather than answering wrong.
func TestInjectedEnv_ReportsATransientServiceLookupFailure(t *testing.T) {
	appCR := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "blog-test"},
		Spec: kipperv1.AppSpec{
			Image: "myapp:v1", Port: 8080, Replicas: int32Ptr(1),
			ServiceBindings: []kipperv1.ServiceBinding{{Name: "cache", Database: "2"}},
		},
	}
	crClient := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(appCR).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
				if _, isService := obj.(*kipperv1.Service); isService {
					return apierrors.NewInternalError(errors.New("etcd unavailable"))
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	handler := &Services{Client: fake.NewClientset(), CRClient: crClient}

	r := chi.NewRouter()
	r.Get("/projects/{name}/apps/{app}/env/injected", handler.InjectedEnv)
	req := httptest.NewRequest(http.MethodGet, "/projects/blog-test/apps/myapp/env/injected", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"a lookup that failed must be reported, not answered as though the service had no type")
}
