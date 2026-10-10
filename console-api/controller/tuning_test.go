package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/labels"
)

// workloadName names the workload every test here tunes.
const workloadName = "web"

func tuningCRClient(objs ...crclient.Object) crclient.Client {
	return crfake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&kipperv1.ResourceTuning{}).
		WithReturnManagedFields().
		Build()
}

// memoryContainer is a container with 200m of CPU and the given memory.
func memoryContainer(memReq, memLim string) corev1.Container {
	return corev1.Container{
		Name: "web",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse(memReq)},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse(memLim)},
		},
	}
}

func ownedDeployment(kind string, container corev1.Container) *appsv1.Deployment {
	name := workloadName
	replicas := int32(2)
	controller := true
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", Labels: map[string]string{"app": name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(), Kind: kind, Name: name,
				UID: types.UID("uid-" + name), Controller: &controller,
			}},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{container}}},
		},
	}
}

func usage(cpuMillis int64, memory string) map[string][]podMetricsEntry {
	name := workloadName
	m := resource.MustParse(memory)
	return map[string][]podMetricsEntry{"default/" + name: {{
		Namespace: "default", PodName: name + "-abc", CPUMillis: cpuMillis, MemoryBytes: m.Value(), Age: 10 * time.Minute,
	}}}
}

func oomUsage(at time.Time) map[string][]podMetricsEntry {
	name := workloadName
	return map[string][]podMetricsEntry{"default/" + name: {{
		Namespace: "default", PodName: name + "-abc", CPUMillis: 10, MemoryBytes: 1, Age: 10 * time.Minute,
		OOMKilled: true, OOMAt: at,
	}}}
}

func tuningOf(t *testing.T, c crclient.Client, kind string) *kipperv1.ResourceTuning {
	t.Helper()
	name := workloadName
	var rt kipperv1.ResourceTuning
	if err := c.Get(context.Background(), crclient.ObjectKey{Namespace: "default", Name: strings.ToLower(kind) + "-" + name}, &rt); err != nil {
		t.Fatalf("reading the tuning record: %v", err)
	}
	return &rt
}

func assertNoWorkloadWrites(t *testing.T, client *fake.Clientset) {
	t.Helper()
	for _, a := range client.Actions() {
		if a.GetVerb() == "update" || a.GetVerb() == "patch" {
			if r := a.GetResource().Resource; r == "deployments" || r == "statefulsets" {
				t.Fatalf("the auto-sizer wrote a workload: %s %s", a.GetVerb(), r)
			}
		}
	}
}

func hasAction(entries []ResourceLogEntry, action string) bool {
	for _, e := range entries {
		if strings.Contains(e.Action, action) {
			return true
		}
	}
	return false
}

// An idle workload at its memory floor should produce no memory-change alert.
func TestBoundedWorkloadIdlingAtItsFloorStaysPut(t *testing.T) {
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", UID: "uid-web"},
		Spec:       kipperv1.ServiceSpec{Resources: kipperv1.ServiceResources{MemoryRequest: "512Mi", MemoryLimit: "2Gi"}},
	}
	crClient := tuningCRClient(svc)
	deploy := ownedDeployment("Service", memoryContainer("512Mi", "2Gi"))
	client := fake.NewClientset(deploy)
	rc := NewResourceController(client, crClient)

	for tick := 0; tick < 4; tick++ {
		entries := rc.processDeployment(context.Background(), deploy.DeepCopy(), usage(150, "20Mi"), nil)
		if hasAction(entries, "memory") {
			t.Fatalf("tick %d: memory entry for a bounded app idling at its floor: %v", tick, entries)
		}
	}
	assertNoWorkloadWrites(t, client)
	rec := tuningOf(t, crClient, "Service").Status.Recommendation
	if rec.MemoryRequest != "512Mi" || rec.MemoryLimit != "2Gi" {
		t.Fatalf("memory recommendation = %s/%s, want 512Mi/2Gi", rec.MemoryRequest, rec.MemoryLimit)
	}
}

