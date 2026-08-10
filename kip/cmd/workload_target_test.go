package cmd

import (
	"context"
	stderrors "errors"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

// The two commands under test address a pod directly, so every case here is
// expressed through the request both of them build. That is the seam: cobra
// flag parsing and the SPDY dial sit either side of it, and everything that
// chooses a workload sits inside.

func execRequest(name string) workloadTargetRequest {
	return workloadTargetRequest{name: name, preference: acceptUnready}
}

func tunnelRequest(name string) workloadTargetRequest {
	return workloadTargetRequest{name: name, preference: preferReady}
}

func bareCluster() *config.Cluster { return &config.Cluster{} }

// runningPod builds a pod the resolver considers eligible and Ready.
func runningPod(name, ns, workload string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{"app": workload},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  workload,
			Ports: []corev1.ContainerPort{{ContainerPort: 3000}},
		}}},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
}

func unreadyPod(name, ns, workload string) *corev1.Pod {
	pod := runningPod(name, ns, workload)
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	return pod
}

func terminatingPod(name, ns, workload string) *corev1.Pod {
	pod := runningPod(name, ns, workload)
	now := metav1.Now()
	pod.DeletionTimestamp = &now
	pod.Finalizers = []string{"kipper.run/test"} // the fake client drops an object deleted with no finalizer
	return pod
}

func pendingPod(name, ns, workload string) *corev1.Pod {
	pod := runningPod(name, ns, workload)
	pod.Status.Phase = corev1.PodPending
	pod.Status.Conditions = nil
	return pod
}

// Defect 1: the cluster-wide fallback took the first match with no ambiguity
// check. One project's environments are separate namespaces, so the same app
// name in blog-test and blog-prod is the ordinary case, not an exotic one.
func TestResolveWorkloadTarget_RefusesANameInSeveralNamespaces(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "blog-prod", "api", nil)
	seedWorkload(t, dyn, manifest.AppGVR, "App", "blog-test", "api", nil)
	cs := k8sfake.NewSimpleClientset(
		runningPod("api-1", "blog-prod", "api"),
		runningPod("api-2", "blog-test", "api"),
	)

	_, err := execRequest("api").resolve(context.Background(), cs, dyn, bareCluster())
	require.Error(t, err, "two namespaces hold an api; picking by list order can open a shell in prod")
	assert.Contains(t, err.Error(), "app/blog-prod")
	assert.Contains(t, err.Error(), "app/blog-test")
	assert.Contains(t, err.Error(), "--project", "the error has to say how to disambiguate")

	var ambiguous *ambiguousTargetError
	assert.True(t, stderrors.As(err, &ambiguous))
}

// Defect 2: the cluster-wide block was guarded by `if podName == ""` rather
// than by whether a project was given, so naming a project that holds no such
// workload silently searched every other project instead.
func TestResolveWorkloadTarget_AnExplicitProjectIsNotWidened(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "tools-prod", "api", nil)
	cs := k8sfake.NewSimpleClientset(runningPod("api-1", "tools-prod", "api"))

	req := execRequest("api")
	req.project = "shop"
	req.environment = "prod"

	_, err := req.resolve(context.Background(), cs, dyn, bareCluster())
	require.Error(t, err, "shop-prod holds no api; another project's api is not the answer")
	assert.Contains(t, err.Error(), "shop-prod")
	assert.NotContains(t, err.Error(), "tools-prod", "the workload the operator did not name must not be offered")

	var notFound *workloadTargetNotFoundError
	assert.True(t, stderrors.As(err, &notFound))
}

// Defect 3: neither command consulted the saved context, so `kip project use`
// had no effect on them.
func TestResolveWorkloadTarget_UsesTheSavedProject(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "shop-prod", "api", nil)
	seedWorkload(t, dyn, manifest.AppGVR, "App", "tools-prod", "api", nil)
	cs := k8sfake.NewSimpleClientset(
		runningPod("api-1", "shop-prod", "api"),
		runningPod("api-2", "tools-prod", "api"),
	)

	// resolveProjectAndEnvironment folds the saved context into the request
	// before it gets here, which is what the commands now do.
	req := execRequest("api")
	req.project = "shop"
	req.environment = "prod"

	target, err := req.resolve(context.Background(), cs, dyn, bareCluster())
	require.NoError(t, err, "the saved project makes this unambiguous")
	assert.Equal(t, "shop-prod", target.candidate.namespace)
}

