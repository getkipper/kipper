package wire

import "testing"

func TestURL(t *testing.T) {
	tests := []struct {
		base, id, op, want string
	}{
		{"https://console-api.example.com", "t1", "manifest", "https://console-api.example.com/kipper-transfer/t1/manifest"},
		{"https://console-api.example.com/", "t1", "chunk/3", "https://console-api.example.com/kipper-transfer/t1/chunk/3"},
	}
	for _, tt := range tests {
		if got := URL(tt.base, tt.id, tt.op); got != tt.want {
			t.Errorf("URL(%q, %q, %q) = %q, want %q", tt.base, tt.id, tt.op, got, tt.want)
		}
	}
}

func TestBitmap(t *testing.T) {
	b := NewBitmap(10)
	if len(b) != 2 {
		t.Fatalf("bitmap for 10 chunks has %d bytes, want 2", len(b))
	}
	for _, n := range []int{0, 7, 8, 9} {
		if b.Get(n) {
			t.Errorf("fresh bitmap has bit %d set", n)
		}
		b.Set(n)
		if !b.Get(n) {
			t.Errorf("bit %d lost after Set", n)
		}
	}
	if got := b.Count(10); got != 4 {
		t.Errorf("Count = %d, want 4", got)
	}
	if b.Get(100) {
		t.Error("out-of-range Get must report false")
	}
}

func TestTokenEqual(t *testing.T) {
	if !TokenEqual("secret", "secret") {
		t.Error("equal tokens must match")
	}
	if TokenEqual("secret", "Secret") {
		t.Error("different tokens must not match")
	}
	if TokenEqual("secret", "secret-longer") {
		t.Error("prefix tokens must not match")
	}
}

func TestBitmapClear(t *testing.T) {
	b := NewBitmap(8)
	b.Set(3)
	b.Clear(3)
	if b.Get(3) {
		t.Error("bit 3 still set after Clear")
	}
	b.Set(2)
	b.Clear(3)
	if !b.Get(2) {
		t.Error("Clear must not disturb other bits")
	}
}
