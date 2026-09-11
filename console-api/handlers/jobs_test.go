package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/middleware"
)

// The collision fixture: two projects each holding a job of the same name, and
// a caller who belongs to both. Every test below turns on which of the two a
// request means.
const (
	collidingJob = "nightly-cleanup"
	shopNS       = "shop-prod"
	blogNS       = "blog-prod"
)

// withCollisionResolver wires a resolver where dev@test.com holds shopRole in
// shop-prod and blogRole in blog-prod. An empty role means no membership.
func withCollisionResolver(t *testing.T, shopRole, blogRole string) {
	t.Helper()
	namespaces := []*corev1.Namespace{projectNS(shopNS, shopNS), projectNS(blogNS, blogNS)}
	client := fake.NewClientset(namespaces[0], namespaces[1])
	roles := middleware.NewRoleStore(fake.NewClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-users", Namespace: "kipper-system"},
		Data:       map[string]string{"users": `{"root@test.com":"admin","dev@test.com":"member"}`},
	}))
	members := stubMemberSource{}
	if shopRole != "" {
		members[shopNS] = map[string]string{"dev@test.com": shopRole}
	}
	if blogRole != "" {
		members[blogNS] = map[string]string{"dev@test.com": blogRole}
	}
	prev := projectResolver
	SetProjectResolver(middleware.NewProjectAccessResolver(client, roles, members, handlerOwners(t, namespaces...)))
	t.Cleanup(func() { projectResolver = prev })
}

func jobCR(namespace, name, schedule string) *kipperv1.Job {
	return &kipperv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       kipperv1.JobSpec{Image: "busybox", Schedule: schedule},
	}
}

// collidingCronJob is the child the reconciler builds for the colliding job,
// which is what Trigger copies a template from and what the resources verbs read.
func collidingCronJob(namespace string) *batchv1.CronJob {
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      collidingJob,
			Namespace: namespace,
			Labels:    map[string]string{"app": collidingJob, kipperLabel: kipperValue},
		},
		Spec: batchv1.CronJobSpec{
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: collidingJob, Image: "busybox"}}},
					},
				},
			},
		},
	}
}

// jobRequest builds a request against the bare-name routes, which carry the job
// name in {name} and no namespace at all.
func jobRequest(method, target, email, jobName, body string) *http.Request {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", jobName)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = context.WithValue(ctx, middleware.UserContextKey, &middleware.Claims{Email: email})
	return req.WithContext(ctx)
}

// namespacedJobRequest builds a request against the routes under
// /projects/{name}/jobs/{job}, where {name} is the namespace.
func namespacedJobRequest(method, target, email, namespace, jobName, body string) *http.Request {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", namespace)
	rctx.URLParams.Add("job", jobName)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = context.WithValue(ctx, middleware.UserContextKey, &middleware.Claims{Email: email})
	return req.WithContext(ctx)
}

func triggeredJobNamespaces(t *testing.T, client *fake.Clientset) []string {
	t.Helper()
	list, err := client.BatchV1().Jobs("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing triggered jobs: %v", err)
	}
	var out []string
	for _, j := range list.Items {
		if j.Labels["kipper.run/triggered-by"] == "console" {
			out = append(out, j.Namespace)
		}
	}
	return out
}

func TestTriggerRefusesAnAmbiguousName(t *testing.T) {
	withCollisionResolver(t, "deployer", "deployer")
	client := fake.NewClientset(collidingCronJob(shopNS), collidingCronJob(blogNS))
	h := &Jobs{
		Client:   client,
		CRClient: testCRClient(jobCR(shopNS, collidingJob, "0 3 * * *"), jobCR(blogNS, collidingJob, "0 4 * * *")),
	}

	rec := httptest.NewRecorder()
	h.Trigger(rec, jobRequest("POST", "/api/v1/jobs/"+collidingJob+"/trigger", "dev@test.com", collidingJob, ""))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
	for _, ns := range []string{shopNS, blogNS} {
		if !strings.Contains(rec.Body.String(), ns) {
			t.Errorf("the 409 should name %q so the caller can pick one; body %s", ns, rec.Body.String())
		}
	}
	if got := triggeredJobNamespaces(t, client); len(got) != 0 {
		t.Errorf("an ambiguous name must trigger nothing, ran in %v", got)
	}
}

