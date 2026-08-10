package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/kip/internal/deployer"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

func fakeProjectDynamic() *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "kipper.run", Version: "v1alpha1", Kind: "Project"}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "kipper.run", Version: "v1alpha1", Kind: "ProjectList"}, &unstructured.UnstructuredList{})
	return dynamicfake.NewSimpleDynamicClient(scheme)
}

func TestEnsureProject_UpdatePreservesUnmanagedFieldsAndUnionsEnvs(t *testing.T) {
	dyn := fakeProjectDynamic()
	_, err := dyn.Resource(manifest.ProjectGVR).Create(context.Background(), &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "Project",
		"metadata":   map[string]interface{}{"name": "blog"},
		"spec": map[string]interface{}{
			"displayName": "Blog",
			"tier":        "medium",
			"members":     []interface{}{map[string]interface{}{"user": "alice", "role": "admin"}},
			"environments": []interface{}{
				map[string]interface{}{"name": "test"},
				map[string]interface{}{"name": "prod", "quota": map[string]interface{}{"cpu": "4"}},
			},
		},
	}}, metav1.CreateOptions{})
	require.NoError(t, err)

	// A manifest that omits prod and displayName, and adds acc.
	m := &manifest.Manifest{Project: "blog", Environments: []string{"test", "acc"}}
	require.NoError(t, ensureProject(context.Background(), dyn, m, "blog"))

	got, err := dyn.Resource(manifest.ProjectGVR).Get(context.Background(), "blog", metav1.GetOptions{})
	require.NoError(t, err)

	// Fields the manifest never carries must survive.
	tier, _, _ := unstructured.NestedString(got.Object, "spec", "tier")
	assert.Equal(t, "medium", tier, "admin-set tier must survive apply")
	members, _, _ := unstructured.NestedSlice(got.Object, "spec", "members")
	assert.Len(t, members, 1, "project members must survive apply")
	dn, _, _ := unstructured.NestedString(got.Object, "spec", "displayName")
	assert.Equal(t, "Blog", dn, "an omitted displayName must not be cleared")

	// Environments unioned: acc added, prod (with quota) never pruned.
	envs, _, _ := unstructured.NestedSlice(got.Object, "spec", "environments")
	names := map[string]bool{}
	var prodQuota interface{}
	for _, e := range envs {
		em := e.(map[string]interface{})
		names[em["name"].(string)] = true
		if em["name"] == "prod" {
			prodQuota = em["quota"]
		}
	}
	assert.True(t, names["test"] && names["prod"] && names["acc"], "apply must union test+prod+acc and never prune prod")
	assert.NotNil(t, prodQuota, "prod's per-environment quota must survive apply")
}

func appResource(name string, spec map[string]interface{}) manifest.Resource {
	return manifest.Resource{
		GVR: deployer.AppGVR,
		Object: &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "kipper.run/v1alpha1",
				"kind":       "App",
				"metadata":   map[string]interface{}{"name": name},
				"spec":       spec,
			},
		},
	}
}

func TestUnionEnvironments_AddsWithoutPruning(t *testing.T) {
	// Live project has test + prod; prod carries a per-env quota. A manifest
	// listing only test + acc must add acc and keep prod (and its quota) intact.
	existing := []interface{}{
		map[string]interface{}{"name": "test"},
		map[string]interface{}{"name": "prod", "quota": map[string]interface{}{"cpu": "4"}},
	}
	got := unionEnvironments(existing, []string{"test", "acc"})

	names := make([]string, 0, len(got))
	var prod map[string]interface{}
	for _, e := range got {
		em := e.(map[string]interface{})
		names = append(names, em["name"].(string))
		if em["name"] == "prod" {
			prod = em
		}
	}
	assert.ElementsMatch(t, []string{"test", "prod", "acc"}, names, "apply must add acc and never prune prod")
	require.NotNil(t, prod)
	assert.Equal(t, map[string]interface{}{"cpu": "4"}, prod["quota"], "an existing environment's quota must survive apply")
}

func TestApplyResource_GitAppPreservesBuiltImage(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	// A live git app whose in-cluster build produced a real image.
	built := "zot.kipper-system.svc.cluster.local:5000/acme-test/acme-backend:manual-123"
	seeded := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "App",
		"metadata":   map[string]interface{}{"name": "backend", "namespace": "default"},
		"spec": map[string]interface{}{
			"git":   map[string]interface{}{"url": "https://github.com/acme/api.git", "branch": "main"},
			"image": built,
			"env":   map[string]interface{}{"LOG_LEVEL": "info"},
		},
	}}
	_, err := dyn.Resource(deployer.AppGVR).Namespace("default").Create(context.Background(), seeded, metav1.CreateOptions{})
	require.NoError(t, err)

	// Apply a git-only manifest — convert stamps the busybox placeholder as the
	// image (there is no image in a git-only manifest). This must NOT reset the
	// running image to the placeholder.
	res := appResource("backend", map[string]interface{}{
		"git":   map[string]interface{}{"url": "https://github.com/acme/api.git", "branch": "main"},
		"image": "busybox:latest",
		"env":   map[string]interface{}{"LOG_LEVEL": "debug"},
	})
	action, err := applyResource(context.Background(), dyn, "default", res, false, nil, false)
	require.NoError(t, err)
	assert.Equal(t, "updated", action)

	app, _ := dyn.Resource(deployer.AppGVR).Namespace("default").Get(context.Background(), "backend", metav1.GetOptions{})
	image, _, _ := unstructured.NestedString(app.Object, "spec", "image")
	assert.Equal(t, built, image, "a git app's built image must survive apply, not reset to busybox")
	// The rest of the spec is still applied declaratively.
	logLevel, _, _ := unstructured.NestedString(app.Object, "spec", "env", "LOG_LEVEL")
	assert.Equal(t, "debug", logLevel, "non-image fields still apply declaratively")
}

