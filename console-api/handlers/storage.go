package handlers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/controller/pkg/secretname"
)

// Storage provides handlers for browsing MinIO S3 buckets.
type Storage struct {
	Client kubernetes.Interface
}

type bucketResponse struct {
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

type objectResponse struct {
	Key          string `json:"key"`
	Size         int64  `json:"size"`
	LastModified string `json:"last_modified"`
	IsDir        bool   `json:"is_dir"`
	IsPublic     bool   `json:"is_public,omitempty"`
}

type objectsListResponse struct {
	Objects []objectResponse `json:"objects"`
	Prefix  string           `json:"prefix"`
	Bucket  string           `json:"bucket"`
}

type createBucketRequest struct {
	Name string `json:"name"`
}

type shareResponse struct {
	URL     string `json:"url"`
	Expires string `json:"expires"`
}

// errStorageServiceNotFound marks a resolution miss — an empty namespace or
// service, no such StatefulSet, or a non-MinIO workload — so callers return 404.
// A real Kubernetes API failure surfaces as a distinct wrapped error for a 500,
// so an API-server outage is not reported to the caller as a missing service.
var errStorageServiceNotFound = errors.New("minio storage service not found")

// getMinioStatefulSet resolves a MinIO service to its immutable StatefulSet UID
// by an EXACT lookup in the given namespace. The namespace is always supplied by
// the caller — the ?namespace= query the ProjectScopeQuery middleware authorized
// on the authenticated routes, or the explicit request parameter on the
// unauthenticated public/shared routes — so storage never guesses a namespace by
// a cluster-wide name search, and a service name that collides across tenants can
// never resolve to another tenant's namespace.
//
// It queries AppsV1().StatefulSets (never CoreV1().Services), so the target must
// be a real workload physically residing in that namespace: an ExternalName
// Service cannot alias into another tenant's network path. The
// kipper.run/service-type=minio label check rejects an unrelated same-named
// workload. Returns errStorageServiceNotFound on a miss and a wrapped error on a
// Kubernetes API failure — never a fallback.
func (s *Storage) getMinioStatefulSet(ctx context.Context, namespace, service string) (uid string, err error) {
	if namespace == "" || service == "" {
		return "", errStorageServiceNotFound
	}
	ss, err := s.Client.AppsV1().StatefulSets(namespace).Get(ctx, service, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", errStorageServiceNotFound
		}
		return "", fmt.Errorf("looking up minio service %q in %q: %w", service, namespace, err)
	}
	if ss.Labels["kipper.run/service-type"] != "minio" {
		return "", errStorageServiceNotFound
	}
	return string(ss.UID), nil
}

// respondServiceLookupError maps a getMinioStatefulSet error to a status code: a
// resolution miss is 404, a Kubernetes API failure is 500.
func respondServiceLookupError(w http.ResponseWriter, err error, service string) {
	if errors.Is(err, errStorageServiceNotFound) {
		respondError(w, http.StatusNotFound, fmt.Sprintf("storage service %q not found", service))
		return
	}
	respondError(w, http.StatusInternalServerError, "failed to locate storage service")
}

// publicFlagNamespace returns the ?namespace= the middleware authorized and
// confirms the named MinIO service exists in it, so a public-flag write cannot
// create a dangling public-objects registry for a service that is absent or not
// MinIO. Responds 404 and returns false on a miss.
func (s *Storage) publicFlagNamespace(w http.ResponseWriter, r *http.Request) (namespace string, ok bool) {
	service := chi.URLParam(r, "service")
	namespace = r.URL.Query().Get("namespace")
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if _, err := s.getMinioStatefulSet(ctx, namespace, service); err != nil {
		respondServiceLookupError(w, err, service)
		return "", false
	}
	return namespace, true
}

