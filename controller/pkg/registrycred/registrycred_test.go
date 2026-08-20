package registrycred

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestRegistryHostForImage(t *testing.T) {
	tests := []struct {
		image string
		want  string
	}{
		{"nginx:1.25", dockerHubKey},
		{"nginx", dockerHubKey},
		{"myorg/private:v1", dockerHubKey},     // implicit Docker Hub, no host
		{"library/nginx:latest", dockerHubKey}, // implicit Docker Hub
		{"ghcr.io/org/app:v1", "ghcr.io"},      // explicit host
		{"GHCR.IO/Org/App", "ghcr.io"},         // host lowercased
		{"registry.example.com/team/app", "registry.example.com"},
		{"myreg.com:8443/team/app:1.2", "myreg.com:8443"}, // host with port; tag stays in repo
		{"localhost:5000/app", "localhost:5000"},
		{"zot.kipper-system.svc.cluster.local:5000/proj/app:abc", "zot.kipper-system.svc.cluster.local:5000"},
	}
	for _, tt := range tests {
		if got := RegistryHostForImage(tt.image); got != tt.want {
			t.Errorf("RegistryHostForImage(%q) = %q, want %q", tt.image, got, tt.want)
		}
	}
}

func TestFindForProject(t *testing.T) {
	entries := []Entry{
		{Name: "dockerhub", Server: "docker.io", Username: "u", Password: "p", AllowedProjects: []string{"acme"}},
		{Name: "ghcr-acme", Server: "https://ghcr.io", Username: "u", Password: "p", AllowedProjects: []string{"acme"}},
		{Name: "ghcr-shop", Server: "ghcr.io", Username: "u2", Password: "p2", AllowedProjects: []string{"shop"}},
		{Name: "self", Server: "registry.example.com:8443", Username: "u", Password: "p", AllowedProjects: []string{"acme"}},
	}
	tests := []struct {
		image   string
		project string
		want    string // entry name, "" for no match
	}{
		{"nginx:1.25", "acme", "dockerhub"},                            // implicit Docker Hub matches the docker.io entry
		{"myorg/private:v1", "acme", "dockerhub"},                      // implicit Docker Hub
		{"ghcr.io/org/app:v1", "acme", "ghcr-acme"},                    // matches despite the https:// in the entry
		{"ghcr.io/org/app:v1", "shop", "ghcr-shop"},                    // same host, later entry — the project's own grant wins
		{"ghcr.io/org/app:v1", "gamma", ""},                            // same host, no grant → anonymous
		{"nginx:1.25", "shop", ""},                                     // entry exists but project not granted
		{"nginx:1.25", "", ""},                                         // no project (unmanaged namespace) → never granted
		{"registry.example.com:8443/x", "acme", "self"},                // matches host:port
		{"registry.example.com/x", "acme", ""},                         // different (default) port → no match
		{"quay.io/org/app", "acme", ""},                                // unconfigured registry
		{"zot.kipper-system.svc.cluster.local:5000/p/a:t", "acme", ""}, // cluster registry, not in list
	}
	for _, tt := range tests {
		got := FindForProject(entries, tt.image, tt.project)
		name := ""
		if got != nil {
			name = got.Name
		}
		if name != tt.want {
			t.Errorf("FindForProject(%q, %q) = %q, want %q", tt.image, tt.project, name, tt.want)
		}
	}
}

func TestClusterImage(t *testing.T) {
	tests := []struct {
		image     string
		isCluster bool
		namespace string
	}{
		{ClusterRegistryHost + "/project-test/web:abc", true, "project-test"},
		{"ZOT.KIPPER-SYSTEM.SVC.CLUSTER.LOCAL:5000/project-test/web:abc", true, "project-test"}, // host case must not bypass
		{ClusterRegistryHost + "/onlyone", true, ""},                                            // no namespace component
		{"ghcr.io/org/app:v1", false, ""},
		{"nginx:1.25", false, ""},
		{"", false, ""},
	}
	for _, tt := range tests {
		if got := IsClusterRegistryImage(tt.image); got != tt.isCluster {
			t.Errorf("IsClusterRegistryImage(%q) = %v, want %v", tt.image, got, tt.isCluster)
		}
		if got := ClusterImageNamespace(tt.image); got != tt.namespace {
			t.Errorf("ClusterImageNamespace(%q) = %q, want %q", tt.image, got, tt.namespace)
		}
	}
}

