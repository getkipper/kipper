package builder

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/gitreach"
	"github.com/getkipper/kipper/console-api/internal/nsowner"
	"github.com/getkipper/kipper/controller/pkg/giturl"
	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/registrycred"
	"github.com/getkipper/kipper/controller/pkg/secretname"
	"github.com/getkipper/kipper/controller/pkg/sharedcred"
)

// Default build container limits, used when an app sets no override and the
// cluster sets no BUILD_*_LIMIT. Heavy SSR/bundler builds routinely need
// several GiB, so per-app overrides and a cluster default sit above these.
const (
	defaultBuildMemoryLimit = "2Gi"
	defaultBuildCPULimit    = "2"
)

// buildLimits resolves the kaniko build container's CPU and memory limits.
// Resolution is layered, most specific first: the app's own BuildResources,
// then the cluster default (BUILD_MEMORY_LIMIT / BUILD_CPU_LIMIT), then the
// built-in default. A malformed value at any layer is skipped rather than
// wedging the build, so a typo falls through to the next source.
func buildLimits(app *kipperv1.App) (cpu, memory resource.Quantity) {
	var appMem, appCPU string
	if app != nil && app.Spec.Git != nil && app.Spec.Git.BuildResources != nil {
		appMem = app.Spec.Git.BuildResources.Memory
		appCPU = app.Spec.Git.BuildResources.CPU
	}
	memory = resolveQuantity(appMem, os.Getenv("BUILD_MEMORY_LIMIT"), defaultBuildMemoryLimit)
	cpu = resolveQuantity(appCPU, os.Getenv("BUILD_CPU_LIMIT"), defaultBuildCPULimit)
	return cpu, memory
}

// UsableBuildQuantity reports whether a configured limit is one the builder
// would actually apply. Resolution picks the first value that parses as a
// positive quantity, so a malformed or non-positive entry is skipped rather
// than used, and callers reasoning about the effective limit must apply the
// same rule instead of merely checking whether a value is present.
func UsableBuildQuantity(v string) bool {
	if v == "" {
		return false
	}
	q, err := resource.ParseQuantity(v)
	return err == nil && q.Sign() > 0
}

// ClusterBuildDefaults returns the build limits this cluster applies to apps
// that set none of their own, or empty strings when it only uses the built-in
// defaults. Migration reads these: a cluster default is deployment config, not
// part of an App, so an app that depends on one would otherwise arrive on a
// target that has never heard of it and fail its first build.
func ClusterBuildDefaults() (cpu, memory string) {
	if v := os.Getenv("BUILD_CPU_LIMIT"); v != "" {
		if q, err := resource.ParseQuantity(v); err == nil && q.Sign() > 0 {
			cpu = v
		}
	}
	if v := os.Getenv("BUILD_MEMORY_LIMIT"); v != "" {
		if q, err := resource.ParseQuantity(v); err == nil && q.Sign() > 0 {
			memory = v
		}
	}
	return cpu, memory
}

// resolveQuantity returns the first candidate that parses as a quantity,
// falling back through the list. The final fallback is a compile-time
// constant, so MustParse is safe there.
func resolveQuantity(candidates ...string) resource.Quantity {
	for _, c := range candidates[:len(candidates)-1] {
		if c == "" {
			continue
		}
		// A zero or negative limit parses fine but is not a usable resource
		// limit (Kubernetes would reject the Job, wedging the build), so treat
		// it as malformed and fall through to the next source.
		if q, err := resource.ParseQuantity(c); err == nil && q.Sign() > 0 {
			return q
		}
	}
	return resource.MustParse(candidates[len(candidates)-1])
}

// validGitBranch allows only characters that appear in real branch names and
// excludes every shell metacharacter, so a branch cannot inject into the clone
// step.
var validGitBranch = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// validateGitSource rejects branch names and clone URLs that could break out of
// the git clone step. The URL must be https, which also blocks git's dangerous
// local transports (ext::, file://) that would run commands even via argv.
// ValidateGitSource checks what can be judged without a network: the URL's
// shape and the branch's syntax.
//
// Exported so the handlers apply exactly this before storing anything. A
// branch like "release..candidate" needs no host to be known impossible, and
// letting it through because the remote check timed out stores input the
// builder already knows it will reject — the create succeeds and the app can
// never build.
func ValidateGitSource(cloneURL, branch string) error {
	return validateGitSource(cloneURL, branch)
}

func validateGitSource(cloneURL, branch string) error {
	if len(branch) > 255 || !validGitBranch.MatchString(branch) || strings.Contains(branch, "..") {
		return fmt.Errorf("invalid git branch")
	}
	u, err := url.Parse(cloneURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("git url must be a valid https:// URL")
	}
	// A credential written into the URL itself is not a credential this can
	// protect. git records the clone URL in /workspace/.git/config, and
	// /workspace is the build context, so an ordinary COPY bakes the token
	// into an image layer. It is also readable to anyone who can read the App.
	//
	// Checked here rather than only in the handlers, because this is the path
	// every build passes through: an App CR written straight to the Kubernetes
	// API by the CLI or a GitOps engine reaches this and not them.
	if u.User != nil {
		return fmt.Errorf("git url must not carry a username or password: store the token as a credential instead, or git writes it into the build context and it ends up in the image")
	}
	if u.Fragment != "" || strings.ContainsAny(cloneURL, " \t\n\r") {
		return fmt.Errorf("git url must not contain a fragment or whitespace")
	}
	return nil
}

