package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/kip/internal/manifest"
)

const storedSentinel = "0123456789abcdef0123456789abcdef0123456789abcdef" //nolint:gosec // a test value, not a credential

type rotationCluster struct {
	cs    *fake.Clientset
	dyn   *dynamicfake.FakeDynamicClient
	execs []execCall
	// A nil or missing entry means the exec call succeeds.
	execErr []error
	out     bytes.Buffer
}

type execCall struct {
	pod   string
	argv  []string
	stdin string
}

func rotationService(svcType string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kipper.run/v1alpha1", "kind": "Service",
		"metadata": map[string]any{"name": "db", "namespace": "shop"},
		"spec":     map[string]any{"type": svcType},
	}}
	u.SetUID("uid-db")
	return u
}

func rotationWorkload(kind, name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kipper.run/v1alpha1", "kind": kind,
		"metadata": map[string]any{"name": name, "namespace": "shop"},
		"spec":     spec,
	}}
}

func serviceStatefulSet() *appsv1.StatefulSet {
	return &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: "db", Namespace: "shop", UID: "uid-db-sts",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "kipper.run/v1alpha1", Kind: "Service", Name: "db", UID: "uid-db", Controller: new(true)}},
	}}
}

func readyPod(name, stamp string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "shop", Labels: map[string]string{"app": "db"}, Annotations: map[string]string{"kipper.run/restartedAt": stamp},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "db", UID: "uid-db-sts", Controller: new(true)}},
		},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "db"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// pollerDeployment models an event Function linked to the service only through
// its trigger. The poller reads its source URL from a Secret, so credential
// changes must be detected through that reference rather than a binding hash.
func pollerDeployment(name string) *appsv1.Deployment {
	d := settledDeployment(name, "")
	d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "kipper-poll", Env: []corev1.EnvVar{{
		Name: "KIPPER_SOURCE_URL", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "function-" + name + "-poll-source-old"}, Key: "KIPPER_SOURCE_URL",
		}},
	}}}}
	return d
}

func pollSourceRef(c *corev1.Container) *corev1.SecretKeySelector {
	for i := range c.Env {
		if c.Env[i].Name == "KIPPER_SOURCE_URL" && c.Env[i].ValueFrom != nil {
			return c.Env[i].ValueFrom.SecretKeyRef
		}
	}
	return nil
}

func settledDeployment(name, hash string) *appsv1.Deployment {
	one := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", Generation: 1},
		Spec: appsv1.DeploymentSpec{Replicas: &one, Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"kipper.run/binding-credentials": hash}},
		}},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1},
	}
}

func newRotationCluster(svcType string, data map[string][]byte, extra ...k8sruntime.Object) *rotationCluster {
	svc := rotationService(svcType)
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: "shop", OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "kipper.run/v1alpha1", Kind: "Service", Name: "db", UID: "uid-db", Controller: new(true),
		}}},
		Data: data,
	}
	objects := append([]k8sruntime.Object{creds, readyPod("db-0", "old"), serviceStatefulSet()}, extra...)
	c := &rotationCluster{cs: fake.NewSimpleClientset(objects...)}
	c.dyn = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(k8sruntime.NewScheme(), map[schema.GroupVersionResource]string{
		manifest.ServiceGVR: "ServiceList", manifest.AppGVR: "AppList", manifest.FunctionGVR: "FunctionList", veleroBackupGVR: "BackupList",
	}, svc)
	return c
}

func (c *rotationCluster) add(t *testing.T, gvr schema.GroupVersionResource, namespace string, obj *unstructured.Unstructured) {
	t.Helper()
	_, err := c.dyn.Resource(gvr).Namespace(namespace).Create(context.Background(), obj, metav1.CreateOptions{})
	require.NoError(t, err)
}

