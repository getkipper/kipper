package deployer

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/kip/internal/workload"
)

// AppGVR is the GroupVersionResource for Kipper App CRs.
var AppGVR = schema.GroupVersionResource{
	Group:    "kipper.run",
	Version:  "v1alpha1",
	Resource: "apps",
}

// Options describes an application deployment.
type Options struct {
	Name              string
	Namespace         string
	Image             string
	Port              int32
	Replicas          int32
	Domain            string
	Env               map[string]string
	RouteGroup        string
	RoutePath         string
	NoSecurityHeaders bool
	RateLimit         int // requests per second, 0 = use cluster default
	// RedirectFrom are hostnames that answer 301 to this app's own hostname.
	RedirectFrom   []string
	MemoryLimit    string
	CPULimit       string
	Profile        string // named resource profile; empty with --cpu/--memory (those mean custom)
	GitURL         string
	GitBranch      string
	GitCredentials string // name of the K8s Secret with git credentials
	BuildMemory    string // build container memory limit override, e.g. "6Gi"
	BuildCPU       string // build container CPU limit override, e.g. "2"

	// Changed marks which deploy flags the user explicitly set (keyed by flag
	// name: "image", "replicas", "route", "no-security-headers", "rate-limit",
	// "branch", "memory", "cpu", "env", ...). On an update, only these fields
	// are written, so a redeploy touches nothing the user did not ask to
	// change — a bare `kip app deploy --image X` no longer resets replicas,
	// route, or branch to their flag defaults. On create, every field is
	// written with its default. A nil map means nothing was explicitly set.
	Changed map[string]bool
}

// Deployer creates Kipper App CRs for application deployments.
type Deployer struct {
	Client  kubernetes.Interface
	Dynamic dynamic.Interface
}

// Deploy creates or updates an App CR.
//
// `kip app deploy` is imperative: on create it writes the full spec with
// defaults; on update it writes only the fields the user explicitly set
// (opts.Changed), merging them over the live spec. A bare redeploy that just
// bumps the image therefore leaves replicas, route, branch, env and everything
// else exactly as they were — including console-set fields like
// route.requireApiKey. Declarative replace (clearing an omitted field) is the
// job of `kip apply`, not this command.
func (d *Deployer) Deploy(ctx context.Context, opts Options) error {
	release, err := workload.Reserve(ctx, d.Dynamic, opts.Namespace, opts.Name, "app")
	if err != nil {
		return err
	}

	apps := d.Dynamic.Resource(AppGVR).Namespace(opts.Namespace)

	// Whether an app of this name turned out to exist. The retry below consumes
	// the AlreadyExists that says so, and the error this returns is then the
	// update's own, so the fact has to be carried: rolling the reservation back
	// over an app that exists would free its name for another kind.
	existed := false

	// Retry on conflict: the reconciler bumps the App's resourceVersion
	// (status, finalizers) between the read and the write, so re-fetch and
	// re-merge rather than fail a valid redeploy.
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, getErr := apps.Get(ctx, opts.Name, metav1.GetOptions{})
		if getErr != nil && !errors.IsNotFound(getErr) {
			return fmt.Errorf("getting existing app: %w", getErr)
		}
		if getErr == nil {
			existed = true
		}

		if errors.IsNotFound(getErr) {
			app := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "kipper.run/v1alpha1",
					"kind":       "App",
					"metadata": map[string]interface{}{
						"name":      opts.Name,
						"namespace": opts.Namespace,
						"labels": map[string]interface{}{
							"app":            opts.Name,
							labels.ManagedBy: labels.Kipper,
						},
					},
					"spec": buildSpec(opts, true),
				},
			}
			_, err := apps.Create(ctx, app, metav1.CreateOptions{})
			if err == nil {
				return nil
			}
			if !errors.IsAlreadyExists(err) {
				return fmt.Errorf("creating app: %w", err)
			}
			// Lost a create race: another writer created the app first.
			// Fall through and update it instead.
			existed = true
			existing, getErr = apps.Get(ctx, opts.Name, metav1.GetOptions{})
			if getErr != nil {
				return fmt.Errorf("getting existing app: %w", getErr)
			}
		}

		// A rate-limit or security-header flag only qualifies an existing
		// route. Refuse to let one silently resurrect a route — and with it a
		// public Ingress — on an app whose route was removed.
		_, hadRoute, _ := unstructured.NestedMap(existing.Object, "spec", "route")
		if !hadRoute && !opts.Changed["route"] && (opts.Changed["rate-limit"] || opts.Changed["no-security-headers"]) {
			return fmt.Errorf("app %q has no route; add one with --route before setting --rate-limit or --no-security-headers", opts.Name)
		}

		// Update: merge only the user-set fields over the live spec. The merge
		// is recursive so nested objects keep their untouched keys.
		merged, _, _ := unstructured.NestedMap(existing.Object, "spec")
		if merged == nil {
			merged = map[string]interface{}{}
		}
		// Switching to a different git repo replaces the whole git block rather
		// than merging, so the new repo doesn't inherit the old repo's branch or
		// stored credentials. Updating the same repo's branch or token keeps the
		// merge.
		if opts.Changed["git"] && opts.GitURL != "" {
			if oldURL, _, _ := unstructured.NestedString(existing.Object, "spec", "git", "url"); oldURL != opts.GitURL {
				delete(merged, "git")
			}
		}
		// A profile switch replaces the resources block wholesale: merging
		// would keep the old explicit request/limit values, which override
		// the profile in the reconciler.
		if opts.Changed["profile"] && opts.Profile != "" {
			delete(merged, "resources")
		}
		// Setting an image on an app that builds from git used to detach the
		// repository as a side effect. That is data loss wearing the clothes of
		// convenience: the stored token goes with it, and nothing said so.
		// Detaching is now something you ask for.
		if opts.Changed["image"] {
			if _, hasGit := merged["git"]; hasGit {
				return errBuildsFromGit(opts.Name)
			}
		}
		mergeInto(merged, buildSpec(opts, false))
		existing.Object["spec"] = merged
		if _, err := apps.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("updating app: %w", err)
		}
		return nil
	}); err != nil {
		// See function.Create: a reservation backfilled for an app that exists
		// must not be rolled back.
		if !existed {
			release()
		}
		return err
	}
	return nil
}