const (
	// registryEndpoint mirrors registrycred.ClusterRegistryHost — the
	// reconcilers validate cluster-registry image ownership against the same
	// host builds push to.
	registryEndpoint = registrycred.ClusterRegistryHost
	kanikoImage      = "gcr.io/kaniko-project/executor:v1.23.2"
	gitCloneImage    = "alpine/git:2.45.2"
	// The -immutable tag cannot be re-pushed on quay, so the push step's own
	// supply chain is fixed at publish time. v1.22.2 verified 2026-07-20 as
	// the newest tag published to quay.io/skopeo/stable.
	skopeoImage    = "quay.io/skopeo/stable:v1.22.2-immutable"
	buildLabel     = labels.Build
	appLabel       = labels.AppRef
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "kipper"
	// projectLabel names the project a tenant namespace belongs to. It is
	// controller-owned, so a shared credential's project allow-list is checked
	// against an identity a tenant cannot forge.
	projectLabel = "kipper.run/project"
	// Builds run in a shared, installer-owned namespace (buildsNamespace), so
	// a build's objects carry the source (tenant) namespace and the App UID:
	// the source-namespace label scopes lifecycle lookups to the right app,
	// and the UID distinguishes a deleted-and-recreated App of the same name.
	sourceNamespaceLabel = labels.SourceNamespace
	appUIDLabel          = "kipper.run/app-uid"

	// buildsNamespace is the installer-owned namespace all builds run in, away
	// from any tenant-readable surface (see kip/internal/installer/builds.go).
	buildsNamespace = labels.BuildsNamespace
	// buildsServiceAccount is the zero-permission identity build pods run as.
	buildsServiceAccount = "kipper-builder"

	// The cluster registry's accounts and the kipper-system objects kip
	// installs them into (names must match kip/internal/installer/zot.go).
	// The CA comes from the ca.crt of the issued TLS secret — never from the
	// CA Secret itself, whose private key must stay in kipper-system.
	zotSystemNamespace = "kipper-system"
	zotPushSecretName  = "zot-push-credentials" //nolint:gosec // Secret object name, not a credential
	zotTLSSecretName   = "zot-tls"
	zotPushUser        = "kipper-push"
	// imageTarDir is the emptyDir handoff between the Kaniko build and the
	// push container.
	imageTarDir = "/image-out"
	// skopeoAuthDir is where the push container reads its authfile from.
	skopeoAuthDir = "/skopeo-auth"
)

// ImageRef returns the full registry image reference for a built app.
func ImageRef(namespace, appName, commitSHA string) string {
	return fmt.Sprintf("%s/%s/%s:%s", registryEndpoint, namespace, appName, commitSHA)
}

// gitCredentialUsername is what a token is sent as during the preflight. Git
// hosts ignore the username on a personal access token and reject a request
// that sends none.
const gitCredentialUsername = "kipper"

// ReachGit is the clone preflight. A variable, and exported, so that tests in
// this package and in the packages that create builds can drive every answer
// without any of them reaching the network.
var ReachGit = gitreach.Check

