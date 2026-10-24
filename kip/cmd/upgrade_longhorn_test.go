package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

// longhornCluster describes a cluster for the Longhorn report tests.
type longhornCluster struct {
	storage, profile, backup string
	bsl                      string // Velero's s3Url. "-" means no location.
	version                  string
	record                   map[string]string
	nodes                    []*corev1.Node
	objects                  []runtime.Object
	resolve                  map[string][]string
}

func defaultLonghornCluster() longhornCluster {
	return longhornCluster{
		storage: installer.StorageLonghorn, profile: "small", backup: installer.BackupExternal,
		bsl: "https://s3.example.com", version: "v1.7.3",
		nodes:   []*corev1.Node{longhornNode("server-1", "Ubuntu 24.04.3 LTS", "6.8.0-85-generic")},
		resolve: map[string][]string{"s3.example.com": {"198.51.100.7", "2001:db8::7"}},
	}
}

// useResolver replaces backup DNS lookups for the test and restores them at
// cleanup. Hosts absent from c.resolve fail to resolve.
func (c longhornCluster) useResolver(t *testing.T) {
	saved := lookupBackupHost
	t.Cleanup(func() { lookupBackupHost = saved })
	lookupBackupHost = func(_ context.Context, host string) ([]net.IP, error) {
		addrs, ok := c.resolve[host]
		if !ok {
			return nil, errors.New("no such host")
		}
		var ips []net.IP
		for _, a := range addrs {
			ips = append(ips, net.ParseIP(a))
		}
		return ips, nil
	}
}

func longhornNode(name, osImage, kernel string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{
		KubeletVersion: "v1.36.2+k3s1", OSImage: osImage, KernelVersion: kernel,
	}}}
}

func (c longhornCluster) clients() (*fake.Clientset, *dynamicfake.FakeDynamicClient) {
	kube := []runtime.Object{&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "velero"}}}
	for _, n := range c.nodes {
		kube = append(kube, n)
	}
	kube = append(kube, c.objects...)
	if c.record != nil {
		kube = append(kube, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: installer.NodeRecordsNamespace, Name: installer.LonghornHopRecordName}, Data: c.record})
	}
	var dyn []runtime.Object
	if c.bsl != "-" {
		dyn = append(dyn, backupLocation(c.bsl))
	}
	if c.version != "" {
		kube = append(kube, &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: "longhorn-system", Name: "longhorn-manager"},
			Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "longhorn-manager", Image: "longhornio/longhorn-manager:" + c.version},
			}}}},
		})
		dyn = append(dyn,
			&unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "longhorn.io/v1beta2", "kind": "Setting",
				"metadata": map[string]interface{}{"name": "current-longhorn-version", "namespace": "longhorn-system"},
				"value":    c.version,
			}},
			&unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
				"metadata": map[string]interface{}{"name": "volumes.longhorn.io"},
			}},
		)
	}
	client := platformConfigClient(c.storage, dyn...)
	pc, _ := client.Resource(platformConfigGVR).Get(context.Background(), "platform", metav1.GetOptions{})
	_ = unstructured.SetNestedField(pc.Object, c.profile, "spec", "profile")
	_ = unstructured.SetNestedField(pc.Object, c.backup, "spec", "backup")
	_, _ = client.Resource(platformConfigGVR).Update(context.Background(), pc, metav1.UpdateOptions{})
	return fake.NewClientset(kube...), client
}

func (c longhornCluster) report(t *testing.T, localExternal, k3sFirst bool) longhornReport {
	c.useResolver(t)
	cs, dyn := c.clients()
	r, err := readLonghornReport(context.Background(), cs, dyn, localExternal, k3sFirst)
	require.NoError(t, err)
	return r
}

func TestLonghornReportPlansTheHopsFromTheInstalledRelease(t *testing.T) {
	r := defaultLonghornCluster().report(t, true, false)

	assert.True(t, r.Longhorn)
	assert.Empty(t, r.Problems)
	assert.Equal(t, []string{"v1.8.2", "v1.9.2", "v1.10.2", "v1.11.3"}, r.Plan.Hops)

	var out bytes.Buffer
	require.NoError(t, r.print(&out, true))
	assert.Contains(t, out.String(), "✔  Longhorn: kip upgrade --longhorn moves it v1.7.3 → v1.8.2 → v1.9.2 → v1.10.2 → v1.11.3")
}

