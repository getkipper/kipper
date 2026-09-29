package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/resourcebounds"
)

func runningDeployment(name, ns string, cpu, memReq, memLim string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: name,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(memReq)},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(memLim)},
			},
		}}}}},
	}
}

// The resources GET says, per resource, who set each value, how Kipper sizes
// it, what the container runs with and what the auto-sizer recommends.
func TestResourcesGetDescribesEachResource(t *testing.T) {
	cr := crfake.NewClientBuilder().WithScheme(testScheme()).WithReturnManagedFields().Build()
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging", UID: "uid-web"},
		Spec: kipperv1.AppSpec{Image: "nginx:1.25", Port: 80, Resources: kipperv1.AppResources{
			MemoryRequest: "512Mi", MemoryLimit: "2Gi",
		}},
	}
	if err := crclient.WithFieldOwner(cr, "kip").Create(t.Context(), app); err != nil {
		t.Fatal(err)
	}
	controller := true
	rec := &kipperv1.ResourceTuning{
		ObjectMeta: metav1.ObjectMeta{Name: resourcebounds.TuningName("App", "web"), Namespace: "staging", OwnerReferences: []metav1.OwnerReference{{
			APIVersion: kipperv1.GroupVersion.String(), Kind: "App", Name: "web", UID: app.UID, Controller: &controller,
		}}},
		Spec: kipperv1.ResourceTuningSpec{Kind: "App", Name: "web"},
		Status: kipperv1.ResourceTuningStatus{Recommendation: kipperv1.TunedResources{
			MemoryRequest: "768Mi", MemoryLimit: "2Gi", CPURequest: "200m", CPULimit: "200m",
		}},
	}
	if err := cr.Create(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	handler := &Resources{Client: fake.NewClientset(runningDeployment("web", "staging", "150m", "768Mi", "2Gi")), CRClient: cr}
	resp := getAppResources(t, handler)

	mem := resp.Memory
	if mem == nil || mem.Mode != "bounded" {
		t.Fatalf("memory = %+v, want mode bounded", mem)
	}
	if mem.Request != (resourceValue{Value: "512Mi", Source: "user"}) || mem.Limit != (resourceValue{Value: "2Gi", Source: "user"}) {
		t.Errorf("memory bounds = %+v / %+v, want the user's 512Mi / 2Gi", mem.Request, mem.Limit)
	}
	if mem.Live != (resourcePair{Request: "768Mi", Limit: "2Gi"}) {
		t.Errorf("memory live = %+v, want 768Mi / 2Gi", mem.Live)
	}
	if mem.Recommended == nil || *mem.Recommended != (resourcePair{Request: "768Mi", Limit: "2Gi"}) {
		t.Errorf("memory recommended = %+v, want 768Mi / 2Gi", mem.Recommended)
	}

	cpu := resp.CPU
	if cpu == nil || cpu.Mode != "automatic" || cpu.Request.Source != "unset" {
		t.Fatalf("cpu = %+v, want automatic with nothing set", cpu)
	}
	if cpu.Live != (resourcePair{Request: "150m", Limit: "150m"}) {
		t.Errorf("cpu live = %+v, want 150m / 150m", cpu.Live)
	}
}

// A record left by an earlier App of the same name is not this App's
// recommendation, so the console does not show it.
func TestResourcesGetLeavesOutAnotherOwnersRecommendation(t *testing.T) {
	cr := crfake.NewClientBuilder().WithScheme(testScheme()).Build()
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging", UID: "uid-web"},
		Spec:       kipperv1.AppSpec{Image: "nginx:1.25", Port: 80},
	}
	rec := &kipperv1.ResourceTuning{
		ObjectMeta: metav1.ObjectMeta{Name: resourcebounds.TuningName("App", "web"), Namespace: "staging"},
		Spec:       kipperv1.ResourceTuningSpec{Kind: "App", Name: "web"},
		Status:     kipperv1.ResourceTuningStatus{Recommendation: kipperv1.TunedResources{MemoryRequest: "2Gi", MemoryLimit: "2Gi"}},
	}
	for _, obj := range []crclient.Object{app, rec} {
		if err := cr.Create(t.Context(), obj); err != nil {
			t.Fatal(err)
		}
	}
	handler := &Resources{Client: fake.NewClientset(runningDeployment("web", "staging", "150m", "768Mi", "768Mi")), CRClient: cr}

	if mem := getAppResources(t, handler).Memory; mem == nil || mem.Recommended != nil {
		t.Fatalf("memory = %+v, want details without the other owner's recommendation", mem)
	}
}

func getAppResources(t *testing.T, handler *Resources) resourcesResponse {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/projects/{name}/apps/{app}/resources", handler.Get)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/projects/staging/apps/web/resources", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp resourcesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestServiceResourcesGetDescribesEachResource(t *testing.T) {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop"},
		Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "db",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
			},
		}}}}},
	}
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop"},
		Spec:       kipperv1.ServiceSpec{Resources: kipperv1.ServiceResources{MemoryRequest: "1Gi", MemoryLimit: "1Gi"}},
	}
	s := &Services{Client: fake.NewClientset(sts), CRClient: testCRClient(svc)}
	r := chi.NewRouter()
	r.Get("/api/v1/services/{name}/resources", s.GetResources)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/services/db/resources?namespace=shop", nil))
	var resp resourcesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Memory == nil || resp.Memory.Mode != "fixed" || resp.Memory.Limit.Source != "user" || resp.Memory.Live.Limit != "1Gi" {
		t.Fatalf("memory = %+v, want a user-set fixed 1Gi, live 1Gi", resp.Memory)
	}
	if resp.MemoryLimit != "1Gi" {
		t.Errorf("memory_limit = %q, want the existing field kept", resp.MemoryLimit)
	}
}