// CreateBuildJob creates a Kubernetes Job that clones the Git repo and builds
// a container image using Kaniko, then pushes it to the internal Zot registry.
func CreateBuildJob(ctx context.Context, client kubernetes.Interface, owners nsowner.Reader, app *kipperv1.App, commitSHA string) (*batchv1.Job, error) {
	git := app.Spec.Git
	if git == nil {
		return nil, fmt.Errorf("app %s/%s has no git source configured", app.Namespace, app.Name)
	}

	buildCPU, buildMem := buildLimits(app)

	branch := git.Branch
	if branch == "" {
		branch = "main"
	}
	dockerfile := git.DockerfilePath
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	buildContext := git.Context
	if buildContext == "" {
		buildContext = "."
	}

	// A per-build random id makes every build attempt's objects uniquely named
	// in the shared build namespace, so a rebuild never reuses (and races the
	// deletion/GC of) the previous attempt's Job or secrets, and two tenants
	// can never collide on a name.
	buildID, err := newBuildID()
	if err != nil {
		return nil, err
	}
	jobName := buildJobName(app.Namespace, app.Name, buildID)
	// Every object this build creates in the shared build namespace carries
	// these labels: they scope lifecycle lookups to this app's builds and drive
	// the ephemeral-secret cleanup.
	buildLabels := map[string]string{
		managedByLabel:       "kipper",
		appLabel:             app.Name,
		buildLabel:           "true",
		sourceNamespaceLabel: app.Namespace,
		appUIDLabel:          string(app.UID),
	}

	imageRef := ImageRef(app.Namespace, app.Name, commitSHA)
	latestRef := fmt.Sprintf("%s/%s/%s:latest", registryEndpoint, app.Namespace, app.Name)

	cloneURL := git.URL
	if err := validateGitSource(cloneURL, branch); err != nil {
		return nil, err
	}

	// Kaniko args. The build never pushes: user RUN instructions execute
	// inside the Kaniko container, so nothing with access to the cluster
	// registry may exist there. The image lands as a tarball in a shared
	// emptyDir and a separate push container ships it. Kaniko carries no
	// cluster-registry credential or CA, so a Dockerfile cannot pull a base
	// image FROM the cluster registry and cannot read another tenant's images;
	// base images come from public registries.
	kanikoArgs := []string{
		fmt.Sprintf("--dockerfile=%s", dockerfile),
		fmt.Sprintf("--context=dir:///workspace/%s", buildContext),
		fmt.Sprintf("--destination=%s", imageRef),
		"--no-push",
		fmt.Sprintf("--tar-path=%s/image.tar", imageTarDir),
		"--cache=false",
	}

	// Add build args
	for key, val := range git.BuildArgs {
		kanikoArgs = append(kanikoArgs, fmt.Sprintf("--build-arg=%s=%s", key, val))
	}

	backoffLimit := int32(0)
	ttl := int32(3600)

	// A base image FROM the cluster registry cannot build any more (Kaniko has
	// no cluster-registry credential or CA), and would otherwise fail with an
	// opaque Kaniko TLS/auth error. This check runs after the clone and before
	// Kaniko, and fails the build early with a legible reason in its logs. The
	// dockerfile path is passed as $1, never interpolated into the shell, so a
	// crafted path cannot inject; the grep pattern is derived from the fixed
	// registry endpoint.
	dockerfilePath := path.Join("/workspace", buildContext, dockerfile)
	registryHostPattern := strings.ReplaceAll(registryEndpoint, ".", `\.`)
	// The build fails if a base image comes directly FROM the cluster registry:
	// Kaniko has no cluster-registry credential or CA and would otherwise fail
	// with an opaque TLS error, so this fails early with a legible message. The
	// sed prelude joins backslash line-continuations first, so a FROM split
	// across lines is not missed, and the optional quote covers a quoted image
	// reference. A base reached indirectly (an ARG default, a --build-arg) is not
	// diagnosed here; correlating an ARG value to the FROM that uses it is not
	// worth the complexity for a best-effort diagnostic, and such a build still
	// fails closed at Kaniko for lack of the cluster-registry credential and CA.
	// If the check itself errors, the build still fails closed.
	fromCheckScript := fmt.Sprintf(
		`if [ -f "$1" ] && sed -e ':a' -e '/\\$/N; s/\\\n//; ta' "$1" | grep -qiE '^[[:space:]]*FROM[[:space:]]+(--[^[:space:]]+[[:space:]]+)*["'"'"']?%s/'; then echo 'kipper: base images FROM the internal cluster registry are unsupported; publish shared base images to a public registry' >&2; exit 1; fi`,
		registryHostPattern,
	)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: buildsNamespace,
			Labels:    buildLabels,
			Annotations: map[string]string{
				"kipper.run/commit": commitSHA,
				// The source this artefact is built from, so a completion can
				// be checked against the source the app declares by then. The
				// app UID covers a delete and recreate, and job ordering covers
				// a newer build, but neither covers a source that was edited
				// without launching one.
				SourceFingerprintAnnotation: GitSourceFingerprint(app.Spec.Git),
			},
			// No App ownerRef: the App lives in the tenant namespace and
			// cross-namespace ownerRefs are ignored by garbage collection.
			// Cleanup is the label-scoped CancelBuilds sweep plus the Job TTL.
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: buildLabels,
				},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					ServiceAccountName:           buildsServiceAccount,
					AutomountServiceAccountToken: boolPtr(false),
					SecurityContext: &corev1.PodSecurityContext{
						// Kaniko runs as root and writes the image tar; a shared
						// fsGroup makes the tar group-readable so the non-root
						// push container can read it off the shared emptyDir.
						FSGroup:        int64Ptr(1000),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					InitContainers: []corev1.Container{
						{
							Name:            "clone",
							Image:           gitCloneImage,
							SecurityContext: noPrivEscSecurityContext(),
							// argv form, no shell: branch and URL are literal
							// arguments, so neither can inject a command.
							Command: []string{"git", "clone", "--branch", branch, "--single-branch", "--depth", "1", cloneURL, "/workspace"},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "workspace", MountPath: "/workspace"},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("50m"),
									corev1.ResourceMemory: resource.MustParse("64Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
							},
						},
						{
							Name:            "check-base",
							Image:           gitCloneImage,
							SecurityContext: noPrivEscSecurityContext(),
							// $1 is the dockerfile path (never interpolated into
							// the script), so a crafted path cannot inject.
							Command:                  []string{"sh", "-c", fromCheckScript, "sh", dockerfilePath},
							TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
							VolumeMounts: []corev1.VolumeMount{
								{Name: "workspace", MountPath: "/workspace"},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("10m"),
									corev1.ResourceMemory: resource.MustParse("16Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("64Mi"),
								},
							},
						},
						{
							Name:            "build",
							Image:           kanikoImage,
							Args:            kanikoArgs,
							SecurityContext: noPrivEscSecurityContext(),
							VolumeMounts: []corev1.VolumeMount{
								{Name: "workspace", MountPath: "/workspace"},
								{Name: "image-out", MountPath: imageTarDir},
							},
							// Kaniko needs real headroom: image builds routinely
							// use over 1Gi, and a tight limit shows up as an
							// opaque OOMKilled build. The default 2Gi covers most
							// builds; SSR/bundler builds are the heavy case (a
							// Nuxt/Next production build can set a 4Gi Node heap
							// with kaniko layer processing on top), so the ceiling
							// is raised per cluster with BUILD_MEMORY_LIMIT rather
							// than baking a large default that only heavy builds
							// need. The low request keeps the Job schedulable on
							// the smallest supported boxes.
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("200m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    buildCPU,
									corev1.ResourceMemory: buildMem,
								},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:            "push",
							Image:           skopeoImage,
							SecurityContext: noPrivEscSecurityContext(),
							// The push runs no user code, which is the point of
							// the split: the write credential exists only here.
							// Refs are env data expanded inside double quotes,
							// so a hostile tag cannot become shell syntax. The
							// tarball is copied once per tag; the second copy
							// finds every blob already present and only writes
							// the manifest.
							Command: []string{"sh", "-c",
								`skopeo copy --authfile ` + skopeoAuthDir + `/config.json --dest-cert-dir /zot-ca docker-archive:` + imageTarDir + `/image.tar "docker://$IMAGE_REF" && ` +
									`skopeo copy --authfile ` + skopeoAuthDir + `/config.json --dest-cert-dir /zot-ca docker-archive:` + imageTarDir + `/image.tar "docker://$LATEST_REF"`},
							Env: []corev1.EnvVar{
								{Name: "IMAGE_REF", Value: imageRef},
								{Name: "LATEST_REF", Value: latestRef},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "image-out", MountPath: imageTarDir},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("50m"),
									corev1.ResourceMemory: resource.MustParse("64Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "workspace",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
						{
							Name: "image-out",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
					},
				},
			},
		},
	}

	// The build runs in the shared build namespace, so every secret it needs
	// is resolved out of the tenant namespace and staged as an ephemeral,
	// build-scoped Secret there. Each carries the build labels and is owned by
	// the Job after creation, so it is garbage-collected with the Job (TTL or
	// cancel). Track them to clean up if Job creation fails.
	var ephemeral []string
	cleanup := func() {
		for _, name := range ephemeral {
			_ = client.CoreV1().Secrets(buildsNamespace).Delete(ctx, name, metav1.DeleteOptions{})
		}
	}

	// The credential the clone will use, when there is one, so the preflight
	// below asks with exactly what the build has.
	var buildToken string

	// Mount git credentials if configured. The token is resolved centrally
	// (a shared credential from kipper-system, allow-listed to this project and
	// bound to the clone host; or a per-app credential from the tenant
	// namespace) and staged in the build namespace; it flows through a
	// host-scoped credential helper, never into argv, the clone URL, disk, or an
	// image layer.
	if git.CredentialsSecret != "" {
		// The clone URL's canonical authority is the only host the token is
		// offered to, both as the credential-helper config scope and as the host
		// the helper body checks the request against. Cloning from the canonical
		// URL makes git derive exactly this authority for its credential request,
		// so a valid host with mixed case or an explicit :443 is not falsely
		// denied.
		authority, canonicalURL, err := giturl.Canonical(cloneURL)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("git url is not host-bindable: %w", err)
		}
		cloneURL = canonicalURL
		token, err := resolveGitToken(ctx, client, owners, app, authority)
		if err != nil {
			cleanup()
			return nil, err
		}
		buildToken = string(token)
		gitSecret := jobName + "-git"
		if err := writeBuildSecret(ctx, client, gitSecret, buildLabels, map[string][]byte{"token": token}); err != nil {
			cleanup()
			return nil, err
		}
		ephemeral = append(ephemeral, gitSecret)
		job.Spec.Template.Spec.InitContainers[0].Env = []corev1.EnvVar{
			{
				Name: "GIT_TOKEN",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: gitSecret},
						Key:                  "token",
					},
				},
			},
			// The bound host, checked by the helper body against git's request so
			// the token is offered only to this host even if the config-scope
			// match ever misfires.
			{Name: "GIT_EXPECTED_HOST", Value: authority},
		}
		job.Spec.Template.Spec.InitContainers[0].Command = cloneCommand(authority, branch, cloneURL)
	}

	// Snapshot the app's build secrets (ARG/ENV values the Dockerfile uses)
	// into the build namespace so Kaniko still receives them after the move.
	envSecret, err := snapshotBuildSecrets(ctx, client, app, jobName, buildLabels)
	if err != nil {
		cleanup()
		return nil, err
	}
	if envSecret != "" {
		ephemeral = append(ephemeral, envSecret)
		optional := true
		job.Spec.Template.Spec.InitContainers[2].EnvFrom = []corev1.EnvFromSource{
			{
				SecretRef: &corev1.SecretEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: envSecret},
					Optional:             &optional,
				},
			},
		}
	}

	// Registry auth: the push (write) credential is mounted only on the
	// no-user-code push container; Kaniko carries no registry credential at
	// all and pulls base images anonymously.
	registrySecret, err := mountRegistryAuth(ctx, client, jobName, buildLabels, job)
	if err != nil {
		cleanup()
		return nil, err
	}
	ephemeral = append(ephemeral, registrySecret)

	// Prove the clone before launching a pod to attempt it. This is the one
	// place every build passes through — the console, a webhook, a migration,
	// and an App CR written straight to the Kubernetes API by the CLI or a
	// GitOps engine — so it is the only place that can answer for all of them.
	// It runs in console-api rather than in the build pod, so it does not prove
	// the build namespace's own egress. What it does prove is the repository's
	// answer to a real reference advertisement with the credential the build
	// will use, which is what the failures this exists for turn on.
	//
	// Outside the credential branch on purpose: an app with no credential at
	// all against a private repository is the original failure, and checking
	// only credentialled builds would skip exactly it.
	//
	// Only a definite refusal stops the build. A host this cluster cannot reach
	// has said nothing about the repository, and the clone is worth attempting.
	if result, detail := ReachGit(ctx, git.URL, branch, gitCredentialUsername, buildToken); result == gitreach.NeedsCredential || result == gitreach.Unsafe {
		cleanup()
		return nil, fmt.Errorf("%s cannot be cloned: %s", git.URL, detail)
	}

	created, err := client.BatchV1().Jobs(buildsNamespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("creating build job: %w", err)
	}

	// Own the ephemeral secrets by the Job so they GC with it. Best effort:
	// the label-scoped CancelBuilds sweep is the cleanup backstop.
	ownEphemeralSecrets(ctx, client, created, ephemeral)

	return created, nil
}

