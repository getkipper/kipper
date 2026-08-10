package migration

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// Registry coordinates and the credential objects kip installs in
// kipper-system; names must match kip/internal/installer/zot.go.
const (
	zotEndpoint    = "zot.kipper-system.svc.cluster.local:5000"
	zotNamespace   = "kipper-system"
	zotPullSecret  = "zot-pull-credentials" //nolint:gosec // Secret object name, not a credential
	zotTLSSecret   = "zot-tls"
	zotPullUser    = "kipper-pull"
	zotHTTPTimeout = 15 * time.Second
)

// migrateImages decides how each app's container image reaches the target.
// The cluster registry only ever holds Kaniko-built images, so git-based apps
// are rebuilt on the target (createApp triggers the build when the App CR
// lands there) and nothing is copied. An app that runs a cluster-local image
// without a git source has no way to reach the target, so it fails the
// migration with manual copy instructions instead of producing a broken app.
// Registry repositories no live app claims are reported and left behind.
func (h *Handler) migrateImages(ctx context.Context, session *Session, namespace string) error {
	var appList kipperv1.AppList
	if err := h.CRClient.List(ctx, &appList, crclient.InNamespace(namespace)); err != nil {
		return fmt.Errorf("listing apps: %w", err)
	}

	gitApps := make(map[string]bool)
	var stuck []string
	for _, app := range appList.Items {
		if app.Spec.Git != nil {
			gitApps[app.Name] = true
			continue
		}
		if strings.HasPrefix(app.Spec.Image, zotEndpoint+"/") {
			stuck = append(stuck, app.Name)
		}
	}

	stepName := fmt.Sprintf("Container images (%s)", namespace)
	session.AddStep(Step{
		Name:   stepName,
		Phase:  "data",
		Status: StepRunning,
	})

	if len(stuck) > 0 {
		msg := fmt.Sprintf("apps %s run images from this cluster's registry but have no git source to rebuild from", strings.Join(stuck, ", "))
		session.UpdateStep(stepName, func(s *Step) {
			s.Status = StepFailed
			s.Error = msg
			s.ManualSteps = buildManualImageCopySteps(namespace, stuck)
		})
		return fmt.Errorf("%s in %s; copy the images manually, point the apps at a reachable registry, and restart the migration", msg, namespace)
	}

	unclaimed := h.unclaimedRepositories(ctx, namespace, gitApps)

	session.UpdateStep(stepName, func(s *Step) {
		s.Status = StepCompleted
		detail := "no cluster-local images to move"
		if len(gitApps) > 0 {
			detail = fmt.Sprintf("%d git-built apps will be rebuilt on the target", len(gitApps))
		}
		if len(unclaimed) > 0 {
			detail += fmt.Sprintf("; %d registry repositories belong to no app and stay behind: %s", len(unclaimed), strings.Join(unclaimed, ", "))
		}
		s.Detail = detail
		now := time.Now()
		s.CompletedAt = &now
	})

	return nil
}

// unclaimedRepositories lists this namespace's registry repositories that no
// git-based app claims. A registry that cannot be queried counts as empty:
// image-only namespaces have nothing in Zot and that is not a failure.
func (h *Handler) unclaimedRepositories(ctx context.Context, namespace string, gitApps map[string]bool) []string {
	repos, err := h.listZotImages(ctx, namespace)
	if err != nil {
		return nil
	}
	var unclaimed []string
	for _, repo := range repos {
		if !gitApps[strings.TrimPrefix(repo, namespace+"/")] {
			unclaimed = append(unclaimed, repo)
		}
	}
	return unclaimed
}

func buildManualImageCopySteps(namespace string, apps []string) []string {
	steps := []string{
		"# These apps run images that exist only in this cluster's registry and have no git source to rebuild from.",
		"# Copy each image to a registry the target can reach (or to the target's own registry), then update the app:",
		"",
		"# Work in a private scratch directory. The credential and the CA go into files, never into command",
		"# arguments, where any process on the machine could read them from the process table.",
		"umask 077 && mkdir -p zot-copy/certs && cd zot-copy",
		"kubectl -n kipper-system get secret zot-tls -o jsonpath='{.data.ca\\.crt}' | base64 -d > certs/ca.crt",
		"",
		"# Open a tunnel to the source registry (in a second terminal):",
		"kip tunnel zot --local-port 5000",
		"",
		"# Log in with the password on stdin; the registry certificate covers localhost, so TLS verification stays on:",
		"kubectl -n kipper-system get secret zot-pull-credentials -o jsonpath='{.data.password}' | base64 -d | skopeo login --username kipper-pull --password-stdin --cert-dir certs --authfile auth.json localhost:5000",
	}
	for _, app := range apps {
		steps = append(steps,
			"",
			fmt.Sprintf("# Copy %s (requires skopeo):", app),
			fmt.Sprintf("skopeo copy --src-authfile auth.json --src-cert-dir certs docker://localhost:5000/%s/%s:latest docker://<reachable-registry>/%s/%s:latest", namespace, app, namespace, app),
			fmt.Sprintf("kip app update %s --project %s --image <reachable-registry>/%s/%s:latest", app, namespace, namespace, app),
		)
	}
	steps = append(steps,
		"",
		"# Clean up the credential material:",
		"cd .. && rm -rf zot-copy",
	)
	return steps
}

// listZotImages queries the registry's catalog over its authenticated TLS
// endpoint, using the read-only credential and the CA that kip installs into
// kipper-system.
func (h *Handler) listZotImages(ctx context.Context, namespace string) ([]string, error) {
	pullSecret, err := h.Client.CoreV1().Secrets(zotNamespace).Get(ctx, zotPullSecret, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading registry pull credential: %w", err)
	}
	password := string(pullSecret.Data["password"])
	if password == "" {
		return nil, fmt.Errorf("registry pull credential has no password")
	}
	tlsSecret, err := h.Client.CoreV1().Secrets(zotNamespace).Get(ctx, zotTLSSecret, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading registry CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(tlsSecret.Data["ca.crt"]) {
		return nil, fmt.Errorf("registry CA secret holds no usable certificate")
	}

	httpClient := &http.Client{
		Timeout: zotHTTPTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+zotEndpoint+"/v2/_catalog", nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(zotPullUser, password)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("querying registry catalog: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry catalog returned %s", resp.Status)
	}

	var catalog struct {
		Repositories []string `json:"repositories"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		return nil, err
	}

	// Filter for images in this namespace (format: {namespace}/{app})
	var images []string
	prefix := namespace + "/"
	for _, repo := range catalog.Repositories {
		if strings.HasPrefix(repo, prefix) && len(repo) > len(prefix) {
			images = append(images, repo)
		}
	}

	return images, nil
}