func TestBusyBoundedWorkloadRaisesTheRequestUnderItsCeiling(t *testing.T) {
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", UID: "uid-web"},
		Spec:       kipperv1.ServiceSpec{Resources: kipperv1.ServiceResources{MemoryRequest: "512Mi", MemoryLimit: "2Gi"}},
	}
	crClient := tuningCRClient(svc)
	deploy := ownedDeployment("Service", memoryContainer("512Mi", "2Gi"))
	rc := NewResourceController(fake.NewClientset(deploy), crClient)

	entries := rc.processDeployment(context.Background(), deploy.DeepCopy(), usage(150, "480Mi"), nil)
	if !hasAction(entries, "increased memory") {
		t.Fatalf("expected a memory increase, got %v", entries)
	}
	rec := tuningOf(t, crClient, "Service").Status.Recommendation
	if rec.MemoryRequest != "768Mi" || rec.MemoryLimit != "2Gi" {
		t.Fatalf("memory recommendation = %s/%s, want 768Mi/2Gi", rec.MemoryRequest, rec.MemoryLimit)
	}
}

func TestOOMAtTheUsersCeilingOnlyAlerts(t *testing.T) {
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", UID: "uid-web"},
		Spec:       kipperv1.ServiceSpec{Resources: kipperv1.ServiceResources{MemoryRequest: "512Mi", MemoryLimit: "2Gi"}},
	}
	crClient := tuningCRClient(svc)
	deploy := ownedDeployment("Service", memoryContainer("512Mi", "2Gi"))
	client := fake.NewClientset(deploy)
	rc := NewResourceController(client, crClient)

	oomAt := time.Now().Add(-time.Minute)
	entries := rc.processDeployment(context.Background(), deploy.DeepCopy(), oomUsage(oomAt), nil)
	if len(entries) != 1 || entries[0].Action != "OOMKilled at your limit" || alertSeverity(entries[0].Action) != "critical" {
		t.Fatalf("entries = %v, want one critical OOMKilled-at-your-limit alert", entries)
	}
	if !strings.Contains(entries[0].Reason, "2Gi") {
		t.Errorf("reason %q should name the limit", entries[0].Reason)
	}
	rt := tuningOf(t, crClient, "Service")
	if rt.Status.Recommendation.MemoryLimit != "2Gi" {
		t.Fatalf("memory limit recommendation = %q, want the user's 2Gi", rt.Status.Recommendation.MemoryLimit)
	}

	// The alert was stored: the same OOM raises nothing on the next tick, nor
	// after a restart.
	rc.commitStagedAcks(context.Background(), true)
	if again := rc.processDeployment(context.Background(), deploy.DeepCopy(), oomUsage(oomAt), nil); hasAction(again, "OOMKilled") {
		t.Fatalf("the same OOM alerted twice: %v", again)
	}
	restarted := NewResourceController(client, crClient)
	if again := restarted.processDeployment(context.Background(), deploy.DeepCopy(), oomUsage(oomAt), nil); hasAction(again, "OOMKilled") {
		t.Fatalf("the same OOM alerted again after a restart: %v", again)
	}
	assertNoWorkloadWrites(t, client)
}

// Alerting is the only response to an OOM at the user's limit, so an alert
// that could not be stored is raised again rather than lost.
func TestOOMAtTheUsersCeilingAlertsAgainWhenTheAlertWasNotStored(t *testing.T) {
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", UID: "uid-web"},
		Spec:       kipperv1.ServiceSpec{Resources: kipperv1.ServiceResources{MemoryRequest: "512Mi", MemoryLimit: "2Gi"}},
	}
	crClient := tuningCRClient(svc)
	deploy := ownedDeployment("Service", memoryContainer("512Mi", "2Gi"))
	rc := NewResourceController(fake.NewClientset(deploy), crClient)

	oomAt := time.Now().Add(-time.Minute)
	rc.processDeployment(context.Background(), deploy.DeepCopy(), oomUsage(oomAt), nil)
	rc.commitStagedAcks(context.Background(), false)
	if again := rc.processDeployment(context.Background(), deploy.DeepCopy(), oomUsage(oomAt), nil); !hasAction(again, "OOMKilled at your limit") {
		t.Fatalf("an alert that was never stored was not raised again: %v", again)
	}
}

// An OOM increase stays recommended until the reconciler applies it, even on
// ticks that propose nothing new.
func TestAnUnappliedRecommendationSurvivesTheNextTick(t *testing.T) {
	app := automaticApp()
	crClient := tuningCRClient(app)
	deploy := ownedDeployment("App", memoryContainer("128Mi", "128Mi"))
	rc := NewResourceController(fake.NewClientset(deploy), crClient)

	oomAt := time.Now().Add(-time.Minute)
	rc.processDeployment(context.Background(), deploy.DeepCopy(), oomUsage(oomAt), nil)
	// The reconciler has not applied 256Mi yet; the kubelet still reports the OOM.
	rc.processDeployment(context.Background(), deploy.DeepCopy(), oomUsage(oomAt), nil)

	if got := tuningOf(t, crClient, "App").Status.Recommendation.MemoryRequest; got != "256Mi" {
		t.Fatalf("memory recommendation = %s, want the pending 256Mi kept", got)
	}
}

