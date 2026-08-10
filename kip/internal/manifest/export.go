package manifest

import (
	"context"
	"fmt"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
)

// Export reads live CRs from a namespace and produces a Manifest.
func Export(ctx context.Context, dynClient dynamic.Interface, project, environment, namespace string) (*Manifest, error) {
	m := &Manifest{
		Project:     project,
		Environment: environment,
	}

	// Try to read project metadata
	projectCR, projErr := dynClient.Resource(ProjectGVR).Get(ctx, project, metav1.GetOptions{})
	if projErr == nil {
		spec := projectCR.Object["spec"].(map[string]interface{})
		if dn, ok := spec["displayName"].(string); ok {
			m.DisplayName = dn
		}
		if envs := extractSlice(spec, "environments"); len(envs) > 0 {
			for _, e := range envs {
				if em, ok := e.(map[string]interface{}); ok {
					if name, ok := em["name"].(string); ok {
						m.Environments = append(m.Environments, name)
					}
				}
			}
		}
	}

	if err := exportApps(ctx, dynClient, namespace, m); err != nil {
		return nil, err
	}
	if err := exportServices(ctx, dynClient, namespace, m); err != nil {
		return nil, err
	}
	if err := exportVolumes(ctx, dynClient, namespace, m); err != nil {
		return nil, err
	}
	if err := exportJobs(ctx, dynClient, namespace, m); err != nil {
		return nil, err
	}
	if err := exportFunctions(ctx, dynClient, namespace, m); err != nil {
		return nil, err
	}

	return m, nil
}

// Marshal converts a Manifest to YAML bytes.
func Marshal(m *Manifest) ([]byte, error) {
	return yaml.Marshal(m)
}

func exportApps(ctx context.Context, dynClient dynamic.Interface, namespace string, m *Manifest) error {
	list, err := dynClient.Resource(AppGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/managed-by=kipper",
	})
	if err != nil {
		return fmt.Errorf("listing apps: %w", err)
	}

	if len(list.Items) > 0 {
		m.Apps = make(map[string]AppSpec, len(list.Items))
	}

	for _, item := range list.Items {
		name := item.GetName()
		spec := item.Object["spec"].(map[string]interface{})

		app := AppSpec{}
		app.Git = exportAppGit(spec)
		// A git app's built image is build output, not manifest state. Emit it
		// git-only so the manifest passes apply's image/git mutual-exclusion
		// check and export→apply round-trips without resetting the built image.
		if app.Git == nil {
			if v, ok := spec["image"].(string); ok {
				app.Image = v
			}
		}
		if v, ok := spec["port"].(int64); ok {
			app.Port = int32(v) //nolint:gosec // port values are bounded by K8s validation
		}
		if v, ok := spec["replicas"].(int64); ok && v > 1 {
			app.Replicas = int32(v) //nolint:gosec // replica values are bounded
		}
		if env := extractStringMap(spec, "env"); len(env) > 0 {
			app.Env = env
		}
		if refs := extractStringSlice(spec, "secretRefs"); len(refs) > 0 {
			app.SecretRefs = refs
		}
		app.Route = exportRoute(spec)
		app.Resources = exportResources(spec)
		app.ServiceBindings = exportBindings(spec)
		app.Volumes = exportVolumeMounts(spec)
		app.Autoscale = exportAutoscale(spec)

		m.Apps[name] = app
	}

	return nil
}

