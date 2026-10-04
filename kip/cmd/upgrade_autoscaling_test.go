package cmd

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/controller/pkg/capacity"
	"github.com/getkipper/kipper/kip/internal/deployer"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

const repairVersion = "v0.24.0"

func i32(v int32) *int32 { return &v }

// bounded is a valid policy with bounds 2..5 and a CPU target.
func bounded(enabled bool) *capacity.Policy {
	return &capacity.Policy{Enabled: enabled, MinReplicas: i32(2), MaxReplicas: i32(5), CPUTarget: i32(70)}
}

func stateOf(p *capacity.Policy, stored, live int32) appState {
	return appState{Namespace: "shop-prod", Name: "web", Policy: p, Replicas: stored, Live: live}
}

func TestAssessAutoscalingDecisionTable(t *testing.T) {
	readErr := errors.New("connection refused")
	cases := []struct {
		name       string
		state      appState
		row        int
		decision   bool
		startsPods bool
		after      *appCapacity
	}{
		{name: "no block", state: stateOf(nil, 0, 0)},
		{name: "opt-out without bounds", state: stateOf(&capacity.Policy{}, 7, 7)},
		{name: "enabled, everything within bounds", state: stateOf(bounded(true), 3, 4)},
		{name: "disabled, live equals stored within bounds", state: stateOf(bounded(false), 3, 3)},

		// Row 2 is checked for every block before the bounds filter, so an
		// enabled block with no max, which has no bounds, is still caught.
		{name: "row 2: enabled without max", row: 2, decision: true,
			state: stateOf(&capacity.Policy{Enabled: true, CPUTarget: i32(70)}, 1, 1)},
		{name: "row 2: min above max on a disabled block", row: 2, decision: true,
			state: stateOf(&capacity.Policy{MinReplicas: i32(6), MaxReplicas: i32(3)}, 4, 4)},
		{name: "row 2: min without max on a disabled block", row: 2, decision: true,
			state: stateOf(&capacity.Policy{MinReplicas: i32(3)}, 3, 3)},
		{name: "disabled block with the CRD's default min and no max", state: stateOf(&capacity.Policy{MinReplicas: i32(1)}, 7, 7)},
		{name: "row 2: enabled without a target", row: 2, decision: true,
			state: stateOf(&capacity.Policy{Enabled: true, MinReplicas: i32(1), MaxReplicas: i32(3)}, 2, 2)},
		{name: "row 2 wins over an unreadable Deployment", row: 2, decision: true,
			state: appState{Namespace: "shop-prod", Name: "web", Policy: &capacity.Policy{Enabled: true, CPUTarget: i32(70)}, LiveErr: readErr}},

		{name: "row 1: Deployment unreadable", row: 1, decision: true,
			state: appState{Namespace: "shop-prod", Name: "web", Policy: bounded(true), Replicas: 3, LiveErr: readErr}},
		{name: "row 1: Deployment missing for a running app", row: 1, decision: true,
			state: appState{Namespace: "shop-prod", Name: "web", Policy: bounded(false), Replicas: 3, NoDeployment: true}},
		{name: "row 1: unreadable Deployment of a stopped app", row: 1, decision: true,
			state: appState{Namespace: "shop-prod", Name: "web", Policy: bounded(true), Replicas: 9, Stopped: true, LiveErr: readErr}},
		{name: "stopped app with a missing Deployment is not decision needed",
			state: appState{Namespace: "shop-prod", Name: "web", Policy: bounded(true), Replicas: 3, Stopped: true, NoDeployment: true}},

		{name: "row 3: stopped, enabled, stored above max", row: 3,
			state: appState{Namespace: "shop-prod", Name: "web", Policy: bounded(true), Replicas: 9, Stopped: true},
			after: &appCapacity{Replicas: 5, Min: 2, Max: 5, Stopped: true}},
		{name: "row 3: stopped, enabled, stored 0", row: 3,
			state: appState{Namespace: "shop-prod", Name: "web", Policy: bounded(true), Replicas: 0, Stopped: true},
			after: &appCapacity{Replicas: 2, Min: 2, Max: 5, Stopped: true}},
		{name: "row 3: stopped, disabled, stored above max widens the bounds", row: 3,
			state: appState{Namespace: "shop-prod", Name: "web", Policy: bounded(false), Replicas: 7, Stopped: true, NoDeployment: true},
			after: &appCapacity{Replicas: 7, Min: 2, Max: 7, Stopped: true}},
		{name: "row 3: stopped, disabled, stored 0 cannot widen to a valid minimum", row: 3, decision: true,
			state: appState{Namespace: "shop-prod", Name: "web", Policy: bounded(false), Replicas: 0, Stopped: true}},
		// The Deployment of a stopped app runs 0, which says nothing about the
		// count it restarts at.
		{name: "stopped app never takes the restart count from live",
			state: appState{Namespace: "shop-prod", Name: "web", Policy: bounded(false), Replicas: 3, Stopped: true, Live: 0}},

		{name: "row 4: scaled to 0 before stops existed", row: 4,
			state: stateOf(bounded(false), 0, 0),
			after: &appCapacity{Replicas: 2, Min: 2, Max: 5, Stopped: true, StopReason: "scaled to 0 before Kipper " + repairVersion}},

		{name: "row 5: autoscaled at 0, stored within bounds", row: 5, startsPods: true,
			state: stateOf(bounded(true), 3, 0)},
		{name: "row 5: autoscaled at 0, stored outside bounds", row: 5, startsPods: true,
			state: stateOf(bounded(true), 0, 0),
			after: &appCapacity{Replicas: 2, Min: 2, Max: 5}},

		// Row 6 before row 4: with the policy on, a stored 0 is an inert count.
		{name: "row 6: policy on, stored 0, live 3", row: 6,
			state: stateOf(bounded(true), 0, 3),
			after: &appCapacity{Replicas: 3, Min: 2, Max: 5}},
		{name: "row 6: policy on, stored above max", row: 6,
			state: stateOf(bounded(true), 9, 4),
			after: &appCapacity{Replicas: 4, Min: 2, Max: 5}},

		{name: "row 7: policy on, live outside bounds", row: 7, decision: true,
			state: stateOf(bounded(true), 3, 9)},

		{name: "row 8: policy off, live differs from stored", row: 8, decision: true,
			state: stateOf(bounded(false), 2, 3)},
		{name: "row 8: policy off, live 0, stored 3", row: 8, decision: true,
			state: stateOf(bounded(false), 3, 0)},

		{name: "row 9: policy off, live equals stored above max", row: 9,
			state: stateOf(bounded(false), 7, 7),
			after: &appCapacity{Replicas: 7, Min: 2, Max: 7}},
		{name: "row 9: policy off, live equals stored below min", row: 9,
			state: stateOf(bounded(false), 1, 1),
			after: &appCapacity{Replicas: 1, Min: 1, Max: 5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := assessAutoscaling(tc.state, repairVersion)
			if tc.row == 0 {
				assert.Nil(t, f, "an app the new rules accept got a finding")
				return
			}
			require.NotNil(t, f, "no finding")
			assert.Equal(t, tc.row, f.Row, "matched the wrong row: %s", f.Problem)
			assert.Equal(t, tc.decision, f.Decision, "decision needed")
			assert.Equal(t, tc.startsPods, f.StartsPods, "starts pods")
			assert.NotEmpty(t, f.Problem)
			if tc.after == nil {
				assert.False(t, f.HasFix, "a fix was proposed: %+v", f.After)
				return
			}
			require.True(t, f.HasFix, "no fix was proposed")
			assert.Equal(t, *tc.after, f.After)
		})
	}
}

