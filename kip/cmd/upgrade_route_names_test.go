package cmd

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func ingressTo(namespace, name, service string, port int32) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{
				Path: "/", Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
					Name: service, Port: networkingv1.ServiceBackendPort{Number: port},
				}},
			}}}},
		}}},
	}
}

func TestAssessRouteNames(t *testing.T) {
	ings := []networkingv1.Ingress{
		*ingressTo("kipper-system", "console", "console-api", 8080),
		*ingressTo("kipper", "system-console-api", "system-console-api", 8080),
		*ingressTo("kipper-ai", "librechat", "librechat-librechat", 3080),
		*ingressTo("kipper-ai-librechat", "librechat", "librechat", 8080),
		*ingressTo("kipper-ai-librechat", "librechat-internal-paths", "librechat", 8080),
		*ingressTo("team", "prod-web", "prod-web", 8080),
		*ingressTo("team", "prod-web-internal-paths", "prod-web", 8080),
		*ingressTo("team-prod", "web", "web", 8080),
		*ingressTo("team-prod", "web-internal-paths", "web", 8080),
		*ingressTo("shop", "web", "web", 8080),
	}

	got := assessRouteNames(ings)

	require.Len(t, got.PlatformCollisions, 1)
	assert.Equal(t, "kipper/system-console-api", got.PlatformCollisions[0])
	assert.Equal(t, []string{"kipper-ai-librechat/librechat"}, got.Occupied)
	require.Len(t, got.Shared, 1)
	assert.Equal(t, []string{"team-prod/web", "team/prod-web"}, got.Shared[0], "each Service is listed once across its Ingresses")
	assert.Equal(t, 1, got.decisions(), "only a live platform collision needs a decision")
}

func TestCheckRouteNamesReportsAndCountsDecisions(t *testing.T) {
	clientset := fake.NewClientset(
		ingressTo("kipper-system", "console", "console-api", 8080),
		ingressTo("kipper", "system-console-api", "system-console-api", 8080),
	)
	var out bytes.Buffer

	n, err := checkRouteNames(context.Background(), clientset, nil, &out, "0.25.0")

	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Contains(t, out.String(), "kipper/system-console-api")
}

func TestCheckRouteNamesWithNothingToReport(t *testing.T) {
	clientset := fake.NewClientset(ingressTo("shop", "web", "web", 8080))
	var out bytes.Buffer

	n, err := checkRouteNames(context.Background(), clientset, nil, &out, "0.25.0")

	require.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.Contains(t, out.String(), "Route names: no collisions")
}

func TestRouteNameConsent(t *testing.T) {
	collision := []*networkingv1.Ingress{
		ingressTo("kipper-system", "console", "console-api", 8080),
		ingressTo("kipper", "system-console-api", "system-console-api", 8080),
	}
	cases := map[string]struct {
		tty     bool
		yes     bool
		answer  bool
		wantErr bool
		asked   bool
	}{
		"confirmed at a terminal":     {tty: true, answer: true, asked: true},
		"declined at a terminal":      {tty: true, answer: false, wantErr: true, asked: true},
		"no terminal to confirm with": {tty: false, wantErr: true},
		"confirmed with --yes":        {tty: false, yes: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			clientset := fake.NewClientset(collision[0], collision[1])
			var out bytes.Buffer
			asked := false
			err := routeNameConsent(context.Background(), clientset, &out, tc.tty, tc.yes, func() (bool, error) {
				asked = true
				return tc.answer, nil
			})
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tc.asked, asked)
		})
	}

	t.Run("nothing to decide", func(t *testing.T) {
		clientset := fake.NewClientset(ingressTo("shop", "web", "web", 8080))
		err := routeNameConsent(context.Background(), clientset, &bytes.Buffer{}, false, false, func() (bool, error) {
			t.Fatal("asked although nothing needs a decision")
			return false, nil
		})
		assert.NoError(t, err)
	})
}

func TestAssessRouteNamesSeesCollisionsWithinOneNamespaceAndOnNamedPorts(t *testing.T) {
	ui := func(namespace, service string) networkingv1.Ingress {
		ing := *ingressTo(namespace, service+"-ui", service, 0)
		ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Port = networkingv1.ServiceBackendPort{Name: "ui"}
		return ing
	}
	ings := []networkingv1.Ingress{
		*ingressTo("team", "prod-web", "prod-web", 8080),
		*ingressTo("team", "prod--web", "prod--web", 8080),
		ui("team", "prod-db"),
		ui("team-prod", "db"),
	}

	got := assessRouteNames(ings)

	require.Len(t, got.Shared, 2)
	assert.Equal(t, []string{"team-prod/db", "team/prod-db"}, got.Shared[0], "reports identify backend Services")
	assert.Equal(t, []string{"team/prod--web", "team/prod-web"}, got.Shared[1])
}
