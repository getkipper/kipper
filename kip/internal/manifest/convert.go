package manifest

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	AppGVR = schema.GroupVersionResource{
		Group: "kipper.run", Version: "v1alpha1", Resource: "apps",
	}
	ServiceGVR = schema.GroupVersionResource{
		Group: "kipper.run", Version: "v1alpha1", Resource: "services",
	}
	VolumeGVR = schema.GroupVersionResource{
		Group: "kipper.run", Version: "v1alpha1", Resource: "volumes",
	}
	JobGVR = schema.GroupVersionResource{
		Group: "kipper.run", Version: "v1alpha1", Resource: "jobs",
	}
	FunctionGVR = schema.GroupVersionResource{
		Group: "kipper.run", Version: "v1alpha1", Resource: "functions",
	}
	ProjectGVR = schema.GroupVersionResource{
		Group: "kipper.run", Version: "v1alpha1", Resource: "projects",
	}
)

// Resource is a converted CRD object ready to apply.
type Resource struct {
	GVR    schema.GroupVersionResource
	Object *unstructured.Unstructured
}

// Convert transforms a Manifest into a list of Kubernetes CRD objects.
func Convert(m *Manifest, namespace string) []Resource {
	var resources []Resource

	for name, app := range m.Apps {
		resources = append(resources, convertApp(name, namespace, app))
	}
	for name, svc := range m.Services {
		resources = append(resources, convertService(name, namespace, svc))
	}
	for name, vol := range m.Volumes {
		resources = append(resources, convertVolume(name, namespace, vol))
	}
	for name, job := range m.Jobs {
		resources = append(resources, convertJob(name, namespace, job))
	}
	for name, fn := range m.Functions {
		resources = append(resources, convertFunction(name, namespace, fn))
	}

	return resources
}