// Functions run on the lightweight profile, so a quiet one may go below the
// standard profile's floor.
func TestFunctionsUseTheLightweightFloor(t *testing.T) {
	fn := &kipperv1.Function{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", UID: "uid-web"}}
	crClient := tuningCRClient(fn)
	deploy := ownedDeployment("Function", memoryContainer("128Mi", "128Mi"))
	rc := NewResourceController(fake.NewClientset(deploy), crClient)

	for tick := 0; tick < 3; tick++ {
		rc.processDeployment(context.Background(), deploy.DeepCopy(), usage(10, "4Mi"), nil)
	}
	if got := tuningOf(t, crClient, "Function").Status.Recommendation.MemoryRequest; got != "64Mi" {
		t.Fatalf("memory recommendation = %s, want 64Mi, the lightweight floor", got)
	}
}

func automaticApp() *kipperv1.App {
	name := workloadName
	return &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID("uid-" + name)},
		Spec:       kipperv1.AppSpec{Image: "registry.example.com/web:1"},
	}
}

func TestAutomaticWorkloadIsTunedThroughTheRecordOnly(t *testing.T) {
	app := automaticApp()
	crClient := tuningCRClient(app)
	deploy := ownedDeployment("App", memoryContainer("512Mi", "512Mi"))
	client := fake.NewClientset(deploy)
	rc := NewResourceController(client, crClient)

	var entries []ResourceLogEntry
	for tick := 0; tick < 3; tick++ {
		entries = rc.processDeployment(context.Background(), deploy.DeepCopy(), usage(20, "20Mi"), nil)
	}
	if !hasAction(entries, "decreased memory") {
		t.Fatalf("expected a memory decrease after three quiet checks, got %v", entries)
	}
	rec := tuningOf(t, crClient, "App").Status.Recommendation
	if rec.MemoryRequest != "256Mi" || rec.MemoryLimit != "256Mi" {
		t.Fatalf("memory recommendation = %s/%s, want 256Mi/256Mi", rec.MemoryRequest, rec.MemoryLimit)
	}
	assertNoWorkloadWrites(t, client)

	var after kipperv1.App
	if err := crClient.Get(context.Background(), crclient.ObjectKeyFromObject(app), &after); err != nil {
		t.Fatal(err)
	}
	if after.Spec.Resources != app.Spec.Resources {
		t.Fatalf("the auto-sizer wrote the App spec: %+v", after.Spec.Resources)
	}
}

// Exclude tuning records so restores start with fresh recommendations and
// OOM cooldowns.
func TestTheTuningRecordIsLeftOutOfBackups(t *testing.T) {
	crClient := tuningCRClient(automaticApp())
	deploy := ownedDeployment("App", memoryContainer("512Mi", "512Mi"))
	rc := NewResourceController(fake.NewClientset(deploy), crClient)

	rc.processDeployment(context.Background(), deploy.DeepCopy(), usage(20, "20Mi"), nil)

	if got := tuningOf(t, crClient, "App").Labels["velero.io/exclude-from-backup"]; got != "true" {
		t.Fatalf("velero.io/exclude-from-backup = %q, want \"true\"", got)
	}
}

// A record left by an earlier App of the same name is not the new App's: the
// auto-sizer leaves it alone until garbage collection removes it.
func TestARecordLeftByAnEarlierOwnerIsNotReused(t *testing.T) {
	controller := true
	stale := &kipperv1.ResourceTuning{
		ObjectMeta: metav1.ObjectMeta{
			Name: "app-" + workloadName, Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(), Kind: "App", Name: workloadName,
				UID: types.UID("uid-earlier"), Controller: &controller,
			}},
		},
		Spec:   kipperv1.ResourceTuningSpec{Kind: "App", Name: workloadName},
		Status: kipperv1.ResourceTuningStatus{Recommendation: kipperv1.TunedResources{MemoryRequest: "2Gi", MemoryLimit: "2Gi"}},
	}
	crClient := tuningCRClient(automaticApp(), stale)
	deploy := ownedDeployment("App", memoryContainer("512Mi", "512Mi"))
	rc := NewResourceController(fake.NewClientset(deploy), crClient)

	for tick := 0; tick < 3; tick++ {
		rc.processDeployment(context.Background(), deploy.DeepCopy(), usage(20, "20Mi"), nil)
	}

	if rec := tuningOf(t, crClient, "App").Status.Recommendation; rec.MemoryRequest != "2Gi" {
		t.Fatalf("the earlier owner's record was rewritten: %+v", rec)
	}
}