// Proposed repairs must leave both the policy and stored count valid.
func TestAssessAutoscalingFixesSatisfyTheRules(t *testing.T) {
	states := []appState{
		{Namespace: "a", Name: "w", Policy: bounded(true), Replicas: 9, Stopped: true},
		{Namespace: "a", Name: "w", Policy: bounded(false), Replicas: 7, Stopped: true},
		stateOf(bounded(false), 0, 0),
		stateOf(bounded(true), 0, 0),
		stateOf(bounded(true), 0, 3),
		stateOf(bounded(false), 7, 7),
	}
	for _, s := range states {
		f := assessAutoscaling(s, repairVersion)
		require.NotNil(t, f)
		require.True(t, f.HasFix)
		p := *s.Policy
		p.MinReplicas, p.MaxReplicas = i32(f.After.Min), i32(f.After.Max)
		assert.NoError(t, capacity.Validate(&p, &f.After.Replicas), "row %d", f.Row)
	}
}

func TestAssessAutoscalingSaysWhatHappensNext(t *testing.T) {
	within := assessAutoscaling(stateOf(bounded(false), 3, 4), repairVersion)
	require.NotNil(t, within)
	assert.Contains(t, within.Problem, "3")
	assert.Contains(t, within.Problem, "4")
	assert.Contains(t, within.Problem, "restores 3")

	outside := assessAutoscaling(stateOf(bounded(false), 8, 4), repairVersion)
	require.NotNil(t, outside)
	assert.Contains(t, outside.Problem, "ReplicasOutsideBounds")

	stopped := assessAutoscaling(stateOf(bounded(false), 0, 0), repairVersion)
	require.NotNil(t, stopped)
	assert.Contains(t, stopped.Problem, "Stopped", "row 4 must say the app will show as stopped")

	zero := assessAutoscaling(stateOf(bounded(true), 3, 0), repairVersion)
	require.NotNil(t, zero)
	assert.Contains(t, zero.Advice, "kip app stop web")
	assert.Contains(t, zero.Advice, "v0.23.0")

	invalid := assessAutoscaling(stateOf(&capacity.Policy{Enabled: true, MinReplicas: i32(6), MaxReplicas: i32(3), CPUTarget: i32(70)}, 4, 4), repairVersion)
	require.NotNil(t, invalid)
	assert.Contains(t, invalid.Advice, "kip app autoscale web --max 6")

	noMax := assessAutoscaling(stateOf(&capacity.Policy{Enabled: true}, 1, 1), repairVersion)
	require.NotNil(t, noMax)
	assert.Contains(t, noMax.Advice, "kip app autoscale web --max 5 --cpu 70")

	disabledInvalid := assessAutoscaling(stateOf(&capacity.Policy{MinReplicas: i32(6), MaxReplicas: i32(3)}, 4, 4), repairVersion)
	require.NotNil(t, disabledInvalid)
	assert.Contains(t, disabledInvalid.Advice, "kip app autoscale web --remove",
		"a disabled block must not be fixed with a command that enables it")

	minWithoutMax := assessAutoscaling(stateOf(&capacity.Policy{MinReplicas: i32(3)}, 3, 3), repairVersion)
	require.NotNil(t, minWithoutMax)
	assert.Contains(t, minWithoutMax.Problem, "set maxReplicas with minReplicas")
	assert.Contains(t, minWithoutMax.Advice, "kip app autoscale web --remove")
}