// exportRoute mirrors the controller's AppRoute type. Every optional
// field on AppRoute must be readable here, otherwise a CR-to-manifest
// round-trip will silently drop it. The acme-tools migration found this
// the hard way for cspAllowlist; the regression test in
// manifest_test.go now covers every field.
func exportRoute(spec map[string]interface{}) *RouteSpec {
	route := extractMap(spec, "route")
	if route == nil {
		return nil
	}
	r := &RouteSpec{}
	if v, ok := route["host"].(string); ok {
		r.Host = v
	}
	if rf := extractStringSlice(route, "redirectFrom"); len(rf) > 0 {
		r.RedirectFrom = rf
	}
	if v, ok := route["path"].(string); ok {
		r.Path = v
	}
	if v, ok := route["group"].(string); ok {
		r.Group = v
	}
	if v, ok := route["noSecurityHeaders"].(bool); ok {
		r.NoSecurityHeaders = v
	}
	if v, ok := route["noInstanceHeader"].(bool); ok {
		r.NoInstanceHeader = v
	}
	if v, ok := route["rateLimit"].(int64); ok {
		r.RateLimit = int(v)
	}
	if csp := extractStringSlice(route, "cspAllowlist"); len(csp) > 0 {
		r.CSPAllowlist = csp
	}
	if redirects := extractSlice(route, "redirects"); len(redirects) > 0 {
		for _, rd := range redirects {
			if rm, ok := rd.(map[string]interface{}); ok {
				rule := RedirectSpec{}
				if v, ok := rm["source"].(string); ok {
					rule.Source = v
				}
				if v, ok := rm["target"].(string); ok {
					rule.Target = v
				}
				if v, ok := rm["permanent"].(bool); ok {
					rule.Permanent = v
				}
				r.Redirects = append(r.Redirects, rule)
			}
		}
	}
	if v, ok := route["basicAuth"].(bool); ok {
		r.BasicAuth = v
	}
	if v, ok := route["requireApiKey"].(bool); ok {
		r.RequireAPIKey = v
	}
	return r
}

func exportResources(spec map[string]interface{}) *ResourceSpec {
	res := extractMap(spec, "resources")
	if res == nil {
		return nil
	}
	r := &ResourceSpec{}
	if v, ok := res["profile"].(string); ok {
		r.Profile = v
	}
	if v, ok := res["cpuRequest"].(string); ok {
		r.CPURequest = v
	}
	if v, ok := res["cpuLimit"].(string); ok {
		r.CPULimit = v
	}
	if v, ok := res["memoryRequest"].(string); ok {
		r.MemoryRequest = v
	}
	if v, ok := res["memoryLimit"].(string); ok {
		r.MemoryLimit = v
	}
	// Every field on AppResources / FunctionResources is optional, so an
	// otherwise-empty block still represents valid intent ("override
	// nothing"). Emit even if all fields are zero so round-trips don't
	// drop the resources stanza entirely.
	return r
}

func exportBindings(spec map[string]interface{}) []BindingSpec {
	bindings := extractSlice(spec, "serviceBindings")
	if len(bindings) == 0 {
		return nil
	}
	out := make([]BindingSpec, 0, len(bindings))
	for _, b := range bindings {
		bm, ok := b.(map[string]interface{})
		if !ok {
			continue
		}
		binding := BindingSpec{}
		if v, ok := bm["name"].(string); ok {
			binding.Name = v
		}
		if v, ok := bm["prefix"].(string); ok {
			binding.Prefix = v
		}
		// per-binding database name. acme-tools migration found this
		// silently dropped on 3 blog-test apps; the apps then fell
		// back to the postgres service's default `app` database and could
		// not see their tables.
		if v, ok := bm["database"].(string); ok {
			binding.Database = v
		}
		out = append(out, binding)
	}
	return out
}

func exportVolumeMounts(spec map[string]interface{}) []VolumeMountSpec {
	vols := extractSlice(spec, "volumes")
	if len(vols) == 0 {
		return nil
	}
	out := make([]VolumeMountSpec, 0, len(vols))
	for _, v := range vols {
		vm, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		mount := VolumeMountSpec{}
		if s, ok := vm["name"].(string); ok {
			mount.Name = s
		}
		if s, ok := vm["mountPath"].(string); ok {
			mount.MountPath = s
		}
		out = append(out, mount)
	}
	return out
}

func exportAutoscale(spec map[string]interface{}) *AutoscaleSpec {
	as := extractMap(spec, "autoscale")
	if as == nil {
		return nil
	}
	// Emit the block whether enabled or disabled — the controller treats
	// `autoscale.enabled: false` as an explicit opt-out separate from
	// "autoscale unset", and dropping the block on export would lose
	// that distinction.
	a := &AutoscaleSpec{}
	if v, ok := as["enabled"].(bool); ok {
		a.Enabled = v
	}
	if v, ok := as["minReplicas"].(int64); ok {
		a.MinReplicas = int32(v) //nolint:gosec // bounded by K8s
	}
	if v, ok := as["maxReplicas"].(int64); ok {
		a.MaxReplicas = int32(v) //nolint:gosec // bounded by K8s
	}
	if v, ok := as["cpuTarget"].(int64); ok {
		a.CPUTarget = int32(v) //nolint:gosec // bounded by K8s
	}
	if v, ok := as["memoryTarget"].(int64); ok {
		a.MemoryTarget = int32(v) //nolint:gosec // bounded by K8s
	}
	return a
}

