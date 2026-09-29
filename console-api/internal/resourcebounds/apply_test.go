package resourcebounds

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func requirements(cpuReq, cpuLim, memReq, memLim string) *corev1.ResourceRequirements {
	r := &corev1.ResourceRequirements{Requests: corev1.ResourceList{}, Limits: corev1.ResourceList{}}
	set := func(list corev1.ResourceList, name corev1.ResourceName, v string) {
		if v != "" {
			list[name] = q(v)
		}
	}
	set(r.Requests, corev1.ResourceCPU, cpuReq)
	set(r.Limits, corev1.ResourceCPU, cpuLim)
	set(r.Requests, corev1.ResourceMemory, memReq)
	set(r.Limits, corev1.ResourceMemory, memLim)
	return r
}

func assertResources(t *testing.T, got *corev1.ResourceRequirements, cpuReq, cpuLim, memReq, memLim string) {
	t.Helper()
	check := func(list corev1.ResourceList, name corev1.ResourceName, want, what string) {
		v := list[name]
		if v.Cmp(q(want)) != 0 {
			t.Errorf("%s = %s, want %s", what, v.String(), want)
		}
	}
	check(got.Requests, corev1.ResourceCPU, cpuReq, "cpu request")
	check(got.Limits, corev1.ResourceCPU, cpuLim, "cpu limit")
	check(got.Requests, corev1.ResourceMemory, memReq, "memory request")
	check(got.Limits, corev1.ResourceMemory, memLim, "memory limit")
}

func TestApply(t *testing.T) {
	cases := []struct {
		name    string
		spec    Spec
		rec     *kipperv1.TunedResources
		live    *corev1.ResourceRequirements
		desired *corev1.ResourceRequirements
		want    [4]string
	}{
		{
			name:    "bounds keep a quiet recommendation at the floor",
			spec:    Spec{MemoryRequest: user("512Mi"), MemoryLimit: user("2Gi")},
			rec:     &kipperv1.TunedResources{MemoryRequest: "256Mi", MemoryLimit: "256Mi", CPURequest: "100m", CPULimit: "100m"},
			live:    requirements("100m", "100m", "512Mi", "2Gi"),
			desired: requirements("100m", "100m", "512Mi", "2Gi"),
			want:    [4]string{"100m", "100m", "512Mi", "2Gi"},
		},
		{
			name:    "bounds let a busy recommendation raise the request only",
			spec:    Spec{MemoryRequest: user("512Mi"), MemoryLimit: user("2Gi")},
			rec:     &kipperv1.TunedResources{MemoryRequest: "768Mi", MemoryLimit: "768Mi"},
			live:    requirements("100m", "100m", "512Mi", "2Gi"),
			desired: requirements("100m", "100m", "512Mi", "2Gi"),
			want:    [4]string{"100m", "100m", "768Mi", "2Gi"},
		},
		{
			name:    "an automatic value keeps the live size instead of an old spec value",
			spec:    Spec{MemoryRequest: automatic("512Mi"), MemoryLimit: automatic("512Mi")},
			live:    requirements("100m", "100m", "256Mi", "256Mi"),
			desired: requirements("100m", "100m", "512Mi", "512Mi"),
			want:    [4]string{"100m", "100m", "256Mi", "256Mi"},
		},
		{
			name:    "an automatic value follows the recommendation",
			spec:    Spec{MemoryRequest: automatic("256Mi"), MemoryLimit: automatic("256Mi")},
			rec:     &kipperv1.TunedResources{MemoryRequest: "1Gi", MemoryLimit: "1Gi"},
			live:    requirements("100m", "100m", "256Mi", "256Mi"),
			desired: requirements("100m", "100m", "256Mi", "256Mi"),
			want:    [4]string{"100m", "100m", "1Gi", "1Gi"},
		},
		{
			name:    "a first rollout without a recommendation uses what the reconciler built",
			spec:    Spec{},
			desired: requirements("100m", "100m", "128Mi", "128Mi"),
			want:    [4]string{"100m", "100m", "128Mi", "128Mi"},
		},
		{
			name:    "held values stay as the spec says",
			spec:    Spec{MemoryRequest: held("300Mi"), MemoryLimit: held("600Mi")},
			rec:     &kipperv1.TunedResources{MemoryRequest: "128Mi", MemoryLimit: "128Mi"},
			live:    requirements("100m", "100m", "256Mi", "256Mi"),
			desired: requirements("100m", "100m", "300Mi", "600Mi"),
			want:    [4]string{"100m", "100m", "300Mi", "600Mi"},
		},
		{
			name:    "CPU and memory are independent",
			spec:    Spec{CPURequest: user("250m"), CPULimit: user("1")},
			rec:     &kipperv1.TunedResources{CPURequest: "100m", CPULimit: "100m", MemoryRequest: "512Mi", MemoryLimit: "512Mi"},
			live:    requirements("250m", "1", "256Mi", "256Mi"),
			desired: requirements("250m", "1", "128Mi", "128Mi"),
			want:    [4]string{"250m", "1", "512Mi", "512Mi"},
		},
		{
			name:    "an empty recommendation counts as none",
			spec:    Spec{},
			rec:     &kipperv1.TunedResources{},
			live:    requirements("200m", "200m", "384Mi", "384Mi"),
			desired: requirements("100m", "100m", "128Mi", "128Mi"),
			want:    [4]string{"200m", "200m", "384Mi", "384Mi"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			Apply(tc.desired, tc.spec, tc.rec, tc.live)
			assertResources(t, tc.desired, tc.want[0], tc.want[1], tc.want[2], tc.want[3])
		})
	}
}

func TestAppSpecClassifiesEachQuantity(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{ManagedFields: []metav1.ManagedFieldsEntry{
			managedEntry("kip", `{"f:spec":{"f:resources":{"f:memoryRequest":{},"f:memoryLimit":{}}}}`),
			managedEntry("console-api", `{"f:spec":{"f:resources":{"f:cpuRequest":{}}}}`),
		}},
		Spec: kipperv1.AppSpec{Resources: kipperv1.AppResources{
			CPURequest: "100m", MemoryRequest: "512Mi", MemoryLimit: "2Gi",
		}},
	}
	spec, err := AppSpec(app)
	if err != nil {
		t.Fatal(err)
	}
	if spec.MemoryRequest.Source != User || spec.MemoryLimit.Source != User {
		t.Errorf("memory sources = %v/%v, want user/user", spec.MemoryRequest.Source, spec.MemoryLimit.Source)
	}
	if spec.CPURequest.Source != Automatic || spec.CPULimit.Source != Unset {
		t.Errorf("cpu sources = %v/%v, want automatic/unset", spec.CPURequest.Source, spec.CPULimit.Source)
	}
	if spec.MemoryLimit.Value.Cmp(q("2Gi")) != 0 {
		t.Errorf("memory limit = %s, want 2Gi", spec.MemoryLimit.Value.String())
	}
}

func TestOwnedSpecAndInvalidQuantities(t *testing.T) {
	spec, err := OwnedSpec("", "", "1Gi", "")
	if err != nil {
		t.Fatal(err)
	}
	if spec.MemoryRequest.Source != User || spec.MemoryLimit.Source != Unset || spec.CPURequest.Source != Unset {
		t.Errorf("sources = %v, want only memoryRequest set by the user", spec)
	}
	if _, err := OwnedSpec("lots", "", "", ""); err == nil {
		t.Error("OwnedSpec accepted an invalid quantity")
	}
}