// GitAuthorityAnnotation records the clone host a per-app credential was stored
// for, so a token and a URL that stopped agreeing can be caught before the
// token travels.
const GitAuthorityAnnotation = labels.AnnoGitAuthority

// CredentialBoundElsewhere reports the host a per-app credential was stored for
// when that is not the host about to be contacted, and whether there is such a
// disagreement at all.
//
// The token and the URL are written as a pair but not in one transaction, so
// they can stop agreeing without anyone asking. Every path that sends the token
// asks this, not just the build: a health probe that authenticates against the
// app's current URL discloses it just as thoroughly. A credential stored before
// the binding was recorded carries none and is trusted, which is what keeps
// existing clusters working.
func CredentialBoundElsewhere(annotations map[string]string, authority string) (string, bool) {
	bound := annotations[GitAuthorityAnnotation]
	if bound == "" || bound == authority {
		return "", false
	}
	return bound, true
}

// resolveGitToken returns the token the build's credential helper will present.
// A credential name that matches a shared credential in kipper-system is a
// shared credential: it is usable only if the app's project is on the
// credential's allow-list and the credential's bound host equals the clone
// host. Any other name is a per-app credential read from the tenant namespace,
// where the tenant owns both the token and the URL. It is still checked against
// the host recorded when it was stored, because the two are written as a pair
// and not in one transaction. authority is the clone URL's canonical authority.
func resolveGitToken(ctx context.Context, client kubernetes.Interface, owners nsowner.Reader, app *kipperv1.App, authority string) ([]byte, error) {
	name := app.Spec.Git.CredentialsSecret
	shared, err := sharedcred.Load(ctx, client)
	if err != nil {
		// Fail closed: an unreadable shared list must not silently downgrade a
		// shared credential to the unrestricted per-app path.
		return nil, fmt.Errorf("verifying shared git credentials: %w", err)
	}
	if entry := sharedcred.Find(shared, name); entry != nil {
		project, err := namespaceProject(ctx, owners, app.Namespace)
		if err != nil {
			return nil, err
		}
		if !entry.AllowsProject(project) {
			return nil, fmt.Errorf("git credential %q is not allowed for project %q. Allow it with 'kip credentials allow %s --project %s'", name, project, name, project)
		}
		credAuthority, err := giturl.CanonicalAuthority(entry.Server)
		if err != nil {
			return nil, fmt.Errorf("git credential %q has an invalid server: %w", name, err)
		}
		if credAuthority != authority {
			return nil, fmt.Errorf("git credential %q is bound to %s but the app clones from %s", name, credAuthority, authority)
		}
		if entry.Token == "" {
			return nil, fmt.Errorf("git credential %q has no token", name)
		}
		return []byte(entry.Token), nil
	}
	// Per-app credential: only the app's own credential Secret is accepted, so a
	// tenant cannot point CredentialsSecret at another Secret in its namespace to
	// bypass the shared-credential allow-list and host binding.
	if !secretname.IsGitCredentialOf(app.Name, name) {
		return nil, fmt.Errorf("git credential %q is neither an allowed shared credential nor this app's own credential", name)
	}
	secret, err := client.CoreV1().Secrets(app.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading git credential %s: %w", name, err)
	}
	// The token and the URL are written as a pair but not in one transaction,
	// and two overlapping source changes whose CR updates both fail can leave
	// one host's token beside another host's URL. The binding recorded when the
	// token was stored is what proves the pair, so a disagreement fails the
	// build rather than sending the token to the wrong host. A credential
	// stored before the binding was recorded carries none and is trusted.
	if bound, elsewhere := CredentialBoundElsewhere(secret.Annotations, authority); elsewhere {
		return nil, fmt.Errorf("git credential %s was stored for %s but the app clones from %s, so it was not sent: set the git source again", name, bound, authority)
	}
	token := secret.Data["token"]
	if len(token) == 0 {
		return nil, fmt.Errorf("git credential %s has no token", name)
	}
	return token, nil
}

