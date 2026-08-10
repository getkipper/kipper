package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func testFunction(env map[string]string, deps map[string]string, bindings ...kipperv1.ServiceBinding) *kipperv1.Function {
	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "fn", Namespace: "test"},
		Spec: kipperv1.FunctionSpec{
			Runtime: "node",
			Env:     env,
		},
	}
	if deps != nil {
		fn.Spec.Source = &kipperv1.FunctionSource{Dependencies: deps}
	}
	if len(bindings) > 0 {
		fn.Spec.ServiceBindings = bindings
	}
	return fn
}

func TestFunctionConfig_GetEnv(t *testing.T) {
	t.Run("returns empty map when function missing", func(t *testing.T) {
		h := &FunctionConfig{Client: fake.NewClientset(), CRClient: testCRClient()}
		r := chi.NewRouter()
		r.Get("/projects/{name}/functions/{fn}/env", h.GetEnv)

		req := httptest.NewRequest("GET", "/projects/test/functions/fn/env", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var got map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		if len(got) != 0 {
			t.Errorf("expected empty, got %v", got)
		}
	})

	t.Run("returns env from function CR", func(t *testing.T) {
		fn := testFunction(map[string]string{"REGISTRAR_HOST": "api.registrar.example.com"}, nil)
		h := &FunctionConfig{Client: fake.NewClientset(), CRClient: testCRClient(fn)}
		r := chi.NewRouter()
		r.Get("/projects/{name}/functions/{fn}/env", h.GetEnv)

		req := httptest.NewRequest("GET", "/projects/test/functions/fn/env", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		var got map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		if got["REGISTRAR_HOST"] != "api.registrar.example.com" {
			t.Errorf("got %v", got)
		}
	})
}

func TestFunctionConfig_UpdateEnv(t *testing.T) {
	fn := testFunction(map[string]string{"OLD": "value"}, nil)
	h := &FunctionConfig{Client: fake.NewClientset(), CRClient: testCRClient(fn)}
	r := chi.NewRouter()
	r.Put("/projects/{name}/functions/{fn}/env", h.UpdateEnv)

	body := `{"NEW":"value","REGISTRAR_HOST":"api.registrar.example.com"}`
	req := httptest.NewRequest("PUT", "/projects/test/functions/fn/env", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body %s", rec.Code, rec.Body.String())
	}
	var got map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["NEW"] != "value" || got["REGISTRAR_HOST"] != "api.registrar.example.com" {
		t.Errorf("unexpected response: %v", got)
	}
	if _, exists := got["OLD"]; exists {
		t.Errorf("OLD should have been replaced, got %v", got)
	}
}

func TestFunctionConfig_Secrets(t *testing.T) {
	t.Run("ListSecretKeys returns empty when secret missing", func(t *testing.T) {
		h := &FunctionConfig{Client: fake.NewClientset(), CRClient: testCRClient()}
		r := chi.NewRouter()
		r.Get("/projects/{name}/functions/{fn}/secrets", h.ListSecretKeys)

		req := httptest.NewRequest("GET", "/projects/test/functions/fn/secrets", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		var got []fnSecretKeyInfo
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		if len(got) != 0 {
			t.Errorf("expected empty, got %v", got)
		}
	})

	t.Run("SetSecrets creates secret on first call", func(t *testing.T) {
		client := fake.NewClientset()
		h := &FunctionConfig{Client: client, CRClient: testCRClient()}
		r := chi.NewRouter()
		r.Put("/projects/{name}/functions/{fn}/secrets", h.SetSecrets)

		body := `{"REGISTRAR_API_KEY":"super-secret"}`
		req := httptest.NewRequest("PUT", "/projects/test/functions/fn/secrets", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body %s", rec.Code, rec.Body.String())
		}
		secret, err := client.CoreV1().Secrets("test").Get(req.Context(), "function-fn-secrets", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("expected secret to exist: %v", err)
		}
		if string(secret.Data["REGISTRAR_API_KEY"]) != "super-secret" {
			t.Errorf("expected secret value persisted, got %q", string(secret.Data["REGISTRAR_API_KEY"]))
		}
	})

	t.Run("SetSecrets preserves previous value on rotate", func(t *testing.T) {
		existing := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "function-fn-secrets", Namespace: "test"},
			Data:       map[string][]byte{"REGISTRAR_API_KEY": []byte("old")},
		}
		client := fake.NewClientset(existing)
		h := &FunctionConfig{Client: client, CRClient: testCRClient()}
		r := chi.NewRouter()
		r.Put("/projects/{name}/functions/{fn}/secrets", h.SetSecrets)

		body := `{"REGISTRAR_API_KEY":"new"}`
		req := httptest.NewRequest("PUT", "/projects/test/functions/fn/secrets", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		secret, _ := client.CoreV1().Secrets("test").Get(req.Context(), "function-fn-secrets", metav1.GetOptions{})
		if string(secret.Data["REGISTRAR_API_KEY"]) != "new" {
			t.Errorf("expected new value, got %q", string(secret.Data["REGISTRAR_API_KEY"]))
		}
		if string(secret.Data["REGISTRAR_API_KEY.__previous"]) != "old" {
			t.Errorf("expected previous preserved, got %q", string(secret.Data["REGISTRAR_API_KEY.__previous"]))
		}
	})

	t.Run("ListSecretKeys hides .__previous and flags has_previous", func(t *testing.T) {
		existing := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "function-fn-secrets", Namespace: "test"},
			Data: map[string][]byte{
				"REGISTRAR_API_KEY":            []byte("new"),
				"REGISTRAR_API_KEY.__previous": []byte("old"),
			},
		}
		client := fake.NewClientset(existing)
		h := &FunctionConfig{Client: client, CRClient: testCRClient()}
		r := chi.NewRouter()
		r.Get("/projects/{name}/functions/{fn}/secrets", h.ListSecretKeys)

		req := httptest.NewRequest("GET", "/projects/test/functions/fn/secrets", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		var got []fnSecretKeyInfo
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		if len(got) != 1 {
			t.Fatalf("expected one key, got %v", got)
		}
		if got[0].Key != "REGISTRAR_API_KEY" || !got[0].HasPrevious {
			t.Errorf("unexpected entry: %+v", got[0])
		}
	})

	t.Run("DeleteSecret removes key and previous", func(t *testing.T) {
		existing := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "function-fn-secrets", Namespace: "test"},
			Data: map[string][]byte{
				"REGISTRAR_API_KEY":            []byte("new"),
				"REGISTRAR_API_KEY.__previous": []byte("old"),
				"OTHER":                        []byte("keep"),
			},
		}
		client := fake.NewClientset(existing)
		h := &FunctionConfig{Client: client, CRClient: testCRClient()}
		r := chi.NewRouter()
		r.Delete("/projects/{name}/functions/{fn}/secrets/{key}", h.DeleteSecret)

		req := httptest.NewRequest("DELETE", "/projects/test/functions/fn/secrets/REGISTRAR_API_KEY", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Fatalf("expected 204, got %d; body %s", rec.Code, rec.Body.String())
		}
		secret, _ := client.CoreV1().Secrets("test").Get(req.Context(), "function-fn-secrets", metav1.GetOptions{})
		if _, ok := secret.Data["REGISTRAR_API_KEY"]; ok {
			t.Errorf("expected key gone")
		}
		if _, ok := secret.Data["REGISTRAR_API_KEY.__previous"]; ok {
			t.Errorf("expected previous gone")
		}
		if string(secret.Data["OTHER"]) != "keep" {
			t.Errorf("expected unrelated key preserved")
		}
	})
}

