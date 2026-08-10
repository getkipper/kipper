package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/kip/internal/ai"
	"github.com/getkipper/kipper/kip/internal/config"
)

func TestDefaultChatHost(t *testing.T) {
	tests := []struct {
		name    string
		domain  string
		want    string
		wantErr bool
	}{
		{"custom domain", "example.com", "chat.example.com", false},
		{"kipper.run", "myapp.kipper.run", "chat--myapp.kipper.run", false},
		{"empty domain errors", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := defaultChatHost(&config.Cluster{Domain: tt.domain})
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{8 * 1024 * 1024 * 1024, "8.0 GiB"},
		{int64(1.5 * 1024 * 1024 * 1024), "1.5 GiB"},
		{500 * 1024 * 1024, "500 MiB"},
		{0, "0 MiB"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			assert.Equal(t, tt.want, humanBytes(tt.bytes))
		})
	}
}

func TestOfferAutoConfigureWritesConfigWhenAccepted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	out := &bytes.Buffer{}

	err := offerAutoConfigure(strings.NewReader("y\n"), out, false, "qwen2.5:3b-instruct-q4_K_M", ai.TierOne)
	require.NoError(t, err)

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "ollama", cfg.AI.Provider)
	assert.Equal(t, "qwen2.5:3b-instruct-q4_K_M", cfg.AI.Model)
	// Local config gets the localhost URL so a kubectl port-forward
	// makes the local kip CLI work. The cluster Secret separately
	// gets the in-cluster Service URL.
	assert.Equal(t, "http://localhost:11434", cfg.AI.OllamaURL)
	assert.Contains(t, out.String(), "kip AI client wired to Ollama")
	assert.Contains(t, out.String(), "ollama.kipper-ai.svc.cluster.local:11434")
	assert.Contains(t, out.String(), "kubectl port-forward")
}

func TestOfferAutoConfigureDeclineLeavesConfigEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	out := &bytes.Buffer{}

	err := offerAutoConfigure(strings.NewReader("n\n"), out, false, "", ai.TierOne)
	require.NoError(t, err)

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Empty(t, cfg.AI.Provider)
}

func TestOfferAutoConfigureLeavesExistingConfigAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, config.Save(&config.Config{
		AI: config.AIConfig{Provider: "claude", APIKey: "sk-ant-existing"}, //nolint:gosec // test fixture: fake API key for unit test
	}))

	out := &bytes.Buffer{}
	// Even with assumeYes the existing provider must not be overwritten.
	err := offerAutoConfigure(strings.NewReader(""), out, true, "qwen2.5:3b-instruct-q4_K_M", ai.TierOne)
	require.NoError(t, err)

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "claude", cfg.AI.Provider)
	assert.Equal(t, "sk-ant-existing", cfg.AI.APIKey)
	assert.Contains(t, out.String(), "AI client already configured")
}

func TestOfferAutoConfigureRewiresLoopbackOllama(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, config.Save(&config.Config{
		AI: config.AIConfig{Provider: "ollama", OllamaURL: "http://localhost:11434"},
	}))

	out := &bytes.Buffer{}
	// assumeYes: rewire without prompting.
	err := offerAutoConfigure(strings.NewReader(""), out, true, "qwen2.5:7b-instruct-q4_K_M", ai.TierTwo)
	require.NoError(t, err)

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "ollama", cfg.AI.Provider)
	// Local config retains the localhost URL (port-forward target).
	// In-cluster URL goes to the cluster Secret separately.
	assert.Equal(t, "http://localhost:11434", cfg.AI.OllamaURL)
	assert.Contains(t, out.String(), "ollama.kipper-ai.svc.cluster.local:11434")
}

func TestOfferAutoConfigureRespectsExplicitNoneOptOut(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, config.Save(&config.Config{
		AI: config.AIConfig{Provider: "none"},
	}))

	out := &bytes.Buffer{}
	// assumeYes must NOT override an explicit none. The user opted out of
	// AI features deliberately.
	err := offerAutoConfigure(strings.NewReader(""), out, true, "qwen2.5:3b-instruct-q4_K_M", ai.TierOne)
	require.NoError(t, err)

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "none", cfg.AI.Provider, "explicit none must be preserved")
}