// namespaceProject returns the project a namespace belongs to.
//
// It fails closed when nothing owns the namespace, because the answer decides
// whether a build is handed a shared git credential. It resolves through the
// shared owner lookup rather than reading the label here, because the label is
// writable by anyone who can write a namespace and an allow-list checked
// against a forged one is no allow-list. What the lookup requires, and the
// release it starts requiring the claim, is stated once in nsowner.Of.
func namespaceProject(ctx context.Context, reader nsowner.Reader, namespace string) (string, error) {
	project, ok, err := nsowner.Of(ctx, reader, namespace)
	if err != nil {
		return "", fmt.Errorf("resolving project for namespace %s: %w", namespace, err)
	}
	if !ok {
		return "", fmt.Errorf("namespace %s is not a project's", namespace)
	}
	return project, nil
}

// cloneCommand builds the git clone argv with a credential helper scoped to
// exactly one host. git records the remote URL verbatim in /workspace/.git/config
// and /workspace is the Kaniko build context, so a URL-embedded token would be
// readable by any Dockerfile (`RUN cat .git/config`) and baked into a published
// image layer by a plain `COPY . .`. The helper keeps the token off disk and
// out of argv, and binds it to authority in two ways:
//
//   - The config key credential.https://<authority>.helper means git only asks
//     this helper for that host; a clone that redirects elsewhere gets no token.
//   - The helper body answers only the `get` operation, parses git's request,
//     and emits the token only when the request's protocol is https and its host
//     equals $GIT_EXPECTED_HOST — belt-and-braces if the scope match misfires.
//
// The empty credential.helper= (and the empty scoped reset) first clear any
// helper inherited from the image's git config: credential.helper is
// multi-valued and git calls every helper's `store` after a successful auth, so
// an inherited persisting helper could write the token to disk. The resets make
// the no-disk invariant a property of this Job, not of the image.
//
// The token and the bound host reach the helper only as the $GIT_TOKEN and
// $GIT_EXPECTED_HOST env values its shell expands at run time; neither the token
// nor a shell-interpolated authority appears in argv. authority is a validated
// canonical authority (giturl.CanonicalAuthority), so the config key is a safe
// host[:port] token.
//
// `x-access-token` as the username works for every provider Kipper supports:
// GitHub fine-grained PATs (`github_pat_*`) require a username component, and
// GitHub classic (`ghp_*`) and GitLab (`glpat-*`) PATs accept any non-empty one.
func cloneCommand(authority, branch, cloneURL string) []string {
	scoped := "credential.https://" + authority + ".helper"
	const helper = `!f() { [ "$1" = get ] || exit 0; h=; p=; while IFS='=' read -r k v; do case "$k" in host) h=$v;; protocol) p=$v;; esac; done; [ "$p" = https ] && [ "$h" = "$GIT_EXPECTED_HOST" ] && printf 'username=x-access-token\npassword=%s\n' "$GIT_TOKEN"; }; f`
	return []string{
		"git",
		"-c", "credential.helper=",
		"-c", scoped + "=",
		"-c", scoped + "=" + helper,
		"clone", "--branch", branch, "--single-branch", "--depth", "1", cloneURL, "/workspace",
	}
}