// Defect 4: Items[0] could be a pod that is going away. Terminating is not a
// phase — such a pod stays Running with a deletionTimestamp — so a phase filter
// alone does not exclude it.
func TestResolveWorkloadTarget_SkipsATerminatingPod(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "shop-prod", "api", nil)
	// The terminating pod sorts first, so a resolver that filters on phase alone
	// hands it back. Naming it second would let the tie-break hide the defect.
	cs := k8sfake.NewSimpleClientset(
		terminatingPod("api-0-going", "shop-prod", "api"),
		runningPod("api-1-staying", "shop-prod", "api"),
	)

	target, err := execRequest("api").resolve(context.Background(), cs, dyn, bareCluster())
	require.NoError(t, err)
	assert.Equal(t, "api-1-staying", target.pod.Name, "a pod being deleted is not somewhere to open a shell")
}

// Defect 5, CR side: an RBAC denial or an apiserver failure was reported as
// "no running pod", and on the explicit-project path it also fed the widening.
func TestResolveWorkloadTarget_ACRListFailureIsTheAnswer(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	dyn.PrependReactor("list", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "kipper.run", Resource: "services"}, "", stderrors.New("nope"))
	})
	seedWorkload(t, dyn, manifest.AppGVR, "App", "shop-prod", "api", nil)
	cs := k8sfake.NewSimpleClientset(runningPod("api-1", "shop-prod", "api"))

	_, err := execRequest("api").resolve(context.Background(), cs, dyn, bareCluster())
	require.Error(t, err, "a denied lookup answers neither yes nor no; it must not read as a clean single match")
	assert.Contains(t, err.Error(), "service", "the error has to name the lookup that failed")

	var ambiguous *ambiguousTargetError
	assert.False(t, stderrors.As(err, &ambiguous), "a failed lookup is not an ambiguous one")
}

// Defect 5, Deployment side: the CR-less App fallback must fail closed too.
func TestResolveWorkloadTarget_ADeploymentListFailureIsTheAnswer(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	cs := k8sfake.NewSimpleClientset()
	cs.PrependReactor("list", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "deployments"}, "", stderrors.New("nope"))
	})

	_, err := execRequest("api").resolve(context.Background(), cs, dyn, bareCluster())
	require.Error(t, err)

	var notFound *workloadTargetNotFoundError
	assert.False(t, stderrors.As(err, &notFound), "a denied lookup is not proof the workload is absent")
}

// Defect 5, Pod side: a Pod list failure must not become "not found" either.
func TestResolveWorkloadTarget_APodListFailureIsTheAnswer(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "shop-prod", "api", nil)
	cs := k8sfake.NewSimpleClientset()
	cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", stderrors.New("nope"))
	})

	_, err := execRequest("api").resolve(context.Background(), cs, dyn, bareCluster())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shop-prod")

	var notFound *workloadTargetNotFoundError
	assert.False(t, stderrors.As(err, &notFound))
}

// The v1 gap: narrowing to a namespace fixes cross-project targeting and does
// nothing about an App and a Service both called api inside it. Discovery has
// to run in an authoritative namespace too.
func TestResolveWorkloadTarget_RefusesTwoKindsInOneNamespaceWithAnExplicitProject(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "shop-prod", "api", nil)
	seedWorkload(t, dyn, manifest.ServiceGVR, "Service", "shop-prod", "api", nil)
	cs := k8sfake.NewSimpleClientset(runningPod("api-1", "shop-prod", "api"))

	req := execRequest("api")
	req.project = "shop"
	req.environment = "prod"

	_, err := req.resolve(context.Background(), cs, dyn, bareCluster())
	require.Error(t, err, "naming the project does not say which of the two kinds in it was meant")
	assert.Contains(t, err.Error(), "app/shop-prod")
	assert.Contains(t, err.Error(), "service/shop-prod")
	assert.Contains(t, err.Error(), "--kind", "a collision inside one namespace is only resolvable by kind")
	assert.NotContains(t, err.Error(), "--project", "the project is already named; repeating it is no remedy")
}