// Each command the invalid-policy advice names has to leave a policy that
// 'kip app autoscale' accepts, because the upgrade stays blocked otherwise.
func TestInvalidPolicyAdvice_CommandFixesThePolicy(t *testing.T) {
	tests := []struct {
		name   string
		policy *capacity.Policy
	}{
		{"min above max", &capacity.Policy{Enabled: true, MinReplicas: i32(6), MaxReplicas: i32(3), CPUTarget: i32(70)}},
		{"no max and no target", &capacity.Policy{Enabled: true}},
		{"negative memory next to cpu", &capacity.Policy{Enabled: true, MinReplicas: i32(1), MaxReplicas: i32(5), CPUTarget: i32(70), MemoryTarget: i32(-1)}},
		{"negative cpu next to memory", &capacity.Policy{Enabled: true, MinReplicas: i32(1), MaxReplicas: i32(5), CPUTarget: i32(-1), MemoryTarget: i32(80)}},
		{"both targets negative", &capacity.Policy{Enabled: true, MinReplicas: i32(1), MaxReplicas: i32(5), CPUTarget: i32(-1), MemoryTarget: i32(-1)}},
		{"negative memory alone", &capacity.Policy{Enabled: true, MinReplicas: i32(1), MaxReplicas: i32(5), MemoryTarget: i32(-5)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Error(t, capacity.Validate(tt.policy, nil), "the fixture must start invalid")
			f := assessAutoscaling(stateOf(tt.policy, 1, 1), repairVersion)
			require.NotNil(t, f)
			require.True(t, f.Decision)

			autoscale := map[string]any{"enabled": true}
			for name, v := range map[string]*int32{
				"minReplicas": tt.policy.MinReplicas, "maxReplicas": tt.policy.MaxReplicas,
				"cpuTarget": tt.policy.CPUTarget, "memoryTarget": tt.policy.MemoryTarget,
			} {
				if v != nil {
					autoscale[name] = int64(*v)
				}
			}
			dyn := fakeWorkloadDynamic()
			_, err := dyn.Resource(deployer.AppGVR).Namespace("shop-prod").Create(context.Background(), autoscaledApp("web", autoscale, nil), metav1.CreateOptions{})
			require.NoError(t, err)

			d := &deployer.Deployer{Dynamic: dyn}
			_, err = d.EnableAutoscale(context.Background(), "shop-prod", "web", adviceChange(t, f.Advice))
			require.NoError(t, err, "the advised command %q must succeed", f.Advice)
		})
	}
}

// adviceChange reads the flags of the 'kip app autoscale web ...' command the
// advice names, the way the command's flag parsing would.
func adviceChange(t *testing.T, advice string) deployer.AutoscaleChange {
	t.Helper()
	const prefix = "'kip app autoscale web "
	start := strings.Index(advice, prefix)
	require.GreaterOrEqual(t, start, 0, "advice %q names no autoscale command", advice)
	command, _, closed := strings.Cut(advice[start+len(prefix):], "'")
	require.True(t, closed, "advice %q does not close its command", advice)
	fields := strings.Fields(command)
	require.Zero(t, len(fields)%2, "flags come in pairs: %v", fields)

	var c deployer.AutoscaleChange
	targets := map[string]**int32{"--min": &c.MinReplicas, "--max": &c.MaxReplicas, "--cpu": &c.CPUTarget, "--memory": &c.MemoryTarget}
	for i := 0; i < len(fields); i += 2 {
		dst, ok := targets[fields[i]]
		require.True(t, ok, "unknown flag %q", fields[i])
		v, err := strconv.ParseInt(fields[i+1], 10, 32)
		require.NoError(t, err)
		*dst = i32(int32(v))
	}
	return c
}

// Clusters predating stops start autoscaled apps at zero during upgrade.
// Applying replicas: 0 drops the field, and older kip builds pull the same
// mutable console-api tag, so neither avoids the restart.
func TestRow5Advice_OlderClusterIsToldTheAppStarts(t *testing.T) {
	f := assessAutoscaling(stateOf(bounded(true), 3, 0), repairVersion)
	require.NotNil(t, f)
	require.True(t, f.StartsPods)
	assert.NotContains(t, f.Advice, "kip apply")
	assert.NotContains(t, f.Advice, "kip v0.23.0", "an older kip pulls this release's console-api, which starts the app")
	assert.Contains(t, f.Advice, "older than v0.23.0")
	assert.Contains(t, f.Advice, "starts it at 2 pod(s)")
	assert.Contains(t, f.Advice, "'kip app stop web' once the upgrade finishes")

	assert.True(t, strings.HasSuffix(installer.ConsoleAPIImage, ":latest"),
		"if kip pins console-api to a release tag, an upgrade through an older kip keeps a pre-D7 console-api and is worth advising")

	res := manifest.Convert(&manifest.Manifest{Project: "shop", Apps: map[string]manifest.AppSpec{"web": {Image: "nginx", Port: 80, Replicas: i32(0)}}}, "shop-prod")
	require.Len(t, res, 1)
	require.Equal(t, "App", res[0].Object.GetKind())
	_, declared, _ := unstructured.NestedInt64(res[0].Object.Object, "spec", "replicas")
	assert.False(t, declared, "if apply starts carrying replicas: 0, the apply route is worth advising again")

	stopped := stateOf(bounded(true), 3, 0)
	stopped.Stopped = true
	assert.Nil(t, assessAutoscaling(stopped, repairVersion), "the stopped app the advice ends with needs nothing from a later upgrade")
}