func exportAppGit(spec map[string]interface{}) *GitSpec {
	git := extractMap(spec, "git")
	if git == nil {
		return nil
	}
	g := &GitSpec{}
	if v, ok := git["url"].(string); ok {
		g.URL = v
	}
	if v, ok := git["branch"].(string); ok {
		g.Branch = v
	}
	if v, ok := git["credentialsSecret"].(string); ok {
		g.CredentialsSecret = v
	}
	if v, ok := git["dockerfilePath"].(string); ok {
		g.DockerfilePath = v
	}
	if v, ok := git["context"].(string); ok {
		g.Context = v
	}
	if args := extractStringMap(git, "buildArgs"); len(args) > 0 {
		g.BuildArgs = args
	}
	if br, ok := git["buildResources"].(map[string]interface{}); ok {
		res := &BuildResources{}
		if v, ok := br["memory"].(string); ok {
			res.Memory = v
		}
		if v, ok := br["cpu"].(string); ok {
			res.CPU = v
		}
		if res.Memory != "" || res.CPU != "" {
			g.BuildResources = res
		}
	}
	return g
}

func exportServices(ctx context.Context, dynClient dynamic.Interface, namespace string, m *Manifest) error {
	list, err := dynClient.Resource(ServiceGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/managed-by=kipper",
	})
	if err != nil {
		return fmt.Errorf("listing services: %w", err)
	}

	if len(list.Items) > 0 {
		m.Services = make(map[string]SvcSpec, len(list.Items))
	}

	for _, item := range list.Items {
		spec := item.Object["spec"].(map[string]interface{})
		svc := SvcSpec{}
		if v, ok := spec["type"].(string); ok {
			svc.Type = v
		}
		if v, ok := spec["version"].(string); ok {
			svc.Version = v
		}
		if v, ok := spec["storage"].(string); ok {
			svc.Storage = v
		}
		// Export every tuned request/limit value as-is — collapsing the
		// pairs would lose burstable shapes and let a later apply revert
		// the tuning.
		if resources, ok := spec["resources"].(map[string]interface{}); ok {
			rs := &ResourceSpec{}
			if v, ok := resources["cpuRequest"].(string); ok {
				rs.CPURequest = v
			}
			if v, ok := resources["cpuLimit"].(string); ok {
				rs.CPULimit = v
			}
			if v, ok := resources["memoryRequest"].(string); ok {
				rs.MemoryRequest = v
			}
			if v, ok := resources["memoryLimit"].(string); ok {
				rs.MemoryLimit = v
			}
			if *rs != (ResourceSpec{}) {
				svc.Resources = rs
			}
		}
		m.Services[item.GetName()] = svc
	}

	return nil
}

func exportVolumes(ctx context.Context, dynClient dynamic.Interface, namespace string, m *Manifest) error {
	list, err := dynClient.Resource(VolumeGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/managed-by=kipper",
	})
	if err != nil {
		return fmt.Errorf("listing volumes: %w", err)
	}

	if len(list.Items) > 0 {
		m.Volumes = make(map[string]VolSpec, len(list.Items))
	}

	for _, item := range list.Items {
		spec := item.Object["spec"].(map[string]interface{})
		vol := VolSpec{}
		if v, ok := spec["size"].(string); ok {
			vol.Size = v
		}
		if mounts := extractSlice(spec, "mounts"); len(mounts) > 0 {
			for _, mt := range mounts {
				if mm, ok := mt.(map[string]interface{}); ok {
					vol.Mounts = append(vol.Mounts, MountSpec{
						App:       mm["app"].(string),
						MountPath: mm["mountPath"].(string),
					})
				}
			}
		}
		m.Volumes[item.GetName()] = vol
	}

	return nil
}

