package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// A Function's cron trigger creates a CronJob called <function>-cron, which is
// a name a Job CR may also have. Both carry Kipper's managed-by label, so it is
// the resource-type that separates them: the Job's verbs must not copy, read or
// rewrite a Function's child.
func functionCronJob(namespace, name string) *batchv1.CronJob {
	cj := collidingCronJob(namespace)
	cj.Name = name
	cj.Labels = map[string]string{
		"app":                      "report",
		kipperLabel:                kipperValue,
		"kipper.run/resource-type": "function",
	}
	return cj
}

const functionChildName = "report-cron"

func TestTriggerRefusesAChildAnotherWorkloadOwns(t *testing.T) {
	withCollisionResolver(t, "deployer", "")
	client := fake.NewClientset(functionCronJob(shopNS, functionChildName))
	h := &Jobs{
		Client:   client,
		CRClient: testCRClient(jobCR(shopNS, functionChildName, "0 3 * * *")),
	}

	rec := httptest.NewRecorder()
	h.Trigger(rec, jobRequest("POST", "/api/v1/jobs/"+functionChildName+"/trigger",
		"dev@test.com", functionChildName, ""))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
	if got := triggeredJobNamespaces(t, client); len(got) != 0 {
		t.Errorf("another workload's template must not be run, ran in %v", got)
	}
}

func TestGetResourcesReadsTheCRWhenAnotherWorkloadHoldsTheChildKey(t *testing.T) {
	// Resources live on the Job CR, which says what the job is configured to
	// run with whether or not its child can be built. The blocked child is a
	// reconcile problem, and trigger is where the caller meets it.
	withCollisionResolver(t, "deployer", "")
	job := jobCR(shopNS, functionChildName, "0 3 * * *")
	job.Spec.Resources = kipperv1.JobResources{MemoryLimit: "512Mi", CPULimit: "500m"}
	crClient := testCRClient(job)
	h := &Jobs{
		Client:    fake.NewClientset(functionCronJob(shopNS, functionChildName)),
		CRClient:  crClient,
		Resources: &Resources{Client: fake.NewClientset(), CRClient: crClient},
	}

	rec := httptest.NewRecorder()
	h.GetResources(rec, jobRequest("GET", "/api/v1/jobs/"+functionChildName+"/resources",
		"dev@test.com", functionChildName, ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var got resourcesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding resources: %v", err)
	}
	if got.MemoryLimit != "512Mi" {
		t.Fatalf("memory limit = %q, want the CR's 512Mi", got.MemoryLimit)
	}
}

func TestUpdateResourcesWritesTheCRWhenAnotherWorkloadHoldsTheChildKey(t *testing.T) {
	// The write lands on the CR, so it is what the job runs with as soon as the
	// name collision is resolved. Refusing it would leave the job unfixable
	// from the console.
	withCollisionResolver(t, "deployer", "")
	crClient := testCRClient(jobCR(shopNS, functionChildName, "0 3 * * *"))
	h := &Jobs{
		Client:    fake.NewClientset(functionCronJob(shopNS, functionChildName)),
		CRClient:  crClient,
		Resources: &Resources{Client: fake.NewClientset(), CRClient: crClient},
	}

	rec := httptest.NewRecorder()
	h.UpdateResources(rec, jobRequest("PUT", "/api/v1/jobs/"+functionChildName+"/resources",
		"dev@test.com", functionChildName, `{"memory_limit":"512Mi","cpu_limit":"500m"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}

	var job kipperv1.Job
	if err := crClient.Get(context.Background(),
		crclient.ObjectKey{Namespace: shopNS, Name: functionChildName}, &job); err != nil {
		t.Fatalf("reading the job back: %v", err)
	}
	if job.Spec.Resources.MemoryLimit != "512Mi" {
		t.Fatalf("memory limit on the CR = %q, want 512Mi", job.Spec.Resources.MemoryLimit)
	}
}

// A child adopted by this job carries a controller reference to it, which is
// the strong half of the rule and must keep working when the labels do not.
func TestTriggerAcceptsAnAdoptedChild(t *testing.T) {
	withCollisionResolver(t, "deployer", "")
	job := jobCR(shopNS, collidingJob, "0 3 * * *")
	job.UID = "job-uid"
	cj := collidingCronJob(shopNS)
	cj.Labels = map[string]string{"app": "something-else"}
	yes := true
	cj.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "kipper.run/v1alpha1",
		Kind:       "Job",
		Name:       job.Name,
		UID:        job.UID,
		Controller: &yes,
	}}
	client := fake.NewClientset(cj)
	h := &Jobs{Client: client, CRClient: testCRClient(job)}

	rec := httptest.NewRecorder()
	h.Trigger(rec, jobRequest("POST", "/api/v1/jobs/"+collidingJob+"/trigger", "dev@test.com", collidingJob, ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
}