func TestApplyResource_GitToImageSwitchClearsGitAndTakesManifestImage(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	// A live git app with a built image.
	seeded := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "App",
		"metadata":   map[string]interface{}{"name": "backend", "namespace": "default"},
		"spec": map[string]interface{}{
			"git":   map[string]interface{}{"url": "https://github.com/acme/api.git", "branch": "main"},
			"image": "zot.kipper-system.svc.cluster.local:5000/acme-test/acme-backend:manual-123",
		},
	}}
	_, err := dyn.Resource(deployer.AppGVR).Namespace("default").Create(context.Background(), seeded, metav1.CreateOptions{})
	require.NoError(t, err)

	// Apply an image-only manifest: the user is switching the app off git onto a
	// prebuilt image. The image-preservation rule is keyed on the incoming spec
	// having a git block, so an image-only manifest must NOT preserve the built
	// image — it takes the manifest image and drops the git block.
	// force, because dropping the git block is a clear and apply refuses those by
	// default. The switch is the point of the test; the refusal has its own.
	action, err := applyResource(context.Background(), dyn, "default", appResource("backend", map[string]interface{}{
		"image": "ghcr.io/acme/api:1.4.0",
	}), true, nil, false)
	require.NoError(t, err)
	assert.Equal(t, "updated", action)

	app, _ := dyn.Resource(deployer.AppGVR).Namespace("default").Get(context.Background(), "backend", metav1.GetOptions{})
	image, _, _ := unstructured.NestedString(app.Object, "spec", "image")
	assert.Equal(t, "ghcr.io/acme/api:1.4.0", image, "a git->image switch must take the manifest image, not preserve the built one")
	_, hasGit, _ := unstructured.NestedMap(app.Object, "spec", "git")
	assert.False(t, hasGit, "a git->image switch must clear the git block")
}

func TestApplyResource_GitAppPreservesNewestImageAcrossConflict(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	// A live git app the build controller keeps updating: between apply's read and
	// its write, the controller pushes a newer built image and bumps the object.
	first := "zot.kipper-system.svc.cluster.local:5000/acme-test/acme-backend:manual-100"
	newest := "zot.kipper-system.svc.cluster.local:5000/acme-test/acme-backend:manual-200"
	seeded := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "App",
		"metadata":   map[string]interface{}{"name": "backend", "namespace": "default"},
		"spec": map[string]interface{}{
			"git":   map[string]interface{}{"url": "https://github.com/acme/api.git", "branch": "main"},
			"image": first,
		},
	}}
	_, err := dyn.Resource(deployer.AppGVR).Namespace("default").Create(context.Background(), seeded, metav1.CreateOptions{})
	require.NoError(t, err)

	// The first Update conflicts. On the retry, the live object now carries the
	// newer built image (as if the controller advanced it). Because the image is
	// read inside the retry closure, the retry must preserve manual-200, not the
	// stale manual-100 read on the first attempt.
	//
	// applyResource issues Gets in this order: (1) the existence check, (2) the
	// first retry-closure read, (3) the second retry-closure read. Override only
	// the third to return the advanced image, then let the final assertion read
	// the real store — proving the closure's Update actually persisted manual-200.
	gets := 0
	dyn.PrependReactor("get", "apps", func(k8stesting.Action) (bool, runtime.Object, error) {
		gets++
		if gets == 3 {
			return true, &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "kipper.run/v1alpha1",
				"kind":       "App",
				"metadata":   map[string]interface{}{"name": "backend", "namespace": "default"},
				"spec": map[string]interface{}{
					"git":   map[string]interface{}{"url": "https://github.com/acme/api.git", "branch": "main"},
					"image": newest,
				},
			}}, nil
		}
		return false, nil, nil
	})
	updates := 0
	dyn.PrependReactor("update", "apps", func(k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates == 1 {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "kipper.run", Resource: "apps"}, "backend", context.DeadlineExceeded)
		}
		return false, nil, nil
	})

	action, err := applyResource(context.Background(), dyn, "default", appResource("backend", map[string]interface{}{
		"git":   map[string]interface{}{"url": "https://github.com/acme/api.git", "branch": "main"},
		"image": "busybox:latest",
	}), false, nil, false)
	require.NoError(t, err)
	assert.Equal(t, "updated", action)

	app, _ := dyn.Resource(deployer.AppGVR).Namespace("default").Get(context.Background(), "backend", metav1.GetOptions{})
	image, _, _ := unstructured.NestedString(app.Object, "spec", "image")
	assert.Equal(t, newest, image, "the conflict retry must preserve the newest live image read inside the retry, not the stale first read")
}

func TestApplyResource_CreatesWhenAbsent(t *testing.T) {
	dyn := fakeWorkloadDynamic()

	action, err := applyResource(context.Background(), dyn, "default", appResource("api", map[string]interface{}{"image": "nginx", "port": int64(80)}), false, nil, false)
	require.NoError(t, err)
	assert.Equal(t, "created", action)

	app, err := dyn.Resource(deployer.AppGVR).Namespace("default").Get(context.Background(), "api", metav1.GetOptions{})
	require.NoError(t, err)
	image, _, _ := unstructured.NestedString(app.Object, "spec", "image")
	assert.Equal(t, "nginx", image)
}