// Fixtures for the cluster-level passes.

func autoscaledApp(name string, autoscale map[string]any, replicas *int64) *unstructured.Unstructured {
	spec := map[string]any{"image": "nginx", "port": int64(80)}
	if autoscale != nil {
		spec["autoscale"] = autoscale
	}
	if replicas != nil {
		spec["replicas"] = *replicas
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": deployer.AppGVR.GroupVersion().String(),
		"kind":       "App",
		"metadata":   map[string]any{"name": name, "namespace": "shop-prod"},
		"spec":       spec,
	}}
}

func i64(v int64) *int64 { return &v }

func block(enabled bool, minReplicas, maxReplicas int64) map[string]any {
	return map[string]any{"enabled": enabled, "minReplicas": minReplicas, "maxReplicas": maxReplicas, "cpuTarget": int64(70)}
}

func appDeployment(name string, replicas *int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop-prod"},
		Spec:       appsv1.DeploymentSpec{Replicas: replicas},
	}
}

func envNamespace(name, project, environment string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
		"kipper.run/project": project, "kipper.run/environment": environment,
	}}}
}

func storedApp(t *testing.T, dyn *dynamicfake.FakeDynamicClient, name string) *unstructured.Unstructured {
	t.Helper()
	app, err := dyn.Resource(deployer.AppGVR).Namespace("shop-prod").Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return app
}

func storedReplicasOf(t *testing.T, dyn *dynamicfake.FakeDynamicClient, name string) int64 {
	t.Helper()
	v, found, _ := unstructured.NestedInt64(storedApp(t, dyn, name).Object, "spec", "replicas")
	require.True(t, found, "spec.replicas is absent")
	return v
}

func appWrites(dyn *dynamicfake.FakeDynamicClient) int {
	n := 0
	for _, a := range dyn.Actions() {
		if a.GetVerb() == "update" || a.GetVerb() == "patch" || a.GetVerb() == "create" || a.GetVerb() == "delete" {
			n++
		}
	}
	return n
}

func clusterWrites(clientset *k8sfake.Clientset) int {
	n := 0
	for _, a := range clientset.Actions() {
		switch a.GetVerb() {
		case "get", "list", "watch":
		default:
			n++
		}
	}
	return n
}

func answerNever() (bool, error) { panic("the operator was asked") }

func answerYes() (bool, error) { return true, nil }

func answerNo() (bool, error) { return false, nil }

// fixOnlyCluster needs only a stored-count repair (row 6).
func fixOnlyCluster() (*k8sfake.Clientset, *dynamicfake.FakeDynamicClient) {
	clientset := k8sfake.NewClientset(
		envNamespace("shop-prod", "shop", "prod"),
		appDeployment("web", i32(3)),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		autoscaledApp("web", block(true, 2, 5), i64(0)))
	return clientset, dyn
}

func TestConsentOnACleanClusterAsksNothing(t *testing.T) {
	clientset := k8sfake.NewClientset(appDeployment("web", i32(3)))
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		autoscaledApp("web", block(true, 2, 5), i64(3)),
		autoscaledApp("plain", nil, i64(0)))
	var out bytes.Buffer

	repair, err := autoscalingRepairConsent(context.Background(), clientset, dyn, &out, repairVersion,
		autoscalingConsentOptions{}, answerNever)

	require.NoError(t, err)
	assert.Empty(t, repair.consented)
	assert.Empty(t, out.String(), "a clean cluster printed something")
}

func TestConsentWithTheFlagAsksNothing(t *testing.T) {
	clientset, dyn := fixOnlyCluster()
	var out bytes.Buffer

	repair, err := autoscalingRepairConsent(context.Background(), clientset, dyn, &out, repairVersion,
		autoscalingConsentOptions{Repair: true}, answerNever)

	require.NoError(t, err)
	require.Len(t, repair.consented, 1)
	assert.Contains(t, out.String(), "shop-prod/web")
	assert.Contains(t, out.String(), "replicas: 0 → 3", "the fix was not shown with before and after")
	assert.Zero(t, appWrites(dyn), "consent wrote to an app")
}

func TestConsentNonInteractiveWithoutTheFlagStopsBeforeAnyChange(t *testing.T) {
	clientset, dyn := fixOnlyCluster()
	var out bytes.Buffer

	_, err := autoscalingRepairConsent(context.Background(), clientset, dyn, &out, repairVersion,
		autoscalingConsentOptions{}, answerNever)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--repair-autoscaling")
	assert.Contains(t, err.Error(), "Nothing was changed")
	assert.Zero(t, appWrites(dyn))
	assert.Zero(t, clusterWrites(clientset))
}

func TestConsentInteractive(t *testing.T) {
	clientset, dyn := fixOnlyCluster()
	repair, err := autoscalingRepairConsent(context.Background(), clientset, dyn, &bytes.Buffer{}, repairVersion,
		autoscalingConsentOptions{IsTTY: true}, answerYes)
	require.NoError(t, err)
	assert.Len(t, repair.consented, 1)

	clientset, dyn = fixOnlyCluster()
	_, err = autoscalingRepairConsent(context.Background(), clientset, dyn, &bytes.Buffer{}, repairVersion,
		autoscalingConsentOptions{IsTTY: true}, answerNo)
	require.Error(t, err, "a declined prompt carried on with data the new rules refuse")
	assert.Contains(t, err.Error(), "Nothing was changed")
	assert.Zero(t, appWrites(dyn))
}

