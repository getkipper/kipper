package handlers

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// The CRD travels in the kip binary and this code travels in an image, so a
// cluster can run a console-api that writes a scope its own schema refuses.
// Silence there loses the audit record and says nothing about why.
func TestARefusedAdjustmentIsNotSilent(t *testing.T) {
	telemetryOn := &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: platformConfigName},
		Spec: kipperv1.PlatformConfigSpec{
			Telemetry: &kipperv1.TelemetrySpec{RecordResourceAdjustments: true},
		},
	}
	refuseCreate := interceptor.Funcs{
		Create: func(context.Context, crclient.WithWatch, crclient.Object, ...crclient.CreateOption) error {
			return &fieldRefused{}
		},
	}
	client := crfake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(telemetryOn).WithInterceptorFuncs(refuseCreate).Build()

	// Restore whatever the package logger was writing to: the suite shares one
	// process, and leaving it pointed elsewhere breaks tests that log.
	var out bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(prev) })

	a := &Adjustments{CRClient: client}
	a.Record(context.Background(), "job", "shop-prod", "nightly-cleanup", "memory", "128Mi", "512Mi", "", "dev@test.com")

	if !strings.Contains(out.String(), "nightly-cleanup") {
		t.Fatalf("a refused adjustment left no trace; log was %q", out.String())
	}
}

type fieldRefused struct{}

func (*fieldRefused) Error() string {
	return `ResourceAdjustment.kipper.run "radj-" is invalid: spec.scope: Unsupported value: "job"`
}