func TestApplyResource_CreateRaceFallsBackToUpdate(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	// The race winner already exists, carrying nothing the manifest does not.
	seeded := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "App",
		"metadata":   map[string]interface{}{"name": "api", "namespace": "default"},
		"spec":       map[string]interface{}{"image": "nginx"},
	}}
	_, err := dyn.Resource(deployer.AppGVR).Namespace("default").Create(context.Background(), seeded, metav1.CreateOptions{})
	require.NoError(t, err)

	// Force the first Get to miss (as if the object were still absent) so we take
	// the create path, and make Create report the object already exists.
	gets := 0
	dyn.PrependReactor("get", "apps", func(k8stesting.Action) (bool, runtime.Object, error) {
		gets++
		if gets == 1 {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "kipper.run", Resource: "apps"}, "api")
		}
		return false, nil, nil
	})
	dyn.PrependReactor("create", "apps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Group: "kipper.run", Resource: "apps"}, "api")
	})

	action, err := applyResource(context.Background(), dyn, "default", appResource("api", map[string]interface{}{"image": "nginx:1.27"}), false, nil, false)
	require.NoError(t, err)
	assert.Equal(t, "updated", action, "a lost create race must fall back to update, not fail")

	got, _ := dyn.Resource(deployer.AppGVR).Namespace("default").Get(context.Background(), "api", metav1.GetOptions{})
	image, _, _ := unstructured.NestedString(got.Object, "spec", "image")
	assert.Equal(t, "nginx:1.27", image, "the manifest spec must be applied after falling back to update")
}

func TestApplyResource_ReplacesSpecAndClearsOmittedFields(t *testing.T) {
	dyn := fakeWorkloadDynamic()

	// Seed an App whose route has API-key gating set via the console, plus a
	// live resourceVersion, labels, and a status the reconciler owns.
	seeded := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kipper.run/v1alpha1",
			"kind":       "App",
			"metadata": map[string]interface{}{
				"name":      "api",
				"namespace": "default",
				"labels":    map[string]interface{}{"team": "platform"},
			},
			"spec": map[string]interface{}{
				"image": "nginx",
				"route": map[string]interface{}{
					"host":          "api.example.com",
					"requireApiKey": true,
				},
			},
			"status": map[string]interface{}{"phase": "Running"},
		},
	}
	_, err := dyn.Resource(deployer.AppGVR).Namespace("default").Create(context.Background(), seeded, metav1.CreateOptions{})
	require.NoError(t, err)

	// Apply a manifest that keeps the route host but omits requireApiKey.
	action, err := applyResource(context.Background(), dyn, "default", appResource("api", map[string]interface{}{
		"image": "nginx:1.27",
		"route": map[string]interface{}{"host": "api.example.com"},
	}), true, nil, false)
	require.NoError(t, err)
	assert.Equal(t, "updated", action)

	app, err := dyn.Resource(deployer.AppGVR).Namespace("default").Get(context.Background(), "api", metav1.GetOptions{})
	require.NoError(t, err)

	// The omitted field is gone — apply is declarative.
	_, found, _ := unstructured.NestedBool(app.Object, "spec", "route", "requireApiKey")
	assert.False(t, found, "a field left out of the manifest must be cleared on apply")

	image, _, _ := unstructured.NestedString(app.Object, "spec", "image")
	assert.Equal(t, "nginx:1.27", image, "the manifest spec replaces the live spec")

	// Metadata the manifest does not carry is preserved.
	labels := app.GetLabels()
	assert.Equal(t, "platform", labels["team"], "labels set outside the manifest must survive an apply")

	// Status is owned by the reconciler and must not be wiped by a spec replace.
	phase, _, _ := unstructured.NestedString(app.Object, "status", "phase")
	assert.Equal(t, "Running", phase, "apply must not clobber reconciler-owned status")
}

// The trap this whole change exists for: a redirect set with `kip app update`
// and never written into the manifest. Apply removes it, and until now nothing
// said so before it happened.
func TestScanClears_NamesWhatAnApplyWouldRemove(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seeded := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "App",
		"metadata":   map[string]interface{}{"name": "shop", "namespace": "default"},
		"spec": map[string]interface{}{
			"image": "shop:v1",
			"route": map[string]interface{}{
				"host":         "example.com",
				"redirectFrom": []interface{}{"www.example.com"},
			},
		},
	}}
	_, err := dyn.Resource(deployer.AppGVR).Namespace("default").Create(context.Background(), seeded, metav1.CreateOptions{})
	require.NoError(t, err)

	// A hand-written manifest that never knew about the redirect.
	res := appResource("shop", map[string]interface{}{
		"image": "shop:v1",
		"route": map[string]interface{}{"host": "example.com"},
	})

	changes, err := scanChanges(context.Background(), dyn, "default", []manifest.Resource{res})
	require.NoError(t, err)
	clears := clearsOf(changes)
	require.Len(t, clears, 1)
	assert.Equal(t, "route.redirectFrom", clears[0].change.Path)
	assert.Equal(t, "[www.example.com]", clears[0].change.Live, "the value going away is named")
	assert.Equal(t, "App", clears[0].kind)
	assert.Equal(t, "shop", clears[0].name)
}

// A git app's built image is carried forward by apply, so reporting it as
// cleared would be crying wolf on the one field that is safe.
func TestScanClears_DoesNotReportAGitAppsBuiltImage(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seeded := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "App",
		"metadata":   map[string]interface{}{"name": "backend", "namespace": "default"},
		"spec": map[string]interface{}{
			"git":   map[string]interface{}{"url": "https://github.com/acme/api.git"},
			"image": "zot.kipper-system.svc.cluster.local:5000/acme/backend:manual-123",
		},
	}}
	_, err := dyn.Resource(deployer.AppGVR).Namespace("default").Create(context.Background(), seeded, metav1.CreateOptions{})
	require.NoError(t, err)

	// Convert always stamps an image — busybox for a git-only manifest — so the
	// field is present on both sides and can never be *cleared*. What the guard
	// actually prevents is `kip diff` reporting it as changing to the
	// placeholder, which is the alarming and false message, since apply keeps
	// the built image. So that is what this asserts.
	res := appResource("backend", map[string]interface{}{
		"git":   map[string]interface{}{"url": "https://github.com/acme/api.git"},
		"image": "busybox:latest",
	})
	changes, err := scanChanges(context.Background(), dyn, "default", []manifest.Resource{res})
	require.NoError(t, err)
	assert.Empty(t, clearsOf(changes), "the built image is not being cleared")

	// And nothing reports it as changing either. This drives scanChanges, the
	// function both diff and apply call, so removing the preserved-path
	// argument there fails here — where calling DiffSpec directly would only
	// have proved the parameter works.
	for _, rc := range changes {
		assert.NotEqual(t, "image", rc.change.Path,
			"apply preserves a git app's built image, so no diff may say it becomes the placeholder")
	}
}

