package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

// The env tests all use one app in one namespace; helpers hardcode them so
// there is no constant-argument noise.
const (
	envTestApp = "api"
	envTestNS  = "blog"
)

func envApp() *kipperv1.App {
	return &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: envTestApp, Namespace: envTestNS},
		Spec:       kipperv1.AppSpec{Image: "api:v1"},
	}
}

// restartStatus drives the handler with an App carrying the generation the
// controller last published, and a Deployment whose pod template names one.
// Those two names are the whole question the banner asks.
func restartStatus(t *testing.T, publishedEnv string, deployed bool, podTemplateEnv string) bool {
	t.Helper()

	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: envTestApp, Namespace: envTestNS},
		Status:     kipperv1.AppStatus{PublishedEnv: publishedEnv},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, kipperv1.AddToScheme(scheme))
	crc := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app).Build()

	var core []runtime.Object
	if deployed {
		container := corev1.Container{Name: envTestApp}
		if podTemplateEnv != "" {
			container.EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: podTemplateEnv},
			}}}
		}
		core = append(core, &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: envTestApp, Namespace: envTestNS},
			Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{container}},
			}},
		})
	}

	e := &Env{Client: fake.NewClientset(core...), CRClient: crc}
	r := chi.NewRouter()
	r.Get("/projects/{name}/apps/{app}/env/status", e.RestartStatus)
	req := httptest.NewRequest(http.MethodGet, "/projects/"+envTestNS+"/apps/"+envTestApp+"/env/status", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		RestartPending bool `json:"restartPending"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body.RestartPending
}

func gen(digest string) string {
	return secretname.EnvGeneration(secretname.KindApp, envTestApp, digest)
}

func TestEnvRestartStatus(t *testing.T) {
	t.Run("nothing published yet means no restart pending", func(t *testing.T) {
		assert.False(t, restartStatus(t, "", true, gen("aaaaaaaaaaaa")),
			"the window between creating a workload and its first pass is not staleness")
	})

	t.Run("the pod is on the published environment", func(t *testing.T) {
		assert.False(t, restartStatus(t, gen("aaaaaaaaaaaa"), true, gen("aaaaaaaaaaaa")))
	})

	t.Run("the pod is on an older environment", func(t *testing.T) {
		assert.True(t, restartStatus(t, gen("bbbbbbbbbbbb"), true, gen("aaaaaaaaaaaa")),
			"a restart would move it onto what was published")
	})

	t.Run("a secrets change is covered like any other", func(t *testing.T) {
		// A workload's own secrets are inside the published environment, so
		// editing one produces a different generation. This is the case the
		// timestamp scan covered separately and this must not lose.
		assert.True(t, restartStatus(t, gen("cccccccccccc"), true, gen("aaaaaaaaaaaa")))
	})

	t.Run("a pod template naming no generation is pending", func(t *testing.T) {
		assert.True(t, restartStatus(t, gen("aaaaaaaaaaaa"), true, ""),
			"a workload from before generations shipped moves onto one when restarted")
	})

	t.Run("no deployment yet means no restart pending", func(t *testing.T) {
		assert.False(t, restartStatus(t, gen("aaaaaaaaaaaa"), false, ""),
			"nothing is deployed, so there is nothing a restart would change")
	})
}

func TestEnvUpdateWritesSpecEnv(t *testing.T) {
	cr := testCRClient(envApp())
	e := &Env{Client: fake.NewClientset(), CRClient: cr}

	body, _ := json.Marshal(map[string]string{"LOG": "debug"})
	r := chi.NewRouter()
	r.Put("/projects/{name}/apps/{app}/env", e.Update)
	req := httptest.NewRequest(http.MethodPut, "/projects/"+envTestNS+"/apps/"+envTestApp+"/env", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var got kipperv1.App
	require.NoError(t, cr.Get(t.Context(), crclient.ObjectKey{Namespace: envTestNS, Name: envTestApp}, &got))
	assert.Equal(t, "debug", got.Spec.Env["LOG"], "the handler persists spec.env; the reconciler stamps the change")
}

// The addresses an app's links inject are direct env: entries, and they are
// meant to be — they are derived each pass and deliberately take precedence
// over the env Secret. Reporting them as conflicts marks every linked app as
// broken, and the offered fix strips the thing making its links work.
func TestLinkAddressesAreNotReportedAsEnvConflicts(t *testing.T) {
	appCR := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "hrportal-backend", Namespace: "hrportal-test"},
		Spec: kipperv1.AppSpec{
			Image: "hrportal:v1", Port: 8080,
			Links: []kipperv1.AppLink{{App: "docuseal", Namespace: "docuseal-test"}},
		},
	}
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "hrportal-backend", Namespace: "hrportal-test"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "hrportal-backend", Env: []corev1.EnvVar{
				{Name: "DOCUSEAL_URL", Value: "http://docuseal.docuseal-test.svc.cluster.local:3000"},
				{Name: "HAND_PLACED", Value: "this one really does shadow the secret"},
			}}},
		}}},
	}

	scheme := runtime.NewScheme()
	require.NoError(t, kipperv1.AddToScheme(scheme))
	h := &Env{
		Client:   fake.NewClientset(deploy),
		CRClient: crfake.NewClientBuilder().WithScheme(scheme).WithObjects(appCR).Build(),
	}

	r := chi.NewRouter()
	r.Get("/projects/{name}/apps/{app}/env/conflicts", h.DirectEnvConflicts)
	r.Delete("/projects/{name}/apps/{app}/env/conflicts", h.RemoveDirectEnvConflicts)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/projects/hrportal-test/apps/hrportal-backend/env/conflicts", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var conflicts []string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &conflicts))
	assert.Equal(t, []string{"HAND_PLACED"}, conflicts,
		"only the entry nobody derived is a conflict")

	// And clearing the conflicts leaves the link's address where it is.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("DELETE", "/projects/hrportal-test/apps/hrportal-backend/env/conflicts", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	after, err := h.Client.AppsV1().Deployments("hrportal-test").Get(context.Background(), "hrportal-backend", metav1.GetOptions{})
	require.NoError(t, err)
	names := []string{}
	for _, ev := range after.Spec.Template.Spec.Containers[0].Env {
		names = append(names, ev.Name)
	}
	assert.Equal(t, []string{"DOCUSEAL_URL"}, names,
		"the reconciler's own entry survives a conflict clear; it would only come back anyway")
}

// The check has to run against the object being written. A link created between
// the first read and a retry would slip past a check made once, and the app
// would end up holding both a stored value and a link — reachable only by losing
// a race, which is exactly when nobody is watching.
func TestAnEnvUpdateCannotRaceALinkIntoOwningTheSameVariable(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, kipperv1.AddToScheme(scheme))
	appCR := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "hrportal-backend", Namespace: "hrportal-test"},
		Spec:       kipperv1.AppSpec{Image: "hrportal:v1", Port: 8080},
	}

	// The link lands after the handler's first read, so only a check inside the
	// retry — against the freshly fetched object — can see it.
	raced := false
	crClient := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(appCR).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
				if err := c.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				if a, ok := obj.(*kipperv1.App); ok && !raced {
					raced = true
					a.Spec.Links = []kipperv1.AppLink{{App: "docuseal", Namespace: "docuseal-test"}}
				}
				return nil
			},
		}).Build()

	h := &Env{Client: fake.NewClientset(), CRClient: crClient}
	r := chi.NewRouter()
	r.Put("/projects/{name}/apps/{app}/env", h.Update)

	req := httptest.NewRequest("PUT", "/projects/hrportal-test/apps/hrportal-backend/env",
		strings.NewReader(`{"DOCUSEAL_URL":"https://proxy.internal/docuseal","LOG_LEVEL":"debug"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusConflict, rec.Code,
		"a variable the app's links own must be refused even when the link arrives mid-request")
	assert.Contains(t, rec.Body.String(), "DOCUSEAL_URL")
}