// writeBuildSecret creates an ephemeral build-scoped Secret in the build
// namespace. The name carries a per-build random id, so a name that already
// exists is not a rebuild — it is a hash collision or a foreign object, and
// overwriting it could hand this build another owner's data or hand another
// build this build's credentials. So it refuses to overwrite: fail closed.
func writeBuildSecret(ctx context.Context, client kubernetes.Interface, name string, labels map[string]string, data map[string][]byte) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: buildsNamespace, Labels: labels},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
	if _, err := client.CoreV1().Secrets(buildsNamespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("creating build secret %s: %w", name, err)
	}
	return nil
}

// snapshotBuildSecrets copies the app's <app>-build-secrets Secret from the
// tenant namespace into a build-scoped Secret in the build namespace. It
// returns "" when the app has no build secrets (an optional feature).
func snapshotBuildSecrets(ctx context.Context, client kubernetes.Interface, app *kipperv1.App, jobName string, labels map[string]string) (string, error) {
	src, err := client.CoreV1().Secrets(app.Namespace).Get(ctx, app.Name+"-build-secrets", metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading build secrets: %w", err)
	}
	// Not a workload's env Secret, so not secretname.Env: this is an ephemeral
	// snapshot of the app's build secrets, named after the build Job that owns
	// it and garbage-collected with it. Build Job names are generated and do not
	// collide with a Job CR's name.
	name := jobName + "-env"
	if err := writeBuildSecret(ctx, client, name, labels, src.Data); err != nil {
		return "", err
	}
	return name, nil
}

// ownEphemeralSecrets sets the build Job as the owner of its ephemeral secrets
// so they are garbage-collected with it. Best effort: a patch failure leaves
// the label-scoped CancelBuilds sweep as the cleanup path.
func ownEphemeralSecrets(ctx context.Context, client kubernetes.Interface, job *batchv1.Job, names []string) {
	ref := metav1.OwnerReference{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Name:       job.Name,
		UID:        job.UID,
	}
	for _, name := range names {
		secret, err := client.CoreV1().Secrets(buildsNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			continue
		}
		secret.OwnerReferences = []metav1.OwnerReference{ref}
		_, _ = client.CoreV1().Secrets(buildsNamespace).Update(ctx, secret, metav1.UpdateOptions{})
	}
}

// dockerConfig is the subset of a docker config.json Kaniko needs: a map of
// registry host to its auth entry.
type dockerConfig struct {
	Auths map[string]json.RawMessage `json:"auths"`
}

// registryAuthEntry renders one docker-config auth entry for a
// username/password pair.
func registryAuthEntry(user, password string) (json.RawMessage, error) {
	entry, err := json.Marshal(map[string]string{
		"auth": base64.StdEncoding.EncodeToString([]byte(user + ":" + password)),
	})
	if err != nil {
		return nil, fmt.Errorf("marshalling registry auth: %w", err)
	}
	return entry, nil
}

// mountRegistryAuth stages the cluster-registry PUSH credential and CA as an
// ephemeral build-scoped Secret and mounts them only on the push container.
// Kaniko runs the Dockerfile's RUN instructions, so it gets no registry
// credential at all: it pulls public base images anonymously, and a third-party
// pull credential is never placed where user code could read it (`RUN cat
// /kaniko/.docker/config.json`) or bake it into a layer. Pulling a private base
// image is no longer supported at build time; publish shared bases to a public
// registry. The push container runs no user code and is the only place the
// cluster-registry write credential and CA exist. Returns the ephemeral
// Secret's name.
func mountRegistryAuth(ctx context.Context, client kubernetes.Interface, jobName string, labels map[string]string, job *batchv1.Job) (string, error) {
	pushPassword, err := readZotPassword(ctx, client, zotPushSecretName)
	if err != nil {
		return "", err
	}
	tlsSecret, err := client.CoreV1().Secrets(zotSystemNamespace).Get(ctx, zotTLSSecretName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading registry CA: %w", err)
	}
	caPEM := tlsSecret.Data["ca.crt"]
	if len(caPEM) == 0 {
		return "", fmt.Errorf("registry TLS secret has no ca.crt")
	}

	pushEntry, err := registryAuthEntry(zotPushUser, pushPassword)
	if err != nil {
		return "", err
	}
	pushJSON, err := json.Marshal(dockerConfig{Auths: map[string]json.RawMessage{registryEndpoint: pushEntry}})
	if err != nil {
		return "", fmt.Errorf("marshalling push docker config: %w", err)
	}

	secretName := jobName + "-registry"
	if err := writeBuildSecret(ctx, client, secretName, labels, map[string][]byte{
		"push-config.json": pushJSON,
		"ca.crt":           caPEM,
	}); err != nil {
		return "", err
	}

	job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes,
		corev1.Volume{
			Name: "push-auth",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: secretName,
					Items:      []corev1.KeyToPath{{Key: "push-config.json", Path: "config.json"}},
				},
			},
		},
		corev1.Volume{
			Name: "zot-ca",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: secretName,
					Items:      []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
				},
			},
		})

	// The push container is the only place the write credential and CA exist.
	push := &job.Spec.Template.Spec.Containers[0]
	push.VolumeMounts = append(push.VolumeMounts,
		corev1.VolumeMount{Name: "push-auth", MountPath: skopeoAuthDir},
		corev1.VolumeMount{Name: "zot-ca", MountPath: "/zot-ca"})
	return secretName, nil
}