func TestLonghornReportAtTheTarget(t *testing.T) {
	c := defaultLonghornCluster()
	c.version = installer.LonghornTarget()
	r := c.report(t, true, false)

	var out bytes.Buffer
	require.NoError(t, r.print(&out, true))
	assert.Contains(t, out.String(), "runs "+installer.LonghornTarget()+", the release this kip installs")
}

func TestLonghornReportRefusesBackupsThatStayOnTheCluster(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*longhornCluster)
		local  bool
		want   string
	}{
		"in-cluster MinIO":    {func(c *longhornCluster) { c.backup, c.bsl = installer.BackupInCluster, "http://minio.velero.svc:9000" }, false, "in-cluster MinIO"},
		"no backup location":  {func(c *longhornCluster) { c.backup, c.bsl = installer.BackupInCluster, "-" }, false, "in-cluster MinIO"},
		"backups off":         {func(c *longhornCluster) { c.backup = installer.BackupNone }, false, "takes no backups"},
		"no local settings":   {func(c *longhornCluster) {}, false, "connection settings are missing"},
		"legacy, MinIO":       {func(c *longhornCluster) { c.backup, c.bsl = "", "http://minio.velero.svc:9000" }, false, "in-cluster MinIO"},
		"MinIO by full name":  {func(c *longhornCluster) { c.bsl = "http://minio.velero.svc.cluster.local:9000/" }, true, "in-cluster MinIO"},
		"service.namespace":   {func(c *longhornCluster) { c.bsl = "http://minio.velero:9000" }, true, "in-cluster MinIO"},
		"another in-cluster":  {func(c *longhornCluster) { c.bsl = "http://Seaweed.storage.SVC.:8333" }, true, "in-cluster MinIO"},
		"legacy, no location": {func(c *longhornCluster) { c.backup, c.bsl = "", "-" }, true, "in-cluster MinIO"},
	} {
		c := defaultLonghornCluster()
		tc.change(&c)
		r := c.report(t, tc.local, false)
		assert.Contains(t, strings.Join(r.Problems, "\n"), tc.want, name)
		if tc.want == "in-cluster MinIO" || tc.want == "takes no backups" {
			assert.Contains(t, strings.Join(r.Problems, "\n"), "--backup-storage-bucket", name)
		}

		var out bytes.Buffer
		assert.Error(t, r.print(&out, true), name)
		assert.Contains(t, out.String(), "✗  Longhorn: ", name)
		out.Reset()
		assert.NoError(t, r.print(&out, false), "without --longhorn the check only warns: "+name)
		assert.Contains(t, out.String(), "⚠  Longhorn: ", name)
	}
}