func convertApp(name, namespace string, app AppSpec) Resource {
	spec := map[string]interface{}{
		"port": int64(app.Port),
	}

	if app.Image != "" {
		spec["image"] = app.Image
	} else if app.Git != nil {
		spec["image"] = "busybox:latest" // placeholder until first build
	}

	if app.Replicas > 0 {
		spec["replicas"] = int64(app.Replicas)
	}

	if len(app.Env) > 0 {
		envMap := make(map[string]interface{}, len(app.Env))
		for k, v := range app.Env {
			envMap[k] = v
		}
		spec["env"] = envMap
	}

	if len(app.SecretRefs) > 0 {
		refs := make([]interface{}, len(app.SecretRefs))
		for i, v := range app.SecretRefs {
			refs[i] = v
		}
		spec["secretRefs"] = refs
	}

	if app.Route != nil {
		route := map[string]interface{}{}
		if app.Route.Host != "" {
			route["host"] = app.Route.Host
		}
		if len(app.Route.RedirectFrom) > 0 {
			rf := make([]interface{}, len(app.Route.RedirectFrom))
			for i, v := range app.Route.RedirectFrom {
				rf[i] = v
			}
			route["redirectFrom"] = rf
		}
		if app.Route.Path != "" {
			route["path"] = app.Route.Path
		}
		if app.Route.Group != "" {
			route["group"] = app.Route.Group
		}
		if app.Route.NoSecurityHeaders {
			route["noSecurityHeaders"] = true
		}
		if app.Route.NoInstanceHeader {
			route["noInstanceHeader"] = true
		}
		if app.Route.RateLimit > 0 {
			route["rateLimit"] = int64(app.Route.RateLimit)
		}
		if len(app.Route.CSPAllowlist) > 0 {
			csp := make([]interface{}, len(app.Route.CSPAllowlist))
			for i, v := range app.Route.CSPAllowlist {
				csp[i] = v
			}
			route["cspAllowlist"] = csp
		}
		if len(app.Route.Redirects) > 0 {
			rds := make([]interface{}, len(app.Route.Redirects))
			for i, rd := range app.Route.Redirects {
				rdMap := map[string]interface{}{
					"source": rd.Source,
					"target": rd.Target,
				}
				if rd.Permanent {
					rdMap["permanent"] = true
				}
				rds[i] = rdMap
			}
			route["redirects"] = rds
		}
		if app.Route.BasicAuth {
			route["basicAuth"] = true
		}
		if app.Route.RequireAPIKey {
			route["requireApiKey"] = true
		}
		if len(route) > 0 {
			spec["route"] = route
		}
	}

	if app.Resources != nil {
		res := map[string]interface{}{}
		if app.Resources.Profile != "" {
			res["profile"] = app.Resources.Profile
		}
		if app.Resources.CPURequest != "" {
			res["cpuRequest"] = app.Resources.CPURequest
		}
		if app.Resources.CPULimit != "" {
			res["cpuLimit"] = app.Resources.CPULimit
		}
		if app.Resources.MemoryRequest != "" {
			res["memoryRequest"] = app.Resources.MemoryRequest
		}
		if app.Resources.MemoryLimit != "" {
			res["memoryLimit"] = app.Resources.MemoryLimit
		}
		if len(res) > 0 {
			spec["resources"] = res
		}
	}

	if len(app.ServiceBindings) > 0 {
		bindings := make([]interface{}, len(app.ServiceBindings))
		for i, b := range app.ServiceBindings {
			binding := map[string]interface{}{"name": b.Name}
			if b.Prefix != "" {
				binding["prefix"] = b.Prefix
			}
			if b.Database != "" {
				binding["database"] = b.Database
			}
			bindings[i] = binding
		}
		spec["serviceBindings"] = bindings
	}

	if len(app.Volumes) > 0 {
		vols := make([]interface{}, len(app.Volumes))
		for i, vm := range app.Volumes {
			vols[i] = map[string]interface{}{
				"name":      vm.Name,
				"mountPath": vm.MountPath,
			}
		}
		spec["volumes"] = vols
	}

	if app.Autoscale != nil {
		as := map[string]interface{}{
			"enabled": app.Autoscale.Enabled,
		}
		if app.Autoscale.MinReplicas > 0 {
			as["minReplicas"] = int64(app.Autoscale.MinReplicas)
		}
		if app.Autoscale.MaxReplicas > 0 {
			as["maxReplicas"] = int64(app.Autoscale.MaxReplicas)
		}
		if app.Autoscale.CPUTarget > 0 {
			as["cpuTarget"] = int64(app.Autoscale.CPUTarget)
		}
		if app.Autoscale.MemoryTarget > 0 {
			as["memoryTarget"] = int64(app.Autoscale.MemoryTarget)
		}
		spec["autoscale"] = as
	}

	if app.Git != nil {
		git := map[string]interface{}{
			"url": app.Git.URL,
		}
		if app.Git.Branch != "" {
			git["branch"] = app.Git.Branch
		}
		if app.Git.CredentialsSecret != "" {
			git["credentialsSecret"] = app.Git.CredentialsSecret
		}
		if app.Git.DockerfilePath != "" {
			git["dockerfilePath"] = app.Git.DockerfilePath
		}
		if app.Git.Context != "" {
			git["context"] = app.Git.Context
		}
		if len(app.Git.BuildArgs) > 0 {
			args := make(map[string]interface{}, len(app.Git.BuildArgs))
			for k, v := range app.Git.BuildArgs {
				args[k] = v
			}
			git["buildArgs"] = args
		}
		if app.Git.BuildResources != nil {
			br := map[string]interface{}{}
			if app.Git.BuildResources.Memory != "" {
				br["memory"] = app.Git.BuildResources.Memory
			}
			if app.Git.BuildResources.CPU != "" {
				br["cpu"] = app.Git.BuildResources.CPU
			}
			if len(br) > 0 {
				git["buildResources"] = br
			}
		}
		spec["git"] = git
	}

	return Resource{
		GVR: AppGVR,
		Object: &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "kipper.run/v1alpha1",
				"kind":       "App",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
					"labels": map[string]interface{}{
						"app.kubernetes.io/managed-by": "kipper",
						"app":                          name,
					},
				},
				"spec": spec,
			},
		},
	}
}

func convertService(name, namespace string, svc SvcSpec) Resource {
	spec := map[string]interface{}{
		"type": svc.Type,
	}
	if svc.Version != "" {
		spec["version"] = svc.Version
	}
	if svc.Storage != "" {
		spec["storage"] = svc.Storage
	}
	if svc.Resources != nil {
		resources := map[string]interface{}{}
		for key, value := range map[string]string{
			"cpuRequest":    svc.Resources.CPURequest,
			"cpuLimit":      svc.Resources.CPULimit,
			"memoryRequest": svc.Resources.MemoryRequest,
			"memoryLimit":   svc.Resources.MemoryLimit,
		} {
			if value != "" {
				resources[key] = value
			}
		}
		if len(resources) > 0 {
			spec["resources"] = resources
		}
	}

	return Resource{
		GVR: ServiceGVR,
		Object: &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "kipper.run/v1alpha1",
				"kind":       "Service",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
					"labels": map[string]interface{}{
						"app.kubernetes.io/managed-by": "kipper",
					},
				},
				"spec": spec,
			},
		},
	}
}

func convertVolume(name, namespace string, vol VolSpec) Resource {
	spec := map[string]interface{}{
		"size": vol.Size,
	}

	if len(vol.Mounts) > 0 {
		mounts := make([]interface{}, len(vol.Mounts))
		for i, m := range vol.Mounts {
			mounts[i] = map[string]interface{}{
				"app":       m.App,
				"mountPath": m.MountPath,
			}
		}
		spec["mounts"] = mounts
	}

	return Resource{
		GVR: VolumeGVR,
		Object: &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "kipper.run/v1alpha1",
				"kind":       "Volume",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
					"labels": map[string]interface{}{
						"app.kubernetes.io/managed-by": "kipper",
					},
				},
				"spec": spec,
			},
		},
	}
}