// readZotPassword reads one of the cluster registry account passwords from
// kipper-system.
func readZotPassword(ctx context.Context, client kubernetes.Interface, secretName string) (string, error) {
	secret, err := client.CoreV1().Secrets(zotSystemNamespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading registry credential %s: %w", secretName, err)
	}
	password := string(secret.Data["password"])
	if password == "" {
		return "", fmt.Errorf("registry credential %s has no password", secretName)
	}
	return password, nil
}

// buildJobName returns a Job name that fits Kubernetes' DNS-1123 label
// limit (63 chars) with room for the 6-char pod suffix Kubernetes appends.
// When the full commit does not fit, the name carries a digest of the
// app/commit pair instead of a right-truncated commit: distinct builds must
// never share a Job name, or replacing an old Job races its own deletion.
// The digest also keeps the name total in bounds for the longest accepted
// app names, where the old truncation sliced with a negative index.
func buildJobName(namespace, appName, buildID string) string {
	const maxLen = 56
	// The build namespace is shared across tenants, so the name must be unique
	// across tenants AND across rebuilds: a collision would let one tenant's
	// build overwrite another's ephemeral registry secret. The digest is
	// ALWAYS applied (not only when the readable form overflows), over the
	// source namespace, app, and a per-build random id, so short names get a
	// disambiguating suffix too and no two builds ever share a name.
	sum := sha256.Sum256([]byte(namespace + "/" + appName + "/" + buildID))
	// 128 bits of digest keeps accidental collisions negligible even across a
	// huge number of builds in the shared namespace.
	digest := hex.EncodeToString(sum[:])[:32]
	prefix := appName
	if budget := maxLen - len("-build-") - len(digest); len(prefix) > budget {
		prefix = prefix[:budget]
	}
	return prefix + "-build-" + digest
}

// newBuildID returns a 128-bit random id for one build attempt, so each
// attempt's Job and ephemeral secrets are uniquely named in the shared build
// namespace.
func newBuildID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating build id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// buildSelector scopes a lifecycle lookup in the shared build namespace to one
// app's builds, identified by the source (tenant) namespace and app name.
func buildSelector(sourceNamespace, appName string) string {
	return fmt.Sprintf("%s=%s,%s=%s,%s=true", sourceNamespaceLabel, sourceNamespace, appLabel, appName, buildLabel)
}

// GetBuildPod finds the pod running the latest build for an app. namespace is
// the app's (tenant) namespace; the build runs in buildsNamespace and is
// matched by its source-namespace label.
func GetBuildPod(ctx context.Context, client kubernetes.Interface, namespace, appName string) (*corev1.Pod, error) {
	pods, err := client.CoreV1().Pods(buildsNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: buildSelector(namespace, appName),
	})
	if err != nil {
		return nil, fmt.Errorf("listing build pods: %w", err)
	}

	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("no build pod found for %s", appName)
	}

	// Return the most recent pod
	latest := &pods.Items[0]
	for i := range pods.Items {
		if pods.Items[i].CreationTimestamp.After(latest.CreationTimestamp.Time) {
			latest = &pods.Items[i]
		}
	}

	return latest, nil
}

// CancelBuild deletes the active build Job for an app. Its ephemeral secrets
// are owned by the Job, so foreground deletion garbage-collects them.
func CancelBuild(ctx context.Context, client kubernetes.Interface, namespace, appName string) error {
	propagation := metav1.DeletePropagationForeground
	jobs, err := client.BatchV1().Jobs(buildsNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: buildSelector(namespace, appName),
	})
	if err != nil {
		return fmt.Errorf("listing build jobs: %w", err)
	}

	for i := range jobs.Items {
		if jobs.Items[i].Status.Active > 0 {
			if err := client.BatchV1().Jobs(buildsNamespace).Delete(ctx, jobs.Items[i].Name, metav1.DeleteOptions{
				PropagationPolicy: &propagation,
			}); err != nil {
				return fmt.Errorf("deleting build job %s: %w", jobs.Items[i].Name, err)
			}
		}
	}

	return nil
}