func TestConsentStopsOnADecisionAndNamesTheCommand(t *testing.T) {
	clientset := k8sfake.NewClientset(
		envNamespace("shop-prod", "shop", "prod"),
		appDeployment("web", i32(3)),
		appDeployment("api", i32(3)),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		autoscaledApp("web", block(true, 2, 5), i64(0)),
		autoscaledApp("api", block(true, 6, 3), i64(3)))
	var out bytes.Buffer

	_, err := autoscalingRepairConsent(context.Background(), clientset, dyn, &out, repairVersion,
		autoscalingConsentOptions{Repair: true}, answerNever)

	require.Error(t, err, "a decision-needed app did not stop the upgrade")
	assert.Contains(t, err.Error(), "--skip-autoscaling-check")
	assert.Contains(t, out.String(), "kip app autoscale api --project shop --environment prod --max 6")
	assert.Zero(t, appWrites(dyn))
}

func TestConsentSkippingDecisionsStillAsksAboutFixes(t *testing.T) {
	clientset := k8sfake.NewClientset(
		appDeployment("web", i32(3)),
		appDeployment("api", i32(3)),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		autoscaledApp("web", block(true, 2, 5), i64(0)),
		autoscaledApp("api", block(true, 6, 3), i64(3)))

	asked := false
	repair, err := autoscalingRepairConsent(context.Background(), clientset, dyn, &bytes.Buffer{}, repairVersion,
		autoscalingConsentOptions{SkipDecisions: true, IsTTY: true}, func() (bool, error) { asked = true; return true, nil })

	require.NoError(t, err)
	assert.True(t, asked, "the fixes were not asked about")
	require.Len(t, repair.consented, 1)
	assert.Equal(t, "web", repair.consented[0].Name, "a decision-needed app was put up for writing")
}

// Row 5 is reported and not gated: with nothing to write, a scripted run
// carries on, and the operator is told how to keep the app at 0.
func TestConsentReportsRowFiveWithoutStopping(t *testing.T) {
	clientset := k8sfake.NewClientset(appDeployment("web", i32(0)))
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		autoscaledApp("web", block(true, 2, 5), i64(3)))
	var out bytes.Buffer

	repair, err := autoscalingRepairConsent(context.Background(), clientset, dyn, &out, repairVersion,
		autoscalingConsentOptions{}, answerNever)

	require.NoError(t, err)
	assert.Empty(t, repair.consented)
	assert.Contains(t, out.String(), "kip app stop web")
}

func TestUpgradeCheckExitsNonZeroOnADecisionAndWritesNothing(t *testing.T) {
	clientset := k8sfake.NewClientset(appDeployment("api", i32(9)))
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		autoscaledApp("api", block(true, 2, 5), i64(3)))
	var out bytes.Buffer

	err := runUpgradeChecks(context.Background(), clientset, dyn, &out, repairVersion)

	require.Error(t, err)
	assert.Contains(t, out.String(), "shop-prod/api")
	assert.Zero(t, appWrites(dyn))
	assert.Zero(t, clusterWrites(clientset))
}

func TestUpgradeCheckPassesWithFixesOnlyAndShowsThem(t *testing.T) {
	clientset, dyn := fixOnlyCluster()
	var out bytes.Buffer

	require.NoError(t, runUpgradeChecks(context.Background(), clientset, dyn, &out, repairVersion))
	assert.Contains(t, out.String(), "replicas: 0 → 3")
	assert.Zero(t, appWrites(dyn))
}

func TestUpgradeCheckOnACleanCluster(t *testing.T) {
	clientset := k8sfake.NewClientset()
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme())
	var out bytes.Buffer

	require.NoError(t, runUpgradeChecks(context.Background(), clientset, dyn, &out, repairVersion))
	assert.Contains(t, out.String(), "nothing for the upgrade to fix")
}

func TestUpgradeCheckFlagIsRegistered(t *testing.T) {
	for _, name := range []string{"check", "repair-autoscaling", "skip-autoscaling-check"} {
		assert.NotNil(t, upgradeCmd.Flags().Lookup(name), "--%s is missing", name)
	}
}

// The write pass.

// A console-api that has finished rolling and has nothing left over from
// before: the closing pass's precondition for writing.
func quiescentConsoleAPI() []runtime.Object {
	return []runtime.Object{consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision), consoleAPIPod("console-api-new", false)}
}

func rolledRecord() consoleAPIRollout {
	return consoleAPIRollout{uid: "console-api-uid", hash: currentRevision}
}

func consentTo(t *testing.T, clientset *k8sfake.Clientset, dyn *dynamicfake.FakeDynamicClient) autoscalingRepair {
	t.Helper()
	repair, err := autoscalingRepairConsent(context.Background(), clientset, dyn, &bytes.Buffer{}, repairVersion,
		autoscalingConsentOptions{Repair: true}, answerNever)
	require.NoError(t, err)
	return repair
}

func TestRepairWritesAfterQuiescenceAndLeavesPodsAlone(t *testing.T) {
	shortQuiescence(t)
	objs := append(quiescentConsoleAPI(), appDeployment("web", i32(3)))
	clientset := k8sfake.NewClientset(objs...)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		autoscaledApp("web", block(true, 2, 5), i64(0)))
	repair := consentTo(t, clientset, dyn)
	clientset.ClearActions()
	var out bytes.Buffer

	repairAutoscaling(context.Background(), clientset, dyn, &out, repairVersion, repair, rolledRecord(), true)

	assert.Equal(t, int64(3), storedReplicasOf(t, dyn, "web"))
	assert.Contains(t, out.String(), "replicas: 0 → 3")
	assert.Zero(t, clusterWrites(clientset), "the repair changed something running")
}