// An App deleted with --cascade=orphan leaves its record with no owner, which
// garbage collection never removes. The next App of that name gets a fresh
// record instead of the old state or none at all.
func TestAnOwnerlessRecordIsRebuiltForTheNewOwner(t *testing.T) {
	orphan := &kipperv1.ResourceTuning{
		ObjectMeta: metav1.ObjectMeta{Name: "app-" + workloadName, Namespace: "default", UID: types.UID("uid-orphan")},
		Spec:       kipperv1.ResourceTuningSpec{Kind: "App", Name: workloadName},
		Status:     kipperv1.ResourceTuningStatus{Recommendation: kipperv1.TunedResources{MemoryRequest: "2Gi", MemoryLimit: "2Gi"}},
	}
	crClient := tuningCRClient(automaticApp(), orphan)
	deploy := ownedDeployment("App", memoryContainer("512Mi", "512Mi"))
	rc := NewResourceController(fake.NewClientset(deploy), crClient)

	rc.processDeployment(context.Background(), deploy.DeepCopy(), usage(20, "20Mi"), nil)

	rt := tuningOf(t, crClient, "App")
	if ref := metav1.GetControllerOf(rt); ref == nil || ref.UID != types.UID("uid-"+workloadName) {
		t.Fatalf("record controller = %+v, want the current App", ref)
	}
	if rt.Status.Recommendation.MemoryRequest == "2Gi" {
		t.Fatalf("the rebuilt record kept the orphan's recommendation: %+v", rt.Status.Recommendation)
	}
	if rt.Labels["velero.io/exclude-from-backup"] != "true" {
		t.Fatalf("the rebuilt record lost its backup exclusion: %v", rt.Labels)
	}
}

// After an OOM doubling, quiet checks do not halve memory again, even when
// console-api restarts in between.
func TestOOMDoublingBlocksDecreasesAcrossARestart(t *testing.T) {
	app := automaticApp()
	crClient := tuningCRClient(app)
	deploy := ownedDeployment("App", memoryContainer("128Mi", "128Mi"))
	rc := NewResourceController(fake.NewClientset(deploy), crClient)

	oomAt := time.Now().Add(-time.Minute)
	entries := rc.processDeployment(context.Background(), deploy.DeepCopy(), oomUsage(oomAt), nil)
	if !hasAction(entries, "doubled memory") {
		t.Fatalf("expected the OOM to double memory, got %v", entries)
	}
	rt := tuningOf(t, crClient, "App")
	if rt.Status.Recommendation.MemoryRequest != "256Mi" {
		t.Fatalf("memory recommendation = %s, want 256Mi", rt.Status.Recommendation.MemoryRequest)
	}
	if rt.Status.DecreaseBlockedUntil == nil || time.Until(rt.Status.DecreaseBlockedUntil.Time) < 23*time.Hour {
		t.Fatalf("decreaseBlockedUntil = %v, want about 24h ahead", rt.Status.DecreaseBlockedUntil)
	}

	// The reconciler applied 256Mi and console-api restarts. The kubelet still
	// reports the same OOM, which must not double memory a second time.
	resized := ownedDeployment("App", memoryContainer("256Mi", "256Mi"))
	restarted := NewResourceController(fake.NewClientset(resized), crClient)
	if again := restarted.processDeployment(context.Background(), resized.DeepCopy(), oomUsage(oomAt), nil); hasAction(again, "doubled memory") {
		t.Fatalf("the same OOM doubled memory again after a restart: %v", again)
	}
	// The app is quiet from here on.
	for tick := 0; tick < 4; tick++ {
		entries = restarted.processDeployment(context.Background(), resized.DeepCopy(), usage(150, "20Mi"), nil)
		if hasAction(entries, "decreased memory") || hasAction(entries, "doubled memory") {
			t.Fatalf("tick %d after restart: %v", tick, entries)
		}
	}
}