func exportJobs(ctx context.Context, dynClient dynamic.Interface, namespace string, m *Manifest) error {
	list, err := dynClient.Resource(JobGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/managed-by=kipper",
	})
	if err != nil {
		return fmt.Errorf("listing jobs: %w", err)
	}

	if len(list.Items) > 0 {
		m.Jobs = make(map[string]JobSpec, len(list.Items))
	}

	for _, item := range list.Items {
		spec := item.Object["spec"].(map[string]interface{})
		job := JobSpec{}
		if v, ok := spec["image"].(string); ok {
			job.Image = v
		}
		if v, ok := spec["schedule"].(string); ok {
			job.Schedule = v
		}
		if cmd := extractStringSlice(spec, "command"); len(cmd) > 0 {
			job.Command = cmd
		}
		if env := extractStringMap(spec, "env"); len(env) > 0 {
			job.Env = env
		}
		m.Jobs[item.GetName()] = job
	}

	return nil
}

func exportFunctions(ctx context.Context, dynClient dynamic.Interface, namespace string, m *Manifest) error {
	list, err := dynClient.Resource(FunctionGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/managed-by=kipper",
	})
	if err != nil {
		return fmt.Errorf("listing functions: %w", err)
	}

	if len(list.Items) > 0 {
		m.Functions = make(map[string]FuncSpec, len(list.Items))
	}

	for _, item := range list.Items {
		spec := item.Object["spec"].(map[string]interface{})
		fn := FuncSpec{}
		if v, ok := spec["image"].(string); ok {
			fn.Image = v
		}
		if v, ok := spec["port"].(int64); ok {
			fn.Port = int32(v) //nolint:gosec // port values are bounded by K8s validation
		}
		if v, ok := spec["runtime"].(string); ok {
			fn.Runtime = v
		}
		if env := extractStringMap(spec, "env"); len(env) > 0 {
			fn.Env = env
		}
		// acme-tools migration found `Function.spec.source` (the actual
		// function code) silently dropped on export — the restored
		// Function would have run an empty body if it had ever fired.
		if src := extractMap(spec, "source"); src != nil {
			fn.Source = &FuncSourceSpec{}
			if v, ok := src["code"].(string); ok {
				fn.Source.Code = v
			}
			if v, ok := src["handler"].(string); ok {
				fn.Source.Handler = v
			}
			if deps := extractStringMap(src, "dependencies"); len(deps) > 0 {
				fn.Source.Dependencies = deps
			}
		}
		fn.Resources = exportResources(spec)
		fn.ServiceBindings = exportBindings(spec)
		fn.Volumes = exportVolumeMounts(spec)
		if triggers := extractSlice(spec, "triggers"); len(triggers) > 0 {
			for _, tr := range triggers {
				tm, ok := tr.(map[string]interface{})
				if !ok {
					continue
				}
				trigger := TriggerSpec{}
				if v, ok := tm["type"].(string); ok {
					trigger.Type = v
				}
				if v, ok := tm["schedule"].(string); ok {
					trigger.Schedule = v
				}
				if cfg := extractStringMap(tm, "config"); len(cfg) > 0 {
					trigger.Config = cfg
				}
				fn.Triggers = append(fn.Triggers, trigger)
			}
		}
		if v, ok := spec["noSecurityHeaders"].(bool); ok {
			fn.NoSecurityHeaders = v
		}
		if csp := extractStringSlice(spec, "cspAllowlist"); len(csp) > 0 {
			fn.CSPAllowlist = csp
		}
		m.Functions[item.GetName()] = fn
	}

	return nil
}

func extractMap(obj map[string]interface{}, key string) map[string]interface{} {
	v, ok := obj[key]
	if !ok {
		return nil
	}
	m, ok := v.(map[string]interface{})
	if !ok {
		return nil
	}
	return m
}

func extractStringMap(obj map[string]interface{}, key string) map[string]string {
	m := extractMap(obj, key)
	if m == nil {
		return nil
	}
	result := make(map[string]string, len(m))
	for k, v := range m {
		if s, ok := v.(string); ok {
			result[k] = s
		}
	}
	return result
}

func extractSlice(obj map[string]interface{}, key string) []interface{} {
	v, ok := obj[key]
	if !ok {
		return nil
	}
	s, ok := v.([]interface{})
	if !ok {
		return nil
	}
	return s
}

// extractStringSlice reads a []string from an unstructured spec slot.
// Skips non-string entries silently — the CR's validation already
// rejects mixed types, so any non-string here is an apiserver bug we
// can't recover from.
func extractStringSlice(obj map[string]interface{}, key string) []string {
	raw := extractSlice(obj, key)
	if len(raw) == 0 {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