// minioClientForNamespace builds a client for the service in an exact
// namespace, so the unauthenticated share path serves from the namespace its
// token was pinned to rather than a first cluster-wide match.
func (s *Storage) minioClientForNamespace(ctx context.Context, service, namespace string) (*minio.Client, error) {
	secret, err := s.Client.CoreV1().Secrets(namespace).Get(ctx, secretname.ServiceCredentials(service), metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading credentials: %w", err)
	}

	endpoint := fmt.Sprintf("%s.%s.svc.cluster.local:9000", service, namespace)
	accessKey := string(secret.Data["ACCESS_KEY"])
	secretKey := string(secret.Data["SECRET_KEY"])

	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: false,
	})
	if err != nil {
		return nil, fmt.Errorf("creating minio client: %w", err)
	}

	return client, nil
}

// resolveClient resolves one concrete service instance and builds its MinIO
// client, returning the (namespace, uid). The namespace is the ?namespace= value
// the ProjectScopeQuery middleware already authorized for this caller on the
// route (nsRead / nsDeployer), used unchanged for the exact StatefulSet lookup,
// the credential Secret, and any share token — so authorization can never split
// from the object served, and there is no cluster-wide fallback.
func (s *Storage) resolveClient(w http.ResponseWriter, r *http.Request) (client *minio.Client, namespace, uid string, ok bool) {
	service := chi.URLParam(r, "service")
	namespace = r.URL.Query().Get("namespace")
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	uid, err := s.getMinioStatefulSet(ctx, namespace, service)
	if err != nil {
		respondServiceLookupError(w, err, service)
		return nil, "", "", false
	}

	client, err = s.minioClientForNamespace(ctx, service, namespace)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to connect to storage service")
		return nil, "", "", false
	}
	return client, namespace, uid, true
}

// ListBuckets returns all buckets for a MinIO service.
// GET /api/v1/storage/{service}/buckets
func (s *Storage) ListBuckets(w http.ResponseWriter, r *http.Request) {
	client, _, _, ok := s.resolveClient(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	buckets, err := client.ListBuckets(ctx)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list buckets")
		return
	}

	result := make([]bucketResponse, 0, len(buckets))
	for _, b := range buckets {
		result = append(result, bucketResponse{
			Name:      b.Name,
			CreatedAt: b.CreationDate.Format(time.RFC3339),
		})
	}

	respondJSON(w, http.StatusOK, result)
}

// CreateBucket creates a new bucket.
// POST /api/v1/storage/{service}/buckets
func (s *Storage) CreateBucket(w http.ResponseWriter, r *http.Request) {
	client, _, _, ok := s.resolveClient(w, r)
	if !ok {
		return
	}

	var req createBucketRequest
	if err := decodeJSON(r, &req); err != nil || req.Name == "" {
		respondError(w, http.StatusBadRequest, "name is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if err := client.MakeBucket(ctx, req.Name, minio.MakeBucketOptions{}); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to create bucket: %v", err))
		return
	}

	respondJSON(w, http.StatusCreated, map[string]string{"name": req.Name})
}

// ListObjects lists objects in a bucket with optional prefix.
// GET /api/v1/storage/{service}/objects?bucket=uploads&prefix=images/
func (s *Storage) ListObjects(w http.ResponseWriter, r *http.Request) {
	client, namespace, _, ok := s.resolveClient(w, r)
	if !ok {
		return
	}

	bucket := r.URL.Query().Get("bucket")
	if bucket == "" {
		respondError(w, http.StatusBadRequest, "bucket query parameter is required")
		return
	}

	service := chi.URLParam(r, "service")
	prefix := r.URL.Query().Get("prefix")

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Load the public-objects registry from the SAME namespace the client was
	// built from, so the object list and its is_public flags always describe
	// one service instance — never a different duplicate on a re-resolution.
	publicMap := make(map[string]bool)
	{
		if cm, err := s.Client.CoreV1().ConfigMaps(namespace).Get(ctx, publicObjectsCMName(service), metav1.GetOptions{}); err == nil {
			for k, v := range cm.Data {
				if v == "true" {
					publicMap[k] = true
				}
			}
		}
	}

	objects := make([]objectResponse, 0)
	for obj := range client.ListObjects(ctx, bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: false,
	}) {
		if obj.Err != nil {
			respondError(w, http.StatusInternalServerError, "failed to list objects")
			return
		}

		// Skip the folder marker object for the current prefix
		if obj.Key == prefix {
			continue
		}

		isDir := obj.Size == 0 && len(obj.Key) > 0 && obj.Key[len(obj.Key)-1] == '/'
		lastMod := ""
		if !obj.LastModified.IsZero() {
			lastMod = obj.LastModified.Format(time.RFC3339)
		}

		objects = append(objects, objectResponse{
			Key:          obj.Key,
			Size:         obj.Size,
			LastModified: lastMod,
			IsDir:        isDir,
			IsPublic:     publicMap[publicObjectKey(bucket, obj.Key)],
		})
	}

	respondJSON(w, http.StatusOK, objectsListResponse{
		Objects: objects,
		Prefix:  prefix,
		Bucket:  bucket,
	})
}