// The live block reports what the container has. A request with no limit has
// no cap, so no limit is reported.
func TestLiveValuesKeepAMissingLimitMissing(t *testing.T) {
	live := &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}}
	cpu, _ := describeResources(resourcebounds.Spec{}, live, nil)
	if cpu.Live != (resourcePair{Request: "100m"}) {
		t.Fatalf("cpu live = %+v, want request 100m and no limit", cpu.Live)
	}
}

// A resource is pending while the running container does not yet have what the
// spec asks for, so the console knows to read again after a save.
func TestPendingSaysWhetherTheContainerHasTheSpecsSize(t *testing.T) {
	cases := []struct {
		name             string
		memReq, memLim   string
		liveReq, liveLim string
		noLive           bool
		want             bool
	}{
		{name: "a new fixed size not yet rolled out", memReq: "1Gi", memLim: "1Gi", liveReq: "256Mi", liveLim: "256Mi", want: true},
		{name: "a fixed size the container has", memReq: "1Gi", memLim: "1Gi", liveReq: "1Gi", liveLim: "1Gi"},
		{name: "a tuned request inside the bounds", memReq: "512Mi", memLim: "2Gi", liveReq: "768Mi", liveLim: "2Gi"},
		{name: "a new floor not yet rolled out", memReq: "512Mi", memLim: "2Gi", liveReq: "256Mi", liveLim: "2Gi", want: true},
		{name: "a new limit not yet rolled out", memReq: "512Mi", memLim: "2Gi", liveReq: "768Mi", liveLim: "1Gi", want: true},
		{name: "automatic sizing follows the container", liveReq: "300Mi", liveLim: "300Mi"},
		{name: "no container yet", memReq: "1Gi", memLim: "1Gi", noLive: true, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := resourcebounds.OwnedSpec("", "", tc.memReq, tc.memLim)
			if err != nil {
				t.Fatal(err)
			}
			var live *corev1.ResourceRequirements
			if !tc.noLive {
				live = &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(tc.liveReq)},
					Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(tc.liveLim)},
				}
			}
			_, memory := describeResources(spec, live, nil)
			if memory.Pending != tc.want {
				t.Fatalf("pending = %v, want %v", memory.Pending, tc.want)
			}
		})
	}
}

// A Deployment that cannot be read says nothing about the container, so the
// details are left out rather than describing it as having no size.
func TestResourcesGetLeavesTheDetailsOutWhenTheDeploymentCannotBeRead(t *testing.T) {
	app := &kipperv1.App{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"}, Spec: kipperv1.AppSpec{Image: "nginx", Port: 80}}
	client := fake.NewClientset(runningDeployment("web", "staging", "150m", "256Mi", "256Mi"))
	client.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	handler := &Resources{Client: client, CRClient: testCRClient(app)}
	r := chi.NewRouter()
	r.Get("/projects/{name}/apps/{app}/resources", handler.Get)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/projects/staging/apps/web/resources", nil))
	var resp resourcesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.CPU != nil || resp.Memory != nil {
		t.Fatalf("details = %+v / %+v, want none without the Deployment", resp.CPU, resp.Memory)
	}
}

// Before the first rollout there is no Deployment, and an app sized
// automatically has nothing to wait for.
func TestResourcesGetDescribesAnAppNotYetRolledOut(t *testing.T) {
	app := &kipperv1.App{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"}, Spec: kipperv1.AppSpec{Image: "nginx", Port: 80}}
	handler := &Resources{Client: fake.NewClientset(), CRClient: testCRClient(app)}
	r := chi.NewRouter()
	r.Get("/projects/{name}/apps/{app}/resources", handler.Get)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/projects/staging/apps/web/resources", nil))
	var resp resourcesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Memory == nil || resp.Memory.Pending {
		t.Fatalf("memory = %+v, want details that are not pending", resp.Memory)
	}
}

// A current server always says it takes partial edits, even when it could not
// build the per-resource details, so the console never mistakes it for an
// older one and sends values the user did not touch.
func TestResourcesGetAlwaysSaysItTakesPartialEdits(t *testing.T) {
	app := &kipperv1.App{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"}, Spec: kipperv1.AppSpec{Image: "nginx", Port: 80}}
	// No Deployment and no tuning record: the details are partial at best.
	handler := &Resources{Client: fake.NewClientset(), CRClient: testCRClient(app)}
	r := chi.NewRouter()
	r.Get("/projects/{name}/apps/{app}/resources", handler.Get)

	for _, path := range []string{"/projects/staging/apps/web/resources", "/projects/staging/apps/gone/resources"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		var resp resourcesResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if !resp.PartialEdits {
			t.Errorf("%s: partial_edits = false, want true", path)
		}
	}
}