// restart simulates the controllers: it stamps the StatefulSet, replaces the pod
// and re-renders every bound child with the new credentials.
func (c *rotationCluster) restart(ctx context.Context) (string, error) {
	const stamp = "2026-10-22T02:00:00Z"
	sts, err := c.cs.AppsV1().StatefulSets("shop").Get(ctx, "db", metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	sts.Spec.Template.Annotations = map[string]string{"kipper.run/restartedAt": stamp}
	if _, err := c.cs.AppsV1().StatefulSets("shop").Update(ctx, sts, metav1.UpdateOptions{}); err != nil {
		return "", err
	}
	_ = c.cs.CoreV1().Pods("shop").Delete(ctx, "db-0", metav1.DeleteOptions{})
	if _, err := c.cs.CoreV1().Pods("shop").Create(ctx, readyPod("db-0", stamp), metav1.CreateOptions{}); err != nil {
		return "", err
	}
	deps, _ := c.cs.AppsV1().Deployments("shop").List(ctx, metav1.ListOptions{})
	for i := range deps.Items {
		d := &deps.Items[i]
		if d.Spec.Template.Annotations["kipper.run/binding-credentials"] != "" {
			d.Spec.Template.Annotations = map[string]string{"kipper.run/binding-credentials": "new"}
		}
		for j := range d.Spec.Template.Spec.Containers {
			if ref := pollSourceRef(&d.Spec.Template.Spec.Containers[j]); ref != nil {
				ref.Name = "function-" + d.Name + "-poll-source-new"
			}
		}
		d.Generation, d.Status.ObservedGeneration = 2, 2
		_, _ = c.cs.AppsV1().Deployments("shop").Update(ctx, d, metav1.UpdateOptions{})
	}
	cjs, _ := c.cs.BatchV1().CronJobs("shop").List(ctx, metav1.ListOptions{})
	for i := range cjs.Items {
		cjs.Items[i].Spec.JobTemplate.Spec.Template.Spec.Containers = []corev1.Container{{Name: "report", EnvFrom: []corev1.EnvFromSource{{
			SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "function-report-env-new"}},
		}}}}
		_, _ = c.cs.BatchV1().CronJobs("shop").Update(ctx, &cjs.Items[i], metav1.UpdateOptions{})
	}
	return stamp, nil
}

func (c *rotationCluster) rotation() *passwordRotation {
	return &passwordRotation{
		clientset: c.cs,
		dynamic:   c.dyn,
		namespace: "shop",
		service:   "db",
		out:       &c.out,
		execIn: func(pod, _ string) podExecFunc {
			return func(_ context.Context, argv []string, stdin io.Reader, _, stderr io.Writer) error {
				in, _ := io.ReadAll(stdin)
				c.execs = append(c.execs, execCall{pod: pod, argv: argv, stdin: string(in)})
				if n := len(c.execs) - 1; n < len(c.execErr) && c.execErr[n] != nil {
					_, _ = fmt.Fprintf(stderr, "engine said: %s", in)
					return c.execErr[n]
				}
				return nil
			}
		},
		restart:      c.restart,
		pauseTuning:  func(context.Context) (func(), error) { return func() {}, nil },
		confirm:      func(string) bool { return true },
		now:          func() time.Time { return time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC) },
		stampWait:    time.Second,
		serviceWait:  time.Second,
		workloadWait: time.Second,
		poll:         time.Millisecond,
	}
}

func (c *rotationCluster) secret(t *testing.T, name string) *corev1.Secret {
	t.Helper()
	s, err := c.cs.CoreV1().Secrets("shop").Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return s
}

func (c *rotationCluster) hasRecord() bool {
	_, err := c.cs.CoreV1().Secrets("shop").Get(context.Background(), "db-password-rotation", metav1.GetOptions{})
	return err == nil
}

func postgresData() map[string][]byte {
	return map[string][]byte{"USERNAME": []byte("kipper"), "PASSWORD": []byte(storedSentinel), "NAME": []byte("app")}
}