func TestFunctionConfig_Dependencies(t *testing.T) {
	t.Run("UpdateDependencies creates Source if missing", func(t *testing.T) {
		fn := testFunction(nil, nil)
		h := &FunctionConfig{Client: fake.NewClientset(), CRClient: testCRClient(fn)}
		r := chi.NewRouter()
		r.Put("/projects/{name}/functions/{fn}/dependencies", h.UpdateDependencies)

		body := `{"pg":"8.11.5","axios":"1.6.7"}`
		req := httptest.NewRequest("PUT", "/projects/test/functions/fn/dependencies", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body %s", rec.Code, rec.Body.String())
		}
		var got map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		if got["pg"] != "8.11.5" || got["axios"] != "1.6.7" {
			t.Errorf("unexpected response: %v", got)
		}
	})

	t.Run("GetDependencies returns map from CR", func(t *testing.T) {
		fn := testFunction(nil, map[string]string{"pg": "8.11.5"})
		h := &FunctionConfig{Client: fake.NewClientset(), CRClient: testCRClient(fn)}
		r := chi.NewRouter()
		r.Get("/projects/{name}/functions/{fn}/dependencies", h.GetDependencies)

		req := httptest.NewRequest("GET", "/projects/test/functions/fn/dependencies", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		var got map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		if got["pg"] != "8.11.5" {
			t.Errorf("expected pg=8.11.5, got %v", got)
		}
	})
}

func TestFunctionConfig_ListBindings(t *testing.T) {
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "eventdb", Namespace: "test"},
		Spec:       kipperv1.ServiceSpec{Type: "postgres"},
	}
	fn := testFunction(nil, nil, kipperv1.ServiceBinding{
		Name: "eventdb", Prefix: "DB_", Database: "domain_sync_dev",
	})
	h := &FunctionConfig{Client: fake.NewClientset(), CRClient: testCRClient(fn, svc)}
	r := chi.NewRouter()
	r.Get("/projects/{name}/functions/{fn}/bindings", h.ListBindings)

	req := httptest.NewRequest("GET", "/projects/test/functions/fn/bindings", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body %s", rec.Code, rec.Body.String())
	}
	var got []bindingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected one binding, got %v", got)
	}
	b := got[0]
	if b.Service != "eventdb" || b.Type != "postgres" || b.Prefix != "DB_" || b.Database != "domain_sync_dev" {
		t.Errorf("unexpected binding: %+v", b)
	}
	want := []string{"DB_HOST", "DB_PORT", "DB_USERNAME", "DB_PASSWORD", "DB_NAME"}
	if len(b.InjectedEnv) != len(want) {
		t.Fatalf("expected injected env %v, got %v", want, b.InjectedEnv)
	}
	for i, name := range want {
		if b.InjectedEnv[i] != name {
			t.Errorf("injected_env[%d] = %q, want %q", i, b.InjectedEnv[i], name)
		}
	}
}