// Upload handles multipart file upload.
// POST /api/v1/storage/{service}/upload?bucket=uploads&key=images/photo.jpg
func (s *Storage) Upload(w http.ResponseWriter, r *http.Request) {
	client, _, _, ok := s.resolveClient(w, r)
	if !ok {
		return
	}

	bucket := r.URL.Query().Get("bucket")
	key := r.URL.Query().Get("key")
	if bucket == "" || key == "" {
		respondError(w, http.StatusBadRequest, "bucket and key query parameters are required")
		return
	}

	// 100 MB limit
	r.Body = http.MaxBytesReader(w, r.Body, 100<<20)
	if err := r.ParseMultipartForm(100 << 20); err != nil { //nolint:gosec // body capped to 100 MiB by MaxBytesReader above
		respondError(w, http.StatusBadRequest, "invalid multipart form data")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		respondError(w, http.StatusBadRequest, "file field is required")
		return
	}
	defer func() { _ = file.Close() }()

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	_, err = client.PutObject(ctx, bucket, key, file, header.Size, minio.PutObjectOptions{
		ContentType: header.Header.Get("Content-Type"),
	})
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to upload file")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"key": key, "bucket": bucket})
}

// CreateFolder creates a zero-byte object with a trailing / to represent a folder.
// POST /api/v1/storage/{service}/folder?bucket=uploads&prefix=images/
func (s *Storage) CreateFolder(w http.ResponseWriter, r *http.Request) {
	client, _, _, ok := s.resolveClient(w, r)
	if !ok {
		return
	}

	bucket := r.URL.Query().Get("bucket")
	prefix := r.URL.Query().Get("prefix")
	if bucket == "" || prefix == "" {
		respondError(w, http.StatusBadRequest, "bucket and prefix are required")
		return
	}

	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	_, err := client.PutObject(ctx, bucket, prefix, bytes.NewReader(nil), 0, minio.PutObjectOptions{})
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to create folder: %v", err))
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"prefix": prefix, "bucket": bucket})
}

// Download proxies a file download from MinIO.
// GET /api/v1/storage/{service}/download?bucket=uploads&key=images/photo.jpg
func (s *Storage) Download(w http.ResponseWriter, r *http.Request) {
	client, _, _, ok := s.resolveClient(w, r)
	if !ok {
		return
	}

	bucket := r.URL.Query().Get("bucket")
	key := r.URL.Query().Get("key")
	if bucket == "" || key == "" {
		respondError(w, http.StatusBadRequest, "bucket and key query parameters are required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	obj, err := client.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to download file")
		return
	}
	defer func() { _ = obj.Close() }()

	info, err := obj.Stat()
	if err != nil {
		respondError(w, http.StatusNotFound, "file not found")
		return
	}

	w.Header().Set("Content-Type", info.ContentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", key))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size))

	if _, err := io.Copy(w, obj); err != nil {
		// Headers already sent, nothing useful we can do
		return
	}
}

// DeleteObject removes a single object from a bucket.
// DELETE /api/v1/storage/{service}/objects?bucket=uploads&key=images/photo.jpg
func (s *Storage) DeleteObject(w http.ResponseWriter, r *http.Request) {
	client, _, _, ok := s.resolveClient(w, r)
	if !ok {
		return
	}

	bucket := r.URL.Query().Get("bucket")
	key := r.URL.Query().Get("key")
	if bucket == "" || key == "" {
		respondError(w, http.StatusBadRequest, "bucket and key query parameters are required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if err := client.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{}); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to delete object")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// shareSecret is used to sign share link tokens. Loaded from a Kubernetes
// Secret on startup so links survive process restarts.
var shareSecret []byte

// InitShareSecret loads or creates the share signing key from a Kubernetes Secret.
func InitShareSecret(client kubernetes.Interface) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const secretName = "kipper-share-secret"
	const namespace = "kipper-system"
	const key = "signing-key"

	secret, err := client.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err == nil {
		shareSecret = secret.Data[key]
		return
	}

	// Create a new random secret
	b := make([]byte, 32)
	_, _ = rand.Read(b)

	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			key: b,
		},
	}
	if _, err := client.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		// Another replica may have created it concurrently
		if existing, getErr := client.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{}); getErr == nil {
			shareSecret = existing.Data[key]
			return
		}
	}
	shareSecret = b
}