// CancelBuilds deletes every build Job for an app, whether active, pending, or
// already finished. Unlike CancelBuild (which only stops a running build), this
// guarantees no build Job survives to reconcile later and write its image onto
// the App — the precedence rollback needs so a completing build can't undo it.
func CancelBuilds(ctx context.Context, client kubernetes.Interface, namespace, appName string) error {
	propagation := metav1.DeletePropagationForeground
	selector := buildSelector(namespace, appName)
	jobs, err := client.BatchV1().Jobs(buildsNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("listing build jobs: %w", err)
	}
	for i := range jobs.Items {
		if err := client.BatchV1().Jobs(buildsNamespace).Delete(ctx, jobs.Items[i].Name, metav1.DeleteOptions{
			PropagationPolicy: &propagation,
		}); err != nil && !k8serrors.IsNotFound(err) {
			return fmt.Errorf("deleting build job %s: %w", jobs.Items[i].Name, err)
		}
	}

	// Foreground deletion above GCs the ephemeral secrets each Job owns, but a
	// secret whose ownerRef patch never landed (console-api died mid-build) has
	// no owner, so delete this app's build secrets by label too. This must not
	// wait for the janitor's hours-long window, since they hold live credentials.
	secrets, err := client.CoreV1().Secrets(buildsNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("listing build secrets: %w", err)
	}
	for i := range secrets.Items {
		if err := client.CoreV1().Secrets(buildsNamespace).Delete(ctx, secrets.Items[i].Name, metav1.DeleteOptions{}); err != nil && !k8serrors.IsNotFound(err) {
			return fmt.Errorf("deleting build secret %s: %w", secrets.Items[i].Name, err)
		}
	}

	return nil
}

// GetBuildStatus returns the current build status from the latest build Job.
func GetBuildStatus(ctx context.Context, client kubernetes.Interface, namespace, appName string) (*kipperv1.AppBuildStatus, error) {
	jobs, err := client.BatchV1().Jobs(buildsNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: buildSelector(namespace, appName),
	})
	if err != nil {
		return nil, fmt.Errorf("listing build jobs: %w", err)
	}

	if len(jobs.Items) == 0 {
		return nil, nil
	}

	// Find the most recent job. Equal timestamps break on name, the same way
	// supersession does, so two jobs created in the same second cannot make the
	// older one the latest for one reader and not the other.
	latest := &jobs.Items[0]
	for i := range jobs.Items {
		switch {
		case jobs.Items[i].CreationTimestamp.After(latest.CreationTimestamp.Time):
			latest = &jobs.Items[i]
		case jobs.Items[i].CreationTimestamp.Equal(&latest.CreationTimestamp) && jobs.Items[i].Name > latest.Name:
			latest = &jobs.Items[i]
		}
	}

	status := &kipperv1.AppBuildStatus{
		StartedAt: &latest.CreationTimestamp,
		Build:     latest.Name,
	}

	switch {
	case latest.Status.Succeeded > 0:
		status.Phase = "Succeeded"
		if len(latest.Status.Conditions) > 0 {
			t := latest.Status.Conditions[0].LastTransitionTime
			status.CompletedAt = &t
		}
	case latest.Status.Failed > 0:
		status.Phase = "Failed"
		status.Message = "build failed"
		if len(latest.Status.Conditions) > 0 {
			status.Message = latest.Status.Conditions[0].Message
			t := latest.Status.Conditions[0].LastTransitionTime
			status.CompletedAt = &t
		}
	case latest.Status.Active > 0:
		status.Phase = "Building"
	default:
		status.Phase = "Pending"
	}

	return status, nil
}

func propagationBackground() *metav1.DeletionPropagation {
	p := metav1.DeletePropagationBackground
	return &p
}

func boolPtr(b bool) *bool    { return &b }
func int64Ptr(i int64) *int64 { return &i }

// noPrivEscSecurityContext denies privilege escalation on a build container.
// Fuller hardening (non-root, capability drop, read-only rootfs) is deferred
// until it can be validated on a real cluster: the clone, Kaniko, and skopeo
// images have not been confirmed to run non-root here, and PodSecurity
// baseline on the build namespace already blocks the escape vectors
// (privileged, hostPath, host namespaces, added capabilities).
func noPrivEscSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{AllowPrivilegeEscalation: boolPtr(false)}
}

// sourceFingerprintAnnotation records which git source a build job was created
// from.
//
// Exported because the build controller must compare it, and a second spelling
// of the same key in another package is how the two would drift apart.
const SourceFingerprintAnnotation = "kipper.run/git-source"

// GitSourceFingerprint identifies the source an artefact is built from.
//
// Derived from the whole source rather than a list of fields, because listing
// them is how the last version of this went wrong: it named the four it knew
// about and omitted BuildArgs, which Kaniko is given directly and which
// therefore decides what the image contains.
//
// Only the fields that demonstrably cannot change the artefact are cleared
// first. Everything else counts, so a field added to this type later is
// included by default — and the two ways of being wrong are not equal. A field
// wrongly included discards a build that was in flight, which costs a rebuild.
// A field wrongly omitted deploys an artefact the app did not ask for.
//
// A nil source fingerprints as empty, which is what a detached app compares
// against and never matches a job built from a real one.
// UnfingerprintableSource is stamped in place of a fingerprint that could not
// be computed. The reconciler treats it as belonging to no source at all: a
// sentinel that compared equal to itself would let exactly the completions it
// exists to catch deploy unchecked, since both sides compute it the same way.
const UnfingerprintableSource = "unfingerprintable"

func GitSourceFingerprint(git *kipperv1.AppGitSource) string {
	if git == nil {
		return ""
	}
	deciding := *git
	// The credential decides whether the repository can be read, not what is
	// built from it, and rotating a token mid-build must not discard the build.
	deciding.CredentialsSecret = ""
	// Resource limits decide how the build runs, not what it produces.
	deciding.BuildResources = nil

	// encoding/json sorts map keys, so BuildArgs fingerprints deterministically.
	encoded, err := json.Marshal(deciding)
	if err != nil {
		return UnfingerprintableSource
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