// A resource being created has nothing to lose.
func TestScanClears_IgnoresResourcesThatDoNotExistYet(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	res := appResource("brand-new", map[string]interface{}{"image": "shop:v1"})
	changes, err := scanChanges(context.Background(), dyn, "default", []manifest.Resource{res})
	require.NoError(t, err)
	assert.Empty(t, clearsOf(changes))
}

// A manifest that carries everything the cluster has warns about nothing, or
// the warning becomes noise people learn to pass --force through.
func TestScanClears_SaysNothingWhenTheManifestIsComplete(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	spec := map[string]interface{}{
		"image": "shop:v1",
		"route": map[string]interface{}{
			"host":         "example.com",
			"redirectFrom": []interface{}{"www.example.com"},
		},
	}
	seeded := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1", "kind": "App",
		"metadata": map[string]interface{}{"name": "shop", "namespace": "default"},
		"spec":     spec,
	}}
	_, err := dyn.Resource(deployer.AppGVR).Namespace("default").Create(context.Background(), seeded, metav1.CreateOptions{})
	require.NoError(t, err)

	changes, err := scanChanges(context.Background(), dyn, "default",
		[]manifest.Resource{appResource("shop", spec)})
	require.NoError(t, err)
	assert.Empty(t, changes, "a complete manifest changes nothing at all")
}

// The scan is one read and the write is another, so anything set in between was
// invisible to the preflight. The refusal has to hold against the object being
// replaced, not against the one that was scanned.
func TestApplyResource_RefusesAFieldThatAppearedAfterTheScan(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seeded := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "App",
		"metadata":   map[string]interface{}{"name": "api", "namespace": "default"},
		"spec":       map[string]interface{}{"image": "nginx", "route": map[string]interface{}{"host": "api.example.com"}},
	}}
	_, err := dyn.Resource(deployer.AppGVR).Namespace("default").Create(context.Background(), seeded, metav1.CreateOptions{})
	require.NoError(t, err)

	res := appResource("api", map[string]interface{}{
		"image": "nginx:1.27",
		"route": map[string]interface{}{"host": "api.example.com"},
	})

	// The preflight is clean: the manifest carries everything the cluster has.
	changes, err := scanChanges(context.Background(), dyn, "default", []manifest.Resource{res})
	require.NoError(t, err)
	require.Empty(t, clearsOf(changes), "the scan must find nothing, or this proves the wrong thing")

	// Now the console sets a rate limit, after the scan and before the write.
	dyn.PrependReactor("get", "apps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "kipper.run/v1alpha1",
			"kind":       "App",
			"metadata":   map[string]interface{}{"name": "api", "namespace": "default"},
			"spec": map[string]interface{}{
				"image": "nginx",
				"route": map[string]interface{}{"host": "api.example.com", "rateLimit": int64(100)},
			},
		}}, nil
	})

	_, err = applyResource(context.Background(), dyn, "default", res, false, nil, false)
	require.Error(t, err, "a field added between the scan and the write must not be cleared silently")
	assert.Contains(t, err.Error(), "route.rateLimit")

	app, _ := dyn.Resource(deployer.AppGVR).Namespace("default").Get(context.Background(), "api", metav1.GetOptions{})
	image, _, _ := unstructured.NestedString(app.Object, "spec", "image")
	assert.Equal(t, "nginx", image, "a refused apply must write nothing at all")
}

// The same window, reached the other way: the resource did not exist when the
// scan ran, so the scan skipped it entirely, and the create then lost its race.
func TestApplyResource_RefusesWhenTheCreateRaceWinnerHasMore(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seeded := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "App",
		"metadata":   map[string]interface{}{"name": "api", "namespace": "default"},
		"spec":       map[string]interface{}{"image": "nginx", "route": map[string]interface{}{"requireApiKey": true}},
	}}
	_, err := dyn.Resource(deployer.AppGVR).Namespace("default").Create(context.Background(), seeded, metav1.CreateOptions{})
	require.NoError(t, err)

	gets := 0
	dyn.PrependReactor("get", "apps", func(k8stesting.Action) (bool, runtime.Object, error) {
		gets++
		if gets == 1 {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "kipper.run", Resource: "apps"}, "api")
		}
		return false, nil, nil
	})
	dyn.PrependReactor("create", "apps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Group: "kipper.run", Resource: "apps"}, "api")
	})

	_, err = applyResource(context.Background(), dyn, "default", appResource("api", map[string]interface{}{"image": "nginx:1.27"}), false, nil, false)
	require.Error(t, err, "falling back to update must not clear what the race winner carries")
	assert.Contains(t, err.Error(), "route.requireApiKey")

	got, _ := dyn.Resource(deployer.AppGVR).Namespace("default").Get(context.Background(), "api", metav1.GetOptions{})
	gated, _, _ := unstructured.NestedBool(got.Object, "spec", "route", "requireApiKey")
	assert.True(t, gated, "the winner's field must survive a refused fallback")
}