// signShareToken binds a share link to a specific service instance: the
// namespace and the StatefulSet UID are part of the signed payload, so a link
// minted for one tenant's service can never be redeemed against another
// tenant's same-named service, nor against a deleted-and-recreated service
// (whose UID differs).
func signShareToken(namespace, uid, service, bucket, key string, expires int64) string {
	mac := hmac.New(sha256.New, shareSecret)
	_, _ = fmt.Fprintf(mac, "%s:%s:%s:%s:%s:%d", namespace, uid, service, bucket, key, expires)
	return hex.EncodeToString(mac.Sum(nil))
}

func verifyShareToken(namespace, uid, service, bucket, key string, expires int64, token string) bool {
	expected := signShareToken(namespace, uid, service, bucket, key, expires)
	return hmac.Equal([]byte(expected), []byte(token))
}

// Share generates a public download link that proxies through the console-api.
// The link includes a signed token so it works without authentication.
// POST /api/v1/storage/{service}/share?bucket=uploads&key=images/photo.jpg&expires=24h
func (s *Storage) Share(w http.ResponseWriter, r *http.Request) {
	// Resolve, authorize, and capture the exact service instance in one step.
	// The link is signed with THIS namespace/UID — the same one the caller was
	// authorized for — so minting can never sign another tenant's service.
	_, namespace, uid, ok := s.resolveClient(w, r)
	if !ok {
		return
	}

	service := chi.URLParam(r, "service")
	bucket := r.URL.Query().Get("bucket")
	key := r.URL.Query().Get("key")
	if bucket == "" || key == "" {
		respondError(w, http.StatusBadRequest, "bucket and key query parameters are required")
		return
	}

	expiresStr := r.URL.Query().Get("expires")
	if expiresStr == "" {
		expiresStr = "24h"
	}

	duration, err := time.ParseDuration(expiresStr)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid expires duration")
		return
	}

	if duration > 7*24*time.Hour {
		duration = 7 * 24 * time.Hour
	}

	expiresAt := time.Now().Add(duration).Unix()
	token := signShareToken(namespace, uid, service, bucket, key, expiresAt)

	// Build a public URL using the request's host
	scheme := "https"
	if r.TLS == nil && !strings.HasPrefix(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "http"
	}
	host := r.Host
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		host = fwd
	}

	// The namespace pins which service instance the link resolves to; the UID
	// stays in the signature so the link can't be tampered onto another one.
	params := url.Values{
		"namespace": {namespace},
		"bucket":    {bucket},
		"key":       {key},
		"expires":   {strconv.FormatInt(expiresAt, 10)},
		"token":     {token},
	}
	shareURL := fmt.Sprintf("%s://%s/api/v1/storage/%s/shared?%s", scheme, host, url.PathEscape(service), params.Encode())

	respondJSON(w, http.StatusOK, shareResponse{
		URL:     shareURL,
		Expires: duration.String(),
	})
}