// Discard old samples so a resize starts a fresh observation window.
func TestHistoryStartsOverWhenTheWorkloadIsResized(t *testing.T) {
	app := automaticApp()
	crClient := tuningCRClient(app)
	small := ownedDeployment("App", memoryContainer("512Mi", "512Mi"))
	rc := NewResourceController(fake.NewClientset(small), crClient)

	for tick := 0; tick < 2; tick++ {
		rc.processDeployment(context.Background(), small.DeepCopy(), usage(20, "20Mi"), nil)
	}
	large := ownedDeployment("App", memoryContainer("1Gi", "1Gi"))
	entries := rc.processDeployment(context.Background(), large.DeepCopy(), usage(20, "20Mi"), nil)
	if hasAction(entries, "decreased memory") {
		t.Fatalf("decreased on the first sample after a resize: %v", entries)
	}
}

func TestWorkloadWithoutAnOwnerIsLeftAlone(t *testing.T) {
	crClient := tuningCRClient()
	deploy := ownedDeployment("App", memoryContainer("512Mi", "512Mi"))
	deploy.OwnerReferences = nil
	client := fake.NewClientset(deploy)
	rc := NewResourceController(client, crClient)

	for tick := 0; tick < 3; tick++ {
		if entries := rc.processDeployment(context.Background(), deploy.DeepCopy(), usage(20, "20Mi"), nil); len(entries) != 0 {
			t.Fatalf("tick %d: entries for an ownerless workload: %v", tick, entries)
		}
	}
	var list kipperv1.ResourceTuningList
	if err := crClient.List(context.Background(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("created %d tuning records for an ownerless workload", len(list.Items))
	}
	assertNoWorkloadWrites(t, client)
}

// An OOM increase the project quota blocks stays unhandled, so it goes ahead
// once the quota allows it.
func TestAQuotaBlockedOOMIncreaseIsRetried(t *testing.T) {
	app := automaticApp()
	crClient := tuningCRClient(app)
	deploy := ownedDeployment("App", memoryContainer("128Mi", "128Mi"))
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: kipperv1.ProjectQuotaName, Namespace: "default"},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourceLimitsMemory: resource.MustParse("300Mi"), corev1.ResourceRequestsMemory: resource.MustParse("300Mi"),
		}},
		// Two replicas at 128Mi.
		Status: corev1.ResourceQuotaStatus{Used: corev1.ResourceList{
			corev1.ResourceLimitsMemory: resource.MustParse("256Mi"), corev1.ResourceRequestsMemory: resource.MustParse("256Mi"),
		}},
	}
	client := fake.NewClientset(deploy, quota)
	rc := NewResourceController(client, crClient)

	oomAt := time.Now().Add(-time.Minute)
	if entries := rc.processDeployment(context.Background(), deploy.DeepCopy(), oomUsage(oomAt), nil); hasAction(entries, "doubled memory") {
		t.Fatalf("the increase should be blocked by the quota: %v", entries)
	}

	quota.Spec.Hard = corev1.ResourceList{
		corev1.ResourceLimitsMemory: resource.MustParse("4Gi"), corev1.ResourceRequestsMemory: resource.MustParse("4Gi"),
	}
	if _, err := client.CoreV1().ResourceQuotas("default").Update(context.Background(), quota, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if entries := rc.processDeployment(context.Background(), deploy.DeepCopy(), oomUsage(oomAt), nil); !hasAction(entries, "doubled memory") {
		t.Fatalf("the blocked OOM increase was not retried once the quota allowed it: %v", entries)
	}
}