func TestRepairRecordsAStopForRowFour(t *testing.T) {
	shortQuiescence(t)
	objs := append(quiescentConsoleAPI(), appDeployment("web", i32(0)))
	clientset := k8sfake.NewClientset(objs...)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		autoscaledApp("web", block(false, 2, 5), i64(0)))
	repair := consentTo(t, clientset, dyn)

	repairAutoscaling(context.Background(), clientset, dyn, &bytes.Buffer{}, repairVersion, repair, rolledRecord(), true)

	app := storedApp(t, dyn, "web")
	reason, _, _ := unstructured.NestedString(app.Object, "spec", "stopped", "reason")
	assert.Equal(t, "scaled to 0 before Kipper "+repairVersion, reason)
	by, _, _ := unstructured.NestedString(app.Object, "spec", "stopped", "by")
	assert.Equal(t, "kip upgrade", by)
	assert.Equal(t, int64(2), storedReplicasOf(t, dyn, "web"))
}

func TestRepairWidensBoundsForRowNine(t *testing.T) {
	shortQuiescence(t)
	objs := append(quiescentConsoleAPI(), appDeployment("web", i32(7)))
	clientset := k8sfake.NewClientset(objs...)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		autoscaledApp("web", block(false, 2, 5), i64(7)))
	repair := consentTo(t, clientset, dyn)

	repairAutoscaling(context.Background(), clientset, dyn, &bytes.Buffer{}, repairVersion, repair, rolledRecord(), true)

	app := storedApp(t, dyn, "web")
	maxR, _, _ := unstructured.NestedInt64(app.Object, "spec", "autoscale", "maxReplicas")
	minR, _, _ := unstructured.NestedInt64(app.Object, "spec", "autoscale", "minReplicas")
	enabled, _, _ := unstructured.NestedBool(app.Object, "spec", "autoscale", "enabled")
	assert.Equal(t, int64(7), maxR)
	assert.Equal(t, int64(2), minR)
	assert.False(t, enabled, "widening switched the policy on")
	assert.Equal(t, int64(7), storedReplicasOf(t, dyn, "web"))
}

func TestRepairSkipsWritesWhenTheOldWriterLingers(t *testing.T) {
	shortQuiescence(t)
	clientset := k8sfake.NewClientset(
		consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision), consoleAPIReplicaSet("1", "old456"),
		consoleAPIPod("console-api-new", false), consoleAPIPodOfRevision("console-api-old", "old456", false),
		appDeployment("web", i32(3)),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		autoscaledApp("web", block(true, 2, 5), i64(0)))
	repair := consentTo(t, clientset, dyn)
	dyn.ClearActions()
	var out bytes.Buffer

	repairAutoscaling(context.Background(), clientset, dyn, &out, repairVersion, repair, rolledRecord(), true)

	assert.Zero(t, appWrites(dyn), "an app was written while the old console-api still ran")
	assert.Contains(t, out.String(), "still running")
	assert.Contains(t, out.String(), "kip upgrade", "the operator was not told how to resume")
}

func TestRepairSkipsWritesWhenTheRolloutWasNotRecorded(t *testing.T) {
	shortQuiescence(t)
	objs := append(quiescentConsoleAPI(), appDeployment("web", i32(3)))
	clientset := k8sfake.NewClientset(objs...)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		autoscaledApp("web", block(true, 2, 5), i64(0)))
	repair := consentTo(t, clientset, dyn)
	dyn.ClearActions()
	var out bytes.Buffer

	repairAutoscaling(context.Background(), clientset, dyn, &out, repairVersion, repair, consoleAPIRollout{}, true)

	assert.Zero(t, appWrites(dyn), "an app was written without knowing which console-api serves")
	assert.Contains(t, out.String(), "kip upgrade")
}

// Something reconciled between the consent and the write pass and changed
// the field values the operator agreed to.
func TestRepairReportsChangedValuesInsteadOfWriting(t *testing.T) {
	shortQuiescence(t)
	objs := append(quiescentConsoleAPI(), appDeployment("web", i32(7)))
	clientset := k8sfake.NewClientset(objs...)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		autoscaledApp("web", block(false, 2, 5), i64(7)))
	repair := consentTo(t, clientset, dyn)

	// Row 9 at consent (widen to 7); the live count then moved to 8 and the
	// stored count with it, so the fix would now widen to 8.
	moveTo(t, clientset, dyn, "shop-prod", "web", 8, 8)
	dyn.ClearActions()
	var out bytes.Buffer

	repairAutoscaling(context.Background(), clientset, dyn, &out, repairVersion, repair, rolledRecord(), true)

	assert.Zero(t, appWrites(dyn), "a fix nobody agreed to was written")
	assert.Contains(t, out.String(), "shop-prod/web")
	assert.Contains(t, out.String(), "kip upgrade --check")
}

// Starting an autoscaled app at its minimum changes row 5 to row 6.
// Consent covers the current count within bounds in either case.
func TestRepairWritesARowFiveAppThatDSevenMovedToRowSix(t *testing.T) {
	shortQuiescence(t)
	objs := append(quiescentConsoleAPI(), appDeployment("web", i32(0)))
	clientset := k8sfake.NewClientset(objs...)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		autoscaledApp("web", block(true, 2, 5), i64(0)))
	repair := consentTo(t, clientset, dyn)
	require.Len(t, repair.consented, 1)
	require.Equal(t, 5, repair.consented[0].Row)

	// Startup, then the autoscaler, moved the Deployment to 3.
	setLive(t, clientset, "shop-prod", "web", 3)

	repairAutoscaling(context.Background(), clientset, dyn, &bytes.Buffer{}, repairVersion, repair, rolledRecord(), true)

	assert.Equal(t, int64(3), storedReplicasOf(t, dyn, "web"))
}

