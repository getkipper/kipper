package v1alpha1

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/getkipper/kipper/console-api/internal/apiservertest"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

const (
	moveServicesPolicy = "../../../deploy/policies/kipper-storage-move-services.yaml"
	movePodsPolicy     = "../../../deploy/policies/kipper-storage-move-pods.yaml"
)

// asUserIn grants rules in each namespace and returns a client impersonating user.
func asUserIn(ctx context.Context, t *testing.T, admin crclient.Client, cfg *rest.Config, s *runtime.Scheme, user string, rules []rbacv1.PolicyRule, namespaces ...string) crclient.Client {
	t.Helper()
	for _, ns := range namespaces {
		role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: user, Namespace: ns}, Rules: rules}
		binding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: user, Namespace: ns},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: user},
			Subjects:   []rbacv1.Subject{{APIGroup: "rbac.authorization.k8s.io", Kind: "User", Name: user}},
		}
		for _, obj := range []crclient.Object{role, binding} {
			if err := admin.Create(ctx, obj); err != nil {
				t.Fatal(err)
			}
		}
	}
	impersonated := rest.CopyConfig(cfg)
	impersonated.Impersonate = rest.ImpersonationConfig{UserName: user}
	c, err := crclient.New(impersonated, crclient.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// connectAs opens subresource (exec or attach) on a pod as user, or as the
// admin when user is empty. envtest runs no kubelet, so an admitted request
// fails later for a different reason.
func connectAs(ctx context.Context, cfg *rest.Config, user, namespace, pod, subresource string) error {
	c := rest.CopyConfig(cfg)
	if user != "" {
		c.Impersonate = rest.ImpersonationConfig{UserName: user}
	}
	cs, err := kubernetes.NewForConfig(c)
	if err != nil {
		return err
	}
	return cs.CoreV1().RESTClient().Post().Namespace(namespace).Resource("pods").Name(pod).SubResource(subresource).
		Param("container", "main").Param("command", "sh").Param("stdout", "true").Do(ctx).Error()
}

func policyFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func movePod(namespace, name string, claims ...string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "busybox"}}},
	}
	for i, claim := range claims {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name:         fmt.Sprintf("v%d", i),
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim}},
		})
	}
	return pod
}

func refusedByPodsPolicy(err error) bool {
	return err != nil && strings.Contains(err.Error(), platform.StorageMovePodsPolicy)
}

