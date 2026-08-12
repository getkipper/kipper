package controllers

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func liveDexConfig(t *testing.T, r *ClusterIdentityReconciler) string {
	t.Helper()
	var cm corev1.ConfigMap
	if err := r.Get(context.Background(), types.NamespacedName{Name: dexConfigMapName, Namespace: dexNamespace}, &cm); err != nil {
		t.Fatalf("get dex-config: %v", err)
	}
	return cm.Data[dexConfigKey]
}

// A render merges onto the config a pass read at its start, so applying it
// blind overwrites whatever landed since. `kip auth reset-password` writing a
// new admin hash is the case that matters: the operator has already been shown
// a password the restored config would not accept.
func TestWriteDexConfigRefusesARenderBuiltOnAStaleRead(t *testing.T) {
	const readAtStart = "issuer: https://dex.acme.kipper.run/dex\n" +
		"staticPasswords:\n- {email: admin@acme.kipper.run, hash: OLD_HASH, username: admin}\n"
	writtenSince := strings.Replace(readAtStart, "OLD_HASH", "NEW_HASH_FROM_RESET_PASSWORD", 1)

	r, _ := reconcilerFor(dexConfigCM(writtenSince))

	// The render carries the hash as it was read, which is the old one.
	wrote, err := r.writeDexConfig(context.Background(), readAtStart, readAtStart, false)
	if err == nil {
		t.Fatal("a render built on a superseded read was applied")
	}
	if wrote {
		t.Fatal("writeDexConfig reported a write it refused")
	}
	if live := liveDexConfig(t, r); !strings.Contains(live, "NEW_HASH_FROM_RESET_PASSWORD") {
		t.Fatalf("the newer admin hash was overwritten:\n%s", live)
	}
}

func TestWriteDexConfigWritesWhenTheReadIsCurrent(t *testing.T) {
	const live = "issuer: https://dex.acme.kipper.run/dex\n" +
		"staticPasswords:\n- {email: admin@acme.kipper.run, hash: HASH, username: admin}\n"
	rendered := live + "someFutureDexKnob: added-by-this-pass\n"

	r, _ := reconcilerFor(dexConfigCM(live))

	wrote, err := r.writeDexConfig(context.Background(), rendered, live, false)
	if err != nil {
		t.Fatalf("writeDexConfig: %v", err)
	}
	if !wrote {
		t.Fatal("writeDexConfig reported no write")
	}
	if got := liveDexConfig(t, r); got != rendered {
		t.Fatalf("live config = %q, want %q", got, rendered)
	}
}

// The content check cannot see a write the informer cache has not caught up on,
// and both reads in a pass go through that same cache. So the case the guard
// actually has to survive is the one where the re-read still returns the config
// this pass rendered from, and only the resourceVersion it carries is stale.
func TestWriteDexConfigRefusesWhenOnlyTheResourceVersionIsStale(t *testing.T) {
	const readAtStart = "issuer: https://dex.acme.kipper.run/dex\n" +
		"staticPasswords:\n- {email: admin@acme.kipper.run, hash: OLD_HASH, username: admin}\n"
	writtenSince := strings.Replace(readAtStart, "OLD_HASH", "NEW_HASH_FROM_RESET_PASSWORD", 1)

	// What the lagging cache keeps serving, captured before the write lands.
	var stale *corev1.ConfigMap

	c := crfake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(dexConfigCM(readAtStart)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if cm, ok := obj.(*corev1.ConfigMap); ok && stale != nil && key.Name == dexConfigMapName {
					stale.DeepCopyInto(cm)
					return nil
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	r := &ClusterIdentityReconciler{Client: c, Scheme: testScheme()}

	var atStart corev1.ConfigMap
	key := types.NamespacedName{Name: dexConfigMapName, Namespace: dexNamespace}
	if err := c.Get(context.Background(), key, &atStart); err != nil {
		t.Fatalf("get dex-config: %v", err)
	}

	// `kip auth reset-password` writes the new admin hash, bumping the version.
	current := atStart.DeepCopy()
	current.Data[dexConfigKey] = writtenSince
	if err := c.Update(context.Background(), current); err != nil {
		t.Fatalf("update dex-config: %v", err)
	}
	stale = &atStart

	wrote, err := r.writeDexConfig(context.Background(), readAtStart, readAtStart, false)
	stale = nil
	if err == nil {
		t.Fatal("a render whose read was superseded was applied")
	}
	if wrote {
		t.Fatal("writeDexConfig reported a write it refused")
	}
	if live := liveDexConfig(t, r); !strings.Contains(live, "NEW_HASH_FROM_RESET_PASSWORD") {
		t.Fatalf("the newer admin hash was overwritten:\n%s", live)
	}
}

// A first reconcile before Dex is installed has nothing to compare against and
// must still be able to create the ConfigMap.
func TestWriteDexConfigCreatesWhenThereIsNoLiveConfig(t *testing.T) {
	const rendered = "issuer: https://dex.acme.kipper.run/dex\n"

	r, _ := reconcilerFor()

	wrote, err := r.writeDexConfig(context.Background(), rendered, "", false)
	if err != nil {
		t.Fatalf("writeDexConfig: %v", err)
	}
	if !wrote {
		t.Fatal("writeDexConfig reported no write")
	}
	if got := liveDexConfig(t, r); got != rendered {
		t.Fatalf("live config = %q, want %q", got, rendered)
	}
}