func TestRotationMovesTheEngineTheSecretAndEveryBoundWorkload(t *testing.T) {
	c := newRotationCluster("postgres", postgresData(),
		settledDeployment("api", "old"),
		pollerDeployment("events"),
		&batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "report-cron", Namespace: "shop"}})
	c.add(t, manifest.AppGVR, "shop", rotationWorkload("App", "api", map[string]any{"serviceBindings": []any{map[string]any{"name": "db"}}}))
	c.add(t, manifest.FunctionGVR, "shop", rotationWorkload("Function", "events", map[string]any{"triggers": []any{
		map[string]any{"type": "postgres", "config": map[string]any{"source": "db"}},
	}}))
	c.add(t, manifest.FunctionGVR, "shop", rotationWorkload("Function", "report", map[string]any{
		"serviceBindings": []any{map[string]any{"name": "db"}},
		"triggers":        []any{map[string]any{"type": "cron", "schedule": "@daily"}},
	}))

	require.NoError(t, c.rotation().run(context.Background()))

	require.Len(t, c.execs, 1)
	assert.Equal(t, "psql", c.execs[0].argv[0])
	next := strings.SplitN(c.execs[0].stdin, "\n", 2)[0]
	assert.Regexp(t, `^[0-9a-f]{48}$`, next)
	assert.Equal(t, next, string(c.secret(t, "db-credentials").Data["PASSWORD"]))
	assert.False(t, c.hasRecord(), "a finished rotation leaves no record")
	out := c.out.String()
	assert.Contains(t, out, "app api picked up the new password")
	assert.Contains(t, out, "function events picked up the new password", "an event source rolls through the poller's URL, not the binding hash")
	assert.Contains(t, out, "function report uses the new password from its next scheduled run")
	assert.Contains(t, out, "function report picked up the new password")
	assert.NotContains(t, out, next)
	assert.NotContains(t, out, storedSentinel)
}

func TestAnEngineFailureKeepsTheRecordForTheNextRun(t *testing.T) {
	c := newRotationCluster("postgres", postgresData())
	c.execErr = []error{errors.New("stream closed")}

	err := c.rotation().run(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "run kip service rotate-password db again")
	assert.NotContains(t, err.Error(), storedSentinel)
	assert.Contains(t, err.Error(), "[password]", "engine output is printed with the passwords taken out")
	assert.Equal(t, storedSentinel, string(c.secret(t, "db-credentials").Data["PASSWORD"]))
	record := c.secret(t, "db-password-rotation")
	assert.NotContains(t, err.Error(), string(record.Data["PASSWORD"]))
	assert.Empty(t, record.Annotations["kipper.run/rotation-holder"], "the lease is released so a rerun need not wait")
}

func TestAResumedMySQLRotationTriesTheNewPasswordFirst(t *testing.T) {
	const pending = "fedcba9876543210fedcba9876543210fedcba9876543210" //nolint:gosec // a test value, not a credential
	record := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-password-rotation", Namespace: "shop"},
		Data:       map[string][]byte{"PASSWORD": []byte(pending)},
	}
	c := newRotationCluster("mysql", postgresData(), record)
	c.execErr = []error{errors.New("access denied")}

	require.NoError(t, c.rotation().run(context.Background()))

	require.Len(t, c.execs, 2)
	assert.True(t, strings.HasPrefix(c.execs[0].stdin, pending+"\n"), "the engine may already hold the staged password")
	assert.True(t, strings.HasPrefix(c.execs[1].stdin, storedSentinel+"\n"))
	assert.Equal(t, pending, string(c.secret(t, "db-credentials").Data["PASSWORD"]))
	assert.Contains(t, c.out.String(), "Finishing the rotation an earlier run started")
}

func TestRotationWaitsForABackupOfTheNamespace(t *testing.T) {
	c := newRotationCluster("postgres", postgresData())
	backup := func(name, phase string, namespaces ...any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "velero.io/v1", "kind": "Backup",
			"metadata": map[string]any{"name": name, "namespace": "velero"},
			"spec":     map[string]any{"includedNamespaces": namespaces},
			"status":   map[string]any{"phase": phase},
		}}
	}
	c.add(t, veleroBackupGVR, "velero", backup("other", "InProgress", "elsewhere"))
	c.add(t, veleroBackupGVR, "velero", backup("done", "Completed", "shop"))
	require.NoError(t, c.rotation().refuseDuringBackup(context.Background()))

	c.add(t, veleroBackupGVR, "velero", backup("nightly", "InProgress", "*"))
	err := c.rotation().run(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "nightly")
	assert.Empty(t, c.execs)
	assert.False(t, c.hasRecord())
}

