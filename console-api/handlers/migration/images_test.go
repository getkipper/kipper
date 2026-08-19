package migration

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/builder"
	"github.com/getkipper/kipper/console-api/internal/gitreach"
)

func migrationScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = kipperv1.AddToScheme(s)
	return s
}

func newSession() *Session {
	return &Session{
		ID:          "abcdef1234567890",
		SavedRoutes: make(map[string]map[string]interface{}),
	}
}

// A non-git app whose image lives only in this cluster's registry cannot run
// on the target. The image step must fail the migration loudly instead of
// letting the app land there with an unpullable image.
func TestMigrateImages_FailsForLocalImageWithoutGit(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "shop-prod"},
		Spec: kipperv1.AppSpec{
			Image: zotEndpoint + "/shop-prod/legacy:latest",
			Port:  8080,
		},
	}
	h := &Handler{
		Client:   fake.NewSimpleClientset(),
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).WithStatusSubresource(&kipperv1.App{}).WithObjects(app).Build(),
	}

	session := newSession()
	err := h.migrateImages(context.Background(), session, "shop-prod")
	if err == nil {
		t.Fatal("expected migrateImages to fail for a cluster-local image without a git source")
	}
	if !strings.Contains(err.Error(), "legacy") {
		t.Fatalf("error should name the stuck app, got: %v", err)
	}

	last := session.Steps[len(session.Steps)-1]
	if last.Status != StepFailed {
		t.Fatalf("step status = %s, want failed", last.Status)
	}
	if len(last.ManualSteps) == 0 {
		t.Fatal("failed image step must carry manual copy instructions")
	}
}

// Git-built apps are rebuilt on the target, and external images are pulled
// there directly, so the image step completes without copying anything.
func TestMigrateImages_GitAndExternalImagesPass(t *testing.T) {
	gitApp := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: "shop-prod"},
		Spec: kipperv1.AppSpec{
			Image: zotEndpoint + "/shop-prod/backend:abc123",
			Port:  8080,
			Git:   &kipperv1.AppGitSource{URL: "https://github.com/example/backend"},
		},
	}
	externalApp := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "docs", Namespace: "shop-prod"},
		Spec: kipperv1.AppSpec{
			Image: "registry.example.com/docs:v3",
			Port:  8080,
		},
	}
	h := &Handler{
		Client:   fake.NewSimpleClientset(),
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).WithStatusSubresource(&kipperv1.App{}).WithObjects(gitApp, externalApp).Build(),
	}

	session := newSession()
	if err := h.migrateImages(context.Background(), session, "shop-prod"); err != nil {
		t.Fatalf("migrateImages: %v", err)
	}

	last := session.Steps[len(session.Steps)-1]
	if last.Status != StepCompleted {
		t.Fatalf("step status = %s, want completed", last.Status)
	}
	if !strings.Contains(last.Detail, "rebuilt on the target") {
		t.Fatalf("detail should explain the rebuild, got: %q", last.Detail)
	}
}

// A migrated git app must land with the standard "building" placeholder image
// and a triggered build job — its original image reference points at the
// source cluster's registry, which does not exist here.
func TestCreateApp_GitAppGetsPlaceholderAndBuild(t *testing.T) {
	// The triggered build reads the cluster registry's push credential and
	// CA, which kip installs into kipper-system on every cluster.
	clientset := fake.NewSimpleClientset(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "zot-pull-credentials", Namespace: zotNamespace},
			Data:       map[string][]byte{"password": []byte("pullpw")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "zot-push-credentials", Namespace: zotNamespace},
			Data:       map[string][]byte{"password": []byte("pushpw")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: zotTLSSecret, Namespace: zotNamespace},
			Data:       map[string][]byte{"ca.crt": []byte("-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n")},
		},
	)
	h := &Handler{
		Client:   clientset,
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).WithStatusSubresource(&kipperv1.App{}).Build(),
	}

	spec := map[string]interface{}{
		"image": zotEndpoint + "/shop-prod/backend:abc123",
		"port":  8080,
		"git": map[string]interface{}{
			"url":    "https://github.com/example/backend",
			"branch": "main",
		},
	}
	if err := h.createApp(context.Background(), "backend", "shop-prod", spec); err != nil {
		t.Fatalf("createApp: %v", err)
	}

	var app kipperv1.App
	if err := h.CRClient.Get(context.Background(), crclient.ObjectKey{Namespace: "shop-prod", Name: "backend"}, &app); err != nil {
		t.Fatalf("getting created app: %v", err)
	}
	if app.Spec.Image != "busybox:latest" {
		t.Fatalf("image = %q, want the busybox:latest placeholder", app.Spec.Image)
	}

	jobs, err := clientset.BatchV1().Jobs("kipper-builds").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("build jobs = %d, want 1", len(jobs.Items))
	}
	if jobs.Items[0].Labels["kipper.run/build"] != "true" {
		t.Fatal("triggered job must carry the build label")
	}
}

// An image-based app keeps its image untouched and triggers no build.
func TestCreateApp_ImageAppUnchanged(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	h := &Handler{
		Client:   clientset,
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).WithStatusSubresource(&kipperv1.App{}).Build(),
	}

	spec := map[string]interface{}{
		"image": "registry.example.com/docs:v3",
		"port":  8080,
	}
	if err := h.createApp(context.Background(), "docs", "shop-prod", spec); err != nil {
		t.Fatalf("createApp: %v", err)
	}

	var app kipperv1.App
	if err := h.CRClient.Get(context.Background(), crclient.ObjectKey{Namespace: "shop-prod", Name: "docs"}, &app); err != nil {
		t.Fatalf("getting created app: %v", err)
	}
	if app.Spec.Image != "registry.example.com/docs:v3" {
		t.Fatalf("image = %q, want it unchanged", app.Spec.Image)
	}

	jobs, err := clientset.BatchV1().Jobs("shop-prod").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("build jobs = %d, want none", len(jobs.Items))
	}
}

// Builds run a clone preflight, and these tests are about what a migration
// creates rather than about whether a repository answers. Without this every
// run would reach the real internet and a private or missing repository would
// fail the migration for the wrong reason.
func init() {
	builder.ReachGit = func(context.Context, string, string, string, string) (gitreach.Result, string) {
		return gitreach.Reachable, ""
	}
}
