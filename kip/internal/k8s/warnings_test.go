package k8s

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/client-go/rest"
)

type recordedWarnings []string

func (r *recordedWarnings) HandleWarningHeaderWithContext(_ context.Context, _ int, _ string, message string) {
	*r = append(*r, message)
}

func TestWithoutPodSecurityWarningsDropsOnlyPodSecurityVerdicts(t *testing.T) {
	var seen recordedWarnings
	cfg := WithoutPodSecurityWarnings(&rest.Config{Host: "https://cluster.example", WarningHandlerWithContext: &seen})

	cfg.WarningHandlerWithContext.HandleWarningHeaderWithContext(context.Background(), 299, "-",
		`would violate PodSecurity "restricted:latest": allowPrivilegeEscalation != false (container "db" must set securityContext.allowPrivilegeEscalation=false)`)
	cfg.WarningHandlerWithContext.HandleWarningHeaderWithContext(context.Background(), 299, "-",
		"apps/v1beta1 StatefulSet is deprecated")

	assert.Equal(t, recordedWarnings{"apps/v1beta1 StatefulSet is deprecated"}, seen)
}

func TestWithoutPodSecurityWarningsLeavesTheGivenConfigAlone(t *testing.T) {
	original := &rest.Config{Host: "https://cluster.example"}
	quiet := WithoutPodSecurityWarnings(original)

	assert.Nil(t, original.WarningHandlerWithContext)
	assert.NotNil(t, quiet.WarningHandlerWithContext)
	assert.Equal(t, original.Host, quiet.Host)
}

type legacyRecorder []string

func (r *legacyRecorder) HandleWarningHeader(_ int, _ string, message string) {
	*r = append(*r, message)
}

func TestWithoutPodSecurityWarningsKeepsALegacyHandler(t *testing.T) {
	var seen legacyRecorder
	cfg := WithoutPodSecurityWarnings(&rest.Config{Host: "https://cluster.example", WarningHandler: &seen})

	cfg.WarningHandlerWithContext.HandleWarningHeaderWithContext(context.Background(), 299, "-", "would violate PodSecurity \"restricted:latest\": runAsNonRoot != true")
	cfg.WarningHandlerWithContext.HandleWarningHeaderWithContext(context.Background(), 299, "-", "a deprecated field is set")

	assert.Equal(t, legacyRecorder{"a deprecated field is set"}, seen)
}
