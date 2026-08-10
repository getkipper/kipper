package manifest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validManifest = `
project: blog
environment: test

apps:
  frontend:
    image: registry.git.example.com/frontend:latest
    port: 80
    route:
      group: blog
      path: /
  api:
    image: registry.git.example.com/api:latest
    port: 8080
    replicas: 2
    resources:
      profile: jvm
    env:
      LOG_LEVEL: info
    serviceBindings:
      - name: db
        prefix: DB_
    autoscale:
      enabled: true
      minReplicas: 2
      maxReplicas: 5
      cpuTarget: 70

services:
  db:
    type: postgres
    storage: 5Gi

volumes:
  uploads:
    size: 10Gi
    mounts:
      - app: api
        mountPath: /data/uploads

jobs:
  cleanup:
    image: registry.git.example.com/cleanup:latest
    schedule: "0 3 * * *"
`

func TestParseFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kipper.yaml")
	require.NoError(t, os.WriteFile(path, []byte(validManifest), 0600))

	m, err := ParseFile(path)
	require.NoError(t, err)

	assert.Equal(t, "blog", m.Project)
	assert.Equal(t, "test", m.Environment)

	// Apps
	require.Len(t, m.Apps, 2)
	assert.Equal(t, int32(80), m.Apps["frontend"].Port)
	assert.Equal(t, "registry.git.example.com/api:latest", m.Apps["api"].Image)
	assert.Equal(t, int32(2), m.Apps["api"].Replicas)
	assert.Equal(t, "jvm", m.Apps["api"].Resources.Profile)
	assert.Equal(t, "info", m.Apps["api"].Env["LOG_LEVEL"])
	require.Len(t, m.Apps["api"].ServiceBindings, 1)
	assert.Equal(t, "db", m.Apps["api"].ServiceBindings[0].Name)
	assert.True(t, m.Apps["api"].Autoscale.Enabled)
	assert.Equal(t, int32(70), m.Apps["api"].Autoscale.CPUTarget)

	// Services
	require.Len(t, m.Services, 1)
	assert.Equal(t, "postgres", m.Services["db"].Type)
	assert.Equal(t, "5Gi", m.Services["db"].Storage)

	// Volumes
	require.Len(t, m.Volumes, 1)
	assert.Equal(t, "10Gi", m.Volumes["uploads"].Size)
	require.Len(t, m.Volumes["uploads"].Mounts, 1)
	assert.Equal(t, "api", m.Volumes["uploads"].Mounts[0].App)

	// Jobs
	require.Len(t, m.Jobs, 1)
	assert.Equal(t, "0 3 * * *", m.Jobs["cleanup"].Schedule)
}

func TestParseFile_MissingProject(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	require.NoError(t, os.WriteFile(path, []byte("apps:\n  foo:\n    port: 80\n    image: x"), 0600))

	_, err := ParseFile(path)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "project is required")
}

func TestParseFile_AppMissingPort(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	require.NoError(t, os.WriteFile(path, []byte("project: test\napps:\n  foo:\n    image: x"), 0600))

	_, err := ParseFile(path)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "port is required")
}

func TestParseFile_AppMissingImageAndGit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	require.NoError(t, os.WriteFile(path, []byte("project: test\napps:\n  foo:\n    port: 80"), 0600))

	_, err := ParseFile(path)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "either image or git is required")
}

func TestParseDir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "test.yaml"), []byte("project: p\nenvironment: test\napps:\n  a:\n    image: x\n    port: 80"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "prod.yaml"), []byte("project: p\nenvironment: prod\napps:\n  a:\n    image: x\n    port: 80"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("not yaml"), 0600))

	manifests, err := Parse(dir)
	require.NoError(t, err)
	assert.Len(t, manifests, 2)
}

func TestConvert(t *testing.T) {
	m := &Manifest{
		Project:     "blog",
		Environment: "test",
		Apps: map[string]AppSpec{
			"api": {Image: "img:latest", Port: 8080, Replicas: 2},
		},
		Services: map[string]SvcSpec{
			"db": {Type: "postgres", Storage: "5Gi"},
		},
		Volumes: map[string]VolSpec{
			"uploads": {Size: "10Gi", Mounts: []MountSpec{{App: "api", MountPath: "/data"}}},
		},
		Jobs: map[string]JobSpec{
			"cleanup": {Image: "cleanup:latest", Schedule: "0 3 * * *"},
		},
	}

	resources := Convert(m, "blog-test")

	assert.Len(t, resources, 4) // 1 app + 1 service + 1 volume + 1 job

	// Find the app resource
	var appRes *Resource
	for i := range resources {
		if resources[i].GVR == AppGVR {
			appRes = &resources[i]
			break
		}
	}
	require.NotNil(t, appRes)

	assert.Equal(t, "api", appRes.Object.GetName())
	assert.Equal(t, "blog-test", appRes.Object.GetNamespace())

	spec := appRes.Object.Object["spec"].(map[string]interface{})
	assert.Equal(t, "img:latest", spec["image"])
	assert.Equal(t, int64(8080), spec["port"])
	assert.Equal(t, int64(2), spec["replicas"])
}

func TestConvertGitApp(t *testing.T) {
	m := &Manifest{
		Project: "test",
		Apps: map[string]AppSpec{
			"api": {
				Port: 3000,
				Git: &GitSpec{
					URL:    "https://github.com/acme/api.git",
					Branch: "develop",
				},
			},
		},
	}

	resources := Convert(m, "test")
	require.Len(t, resources, 1)

	spec := resources[0].Object.Object["spec"].(map[string]interface{})
	assert.Equal(t, "busybox:latest", spec["image"]) // placeholder
	git := spec["git"].(map[string]interface{})
	assert.Equal(t, "https://github.com/acme/api.git", git["url"])
	assert.Equal(t, "develop", git["branch"])
}

func TestMarshalRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kipper.yaml")
	require.NoError(t, os.WriteFile(path, []byte(validManifest), 0600))

	m, err := ParseFile(path)
	require.NoError(t, err)

	data, err := Marshal(m)
	require.NoError(t, err)

	// Parse the marshalled output again
	path2 := filepath.Join(dir, "roundtrip.yaml")
	require.NoError(t, os.WriteFile(path2, data, 0600))

	m2, err := ParseFile(path2)
	require.NoError(t, err)

	assert.Equal(t, m.Project, m2.Project)
	assert.Equal(t, len(m.Apps), len(m2.Apps))
	assert.Equal(t, len(m.Services), len(m2.Services))
}