// A refusal is a preflight, so it has to happen before anything is written —
// including for the manifests ahead of the one that triggers it. Scanned per
// manifest, a directory whose last file cleared a field had already created the
// projects, namespaces and workloads for every file before it.
func TestApplyPlans_RefusalWritesNothingForEarlierManifests(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	clientset := k8sfake.NewSimpleClientset()

	// The second manifest's app has a console-set redirect the manifest omits.
	seeded := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "App",
		"metadata":   map[string]interface{}{"name": "shop", "namespace": "acme-test"},
		"spec": map[string]interface{}{
			"image": "nginx",
			"route": map[string]interface{}{"host": "shop.example.com", "redirectFrom": []interface{}{"www.example.com"}},
		},
	}}
	_, err := dyn.Resource(deployer.AppGVR).Namespace("acme-test").Create(context.Background(), seeded, metav1.CreateOptions{})
	require.NoError(t, err)

	plans := []applyPlan{
		{
			m:         &manifest.Manifest{Project: "acme", Environments: []string{"test"}},
			project:   "acme",
			namespace: "acme-test",
			resources: []manifest.Resource{appResource("api", map[string]interface{}{"image": "nginx:1.27"})},
		},
		{
			m:         &manifest.Manifest{Project: "acme", Environments: []string{"test"}},
			project:   "acme",
			namespace: "acme-test",
			resources: []manifest.Resource{appResource("shop", map[string]interface{}{
				"image": "nginx",
				"route": map[string]interface{}{"host": "shop.example.com"},
			})},
		},
	}

	created, updated, err := applyPlans(context.Background(), dyn, clientset, plans, false)
	require.Error(t, err, "a clear in the second manifest must refuse the whole run")
	assert.Zero(t, created)
	assert.Zero(t, updated)

	_, getErr := dyn.Resource(deployer.AppGVR).Namespace("acme-test").Get(context.Background(), "api", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(getErr), "the first manifest's app must not be created when a later one refuses")

	_, getErr = dyn.Resource(manifest.ProjectGVR).Get(context.Background(), "acme", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(getErr), "no Project may be created ahead of the refusal")

	_, getErr = clientset.CoreV1().Namespaces().Get(context.Background(), "acme-test", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(getErr), "no Namespace may be created ahead of the refusal")

	redirect, _, _ := unstructured.NestedStringSlice(seeded.Object, "spec", "route", "redirectFrom")
	assert.Equal(t, []string{"www.example.com"}, redirect)
}

// The same run with --force applies every plan, so the refusal is the only thing
// that stops it.
func TestApplyPlans_ForceAppliesEveryPlan(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	clientset := k8sfake.NewSimpleClientset()

	seeded := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "App",
		"metadata":   map[string]interface{}{"name": "shop", "namespace": "acme-test"},
		"spec": map[string]interface{}{
			"image": "nginx",
			"route": map[string]interface{}{"host": "shop.example.com", "redirectFrom": []interface{}{"www.example.com"}},
		},
	}}
	_, err := dyn.Resource(deployer.AppGVR).Namespace("acme-test").Create(context.Background(), seeded, metav1.CreateOptions{})
	require.NoError(t, err)

	plans := []applyPlan{
		{
			m:         &manifest.Manifest{Project: "acme"},
			project:   "acme",
			namespace: "acme-test",
			resources: []manifest.Resource{appResource("api", map[string]interface{}{"image": "nginx:1.27"})},
		},
		{
			m:         &manifest.Manifest{Project: "acme"},
			project:   "acme",
			namespace: "acme-test",
			resources: []manifest.Resource{appResource("shop", map[string]interface{}{
				"image": "nginx",
				"route": map[string]interface{}{"host": "shop.example.com"},
			})},
		},
	}

	created, updated, err := applyPlans(context.Background(), dyn, clientset, plans, true)
	require.NoError(t, err)
	assert.Equal(t, 1, created)
	assert.Equal(t, 1, updated)

	shop, _ := dyn.Resource(deployer.AppGVR).Namespace("acme-test").Get(context.Background(), "shop", metav1.GetOptions{})
	_, found, _ := unstructured.NestedSlice(shop.Object, "spec", "route", "redirectFrom")
	assert.False(t, found, "--force clears what the manifest does not carry")
}

// The preflight cannot cover a field that appears after it, so the write-time
// guard can stop a run that has already written earlier resources. Kubernetes
// has no transaction across objects, so those writes stand — and the run has to
// say so, because an error with no other output reads as "nothing happened".
func TestApplyPlans_ReportsWhatItWroteBeforeStopping(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	clientset := k8sfake.NewSimpleClientset()

	// Both apps match their manifests, so the preflight is clean.
	for _, name := range []string{"api", "shop"} {
		seeded := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "kipper.run/v1alpha1",
			"kind":       "App",
			"metadata":   map[string]interface{}{"name": name, "namespace": "acme-test"},
			"spec":       map[string]interface{}{"image": "nginx"},
		}}
		_, err := dyn.Resource(deployer.AppGVR).Namespace("acme-test").Create(context.Background(), seeded, metav1.CreateOptions{})
		require.NoError(t, err)
	}

	plans := []applyPlan{
		{m: &manifest.Manifest{Project: "acme"}, project: "acme", namespace: "acme-test",
			resources: []manifest.Resource{appResource("api", map[string]interface{}{"image": "nginx:1.27"})}},
		{m: &manifest.Manifest{Project: "acme"}, project: "acme", namespace: "acme-test",
			resources: []manifest.Resource{appResource("shop", map[string]interface{}{"image": "nginx:1.27"})}},
	}

	scanned, err := scanChanges(context.Background(), dyn, "acme-test", plans[1].resources)
	require.NoError(t, err)
	require.Empty(t, clearsOf(scanned), "the preflight must be clean, or this proves the wrong thing")

	// After the scan, another client gates shop's route. api is written first,
	// then shop's fresh read finds the new field.
	// The first read of shop is the preflight's, which must stay clean; every
	// read after it is the write path's and sees the new field.
	shopReads := 0
	dyn.PrependReactor("get", "apps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.(k8stesting.GetAction).GetName() != "shop" {
			return false, nil, nil
		}
		shopReads++
		if shopReads == 1 {
			return false, nil, nil
		}
		return true, &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "kipper.run/v1alpha1",
			"kind":       "App",
			"metadata":   map[string]interface{}{"name": "shop", "namespace": "acme-test"},
			"spec":       map[string]interface{}{"image": "nginx", "route": map[string]interface{}{"requireApiKey": true}},
		}}, nil
	})

	out := captureStdout(t, func() {
		_, updated, applyErr := applyPlans(context.Background(), dyn, clientset, plans, false)
		require.Error(t, applyErr, "the field added after the scan must stop the run")
		assert.Equal(t, 1, updated, "the count of what was already written must survive the error")
	})

	assert.Contains(t, out, "Stopped part-way: 1 updated, 1 namespace(s) created")
	assert.Contains(t, out, "Those writes stand")

	api, _ := dyn.Resource(deployer.AppGVR).Namespace("acme-test").Get(context.Background(), "api", metav1.GetOptions{})
	image, _, _ := unstructured.NestedString(api.Object, "spec", "image")
	assert.Equal(t, "nginx:1.27", image, "the earlier write is what the message is reporting")
}