func TestOfferAutoConfigurePreservesCustomOllamaURL(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, config.Save(&config.Config{
		AI: config.AIConfig{Provider: "ollama", OllamaURL: "https://my-ollama.example.com"},
	}))

	out := &bytes.Buffer{}
	err := offerAutoConfigure(strings.NewReader(""), out, true, "qwen2.5:7b-instruct-q4_K_M", ai.TierTwo)
	require.NoError(t, err)

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "https://my-ollama.example.com", cfg.AI.OllamaURL,
		"a user-set non-loopback Ollama URL must be preserved")
}

func TestShouldRewireAIClient(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.AIConfig
		want bool
	}{
		{"unset", config.AIConfig{}, true},
		{"explicit none is an opt-out", config.AIConfig{Provider: "none"}, false},
		{"claude configured", config.AIConfig{Provider: "claude", APIKey: "k"}, false},
		{"openai configured", config.AIConfig{Provider: "openai", APIKey: "k"}, false},
		{"ollama loopback localhost", config.AIConfig{Provider: "ollama", OllamaURL: "http://localhost:11434"}, true},
		{"ollama loopback 127.0.0.1", config.AIConfig{Provider: "ollama", OllamaURL: "http://127.0.0.1:11434"}, true},
		{"ollama loopback trailing slash", config.AIConfig{Provider: "ollama", OllamaURL: "http://localhost:11434/"}, true},
		{"ollama empty url", config.AIConfig{Provider: "ollama"}, true},
		{"ollama custom remote", config.AIConfig{Provider: "ollama", OllamaURL: "https://my-ollama.example.com"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			assert.Equal(t, tt.want, shouldRewireAIClient(&cfg))
		})
	}
}

func TestOfferAutoConfigureAssumeYesSkipsPrompt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	out := &bytes.Buffer{}

	// Empty reader: would block if prompt were issued. assumeYes must
	// skip directly to the write path.
	err := offerAutoConfigure(strings.NewReader(""), out, true, "", ai.TierTwo)
	require.NoError(t, err)

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "ollama", cfg.AI.Provider)
	assert.Equal(t, ai.TierTwo.DefaultModel(), cfg.AI.Model)
}

func TestBundleDeploymentStateNotDeployed(t *testing.T) {
	clientset := fake.NewSimpleClientset() //nolint:staticcheck
	got := bundleDeploymentState(context.Background(), clientset, "ollama")
	assert.Contains(t, got, "not deployed")
}

func TestBundleDeploymentStateAvailable(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "ollama", Namespace: ai.Namespace},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas: 1,
			Replicas:      1,
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
			},
		},
	}
	clientset := fake.NewSimpleClientset(dep) //nolint:staticcheck

	got := bundleDeploymentState(context.Background(), clientset, "ollama")
	assert.Contains(t, got, "1/1 ready")
	assert.Contains(t, got, "✔")
}

func TestBundleDeploymentStateRollingOut(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "librechat", Namespace: ai.Namespace},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas: 0,
			Replicas:      1,
		},
	}
	clientset := fake.NewSimpleClientset(dep) //nolint:staticcheck

	got := bundleDeploymentState(context.Background(), clientset, "librechat")
	assert.Contains(t, got, "rolling out")
	assert.Contains(t, got, "0/1")
}

