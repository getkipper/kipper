package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

type recordingRestartHost struct {
	commands []string
	closed   bool
}

func (h *recordingRestartHost) Run(command string) (string, error) {
	h.commands = append(h.commands, command)
	return "deployment.apps/traefik patched", nil
}

func (h *recordingRestartHost) Close() error {
	h.closed = true
	return nil
}

func traefikTarget() restartTarget {
	return restartTarget{Namespace: "traefik", Kind: "Deployment", Name: "traefik"}
}

func clusterWithTraefik(patchErr error) *fake.Clientset {
	cs := fake.NewClientset(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "traefik", Name: "traefik"}})
	if patchErr != nil {
		cs.PrependReactor("patch", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, patchErr
		})
	}
	return cs
}

func dialing(host *recordingRestartHost) func() (restartHost, error) {
	return func() (restartHost, error) { return host, nil }
}

func TestRestartComponentStampsThePodTemplateOverTheAPI(t *testing.T) {
	cs := clusterWithTraefik(nil)
	host := &recordingRestartHost{}

	overSSH, err := restartComponent(context.Background(), cs, dialing(host), traefikTarget(), "2026-10-22T05:00:00Z")
	require.NoError(t, err)
	assert.False(t, overSSH)
	assert.Empty(t, host.commands)

	d, err := cs.AppsV1().Deployments("traefik").Get(context.Background(), "traefik", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "2026-10-22T05:00:00Z", d.Spec.Template.Annotations["kipper.run/restartedAt"])
}

func TestRestartComponentFallsBackToSSHWhenTheAPIRejectsTheLogin(t *testing.T) {
	cs := clusterWithTraefik(apierrors.NewUnauthorized("token rejected"))
	host := &recordingRestartHost{}

	overSSH, err := restartComponent(context.Background(), cs, dialing(host), traefikTarget(), "2026-10-22T05:00:00Z")
	require.NoError(t, err)
	assert.True(t, overSSH)
	require.Len(t, host.commands, 1)
	assert.Equal(t, `kubectl -n traefik patch deployment traefik --type merge -p '{"spec":{"template":{"metadata":{"annotations":{"kipper.run/restartedAt":"2026-10-22T05:00:00Z"}}}}}' --request-timeout=20s`, host.commands[0])
	assert.True(t, host.closed)
}

func TestRestartComponentFallsBackToSSHWhenTheAPINeverAnswers(t *testing.T) {
	cs := clusterWithTraefik(&url.Error{Op: "Patch", URL: "https://203.0.113.10:6443/apis/apps/v1/namespaces/traefik/deployments/traefik",
		Err: errors.New("getting credentials: exec: executable kip failed with exit code 1")})
	host := &recordingRestartHost{}

	overSSH, err := restartComponent(context.Background(), cs, dialing(host), traefikTarget(), "2026-10-22T05:00:00Z")
	require.NoError(t, err)
	assert.True(t, overSSH)
	assert.Len(t, host.commands, 1)
}

func TestRestartComponentKeepsAnAPIRefusal(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "traefik", errors.New("no"))
	cs := clusterWithTraefik(forbidden)
	host := &recordingRestartHost{}

	_, err := restartComponent(context.Background(), cs, dialing(host), traefikTarget(), "2026-10-22T05:00:00Z")
	require.Error(t, err)
	assert.True(t, apierrors.IsForbidden(err))
	assert.Empty(t, host.commands, "a refusal from the API is an answer, so SSH must not override it")
}

func TestRestartComponentNamesBothFailures(t *testing.T) {
	cs := clusterWithTraefik(apierrors.NewUnauthorized("token rejected"))
	noSSH := func() (restartHost, error) { return nil, errors.New("no SSH key") }

	_, err := restartComponent(context.Background(), cs, noSSH, traefikTarget(), "2026-10-22T05:00:00Z")
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "token rejected") && strings.Contains(err.Error(), "no SSH key"), err.Error())
}

type failingRestartHost struct{ recordingRestartHost }

func (h *failingRestartHost) Run(command string) (string, error) {
	h.commands = append(h.commands, command)
	const out = "error: deployments.apps \"traefik\" not found"
	return out, fmt.Errorf("running %q on 203.0.113.10: exit status 1: %s", command, out)
}

func TestRestartComponentReportsTheServerOutputOnce(t *testing.T) {
	cs := clusterWithTraefik(apierrors.NewUnauthorized("token rejected"))
	host := &failingRestartHost{}

	_, err := restartComponent(context.Background(), cs, func() (restartHost, error) { return host, nil }, traefikTarget(), "2026-10-22T05:00:00Z")
	require.Error(t, err)
	assert.Equal(t, 1, strings.Count(err.Error(), "not found"), err.Error())
}

func TestRestartComponentPatchesEveryKind(t *testing.T) {
	for _, target := range restartTargets {
		host := &recordingRestartHost{}
		_, err := restartComponent(context.Background(), fake.NewClientset(), func() (restartHost, error) { return host, nil }, target, "2026-10-22T05:00:00Z")
		assert.True(t, apierrors.IsNotFound(err), "%s/%s: %v", target.Kind, target.Name, err)
	}
}

func TestRestartComponentKeepsAResponseThatBrokeOff(t *testing.T) {
	cs := clusterWithTraefik(fmt.Errorf("stream error when reading response body, may be caused by closed connection. Please retry. Original error: %w", io.ErrUnexpectedEOF))
	host := &recordingRestartHost{}

	_, err := restartComponent(context.Background(), cs, dialing(host), traefikTarget(), "2026-10-22T05:00:00Z")
	require.Error(t, err)
	assert.Empty(t, host.commands, "a response arrived and may have been a refusal, so SSH must not override it")
}