func TestMinIORotatesTheSecretKeyWithoutAnEngineStep(t *testing.T) {
	c := newRotationCluster("minio", map[string][]byte{"ACCESS_KEY": []byte("kipper"), "SECRET_KEY": []byte(storedSentinel)})

	require.NoError(t, c.rotation().run(context.Background()))

	assert.Empty(t, c.execs)
	key := string(c.secret(t, "db-credentials").Data["SECRET_KEY"])
	assert.Regexp(t, `^[0-9a-f]{48}$`, key)
	assert.NotEqual(t, storedSentinel, key)
	assert.Contains(t, c.out.String(), "MinIO takes it when its pod restarts")
	assert.NotContains(t, c.out.String(), "changed in the engine")
}

func TestRotationRefusesCredentialsTheServiceDoesNotOwn(t *testing.T) {
	c := newRotationCluster("postgres", postgresData())
	creds := c.secret(t, "db-credentials")
	creds.OwnerReferences[0].UID = "uid-someone-else"
	_, err := c.cs.CoreV1().Secrets("shop").Update(context.Background(), creds, metav1.UpdateOptions{})
	require.NoError(t, err)

	err = c.rotation().run(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--repair")
	assert.Empty(t, c.execs)
}

func TestDecliningTheRotationChangesNothing(t *testing.T) {
	c := newRotationCluster("postgres", postgresData())
	r := c.rotation()
	r.confirm = func(string) bool { return false }

	require.NoError(t, r.run(context.Background()))

	assert.Empty(t, c.execs)
	assert.False(t, c.hasRecord())
	assert.Equal(t, storedSentinel, string(c.secret(t, "db-credentials").Data["PASSWORD"]))
}

func TestAWorkloadThatNeverRollsIsNamedWhenTheWaitRunsOut(t *testing.T) {
	c := newRotationCluster("postgres", postgresData(), settledDeployment("api", "old"))
	c.add(t, manifest.AppGVR, "shop", rotationWorkload("App", "api", map[string]any{"serviceBindings": []any{map[string]any{"name": "db"}}}))
	r := c.rotation()
	r.restart = func(ctx context.Context) (string, error) {
		stamp, err := c.restart(ctx)
		// Undo the re-render: the controller never got to this app.
		dep, _ := c.cs.AppsV1().Deployments("shop").Get(ctx, "api", metav1.GetOptions{})
		dep.Spec.Template.Annotations["kipper.run/binding-credentials"] = "old"
		_, _ = c.cs.AppsV1().Deployments("shop").Update(ctx, dep, metav1.UpdateOptions{})
		return stamp, err
	}
	r.workloadWait = 50 * time.Millisecond

	err := r.run(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "app api")
	assert.Contains(t, err.Error(), "The password is rotated")
}

func TestAnUnrelatedRolloutDoesNotCountAsTakingTheNewPassword(t *testing.T) {
	c := newRotationCluster("postgres", postgresData(), settledDeployment("api", "old"))
	c.add(t, manifest.AppGVR, "shop", rotationWorkload("App", "api", map[string]any{"serviceBindings": []any{map[string]any{"name": "db"}}}))
	r := c.rotation()
	r.restart = func(ctx context.Context) (string, error) {
		stamp, err := c.restart(ctx)
		dep, _ := c.cs.AppsV1().Deployments("shop").Get(ctx, "api", metav1.GetOptions{})
		dep.Spec.Template.Annotations = map[string]string{"kipper.run/binding-credentials": "old", "kipper.run/restartedAt": "unrelated"}
		_, _ = c.cs.AppsV1().Deployments("shop").Update(ctx, dep, metav1.UpdateOptions{})
		return stamp, err
	}
	r.workloadWait = 50 * time.Millisecond

	err := r.run(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "app api")
	assert.NotContains(t, c.out.String(), "app api picked up the new password")
}

func TestRabbitMQRotatesThroughTheManagementAPIWithoutAnExec(t *testing.T) {
	c := newRotationCluster("rabbitmq", map[string][]byte{"USERNAME": []byte("kipper"), "PASSWORD": []byte(storedSentinel)})
	var put map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pass, _ := r.BasicAuth(); pass != storedSentinel {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPut {
			_ = json.NewDecoder(r.Body).Decode(&put)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte(`{"tags":["administrator"]}`))
	}))
	defer srv.Close()
	stopped := false
	r := c.rotation()
	r.forward = func(_ context.Context, pod string, port int) (string, func(), error) {
		assert.Equal(t, "db-0", pod)
		assert.Equal(t, 15672, port)
		return srv.URL, func() { stopped = true }, nil
	}

	require.NoError(t, r.run(context.Background()))

	assert.Empty(t, c.execs, "rabbitmqctl would start a second Erlang VM in the broker's container")
	assert.Equal(t, put["password"], string(c.secret(t, "db-credentials").Data["PASSWORD"]))
	assert.Equal(t, "administrator", put["tags"])
	assert.True(t, stopped, "the port-forward is closed")
}