// The same collision reached through the saved context rather than a flag.
func TestResolveWorkloadTarget_RefusesTwoKindsInOneNamespaceWithASavedProject(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "shop-prod", "api", nil)
	seedWorkload(t, dyn, manifest.FunctionGVR, "Function", "shop-prod", "api", nil)
	cs := k8sfake.NewSimpleClientset(runningPod("api-1", "shop-prod", "api"))

	cluster := &config.Cluster{CurrentProject: "shop", CurrentEnvironment: "prod"}
	project, environment := "shop", "prod" // what resolveProjectAndEnvironment returns for this cluster
	req := execRequest("api")
	req.project, req.environment = project, environment

	_, err := req.resolve(context.Background(), cs, dyn, cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--kind")
}

// --kind is the way out of that collision.
func TestResolveWorkloadTarget_KindPicksOneOfTwoInOneNamespace(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "shop-prod", "api", nil)
	seedWorkload(t, dyn, manifest.ServiceGVR, "Service", "shop-prod", "api", nil)
	cs := k8sfake.NewSimpleClientset(servicePod("api-0", "api"))

	req := execRequest("api")
	req.project, req.environment = "shop", "prod"
	req.kind = targetKindService

	target, err := req.resolve(context.Background(), cs, dyn, bareCluster())
	require.NoError(t, err)
	assert.Equal(t, targetKindService, target.candidate.kind)
}

// --kind narrows, it does not invent. Naming a kind nothing holds is an error,
// never a reason to fall back to the kind that does hold the name.
func TestResolveWorkloadTarget_KindThatMatchesNothingIsNotFound(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "shop-prod", "api", nil)
	cs := k8sfake.NewSimpleClientset(runningPod("api-1", "shop-prod", "api"))

	req := execRequest("api")
	req.kind = targetKindService

	_, err := req.resolve(context.Background(), cs, dyn, bareCluster())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "service")

	var notFound *workloadTargetNotFoundError
	assert.True(t, stderrors.As(err, &notFound))
}

// `kip app promote` builds a Deployment with no App CR behind it, so the CR
// lookup finds nothing and the Deployment is the only way to place the app.
func TestResolveWorkloadTarget_FindsAPromotedCRlessApp(t *testing.T) {
	dyn := fakeWorkloadDynamic() // nothing promoted has a CR
	cs := k8sfake.NewSimpleClientset(
		kipperDeployment("web", "shop-prod", nil),
		runningPod("web-1", "shop-prod", "web"),
	)

	target, err := execRequest("web").resolve(context.Background(), cs, dyn, bareCluster())
	require.NoError(t, err)
	assert.Equal(t, targetKindApp, target.candidate.kind)
	assert.Equal(t, "shop-prod", target.candidate.namespace)
}

// An ordinary App has both a CR and a Deployment. It must count once, or every
// app on the cluster reports as ambiguous with itself.
func TestResolveWorkloadTarget_AnAppWithBothACRAndADeploymentCountsOnce(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "shop-prod", "api", nil)
	cs := k8sfake.NewSimpleClientset(
		kipperDeployment("api", "shop-prod", nil),
		runningPod("api-1", "shop-prod", "api"),
	)

	target, err := execRequest("api").resolve(context.Background(), cs, dyn, bareCluster())
	require.NoError(t, err, "one App is one candidate however many objects represent it")
	assert.Equal(t, "shop-prod", target.candidate.namespace)
}

// A promoted App in one namespace and a CR App of the same name in another are
// two workloads, and the operator has to say which.
func TestResolveWorkloadTarget_PromotedAndCRAppsInDifferentNamespacesAreAmbiguous(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "shop-prod", "api", nil)
	cs := k8sfake.NewSimpleClientset(
		kipperDeployment("api", "shop-test", nil),
		runningPod("api-1", "shop-prod", "api"),
		runningPod("api-2", "shop-test", "api"),
	)

	_, err := execRequest("api").resolve(context.Background(), cs, dyn, bareCluster())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shop-prod")
	assert.Contains(t, err.Error(), "shop-test")
}