// A backup endpoint given by address or by a public name can still be a
// service inside the cluster. kip resolves it and compares every address with
// the cluster's own.
func TestLonghornReportRefusesABackupEndpointThatResolvesIntoTheCluster(t *testing.T) {
	node := longhornNode("server-1", "Ubuntu 24.04.3 LTS", "6.8.0-85-generic")
	node.Spec.PodCIDRs = []string{"10.52.0.0/24"}
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.0.2.10"}, {Type: corev1.NodeExternalIP, Address: "203.0.113.10"}}
	minio := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "velero", Name: "minio"}, Spec: corev1.ServiceSpec{ClusterIP: "10.53.0.20", ClusterIPs: []string{"10.53.0.20"}, ExternalIPs: []string{"198.51.100.30"}}}
	slice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: "velero", Name: "minio-abc", Labels: map[string]string{discoveryv1.LabelServiceName: "minio"}},
		Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"10.60.1.5"}}}}
	// Sharing the node's address models a host-network pod and checks that the node keeps ownership.
	exporter := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: "monitoring", Name: "node-exporter-abc", Labels: map[string]string{discoveryv1.LabelServiceName: "node-exporter"}},
		Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"192.0.2.10"}}}}
	serviceCIDR := &networkingv1.ServiceCIDR{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes"}, Spec: networkingv1.ServiceCIDRSpec{CIDRs: []string{"10.53.0.0/16"}}}

	for name, tc := range map[string]struct {
		bsl, want string
	}{
		"pod range by address":          {"http://10.52.0.9:9000", "pod range"},
		"service range by address":      {"http://10.53.4.4:9000", "service range"},
		"ClusterIP by address":          {"http://10.53.0.20:9000", "service velero/minio"},
		"external IP of a service":      {"http://198.51.100.30:9000", "service velero/minio"},
		"endpoint of a service":         {"http://10.60.1.5:9000", "service velero/minio"},
		"node internal address":         {"https://192.0.2.10:9000", "node server-1"},
		"node external address":         {"https://203.0.113.10", "node server-1"},
		"loopback":                      {"http://127.0.0.1:9000", "loopback"},
		"IPv6 loopback":                 {"http://[::1]:9000", "loopback"},
		"link-local":                    {"http://169.254.10.1:9000", "link-local"},
		"public name of the node":       {"https://backups.example.net", "node server-1"},
		"one of several names inside":   {"https://mixed.example.net", "node server-1"},
		"name that does not resolve":    {"https://gone.example.net", "cannot resolve"},
		"malformed endpoint":            {"http://[::1", "cannot read"},
		"public name resolving nowhere": {"https://empty.example.net", "cannot resolve"},
	} {
		c := defaultLonghornCluster()
		c.bsl = tc.bsl
		c.nodes = []*corev1.Node{node}
		c.objects = []runtime.Object{minio, slice, exporter, serviceCIDR}
		c.resolve = map[string][]string{
			"backups.example.net": {"203.0.113.10"},
			"mixed.example.net":   {"198.51.100.7", "192.0.2.10"},
			"empty.example.net":   {},
		}
		problems := strings.Join(c.report(t, true, false).Problems, "\n")
		assert.Contains(t, problems, tc.want, name)
		assert.Contains(t, problems, "--backup-storage-bucket", name)
		assert.NotContains(t, problems, "location 192.0.2.10 points", name)
	}

	c := defaultLonghornCluster()
	c.nodes = []*corev1.Node{node}
	c.objects = []runtime.Object{minio, slice, serviceCIDR}
	assert.Empty(t, c.report(t, true, false).Problems, "an endpoint outside every cluster range passes")
}

func TestLonghornReportRefusesAMalformedBackupLocation(t *testing.T) {
	c := defaultLonghornCluster()
	c.useResolver(t)
	cs, dyn := c.clients()
	bsl := backupLocation("")
	bsl.Object["spec"] = map[string]interface{}{"config": map[string]interface{}{"s3Url": int64(9000)}}
	_, err := dyn.Resource(backupStorageLocationGVR).Namespace("velero").Update(context.Background(), bsl, metav1.UpdateOptions{})
	require.NoError(t, err)

	r, err := readLonghornReport(context.Background(), cs, dyn, true, false)
	require.NoError(t, err)
	problems := strings.Join(r.Problems, "\n")
	assert.Contains(t, problems, "cannot read the endpoint of Velero's backup location")
	assert.Contains(t, problems, "--backup-storage-bucket")
}

// The location can change during the run, so every step checks it again
// before its backup.
func TestLonghornBackupChecksTheLocationBeforeEachStep(t *testing.T) {
	c := defaultLonghornCluster()
	c.useResolver(t)
	cs, dyn := c.clients()
	var taken []string
	backup := longhornBackup(cs, dyn, func(_ context.Context, name string) error {
		taken = append(taken, name)
		return nil
	})

	name, err := backup("v1.8.2")
	require.NoError(t, err)
	assert.Regexp(t, `^before-longhorn-v1-8-2-\d{8}-\d{6}$`, name)

	_, err = dyn.Resource(backupStorageLocationGVR).Namespace("velero").Update(context.Background(), backupLocation("http://127.0.0.1:9000"), metav1.UpdateOptions{})
	require.NoError(t, err)
	_, err = backup("v1.9.2")
	assert.ErrorContains(t, err, "loopback")
	assert.ErrorContains(t, err, "--backup-storage-bucket")
	assert.Len(t, taken, 1, "no backup is taken into the cluster")
}

