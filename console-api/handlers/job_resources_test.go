package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// The CronJob is a child the reconciler rebuilds from the Job CR, so a change
// written to it is gone by the next reconcile. See
// TestReconcileCronJob_RebuildsResourcesFromTheJobCR in the controllers package.
func TestUpdateResourcesWritesTheJobCR(t *testing.T) {
	withCollisionResolver(t, "deployer", "")
	crClient := testCRClient(jobCR(shopNS, collidingJob, "0 3 * * *"))
	h := &Jobs{
		Client:    fake.NewClientset(collidingCronJob(shopNS)),
		CRClient:  crClient,
		Resources: &Resources{Client: fake.NewClientset(), CRClient: crClient},
	}

	rec := httptest.NewRecorder()
	h.UpdateResources(rec, jobRequest("PUT", "/api/v1/jobs/"+collidingJob+"/resources",
		"dev@test.com", collidingJob, `{"memory_limit":"512Mi","cpu_limit":"500m"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}

	var job kipperv1.Job
	if err := crClient.Get(context.Background(),
		crclient.ObjectKey{Namespace: shopNS, Name: collidingJob}, &job); err != nil {
		t.Fatalf("reading the job back: %v", err)
	}
	if job.Spec.Resources.MemoryLimit != "512Mi" {
		t.Errorf("memory limit on the CR = %q, want 512Mi; a write the reconciler reverts is not a write",
			job.Spec.Resources.MemoryLimit)
	}
	if job.Spec.Resources.CPULimit != "500m" {
		t.Errorf("cpu limit on the CR = %q, want 500m", job.Spec.Resources.CPULimit)
	}
}

func TestGetResourcesReadsTheJobCR(t *testing.T) {
	withCollisionResolver(t, "deployer", "")
	job := jobCR(shopNS, collidingJob, "0 3 * * *")
	job.Spec.Resources = kipperv1.JobResources{MemoryLimit: "512Mi", CPULimit: "500m"}
	crClient := testCRClient(job)
	h := &Jobs{
		Client:    fake.NewClientset(),
		CRClient:  crClient,
		Resources: &Resources{Client: fake.NewClientset(), CRClient: crClient},
	}

	rec := httptest.NewRecorder()
	h.GetResources(rec, jobRequest("GET", "/api/v1/jobs/"+collidingJob+"/resources", "dev@test.com", collidingJob, ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var got resourcesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding resources: %v", err)
	}
	if got.MemoryLimit != "512Mi" || got.CPULimit != "500m" {
		t.Fatalf("resources = %+v, want the CR's values", got)
	}
}

// A job with nothing pinned runs on the reconciler's fallback, so that is what
// the editor has to show rather than an empty box.
func TestGetResourcesShowsWhatAnUnpinnedJobRunsOn(t *testing.T) {
	withCollisionResolver(t, "deployer", "")
	crClient := testCRClient(jobCR(shopNS, collidingJob, "0 3 * * *"))
	h := &Jobs{
		Client:    fake.NewClientset(),
		CRClient:  crClient,
		Resources: &Resources{Client: fake.NewClientset(), CRClient: crClient},
	}

	rec := httptest.NewRecorder()
	h.GetResources(rec, jobRequest("GET", "/api/v1/jobs/"+collidingJob+"/resources", "dev@test.com", collidingJob, ""))

	var got resourcesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding resources: %v", err)
	}
	if got.MemoryLimit != "128Mi" || got.CPULimit != "100m" {
		t.Fatalf("resources = %+v, want the reconciler's 128Mi/100m fallback", got)
	}
}

// A one-off job's run is created from the CR once and never patched, and the
// reconciler may already be mid-pass when a write is accepted, so there is no
// window a caller can be held to. Refusing is the only answer that is true
// whichever way the race falls.
func TestUpdateResourcesRefusesAOneOffJob(t *testing.T) {
	withCollisionResolver(t, "deployer", "")
	for _, tc := range []struct {
		name string
		job  *kipperv1.Job
		run  []runtime.Object
	}{
		{name: "before it runs", job: jobCR(shopNS, "one-off", "")},
		{
			name: "while it runs",
			job:  jobCR(shopNS, "one-off", ""),
			run:  []runtime.Object{&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "one-off", Namespace: shopNS}}},
		},
		{name: "after its run was cleaned up", job: ranOnce(jobCR(shopNS, "one-off", ""))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			crClient := testCRClient(tc.job)
			h := &Jobs{
				Client:    fake.NewClientset(tc.run...),
				CRClient:  crClient,
				Resources: &Resources{Client: fake.NewClientset(tc.run...), CRClient: crClient},
			}

			rec := httptest.NewRecorder()
			h.UpdateResources(rec, jobRequest("PUT", "/api/v1/jobs/one-off/resources",
				"dev@test.com", "one-off", `{"memory_limit":"512Mi","cpu_limit":"500m"}`))

			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// A scheduled job takes the change, because the reconciler rebuilds its CronJob
// from the CR on every pass.
func TestUpdateResourcesAcceptsAScheduledJob(t *testing.T) {
	withCollisionResolver(t, "deployer", "")
	crClient := testCRClient(jobCR(shopNS, collidingJob, "0 3 * * *"))
	h := &Jobs{
		Client:    fake.NewClientset(collidingCronJob(shopNS)),
		CRClient:  crClient,
		Resources: &Resources{Client: fake.NewClientset(), CRClient: crClient},
	}

	rec := httptest.NewRecorder()
	h.UpdateResources(rec, jobRequest("PUT", "/api/v1/jobs/"+collidingJob+"/resources",
		"dev@test.com", collidingJob, `{"memory_limit":"512Mi","cpu_limit":"500m"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
}

func ranOnce(job *kipperv1.Job) *kipperv1.Job {
	job.Status.LastRun = &metav1.Time{Time: time.Unix(1757000000, 0)}
	job.Status.Phase = "Completed"
	return job
}

// An unreadable job is not an unpinned one, and an unwritable one is not a
// missing one. The pair used to answer 200 with an empty body and 404, so the
// caller could not tell an outage from a job with nothing set.
func TestResourceVerbsReportAnOutageRatherThanEmptiness(t *testing.T) {
	withCollisionResolver(t, "deployer", "")
	unreadable := interceptor.Funcs{
		Get: func(context.Context, crclient.WithWatch, crclient.ObjectKey, crclient.Object, ...crclient.GetOption) error {
			return apierrors.NewInternalError(errors.New("etcd is unreachable"))
		},
	}
	resolving := testCRClient(jobCR(shopNS, collidingJob, "0 3 * * *"))
	failing := crfake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(jobCR(shopNS, collidingJob, "0 3 * * *")).
		WithInterceptorFuncs(unreadable).Build()
	h := &Jobs{
		Client:    fake.NewClientset(),
		CRClient:  resolving,
		Resources: &Resources{Client: fake.NewClientset(), CRClient: failing},
	}

	read := httptest.NewRecorder()
	h.GetResources(read, jobRequest("GET", "/api/v1/jobs/"+collidingJob+"/resources", "dev@test.com", collidingJob, ""))
	if read.Code != http.StatusInternalServerError {
		t.Errorf("read status = %d, want 500; body %s", read.Code, read.Body.String())
	}

	write := httptest.NewRecorder()
	h.UpdateResources(write, jobRequest("PUT", "/api/v1/jobs/"+collidingJob+"/resources",
		"dev@test.com", collidingJob, `{"memory_limit":"512Mi","cpu_limit":"500m"}`))
	if write.Code != http.StatusInternalServerError {
		t.Errorf("write status = %d, want 500; body %s", write.Code, write.Body.String())
	}
}

// The namespaced route names a Job CR outright, so a missing one is a missing
// job and not a job with nothing pinned. Answering 200 with an empty form is
// what the console reads as "safe to edit", which is the state the read-failure
// handling exists to prevent.
func TestNamespacedResourcesReadReportsAMissingJob(t *testing.T) {
	withCollisionResolver(t, "deployer", "")
	crClient := testCRClient()
	res := &Resources{Client: fake.NewClientset(), CRClient: crClient}

	rec := httptest.NewRecorder()
	req := namespacedJobRequest("GET",
		"/api/v1/projects/"+shopNS+"/jobs/"+collidingJob+"/resources", "dev@test.com", shopNS, collidingJob, "")
	res.GetByParam("job", ResourceKindJob)(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body %s", rec.Code, rec.Body.String())
	}
}

// The kind is decided on the object that is written, so a job that turns into a
// one-off between the two reads cannot slip through as a scheduled one. The
// interceptor is that other writer: the stored job is scheduled, the read the
// handler gets is not.
func TestUpdateResourcesDecidesOnTheObjectItWrites(t *testing.T) {
	withCollisionResolver(t, "deployer", "")
	turnsOneOff := interceptor.Funcs{
		Get: func(ctx context.Context, c crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if job, ok := obj.(*kipperv1.Job); ok {
				job.Spec.Schedule = ""
			}
			return nil
		},
	}
	crClient := crfake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(jobCR(shopNS, collidingJob, "0 3 * * *")).
		WithInterceptorFuncs(turnsOneOff).Build()
	h := &Jobs{
		Client:    fake.NewClientset(),
		CRClient:  testCRClient(jobCR(shopNS, collidingJob, "0 3 * * *")),
		Resources: &Resources{Client: fake.NewClientset(), CRClient: crClient},
	}

	rec := httptest.NewRecorder()
	h.UpdateResources(rec, jobRequest("PUT", "/api/v1/jobs/"+collidingJob+"/resources",
		"dev@test.com", collidingJob, `{"memory_limit":"512Mi","cpu_limit":"500m"}`))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
}

// A job someone else changed while this write was in flight is refused rather
// than overwritten, because the update carries the resourceVersion it read.
func TestUpdateResourcesRefusesAJobThatChangedUnderIt(t *testing.T) {
	withCollisionResolver(t, "deployer", "")
	stale := interceptor.Funcs{
		Update: func(context.Context, crclient.WithWatch, crclient.Object, ...crclient.UpdateOption) error {
			return apierrors.NewConflict(
				schema.GroupResource{Group: "kipper.run", Resource: "jobs"}, collidingJob,
				errors.New("the object has been modified"))
		},
	}
	crClient := crfake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(jobCR(shopNS, collidingJob, "0 3 * * *")).
		WithInterceptorFuncs(stale).Build()
	h := &Jobs{
		Client:    fake.NewClientset(),
		CRClient:  testCRClient(jobCR(shopNS, collidingJob, "0 3 * * *")),
		Resources: &Resources{Client: fake.NewClientset(), CRClient: crClient},
	}

	rec := httptest.NewRecorder()
	h.UpdateResources(rec, jobRequest("PUT", "/api/v1/jobs/"+collidingJob+"/resources",
		"dev@test.com", collidingJob, `{"memory_limit":"512Mi","cpu_limit":"500m"}`))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "read it again") {
		t.Errorf("the conflict should say what to do; body %s", rec.Body.String())
	}
}