// Two promoted CR-less Apps of the same name, in two namespaces, likewise.
func TestResolveWorkloadTarget_TwoPromotedAppsAreAmbiguous(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	cs := k8sfake.NewSimpleClientset(
		kipperDeployment("web", "shop-prod", nil),
		kipperDeployment("web", "shop-test", nil),
		runningPod("web-1", "shop-prod", "web"),
		runningPod("web-2", "shop-test", "web"),
	)

	_, err := execRequest("web").resolve(context.Background(), cs, dyn, bareCluster())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shop-prod")
	assert.Contains(t, err.Error(), "shop-test")
}

// A Function's Deployment carries the same app=<name> label, so the CR-less App
// fallback must exclude it. Otherwise a Function resolves as an App.
func TestResolveWorkloadTarget_AFunctionDeploymentIsNotASecondCandidate(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.FunctionGVR, "Function", "tools-prod", "resize", nil)
	cs := k8sfake.NewSimpleClientset(
		kipperDeployment("resize", "tools-prod", map[string]string{"kipper.run/resource-type": "function"}),
		functionPod("resize-1", "tools-prod", "resize"),
	)

	target, err := execRequest("resize").resolve(context.Background(), cs, dyn, bareCluster())
	require.NoError(t, err, "one Function is one candidate, not a Function plus an App")
	assert.Equal(t, targetKindFunction, target.candidate.kind)
}

// During a rollout the old pod is Running with a deletionTimestamp while the
// new one is Ready. tunnel must land on the replacement.
func TestResolveWorkloadTarget_TunnelPrefersTheReadyReplacement(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "shop-prod", "api", nil)
	// Same tie-break trap as the exec case: the pod on its way out sorts first.
	cs := k8sfake.NewSimpleClientset(
		terminatingPod("api-0-going", "shop-prod", "api"),
		runningPod("api-1-staying", "shop-prod", "api"),
	)

	target, err := tunnelRequest("api").resolve(context.Background(), cs, dyn, bareCluster())
	require.NoError(t, err)
	assert.Equal(t, "api-1-staying", target.pod.Name)
}

// Forwarding a port to a pod that cannot serve is a worse default than saying
// so, and the failure it produces looks like a broken app rather than a
// mistargeted tunnel.
func TestResolveWorkloadTarget_TunnelRefusesWhenNothingIsReady(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "shop-prod", "api", nil)
	cs := k8sfake.NewSimpleClientset(unreadyPod("api-1", "shop-prod", "api"))

	_, err := tunnelRequest("api").resolve(context.Background(), cs, dyn, bareCluster())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ready")
}

// Debugging an unready pod is a legitimate reason to be there, so exec accepts
// what tunnel refuses.
func TestResolveWorkloadTarget_ExecAcceptsAnUnreadyPod(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "shop-prod", "api", nil)
	cs := k8sfake.NewSimpleClientset(unreadyPod("api-1", "shop-prod", "api"))

	target, err := execRequest("api").resolve(context.Background(), cs, dyn, bareCluster())
	require.NoError(t, err, "a running-but-unready pod is exactly what an operator wants a shell in")
	assert.Equal(t, "api-1", target.pod.Name)
}

// A pod that has not started has no container to enter, whichever command asks.
func TestResolveWorkloadTarget_ExecRefusesAPendingPod(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "shop-prod", "api", nil)
	cs := k8sfake.NewSimpleClientset(pendingPod("api-1", "shop-prod", "api"))

	_, err := execRequest("api").resolve(context.Background(), cs, dyn, bareCluster())
	require.Error(t, err, "Pending has no running container")
}

// A workload that exists but has no pod must not read as a workload that does
// not exist — the remedy is different.
func TestResolveWorkloadTarget_NamesTheNamespaceWhenTheWorkloadHasNoPod(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "shop-prod", "api", nil)
	cs := k8sfake.NewSimpleClientset()

	_, err := execRequest("api").resolve(context.Background(), cs, dyn, bareCluster())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shop-prod", "the operator needs to know where we looked")
}