// A step's backup copies files from running pods only, so a volume nothing
// mounts would be missing from the backup the step relies on.
func TestLonghornRefusesAStepWhoseBackupWouldOmitAVolume(t *testing.T) {
	c := defaultLonghornCluster()
	c.objects = []runtime.Object{&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "uploads"}}}
	problems := strings.Join(c.report(t, true, false).Problems, "\n")
	assert.Contains(t, problems, "shop/uploads")
	assert.Contains(t, problems, "kip app start")
	assert.Contains(t, problems, "kip service start", "a service the fence stopped leaves its volume unmounted too")

	cs, dyn := c.clients()
	taken := 0
	_, err := longhornBackup(cs, dyn, func(context.Context, string) error { taken++; return nil })("v1.8.2")
	assert.ErrorContains(t, err, "shop/uploads")
	assert.Zero(t, taken)
}

// AWS S3 needs no s3Url, so a location without one is external.
func TestLonghornReportAcceptsExternalBackupsWithoutAnEndpoint(t *testing.T) {
	c := defaultLonghornCluster()
	c.bsl = ""
	assert.Empty(t, c.report(t, true, false).Problems)
}

func TestLonghornReportHoldsRHEL97LevelNodes(t *testing.T) {
	c := defaultLonghornCluster()
	c.nodes = append(c.nodes, longhornNode("worker-1", "Rocky Linux 9.8 (Blue Onyx)", "5.14.0-687.41.1.el9_8.x86_64"))
	r := c.report(t, true, false)

	require.Len(t, r.Held, 1)
	assert.Contains(t, r.Held[0], "node worker-1")
	assert.Contains(t, r.Problems, r.Held[0])

	cs, dyn := c.clients()
	var out bytes.Buffer
	cluster := &config.Cluster{Name: "lab", BackupStorage: &config.BackupStorageRef{Mode: "external", Bucket: "b"}}
	got, err := preflightLonghorn(context.Background(), cs, dyn, &out, cluster, false, false)
	require.NoError(t, err, "a plain upgrade warns and carries on")
	assert.Equal(t, r.Held, got.Held)
	assert.Contains(t, out.String(), "⚠  Longhorn host: node worker-1")

	out.Reset()
	_, err = preflightLonghorn(context.Background(), cs, dyn, &out, cluster, false, true)
	assert.ErrorContains(t, err, "Longhorn was not changed")
}

func TestLonghornReportChecksKubernetesFloorsUnlessK3sMovesFirst(t *testing.T) {
	c := defaultLonghornCluster()
	c.nodes[0].Status.NodeInfo.KubeletVersion = "v1.33.5+k3s1"

	assert.Contains(t, strings.Join(c.report(t, true, false).Problems, "\n"), "Run 'kip upgrade --k3s' first")
	assert.Empty(t, c.report(t, true, true).Problems)
}

func TestLonghornReportNamesAnUnfinishedStep(t *testing.T) {
	c := defaultLonghornCluster()
	c.version = "v1.8.2"
	c.record = map[string]string{"from": "v1.8.2", "to": "v1.9.2", "backup": "before-longhorn-v1-9-2", "export": "/home/op/.kip/x"}
	r := c.report(t, true, false)

	assert.Equal(t, "v1.8.2", r.Plan.From, "a step never applied starts from where Longhorn is")
	var out bytes.Buffer
	require.NoError(t, r.print(&out, true))
	assert.Contains(t, out.String(), "the step from v1.8.2 to v1.9.2 was recorded but never applied, and kip upgrade --longhorn starts it again with a new backup")
	out.Reset()
	longhornStep{Report: r}.preview(&out)
	assert.Contains(t, out.String(), "Longhorn stays at v1.8.2")

	c.version = "v1.9.2"
	r = c.report(t, true, false)
	assert.Equal(t, "v1.9.2", r.Plan.From, "the next run finishes the step it started")
	out.Reset()
	require.NoError(t, r.print(&out, true))
	assert.Contains(t, out.String(), "the step from v1.8.2 to v1.9.2 did not finish, and kip upgrade --longhorn resumes it")
	out.Reset()
	longhornStep{Report: r}.preview(&out)
	assert.Contains(t, out.String(), "Longhorn re-applies v1.9.2")
}