// buildSpec assembles the App spec the CLI wants to write. On create every
// field is written with its default; on update only the fields whose flags the
// user explicitly set are written, so merging them over the live spec touches
// nothing else.
func buildSpec(opts Options, creating bool) map[string]interface{} {
	changed := func(flag string) bool { return opts.Changed[flag] }
	include := func(flag string) bool { return creating || changed(flag) }

	spec := map[string]interface{}{}

	if include("image") {
		spec["image"] = opts.Image
	}
	if include("port") {
		spec["port"] = int64(opts.Port)
	}
	if include("replicas") {
		replicas := opts.Replicas
		if replicas == 0 {
			replicas = 1
		}
		spec["replicas"] = int64(replicas)
	}

	// Resources. The CLI's single --memory / --cpu flags configure the limit;
	// the request mirrors the limit for Guaranteed QoS. Burstable configs
	// (request != limit) are set via the console or kip apply.
	resources := map[string]interface{}{}
	if include("memory") && opts.MemoryLimit != "" {
		resources["memoryRequest"] = opts.MemoryLimit
		resources["memoryLimit"] = opts.MemoryLimit
	}
	if include("cpu") && opts.CPULimit != "" {
		resources["cpuRequest"] = opts.CPULimit
		resources["cpuLimit"] = opts.CPULimit
	}
	if include("profile") && opts.Profile != "" {
		resources["profile"] = opts.Profile
	}
	if len(resources) > 0 {
		// Explicit --cpu/--memory values mean the custom profile; the CLI
		// enforces that they never combine with --profile.
		if _, ok := resources["profile"]; !ok {
			resources["profile"] = "custom"
		}
		spec["resources"] = resources
	}

	if include("env") && len(opts.Env) > 0 {
		envMap := make(map[string]interface{}, len(opts.Env))
		for k, v := range opts.Env {
			envMap[k] = v
		}
		spec["env"] = envMap
	}

	if include("git") && opts.GitURL != "" {
		git := map[string]interface{}{"url": opts.GitURL}
		if (creating || changed("branch")) && opts.GitBranch != "" {
			git["branch"] = opts.GitBranch
		}
		if opts.GitCredentials != "" {
			git["credentialsSecret"] = opts.GitCredentials
		}
		if creating || changed("build-memory") || changed("build-cpu") {
			br := map[string]interface{}{}
			if opts.BuildMemory != "" {
				br["memory"] = opts.BuildMemory
			}
			if opts.BuildCPU != "" {
				br["cpu"] = opts.BuildCPU
			}
			if len(br) > 0 {
				git["buildResources"] = br
			}
		}
		spec["git"] = git
	}

	// Route. Only touch the route sub-object when a route-affecting flag was set
	// (or on create). host/group/path are written only when the route itself was
	// given, so toggling --no-security-headers, --rate-limit or --redirect-from
	// on an existing app does not overwrite a console-set custom host.
	if creating || changed("route") || changed("no-security-headers") || changed("rate-limit") || changed("redirect-from") {
		route := map[string]interface{}{}
		if (creating || changed("route")) && opts.Domain != "" {
			route["host"] = opts.Domain
			if opts.RouteGroup != "" {
				route["group"] = opts.RouteGroup
			}
			if opts.RoutePath != "" {
				route["path"] = opts.RoutePath
			}
		}
		if changed("no-security-headers") || (creating && opts.NoSecurityHeaders) {
			route["noSecurityHeaders"] = opts.NoSecurityHeaders
		}
		if changed("rate-limit") || (creating && opts.RateLimit > 0) {
			route["rateLimit"] = int64(opts.RateLimit)
		}
		if changed("redirect-from") || (creating && len(opts.RedirectFrom) > 0) {
			hosts := make([]interface{}, len(opts.RedirectFrom))
			for i, h := range opts.RedirectFrom {
				hosts[i] = h
			}
			route["redirectFrom"] = hosts
		}
		if len(route) > 0 {
			spec["route"] = route
		}
	}

	return spec
}

