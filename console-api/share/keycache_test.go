package share

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func legacySigningSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: SigningSecretName, Namespace: SigningSecretNamespace},
		// The retired v0 shape: a bare "key" field with no kid slot.
		Data: map[string][]byte{"key": []byte("0123456789abcdef0123456789abcdef")},
	}
}

func slottedSigningSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: SigningSecretName, Namespace: SigningSecretNamespace},
		Data: map[string][]byte{
			slotCurrent: encodeSlot("kidcafe0deadbeef", []byte("0123456789abcdef0123456789abcdef")),
		},
	}
}

// TestKeyCacheFailsClosedOnLegacySecret: the gate never reads the v0 key
// field. Until the mint path migrates the Secret to kid slots, the share
// path stays closed — which is the moment v0's kid-less links die.
func TestKeyCacheFailsClosedOnLegacySecret(t *testing.T) {
	client := fake.NewSimpleClientset(legacySigningSecret())
	clock := time.Unix(1_700_000_000, 0)
	c := NewKeyCache(client, 30*time.Second)
	c.now = func() time.Time { return clock }

	if _, ok := c.Get(); ok {
		t.Error("a legacy v0 secret must not produce a keyring")
	}
}

// A cluster that never minted a link must not pay one API read per gate
// request: the not-found answer is cached for the TTL like any other.
func TestKeyCacheCachesNotFound(t *testing.T) {
	client := fake.NewSimpleClientset()
	reads := 0
	client.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		reads++
		return false, nil, nil
	})
	clock := time.Unix(1_700_000_000, 0)
	c := NewKeyCache(client, 30*time.Second)
	c.now = func() time.Time { return clock }

	for i := 0; i < 5; i++ {
		if _, ok := c.Get(); ok {
			t.Fatal("no keyring must fail closed")
		}
	}
	if reads != 1 {
		t.Fatalf("reads = %d, want 1 inside the TTL", reads)
	}

	clock = clock.Add(31 * time.Second)
	if _, ok := c.Get(); ok {
		t.Fatal("still no keyring")
	}
	if reads != 2 {
		t.Fatalf("reads = %d, want a re-read after the TTL", reads)
	}
}

func TestKeyCacheServesAndCaches(t *testing.T) {
	client := fake.NewSimpleClientset(slottedSigningSecret())
	clock := time.Unix(1_700_000_000, 0)
	c := NewKeyCache(client, 30*time.Second)
	c.now = func() time.Time { return clock }

	if _, ok := c.Get(); !ok {
		t.Fatal("expected key on first Get")
	}

	// A second Get inside the ttl must not hit the API server.
	reads := 0
	client.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		reads++
		return false, nil, nil
	})
	clock = clock.Add(10 * time.Second)
	if _, ok := c.Get(); !ok {
		t.Fatal("expected cached key")
	}
	if reads != 0 {
		t.Errorf("cached Get made %d API reads, want 0", reads)
	}
}

func TestKeyCacheFailsClosedAfterTTLOnError(t *testing.T) {
	client := fake.NewSimpleClientset(slottedSigningSecret())
	clock := time.Unix(1_700_000_000, 0)
	c := NewKeyCache(client, 30*time.Second)
	c.now = func() time.Time { return clock }

	if _, ok := c.Get(); !ok {
		t.Fatal("expected key on first Get")
	}

	// Now make reads fail transiently.
	client.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errTransient{}
	})

	// Still inside ttl → last-good key served.
	clock = clock.Add(20 * time.Second)
	if _, ok := c.Get(); !ok {
		t.Error("key should still be served inside ttl")
	}

	// Past validThru with reads still failing → fail closed, NOT stale.
	clock = clock.Add(60 * time.Second)
	if _, ok := c.Get(); ok {
		t.Error("stale key served past its trust window during a read failure — must fail closed")
	}
}

func TestKeyCacheFailsClosedOnNotFound(t *testing.T) {
	client := fake.NewSimpleClientset() // no secret
	clock := time.Unix(1_700_000_000, 0)
	c := NewKeyCache(client, 30*time.Second)
	c.now = func() time.Time { return clock }

	if _, ok := c.Get(); ok {
		t.Error("missing signing secret must yield ok=false")
	}
}

type errTransient struct{}

func (errTransient) Error() string { return "transient API error" }