func TestStorageMovePolicies(t *testing.T) {
	cfg := apiservertest.Start(t)
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	admin, err := crclient.New(cfg, crclient.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	createManifest(ctx, t, admin, policyFile(t, movePodsPolicy))
	createManifest(ctx, t, admin, policyFile(t, moveServicesPolicy))

	claims := map[string]string{platform.StorageMoveClaimsAnnotation: "data-db-0,db-dump"}
	moving := map[string]string{platform.StorageMoveNamespaceLabel: "true"}
	for name, ns := range map[string]*corev1.Namespace{
		"moving":         {ObjectMeta: metav1.ObjectMeta{Name: "moving", Labels: moving, Annotations: claims}},
		"unlabelled":     {ObjectMeta: metav1.ObjectMeta{Name: "unlabelled", Annotations: claims}},
		"label-no-claim": {ObjectMeta: metav1.ObjectMeta{Name: "label-no-claim", Labels: moving}},
	} {
		if err := admin.Create(ctx, ns); err != nil {
			t.Fatalf("namespace %s: %v", name, err)
		}
	}

	writePodsAndServices := []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"create", "get", "update", "patch"}},
		{APIGroups: []string{""}, Resources: []string{"pods/exec", "pods/attach"}, Verbs: []string{"create", "get"}},
		{APIGroups: []string{""}, Resources: []string{"pods/ephemeralcontainers"}, Verbs: []string{"update", "patch"}},
		{APIGroups: []string{"kipper.run"}, Resources: []string{"services"}, Verbs: []string{"get", "list", "create", "update", "patch", "delete"}},
	}
	deployer := asUserIn(ctx, t, admin, cfg, s, "move-deployer", writePodsAndServices, "moving", "unlabelled", "label-no-claim")

	probe := 0
	eventually(t, "the pods policy never refused a pod on a moving claim", func() error {
		probe++
		err := admin.Create(ctx, movePod("moving", fmt.Sprintf("probe-%d", probe), "data-db-0"), crclient.DryRunAll)
		if !refusedByPodsPolicy(err) {
			return fmt.Errorf("not refused yet: %v", err)
		}
		return nil
	})

	t.Run("pods elsewhere are untouched", func(t *testing.T) {
		if err := deployer.Create(ctx, movePod("unlabelled", "app", "data-db-0")); err != nil {
			t.Fatalf("an unlabelled namespace: %v", err)
		}
		if err := deployer.Create(ctx, movePod("label-no-claim", "app", "data-db-0")); err != nil {
			t.Fatalf("a labelled namespace without listed claims: %v", err)
		}
		if err := deployer.Create(ctx, movePod("moving", "other", "data-cache-0")); err != nil {
			t.Fatalf("an unrelated claim: %v", err)
		}
		if err := deployer.Create(ctx, movePod("moving", "stateless")); err != nil {
			t.Fatalf("a pod without volumes: %v", err)
		}
	})

	t.Run("no pod but an admin's copy pod mounts a moving claim", func(t *testing.T) {
		err := deployer.Create(ctx, movePod("moving", "db-0", "data-db-0"))
		if !refusedByPodsPolicy(err) || !strings.Contains(err.Error(), "kip service storage longhorn") {
			t.Fatalf("want the move refusal for the data claim, got %v", err)
		}
		err = deployer.Create(ctx, movePod("moving", "dumper", "other", "db-dump"))
		if !refusedByPodsPolicy(err) {
			t.Fatalf("want the move refusal for the dump claim, got %v", err)
		}
		err = deployer.Create(ctx, movePod("moving", platform.StorageMoveCopyPodPrefix+"db", "data-db-0"))
		if !refusedByPodsPolicy(err) {
			t.Fatalf("the copy pod's name alone must not let a deployer in, got %v", err)
		}
		err = admin.Create(ctx, movePod("moving", "receiver", "data-db-0"))
		if !refusedByPodsPolicy(err) {
			t.Fatalf("an admin's other pods are refused too, got %v", err)
		}
		if err := admin.Create(ctx, movePod("moving", platform.StorageMoveCopyPodPrefix+"db", "data-db-0", "kipper-move-src-db")); err != nil {
			t.Fatalf("the admin's copy pod: %v", err)
		}
	})

	t.Run("only an admin creates a pod with the copy pod's name", func(t *testing.T) {
		err := deployer.Create(ctx, movePod("moving", platform.StorageMoveCopyPodPrefix+"cache"))
		if !refusedByPodsPolicy(err) {
			t.Fatalf("a deployer's pod named like the copy pod, without volumes, got %v", err)
		}
		if err := deployer.Create(ctx, movePod("unlabelled", platform.StorageMoveCopyPodPrefix+"cache")); err != nil {
			t.Fatalf("the name is free outside a move: %v", err)
		}
		if err := admin.Create(ctx, movePod("moving", platform.StorageMoveCopyPodPrefix+"cache")); err != nil {
			t.Fatalf("the admin's pod: %v", err)
		}
	})

	t.Run("only an admin gets into the copy pod", func(t *testing.T) {
		copyPod := platform.StorageMoveCopyPodPrefix + "db"
		for _, sub := range []string{"exec", "attach"} {
			if err := connectAs(ctx, cfg, "move-deployer", "moving", copyPod, sub); !refusedByPodsPolicy(err) {
				t.Fatalf("a deployer's %s into the copy pod, got %v", sub, err)
			}
			if err := connectAs(ctx, cfg, "move-deployer", "moving", "stateless", sub); refusedByPodsPolicy(err) {
				t.Fatalf("a deployer's %s into another pod is the policy's business: %v", sub, err)
			}
		}
		if err := connectAs(ctx, cfg, "", "moving", copyPod, "exec"); refusedByPodsPolicy(err) {
			t.Fatalf("the admin's exec into the copy pod: %v", err)
		}
		pod := &corev1.Pod{}
		if err := admin.Get(ctx, crclient.ObjectKey{Namespace: "moving", Name: copyPod}, pod); err != nil {
			t.Fatal(err)
		}
		pod.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Name: "debug", Image: "busybox", VolumeMounts: []corev1.VolumeMount{{Name: "v0", MountPath: "/dst"}},
		}}}
		if err := deployer.SubResource("ephemeralcontainers").Update(ctx, pod); !refusedByPodsPolicy(err) {
			t.Fatalf("a deployer's debug container in the copy pod, got %v", err)
		}
	})

	t.Run("only an admin changes the copy pod", func(t *testing.T) {
		image := func(c crclient.Client, name string) error {
			pod := &corev1.Pod{}
			if err := admin.Get(ctx, crclient.ObjectKey{Namespace: "moving", Name: name}, pod); err != nil {
				return err
			}
			changed := pod.DeepCopy()
			changed.Spec.Containers[0].Image = "busybox:other"
			return c.Patch(ctx, changed, crclient.MergeFrom(pod))
		}
		if err := image(deployer, platform.StorageMoveCopyPodPrefix+"db"); !refusedByPodsPolicy(err) {
			t.Fatalf("a deployer's image change on the copy pod, got %v", err)
		}
		if err := image(deployer, "stateless"); err != nil {
			t.Fatalf("a deployer's change to another pod: %v", err)
		}
		if err := image(admin, platform.StorageMoveCopyPodPrefix+"db"); err != nil {
			t.Fatalf("the admin's change to the copy pod: %v", err)
		}
	})

	t.Run("only an admin changes the move mark or deletes a marked service", func(t *testing.T) {
		svc := &Service{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "moving"}, Spec: ServiceSpec{Type: "postgres"}}
		if err := deployer.Create(ctx, svc); err != nil {
			t.Fatal(err)
		}
		mark := func(c crclient.Client, value string) error {
			current := &Service{}
			if err := c.Get(ctx, crclient.ObjectKeyFromObject(svc), current); err != nil {
				return err
			}
			annotations := current.GetAnnotations()
			if annotations == nil {
				annotations = map[string]string{}
			}
			if value == "" {
				delete(annotations, platform.StorageMoveAnnotation)
			} else {
				annotations[platform.StorageMoveAnnotation] = value
			}
			current.SetAnnotations(annotations)
			return c.Update(ctx, current)
		}
		refused := func(err error) bool {
			return err != nil && strings.Contains(err.Error(), "kipper-storage-move-services")
		}

		if err := mark(deployer, "kipper-storage-move-1"); !refused(err) {
			t.Fatalf("a deployer marking a service, got %v", err)
		}
		if err := mark(admin, "kipper-storage-move-1"); err != nil {
			t.Fatalf("the admin marking: %v", err)
		}
		current := &Service{}
		if err := deployer.Get(ctx, crclient.ObjectKeyFromObject(svc), current); err != nil {
			t.Fatal(err)
		}
		current.Spec.Version = "16"
		if err := deployer.Update(ctx, current); err != nil {
			t.Fatalf("an update that keeps the mark: %v", err)
		}
		if err := mark(deployer, "kipper-storage-move-2"); !refused(err) {
			t.Fatalf("a deployer changing the mark, got %v", err)
		}
		if err := mark(deployer, ""); !refused(err) {
			t.Fatalf("a deployer removing the mark, got %v", err)
		}
		if err := deployer.Delete(ctx, svc); !refused(err) {
			t.Fatalf("a deployer deleting a marked service, got %v", err)
		}
		if err := mark(admin, ""); err != nil {
			t.Fatalf("the admin unmarking: %v", err)
		}
		if err := deployer.Delete(ctx, svc); err != nil {
			t.Fatalf("a deployer deleting an unmarked service: %v", err)
		}
	})
}