// mergeInto recursively merges src into dst. Leaf values in src overwrite
// dst, but when both sides hold a map the merge recurses so keys present only
// in dst are preserved. This lets the CLI update the fields it manages without
// discarding sibling fields set elsewhere (console, kip apply).
func mergeInto(dst, src map[string]interface{}) {
	for k, srcVal := range src {
		srcMap, srcIsMap := srcVal.(map[string]interface{})
		dstMap, dstIsMap := dst[k].(map[string]interface{})
		if srcIsMap && dstIsMap {
			mergeInto(dstMap, srcMap)
			continue
		}
		dst[k] = srcVal
	}
}

// Delete removes an App CR and all its owned resources.
func (d *Deployer) Delete(ctx context.Context, namespace, name string) error {
	err := d.Dynamic.Resource(AppGVR).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("app %q not found", name)
		}
		return fmt.Errorf("deleting app: %w", err)
	}
	return nil
}

// Scale sets the replica count on the App CR.
func (d *Deployer) Scale(ctx context.Context, namespace, name string, replicas int32) error {
	app, err := d.Dynamic.Resource(AppGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("app %q not found", name)
		}
		return fmt.Errorf("getting app: %w", err)
	}

	// With autoscaling enabled, the HPA owns the replica count and the
	// reconciler never applies spec.replicas to the Deployment. Accepting
	// the write would report "scaled to 0" while the app keeps running —
	// a trap for anyone freezing writes before a migration.
	if enabled, _, _ := unstructured.NestedBool(app.Object, "spec", "autoscale", "enabled"); enabled {
		return fmt.Errorf("autoscaling keeps %s running regardless of the replica count; disable it first with 'kip app autoscale %s --off', then scale", name, name)
	}

	if err := unstructured.SetNestedField(app.Object, int64(replicas), "spec", "replicas"); err != nil {
		return fmt.Errorf("setting replicas: %w", err)
	}

	if _, err := d.Dynamic.Resource(AppGVR).Namespace(namespace).Update(ctx, app, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating app: %w", err)
	}

	return nil
}

// Restart triggers a rolling restart by annotating the App CR.
func (d *Deployer) Restart(ctx context.Context, namespace, name string) error {
	return d.RestartWorkload(ctx, AppGVR, "app", namespace, name)
}

// RestartWorkload triggers a rolling restart of any workload kind by bumping
// the restartedAt annotation on its CR, which the workload's controller
// projects onto the pod template. kind names the workload in the error text,
// since the GVR carries only the plural resource.
func (d *Deployer) RestartWorkload(ctx context.Context, gvr schema.GroupVersionResource, kind, namespace, name string) error {
	// Nano rather than seconds: the stamp is what makes the pod template
	// differ, so two restarts inside one second produced the identical value,
	// the second template matched the running one, and the workload did not
	// roll — while the command reported that it had.
	stamp := time.Now().Format(time.RFC3339Nano)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		workload, err := d.Dynamic.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("%s %q not found", kind, name)
			}
			return fmt.Errorf("getting %s: %w", kind, err)
		}

		annotations := workload.GetAnnotations()
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations["kipper.run/restartedAt"] = stamp
		workload.SetAnnotations(annotations)

		if _, err := d.Dynamic.Resource(gvr).Namespace(namespace).Update(ctx, workload, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("updating %s: %w", kind, err)
		}
		return nil
	})
}