// A tick whose increase the quota blocks leaves the pending recommendation as
// it was, so an OOM increase not applied yet is not replaced by the live size.
func TestAQuotaBlockedTickKeepsThePendingRecommendation(t *testing.T) {
	app := automaticApp()
	crClient := tuningCRClient(app)
	deploy := ownedDeployment("App", memoryContainer("128Mi", "128Mi"))
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: kipperv1.ProjectQuotaName, Namespace: "default"},
		// Two replicas at 200m use 400m; 600m leaves room for one surge pod at
		// the current CPU, so the memory rollout fits and a CPU increase does not.
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourceLimitsCPU: resource.MustParse("600m"), corev1.ResourceRequestsCPU: resource.MustParse("600m"),
		}},
		Status: corev1.ResourceQuotaStatus{Used: corev1.ResourceList{
			corev1.ResourceLimitsCPU: resource.MustParse("400m"), corev1.ResourceRequestsCPU: resource.MustParse("400m"),
		}},
	}
	rc := NewResourceController(fake.NewClientset(deploy, quota), crClient)

	oomAt := time.Now().Add(-time.Minute)
	rc.processDeployment(context.Background(), deploy.DeepCopy(), oomUsage(oomAt), nil)
	if got := tuningOf(t, crClient, "App").Status.Recommendation.MemoryRequest; got != "256Mi" {
		t.Fatalf("after the OOM: memory recommendation = %s, want 256Mi", got)
	}

	// Not applied yet. The pod now pins its CPU limit, so the tick proposes a
	// CPU increase, which the full CPU quota blocks.
	saturated := map[string][]podMetricsEntry{"default/web": {{
		Namespace: "default", PodName: "web-abc", CPUMillis: 199, MemoryBytes: 1, Age: 10 * time.Minute,
	}}}
	rc.processDeployment(context.Background(), deploy.DeepCopy(), saturated, nil)

	if got := tuningOf(t, crClient, "App").Status.Recommendation.MemoryRequest; got != "256Mi" {
		t.Fatalf("after the blocked tick: memory recommendation = %s, want the pending 256Mi kept", got)
	}
}

// An OOM alert at the user's limit is reported even when the tuning record
// cannot be saved, since its acknowledgement waits for that alert.
func TestAnOOMAlertSurvivesAFailedRecordWrite(t *testing.T) {
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", UID: "uid-web"},
		Spec:       kipperv1.ServiceSpec{Resources: kipperv1.ServiceResources{MemoryRequest: "512Mi", MemoryLimit: "2Gi"}},
	}
	crClient := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc).
		WithStatusSubresource(&kipperv1.ResourceTuning{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(context.Context, crclient.WithWatch, crclient.Object, ...crclient.CreateOption) error {
				return errors.New("the API server did not answer")
			},
		}).Build()
	deploy := ownedDeployment("Service", memoryContainer("512Mi", "2Gi"))
	rc := NewResourceController(fake.NewClientset(deploy), crClient)

	entries := rc.processDeployment(context.Background(), deploy.DeepCopy(), oomUsage(time.Now().Add(-time.Minute)), nil)
	if !hasAction(entries, "OOMKilled at your limit") {
		t.Fatalf("the OOM alert was dropped with the failed record write: %v", entries)
	}
}

// While the post-OOM cooldown runs, a usage-based increase worked out from the
// old live size must not shrink the OOM increase still waiting to be applied.
func TestAnUsageIncreaseDoesNotShrinkAPendingOOMIncrease(t *testing.T) {
	app := automaticApp()
	crClient := tuningCRClient(app)
	deploy := ownedDeployment("App", memoryContainer("512Mi", "512Mi"))
	rc := NewResourceController(fake.NewClientset(deploy), crClient)

	rc.processDeployment(context.Background(), deploy.DeepCopy(), oomUsage(time.Now().Add(-time.Minute)), nil)
	if got := tuningOf(t, crClient, "App").Status.Recommendation.MemoryRequest; got != "1Gi" {
		t.Fatalf("after the OOM: memory recommendation = %s, want 1Gi", got)
	}

	// Not applied yet; a surviving replica now uses 450Mi of the live 512Mi.
	rc.processDeployment(context.Background(), deploy.DeepCopy(), usage(20, "450Mi"), nil)

	rec := tuningOf(t, crClient, "App").Status.Recommendation
	if rec.MemoryRequest != "1Gi" || rec.MemoryLimit != "1Gi" {
		t.Fatalf("memory recommendation = %s/%s, want the pending 1Gi kept", rec.MemoryRequest, rec.MemoryLimit)
	}
}

func serviceWithResources(res kipperv1.ServiceResources) *kipperv1.Service {
	return &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", UID: "uid-web"},
		Spec:       kipperv1.ServiceSpec{Resources: res},
	}
}

func cpuMemoryContainer(cpuReq, cpuLim, mem string) corev1.Container {
	return corev1.Container{
		Name: "web",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpuReq), corev1.ResourceMemory: resource.MustParse(mem)},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpuLim), corev1.ResourceMemory: resource.MustParse(mem)},
		},
	}
}