func TestLonghornReportOnLocalStorage(t *testing.T) {
	c := defaultLonghornCluster()
	c.storage, c.version = installer.StorageLocal, ""
	r := c.report(t, false, false)
	assert.False(t, r.Longhorn)

	var out bytes.Buffer
	require.NoError(t, r.print(&out, false))
	assert.Empty(t, out.String())
	assert.Error(t, r.print(&out, true))
	assert.Contains(t, out.String(), "local storage")
}

func TestLonghornReportRefusesAReleaseItCannotPlace(t *testing.T) {
	c := defaultLonghornCluster()
	c.version = "v1.13.0"
	assert.Contains(t, strings.Join(c.report(t, true, false).Problems, "\n"), "newer than")
}

func TestLonghornFlagNeedsTheSystemStep(t *testing.T) {
	assert.Error(t, checkLonghornFlags(true, true))
	assert.NoError(t, checkLonghornFlags(true, false))
	assert.NoError(t, checkLonghornFlags(false, true))
}

func TestLonghornStepReplacesOnlyTheLonghornComponent(t *testing.T) {
	ran := ""
	components := []installer.SystemComponent{
		{Name: "traefik", Apply: func(installer.CommandRunner) error { ran += "traefik "; return nil }},
		{Name: "longhorn", Apply: func(installer.CommandRunner) error { ran += "re-apply "; return nil }},
	}
	out := longhornStep{}.apply(components)
	require.NoError(t, out[1].Apply(&leaseServer{answer: leaseAcquired}))
	assert.Equal(t, "re-apply ", ran)

	out = longhornStep{Upgrade: &installer.LonghornUpgrade{}}.apply([]installer.SystemComponent{components[0], {Name: "longhorn", Apply: func(installer.CommandRunner) error { ran += "re-apply "; return nil }}})
	require.NoError(t, out[0].Apply(nil))
	assert.ErrorContains(t, out[1].Apply(&leaseServer{answer: leaseAcquired}), "Longhorn is not installed", "the upgrade runs in place of the re-apply")
	assert.Equal(t, "re-apply traefik ", ran)
}

const leaseAcquired = "kipper-node-lease-acquired 7 0123456789abcdef0123456789abcdef"

// leaseServer answers the node lifecycle lease with answer and records the
// commands sent through it.
type leaseServer struct {
	answer   string
	commands []string
}

func (s *leaseServer) Run(command string) (string, error) {
	s.commands = append(s.commands, command)
	return "", nil
}
func (s *leaseServer) RunStdin(command string, _ io.Reader) (string, error) { return s.Run(command) }
func (s *leaseServer) Start(string) (*ssh.Held, error) {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	go func() {
		_, _ = io.WriteString(stdoutW, s.answer+"\n")
		_, _ = io.Copy(io.Discard, stdinR)
		_ = stdoutW.Close()
	}()
	return &ssh.Held{Stdin: stdinW, Stdout: stdoutR, Wait: func() error { return nil }}, nil
}
func (*leaseServer) SurvivesSSHRateLimit() bool             { return true }
func (*leaseServer) ConfirmFreshConnection() error          { return nil }
func (*leaseServer) ObservedAddress(string) (string, error) { return "", nil }

// Two runs that change Longhorn at once could apply an older manifest over a
// newer step, so each holds the lease kip node commands take.
func TestLonghornStepHoldsTheNodeLease(t *testing.T) {
	for name, step := range map[string]longhornStep{"plain": {}, "--longhorn": {Upgrade: &installer.LonghornUpgrade{}}} {
		server := &leaseServer{answer: leaseAcquired}
		components := step.apply([]installer.SystemComponent{{Name: "longhorn", Apply: func(c installer.CommandRunner) error {
			_, err := c.Run("true")
			return err
		}}})
		// The empty server stops the upgrade early, which is enough to check its commands.
		_ = components[0].Apply(server)
		require.NotEmpty(t, server.commands, name)
		assert.Contains(t, server.commands[0], "kipper-node-lifecycle.owner", name+": commands run inside the lease")
	}
}