// UpdateImage changes the container image on the App CR.
func (d *Deployer) UpdateImage(ctx context.Context, namespace, name, image string) error {
	app, err := d.Dynamic.Resource(AppGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("app %q not found", name)
		}
		return fmt.Errorf("getting app: %w", err)
	}

	// The same rule the deploy path applies: an app that builds from git does
	// not quietly stop doing so because someone set an image.
	if _, hasGit, _ := unstructured.NestedMap(app.Object, "spec", "git"); hasGit {
		return errBuildsFromGit(name)
	}

	if err := unstructured.SetNestedField(app.Object, image, "spec", "image"); err != nil {
		return fmt.Errorf("setting image: %w", err)
	}

	if _, err := d.Dynamic.Resource(AppGVR).Namespace(namespace).Update(ctx, app, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating app: %w", err)
	}

	return nil
}

// UpdateResources changes CPU and memory on the App CR.
// UpdateProfile switches the app onto a named resource profile. The
// resources block is replaced wholesale — leftover custom request/limit
// values would override the profile in the reconciler.
func (d *Deployer) UpdateProfile(ctx context.Context, namespace, name, profile string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		app, err := d.Dynamic.Resource(AppGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("app %q not found", name)
			}
			return fmt.Errorf("getting app: %w", err)
		}
		resources := map[string]interface{}{"profile": profile}
		if err := unstructured.SetNestedMap(app.Object, resources, "spec", "resources"); err != nil {
			return fmt.Errorf("setting resources: %w", err)
		}
		if _, err := d.Dynamic.Resource(AppGVR).Namespace(namespace).Update(ctx, app, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("updating app: %w", err)
		}
		return nil
	})
}

// UpdateRedirectFrom replaces an app's redirect domains, leaving the rest of
// its route alone. Passing an empty list clears them.
//
// Separate from Deploy because changing them is a configuration edit rather
// than a deployment: it touches spec.route, which the reconciler turns into
// Ingress rules and middlewares, and never the pod template. No pod restarts.
func (d *Deployer) UpdateRedirectFrom(ctx context.Context, namespace, name string, hosts []string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		app, err := d.Dynamic.Resource(AppGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("app %q not found", name)
			}
			return fmt.Errorf("getting app: %w", err)
		}
		// A redirect domain answers for a hostname this app serves, so there has
		// to be one. Writing the list onto an app with no route would leave the
		// redirects pointing nowhere and the reconciler with nothing to build.
		if _, found, _ := unstructured.NestedMap(app.Object, "spec", "route"); !found {
			return fmt.Errorf("app %q has no route, so it has no hostname to redirect to; give it one first", name)
		}
		if len(hosts) == 0 {
			unstructured.RemoveNestedField(app.Object, "spec", "route", "redirectFrom")
		} else if err := unstructured.SetNestedStringSlice(app.Object, hosts, "spec", "route", "redirectFrom"); err != nil {
			return fmt.Errorf("setting redirect domains: %w", err)
		}
		if _, err := d.Dynamic.Resource(AppGVR).Namespace(namespace).Update(ctx, app, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("updating app: %w", err)
		}
		return nil
	})
}

func (d *Deployer) UpdateResources(ctx context.Context, namespace, name, memoryLimit, cpuLimit string) error {
	app, err := d.Dynamic.Resource(AppGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("app %q not found", name)
		}
		return fmt.Errorf("getting app: %w", err)
	}

	resources, _, _ := unstructured.NestedMap(app.Object, "spec", "resources")
	if resources == nil {
		resources = map[string]interface{}{}
	}

	// The App CRD keys resources as request/limit pairs. Mirror the request to
	// the limit for Guaranteed QoS, matching how buildSpec writes --memory/--cpu.
	if memoryLimit != "" {
		resources["memoryRequest"] = memoryLimit
		resources["memoryLimit"] = memoryLimit
	}
	if cpuLimit != "" {
		resources["cpuRequest"] = cpuLimit
		resources["cpuLimit"] = cpuLimit
	}
	resources["profile"] = "custom"

	if err := unstructured.SetNestedField(app.Object, resources, "spec", "resources"); err != nil {
		return fmt.Errorf("setting resources: %w", err)
	}

	if _, err := d.Dynamic.Resource(AppGVR).Namespace(namespace).Update(ctx, app, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating app: %w", err)
	}

	return nil
}