// A namespace or Project write is a mutation too, so failing at one after
// earlier plans have been applied leaves the cluster part-way just the same.
func TestApplyPlans_ReportsWhenALaterNamespaceFails(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	clientset := k8sfake.NewSimpleClientset()

	seeded := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "App",
		"metadata":   map[string]interface{}{"name": "api", "namespace": "acme-test"},
		"spec":       map[string]interface{}{"image": "nginx"},
	}}
	_, err := dyn.Resource(deployer.AppGVR).Namespace("acme-test").Create(context.Background(), seeded, metav1.CreateOptions{})
	require.NoError(t, err)

	clientset.PrependReactor("create", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ns := action.(k8stesting.CreateAction).GetObject().(*corev1.Namespace)
		if ns.Name == "acme-prod" {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "acme-prod", context.DeadlineExceeded)
		}
		return false, nil, nil
	})

	plans := []applyPlan{
		{m: &manifest.Manifest{Project: "acme"}, project: "acme", namespace: "acme-test",
			resources: []manifest.Resource{appResource("api", map[string]interface{}{"image": "nginx:1.27"})}},
		{m: &manifest.Manifest{Project: "acme"}, project: "acme", namespace: "acme-prod",
			resources: []manifest.Resource{appResource("api", map[string]interface{}{"image": "nginx:1.27"})}},
	}

	out := captureStdout(t, func() {
		_, updated, applyErr := applyPlans(context.Background(), dyn, clientset, plans, false)
		require.Error(t, applyErr)
		assert.Contains(t, applyErr.Error(), "creating namespace acme-prod")
		assert.Equal(t, 1, updated)
	})

	assert.Contains(t, out, "Stopped part-way:", "a namespace failure after an earlier write must report it too")
	assert.Contains(t, out, "1 updated")
}

// Failing on the very first thing wrote nothing, and saying "stopped part-way"
// there would be its own kind of wrong.
func TestApplyPlans_SaysNothingWhenItWroteNothing(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	clientset := k8sfake.NewSimpleClientset()

	clientset.PrependReactor("create", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "acme-test", context.DeadlineExceeded)
	})

	plans := []applyPlan{
		{m: &manifest.Manifest{Project: "acme"}, project: "acme", namespace: "acme-test",
			resources: []manifest.Resource{appResource("api", map[string]interface{}{"image": "nginx:1.27"})}},
	}

	out := captureStdout(t, func() {
		_, _, applyErr := applyPlans(context.Background(), dyn, clientset, plans, false)
		require.Error(t, applyErr)
	})
	assert.NotContains(t, out, "Stopped part-way")
}

// Losing the namespace create race is not a failure — the namespace is there
// either way — but it is not one of this run's writes, and the tick and the
// partial-apply count must both stop claiming it.
func TestApplyPlans_DoesNotClaimANamespaceItLostTheRaceFor(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	clientset := k8sfake.NewSimpleClientset()

	seeded := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "App",
		"metadata":   map[string]interface{}{"name": "api", "namespace": "acme-test"},
		"spec":       map[string]interface{}{"image": "nginx"},
	}}
	_, err := dyn.Resource(deployer.AppGVR).Namespace("acme-test").Create(context.Background(), seeded, metav1.CreateOptions{})
	require.NoError(t, err)

	// acme-test's Get misses and someone else creates it before our Create
	// lands; acme-prod then fails outright, so the run stops and reports.
	clientset.PrependReactor("create", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
		name := action.(k8stesting.CreateAction).GetObject().(*corev1.Namespace).Name
		if name == "acme-test" {
			return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "namespaces"}, name)
		}
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, name, context.DeadlineExceeded)
	})

	plans := []applyPlan{
		{m: &manifest.Manifest{Project: "acme"}, project: "acme", namespace: "acme-test",
			resources: []manifest.Resource{appResource("api", map[string]interface{}{"image": "nginx:1.27"})}},
		{m: &manifest.Manifest{Project: "acme"}, project: "acme", namespace: "acme-prod",
			resources: []manifest.Resource{appResource("api", map[string]interface{}{"image": "nginx:1.27"})}},
	}

	out := captureStdout(t, func() {
		_, _, applyErr := applyPlans(context.Background(), dyn, clientset, plans, false)
		require.Error(t, applyErr)
		assert.Contains(t, applyErr.Error(), "creating namespace acme-prod")
	})

	assert.NotContains(t, out, "Namespace acme-test created", "another actor created it, so the tick is not ours to print")
	assert.NotContains(t, out, "namespace(s) created", "and it is not one of this run's writes")
	assert.Contains(t, out, "1 updated", "the workload write is still reported")
}