func TestLonghornStepRefusesWhileAnotherRunHoldsTheLease(t *testing.T) {
	for name, step := range map[string]longhornStep{"plain": {}, "--longhorn": {Upgrade: &installer.LonghornUpgrade{}}} {
		server := &leaseServer{answer: "kipper-node-lease-held kip upgrade --longhorn"}
		components := step.apply([]installer.SystemComponent{{Name: "longhorn", Apply: func(installer.CommandRunner) error {
			t.Fatal("Longhorn changed without the lease")
			return nil
		}}})
		err := components[0].Apply(server)
		assert.ErrorContains(t, err, "running on this cluster (kip upgrade --longhorn)", name)
		assert.ErrorContains(t, err, "Longhorn was not changed", name)
		assert.Empty(t, server.commands, name)
	}
}

func TestLonghornPreview(t *testing.T) {
	r := defaultLonghornCluster().report(t, true, false)

	var out bytes.Buffer
	longhornStep{Report: r}.preview(&out)
	assert.Contains(t, out.String(), "Longhorn stays at v1.7.3. kip upgrade --longhorn moves it to "+installer.LonghornTarget())

	out.Reset()
	longhornStep{Report: r, Upgrade: &installer.LonghornUpgrade{ExportDir: "/home/op/.kip/longhorn-upgrades/lab"}}.preview(&out)
	for _, want := range []string{
		"Longhorn moves v1.7.3 → v1.8.2",
		"/home/op/.kip/longhorn-upgrades/lab",
		"kip backup repository-key export --to <file>",
		"kip backup sse-key export --to <file>",
		"kip backup drill <namespace>",
	} {
		assert.Contains(t, out.String(), want)
	}

	r.Profile = installer.ProfileNano
	out.Reset()
	longhornStep{Report: r, Upgrade: &installer.LonghornUpgrade{ExportDir: "x"}}.preview(&out)
	assert.Contains(t, out.String(), "separate recovery cluster")
	assert.NotContains(t, out.String(), "kip backup drill <namespace>")

	r = defaultLonghornCluster().report(t, true, false)
	r.Plan = installer.LonghornPlan{From: installer.LonghornTarget()}
	out.Reset()
	longhornStep{Report: r, Upgrade: &installer.LonghornUpgrade{ExportDir: "x"}}.preview(&out)
	assert.Contains(t, out.String(), "already runs")
	assert.Contains(t, out.String(), "kip backup repository-key export", "an engine catch-up changes volumes too")
}

func TestLonghornExportDirIsPerClusterUnderKipHome(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, err := longhornExportDir(&config.Cluster{Name: "lab.example.com"})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join("longhorn-upgrades", "lab.example.com"), filepath.Join(filepath.Base(filepath.Dir(dir)), filepath.Base(dir)))

	dir, err = longhornExportDir(&config.Cluster{Name: "../../etc"})
	require.NoError(t, err)
	assert.Equal(t, "..-..-etc", filepath.Base(dir))
	assert.Equal(t, "longhorn-upgrades", filepath.Base(filepath.Dir(dir)))

	_, err = longhornExportDir(&config.Cluster{Name: "/"})
	assert.Error(t, err)
}

func TestInClusterEndpoint(t *testing.T) {
	for url, want := range map[string]bool{
		"":                       false,
		"https://s3.example.com": false,
		"https://fsn1.your-objectstorage.example":    false,
		"http://minio.velero.svc:9000":               true,
		"http://minio.velero.svc.cluster.local:9000": true,
		"http://minio:9000":                          true,
		"http://minio.velero:9000":                   true,
		"http://MINIO.Velero.:9000":                  true,
		"https://storage.example:443":                false,
		"://":                                        true,
	} {
		assert.Equal(t, want, inClusterEndpoint(url, map[string]bool{"velero": true}), url)
	}
}