// GetResources returns current resource limits from the Deployment (reads actual state).
func (d *Deployer) GetResources(ctx context.Context, namespace, name string) (memory, cpu string, err error) {
	deploy, err := d.Client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", "", err
	}

	if len(deploy.Spec.Template.Spec.Containers) == 0 {
		return "", "", nil
	}

	container := deploy.Spec.Template.Spec.Containers[0]
	if container.Resources.Limits != nil {
		if m, ok := container.Resources.Limits[corev1.ResourceMemory]; ok {
			memory = m.String()
		}
		if c, ok := container.Resources.Limits[corev1.ResourceCPU]; ok {
			cpu = c.String()
		}
	}

	return memory, cpu, nil
}

// List returns all apps in a namespace by reading App CRs.
//
// The CR is the source of truth for what apps exist; the controller
// populates status fields with live workload state. CRs that have not yet
// been reconciled still appear in the list, with phase "pending" and the
// requested image/replicas from spec.
func (d *Deployer) List(ctx context.Context, namespace string) ([]AppStatus, error) {
	crList, err := d.Dynamic.Resource(AppGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing app CRs: %w", err)
	}

	apps := make([]AppStatus, 0, len(crList.Items))
	for i := range crList.Items {
		apps = append(apps, appStatusFromCR(&crList.Items[i]))
	}
	return apps, nil
}

// AppStatus is a summary of a deployed app.
type AppStatus struct {
	Name     string
	Status   string
	Image    string
	Replicas int32
	Ready    int32
}

// appStatusFromCR derives the CLI's display status from an App CR. Status
// fields populated by the controller take precedence; spec fields fill in
// for CRs that have not yet been reconciled.
func appStatusFromCR(cr *unstructured.Unstructured) AppStatus {
	image, _, _ := unstructured.NestedString(cr.Object, "status", "image")
	if image == "" {
		image, _, _ = unstructured.NestedString(cr.Object, "spec", "image")
	}

	var replicas int32
	if r, found, _ := unstructured.NestedInt64(cr.Object, "status", "replicas"); found && r > 0 {
		replicas = int32(r) //nolint:gosec // replica counts are well within int32
	} else if r, found, _ := unstructured.NestedInt64(cr.Object, "spec", "replicas"); found {
		replicas = int32(r) //nolint:gosec // replica counts are well within int32
	}

	var ready int32
	if r, found, _ := unstructured.NestedInt64(cr.Object, "status", "readyReplicas"); found {
		ready = int32(r) //nolint:gosec // replica counts are well within int32
	}

	phase, _, _ := unstructured.NestedString(cr.Object, "status", "phase")
	if phase == "" {
		phase = "Pending"
	}

	return AppStatus{
		Name:     cr.GetName(),
		Status:   strings.ToLower(phase),
		Image:    image,
		Replicas: replicas,
		Ready:    ready,
	}
}

// RemoveGitSource detaches an app's git repository, and reports whether there
// was one to detach.
//
// Only spec.git is cleared. The token the source used and the build status it
// left behind belong to the controller, which removes them on the next pass —
// a client that did all three would leave a half-detached app behind on any
// failure between them, with nothing coming back to finish the job.
//
// The image the app runs is untouched: detaching a source is not a deploy, and
// an app whose pods vanish because its build config changed would be a
// surprise nobody asked for.
func (d *Deployer) RemoveGitSource(ctx context.Context, namespace, name string) (bool, error) {
	removed := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		app, err := d.Dynamic.Resource(AppGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("app %q not found", name)
			}
			return fmt.Errorf("getting app: %w", err)
		}
		spec, found, _ := unstructured.NestedMap(app.Object, "spec")
		if !found {
			return nil
		}
		if _, hasGit := spec["git"]; !hasGit {
			return nil
		}
		delete(spec, "git")
		app.Object["spec"] = spec
		if _, err := d.Dynamic.Resource(AppGVR).Namespace(namespace).Update(ctx, app, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("updating app: %w", err)
		}
		removed = true
		return nil
	})
	return removed, err
}

// errBuildsFromGit is the one refusal both image writers give, so the two
// cannot drift into disagreeing about what setting an image means.
func errBuildsFromGit(name string) error {
	return fmt.Errorf("%s builds its image from git, so setting one here would be overwritten by the next build. Run 'kip app git remove %s' first if it should deploy prebuilt images instead", name, name)
}