// stampOnly restarts the service without re-rendering any bound workload.
func (c *rotationCluster) stampOnly(ctx context.Context) (string, error) {
	const stamp = "2026-10-22T02:00:00Z"
	sts, _ := c.cs.AppsV1().StatefulSets("shop").Get(ctx, "db", metav1.GetOptions{})
	sts.Spec.Template.Annotations = map[string]string{"kipper.run/restartedAt": stamp}
	_, _ = c.cs.AppsV1().StatefulSets("shop").Update(ctx, sts, metav1.UpdateOptions{})
	_ = c.cs.CoreV1().Pods("shop").Delete(ctx, "db-0", metav1.DeleteOptions{})
	_, err := c.cs.CoreV1().Pods("shop").Create(ctx, readyPod("db-0", stamp), metav1.CreateOptions{})
	return stamp, err
}

func TestResumingACommittedRotationDoesNotWaitForAChangeAlreadyMade(t *testing.T) {
	record := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-password-rotation", Namespace: "shop"},
		Data:       map[string][]byte{"PASSWORD": []byte(storedSentinel)},
	}
	c := newRotationCluster("postgres", postgresData(), record, settledDeployment("api", "already-new"))
	c.add(t, manifest.AppGVR, "shop", rotationWorkload("App", "api", map[string]any{"serviceBindings": []any{map[string]any{"name": "db"}}}))
	r := c.rotation()
	r.restart = c.stampOnly
	r.workloadWait = 50 * time.Millisecond

	require.NoError(t, r.run(context.Background()))

	assert.Contains(t, c.out.String(), "already stored this password")
	assert.False(t, c.hasRecord())
}

func TestCtrlCDuringTheCommitStillRequestsTheServiceRestart(t *testing.T) {
	c := newRotationCluster("minio", map[string][]byte{"ACCESS_KEY": []byte("kipper"), "SECRET_KEY": []byte(storedSentinel)}, settledDeployment("api", "old"))
	c.add(t, manifest.AppGVR, "shop", rotationWorkload("App", "api", map[string]any{"serviceBindings": []any{map[string]any{"name": "db"}}}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.cs.PrependReactor("update", "secrets", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		if a.(k8stesting.UpdateAction).GetObject().(*corev1.Secret).Name == "db-credentials" {
			cancel() // the operator presses Ctrl-C while the commit is in flight
		}
		return false, nil, nil
	})
	restarted := false
	r := c.rotation()
	r.restart = func(rctx context.Context) (string, error) {
		restarted = rctx.Err() == nil
		return c.stampOnly(rctx)
	}

	err := r.run(ctx)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "interrupted after the new password was stored", "not a deadline that never passed")
	assert.True(t, restarted, "MinIO takes the new key only on a restart, so the request must survive Ctrl-C")
	assert.NotEqual(t, storedSentinel, string(c.secret(t, "db-credentials").Data["SECRET_KEY"]))
}

