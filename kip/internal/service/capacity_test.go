package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// Platform requests of 1580m on a 2 vCPU node leave too little capacity for
// the default PostgreSQL request of 500m.

func mi(n int64) int64   { return n * 1024 * 1024 }
func gi(n float64) int64 { return int64(n * 1024 * 1024 * 1024) }

func node(name string, cpu, memory string, unschedulable bool) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{Unschedulable: unschedulable},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(memory)},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

func requestingPod(name, node, cpu, memory string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system"},
		Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(memory)},
		}}}},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func TestNodeHeadroomSubtractsWhatRunningPodsRequest(t *testing.T) {
	client := fake.NewSimpleClientset( //nolint:staticcheck
		node("server", "2", "4Gi", false),
		requestingPod("platform-a", "server", "1000m", "1Gi", corev1.PodRunning),
		requestingPod("platform-b", "server", "580m", "512Mi", corev1.PodPending),
		requestingPod("finished-job", "server", "900m", "2Gi", corev1.PodSucceeded),
	)
	mgr := &Manager{Client: client}

	nodes, err := mgr.NodeHeadroom(context.Background())
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	assert.Equal(t, "server", nodes[0].Node)
	assert.Equal(t, int64(420), nodes[0].CPUMilli, "2000m minus 1580m; a finished job holds nothing")
	assert.Equal(t, gi(2.5), nodes[0].MemoryBytes)
}

func TestNodeHeadroomSkipsNodesThatCannotTakePods(t *testing.T) {
	cordoned := node("worker", "8", "16Gi", true)
	notReady := node("down", "8", "16Gi", false)
	notReady.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}
	// The join validation taint reserves the worker until validation completes.
	tainted := node("joining", "8", "16Gi", false)
	tainted.Spec.Taints = []corev1.Taint{{Key: "kipper.run/checking", Effect: corev1.TaintEffectNoSchedule}}
	client := fake.NewSimpleClientset(node("server", "2", "4Gi", false), cordoned, notReady, tainted) //nolint:staticcheck
	mgr := &Manager{Client: client}

	nodes, err := mgr.NodeHeadroom(context.Background())
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	assert.Equal(t, "server", nodes[0].Node)
}

func TestNodeHeadroomAccountsEachNodeOnAMultiNodeCluster(t *testing.T) {
	client := fake.NewSimpleClientset( //nolint:staticcheck
		node("server", "2", "4Gi", false),
		node("worker-1", "2", "4Gi", false),
		node("worker-2", "2", "4Gi", false),
		requestingPod("a", "server", "1800m", "1Gi", corev1.PodRunning),
		requestingPod("b", "worker-1", "1800m", "1Gi", corev1.PodRunning),
		requestingPod("c", "worker-2", "300m", "1Gi", corev1.PodRunning),
	)
	nodes, err := (&Manager{Client: client}).NodeHeadroom(context.Background())
	require.NoError(t, err)
	byName := map[string]int64{}
	for _, n := range nodes {
		byName[n.Node] = n.CPUMilli
	}
	assert.Equal(t, map[string]int64{"server": 200, "worker-1": 200, "worker-2": 1700}, byName)
}

// Restartable init containers overlap later init containers and app containers,
// so their startup order affects peak requests.
func TestNodeHeadroomUsesTheSchedulersEffectiveRequest(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	cases := map[string]struct {
		init     []corev1.Container
		pod      func(*corev1.Pod)
		wantFree int64
	}{
		"ordinary init before a sidecar": {
			init: []corev1.Container{
				{Name: "prepare", Resources: cpuRequest("1600m")},
				{Name: "sidecar", RestartPolicy: &always, Resources: cpuRequest("200m")},
			},
			wantFree: 400, // The 1600m init request determines the peak.
		},
		"sidecar started before an ordinary init runs alongside it": {
			init: []corev1.Container{
				{Name: "sidecar", RestartPolicy: &always, Resources: cpuRequest("200m")},
				{Name: "prepare", Resources: cpuRequest("1400m")},
			},
			wantFree: 400, // The sidecar runs alongside the 1400m init container.
		},
		"a pod-level request replaces the containers' sum": {
			pod: func(p *corev1.Pod) {
				p.Spec.Resources = &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1600m")}}
			},
			wantFree: 400,
		},
		"runtime overhead comes on top": {
			pod: func(p *corev1.Pod) {
				p.Spec.Overhead = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1500m")}
			},
			wantFree: 400, // Overhead adds 1500m to the app request.
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			pod := requestingPod("p", "server", "100m", "128Mi", corev1.PodRunning)
			pod.Spec.InitContainers = tc.init
			if tc.pod != nil {
				tc.pod(pod)
			}
			client := fake.NewSimpleClientset(node("server", "2", "4Gi", false), pod) //nolint:staticcheck
			nodes, err := (&Manager{Client: client}).NodeHeadroom(context.Background())
			require.NoError(t, err)
			require.Len(t, nodes, 1)
			assert.Equal(t, tc.wantFree, nodes[0].CPUMilli)
		})
	}
}

