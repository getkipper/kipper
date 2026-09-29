package resourcebounds

import (
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func TestCarriedQuantities(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{ManagedFields: []metav1.ManagedFieldsEntry{
			managedEntry("kip", `{"f:spec":{"f:resources":{"f:memoryRequest":{},"f:memoryLimit":{}}}}`),
			managedEntry("console-api", `{"f:spec":{"f:resources":{"f:cpuRequest":{}}}}`),
			managedEntry("before-first-apply", `{"f:spec":{"f:resources":{"f:cpuLimit":{}}}}`),
		}},
		Spec: kipperv1.AppSpec{Resources: kipperv1.AppResources{
			Profile: "standard", CPURequest: "100m", CPULimit: "300m", MemoryRequest: "512Mi", MemoryLimit: "2Gi",
		}},
	}
	carried, user := CarriedQuantities(app)
	want := kipperv1.AppResources{Profile: "standard", CPULimit: "300m", MemoryRequest: "512Mi", MemoryLimit: "2Gi"}
	if carried != want {
		t.Errorf("carried = %+v, want %+v; the automatic cpuRequest stays behind", carried, want)
	}
	if !reflect.DeepEqual(user, map[string]string{"memoryRequest": "512Mi", "memoryLimit": "2Gi"}) {
		t.Errorf("user = %v, want the memory pair only; the held cpuLimit is carried but not claimed", user)
	}
}