// The bare-name form has to survive: refuse only when genuinely unsure.
func TestResolveWorkloadTarget_ASingleMatchNeedsNoFlags(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.ServiceGVR, "Service", "shop-prod", "db", nil)
	cs := k8sfake.NewSimpleClientset(servicePod("db-0", "db"))

	target, err := tunnelRequest("db").resolve(context.Background(), cs, dyn, bareCluster())
	require.NoError(t, err)
	assert.Equal(t, "db-0", target.pod.Name)
	assert.Equal(t, targetKindService, target.candidate.kind)
	assert.Equal(t, int32(3000), target.containerPort(), "tunnel reads the port off the pod it resolved")
}

// The org prefix is part of how a project becomes a namespace, and the request
// must go through the cluster's own resolution rather than reimplementing it.
func TestResolveWorkloadTarget_HonoursTheOrgPrefix(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "acme-shop-prod", "api", nil)
	cs := k8sfake.NewSimpleClientset(runningPod("api-1", "acme-shop-prod", "api"))

	req := execRequest("api")
	req.project, req.environment = "shop", "prod"

	target, err := req.resolve(context.Background(), cs, dyn, &config.Cluster{Org: "acme"})
	require.NoError(t, err)
	assert.Equal(t, "acme-shop-prod", target.candidate.namespace)
}

// servicePod and functionPod carry the labels their reconcilers set, which is
// the only thing separating them from an app's pods once app=<name> matches all
// three.
func servicePod(name, workload string) *corev1.Pod {
	pod := runningPod(name, "shop-prod", workload)
	pod.Labels["kipper.run/service-type"] = "postgres"
	return pod
}

func functionPod(name, ns, workload string) *corev1.Pod {
	pod := runningPod(name, ns, workload)
	pod.Labels["kipper.run/resource-type"] = "function"
	return pod
}

// Choosing the candidate is only half the job. An App and a Service called api
// in one namespace have pods carrying the same app=api, so a kind-blind pod
// lookup hands back whichever sorts first and quietly undoes --kind. The app
// pod is named to sort first here, so the test fails if the selector stops
// discriminating.
func TestResolveWorkloadTarget_KindPicksTheRightPodNotJustTheRightCandidate(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "shop-prod", "api", nil)
	seedWorkload(t, dyn, manifest.ServiceGVR, "Service", "shop-prod", "api", nil)
	cs := k8sfake.NewSimpleClientset(
		runningPod("api-0-the-app", "shop-prod", "api"),
		servicePod("api-1-the-service", "api"),
	)

	req := execRequest("api")
	req.kind = targetKindService

	target, err := req.resolve(context.Background(), cs, dyn, bareCluster())
	require.NoError(t, err)
	assert.Equal(t, "api-1-the-service", target.pod.Name, "--kind service must not open a shell in the app")
}

// The same collision between an App and a Function.
func TestResolveWorkloadTarget_KindPicksTheFunctionPod(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "shop-prod", "api", nil)
	seedWorkload(t, dyn, manifest.FunctionGVR, "Function", "shop-prod", "api", nil)
	cs := k8sfake.NewSimpleClientset(
		runningPod("api-0-the-app", "shop-prod", "api"),
		functionPod("api-1-the-function", "shop-prod", "api"),
	)

	req := execRequest("api")
	req.kind = targetKindFunction

	target, err := req.resolve(context.Background(), cs, dyn, bareCluster())
	require.NoError(t, err)
	assert.Equal(t, "api-1-the-function", target.pod.Name)
}

// And the app must not be reached through another kind's pods either: an app
// resolved by name has to land on the pods carrying neither marker label.
func TestResolveWorkloadTarget_AnAppDoesNotLandOnAServicePod(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "shop-prod", "api", nil)
	seedWorkload(t, dyn, manifest.ServiceGVR, "Service", "shop-prod", "api", nil)
	cs := k8sfake.NewSimpleClientset(
		servicePod("api-0-the-service", "api"),
		runningPod("api-1-the-app", "shop-prod", "api"),
	)

	req := execRequest("api")
	req.kind = targetKindApp

	target, err := req.resolve(context.Background(), cs, dyn, bareCluster())
	require.NoError(t, err)
	assert.Equal(t, "api-1-the-app", target.pod.Name)
}