func cpuRequest(cpu string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}}
}

func TestNodeHeadroomUsesTheSchedulersEffectiveRequestForMemoryToo(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	pod := requestingPod("init-heavy", "server", "100m", "128Mi", corev1.PodRunning)
	pod.Spec.InitContainers = []corev1.Container{
		{Name: "prepare", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1600m")}}},
		{Name: "sidecar", RestartPolicy: &always, Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")}}},
	}
	pod.Spec.InitContainers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("3Gi")
	client := fake.NewSimpleClientset(node("server", "2", "4Gi", false), pod) //nolint:staticcheck
	nodes, err := (&Manager{Client: client}).NodeHeadroom(context.Background())
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	assert.Equal(t, int64(400), nodes[0].CPUMilli, "2000m minus max(1600m init, 100m + 200m sidecar)")
	assert.Equal(t, gi(1), nodes[0].MemoryBytes, "4Gi minus the 3Gi init peak, which beats the 128Mi main container")
}

func TestNodeHeadroomReportsAListingItCannotMake(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck
	client.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, assert.AnError
	})
	_, err := (&Manager{Client: client}).NodeHeadroom(context.Background())
	assert.Error(t, err, "a project operator without node access gets an error, and the caller decides to skip the check")
}

func TestCheckFitsRefusesAServiceNoNodeCanTake(t *testing.T) {
	nodes := []NodeHeadroom{{Node: "server", CPUMilli: 420, MemoryBytes: gi(2.5)}}

	err := CheckFits(nodes, "postgres", "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "postgres")
	assert.Contains(t, err.Error(), "500m", "the request it would get without --cpu")
	assert.Contains(t, err.Error(), "420m", "what the fullest-but-best node still has")
	assert.Contains(t, err.Error(), "--cpu", "the way out")

	assert.NoError(t, CheckFits(nodes, "postgres", "250m", ""), "a smaller --cpu fits")
	assert.NoError(t, CheckFits(nodes, "redis", "", ""), "a 100m service fits as it is")
}

func TestCheckFitsNeedsOneNodeWithRoomForBoth(t *testing.T) {
	nodes := []NodeHeadroom{
		{Node: "cpu-rich", CPUMilli: 4000, MemoryBytes: mi(512)},
		{Node: "memory-rich", CPUMilli: 200, MemoryBytes: gi(8)},
	}
	err := CheckFits(nodes, "postgres", "", "")
	require.Error(t, err, "CPU on one node and memory on another is not a place to run")
	assert.Contains(t, err.Error(), "cpu-rich")
	assert.Contains(t, err.Error(), "memory-rich")

	assert.NoError(t, CheckFits(nodes, "postgres", "", "256Mi"), "with less memory the CPU-rich node takes it")
}

// The cleanup init container sets a minimum request of 10m CPU and 16Mi memory.
func TestCheckFitsCountsTheInitContainerFloor(t *testing.T) {
	nodes := []NodeHeadroom{{Node: "server", CPUMilli: 8, MemoryBytes: mi(8)}}
	assert.ErrorContains(t, CheckFits(nodes, "redis", "5m", "4Mi"), "10m")
	assert.NoError(t, CheckFits([]NodeHeadroom{{Node: "server", CPUMilli: 10, MemoryBytes: mi(16)}}, "redis", "5m", "4Mi"))
}

func TestCheckFitsRefusesAMalformedRequest(t *testing.T) {
	nodes := []NodeHeadroom{{Node: "server", CPUMilli: 4000, MemoryBytes: gi(8)}}
	assert.ErrorContains(t, CheckFits(nodes, "postgres", "lots", ""), "lots")
	assert.ErrorContains(t, CheckFits(nodes, "postgres", "", "many"), "many")
}

func TestCheckFitsWithNoSchedulableNode(t *testing.T) {
	assert.ErrorContains(t, CheckFits(nil, "postgres", "", ""), "no node")
}

func TestFormatMemoryRoundsFreeCapacityDownToMi(t *testing.T) {
	assert.Equal(t, "1Gi", formatMemory(1<<30))
	assert.Equal(t, "256Mi", formatMemory(256<<20))
	assert.Equal(t, "5527Mi", formatMemory(5660044*1024), "a node's free memory reads in Mi, never Ki")
	assert.Equal(t, "0Mi", formatMemory(512*1024))
}
