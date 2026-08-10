package ingress

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCheckHostAvailablePassesWhenFree(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck
	ctx := context.Background()

	err := CheckHostAvailable(ctx, client, "myapp.kipper.run", "myapp")
	assert.NoError(t, err)
}

func TestCheckHostAvailableFailsWhenTaken(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck
	ctx := context.Background()

	pathType := networkingv1.PathTypePrefix
	existing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-app",
			Namespace: "default",
			Labels:    map[string]string{"app": "other-app"},
		},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{
				{Host: "myapp.kipper.run", IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{
							{Path: "/", PathType: &pathType},
						},
					},
				}},
			},
		},
	}
	_, err := client.NetworkingV1().Ingresses("default").Create(ctx, existing, metav1.CreateOptions{})
	require.NoError(t, err)

	err = CheckHostAvailable(ctx, client, "myapp.kipper.run", "new-app")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already in use")
	assert.Contains(t, err.Error(), "other-app")
}

func TestCheckHostAvailableAllowsOwnIngress(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck
	ctx := context.Background()

	pathType := networkingv1.PathTypePrefix
	existing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp",
			Namespace: "default",
			Labels:    map[string]string{"app": "myapp"},
		},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{
				{Host: "myapp.kipper.run", IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{
							{Path: "/", PathType: &pathType},
						},
					},
				}},
			},
		},
	}
	_, err := client.NetworkingV1().Ingresses("default").Create(ctx, existing, metav1.CreateOptions{})
	require.NoError(t, err)

	// Same owner should pass (for updates)
	err = CheckHostAvailable(ctx, client, "myapp.kipper.run", "myapp")
	assert.NoError(t, err)
}

func TestCheckHostAvailableAllowsFunctionIngress(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck
	ctx := context.Background()

	pathType := networkingv1.PathTypePrefix
	existing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fn-myfn",
			Namespace: "keda",
		},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{
				{Host: "fn-myfn.kipper.run", IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{
							{Path: "/", PathType: &pathType},
						},
					},
				}},
			},
		},
	}
	_, err := client.NetworkingV1().Ingresses("keda").Create(ctx, existing, metav1.CreateOptions{})
	require.NoError(t, err)

	// Same function owner should pass
	err = CheckHostAvailable(ctx, client, "fn-myfn.kipper.run", "myfn")
	assert.NoError(t, err)
}