// Two manifests naming one resource make the preflight unanswerable: each is
// compared against the live object, so a field the first adds and the second
// omits is invisible until the first has been written and the second refuses —
// a partial apply from a deterministic mistake rather than a race.
func TestApplyPlans_RefusesTwoManifestsWritingOneResource(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	clientset := k8sfake.NewSimpleClientset()

	seeded := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "App",
		"metadata":   map[string]interface{}{"name": "api", "namespace": "acme-test"},
		"spec":       map[string]interface{}{"image": "nginx"},
	}}
	_, err := dyn.Resource(deployer.AppGVR).Namespace("acme-test").Create(context.Background(), seeded, metav1.CreateOptions{})
	require.NoError(t, err)

	plans := []applyPlan{
		{m: &manifest.Manifest{Project: "acme"}, project: "acme", namespace: "acme-test",
			resources: []manifest.Resource{appResource("api", map[string]interface{}{
				"image": "nginx:1.27",
				"route": map[string]interface{}{"host": "api.example.com", "rateLimit": int64(100)},
			})}},
		{m: &manifest.Manifest{Project: "acme"}, project: "acme", namespace: "acme-test",
			resources: []manifest.Resource{appResource("api", map[string]interface{}{
				"image": "nginx:1.27",
			})}},
	}

	created, updated, applyErr := applyPlans(context.Background(), dyn, clientset, plans, false)
	require.Error(t, applyErr)
	assert.Contains(t, applyErr.Error(), "App/api in acme-test")
	assert.Zero(t, created)
	assert.Zero(t, updated)

	app, _ := dyn.Resource(deployer.AppGVR).Namespace("acme-test").Get(context.Background(), "api", metav1.GetOptions{})
	image, _, _ := unstructured.NestedString(app.Object, "spec", "image")
	assert.Equal(t, "nginx", image, "the refusal must come before any write")
}

// seedAppCRD gives the fake cluster a schema that defaults spec.replicas, the
// way a real one does.
func seedAppCRD(t *testing.T, dyn *dynamicfake.FakeDynamicClient) {
	t.Helper()
	crd := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]interface{}{"name": "apps.kipper.run"},
		"spec": map[string]interface{}{
			"versions": []interface{}{map[string]interface{}{
				"name": "v1alpha1",
				"schema": map[string]interface{}{"openAPIV3Schema": map[string]interface{}{
					"properties": map[string]interface{}{"spec": map[string]interface{}{
						"properties": map[string]interface{}{
							"replicas": map[string]interface{}{"type": "integer", "default": int64(1)},
						},
					}},
				}},
			}},
		},
	}}
	_, err := dyn.Resource(installer.CRDGVR).Create(context.Background(), crd, metav1.CreateOptions{})
	require.NoError(t, err)
}

// The whole feature turns on this. Admission stores spec.replicas: 1 for a
// manifest that never mentions replicas, and reading the difference off the
// manifest alone called that a field the apply would destroy — so apply refused
// an ordinary manifest, every time, and a Job could not be fixed at all because
// its manifest type has no backoffLimit to add.
func TestScanChanges_ADefaultedFieldIsNotReportedAsAClear(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedAppCRD(t, dyn)

	seeded := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "App",
		"metadata":   map[string]interface{}{"name": "api", "namespace": "acme-test"},
		// What admission stores for a manifest that omits replicas.
		"spec": map[string]interface{}{"image": "nginx", "replicas": int64(1)},
	}}
	_, err := dyn.Resource(deployer.AppGVR).Namespace("acme-test").Create(context.Background(), seeded, metav1.CreateOptions{})
	require.NoError(t, err)

	res := []manifest.Resource{appResource("api", map[string]interface{}{"image": "nginx"})}
	changes, err := scanChanges(context.Background(), dyn, "acme-test", res)
	require.NoError(t, err)
	assert.Empty(t, clearsOf(changes), "the cluster puts the same default straight back")
}

// And the apply that follows must go through rather than refuse.
func TestApplyPlans_AppliesAManifestThatOmitsADefaultedField(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	clientset := k8sfake.NewSimpleClientset()
	seedAppCRD(t, dyn)

	seeded := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "App",
		"metadata":   map[string]interface{}{"name": "api", "namespace": "acme-test"},
		"spec":       map[string]interface{}{"image": "nginx", "replicas": int64(1)},
	}}
	_, err := dyn.Resource(deployer.AppGVR).Namespace("acme-test").Create(context.Background(), seeded, metav1.CreateOptions{})
	require.NoError(t, err)

	plans := []applyPlan{{
		m: &manifest.Manifest{Project: "acme"}, project: "acme", namespace: "acme-test",
		resources: []manifest.Resource{appResource("api", map[string]interface{}{"image": "nginx:1.27"})},
	}}

	_, updated, applyErr := applyPlans(context.Background(), dyn, clientset, plans, false)
	require.NoError(t, applyErr, "an ordinary manifest must not need --force")
	assert.Equal(t, 1, updated)
}

// Without the schema a value the cluster fills in cannot be told from one the
// manifest destroys. Listing both is right; calling them all destroyed is not,
// and an operator reading that reaches for --force to make it go away.
func TestApplyPlans_SaysWhenItCouldNotReadTheSchema(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	clientset := k8sfake.NewSimpleClientset()
	// No CRD seeded, so the schema is unavailable.

	seeded := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "App",
		"metadata":   map[string]interface{}{"name": "api", "namespace": "acme-test"},
		"spec":       map[string]interface{}{"image": "nginx", "replicas": int64(1)},
	}}
	_, err := dyn.Resource(deployer.AppGVR).Namespace("acme-test").Create(context.Background(), seeded, metav1.CreateOptions{})
	require.NoError(t, err)

	plans := []applyPlan{{
		m: &manifest.Manifest{Project: "acme"}, project: "acme", namespace: "acme-test",
		resources: []manifest.Resource{appResource("api", map[string]interface{}{"image": "nginx:1.27"})},
	}}

	out := captureStdout(t, func() {
		_, _, applyErr := applyPlans(context.Background(), dyn, clientset, plans, false)
		require.Error(t, applyErr, "with no schema it cannot know, so it asks")
	})
	assert.Contains(t, out, "replicas", "the field is still listed")
	assert.Contains(t, out, "may not be losses at all", "and the reason it might not be is said")
}