func TestParseLoadedModels(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "two models",
			body: `{"models":[{"name":"qwen2.5:3b-instruct-q4_K_M"},{"name":"nomic-embed-text"}]}`,
			want: []string{"qwen2.5:3b-instruct-q4_K_M", "nomic-embed-text"},
		},
		{
			name: "empty list",
			body: `{"models":[]}`,
			want: []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseLoadedModels([]byte(tt.body))
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseLoadedModelsRejectsGarbage(t *testing.T) {
	_, err := parseLoadedModels([]byte("not-json"))
	require.Error(t, err)
}

func TestBundleModelStateReportsUnknownWhenNotConfigured(t *testing.T) {
	clientset := fake.NewSimpleClientset() //nolint:staticcheck
	got := bundleModelState(context.Background(), clientset, "")
	assert.Contains(t, got, "not configured")
}

func TestReadBundleStateModelReturnsRecorded(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ai.BundleStateConfigMapName, Namespace: ai.Namespace},
		Data:       map[string]string{"model": "qwen2.5:7b-instruct-q4_K_M", "host": "chat.example.com"},
	}
	clientset := fake.NewSimpleClientset(cm) //nolint:staticcheck
	assert.Equal(t, "qwen2.5:7b-instruct-q4_K_M", readBundleStateModel(context.Background(), clientset))
}

func TestReadBundleStateModelReturnsEmptyWhenMissing(t *testing.T) {
	clientset := fake.NewSimpleClientset() //nolint:staticcheck
	assert.Empty(t, readBundleStateModel(context.Background(), clientset))
}

func TestBundleChatHostFromIngress(t *testing.T) {
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "librechat", Namespace: ai.Namespace},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{Host: "chat.example.com"}},
		},
	}
	clientset := fake.NewSimpleClientset(ing) //nolint:staticcheck

	host := bundleChatHost(context.Background(), clientset)
	assert.Equal(t, "chat.example.com", host)
}

func TestBundleChatHostMissingIngress(t *testing.T) {
	clientset := fake.NewSimpleClientset() //nolint:staticcheck
	assert.Empty(t, bundleChatHost(context.Background(), clientset))
}

func TestBundleAlreadyInstalledTrueWhenKipperOwnedNamespaceExists(t *testing.T) {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   ai.Namespace,
			Labels: map[string]string{"app.kubernetes.io/managed-by": "kipper"},
		},
	}
	clientset := fake.NewSimpleClientset(ns) //nolint:staticcheck
	got, err := bundleAlreadyInstalled(context.Background(), clientset)
	require.NoError(t, err)
	assert.True(t, got)
}

func TestBundleAlreadyInstalledFalseWhenAbsent(t *testing.T) {
	clientset := fake.NewSimpleClientset() //nolint:staticcheck
	got, err := bundleAlreadyInstalled(context.Background(), clientset)
	require.NoError(t, err)
	assert.False(t, got)
}

func TestBundleAlreadyInstalledFalseWhenNamespaceNotKipperOwned(t *testing.T) {
	// A user-created namespace happens to be called kipper-ai. We do
	// not adopt it; reconcile path stays off so the tier gate runs.
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ai.Namespace},
	}
	clientset := fake.NewSimpleClientset(ns) //nolint:staticcheck
	got, err := bundleAlreadyInstalled(context.Background(), clientset)
	require.NoError(t, err)
	assert.False(t, got)
}

func TestAIInstallCmdHasExpectedFlags(t *testing.T) {
	for _, name := range []string{"host", "model", "pvc-size", "yes", "skip-storage-check"} {
		assert.NotNil(t, aiInstallCmd.Flags().Lookup(name), "expected --%s flag", name)
	}
}

func TestAIBackupRepairCmdHasYesFlag(t *testing.T) {
	assert.NotNil(t, aiBackupRepairCmd.Flags().Lookup("yes"), "kip ai backup repair must accept --yes for non-interactive use")
}

func TestAIUninstallCmdDeclaresOnlyForce(t *testing.T) {
	// Uninstall is unconditionally destructive on the data side
	// (model cache, chat history, MongoDB, Secrets all wiped). The
	// only flag we accept is --force, which lets the operator bypass
	// the in-flight-backup preflight when they explicitly want to.
	var names []string
	aiUninstallCmd.Flags().VisitAll(func(f *pflag.Flag) { names = append(names, f.Name) })
	assert.Equal(t, []string{"force"}, names, "kip ai uninstall flags should be exactly [force]")
}
