package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/console-api/middleware"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
)

func newMinioStatefulSet() *appsv1.StatefulSet {
	replicas := int32(1)
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mystorage",
			Namespace: "default",
			UID:       "uid-mystorage",
			Labels: map[string]string{
				kipperLabel:               kipperValue,
				"app":                     "mystorage",
				"kipper.run/service-type": "minio",
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "mystorage"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "mystorage"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "minio", Image: "minio/minio:RELEASE.2025-09-07T16-13-09Z"},
					},
				},
			},
		},
		Status: appsv1.StatefulSetStatus{
			ReadyReplicas: 1,
		},
	}
}

func newMinioSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mystorage-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"ACCESS_KEY": []byte("kipper"),
			"SECRET_KEY": []byte("testpassword123"),
		},
	}
}

func TestStorage_ServiceNotFound(t *testing.T) {
	client := fake.NewClientset()
	handler := &Storage{Client: client}

	r := chi.NewRouter()
	r.Get("/storage/{service}/buckets", handler.ListBuckets)

	req := httptest.NewRequest("GET", "/storage/nonexistent/buckets?namespace=default", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected status %d, got %d; body: %s", http.StatusNotFound, rec.Code, rec.Body.String())
	}
}

func TestStorage_MissingBucket(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		path    string
		handler func(*Storage) http.HandlerFunc
	}{
		{
			name:   "list objects without bucket param",
			method: "GET",
			path:   "/storage/mystorage/objects?namespace=default",
			handler: func(s *Storage) http.HandlerFunc {
				return s.ListObjects
			},
		},
		{
			name:   "upload without bucket param",
			method: "POST",
			path:   "/storage/mystorage/upload?namespace=default",
			handler: func(s *Storage) http.HandlerFunc {
				return s.Upload
			},
		},
		{
			name:   "download without bucket param",
			method: "GET",
			path:   "/storage/mystorage/download?namespace=default",
			handler: func(s *Storage) http.HandlerFunc {
				return s.Download
			},
		},
		{
			name:   "delete without bucket param",
			method: "DELETE",
			path:   "/storage/mystorage/objects?namespace=default",
			handler: func(s *Storage) http.HandlerFunc {
				return s.DeleteObject
			},
		},
		{
			name:   "share without bucket param",
			method: "POST",
			path:   "/storage/mystorage/share?namespace=default",
			handler: func(s *Storage) http.HandlerFunc {
				return s.Share
			},
		},
		{
			name:   "create folder without bucket param",
			method: "POST",
			path:   "/storage/mystorage/folder?namespace=default",
			handler: func(s *Storage) http.HandlerFunc {
				return s.CreateFolder
			},
		},
		{
			name:   "create folder without prefix param",
			method: "POST",
			path:   "/storage/mystorage/folder?bucket=test&namespace=default",
			handler: func(s *Storage) http.HandlerFunc {
				return s.CreateFolder
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a fake MinIO StatefulSet so the service lookup succeeds,
			// then the missing bucket param should return 400
			ss := newMinioStatefulSet()
			secret := newMinioSecret()
			client := fake.NewClientset(ss, secret)
			handler := &Storage{Client: client}

			r := chi.NewRouter()
			switch tt.method {
			case "GET":
				r.Get("/storage/{service}/*", tt.handler(handler))
			case "POST":
				r.Post("/storage/{service}/*", tt.handler(handler))
			case "DELETE":
				r.Delete("/storage/{service}/*", tt.handler(handler))
			}

			req := httptest.NewRequest(tt.method, tt.path, nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("expected status %d, got %d; body: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestStorage_CreateBucketMissingName(t *testing.T) {
	ss := newMinioStatefulSet()
	secret := newMinioSecret()
	client := fake.NewClientset(ss, secret)
	handler := &Storage{Client: client}

	r := chi.NewRouter()
	r.Post("/storage/{service}/buckets", handler.CreateBucket)

	req := httptest.NewRequest("POST", "/storage/mystorage/buckets?namespace=default", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d; body: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
}

func TestStorage_CreateFolderServiceNotFound(t *testing.T) {
	client := fake.NewClientset()
	handler := &Storage{Client: client}

	r := chi.NewRouter()
	r.Post("/storage/{service}/folder", handler.CreateFolder)

	req := httptest.NewRequest("POST", "/storage/nonexistent/folder?bucket=test&prefix=images&namespace=default", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected status %d, got %d; body: %s", http.StatusNotFound, rec.Code, rec.Body.String())
	}
}

func TestStorage_CreateFolderMissingParams(t *testing.T) {
	ss := newMinioStatefulSet()
	secret := newMinioSecret()
	client := fake.NewClientset(ss, secret)
	handler := &Storage{Client: client}

	r := chi.NewRouter()
	r.Post("/storage/{service}/folder", handler.CreateFolder)

	tests := []struct {
		name string
		path string
	}{
		{"missing both", "/storage/mystorage/folder?namespace=default"},
		{"missing prefix", "/storage/mystorage/folder?bucket=test&namespace=default"},
		{"missing bucket", "/storage/mystorage/folder?prefix=images&namespace=default"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", tt.path, nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("expected status %d, got %d; body: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestPublicObjectKey(t *testing.T) {
	tests := []struct {
		bucket string
		key    string
	}{
		{"uploads", "images/photo.jpg"},
		{"test", "path/with spaces/file.txt"},
		{"data", "special-chars/file+name(1).pdf"},
	}

	for _, tt := range tests {
		t.Run(tt.bucket+"/"+tt.key, func(t *testing.T) {
			result := publicObjectKey(tt.bucket, tt.key)
			assert.NotEmpty(t, result)
			// Must be deterministic
			assert.Equal(t, result, publicObjectKey(tt.bucket, tt.key))
			// Different inputs produce different keys
			assert.NotEqual(t, result, publicObjectKey(tt.bucket, "other-key"))
		})
	}
}

func TestPublicObjectsCMName(t *testing.T) {
	assert.Equal(t, "kipper-public-objects-mystorage", publicObjectsCMName("mystorage"))
	assert.Equal(t, "kipper-public-objects-media", publicObjectsCMName("media"))
}

func TestSignAndVerifyShareToken(t *testing.T) {
	shareSecret = []byte("test-secret-key-for-signing")

	token := signShareToken("proj-acme", "uid-1", "mystorage", "uploads", "images/photo.jpg", 1234567890)
	assert.NotEmpty(t, token)

	t.Run("valid token passes verification", func(t *testing.T) {
		assert.True(t, verifyShareToken("proj-acme", "uid-1", "mystorage", "uploads", "images/photo.jpg", 1234567890, token))
	})

	t.Run("wrong namespace fails verification", func(t *testing.T) {
		// The load-bearing property: another tenant's namespace can't redeem it.
		assert.False(t, verifyShareToken("proj-evil", "uid-1", "mystorage", "uploads", "images/photo.jpg", 1234567890, token))
	})

	t.Run("wrong uid fails verification", func(t *testing.T) {
		// A deleted-and-recreated service (new UID) can't redeem it.
		assert.False(t, verifyShareToken("proj-acme", "uid-2", "mystorage", "uploads", "images/photo.jpg", 1234567890, token))
	})

	t.Run("wrong service fails verification", func(t *testing.T) {
		assert.False(t, verifyShareToken("proj-acme", "uid-1", "other", "uploads", "images/photo.jpg", 1234567890, token))
	})

	t.Run("wrong bucket fails verification", func(t *testing.T) {
		assert.False(t, verifyShareToken("proj-acme", "uid-1", "mystorage", "other", "images/photo.jpg", 1234567890, token))
	})

	t.Run("wrong key fails verification", func(t *testing.T) {
		assert.False(t, verifyShareToken("proj-acme", "uid-1", "mystorage", "uploads", "other.jpg", 1234567890, token))
	})

	t.Run("wrong expiry fails verification", func(t *testing.T) {
		assert.False(t, verifyShareToken("proj-acme", "uid-1", "mystorage", "uploads", "images/photo.jpg", 9999999999, token))
	})

	t.Run("tampered token fails verification", func(t *testing.T) {
		assert.False(t, verifyShareToken("proj-acme", "uid-1", "mystorage", "uploads", "images/photo.jpg", 1234567890, "badtoken"))
	})

	t.Run("deterministic output", func(t *testing.T) {
		token2 := signShareToken("proj-acme", "uid-1", "mystorage", "uploads", "images/photo.jpg", 1234567890)
		assert.Equal(t, token, token2)
	})
}

func TestInitShareSecret_CreatesNew(t *testing.T) {
	shareSecret = nil
	client := fake.NewClientset()

	InitShareSecret(client)

	assert.NotNil(t, shareSecret)
	assert.Len(t, shareSecret, 32)

	// Verify the secret was persisted in Kubernetes
	secret, err := client.CoreV1().Secrets("kipper-system").Get(
		t.Context(), "kipper-share-secret", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, shareSecret, secret.Data["signing-key"])
}

func TestInitShareSecret_LoadsExisting(t *testing.T) {
	shareSecret = nil
	existingKey := []byte("pre-existing-signing-key-value!!")

	client := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kipper-share-secret",
			Namespace: "kipper-system",
		},
		Data: map[string][]byte{
			"signing-key": existingKey,
		},
	})

	InitShareSecret(client)

	assert.Equal(t, existingKey, shareSecret)
}

func TestStorage_MakePublic(t *testing.T) {
	ss := newMinioStatefulSet()
	secret := newMinioSecret()
	client := fake.NewClientset(ss, secret)
	handler := &Storage{Client: client}

	r := chi.NewRouter()
	r.Put("/storage/{service}/public", handler.MakePublic)

	req := httptest.NewRequest("PUT", "/storage/mystorage/public?bucket=uploads&key=images/photo.jpg&namespace=default", nil)
	req.Host = "console.example.com"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "public", resp["status"])
	assert.Contains(t, resp["url"], "/api/v1/storage/mystorage/public/uploads/images/photo.jpg")
	assert.Contains(t, resp["url"], "namespace=default", "the public URL must carry the namespace")

	// Verify ConfigMap was created
	cm, err := client.CoreV1().ConfigMaps("default").Get(
		t.Context(), publicObjectsCMName("mystorage"), metav1.GetOptions{})
	require.NoError(t, err)
	cmKey := publicObjectKey("uploads", "images/photo.jpg")
	assert.Equal(t, "true", cm.Data[cmKey])
}

func TestStorage_MakePublic_MissingParams(t *testing.T) {
	ss := newMinioStatefulSet()
	secret := newMinioSecret()
	client := fake.NewClientset(ss, secret)
	handler := &Storage{Client: client}

	r := chi.NewRouter()
	r.Put("/storage/{service}/public", handler.MakePublic)

	tests := []struct {
		name string
		path string
	}{
		{"missing both", "/storage/mystorage/public?namespace=default"},
		{"missing key", "/storage/mystorage/public?bucket=uploads&namespace=default"},
		{"missing bucket", "/storage/mystorage/public?key=photo.jpg&namespace=default"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("PUT", tt.path, nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
}

// TestStorage_MakePublic_ServiceNotFound proves a public-flag write against a
// namespace with no such MinIO service 404s and creates no dangling registry.
func TestStorage_MakePublic_ServiceNotFound(t *testing.T) {
	client := fake.NewClientset()
	handler := &Storage{Client: client}

	r := chi.NewRouter()
	r.Put("/storage/{service}/public", handler.MakePublic)

	req := httptest.NewRequest("PUT", "/storage/mystorage/public?bucket=uploads&key=photo.jpg&namespace=default", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)

	_, err := client.CoreV1().ConfigMaps("default").Get(
		t.Context(), publicObjectsCMName("mystorage"), metav1.GetOptions{})
	assert.Error(t, err, "no dangling public-objects ConfigMap must be created for an absent service")
}

func TestStorage_MakePrivate(t *testing.T) {
	cmKey := publicObjectKey("uploads", "images/photo.jpg")
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      publicObjectsCMName("mystorage"),
			Namespace: "default",
		},
		Data: map[string]string{cmKey: "true"},
	}
	client := fake.NewClientset(cm, newMinioStatefulSet())
	handler := &Storage{Client: client}

	r := chi.NewRouter()
	r.Delete("/storage/{service}/public", handler.MakePrivate)

	req := httptest.NewRequest("DELETE", "/storage/mystorage/public?bucket=uploads&key=images/photo.jpg&namespace=default", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "private", resp["status"])

	// Verify the key was removed from the ConfigMap
	updated, err := client.CoreV1().ConfigMaps("default").Get(
		t.Context(), publicObjectsCMName("mystorage"), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Empty(t, updated.Data[cmKey])
}

func TestStorage_MakePrivate_NoConfigMap(t *testing.T) {
	client := fake.NewClientset(newMinioStatefulSet())
	handler := &Storage{Client: client}

	r := chi.NewRouter()
	r.Delete("/storage/{service}/public", handler.MakePrivate)

	req := httptest.NewRequest("DELETE", "/storage/mystorage/public?bucket=uploads&key=images/photo.jpg&namespace=default", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "private", resp["status"])
}

func TestStorage_IsPublic(t *testing.T) {
	cmKey := publicObjectKey("uploads", "images/photo.jpg")
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      publicObjectsCMName("mystorage"),
			Namespace: "default",
		},
		Data: map[string]string{cmKey: "true"},
	}
	client := fake.NewClientset(cm, newMinioStatefulSet())
	handler := &Storage{Client: client}

	r := chi.NewRouter()
	r.Get("/storage/{service}/public", handler.IsPublic)

	t.Run("public object", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/storage/mystorage/public?bucket=uploads&key=images/photo.jpg&namespace=default", nil)
		req.Host = "console.example.com"
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var resp map[string]interface{}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, true, resp["public"])
		assert.Contains(t, resp["url"], "/api/v1/storage/mystorage/public/uploads/images/photo.jpg")
		assert.Contains(t, resp["url"], "namespace=default")
	})

	t.Run("private object", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/storage/mystorage/public?bucket=uploads&key=other.jpg&namespace=default", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var resp map[string]interface{}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, false, resp["public"])
		assert.Nil(t, resp["url"])
	})
}

func TestStorage_SharedDownload_Expired(t *testing.T) {
	shareSecret = []byte("test-secret-key-for-signing")

	client := fake.NewClientset()
	handler := &Storage{Client: client}

	r := chi.NewRouter()
	r.Get("/storage/{service}/shared", handler.SharedDownload)

	expired := time.Now().Add(-1 * time.Hour).Unix()
	token := signShareToken("default", "uid-mystorage", "mystorage", "uploads", "photo.jpg", expired)

	path := fmt.Sprintf("/storage/mystorage/shared?namespace=default&bucket=uploads&key=photo.jpg&expires=%d&token=%s", expired, token)
	req := httptest.NewRequest("GET", path, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusGone, rec.Code)
}

func TestStorage_SharedDownload_InvalidToken(t *testing.T) {
	shareSecret = []byte("test-secret-key-for-signing")

	// The service must exist so resolution succeeds and the request reaches the
	// token check rather than a not-found.
	client := fake.NewClientset(newMinioStatefulSet(), newMinioSecret())
	handler := &Storage{Client: client}

	r := chi.NewRouter()
	r.Get("/storage/{service}/shared", handler.SharedDownload)

	future := time.Now().Add(1 * time.Hour).Unix()
	path := fmt.Sprintf("/storage/mystorage/shared?namespace=default&bucket=uploads&key=photo.jpg&expires=%d&token=forged", future)
	req := httptest.NewRequest("GET", path, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestStorage_SharedDownload_CrossTenantAliasingRefused(t *testing.T) {
	// The core B11 property: a link minted for one tenant's service can't be
	// redeemed against another tenant's same-named service. Two projects each
	// run a service named "mystorage" in their own namespace; a token minted
	// for tenant A must fail when its namespace is swapped to tenant B.
	shareSecret = []byte("test-secret-key-for-signing")

	tenantB := newMinioStatefulSet()
	tenantB.Namespace = "project-b"
	tenantB.UID = "uid-tenant-b"
	secretB := newMinioSecret()
	secretB.Namespace = "project-b"
	client := fake.NewClientset(newMinioStatefulSet(), newMinioSecret(), tenantB, secretB)
	handler := &Storage{Client: client}

	r := chi.NewRouter()
	r.Get("/storage/{service}/shared", handler.SharedDownload)

	// A valid token minted for tenant A (namespace "default", uid-mystorage).
	future := time.Now().Add(1 * time.Hour).Unix()
	token := signShareToken("default", "uid-mystorage", "mystorage", "uploads", "photo.jpg", future)

	// Redeeming it against tenant B's namespace must be refused: B's UID does
	// not reproduce the signature.
	path := fmt.Sprintf("/storage/mystorage/shared?namespace=project-b&bucket=uploads&key=photo.jpg&expires=%d&token=%s", future, token)
	req := httptest.NewRequest("GET", path, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code, "a token minted for tenant A must not redeem against tenant B")
}

func TestStorage_Share_MintsTokenBoundToResolvedIdentity(t *testing.T) {
	// Minting resolves the service in the authorized namespace and signs the same
	// namespace/UID. The proof: the token in the returned URL verifies against the
	// service's actual namespace and UID, so the mint can't sign a different
	// tenant's instance than the one it authorized.
	shareSecret = []byte("test-secret-key-for-signing")
	client := fake.NewClientset(newMinioStatefulSet(), newMinioSecret())
	handler := &Storage{Client: client}

	r := chi.NewRouter()
	r.Post("/storage/{service}/share", handler.Share)

	req := httptest.NewRequest("POST", "/storage/mystorage/share?bucket=uploads&key=images/photo.jpg&expires=1h&namespace=default", nil)
	req.Host = "console.example.com"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp shareResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	u, err := url.Parse(resp.URL)
	require.NoError(t, err)
	q := u.Query()

	assert.Equal(t, "default", q.Get("namespace"), "the link must pin the resolved namespace")
	expires, err := strconv.ParseInt(q.Get("expires"), 10, 64)
	require.NoError(t, err)
	assert.True(t,
		verifyShareToken("default", "uid-mystorage", "mystorage", "uploads", "images/photo.jpg", expires, q.Get("token")),
		"the minted token must be signed with the resolved namespace and UID")
}

func TestStorage_IsObjectPublic_PerNamespace(t *testing.T) {
	// A public flag lives in the service's own namespace, so a flag set by one
	// tenant is invisible when the same service name is checked in another.
	cmKey := publicObjectKey("uploads", "x.jpg")
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: publicObjectsCMName("assets"), Namespace: "project-a"},
		Data:       map[string]string{cmKey: "true"},
	}
	handler := &Storage{Client: fake.NewClientset(cm)}
	ctx := context.Background()

	assert.True(t, handler.isObjectPublic(ctx, "assets", "project-a", "uploads", "x.jpg"),
		"the flag must be visible in the namespace it was set in")
	assert.False(t, handler.isObjectPublic(ctx, "assets", "project-b", "uploads", "x.jpg"),
		"another tenant's same-named service must not see the flag")
}

// TestStorage_GetMinioStatefulSet_ExactNamespace proves resolution is an exact
// per-namespace lookup, never a first cluster-wide match, and rejects a
// same-named workload that is not a MinIO service.
func TestStorage_GetMinioStatefulSet_ExactNamespace(t *testing.T) {
	ssA := newMinioStatefulSet() // "default", uid-mystorage
	ssB := newMinioStatefulSet()
	ssB.Namespace = "project-b"
	ssB.UID = "uid-tenant-b"
	// A same-named workload without the minio label must be rejected.
	plain := newMinioStatefulSet()
	plain.Namespace = "project-c"
	plain.UID = "uid-plain"
	plain.Labels = map[string]string{"app": "mystorage"}

	handler := &Storage{Client: fake.NewClientset(ssA, ssB, plain)}
	ctx := context.Background()

	uid, err := handler.getMinioStatefulSet(ctx, "default", "mystorage")
	assert.NoError(t, err)
	assert.Equal(t, "uid-mystorage", uid)

	uid, err = handler.getMinioStatefulSet(ctx, "project-b", "mystorage")
	assert.NoError(t, err, "the exact namespace resolves, not the first match")
	assert.Equal(t, "uid-tenant-b", uid, "resolution must return the instance in the requested namespace")

	_, err = handler.getMinioStatefulSet(ctx, "", "mystorage")
	assert.ErrorIs(t, err, errStorageServiceNotFound, "an empty namespace never resolves")

	_, err = handler.getMinioStatefulSet(ctx, "nonexistent", "mystorage")
	assert.ErrorIs(t, err, errStorageServiceNotFound, "a namespace without the service never resolves")

	_, err = handler.getMinioStatefulSet(ctx, "project-c", "mystorage")
	assert.ErrorIs(t, err, errStorageServiceNotFound, "a same-named workload without the minio label is rejected")
}

// TestStorage_PublicDownload_NamespaceScoped proves the unauthenticated public
// route cannot be pointed at another tenant's namespace to serve their object.
func TestStorage_PublicDownload_NamespaceScoped(t *testing.T) {
	// An object is public only in tenant A (default). A same-named service exists
	// in tenant B, which never flagged this object public.
	cmKey := publicObjectKey("uploads", "photo.jpg")
	cmA := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: publicObjectsCMName("mystorage"), Namespace: "default"},
		Data:       map[string]string{cmKey: "true"},
	}
	ssB := newMinioStatefulSet()
	ssB.Namespace = "project-b"
	ssB.UID = "uid-tenant-b"
	client := fake.NewClientset(cmA, newMinioStatefulSet(), newMinioSecret(), ssB)
	handler := &Storage{Client: client}

	r := chi.NewRouter()
	r.Get("/api/v1/storage/{service}/public/{bucket}/*", handler.PublicDownload)

	t.Run("missing namespace is rejected", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/storage/mystorage/public/uploads/photo.jpg", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("attacker namespace cannot serve the victim's public object", func(t *testing.T) {
		// project-b runs the same-named service but never flagged this object.
		req := httptest.NewRequest("GET", "/api/v1/storage/mystorage/public/uploads/photo.jpg?namespace=project-b", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusForbidden, rec.Code, "the public flag is checked in the requested namespace only")
	})

	t.Run("service absent in the namespace is not found", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/storage/mystorage/public/uploads/photo.jpg?namespace=nonexistent", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})
}