// An app linked before addresses became derived carries both a stored address
// and the link, and nothing migrates it. The editor posts the whole map back for
// any edit, so refusing on the stale key would lock the operator out of every
// variable on that app until they worked out which one the error meant.
//
// A value already stored and resent unchanged is not somebody setting it — the
// link has been providing the address regardless — so it is dropped. A changed
// one is a real attempt to set it and is still refused.
func TestAStaleStoredAddressIsClearedRatherThanBlockingEveryEdit(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, kipperv1.AddToScheme(scheme))
	appCR := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "hrportal-backend", Namespace: "hrportal-test"},
		Spec: kipperv1.AppSpec{
			Image: "p:v1", Port: 8080,
			Env:   map[string]string{"DOCUSEAL_URL": "http://stored.from.before:3000", "LOG_LEVEL": "debug"},
			Links: []kipperv1.AppLink{{App: "docuseal", Namespace: "docuseal-test"}},
		},
	}
	crClient := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(appCR).Build()
	h := &Env{Client: fake.NewClientset(), CRClient: crClient}
	r := chi.NewRouter()
	r.Put("/projects/{name}/apps/{app}/env", h.Update)

	put := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest("PUT",
			"/projects/hrportal-test/apps/hrportal-backend/env", strings.NewReader(body)))
		return rec
	}

	// The editor posts everything back, stale key included, to change LOG_LEVEL.
	rec := put(`{"DOCUSEAL_URL":"http://stored.from.before:3000","LOG_LEVEL":"info"}`)
	require.Equal(t, http.StatusOK, rec.Code,
		"an edit to another variable must not be refused because of a value the link already supersedes")

	var after kipperv1.App
	require.NoError(t, crClient.Get(context.Background(),
		crclient.ObjectKey{Namespace: "hrportal-test", Name: "hrportal-backend"}, &after))
	assert.Equal(t, "info", after.Spec.Env["LOG_LEVEL"], "the edit lands")
	assert.NotContains(t, after.Spec.Env, "DOCUSEAL_URL",
		"and the dead copy is cleared, so the app stops carrying two answers")

	// Deliberately setting it to something else is still refused.
	rec = put(`{"DOCUSEAL_URL":"https://proxy.internal/docuseal","LOG_LEVEL":"info"}`)
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "Remove it here to use the link's address")
}