func TestTriggerActsOnTheNamespaceInThePath(t *testing.T) {
	withCollisionResolver(t, "deployer", "deployer")
	client := fake.NewClientset(collidingCronJob(shopNS), collidingCronJob(blogNS))
	h := &Jobs{
		Client:   client,
		CRClient: testCRClient(jobCR(shopNS, collidingJob, "0 3 * * *"), jobCR(blogNS, collidingJob, "0 4 * * *")),
	}

	rec := httptest.NewRecorder()
	h.TriggerInNamespace(rec, namespacedJobRequest("POST",
		"/api/v1/projects/"+blogNS+"/jobs/"+collidingJob+"/trigger", "dev@test.com", blogNS, collidingJob, ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	got := triggeredJobNamespaces(t, client)
	if len(got) != 1 || got[0] != blogNS {
		t.Fatalf("triggered in %v, want exactly one run in %q", got, blogNS)
	}
}

func TestTriggerActsWhenOnlyOneTargetIsEligible(t *testing.T) {
	// A viewer in shop-prod and a deployer in blog-prod. Only one of the two
	// same-named jobs can be triggered, so the name is not ambiguous.
	withCollisionResolver(t, "viewer", "deployer")
	client := fake.NewClientset(collidingCronJob(shopNS), collidingCronJob(blogNS))
	h := &Jobs{
		Client:   client,
		CRClient: testCRClient(jobCR(shopNS, collidingJob, "0 3 * * *"), jobCR(blogNS, collidingJob, "0 4 * * *")),
	}

	rec := httptest.NewRecorder()
	h.Trigger(rec, jobRequest("POST", "/api/v1/jobs/"+collidingJob+"/trigger", "dev@test.com", collidingJob, ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	got := triggeredJobNamespaces(t, client)
	if len(got) != 1 || got[0] != blogNS {
		t.Fatalf("triggered in %v, want exactly one run in %q", got, blogNS)
	}
}

func TestTriggerRefusesWhenNoTargetIsEligible(t *testing.T) {
	withCollisionResolver(t, "viewer", "viewer")
	client := fake.NewClientset(collidingCronJob(shopNS))
	h := &Jobs{
		Client:   client,
		CRClient: testCRClient(jobCR(shopNS, collidingJob, "0 3 * * *")),
	}

	rec := httptest.NewRecorder()
	h.Trigger(rec, jobRequest("POST", "/api/v1/jobs/"+collidingJob+"/trigger", "dev@test.com", collidingJob, ""))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body %s", rec.Code, rec.Body.String())
	}
}

func TestTriggerNotFoundWhenTheNameIsUnknown(t *testing.T) {
	withCollisionResolver(t, "deployer", "deployer")
	h := &Jobs{Client: fake.NewClientset(), CRClient: testCRClient()}

	rec := httptest.NewRecorder()
	h.Trigger(rec, jobRequest("POST", "/api/v1/jobs/absent/trigger", "dev@test.com", "absent", ""))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body %s", rec.Code, rec.Body.String())
	}
}

func TestTriggerRefusesAJobWithNoSchedule(t *testing.T) {
	// A one-off job has no CronJob to copy a template from, so there is
	// nothing to run now.
	withCollisionResolver(t, "deployer", "")
	h := &Jobs{
		Client:   fake.NewClientset(),
		CRClient: testCRClient(jobCR(shopNS, "one-off", "")),
	}

	rec := httptest.NewRecorder()
	h.Trigger(rec, jobRequest("POST", "/api/v1/jobs/one-off/trigger", "dev@test.com", "one-off", ""))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body.String())
	}
}

func TestHistoryRefusesAnAmbiguousName(t *testing.T) {
	withCollisionResolver(t, "deployer", "deployer")
	h := &Jobs{
		Client:   fake.NewClientset(),
		CRClient: testCRClient(jobCR(shopNS, collidingJob, "0 3 * * *"), jobCR(blogNS, collidingJob, "0 4 * * *")),
	}

	rec := httptest.NewRecorder()
	h.History(rec, jobRequest("GET", "/api/v1/jobs/"+collidingJob+"/history", "dev@test.com", collidingJob, ""))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
}

func TestHistoryReadsOnlyTheResolvedNamespace(t *testing.T) {
	withCollisionResolver(t, "deployer", "deployer")
	runs := fake.NewClientset(
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{
			Name: collidingJob + "-1", Namespace: shopNS,
			Labels: map[string]string{"app": collidingJob, kipperLabel: kipperValue},
		}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{
			Name: collidingJob + "-2", Namespace: blogNS,
			Labels: map[string]string{"app": collidingJob, kipperLabel: kipperValue},
		}},
	)
	h := &Jobs{
		Client:   runs,
		CRClient: testCRClient(jobCR(shopNS, collidingJob, "0 3 * * *"), jobCR(blogNS, collidingJob, "0 4 * * *")),
	}

	rec := httptest.NewRecorder()
	h.HistoryInNamespace(rec, namespacedJobRequest("GET",
		"/api/v1/projects/"+blogNS+"/jobs/"+collidingJob+"/history", "dev@test.com", blogNS, collidingJob, ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var got []jobResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding history: %v", err)
	}
	if len(got) != 1 || got[0].Namespace != blogNS {
		t.Fatalf("history = %+v, want only the run in %q", got, blogNS)
	}
}