func convertFunction(name, namespace string, fn FuncSpec) Resource {
	spec := map[string]interface{}{}

	if fn.Image != "" {
		spec["image"] = fn.Image
	}
	if fn.Port > 0 {
		spec["port"] = int64(fn.Port)
	}
	if fn.Runtime != "" {
		spec["runtime"] = fn.Runtime
	}
	if len(fn.Env) > 0 {
		envMap := make(map[string]interface{}, len(fn.Env))
		for k, v := range fn.Env {
			envMap[k] = v
		}
		spec["env"] = envMap
	}
	if fn.Source != nil {
		src := map[string]interface{}{}
		if fn.Source.Code != "" {
			src["code"] = fn.Source.Code
		}
		if fn.Source.Handler != "" {
			src["handler"] = fn.Source.Handler
		}
		if len(fn.Source.Dependencies) > 0 {
			deps := make(map[string]interface{}, len(fn.Source.Dependencies))
			for k, v := range fn.Source.Dependencies {
				deps[k] = v
			}
			src["dependencies"] = deps
		}
		if len(src) > 0 {
			spec["source"] = src
		}
	}
	if fn.Resources != nil {
		res := map[string]interface{}{}
		if fn.Resources.CPURequest != "" {
			res["cpuRequest"] = fn.Resources.CPURequest
		}
		if fn.Resources.CPULimit != "" {
			res["cpuLimit"] = fn.Resources.CPULimit
		}
		if fn.Resources.MemoryRequest != "" {
			res["memoryRequest"] = fn.Resources.MemoryRequest
		}
		if fn.Resources.MemoryLimit != "" {
			res["memoryLimit"] = fn.Resources.MemoryLimit
		}
		if len(res) > 0 {
			spec["resources"] = res
		}
	}
	if len(fn.ServiceBindings) > 0 {
		bindings := make([]interface{}, len(fn.ServiceBindings))
		for i, b := range fn.ServiceBindings {
			binding := map[string]interface{}{"name": b.Name}
			if b.Prefix != "" {
				binding["prefix"] = b.Prefix
			}
			if b.Database != "" {
				binding["database"] = b.Database
			}
			bindings[i] = binding
		}
		spec["serviceBindings"] = bindings
	}
	if len(fn.Volumes) > 0 {
		vols := make([]interface{}, len(fn.Volumes))
		for i, vm := range fn.Volumes {
			vols[i] = map[string]interface{}{
				"name":      vm.Name,
				"mountPath": vm.MountPath,
			}
		}
		spec["volumes"] = vols
	}
	if len(fn.Triggers) > 0 {
		triggers := make([]interface{}, len(fn.Triggers))
		for i, tr := range fn.Triggers {
			t := map[string]interface{}{"type": tr.Type}
			if tr.Schedule != "" {
				t["schedule"] = tr.Schedule
			}
			if len(tr.Config) > 0 {
				cfg := make(map[string]interface{}, len(tr.Config))
				for k, v := range tr.Config {
					cfg[k] = v
				}
				t["config"] = cfg
			}
			triggers[i] = t
		}
		spec["triggers"] = triggers
	}
	if fn.NoSecurityHeaders {
		spec["noSecurityHeaders"] = true
	}
	if len(fn.CSPAllowlist) > 0 {
		csp := make([]interface{}, len(fn.CSPAllowlist))
		for i, v := range fn.CSPAllowlist {
			csp[i] = v
		}
		spec["cspAllowlist"] = csp
	}

	return Resource{
		GVR: FunctionGVR,
		Object: &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "kipper.run/v1alpha1",
				"kind":       "Function",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
					"labels": map[string]interface{}{
						"app.kubernetes.io/managed-by": "kipper",
					},
				},
				"spec": spec,
			},
		},
	}
}

func convertJob(name, namespace string, job JobSpec) Resource {
	spec := map[string]interface{}{
		"image": job.Image,
	}

	if job.Schedule != "" {
		spec["schedule"] = job.Schedule
	}
	if len(job.Command) > 0 {
		cmd := make([]interface{}, len(job.Command))
		for i, c := range job.Command {
			cmd[i] = c
		}
		spec["command"] = cmd
	}
	if len(job.Env) > 0 {
		envMap := make(map[string]interface{}, len(job.Env))
		for k, v := range job.Env {
			envMap[k] = v
		}
		spec["env"] = envMap
	}

	return Resource{
		GVR: JobGVR,
		Object: &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "kipper.run/v1alpha1",
				"kind":       "Job",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
					"labels": map[string]interface{}{
						"app.kubernetes.io/managed-by": "kipper",
					},
				},
				"spec": spec,
			},
		},
	}
}