// The detached restart request can still fail. Ctrl-C at the same moment must
// not turn that failure into a generic interruption, because the operator then
// never learns that the service still needs a restart.
func TestAFailedRestartRequestIsReportedEvenAfterCtrlC(t *testing.T) {
	c := newRotationCluster("minio", map[string][]byte{"ACCESS_KEY": []byte("kipper"), "SECRET_KEY": []byte(storedSentinel)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := c.rotation()
	r.restart = func(context.Context) (string, error) {
		cancel()
		return "", errors.New("the API server did not answer")
	}

	err := r.run(ctx)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "run kip service restart db")
	assert.Contains(t, err.Error(), "keeps accepting the old one")
	assert.NotContains(t, err.Error(), "interrupted")
}

// The commit retries on a conflict, and every retry checks that the lease is
// still this run's. Renewing the lease right before the commit gives those
// retries a full lease, however long the engine change took.
func TestTheLeaseIsRenewedBeforeTheCommit(t *testing.T) {
	c := newRotationCluster("postgres", postgresData())
	clock := time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)
	r := c.rotation()
	r.now = func() time.Time { return clock }
	engine := r.execIn
	r.execIn = func(pod, container string) podExecFunc {
		exec := engine(pod, container)
		return func(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
			clock = clock.Add(3 * time.Minute)
			return exec(ctx, argv, stdin, stdout, stderr)
		}
	}
	conflicted := false
	c.cs.PrependReactor("update", "secrets", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		if conflicted || a.(k8stesting.UpdateAction).GetObject().(*corev1.Secret).Name != "db-credentials" {
			return false, nil, nil
		}
		conflicted = true
		clock = clock.Add(3 * time.Minute)
		return true, nil, apierrors.NewConflict(corev1.Resource("secrets"), "db-credentials", errors.New("changed"))
	})

	require.NoError(t, r.run(context.Background()))

	assert.True(t, conflicted)
	assert.NotEqual(t, storedSentinel, string(c.secret(t, "db-credentials").Data["PASSWORD"]))
}

// TestRotationOnlyTalksToThePodsOfTheServicesStatefulSet checks ownership because
// the engine command carries passwords. The unowned pod sorts first, so selecting
// by label alone would send them to it instead of the Service's StatefulSet pod.
func TestRotationOnlyTalksToThePodsOfTheServicesStatefulSet(t *testing.T) {
	impostor := readyPod("a-stranger", "old")
	impostor.OwnerReferences = nil
	impostor.Annotations = nil
	c := newRotationCluster("postgres", postgresData(), impostor)

	require.NoError(t, c.rotation().run(context.Background()))

	require.NotEmpty(t, c.execs)
	for _, e := range c.execs {
		assert.Equal(t, "db-0", e.pod, "the passwords must reach the service's own pod")
	}
}

func TestTheRestartWaitIgnoresAStrangerWithTheNewStamp(t *testing.T) {
	const stamp = "2026-10-22T02:00:00Z"
	stranger := readyPod("a-stranger", stamp)
	stranger.OwnerReferences = nil
	c := newRotationCluster("postgres", postgresData(), stranger)
	r := c.rotation()
	r.restart = func(ctx context.Context) (string, error) {
		sts, err := c.cs.AppsV1().StatefulSets("shop").Get(ctx, "db", metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		sts.Spec.Template.Annotations = map[string]string{"kipper.run/restartedAt": stamp}
		if _, err := c.cs.AppsV1().StatefulSets("shop").Update(ctx, sts, metav1.UpdateOptions{}); err != nil {
			return "", err
		}
		return stamp, c.cs.CoreV1().Pods("shop").Delete(ctx, "db-0", metav1.DeleteOptions{})
	}

	err := r.run(context.Background())

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "did not reach", "the restart reached the StatefulSet, only its pod never came back")
	assert.NotContains(t, c.out.String(), "Service pod recreated")
}