func TestStorage_SharedDownload_MissingParams(t *testing.T) {
	client := fake.NewClientset()
	handler := &Storage{Client: client}

	r := chi.NewRouter()
	r.Get("/storage/{service}/shared", handler.SharedDownload)

	tests := []struct {
		name string
		path string
	}{
		{"missing all", "/storage/mystorage/shared"},
		{"missing namespace", "/storage/mystorage/shared?bucket=b&key=k&expires=123&token=t"},
		{"missing token", "/storage/mystorage/shared?namespace=default&bucket=b&key=k&expires=123"},
		{"missing expires", "/storage/mystorage/shared?namespace=default&bucket=b&key=k&token=t"},
		{"missing key", "/storage/mystorage/shared?namespace=default&bucket=b&expires=123&token=t"},
		{"missing bucket", "/storage/mystorage/shared?namespace=default&key=k&expires=123&token=t"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
}

func TestStorage_SharedDownload_InvalidExpires(t *testing.T) {
	client := fake.NewClientset()
	handler := &Storage{Client: client}

	r := chi.NewRouter()
	r.Get("/storage/{service}/shared", handler.SharedDownload)

	req := httptest.NewRequest("GET", "/storage/mystorage/shared?namespace=default&bucket=b&key=k&expires=notanumber&token=t", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestStorage_MakePublicThenPrivateRoundtrip(t *testing.T) {
	ss := newMinioStatefulSet()
	secret := newMinioSecret()
	client := fake.NewClientset(ss, secret)
	handler := &Storage{Client: client}

	r := chi.NewRouter()
	r.Put("/storage/{service}/public", handler.MakePublic)
	r.Delete("/storage/{service}/public", handler.MakePrivate)
	r.Get("/storage/{service}/public", handler.IsPublic)

	// Make public
	req := httptest.NewRequest("PUT", "/storage/mystorage/public?bucket=test&key=doc.pdf&namespace=default", nil)
	req.Host = "console.example.com"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)

	// Verify public
	req = httptest.NewRequest("GET", "/storage/mystorage/public?bucket=test&key=doc.pdf&namespace=default", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	var checkResp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &checkResp))
	assert.Equal(t, true, checkResp["public"])

	// Make private
	req = httptest.NewRequest("DELETE", "/storage/mystorage/public?bucket=test&key=doc.pdf&namespace=default", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)

	// Verify private
	req = httptest.NewRequest("GET", "/storage/mystorage/public?bucket=test&key=doc.pdf&namespace=default", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &checkResp))
	assert.Equal(t, false, checkResp["public"])
}

// stubProjectMembers is an in-memory ProjectMemberSource for the route-level
// authorization test.
type stubProjectMembers map[string]map[string]string

func (s stubProjectMembers) ProjectMembers(_ context.Context, project string) (map[string]string, bool, error) {
	m, ok := s[project]
	return m, ok, nil
}

// TestStorage_RoutesEnforceAuthzBeforeAccess mounts the storage handlers behind
// the same ProjectScopeQuery / RequireProjectRole wrappers used in main.go and
// proves the plan's release-blocking invariant: an omitted namespace, a
// non-member namespace, and a viewer on a write route are all denied before the
// handler resolves any service. (ProjectScopeQuery's deny-before-handler
// property is proven in the middleware package; this asserts the storage routes
// actually compose it.)
func TestStorage_RoutesEnforceAuthzBeforeAccess(t *testing.T) {
	ns := func(name, project string) *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: name, Labels: map[string]string{kipperlabels.Project: project}}}
	}
	usersCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-users", Namespace: "kipper-system"},
		Data:       map[string]string{"users": `{"dev@test.com":"deployer","viewer@test.com":"viewer","root@test.com":"admin"}`},
	}
	// The MinIO service lives in blog; IsPublic reads a ConfigMap (no live MinIO
	// needed), so the reach-the-handler case returns a clean 200.
	ss := newMinioStatefulSet()
	ss.Namespace = "blog"
	client := fake.NewClientset(
		ns("blog", "blog"), ns("shop", "shop"),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kipper-system"}},
		usersCM, ss)

	resolver := middleware.NewProjectAccessResolver(client, middleware.NewRoleStore(client), stubProjectMembers{
		"blog": {"dev@test.com": middleware.ProjectRoleDeployer, "viewer@test.com": middleware.ProjectRoleViewer},
	}, handlerOwners(t, ns("blog", "blog"), ns("shop", "shop")))
	qscope := middleware.ProjectScopeQuery(resolver)
	nsRead := func(h http.HandlerFunc) http.HandlerFunc { return qscope(h).ServeHTTP }
	nsDeployer := func(h http.HandlerFunc) http.HandlerFunc {
		return qscope(middleware.RequireProjectRole(middleware.ProjectRoleDeployer)(h)).ServeHTTP
	}

	handler := &Storage{Client: client}
	r := chi.NewRouter()
	r.Get("/storage/{service}/public", nsRead(handler.IsPublic))
	r.Put("/storage/{service}/public", nsDeployer(handler.MakePublic))

	withUser := func(method, target, email string) *http.Request {
		req := httptest.NewRequest(method, target, nil)
		ctx := context.WithValue(req.Context(), middleware.UserContextKey, &middleware.Claims{Email: email})
		return req.WithContext(ctx)
	}

	tests := []struct {
		name string
		req  *http.Request
		want int
	}{
		{"omitted namespace is rejected before the handler",
			withUser("GET", "/storage/mystorage/public?bucket=b&key=k", "dev@test.com"), http.StatusBadRequest},
		{"non-member namespace is forbidden",
			withUser("GET", "/storage/mystorage/public?bucket=b&key=k&namespace=shop", "dev@test.com"), http.StatusForbidden},
		{"viewer cannot write",
			withUser("PUT", "/storage/mystorage/public?bucket=b&key=k&namespace=blog", "viewer@test.com"), http.StatusForbidden},
		{"member reaches the handler",
			withUser("GET", "/storage/mystorage/public?bucket=b&key=k&namespace=blog", "dev@test.com"), http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, tt.req)
			assert.Equal(t, tt.want, rec.Code, rec.Body.String())
		})
	}
}