// With the schema there is nothing to caveat.
func TestApplyPlans_SaysNothingAboutTheSchemaWhenItReadIt(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	clientset := k8sfake.NewSimpleClientset()
	seedAppCRD(t, dyn)

	seeded := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "App",
		"metadata":   map[string]interface{}{"name": "api", "namespace": "acme-test"},
		"spec": map[string]interface{}{
			"image": "nginx", "replicas": int64(1),
			"route": map[string]interface{}{"redirectFrom": []interface{}{"www.example.com"}},
		},
	}}
	_, err := dyn.Resource(deployer.AppGVR).Namespace("acme-test").Create(context.Background(), seeded, metav1.CreateOptions{})
	require.NoError(t, err)

	plans := []applyPlan{{
		m: &manifest.Manifest{Project: "acme"}, project: "acme", namespace: "acme-test",
		resources: []manifest.Resource{appResource("api", map[string]interface{}{"image": "nginx:1.27"})},
	}}

	out := captureStdout(t, func() {
		_, _, applyErr := applyPlans(context.Background(), dyn, clientset, plans, false)
		require.Error(t, applyErr, "redirectFrom really is being removed")
	})
	assert.NotContains(t, out, "may not be losses at all")
	assert.NotContains(t, out, "replicas", "a defaulted field is not listed when the schema says so")
}

// The caveat belongs to the list it qualifies. A kind whose schema could not be
// read but which contributed nothing to the list makes the warning noise, and
// noise is what teaches an operator to skip it.
func TestApplyPlans_DoesNotCaveatAListItCanVouchFor(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	clientset := k8sfake.NewSimpleClientset()
	seedAppCRD(t, dyn)

	// The App's schema is readable and its only loss is a real one. The
	// Function's is not readable, but it has nothing to report.
	app := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1", "kind": "App",
		"metadata": map[string]interface{}{"name": "api", "namespace": "acme-test"},
		"spec": map[string]interface{}{
			"image": "nginx",
			"route": map[string]interface{}{"redirectFrom": []interface{}{"www.example.com"}},
		},
	}}
	_, err := dyn.Resource(deployer.AppGVR).Namespace("acme-test").Create(context.Background(), app, metav1.CreateOptions{})
	require.NoError(t, err)

	fn := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1", "kind": "Function",
		"metadata": map[string]interface{}{"name": "resize", "namespace": "acme-test"},
		"spec":     map[string]interface{}{"image": "nginx"},
	}}
	_, err = dyn.Resource(manifest.FunctionGVR).Namespace("acme-test").Create(context.Background(), fn, metav1.CreateOptions{})
	require.NoError(t, err)

	plans := []applyPlan{{
		m: &manifest.Manifest{Project: "acme"}, project: "acme", namespace: "acme-test",
		resources: []manifest.Resource{
			appResource("api", map[string]interface{}{"image": "nginx:1.27"}),
			{GVR: manifest.FunctionGVR, Object: &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "kipper.run/v1alpha1", "kind": "Function",
				"metadata": map[string]interface{}{"name": "resize", "namespace": "acme-test"},
				"spec":     map[string]interface{}{"image": "nginx"},
			}}},
		},
	}}

	out := captureStdout(t, func() {
		_, _, applyErr := applyPlans(context.Background(), dyn, clientset, plans, false)
		require.Error(t, applyErr, "redirectFrom is a real loss")
	})
	assert.Contains(t, out, "route.redirectFrom")
	assert.NotContains(t, out, "may not be losses at all",
		"nothing in the list came from the kind it could not read")
}

// The write-time guard is the authoritative one, so it carries the same
// uncertainty the preflight discloses. Without it the operator is told a field
// is being destroyed and sent to --force, when the truth may be that nothing
// could be read about it.
func TestApplyResource_SaysWhenItCouldNotReadTheSchema(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seeded := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1", "kind": "App",
		"metadata": map[string]interface{}{"name": "api", "namespace": "default"},
		"spec":     map[string]interface{}{"image": "nginx"},
	}}
	_, err := dyn.Resource(deployer.AppGVR).Namespace("default").Create(context.Background(), seeded, metav1.CreateOptions{})
	require.NoError(t, err)

	// The field appears between the preflight and the write.
	dyn.PrependReactor("get", "apps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "kipper.run/v1alpha1", "kind": "App",
			"metadata": map[string]interface{}{"name": "api", "namespace": "default"},
			"spec":     map[string]interface{}{"image": "nginx", "replicas": int64(1)},
		}}, nil
	})

	res := appResource("api", map[string]interface{}{"image": "nginx:1.27"})
	_, err = applyResource(context.Background(), dyn, "default", res, false, nil, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "replicas")
	assert.Contains(t, err.Error(), "may not be a loss at all",
		"the schema was unreadable, so the explanation cannot be certain")

	_, err = applyResource(context.Background(), dyn, "default", res, false, nil, false)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "may not be a loss at all",
		"with the schema read there is nothing to hedge")
}

// A dry run reports what the real one would do, so it refuses the same thing:
// with one resource defined twice there is no accurate report to print.
func TestRefuseDuplicateResources_CatchesTheSameThingTwice(t *testing.T) {
	plans := []applyPlan{
		{project: "acme", namespace: "acme-test",
			resources: []manifest.Resource{appResource("api", map[string]interface{}{"image": "nginx"})}},
		{project: "acme", namespace: "acme-test",
			resources: []manifest.Resource{appResource("api", map[string]interface{}{"image": "nginx:1.27"})}},
	}
	err := refuseDuplicateResources(plans)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "App/api in acme-test")

	// The same name in another namespace is two resources, not one.
	fine := []applyPlan{
		{project: "acme", namespace: "acme-test",
			resources: []manifest.Resource{appResource("api", map[string]interface{}{"image": "nginx"})}},
		{project: "acme", namespace: "acme-prod",
			resources: []manifest.Resource{appResource("api", map[string]interface{}{"image": "nginx"})}},
	}
	assert.NoError(t, refuseDuplicateResources(fine))
}