func busy(cpuMillis int64, memory string) map[string][]podMetricsEntry {
	m := resource.MustParse(memory)
	return map[string][]podMetricsEntry{"default/web": {{
		Namespace: "default", PodName: "web-abc", CPUMillis: cpuMillis, MemoryBytes: m.Value(), Age: 10 * time.Minute,
	}}}
}

// A user's CPU ceiling cannot be raised, so a pod pinning it must not stop the
// auto-sizer from tuning memory.
func TestAFixedCPUAtItsLimitDoesNotStopMemoryTuning(t *testing.T) {
	crClient := tuningCRClient(serviceWithResources(kipperv1.ServiceResources{CPURequest: "200m", CPULimit: "200m"}))
	deploy := ownedDeployment("Service", cpuMemoryContainer("200m", "200m", "512Mi"))
	rc := NewResourceController(fake.NewClientset(deploy), crClient)

	entries := rc.processDeployment(context.Background(), deploy.DeepCopy(), busy(199, "450Mi"), nil)
	if !hasAction(entries, "increased memory") {
		t.Fatalf("memory at 88%% was not raised while CPU sat at the user's limit: %v", entries)
	}
}

// Bounded CPU pinned at its ceiling still gets its request raised, inside the
// bounds.
func TestSaturatedBoundedCPURaisesItsRequest(t *testing.T) {
	crClient := tuningCRClient(serviceWithResources(kipperv1.ServiceResources{CPURequest: "100m", CPULimit: "200m"}))
	deploy := ownedDeployment("Service", cpuMemoryContainer("100m", "200m", "512Mi"))
	rc := NewResourceController(fake.NewClientset(deploy), crClient)

	rc.processDeployment(context.Background(), deploy.DeepCopy(), busy(199, "100Mi"), nil)
	rec := tuningOf(t, crClient, "Service").Status.Recommendation
	if rec.CPURequest != "150m" || rec.CPULimit != "200m" {
		t.Fatalf("cpu recommendation = %s/%s, want 150m/200m", rec.CPURequest, rec.CPULimit)
	}
}

func jvmDeployment(cpuReq, cpuLim string) *appsv1.Deployment {
	deploy := ownedDeployment("App", corev1.Container{
		Name: "web",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpuReq), corev1.ResourceMemory: resource.MustParse("2Gi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpuLim), corev1.ResourceMemory: resource.MustParse("2Gi")},
		},
	})
	deploy.Labels[labels.ResourceProfile] = "jvm"
	return deploy
}

func TestABusyJVMAppGrowsItsCPURequestStepByStep(t *testing.T) {
	app := automaticApp()
	crClient := tuningCRClient(app)
	deploy := jvmDeployment("100m", "1000m")
	rc := NewResourceController(fake.NewClientset(deploy), crClient)

	entries := rc.processDeployment(context.Background(), deploy.DeepCopy(), usage(877, "1Gi"), nil)
	if !hasAction(entries, "increased CPU") {
		t.Fatalf("expected a CPU increase, got %v", entries)
	}
	rec := tuningOf(t, crClient, "App").Status.Recommendation
	if rec.CPURequest != "150m" {
		t.Fatalf("CPU request recommendation = %s, want 150m: the jvm minimum applies to the limit, not the request", rec.CPURequest)
	}
	if lim := resource.MustParse(rec.CPULimit); lim.Cmp(resource.MustParse("500m")) < 0 {
		t.Fatalf("CPU limit recommendation = %s, want at least the jvm minimum of 500m", rec.CPULimit)
	}
}

func TestAnIdleJVMAppLowersItsCPURequestButKeepsTheLimitMinimum(t *testing.T) {
	app := automaticApp()
	crClient := tuningCRClient(app)
	deploy := jvmDeployment("500m", "500m")
	rc := NewResourceController(fake.NewClientset(deploy), crClient)

	var entries []ResourceLogEntry
	for tick := 0; tick < 3; tick++ {
		entries = rc.processDeployment(context.Background(), deploy.DeepCopy(), usage(20, "1Gi"), nil)
	}
	if !hasAction(entries, "decreased CPU") {
		t.Fatalf("expected a CPU decrease after three quiet checks, got %v", entries)
	}
	rec := tuningOf(t, crClient, "App").Status.Recommendation
	if rec.CPURequest != "250m" || rec.CPULimit != "500m" {
		t.Fatalf("CPU recommendation = %s/%s, want 250m/500m", rec.CPURequest, rec.CPULimit)
	}
}