// TestTheRestartWaitNamesWhyThePodNeverCounted checks that a timeout preserves
// the latest read or ownership error so the operator can diagnose the failure.
func TestTheRestartWaitNamesWhyThePodNeverCounted(t *testing.T) {
	c := newRotationCluster("postgres", postgresData())
	r := c.rotation()
	r.restart = func(ctx context.Context) (string, error) {
		stamp, err := c.restart(ctx)
		sts, _ := c.cs.AppsV1().StatefulSets("shop").Get(ctx, "db", metav1.GetOptions{})
		sts.OwnerReferences = nil
		_, _ = c.cs.AppsV1().StatefulSets("shop").Update(ctx, sts, metav1.UpdateOptions{})
		return stamp, err
	}

	err := r.run(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not come back")
	assert.Contains(t, err.Error(), "not controlled by service db")
}

func TestRotationRefusesWhenOnlyAStrangerCarriesTheServicesLabel(t *testing.T) {
	impostor := readyPod("db-impostor", "old")
	impostor.OwnerReferences = nil
	c := newRotationCluster("postgres", postgresData(), impostor)
	require.NoError(t, c.cs.CoreV1().Pods("shop").Delete(context.Background(), "db-0", metav1.DeleteOptions{}))

	err := c.rotation().run(context.Background())

	require.Error(t, err)
	assert.Empty(t, c.execs, "nothing may be sent to a pod the service does not own")
	assert.False(t, c.hasRecord())
}

func TestRotationRefusesAStatefulSetTheServiceDoesNotControl(t *testing.T) {
	c := newRotationCluster("postgres", postgresData())
	sts := serviceStatefulSet()
	sts.OwnerReferences = nil
	_, err := c.cs.AppsV1().StatefulSets("shop").Update(context.Background(), sts, metav1.UpdateOptions{})
	require.NoError(t, err)

	err = c.rotation().run(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not controlled by service db")
	assert.Empty(t, c.execs)
}

func TestABackupThatStartsDuringTheQuestionStopsTheRotation(t *testing.T) {
	c := newRotationCluster("postgres", postgresData())
	r := c.rotation()
	r.confirm = func(string) bool {
		c.add(t, veleroBackupGVR, "velero", &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "velero.io/v1", "kind": "Backup",
			"metadata": map[string]any{"name": "nightly", "namespace": "velero"},
			"status":   map[string]any{"phase": "InProgress"},
		}})
		return true
	}

	err := r.run(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "nightly")
	assert.Empty(t, c.execs)
	assert.False(t, c.hasRecord())
}

func TestABackupCheckThatIsForbiddenNamesTheMissingAccess(t *testing.T) {
	c := newRotationCluster("postgres", postgresData())
	c.dyn.PrependReactor("list", "backups", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "velero.io", Resource: "backups"}, "", errors.New("no access"))
	})

	err := c.rotation().run(context.Background())

	require.Error(t, err, "a guard the user cannot evaluate must not be skipped")
	assert.Contains(t, err.Error(), "Ask a cluster admin")
	assert.Contains(t, err.Error(), "pauses resource tuning", "the refusal names both things a project role cannot do")
	assert.Empty(t, c.execs)
}

func TestTheBackupCheckComesBeforeThePodLookup(t *testing.T) {
	c := newRotationCluster("postgres", postgresData())
	require.NoError(t, c.cs.CoreV1().Pods("shop").Delete(context.Background(), "db-0", metav1.DeleteOptions{}))
	c.dyn.PrependReactor("list", "backups", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "velero.io", Resource: "backups"}, "", errors.New("no access"))
	})

	err := c.rotation().run(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Ask a cluster admin", "a project owner learns first that the command needs an admin")
}

func TestABackupThatExcludesTheNamespaceDoesNotStopTheRotation(t *testing.T) {
	c := newRotationCluster("postgres", postgresData())
	c.add(t, veleroBackupGVR, "velero", &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "velero.io/v1", "kind": "Backup",
		"metadata": map[string]any{"name": "platform", "namespace": "velero"},
		"spec":     map[string]any{"excludedNamespaces": []any{"shop"}},
		"status":   map[string]any{"phase": "InProgress"},
	}})

	require.NoError(t, c.rotation().run(context.Background()))
	assert.NotEmpty(t, c.execs, "the rotation went ahead")
}

func TestAPendingBackupMatchingTheNamespaceByPatternStopsTheRotation(t *testing.T) {
	c := newRotationCluster("postgres", postgresData())
	c.add(t, veleroBackupGVR, "velero", &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "velero.io/v1", "kind": "Backup",
		"metadata": map[string]any{"name": "shops", "namespace": "velero"},
		"spec":     map[string]any{"includedNamespaces": []any{"sh*"}},
	}})

	err := c.rotation().run(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "shops")
	assert.Empty(t, c.execs)
	assert.False(t, c.hasRecord())
}