func TestRepairIsIdempotent(t *testing.T) {
	shortQuiescence(t)
	objs := append(quiescentConsoleAPI(), appDeployment("web", i32(3)))
	clientset := k8sfake.NewClientset(objs...)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		autoscaledApp("web", block(true, 2, 5), i64(0)))
	repair := consentTo(t, clientset, dyn)

	repairAutoscaling(context.Background(), clientset, dyn, &bytes.Buffer{}, repairVersion, repair, rolledRecord(), true)
	dyn.ClearActions()
	repairAutoscaling(context.Background(), clientset, dyn, &bytes.Buffer{}, repairVersion, repair, rolledRecord(), true)

	assert.Zero(t, appWrites(dyn), "a repeated pass wrote again")
	again := consentTo(t, clientset, dyn)
	assert.Empty(t, again.consented, "a repaired cluster still has findings")
}

// A conflict means somebody else wrote the App, so eligibility and consent
// are decided again from what they wrote.
func TestRepairRecomputesOnAConflictRetry(t *testing.T) {
	shortQuiescence(t)
	objs := append(quiescentConsoleAPI(), appDeployment("web", i32(7)))
	clientset := k8sfake.NewClientset(objs...)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		autoscaledApp("web", block(false, 2, 5), i64(7)))
	repair := consentTo(t, clientset, dyn)

	conflicted := false
	dyn.PrependReactor("update", "apps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if conflicted {
			return false, nil, nil
		}
		conflicted = true
		// The competing write lands, then this one loses the race.
		moveTo(t, clientset, dyn, "shop-prod", "web", 8, 8)
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "kipper.run", Resource: "apps"}, "web", errors.New("changed"))
	})
	var out bytes.Buffer

	repairAutoscaling(context.Background(), clientset, dyn, &out, repairVersion, repair, rolledRecord(), true)

	require.True(t, conflicted)
	maxR, _, _ := unstructured.NestedInt64(storedApp(t, dyn, "web").Object, "spec", "autoscale", "maxReplicas")
	assert.Equal(t, int64(5), maxR, "the retry wrote the fix agreed to for a state that no longer exists")
	assert.Contains(t, out.String(), "kip upgrade --check")
}

func TestRepairStopsAtAFailedWriteAndARerunResumes(t *testing.T) {
	shortQuiescence(t)
	objs := append(quiescentConsoleAPI(),
		appDeployment("api", i32(3)), appDeployment("web", i32(4)))
	clientset := k8sfake.NewClientset(objs...)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		autoscaledApp("api", block(true, 2, 5), i64(0)),
		autoscaledApp("web", block(true, 2, 5), i64(0)))
	repair := consentTo(t, clientset, dyn)
	require.Len(t, repair.consented, 2)

	failing := true
	dyn.PrependReactor("update", "apps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		name := action.(k8stesting.UpdateAction).GetObject().(*unstructured.Unstructured).GetName()
		if failing && name == "api" {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "kipper.run", Resource: "apps"}, "api", errors.New("denied"))
		}
		return false, nil, nil
	})
	var out bytes.Buffer

	repairAutoscaling(context.Background(), clientset, dyn, &out, repairVersion, repair, rolledRecord(), true)

	assert.Contains(t, out.String(), "shop-prod/api", "the failed app was not named")
	assert.Equal(t, int64(0), storedReplicasOf(t, dyn, "web"), "the step went on past a failed write")

	failing = false
	repairAutoscaling(context.Background(), clientset, dyn, &bytes.Buffer{}, repairVersion, consentTo(t, clientset, dyn), rolledRecord(), true)
	assert.Equal(t, int64(3), storedReplicasOf(t, dyn, "api"))
	assert.Equal(t, int64(4), storedReplicasOf(t, dyn, "web"))
}

func TestRepairWithNothingConsentedWaitsForNothing(t *testing.T) {
	clientset := k8sfake.NewClientset()
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme())
	var out bytes.Buffer

	repairAutoscaling(context.Background(), clientset, dyn, &out, repairVersion, autoscalingRepair{}, consoleAPIRollout{}, true)

	assert.Empty(t, out.String())
	assert.Empty(t, clientset.Actions(), "an empty repair looked at the console-api")
}

func setLive(t *testing.T, clientset *k8sfake.Clientset, namespace, name string, live int32) {
	t.Helper()
	dep, err := clientset.AppsV1().Deployments(namespace).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	dep.Spec.Replicas = &live
	_, err = clientset.AppsV1().Deployments(namespace).Update(context.Background(), dep, metav1.UpdateOptions{})
	require.NoError(t, err)
}

// moveTo stands in for a reconcile or an operator writing both counts,
// straight onto the trackers so it does not re-enter any reactor.
func moveTo(t *testing.T, clientset *k8sfake.Clientset, dyn *dynamicfake.FakeDynamicClient, namespace, name string, live int32, stored int64) {
	t.Helper()
	setLive(t, clientset, namespace, name, live)
	obj, err := dyn.Tracker().Get(deployer.AppGVR, namespace, name)
	require.NoError(t, err)
	app := obj.(*unstructured.Unstructured).DeepCopy()
	require.NoError(t, unstructured.SetNestedField(app.Object, stored, "spec", "replicas"))
	require.NoError(t, dyn.Tracker().Update(deployer.AppGVR, app, namespace))
}