// workloadTargetFlags is the production path from cobra to a request, so these
// drive a real command with real flag parsing rather than a hand-built request.
func targetFlagCmd(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "exec", RunE: func(*cobra.Command, []string) error { return nil }}
	cmd.Flags().String("project", "", "project name")
	cmd.Flags().String("environment", "", "target environment")
	cmd.Flags().String("kind", "", "workload kind")
	require.NoError(t, cmd.ParseFlags(args))
	return cmd
}

// `kip exec api --project "$PROJECT"` with the variable unset marks the flag
// changed but empty, which suppresses the saved project. Reading that as
// "search every project" turns a narrowing flag into a widening one.
func TestWorkloadTargetFlags_RefusesAnEmptyExplicitProject(t *testing.T) {
	cmd := targetFlagCmd(t, "--project", "")
	cluster := &config.Cluster{CurrentProject: "shop", CurrentEnvironment: "prod"}

	_, err := workloadTargetFlags(cmd, cluster, "api", acceptUnready)
	require.Error(t, err, "an empty --project must not silently widen the search to every project")
	assert.Contains(t, err.Error(), "--project")
}

// An environment cannot be turned into a namespace on its own, so accepting one
// without a project would drop it silently.
func TestWorkloadTargetFlags_RefusesAnEnvironmentWithNoProject(t *testing.T) {
	cmd := targetFlagCmd(t, "--environment", "prod")

	_, err := workloadTargetFlags(cmd, &config.Cluster{}, "api", acceptUnready)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--project")
}

func TestWorkloadTargetFlags_CarriesTheSavedContextAndKind(t *testing.T) {
	cmd := targetFlagCmd(t, "--kind", "service")
	cluster := &config.Cluster{CurrentProject: "shop", CurrentEnvironment: "prod"}

	req, err := workloadTargetFlags(cmd, cluster, "api", preferReady)
	require.NoError(t, err)
	assert.Equal(t, "shop", req.project)
	assert.Equal(t, "prod", req.environment)
	assert.Equal(t, targetKindService, req.kind)
	assert.Equal(t, preferReady, req.preference)
	assert.Equal(t, "api", req.name)
}

func TestWorkloadTargetFlags_RejectsAnUnknownKind(t *testing.T) {
	cmd := targetFlagCmd(t, "--kind", "job")

	_, err := workloadTargetFlags(cmd, &config.Cluster{}, "api", acceptUnready)
	require.Error(t, err)
}

// docs/en/installation.md quotes these two messages verbatim under "Naming one
// workload". Pinning them here is what stops the docs drifting from the CLI.
func TestAmbiguousTargetError_MessagesTheDocsQuote(t *testing.T) {
	severalNamespaces := &ambiguousTargetError{name: "api", candidates: []workloadCandidate{
		{targetKindApp, "blog-prod"}, {targetKindApp, "blog-test"},
	}}
	assert.Equal(t, `"api" matches more than one workload:
  app/blog-prod
  app/blog-test
Name the one you mean with --project, plus --environment if the project has environments.`, severalNamespaces.Error())

	twoKindsOneNamespace := &ambiguousTargetError{name: "api", candidates: []workloadCandidate{
		{targetKindApp, "blog-prod"}, {targetKindService, "blog-prod"},
	}}
	assert.Equal(t, `"api" matches more than one workload in blog-prod:
  app/blog-prod
  service/blog-prod
Name the one you mean with --kind app or --kind service.`, twoKindsOneNamespace.Error())

	// Differing in both dimensions needs both remedies, and neither alone.
	both := &ambiguousTargetError{name: "api", candidates: []workloadCandidate{
		{targetKindApp, "blog-prod"}, {targetKindService, "tools-prod"},
	}}
	assert.Contains(t, both.Error(), "--project")
	assert.Contains(t, both.Error(), "--kind")
}

func TestParseTargetKind(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want targetKind
	}{
		{"", ""},
		{"app", targetKindApp},
		{"function", targetKindFunction},
		{"service", targetKindService},
	} {
		got, err := parseTargetKind(tc.in)
		require.NoError(t, err, tc.in)
		assert.Equal(t, tc.want, got)
	}

	_, err := parseTargetKind("job")
	require.Error(t, err, "a kind these commands cannot address must be refused, not ignored")
	assert.Contains(t, err.Error(), "app")
	assert.Contains(t, err.Error(), "function")
	assert.Contains(t, err.Error(), "service")
}