// SharedDownload serves a file via a signed share link (no auth required).
// GET /api/v1/storage/{service}/shared?bucket=uploads&key=images/photo.jpg&expires=123&token=abc
func (s *Storage) SharedDownload(w http.ResponseWriter, r *http.Request) {
	service := chi.URLParam(r, "service")
	namespace := r.URL.Query().Get("namespace")
	bucket := r.URL.Query().Get("bucket")
	key := r.URL.Query().Get("key")
	expiresStr := r.URL.Query().Get("expires")
	token := r.URL.Query().Get("token")

	if namespace == "" || bucket == "" || key == "" || expiresStr == "" || token == "" {
		respondError(w, http.StatusBadRequest, "missing required parameters")
		return
	}

	expires, err := strconv.ParseInt(expiresStr, 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid expires")
		return
	}

	if time.Now().Unix() > expires {
		respondError(w, http.StatusGone, "share link has expired")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	// Pin to the exact service instance named in the token: the current UID in
	// this namespace must reproduce the signature. A link minted for a service
	// that has since been deleted and recreated (new UID), or crafted against
	// another tenant's same-named service, fails to verify.
	uid, err := s.getMinioStatefulSet(ctx, namespace, service)
	if err != nil {
		respondServiceLookupError(w, err, service)
		return
	}
	if !verifyShareToken(namespace, uid, service, bucket, key, expires, token) {
		respondError(w, http.StatusForbidden, "invalid share token")
		return
	}

	client, err := s.minioClientForNamespace(ctx, service, namespace)
	if err != nil || client == nil {
		respondError(w, http.StatusNotFound, "service not found")
		return
	}

	obj, err := client.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to download file")
		return
	}
	defer func() { _ = obj.Close() }()

	info, err := obj.Stat()
	if err != nil {
		respondError(w, http.StatusNotFound, "file not found")
		return
	}

	filename := key
	if idx := strings.LastIndex(key, "/"); idx >= 0 {
		filename = key[idx+1:]
	}

	w.Header().Set("Content-Type", info.ContentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", filename))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size))
	_, _ = io.Copy(w, obj)
}

const publicObjectsCMPrefix = "kipper-public-objects-"

func publicObjectsCMName(service string) string {
	return publicObjectsCMPrefix + service
}

func publicObjectKey(bucket, key string) string {
	// ConfigMap data keys must match [-._a-zA-Z0-9]+, so we hex-encode
	// the bucket/key combination.
	return hex.EncodeToString([]byte(bucket + "/" + key))
}

// MakePublic marks an object as publicly accessible.
// PUT /api/v1/storage/{service}/public?bucket=uploads&key=images/photo.jpg
func (s *Storage) MakePublic(w http.ResponseWriter, r *http.Request) {
	service := chi.URLParam(r, "service")
	bucket := r.URL.Query().Get("bucket")
	key := r.URL.Query().Get("key")
	if bucket == "" || key == "" {
		respondError(w, http.StatusBadRequest, "bucket and key are required")
		return
	}
	namespace, ok := s.publicFlagNamespace(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	cmName := publicObjectsCMName(service)
	cmKey := publicObjectKey(bucket, key)

	cm, err := s.Client.CoreV1().ConfigMaps(namespace).Get(ctx, cmName, metav1.GetOptions{})
	if err != nil {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: namespace,
			},
			Data: map[string]string{},
		}
		cm, err = s.Client.CoreV1().ConfigMaps(namespace).Create(ctx, cm, metav1.CreateOptions{})
		if err != nil {
			respondError(w, http.StatusInternalServerError, "failed to create public objects registry")
			return
		}
	}

	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	cm.Data[cmKey] = "true"

	if _, err := s.Client.CoreV1().ConfigMaps(namespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to mark object as public")
		return
	}

	scheme := "https"
	if r.TLS == nil && !strings.HasPrefix(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "http"
	}
	host := r.Host
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		host = fwd
	}
	publicURL := fmt.Sprintf("%s://%s/api/v1/storage/%s/public/%s/%s?namespace=%s", scheme, host, service, bucket, key, url.QueryEscape(namespace))

	respondJSON(w, http.StatusOK, map[string]string{"url": publicURL, "status": "public"})
}