func TestGetResourcesRefusesAnAmbiguousName(t *testing.T) {
	withCollisionResolver(t, "deployer", "deployer")
	h := &Jobs{
		Client:   fake.NewClientset(collidingCronJob(shopNS), collidingCronJob(blogNS)),
		CRClient: testCRClient(jobCR(shopNS, collidingJob, "0 3 * * *"), jobCR(blogNS, collidingJob, "0 4 * * *")),
	}

	rec := httptest.NewRecorder()
	h.GetResources(rec, jobRequest("GET", "/api/v1/jobs/"+collidingJob+"/resources", "dev@test.com", collidingJob, ""))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
}

func TestGetResourcesRefusesANonMember(t *testing.T) {
	// The route carried no capability check at all, so a caller outside every
	// project read a 200 with an empty body and could not tell the two apart.
	withCollisionResolver(t, "", "")
	h := &Jobs{
		Client:   fake.NewClientset(collidingCronJob(shopNS)),
		CRClient: testCRClient(jobCR(shopNS, collidingJob, "0 3 * * *")),
	}

	rec := httptest.NewRecorder()
	h.GetResources(rec, jobRequest("GET", "/api/v1/jobs/"+collidingJob+"/resources", "dev@test.com", collidingJob, ""))

	if rec.Code == http.StatusOK {
		t.Fatalf("a non-member read 200; body %s", rec.Body.String())
	}
}

func TestUpdateResourcesRefusesAnAmbiguousName(t *testing.T) {
	withCollisionResolver(t, "deployer", "deployer")
	client := fake.NewClientset(collidingCronJob(shopNS), collidingCronJob(blogNS))
	h := &Jobs{
		Client:   client,
		CRClient: testCRClient(jobCR(shopNS, collidingJob, "0 3 * * *"), jobCR(blogNS, collidingJob, "0 4 * * *")),
	}

	rec := httptest.NewRecorder()
	h.UpdateResources(rec, jobRequest("PUT", "/api/v1/jobs/"+collidingJob+"/resources",
		"dev@test.com", collidingJob, `{"memory_limit":"512Mi","cpu_limit":"500m"}`))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
}

// unmanagedCronJob carries the colliding name with no Kipper provenance, the way
// one created out of band or left behind by something else would.
func unmanagedCronJob(namespace string) *batchv1.CronJob {
	cj := collidingCronJob(namespace)
	cj.Labels = map[string]string{"app": collidingJob}
	return cj
}

func TestTriggerRefusesACronJobKipperDoesNotOwn(t *testing.T) {
	withCollisionResolver(t, "deployer", "")
	client := fake.NewClientset(unmanagedCronJob(shopNS))
	h := &Jobs{Client: client, CRClient: testCRClient(jobCR(shopNS, collidingJob, "0 3 * * *"))}

	rec := httptest.NewRecorder()
	h.Trigger(rec, jobRequest("POST", "/api/v1/jobs/"+collidingJob+"/trigger", "dev@test.com", collidingJob, ""))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
	if got := triggeredJobNamespaces(t, client); len(got) != 0 {
		t.Errorf("a cronjob Kipper does not own must not be copied, ran in %v", got)
	}
}

// A cluster admin resolves as owner of every namespace, so their eligible set
// is the whole cluster and any collision anywhere makes a bare name ambiguous.
// That is the intended answer, and the person most likely to be holding the
// console during an upgrade is the one who meets it.
func TestAnAdminSeesEveryCollision(t *testing.T) {
	withCollisionResolver(t, "", "")
	client := fake.NewClientset(collidingCronJob(shopNS), collidingCronJob(blogNS))
	h := &Jobs{
		Client:   client,
		CRClient: testCRClient(jobCR(shopNS, collidingJob, "0 3 * * *"), jobCR(blogNS, collidingJob, "0 4 * * *")),
	}

	rec := httptest.NewRecorder()
	h.Trigger(rec, jobRequest("POST", "/api/v1/jobs/"+collidingJob+"/trigger", "root@test.com", collidingJob, ""))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
	for _, ns := range []string{shopNS, blogNS} {
		if !strings.Contains(rec.Body.String(), ns) {
			t.Errorf("the 409 should name %q; body %s", ns, rec.Body.String())
		}
	}
	if got := triggeredJobNamespaces(t, client); len(got) != 0 {
		t.Errorf("an ambiguous name must trigger nothing, ran in %v", got)
	}
}

func TestCreateReturnsTheNamespaceItUsed(t *testing.T) {
	// An empty namespace defaults to "default" on the way in. Echoing the empty
	// string back leaves the client holding a job it cannot address.
	withCollisionResolver(t, "deployer", "deployer")
	h := &Jobs{Client: fake.NewClientset(), CRClient: testCRClient()}

	req := jobRequest("POST", "/api/v1/jobs", "root@test.com", "",
		`{"name":"backup","image":"busybox"}`)
	rec := httptest.NewRecorder()
	h.Create(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body.String())
	}
	var got jobResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding the created job: %v", err)
	}
	if got.Namespace != "default" {
		t.Fatalf("namespace = %q, want %q", got.Namespace, "default")
	}
}