// An App schema without spec.stopped accepts the write and drops the stop,
// which would leave the app to start at its minimum. That is a failed write.
func TestRepairRefusesAStopTheSchemaDropped(t *testing.T) {
	shortQuiescence(t)
	objs := append(quiescentConsoleAPI(), appDeployment("web", i32(0)))
	clientset := k8sfake.NewClientset(objs...)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		autoscaledApp("web", block(false, 2, 5), i64(0)))
	repair := consentTo(t, clientset, dyn)
	dyn.PrependReactor("update", "apps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		app := action.(k8stesting.UpdateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		unstructured.RemoveNestedField(app.Object, "spec", "stopped")
		require.NoError(t, dyn.Tracker().Update(deployer.AppGVR, app, "shop-prod"))
		return true, app, nil
	})
	var out bytes.Buffer

	repairAutoscaling(context.Background(), clientset, dyn, &out, repairVersion, repair, rolledRecord(), true)

	assert.Contains(t, out.String(), "Could not write the fix for shop-prod/web")
	assert.Contains(t, out.String(), "did not keep the stop")
}

func TestRepairPointsAtWhatIsStillOpen(t *testing.T) {
	shortQuiescence(t)
	objs := append(quiescentConsoleAPI(),
		appDeployment("web", i32(3)), appDeployment("api", i32(3)))
	clientset := k8sfake.NewClientset(objs...)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		autoscaledApp("web", block(true, 2, 5), i64(0)),
		autoscaledApp("api", block(true, 6, 3), i64(3)))
	repair, err := autoscalingRepairConsent(context.Background(), clientset, dyn, &bytes.Buffer{}, repairVersion,
		autoscalingConsentOptions{Repair: true, SkipDecisions: true}, answerNever)
	require.NoError(t, err)
	var out bytes.Buffer

	repairAutoscaling(context.Background(), clientset, dyn, &out, repairVersion, repair, rolledRecord(), true)

	assert.Equal(t, int64(3), storedReplicasOf(t, dyn, "web"))
	assert.Contains(t, out.String(), "1 app(s) still have autoscaling settings")
	assert.Contains(t, out.String(), "kip upgrade --check")
}

// A stored explicit zero bound is invalid under the CRD rule, so the check
// reads it as set, the way admission and the migration precheck do, and
// leaves the decision to the operator rather than proposing a write the API
// server refuses.
func TestAssessClusterReadsAStoredZeroBoundAsInvalid(t *testing.T) {
	tests := []struct {
		name      string
		autoscale map[string]any
		advice    string
	}{
		{"disabled with min 0", map[string]any{"enabled": false, "minReplicas": int64(0), "maxReplicas": int64(5)}, "kip app autoscale web --remove"},
		{"disabled with max 0", map[string]any{"enabled": false, "maxReplicas": int64(0)}, "kip app autoscale web --remove"},
		{"enabled with min 0", map[string]any{"enabled": true, "minReplicas": int64(0), "maxReplicas": int64(5), "cpuTarget": int64(70)}, "kip app autoscale web --min 1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clientset := k8sfake.NewClientset(appDeployment("web", i32(8)))
			dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), autoscaledApp("web", tc.autoscale, i64(8)))

			findings, err := assessClusterAutoscaling(context.Background(), clientset, dyn, repairVersion)

			require.NoError(t, err)
			require.Len(t, findings, 1)
			f := findings[0]
			assert.Equal(t, 2, f.Row, "matched the wrong row: %s", f.Problem)
			assert.True(t, f.Decision)
			assert.False(t, f.HasFix, "a fix the API server refuses was proposed: %+v", f.After)
			assert.Contains(t, f.Problem, "must be at least 1")
			assert.Contains(t, f.Advice, tc.advice)
		})
	}

	t.Run("a stored zero target beside a positive one stays valid", func(t *testing.T) {
		clientset := k8sfake.NewClientset(appDeployment("web", i32(3)))
		dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), autoscaledApp("web",
			map[string]any{"enabled": true, "minReplicas": int64(2), "maxReplicas": int64(5), "cpuTarget": int64(70), "memoryTarget": int64(0)}, i64(3)))

		findings, err := assessClusterAutoscaling(context.Background(), clientset, dyn, repairVersion)

		require.NoError(t, err)
		assert.Empty(t, findings)
	})
}

func TestUpgradeYesHelpNamesTheAutoscalingRepairFlag(t *testing.T) {
	assert.Contains(t, upgradeCmd.Flags().Lookup("yes").Usage, "--repair-autoscaling",
		"--yes does not answer the autoscaling repair, so its help must say which flag does")
}

// Live is the Deployment's spec.replicas, the count it is set to, which is
// not always the number of pods running.
func TestRowsSevenAndEightNameTheCountTheDeploymentIsSetTo(t *testing.T) {
	seven := assessAutoscaling(stateOf(bounded(true), 3, 9), repairVersion)
	require.NotNil(t, seven)
	assert.Contains(t, seven.Problem, "its Deployment is set to 9,")

	eight := assessAutoscaling(stateOf(bounded(false), 3, 4), repairVersion)
	require.NotNil(t, eight)
	assert.Contains(t, eight.Problem, "its Deployment is set to 4 while it stores 3.")
}