func TestDockerConfigJSON(t *testing.T) {
	e := Entry{Server: "ghcr.io", Username: "bob", Password: "secret"}
	data, err := e.DockerConfigJSON()
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Auths map[string]struct {
			Username, Password, Auth string
		} `json:"auths"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	auth, ok := parsed.Auths["ghcr.io"]
	if !ok {
		t.Fatalf("expected an auth entry keyed on the server, got %s", data)
	}
	if auth.Username != "bob" || auth.Password != "secret" {
		t.Errorf("auth = %+v, want bob/secret", auth)
	}
	if want := base64.StdEncoding.EncodeToString([]byte("bob:secret")); auth.Auth != want {
		t.Errorf("auth field = %q, want base64 username:password %q", auth.Auth, want)
	}
}

func TestAllowsProject(t *testing.T) {
	e := Entry{AllowedProjects: []string{"acme", "beta"}}
	if !e.AllowsProject("acme") {
		t.Error("a listed project must be allowed")
	}
	if e.AllowsProject("gamma") {
		t.Error("an unlisted project must be denied")
	}
	if e.AllowsProject("") {
		t.Error("an empty project name must be denied")
	}
	if (Entry{}).AllowsProject("acme") {
		t.Error("an empty allow-list must deny every project (fail closed)")
	}
}

func TestLoadWriteRoundTrip(t *testing.T) {
	client := fake.NewClientset()
	ctx := context.Background()

	if got, err := Load(ctx, client); err != nil || got != nil {
		t.Fatalf("Load with no Secret should be (nil, nil), got (%v, %v)", got, err)
	}

	want := []Entry{{Name: "ghcr", Server: "ghcr.io", Username: "u", Password: "p"}}
	if err := Update(ctx, client, func([]Entry) ([]Entry, error) { return want, nil }); err != nil {
		t.Fatal(err)
	}
	got, err := Load(ctx, client)
	if err != nil || len(got) != 1 || got[0].Name != "ghcr" {
		t.Fatalf("round trip mismatch: %v, %v", got, err)
	}
}

func TestLoad_MalformedFailsClosed(t *testing.T) {
	client := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigSecretName, Namespace: Namespace},
		Data:       map[string][]byte{"registries": []byte("{bad")},
	})
	if _, err := Load(context.Background(), client); err == nil {
		t.Fatal("a malformed registry list must return an error")
	}
}

// The allow-list is written for every entry on every write, not only for the one
// a change happened to touch. A list holding entries from before the field was
// always present would otherwise keep emitting null for the ones left alone,
// and the stored document would not say what was decided about them.
func TestUpdateWritesAnEmptyListForEveryLegacyEntry(t *testing.T) {
	legacy := `[{"name":"ghcr","server":"ghcr.io","username":"u","password":"p"},` +
		`{"name":"quay","server":"quay.io","username":"u","password":"p"}]`
	client := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigSecretName, Namespace: Namespace},
		Data:       map[string][]byte{dataKey: []byte(legacy)},
	})

	// Touch one entry. The other is never looked at by the mutate.
	err := Update(context.Background(), client, func(entries []Entry) ([]Entry, error) {
		entries[0].Password = "rotated"
		return entries, nil
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	live, getErr := client.CoreV1().Secrets(Namespace).Get(
		context.Background(), ConfigSecretName, metav1.GetOptions{})
	if getErr != nil {
		t.Fatalf("get: %v", getErr)
	}
	stored := string(live.Data[dataKey])
	if strings.Contains(stored, "null") {
		t.Errorf("a legacy entry was written back as null: %s", stored)
	}
	if strings.Count(stored, `"allowedProjects":[]`) != 2 {
		t.Errorf("both entries should record an empty decision: %s", stored)
	}
}

func TestUpdateKeepsWhatElseIsInTheSecret(t *testing.T) {
	client := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigSecretName, Namespace: Namespace, Annotations: map[string]string{"kipper.run/note": "kept"}},
		Data:       map[string][]byte{dataKey: []byte(`[{"name":"ghcr"}]`), "unrelated": []byte("kept")},
	})

	err := Update(context.Background(), client, func(entries []Entry) ([]Entry, error) {
		return append(entries, Entry{Name: "quay"}), nil
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	live, _ := client.CoreV1().Secrets(Namespace).Get(
		context.Background(), ConfigSecretName, metav1.GetOptions{})
	if string(live.Data["unrelated"]) != "kept" || live.Annotations["kipper.run/note"] != "kept" {
		t.Error("the write replaced the Secret rather than editing it")
	}
}

func TestUpdateWritesNothingWhenTheMutateRefuses(t *testing.T) {
	client := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigSecretName, Namespace: Namespace},
		Data:       map[string][]byte{dataKey: []byte(`[{"name":"ghcr","allowedProjects":["shop"]}]`)},
	})

	err := Update(context.Background(), client, func([]Entry) ([]Entry, error) {
		return nil, &UnknownRegistryError{Name: "missing"}
	})
	if err == nil {
		t.Fatal("a refused change reported success")
	}

	after, _ := Load(context.Background(), client)
	if len(after) != 1 || !after[0].AllowsProject("shop") {
		t.Errorf("the stored list changed after a refused write: %+v", after)
	}
}