// MakePrivate removes public access from an object.
// DELETE /api/v1/storage/{service}/public?bucket=uploads&key=images/photo.jpg
func (s *Storage) MakePrivate(w http.ResponseWriter, r *http.Request) {
	service := chi.URLParam(r, "service")
	bucket := r.URL.Query().Get("bucket")
	key := r.URL.Query().Get("key")
	if bucket == "" || key == "" {
		respondError(w, http.StatusBadRequest, "bucket and key are required")
		return
	}
	namespace, ok := s.publicFlagNamespace(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	cmName := publicObjectsCMName(service)
	cmKey := publicObjectKey(bucket, key)

	cm, err := s.Client.CoreV1().ConfigMaps(namespace).Get(ctx, cmName, metav1.GetOptions{})
	if err != nil {
		respondJSON(w, http.StatusOK, map[string]string{"status": "private"})
		return
	}

	delete(cm.Data, cmKey)

	if _, err := s.Client.CoreV1().ConfigMaps(namespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to make object private")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "private"})
}

// IsPublic checks whether an object is public.
// GET /api/v1/storage/{service}/public?bucket=uploads&key=images/photo.jpg
func (s *Storage) IsPublic(w http.ResponseWriter, r *http.Request) {
	service := chi.URLParam(r, "service")
	bucket := r.URL.Query().Get("bucket")
	key := r.URL.Query().Get("key")
	namespace, ok := s.publicFlagNamespace(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	isPublic := s.isObjectPublic(ctx, service, namespace, bucket, key)

	scheme := "https"
	if r.TLS == nil && !strings.HasPrefix(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "http"
	}
	host := r.Host
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		host = fwd
	}

	resp := map[string]interface{}{"public": isPublic}
	if isPublic {
		resp["url"] = fmt.Sprintf("%s://%s/api/v1/storage/%s/public/%s/%s?namespace=%s", scheme, host, service, bucket, key, url.QueryEscape(namespace))
	}
	respondJSON(w, http.StatusOK, resp)
}

// PublicDownload serves a public object without authentication.
// GET /api/v1/storage/{service}/public/{bucket}/{key...}
func (s *Storage) PublicDownload(w http.ResponseWriter, r *http.Request) {
	service := chi.URLParam(r, "service")
	bucket := chi.URLParam(r, "bucket")
	key := chi.URLParam(r, "*")

	if bucket == "" || key == "" {
		respondError(w, http.StatusBadRequest, "bucket and key are required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	// The namespace is an explicit request parameter and selects exactly one
	// service instance via an exact StatefulSet lookup — never a cluster-wide
	// search — so a same-named service in another namespace can never be reached
	// from this URL, and the flag check and serve both use that one namespace.
	namespace := r.URL.Query().Get("namespace")
	if namespace == "" {
		respondError(w, http.StatusBadRequest, "namespace query parameter is required")
		return
	}
	if _, err := s.getMinioStatefulSet(ctx, namespace, service); err != nil {
		respondServiceLookupError(w, err, service)
		return
	}
	if !s.isObjectPublic(ctx, service, namespace, bucket, key) {
		respondError(w, http.StatusForbidden, "this object is not public")
		return
	}

	client, err := s.minioClientForNamespace(ctx, service, namespace)
	if err != nil || client == nil {
		respondError(w, http.StatusNotFound, "service not found")
		return
	}

	obj, err := client.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get object")
		return
	}
	defer func() { _ = obj.Close() }()

	info, err := obj.Stat()
	if err != nil {
		respondError(w, http.StatusNotFound, "file not found")
		return
	}

	filename := key
	if idx := strings.LastIndex(key, "/"); idx >= 0 {
		filename = key[idx+1:]
	}

	w.Header().Set("Content-Type", info.ContentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", filename))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size))
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = io.Copy(w, obj)
}

func (s *Storage) isObjectPublic(ctx context.Context, service, namespace, bucket, key string) bool {
	cmName := publicObjectsCMName(service)
	cmKey := publicObjectKey(bucket, key)

	cm, err := s.Client.CoreV1().ConfigMaps(namespace).Get(ctx, cmName, metav1.GetOptions{})
	if err != nil {
		return false
	}
	return cm.Data[cmKey] == "true"
}
